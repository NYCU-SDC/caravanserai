#!/usr/bin/env bash
#
# Prove that the control-plane backups can actually be restored.
#
#   walg-verify.sh [--confirm-destroy]
#
# Two tests, because they exercise different code paths:
#
#   point-in-time   restores to a recorded instant beside the live database.
#                   Uses restore_command, recovery_target_time and
#                   recovery_target_action. Non-destructive, always runs.
#
#   bare disk       deletes the pgdata volume and restores onto nothing.
#                   Uses only restore_command. Requires --confirm-destroy
#                   because it destroys the local database to do its job.
#
# The bare-disk test is the one that matters most and the one easiest to fake.
# docker-compose.yaml keeps Postgres data in a named volume, so removing the
# container leaves the data intact — a test that only kills the container passes
# while reading local files and never contacting the object store at all. This
# deletes the volume.
#
# Markers live in a database of their own, created here and dropped at the end,
# so nothing accumulates in the control-plane database and no test row can ever
# be mistaken for real state. The cluster-wide backup covers it either way,
# which is exactly what the round trip needs.
#
# Each run pushes two base backups into whatever archive the stack is pointed
# at, and each of those triggers retention. With WALG_RETAIN_FULL at its default
# of 7, four verification runs are enough to evict every real daily backup and,
# with them, the WAL those backups depended on. That is harmless against a dev
# archive and destructive against a real one, so the run refuses to start unless
# the object store is a local one — see assert_development_stack below.
#
#   COMPOSE_SERVICE   compose service running Postgres   (default: postgres)
#   LOCK_FILE         maintenance lock, shared with the backup and restore
#                     scripts (default: /tmp/cara-control-plane-maintenance.lock)
#   WALG_VERIFY_ALLOW_NON_DEV_ARCHIVE=yes
#                     run anyway against a non-local object store. A break-glass
#                     override for a person at a terminal — never for a
#                     scheduled job.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
LOCK_FILE="${LOCK_FILE:-/tmp/cara-control-plane-maintenance.lock}"
PROD_PGDATA="/var/lib/postgresql/data"
PROBE_DB="walg_verify"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
CONFIRM_DESTROY="no"
TEST_RUN=""

case "${1:-}" in
    "")                 : ;;
    --confirm-destroy)  CONFIRM_DESTROY="yes" ;;
    # A typo would otherwise skip the destructive test and still report success,
    # which is the one outcome this script must never produce.
    *) printf 'unknown option %s\n\nusage: walg-verify.sh [--confirm-destroy]\n' "$1" >&2; exit 2 ;;
esac

log()  { printf '%s  %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
pass() { printf '\n  PASS  %s\n\n' "$*"; }
die()  { printf '\n  FAIL  %s\n\n' "$*" >&2; exit 1; }

# Whatever a test leaves half-finished gets cleaned up here rather than in each
# failure path. The restore script cleans up after itself, but this script takes
# ownership of the inspect instance it deliberately asked to be left running.
SCRATCH_CONTAINER=""
SCRATCH_VOLUME=""
cleanup() {
    # -v so the scratch instance's anonymous volume goes with it; see the same
    # note in walg-restore.sh. Named volumes are untouched by -v.
    [ -n "$SCRATCH_CONTAINER" ] && docker rm -f -v "$SCRATCH_CONTAINER" >/dev/null 2>&1
    [ -n "$SCRATCH_VOLUME" ] && docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1
    drop_probe_db
    return 0
}
trap cleanup EXIT

pg()    { docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres "$@" </dev/null; }
probe() { pg -d "$PROBE_DB" "$@"; }

# The point-in-time test restores in inspect mode, which publishes a port so the
# instance can be queried. Somebody else's inspect instance left running is a
# normal state and should not fail a verification run, so take the first port
# nothing is listening on.
free_port() {
    local port=5433
    while ss -tln 2>/dev/null | grep -F ":${port} " >/dev/null; do
        port=$(( port + 1 ))
        [ "$port" -gt 5460 ] && die "no free port between 5433 and 5460"
    done
    printf '%s' "$port"
}

ensure_running() {
    docker compose up -d --wait >/dev/null 2>&1 || die "could not bring the stack up"
}

ensure_probe_db() {
    if [ "$(pg -tAc "SELECT 1 FROM pg_database WHERE datname = '${PROBE_DB}'" | tr -d '\r')" != "1" ]; then
        pg -qc "CREATE DATABASE ${PROBE_DB}" >/dev/null
    fi
    probe -qc "CREATE TABLE IF NOT EXISTS probe(
                   id serial primary key,
                   run text not null,
                   marker text not null,
                   at timestamptz not null default now())" >/dev/null
}

