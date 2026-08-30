#!/usr/bin/env bash
#
# Prove that the control-plane backups can actually be restored.
#
#   walg-verify.sh [--confirm-destroy]
#
# Four tests, because they exercise different code paths:
#
#   point-in-time   restores to a recorded instant beside the live database.
#                   Uses restore_command, recovery_target_time and
#                   recovery_target_action.
#
#   upload resumed  archiving is broken, writes continue, archiving comes back.
#                   The restore must reach the latest archived WAL.
#
#   upload lost     archiving is broken and never recovers before the node dies.
#                   The restore must reach the last successfully archived WAL,
#                   and the writes after that point must be asserted ABSENT
#                   rather than treated as a failure. This is the case that says
#                   what the archive actually guarantees.
#
#   bare disk       deletes the pgdata volume and restores onto nothing.
#                   Uses only restore_command. Requires --confirm-destroy
#                   because it destroys the local database to do its job.
#
# The first three are non-destructive to the database and always run; two of
# them do interrupt the object store and one briefly stops Postgres, so none of
# them belongs anywhere near a live service.
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
# Each run pushes one base backup per test into whatever archive the stack is
# pointed at, and each of those triggers retention. With WALG_RETAIN_FULL at its
# default of 7, two verification runs are enough to evict every real daily
# backup and, with them, the WAL those backups depended on. That is harmless against a dev
# archive and destructive against a real one, so the run refuses to start unless
# the object store is a local one — see assert_development_stack below.
#
#   COMPOSE_SERVICE   compose service running Postgres   (default: postgres)
#   OBJECT_STORE_SERVICE
#                     compose service backing the archive  (default: minio)
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
OBJECT_STORE_SERVICE="${OBJECT_STORE_SERVICE:-minio}"
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
RESTORE_OBJECT_STORE_ON_EXIT="no"
RESTART_POSTGRES_ON_EXIT="no"
POSTGRES_MUST_STAY_DOWN="no"

cleanup() {
    local rc="${1:-$?}" cleanup_failed=0
    trap - EXIT
    set +e

    # -v removes only the scratch container's anonymous volumes. Every cleanup
    # action is independent so one Docker error cannot skip service recovery.
    if [ -n "$SCRATCH_CONTAINER" ]; then
        if ! docker inspect "$SCRATCH_CONTAINER" >/dev/null 2>&1 \
            || docker rm -f -v "$SCRATCH_CONTAINER" >/dev/null 2>&1; then
            SCRATCH_CONTAINER=""
        else
            log "WARNING: could not remove scratch container ${SCRATCH_CONTAINER}"
            cleanup_failed=1
        fi
    fi
    if [ -n "$SCRATCH_VOLUME" ]; then
        if ! docker volume inspect "$SCRATCH_VOLUME" >/dev/null 2>&1 \
            || docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1; then
            SCRATCH_VOLUME=""
        else
            log "WARNING: could not remove scratch volume ${SCRATCH_VOLUME}"
            cleanup_failed=1
        fi
    fi

    if [ "$RESTORE_OBJECT_STORE_ON_EXIT" = "yes" ]; then
        if ! docker compose up -d --wait "$OBJECT_STORE_SERVICE" >/dev/null 2>&1; then
            log "WARNING: could not restore ${OBJECT_STORE_SERVICE} during cleanup"
            cleanup_failed=1
        fi
    fi

    if [ "$RESTART_POSTGRES_ON_EXIT" = "yes" ] && [ "$POSTGRES_MUST_STAY_DOWN" = "no" ]; then
        if ! docker compose up -d --wait "$COMPOSE_SERVICE" >/dev/null 2>&1; then
            log "WARNING: could not restart ${COMPOSE_SERVICE} during cleanup"
            cleanup_failed=1
        fi
    elif [ "$POSTGRES_MUST_STAY_DOWN" = "yes" ]; then
        log "WARNING: ${COMPOSE_SERVICE} remains stopped because its PGDATA was not restored"
    fi

    drop_probe_db
    [ "$rc" -eq 0 ] && [ "$cleanup_failed" -ne 0 ] && rc=1
    exit "$rc"
}
trap 'cleanup $?' EXIT

# client_min_messages=warning because CREATE TABLE IF NOT EXISTS and friends
# emit a NOTICE on the second run, and a test harness that prints "relation
# already exists" twice per run trains the reader to skim past its own output.
# Warnings and errors still come through.
pg()    { docker compose exec -T -u postgres -e PGOPTIONS='-c client_min_messages=warning' \
              "$COMPOSE_SERVICE" psql -U postgres "$@" </dev/null; }
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
    docker compose up -d --wait "$COMPOSE_SERVICE" >/dev/null 2>&1 || die "could not bring ${COMPOSE_SERVICE} up"
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
    # Rendered as JSON and read with jq. Reading the YAML by hand needed a
    # careful split, because the value carries its own colons ('http://',
    # ':9000') and a naive field split returns 'http' — which it did, once.
    docker compose config --format json 2>/dev/null \
        | jq -r --arg svc "$COMPOSE_SERVICE" \
            '.services[$svc].environment.AWS_ENDPOINT // empty' 2>/dev/null || true
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

# Seal the segment holding the most recent writes and return its name, without
# waiting for it to reach the archive. The tests that break archiving on purpose
# need the name of the segment they just stranded.
seal_segment() {
    local segment
    segment="$(pg -tAc "SELECT pg_walfile_name(pg_current_wal_lsn())" | tr -d '\r')"
    pg -qc "SELECT pg_switch_wal()" >/dev/null
    printf '%s' "$segment"
}

# Wait for the archiver to confirm a segment. A fixed sleep races the upload, and
# a test that fails intermittently gets skipped rather than fixed.
wait_for_segment_archived() {
    local segment="$1" timeout="${2:-120}" deadline archived waited=0
    log "waiting for ${segment} to be archived"

    deadline=$(( $(date +%s) + timeout ))
    while :; do
        archived="$(pg -tAc "SELECT last_archived_wal FROM pg_stat_archiver" | tr -d '\r')"
        # Segment names are zero-padded hex on a fixed-width format, so a string
        # comparison orders them correctly within a timeline.
        [ -n "$archived" ] && [ ! "$archived" \< "$segment" ] && {
            [ "$waited" -gt 0 ] && log "  archived after ~${waited}s"
            return 0
        }
        [ "$(date +%s)" -ge "$deadline" ] \
            && die "segment ${segment} was not archived within ${timeout}s (last archived: ${archived:-none})"
        sleep 2
        waited=$(( waited + 2 ))
        # A drain that follows an outage waits on wal-g's own retry backoff, so
        # it can take far longer than the healthy path. Say so rather than look
        # hung for minutes.
        [ $(( waited % 30 )) -eq 0 ] && log "  still waiting (${waited}s, last archived: ${archived:-none})"
    done
}

seal_and_wait_for_archive() { wait_for_segment_archived "$(seal_segment)"; }


# --- object store control --------------------------------------------------
#
# The interrupted-upload tests need archiving to actually fail, and the only
# honest way to arrange that is to take the object store away. Stopping the
# service is preferred over firewall or credential tricks: it fails the same way
# a real outage does, at the same layer, with the same WAL-G error.

object_store_cid() { docker compose ps -q "$OBJECT_STORE_SERVICE" 2>/dev/null; }

stop_object_store() {
    log "stopping ${OBJECT_STORE_SERVICE} — archiving will start failing"
    # Armed here rather than at the call sites, and before the stop rather than
    # after it. Everything between this line and start_object_store can die —
    # assert_segment_stuck most of all, since its whole job is to fail when the
    # outage is not what was expected — and any of those paths would otherwise
    # leave the object store down for good. 'docker compose stop' is itself one
    # of them: it can be interrupted or partially succeed, so arming afterwards
    # leaves the very window it was meant to close. Arming when nothing was
    # actually stopped costs one no-op 'up -d --wait' in the trap.
    RESTORE_OBJECT_STORE_ON_EXIT="yes"
    docker compose stop "$OBJECT_STORE_SERVICE" >/dev/null 2>&1 \
        || die "could not stop ${OBJECT_STORE_SERVICE}"
}

start_object_store() {
    local deadline cid
    log "starting ${OBJECT_STORE_SERVICE}"
    docker compose start "$OBJECT_STORE_SERVICE" >/dev/null 2>&1 \
        || die "could not start ${OBJECT_STORE_SERVICE}"

    # Health, not "started". WAL-G against a MinIO that is up but not yet
    # serving fails exactly like one that is down, and the difference would show
    # up as an unexplained flake rather than as this wait timing out.
    deadline=$(( $(date +%s) + 120 ))
    while :; do
        cid="$(object_store_cid)"
        if [ -n "$cid" ] \
            && [ "$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null)" = "healthy" ]; then
            RESTORE_OBJECT_STORE_ON_EXIT="no"
            return 0
        fi
        [ "$(date +%s)" -ge "$deadline" ] \
            && die "${OBJECT_STORE_SERVICE} did not become healthy within 120s"
        sleep 2
    done
}