# --confirm-destroy says "I understand this deletes something". It does not say
# which machine's database, and a script that lives in the repo will eventually
# be run by someone who has not read the header. This answers the other half of
# the question by looking at where the archive actually is.
#
# It gates the whole run rather than only the destructive test: the
# point-in-time half is non-destructive to the database but still pushes a base
# backup and applies retention, which against a real archive is the slower way
# to lose the same thing.
#
# The endpoint is the discriminator because it is the one setting that has to
# differ between the dev stack and anything real. An unset value means plain AWS
# S3, which is emphatically not a development stack, so the catch-all is correct
# for it.
#
# Answered without requiring anything to be running. A stopped host has to be
# able to reach the refusal without this script starting its Compose stack
# first: bringing production Postgres up as a side effect of a check that is
# about to say no is precisely the surprise the check exists to prevent.
#
# A running container wins when there is one — it is authoritative for the
# environment actually in use, which a compose file edited since startup is not.
resolve_archive_endpoint() {
    if docker compose ps --status running --services 2>/dev/null \
        | grep -Fx "$COMPOSE_SERVICE" >/dev/null; then
        docker compose exec -T "$COMPOSE_SERVICE" printenv AWS_ENDPOINT </dev/null 2>/dev/null \
            | tr -d '\r' || true
        return 0
    fi
    # Only the first colon separates the key; the value has its own ('http://',
    # ':9000'), so a field split on ':' would return 'http'.
    docker compose config 2>/dev/null \
        | awk '/^[[:space:]]*AWS_ENDPOINT:[[:space:]]/ {
                   sub(/^[^:]*:[[:space:]]*/, ""); gsub(/^"|"$/, ""); print; exit }' || true
}

assert_development_stack() {
    local endpoint
    endpoint="$(resolve_archive_endpoint)"

    case "$endpoint" in
        http://minio:9000|http://localhost:*|http://127.0.0.1:*)
            log "object store is ${endpoint} — development stack"
            return 0
            ;;
    esac

    if [ "${WALG_VERIFY_ALLOW_NON_DEV_ARCHIVE:-no}" = "yes" ]; then
        log "WARNING: object store is ${endpoint:-<unset>}, not a development stack"
        log "WARNING: continuing because WALG_VERIFY_ALLOW_NON_DEV_ARCHIVE=yes"
        log "WARNING: this run pushes base backups into that archive and applies retention"
        [ "$CONFIRM_DESTROY" = "yes" ] \
            && log "WARNING: and --confirm-destroy will delete this machine's database"
        return 0
    fi

    log "ERROR: the object store is ${endpoint:-<unset>}, not the development MinIO."
    log "       This run pushes base backups into it and applies retention;"
    log "       with --confirm-destroy it also deletes this machine's database."
    die "refusing to run against a non-development archive (override: WALG_VERIFY_ALLOW_NON_DEV_ARCHIVE=yes)"
}

drop_probe_db() {
    docker compose ps --status running --services 2>/dev/null | grep -qx "$COMPOSE_SERVICE" || return 0
    pg -qc "DROP DATABASE IF EXISTS ${PROBE_DB}" >/dev/null 2>&1 || true
}

write_marker() {
    probe -qc "INSERT INTO probe(run, marker) VALUES ('${TEST_RUN}', '$1')" >/dev/null
}

markers_from() {
    # $1 is a psql invocation prefix, so the same assertion works against the
    # live cluster and against a restored instance in another container.
    "$@" -d "$PROBE_DB" -tAc \
        "SELECT string_agg(marker, ',' ORDER BY id) FROM probe WHERE run = '${TEST_RUN}'" | tr -d '\r'
}