# Prove the outage is real before asserting anything about its consequences. A
# test that assumed archiving had stopped while it quietly kept working would
# pass for entirely the wrong reason — and that is the failure mode this whole
# pair of tests exists to rule out.
#
# Not via pg_stat_archiver.failed_count. That counter only moves when
# archive_command RETURNS non-zero, and archive_command here is 'wal-g wal-push',
# which retries an unreachable endpoint with backoff before giving up. Against a
# stopped container the connection times out rather than being refused, so the
# command stays running: archiving is completely stalled while failed_count
# reads zero. Measured — this is what the first version of this test hit.
#
# The queue is what is true immediately. Postgres writes <segment>.ready the
# moment a segment is sealed and renames it to .done only once archive_command
# has succeeded, so a .ready that persists IS the outage, observed directly
# rather than inferred from a counter.
archive_status_of() {
    local segment="$1"
    docker compose exec -T -u postgres "$COMPOSE_SERVICE" sh -c "
        d='${PROD_PGDATA}/pg_wal/archive_status'
        if   [ -e \"\$d/${segment}.done\"  ]; then echo done
        elif [ -e \"\$d/${segment}.ready\" ]; then echo ready
        else echo absent; fi" </dev/null | tr -d '\r'
}

assert_segment_stuck() {
    local segment="$1" deadline status

    # It has to reach the queue first; a segment that never sealed would make
    # everything below vacuous.
    deadline=$(( $(date +%s) + 60 ))
    while :; do
        status="$(archive_status_of "$segment")"
        [ "$status" = "ready" ] && break
        [ "$status" = "done" ] \
            && die "segment ${segment} was archived even though the object store is down"
        [ "$(date +%s)" -ge "$deadline" ] \
            && die "segment ${segment} never entered the archive queue (status: ${status})"
        sleep 2
    done

    # And it has to stay there. The healthy path drains in about a second, so
    # fifteen is well past any ambiguity without padding the run.
    log "segment ${segment} is queued; confirming it stays that way"
    sleep 15
    status="$(archive_status_of "$segment")"
    [ "$status" = "ready" ] \
        || die "segment ${segment} left the queue while the object store was down (status: ${status})"
    log "archiving is stalled as intended (${segment} still .ready)"
}

# --- restoring for inspection ----------------------------------------------
#
# Three tests restore beside the live database and then query the result, so the
# bookkeeping lives here once. The container and volume are captured before any
# assertion runs: from that moment the EXIT trap owns them, and an assertion
# that dies cannot leave them behind.

restore_for_inspection() {
    local journal="$1"; shift
    local port
    port="$(free_port)"
    log "restoring on port ${port}"
    if ! RESTORE_JOURNAL="$journal" RESTORE_PORT="$port" \
        "${SCRIPT_DIR}/walg-restore.sh" inspect "$@" >/dev/null 2>&1; then
        tail -5 "$journal" 2>/dev/null
        die "the restore itself failed; full journal at ${journal}"
    fi
    SCRATCH_CONTAINER="$(awk '/^  container /{print $2}' "$journal")"
    SCRATCH_VOLUME="$(awk '/^  scratch volume /{print $3}' "$journal")"
    [ -n "$SCRATCH_CONTAINER" ] || die "could not find the restored container in ${journal}"
}

restored_markers() {
    markers_from docker exec -u postgres "$SCRATCH_CONTAINER" psql -U postgres
}

release_inspection() {
    docker rm -f -v "$SCRATCH_CONTAINER" >/dev/null 2>&1 || true
    docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1 || true
    SCRATCH_CONTAINER=""
    SCRATCH_VOLUME=""
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

    log "restoring to ${target}"
    restore_for_inspection "/tmp/cara-verify-${RUN_ID}-pitr.log" --at "$target"

    local found
    found="$(restored_markers)"

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

    release_inspection
    pass "restored to ${target}: marker A present, marker B correctly absent"
}

# --- interrupted uploads ---------------------------------------------------
#
# Two cases, asserted separately because they answer different questions. The
# first asks whether a transient outage costs anything permanently; the second
# asks what the archive is actually worth when the outage is still in progress
# at the moment the node dies. Collapsing them into one test would let the
# second answer hide behind the first.
#
# Both leave the database intact. Neither may run anywhere near a live service:
# the object store goes away for the duration, and the second stops Postgres.

# Case 1: upload fails, then recovers.
#
# Nothing is permanently lost — Postgres never discards an unarchived segment,
# so once the archive is reachable again the backlog drains and the restore
# reaches everything. What this proves is that the retry actually happens, and
# that a restore taken afterwards is complete rather than truncated at the point
# of the outage.
test_upload_resumed() {
    log "=== interrupted upload, then recovered ==="
    TEST_RUN="${RUN_ID}-resumed"
    ensure_running
    ensure_probe_db

    log "taking a base backup to restore from"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"

    write_marker A
    seal_and_wait_for_archive

    stop_object_store

    write_marker B
    # Seal the segment holding B so the archiver has something to stall on. The
    # switch itself succeeds; it is the upload behind it that cannot.
    local segment_b
    segment_b="$(seal_segment)"
    assert_segment_stuck "$segment_b"

    start_object_store
    # Generous, because the drain waits on wal-g's retry backoff rather than on
    # the upload itself — the whole point of case 1 is that it does eventually
    # get there.
    wait_for_segment_archived "$segment_b" 300

    restore_for_inspection "/tmp/cara-verify-${RUN_ID}-resumed.log"

    local found
    found="$(restored_markers)"
    # Both. Only A would mean the backlog never drained; neither would mean the
    # base backup itself did not come back.
    [ "$found" = "A,B" ] || die "expected markers A,B after the archive recovered, got '${found:-<none>}'"

    release_inspection
    pass "archiving recovered and the restore reached both markers"
}

# Case 2: upload never succeeds.
#
# The node dies with the outage still in progress. Postgres is stopped before
# the object store returns, so the pending segment is never uploaded and the
# writes it holds exist only on a disk that, in the scenario being modelled, is
# gone. Marker B being absent is the CORRECT outcome, and asserting it is the
# point: this is the measured difference between "what was written" and "what
# was recoverable", which is the thing archive_timeout actually bounds.
test_upload_lost() {
    log "=== interrupted upload, never recovered ==="
    TEST_RUN="${RUN_ID}-lost"
    ensure_running
    ensure_probe_db

    log "taking a base backup to restore from"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"

    write_marker A
    seal_and_wait_for_archive
    log "marker A is in the archive; everything after this point is at risk"

    stop_object_store

    write_marker B
    local segment_b
    segment_b="$(seal_segment)"
    assert_segment_stuck "$segment_b"

    # The node dies here. Stopping Postgres before the object store comes back
    # is what makes the loss permanent: a running server would drain the backlog
    # the moment it could, and the test would silently become case 1.
    RESTART_POSTGRES_ON_EXIT="yes"
    log "stopping ${COMPOSE_SERVICE} while the archive is still unreachable"
    docker compose stop "$COMPOSE_SERVICE" >/dev/null 2>&1 \
        || die "could not stop ${COMPOSE_SERVICE}"
    start_object_store

    restore_for_inspection "/tmp/cara-verify-${RUN_ID}-lost.log"

    local found
    found="$(restored_markers)"
    # A alone. A,B would mean the segment reached the archive after all and the
    # test proved nothing; neither would mean replay never got as far as A.
    [ "$found" = "A" ] \
        || die "expected marker A only — B was written after archiving broke and must not be recoverable — got '${found:-<none>}'"

    release_inspection

    # Bring the stack back before returning. Postgres will drain the backlog on
    # startup, which is correct and happens after every assertion above.
    log "restarting ${COMPOSE_SERVICE}; the pending segment will drain now"
    ensure_running
    RESTART_POSTGRES_ON_EXIT="no"
    pass "restore reached the last archived WAL; the unarchived write is correctly absent"
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
    # Before the first destructive command, not after. From here until the
    # restore returns there is no Postgres and no data volume, and every exit
    # path in between — a failed restore, a failed assertion, Ctrl-C — has to
    # leave a working stack behind. What comes back is an empty cluster, which
    # is the correct outcome: the volume this test deleted is gone either way,
    # and a developer is better served by a stack that starts than by one that
    # silently stays down.
    RESTART_STACK_ON_EXIT="yes"
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
    POSTGRES_MUST_STAY_DOWN="no"

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
test_upload_resumed
test_upload_lost

if [ "$CONFIRM_DESTROY" = "yes" ]; then
    test_bare_disk
else
    log "skipping the bare-disk test; pass --confirm-destroy to run it"
    log "without it the archive has not been proven to work on an empty disk"
fi

[ "$RESTORE_OBJECT_STORE_ON_EXIT" = "no" ] \
    && [ "$RESTART_POSTGRES_ON_EXIT" = "no" ] \
    && [ "$POSTGRES_MUST_STAY_DOWN" = "no" ] \
    || die "verification finished with an armed cleanup state"
log "done"