# Force the segment holding the most recent writes out, then wait for the
# archiver to confirm it. A fixed sleep races the upload, and a test that fails
# intermittently gets skipped rather than fixed.
seal_and_wait_for_archive() {
    local segment deadline archived
    segment="$(pg -tAc "SELECT pg_walfile_name(pg_current_wal_lsn())" | tr -d '\r')"
    pg -qc "SELECT pg_switch_wal()" >/dev/null
    log "waiting for ${segment} to be archived"

    deadline=$(( $(date +%s) + 120 ))
    while :; do
        archived="$(pg -tAc "SELECT last_archived_wal FROM pg_stat_archiver" | tr -d '\r')"
        # Segment names are zero-padded hex on a fixed-width format, so a string
        # comparison orders them correctly within a timeline.
        [ -n "$archived" ] && [ ! "$archived" \< "$segment" ] && break
        [ "$(date +%s)" -ge "$deadline" ] \
            && die "segment ${segment} was not archived within 120s (last archived: ${archived:-none})"
        sleep 2
    done
}

# --- point-in-time ---------------------------------------------------------
#
# Restores beside the live database, so nothing here touches production. The
# assertion is what makes it meaningful: the marker written after the target
# must be absent, not merely "the restore succeeded".

test_point_in_time() {
    log "=== point-in-time restore ==="
    TEST_RUN="${RUN_ID}-pitr"
    ensure_running
    ensure_probe_db

    log "taking a base backup to restore from"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"

    write_marker A
    sleep 1
    local target
    target="$(pg -tAc "SELECT now()" | tr -d '\r')"
    sleep 1
    write_marker B
    seal_and_wait_for_archive

    local port journal
    port="$(free_port)"
    log "restoring to ${target} on port ${port}"
    journal="/tmp/cara-verify-${RUN_ID}-pitr.log"
    if ! RESTORE_JOURNAL="$journal" RESTORE_PORT="$port" \
        "${SCRIPT_DIR}/walg-restore.sh" inspect --at "$target" >/dev/null 2>&1; then
        tail -5 "$journal" 2>/dev/null
        die "the restore itself failed; full journal at ${journal}"
    fi

    # Recorded before querying so the EXIT trap owns them from here on.
    SCRATCH_CONTAINER="$(awk '/^  container /{print $2}' "$journal")"
    SCRATCH_VOLUME="$(awk '/^  scratch volume /{print $3}' "$journal")"
    [ -n "$SCRATCH_CONTAINER" ] || die "could not find the restored container in ${journal}"

    local found
    found="$(markers_from docker exec -u postgres "$SCRATCH_CONTAINER" psql -U postgres)"

    # A alone. Both markers would mean the target was ignored; neither would
    # mean replay never reached the base backup's contents.
    [ "$found" = "A" ] || die "expected marker A only, got '${found:-<none>}'"

    # Marker B being absent is the outcome; this line is the mechanism. Without
    # it a restore that happened to stop in the right place for some other
    # reason would look identical to one that honoured recovery_target_time.
    # See the note in walg-restore.sh: 'grep -q' against an unbounded producer
    # can return 141 through pipefail, and here that would report a passing
    # restore as a failed assertion.
    docker logs "$SCRATCH_CONTAINER" 2>&1 \
        | grep -F "recovery stopping before commit of transaction" >/dev/null \
        || die "the recovery log does not show a stop at the target; the point in time may not have been applied"

    docker rm -f -v "$SCRATCH_CONTAINER" >/dev/null 2>&1 || true
    docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1 || true
    SCRATCH_CONTAINER=""
    SCRATCH_VOLUME=""

    pass "restored to ${target}: marker A present, marker B correctly absent"
}

# --- bare disk -------------------------------------------------------------

test_bare_disk() {
    log "=== bare-disk restore ==="
    log "this deletes the local Postgres volume and rebuilds it from the archive"
    TEST_RUN="${RUN_ID}-baredisk"
    ensure_running
    ensure_probe_db

    write_marker A
    log "taking a base backup"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"
    write_marker B
    seal_and_wait_for_archive

    # Read the volume's real name from the running container rather than
    # guessing at the compose project prefix.
    local volume
    volume="$(docker compose ps -q "$COMPOSE_SERVICE" | xargs -r docker inspect \
        -f "{{range .Mounts}}{{if eq .Destination \"${PROD_PGDATA}\"}}{{.Name}}{{end}}{{end}}")"
    [ -n "$volume" ] || die "could not determine the volume backing ${PROD_PGDATA}"
    log "data volume is ${volume}"

    # The container has to go before the volume can: Docker refuses to remove a
    # volume that a container still references, even a stopped one. Never
    # 'compose down -v' — that takes the MinIO volume and the backups with it.
    log "stopping and removing the container, then deleting the volume"
    docker compose stop "$COMPOSE_SERVICE" >/dev/null 2>&1 || true
    docker compose rm -f "$COMPOSE_SERVICE" >/dev/null 2>&1 || true
    docker volume rm "$volume" >/dev/null || die "could not delete ${volume}"

    docker volume inspect "$volume" >/dev/null 2>&1 \
        && die "${volume} still exists; the test would have read local data"
    log "volume is gone — anything restored now came from the archive"

    local journal="/tmp/cara-verify-${RUN_ID}-baredisk.log"
    if ! RESTORE_JOURNAL="$journal" \
        "${SCRIPT_DIR}/walg-restore.sh" takeover --confirm-destroy >/dev/null 2>&1; then
        tail -5 "$journal" 2>/dev/null
        die "the restore itself failed; full journal at ${journal}"
    fi

    local found lsn
    found="$(markers_from docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres)"
    lsn="$(pg -tAc "SELECT pg_current_wal_lsn()" | tr -d '\r')"
    [ "$found" = "A,B" ] || die "expected markers A,B, got '${found:-<none>}'"
    pass "restored from an empty volume: both markers present, LSN ${lsn}"

    assert_post_restore_backup "$journal"
}

# --- the re-baseline -------------------------------------------------------
#
# Restoring proves the data comes back. It does not prove the system returns to
# a protected state: without a fresh base backup on the new timeline, the next
# restore has to replay across a branch point. That backup runs in the
# background precisely so it does not delay service, which also means nothing
# would notice if it failed.

assert_post_restore_backup() {
    local journal="$1" backup_log name deadline
    backup_log="${journal}.backup"
    log "=== post-restore base backup ==="

    deadline=$(( $(date +%s) + 120 ))
    while :; do
        name="$(awk '/done: backup=/{sub(/^.*done: backup=/, ""); print $1}' "$backup_log" 2>/dev/null | tail -1)"
        [ -n "$name" ] && break
        [ "$(date +%s)" -ge "$deadline" ] && {
            tail -5 "$backup_log" 2>/dev/null
            die "the post-restore backup did not finish within 120s; log at ${backup_log}"
        }
        sleep 2
    done

    pg -tAc "SELECT 1" >/dev/null 2>&1 || die "the database is not answering after the restore"
    docker compose exec -T -u postgres "$COMPOSE_SERVICE" wal-g backup-list </dev/null 2>/dev/null \
        | grep -F "$name" >/dev/null \
        || die "the post-restore backup ${name} is not in backup-list"

    pass "re-baselined on the new timeline: ${name} is in the archive"
}

# --- run -------------------------------------------------------------------

log "verification run ${RUN_ID}"

# The whole run, not each child. walg-backup.sh and walg-restore.sh each take
# this lock for their own duration, which leaves it free in the gaps between
# them — and this script does its most dangerous work in exactly those gaps: it
# writes markers, stops Postgres and deletes the pgdata volume between two
# locked children. A cron backup landing in one of those windows gets killed
# mid-upload and can leave an incomplete backup in the archive.
#
# Children are told to skip it rather than block on it; see the note beside the
# same lock in walg-backup.sh for why sharing it any other way does not work.
exec 9>"$LOCK_FILE"
flock -n 9 || die "another backup or restore holds ${LOCK_FILE}; wait for it to finish"
export CARA_MAINTENANCE_LOCK_HELD=1

# Before the stack is started, not after: see resolve_archive_endpoint.
assert_development_stack
ensure_running

test_point_in_time

if [ "$CONFIRM_DESTROY" = "yes" ]; then
    test_bare_disk
else
    log "skipping the bare-disk test; pass --confirm-destroy to run it"
    log "without it the archive has not been proven to work on an empty disk"
fi

log "done"
