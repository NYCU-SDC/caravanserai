#!/usr/bin/env bash
#
# Prove that the control-plane backups can actually be restored.
#
#   walg-verify.sh [--confirm-destroy]
#
# Two groups. The restore tests prove the data comes back; the guard tests prove
# the refusals, which is the half that only matters when something is wrong.
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
#   verify mode     proves the self-cleaning restore mode answers a query,
#                   publishes no host port and removes its scratch resources.
#
#   bare disk       deletes the pgdata volume and restores onto nothing.
#                   Uses only restore_command. Requires --confirm-destroy
#                   because it destroys the local database to do its job.
#
# The guards, each running a real script in a subprocess and asserting both the
# refusal and that nothing moved before it:
#
#   wrong service   COMPOSE_SERVICE pointed at another service in this stack.
#   wrong archive   a correct role and endpoint over a foreign prefix.
#   lock held       a second backup while this run holds the maintenance lock.
#   cleanup failure a cleanup step reports failure; the steps after it must
#                   still run, and the original exit code must survive.
#   no confirmation takeover over a populated PGDATA without --confirm-destroy.
#                   Refused, and refused before the directory is cleared.
#
# And one more restore test, gated on --confirm-destroy because it clears the
# live data directory:
#
#   replacement     takeover over a populated PGDATA with confirmation. The
#                   counterpart to bare disk: that one deletes the volume so
#                   anything returning proves the archive was read, this one
#                   leaves the files in place and proves they are cleared rather
#                   than restored on top of.
#
# Everything except bare disk and replacement is non-destructive to the database and always
# runs; two of the restore tests interrupt the object store and one briefly
# stops Postgres, so none of this belongs anywhere near a live service.
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
# backup and, with them, the WAL those backups depended on. That is harmless
# against a dev archive and destructive against a real one, so the run refuses to
# start unless the archive proves it is the disposable development one — see
# assert_declared_development_stack and assert_runtime_development_stack below.
# A service named "minio" is not an identity, so the endpoint alone is not
# enough; role, endpoint and prefix must all agree, on both the rendered config
# and the running container.
#
#   COMPOSE_SERVICE   compose service running Postgres   (default: postgres)
#   OBJECT_STORE_SERVICE
#                     compose service backing the archive  (default: minio)
#   LOCK_FILE         maintenance lock, shared with the backup and restore
#                     scripts (default: /tmp/cara-control-plane-maintenance.lock)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
OBJECT_STORE_SERVICE="${OBJECT_STORE_SERVICE:-minio}"
LOCK_FILE="${LOCK_FILE:-/tmp/cara-control-plane-maintenance.lock}"
DEV_ARCHIVE_ROLE="development"
DEV_ARCHIVE_ENDPOINT="http://minio:9000"
DEV_ARCHIVE_PREFIX="s3://cara-backups/cara/v1/control-plane/walg"
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

# Docker's own message is kept rather than discarded. The usual failure here is
# a host port already in use, which names the port and is fixed in seconds —
# while a bare "could not bring postgres up" sends the reader looking at the
# database instead of at the thing holding the port.
ensure_running() {
    local err
    err="$(mktemp)"
    if ! docker compose up -d --wait "$COMPOSE_SERVICE" >/dev/null 2>"$err"; then
        log "$(cat "$err")"
        rm -f "$err"
        die "could not bring ${COMPOSE_SERVICE} up"
    fi
    rm -f "$err"
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

# Endpoint names are not identities: any Compose stack can call its service
# "minio". Verification therefore requires the declared configuration and the
# environment of the running Postgres container to agree on three independent
# values. There is deliberately no production override in this destructive
# harness.
compose_env() {
    local key="$1"
    docker compose config --format json 2>/dev/null \
        | jq -er --arg svc "$COMPOSE_SERVICE" --arg key "$key" \
            '.services[$svc].environment[$key] // empty'
}

runtime_env() {
    docker compose exec -T "$COMPOSE_SERVICE" printenv "$1" </dev/null 2>/dev/null \
        | tr -d '\r'
}

assert_archive_identity_values() {
    local source="$1" role="$2" endpoint="$3" prefix="$4"
    [ "$role" = "$DEV_ARCHIVE_ROLE" ] \
        || die "${source} CARA_ARCHIVE_ROLE is '${role:-<unset>}', expected ${DEV_ARCHIVE_ROLE}"
    [ "$endpoint" = "$DEV_ARCHIVE_ENDPOINT" ] \
        || die "${source} AWS_ENDPOINT is '${endpoint:-<unset>}', expected ${DEV_ARCHIVE_ENDPOINT}"
    [ "$prefix" = "$DEV_ARCHIVE_PREFIX" ] \
        || die "${source} WALG_S3_PREFIX is '${prefix:-<unset>}', expected ${DEV_ARCHIVE_PREFIX}"
}

assert_declared_development_stack() {
    command -v jq >/dev/null \
        || die "jq is required to validate the development archive identity"

    local role endpoint prefix
    role="$(compose_env CARA_ARCHIVE_ROLE)" \
        || die "could not read CARA_ARCHIVE_ROLE from the rendered Compose service '${COMPOSE_SERVICE}'"
    endpoint="$(compose_env AWS_ENDPOINT)" \
        || die "could not read AWS_ENDPOINT from the rendered Compose service '${COMPOSE_SERVICE}'"
    prefix="$(compose_env WALG_S3_PREFIX)" \
        || die "could not read WALG_S3_PREFIX from the rendered Compose service '${COMPOSE_SERVICE}'"
    assert_archive_identity_values "declared" "$role" "$endpoint" "$prefix"
    log "declared archive identity is the disposable development archive"
}

assert_runtime_development_stack() {
    local role endpoint prefix
    role="$(runtime_env CARA_ARCHIVE_ROLE)" \
        || die "running ${COMPOSE_SERVICE} has no CARA_ARCHIVE_ROLE"
    endpoint="$(runtime_env AWS_ENDPOINT)" \
        || die "running ${COMPOSE_SERVICE} has no AWS_ENDPOINT"
    prefix="$(runtime_env WALG_S3_PREFIX)" \
        || die "running ${COMPOSE_SERVICE} has no WALG_S3_PREFIX"
    assert_archive_identity_values "running container" "$role" "$endpoint" "$prefix"
    log "running archive identity: endpoint=${DEV_ARCHIVE_ENDPOINT} prefix=${DEV_ARCHIVE_PREFIX}"
}

# A correct dev role and endpoint can still be pointed at another cluster's
# prefix by copy/paste. If the archive already has backups, compare their
# PostgreSQL system identifier with the live cluster before adding or pruning
# anything. WAL-G emits the identifier as a JSON number; normalize both sides
# through jq so the comparison uses the same numeric representation.
assert_archive_cluster_identity() {
    local local_id local_normalized archive_json archive_err archive_ids id count
    local_id="$(pg -tAc 'SELECT system_identifier FROM pg_control_system()' | tr -d '\r')" \
        || die "could not read the local PostgreSQL system identifier"
    [ -n "$local_id" ] || die "local PostgreSQL returned an empty system identifier"
    local_normalized="$(jq -nr --arg id "$local_id" '$id | tonumber | tostring')" \
        || die "could not normalize local system identifier ${local_id}"

    archive_err="$(mktemp)"
    if ! archive_json="$(docker compose exec -T -u postgres "$COMPOSE_SERVICE" \
        wal-g backup-list --detail --json </dev/null 2>"$archive_err")"; then
        local message
        message="$(cat "$archive_err")"
        rm -f "$archive_err"
        case "$message" in
            *[Nn]o\ backups\ found*)
                log "archive has no base backups yet; cluster identity will be established by this run"
                return 0
                ;;
            *) die "cannot read archive identity: ${message}" ;;
        esac
    fi
    rm -f "$archive_err"

    printf '%s' "$archive_json" | jq -e 'type == "array"' >/dev/null \
        || die "backup-list did not return a JSON array"
    count="$(printf '%s' "$archive_json" | jq -r 'length')"
    [ "$count" -gt 0 ] || {
        log "archive has no base backups yet; cluster identity will be established by this run"
        return 0
    }

    archive_ids="$(printf '%s' "$archive_json" \
        | jq -r 'if any(.[]; .system_identifier == null) then error("missing system_identifier") else .[].system_identifier | tostring end')" \
        || die "could not read system_identifier from every archived backup"
    # Reachable during ordinary development: the compose header suggests deleting
    # only the pgdata volume to exercise a restore, and letting Postgres start
    # after that runs initdb and mints a new identifier while the archive still
    # holds the old cluster's backups. The message has to say how to get out of
    # that state, because from here the guard refuses every run.
    while IFS= read -r id; do
        [ "$id" = "$local_normalized" ] && continue
        log "ERROR: this cluster and the archive are not the same database."
        log "       A cluster that was initialised fresh over an existing archive"
        log "       looks exactly like this."
        log "       Either restore this cluster from the archive:"
        log "         ./scripts/walg-restore.sh takeover --confirm-destroy"
        log "       or discard the archive and start over:"
        log "         docker compose down && docker volume rm \$(docker compose config --format json | jq -r '.name')_minio-data"
        die "archive belongs to system_identifier ${id}, local cluster is ${local_normalized}"
    done <<<"$archive_ids"
    log "archive and local PostgreSQL share system_identifier ${local_id}"
}

TARGET_CONTAINER=""
TARGET_VOLUME=""

assert_destructive_target() {
    local -a containers=()
    local service_label project_label expected_volume_key mount mount_type volume
    local volume_project volume_key

    mapfile -t containers < <(docker compose ps -q "$COMPOSE_SERVICE")
    [ "${#containers[@]}" -eq 1 ] \
        || die "expected exactly one running ${COMPOSE_SERVICE} container, found ${#containers[@]}"
    TARGET_CONTAINER="${containers[0]}"

    service_label="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.service" }}' "$TARGET_CONTAINER")"
    project_label="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$TARGET_CONTAINER")"
    [ "$service_label" = "$COMPOSE_SERVICE" ] \
        || die "target container is Compose service '${service_label:-<none>}', expected ${COMPOSE_SERVICE}"
    [ -n "$project_label" ] || die "target container has no Compose project label"

    expected_volume_key="$(docker compose config --format json \
        | jq -er --arg svc "$COMPOSE_SERVICE" --arg target "$PROD_PGDATA" \
            '.services[$svc].volumes[] | select(.target == $target and .type == "volume") | .source')" \
        || die "Compose service ${COMPOSE_SERVICE} has no named volume at ${PROD_PGDATA}"

    mount="$(docker inspect -f "{{range .Mounts}}{{if eq .Destination \"${PROD_PGDATA}\"}}{{.Type}}|{{.Name}}{{end}}{{end}}" "$TARGET_CONTAINER")"
    IFS='|' read -r mount_type volume <<<"$mount"
    [ "$mount_type" = "volume" ] && [ -n "$volume" ] \
        || die "${PROD_PGDATA} is not backed by a named Docker volume; refusing destructive verification"

    volume_project="$(docker volume inspect -f '{{ index .Labels "com.docker.compose.project" }}' "$volume")"
    volume_key="$(docker volume inspect -f '{{ index .Labels "com.docker.compose.volume" }}' "$volume")"
    [ "$volume_project" = "$project_label" ] \
        || die "volume ${volume} belongs to Compose project '${volume_project:-<none>}', expected ${project_label}"
    [ "$volume_key" = "$expected_volume_key" ] \
        || die "volume ${volume} has Compose key '${volume_key:-<none>}', expected ${expected_volume_key}"

    if docker compose ps --status running --services 2>/dev/null | grep -qx 'cara-server'; then
        die "cara-server Compose service is running; stop it before destructive verification"
    fi
    if pgrep -x cara-server >/dev/null 2>&1; then
        die "a host cara-server process is running; stop it before destructive verification"
    fi

    TARGET_VOLUME="$volume"
    log "destructive target: project=${project_label} service=${service_label}"
    log "destructive target: container=${TARGET_CONTAINER} volume=${TARGET_VOLUME} pgdata=${PROD_PGDATA}"
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

# --- self-cleaning verify mode ----------------------------------------------
#
# The other side restores deliberately use inspect mode because the harness must
# query their markers. This test covers verify's distinct contract: query inside
# the scratch container, no host port and no resources left behind.

test_verify_mode() {
    log "=== self-cleaning verify mode ==="
    ensure_running

    local journal="/tmp/cara-verify-${RUN_ID}-mode.log"
    if ! RESTORE_JOURNAL="$journal" \
        "${SCRIPT_DIR}/walg-restore.sh" verify >/dev/null 2>&1; then
        tail -5 "$journal" 2>/dev/null
        die "verify mode failed; full journal at ${journal}"
    fi

    local container volume
    container="$(awk '/^  container /{print $2}' "$journal")"
    volume="$(awk '/^  scratch volume /{print $3}' "$journal")"
    [ -n "$container" ] || die "verify journal did not record its scratch container"
    [ -n "$volume" ] || die "verify journal did not record its scratch volume"
    grep -E '^  published ports +none$' "$journal" >/dev/null \
        || die "verify mode did not prove that no host port was published"
    grep -E '^  verified +instance answers queries' "$journal" >/dev/null \
        || die "verify mode did not record a successful query"
    ! docker inspect "$container" >/dev/null 2>&1 \
        || die "verify mode left scratch container ${container} behind"
    ! docker volume inspect "$volume" >/dev/null 2>&1 \
        || die "verify mode left scratch volume ${volume} behind"

    pass "verify mode answered a query, exposed no port and removed scratch resources"
}

# --- guards ------------------------------------------------------------------
#
# These prove the refusals rather than the happy path. A guard that refuses only
# after deleting the volume is not a guard, so each test asserts two things: the
# run failed, and the Docker state it was pointed at is unchanged.
#
# The scripts under test run as subprocesses with their own LOCK_FILE. The lock
# is a separate concern with its own test below, and letting these inherit the
# real one would have them refused by the lock before reaching the check being
# tested — a pass for the wrong reason.

# Captures combined output and returns 0 only when the command failed. The
# command substitution sits inside 'if', which errexit exempts, so an expected
# non-zero exit never aborts this script.
run_expecting_failure() {
    local __out="$1"; shift
    local captured
    if captured="$("$@" 2>&1)"; then
        printf -v "$__out" '%s' "$captured"
        return 1
    fi
    printf -v "$__out" '%s' "$captured"
    return 0
}

# Enough of the Docker state to notice a container or volume being removed,
# recreated or added. Compared as a whole rather than field by field, because
# what matters is that nothing moved at all.
docker_state_fingerprint() {
    printf '%s|%s' \
        "$(docker compose ps -q "$COMPOSE_SERVICE" 2>/dev/null | tr '\n' ',')" \
        "$(docker volume ls -q 2>/dev/null | sort | tr '\n' ',')"
}

test_guard_wrong_compose_service() {
    log "=== guard: wrong COMPOSE_SERVICE ==="
    ensure_running

    local before after output
    before="$(docker_state_fingerprint)"

    # minio is a real service in this stack, so this is the plausible typo: a
    # name that resolves rather than one that fails to.
    run_expecting_failure output env \
        COMPOSE_SERVICE="$OBJECT_STORE_SERVICE" \
        LOCK_FILE="/tmp/cara-verify-guard-$$.lock" \
        "${SCRIPT_DIR}/walg-verify.sh" --confirm-destroy \
        || die "pointing COMPOSE_SERVICE at ${OBJECT_STORE_SERVICE} was accepted"

    case "$output" in
        *CARA_ARCHIVE_ROLE*) : ;;
        *) die "refused for the wrong reason: ${output}" ;;
    esac

    after="$(docker_state_fingerprint)"
    [ "$before" = "$after" ] \
        || die "the refused run changed Docker state"

    pass "a wrong COMPOSE_SERVICE is refused before anything is stopped or removed"
}

test_guard_wrong_archive_identity() {
    log "=== guard: wrong archive identity ==="
    ensure_running

    local before after output override
    before="$(docker_state_fingerprint)"

    # A correct role and endpoint pointed at another cluster's prefix — the
    # copy/paste this check exists for. Only the declared config is perturbed;
    # the refusal happens before anything is started, so no container ever sees
    # this value.
    override="$(mktemp --suffix=.yaml)"
    cat >"$override" <<YAML
services:
  ${COMPOSE_SERVICE}:
    environment:
      WALG_S3_PREFIX: s3://cara-backups/cara/v1/some-other-cluster/walg
YAML

    run_expecting_failure output env \
        COMPOSE_FILE="docker-compose.yaml:${override}" \
        LOCK_FILE="/tmp/cara-verify-guard-$$.lock" \
        "${SCRIPT_DIR}/walg-verify.sh" --confirm-destroy \
        || { rm -f "$override"; die "a foreign WALG_S3_PREFIX was accepted"; }
    rm -f "$override"

    case "$output" in
        *WALG_S3_PREFIX*) : ;;
        *) die "refused for the wrong reason: ${output}" ;;
    esac

    after="$(docker_state_fingerprint)"
    [ "$before" = "$after" ] \
        || die "the refused run changed Docker state"

    pass "a foreign archive prefix is refused before any backup or retention"
}

test_guard_maintenance_lock() {
    log "=== guard: concurrent maintenance lock ==="
    ensure_running

    local output
    # This run already holds the lock and hands children an opt-out through the
    # environment. Removing that opt-out makes the child take the lock for real,
    # which is what an unrelated cron backup would do.
    run_expecting_failure output env --unset=CARA_MAINTENANCE_LOCK_HELD \
        "${SCRIPT_DIR}/walg-backup.sh" \
        || die "a second backup was allowed while the maintenance lock was held"

    case "$output" in
        *"${LOCK_FILE}"*) : ;;
        *) die "refused for the wrong reason: ${output}" ;;
    esac

    # The holder has to be unharmed: a lock that survives by killing the process
    # that owns it is worse than no lock.
    pg -tAc 'SELECT 1' >/dev/null \
        || die "the database is not reachable after the refused backup"

    pass "a concurrent backup is refused and the lock holder is unaffected"
}

test_guard_cleanup_failure() {
    log "=== guard: a failing cleanup step does not skip the rest ==="
    ensure_running

    local stub output volume real_docker
    # Resolved here rather than hardcoded: Docker Desktop and snap installs put
    # the binary somewhere other than /usr/bin, and a stub that shadows docker
    # with a broken passthrough would fail every call rather than the one.
    real_docker="$(command -v docker)" \
        || die "docker is not on PATH"

    stub="$(mktemp -d)"
    # Reports a failure for the container removal while still performing it.
    # Leaving the container in place would block the volume removal for a real
    # reason, and the property under test is specifically that step two runs
    # after step one reported failure — not that Docker refuses a busy volume.
    cat >"${stub}/docker" <<STUB
#!/bin/sh
if [ "\$1" = "rm" ]; then
    "${real_docker}" "\$@"
    echo "injected cleanup failure" >&2
    exit 1
fi
exec "${real_docker}" "\$@"
STUB
    chmod +x "${stub}/docker"

    local journal="/tmp/cara-verify-${RUN_ID}-cleanupfail.log"
    run_expecting_failure output env \
        PATH="${stub}:${PATH}" \
        RESTORE_JOURNAL="$journal" \
        "${SCRIPT_DIR}/walg-restore.sh" verify \
        || { rm -rf "$stub"; die "the injected cleanup failure was not reported"; }
    rm -rf "$stub"

    volume="$(awk '/^  scratch volume /{print $3}' "$journal")"
    [ -n "$volume" ] || die "the failed run did not record its scratch volume"

    # The assertion: the container removal reported failure, and the volume
    # removal after it still ran.
    ! docker volume inspect "$volume" >/dev/null 2>&1 \
        || die "scratch volume ${volume} survived; cleanup stopped at the first failure"

    pass "cleanup continued past a failing step and still reported the failure"
}

test_guard_takeover_needs_confirmation() {
    log "=== guard: takeover over an existing cluster without confirmation ==="
    TEST_RUN="${RUN_ID}-noconfirm"
    ensure_running
    ensure_probe_db
    write_marker A

    local journal="/tmp/cara-verify-${RUN_ID}-noconfirm.log"
    local output found

    # takeover refuses outright while the service is running, and that refusal
    # would mask the one being tested. Stopping first puts the run at the check
    # that actually matters: PGDATA is populated and no confirmation was given.
    RESTART_POSTGRES_ON_EXIT="yes"
    docker compose stop "$COMPOSE_SERVICE" >/dev/null 2>&1 \
        || die "could not stop ${COMPOSE_SERVICE}"

    run_expecting_failure output env RESTORE_JOURNAL="$journal" \
        "${SCRIPT_DIR}/walg-restore.sh" takeover \
        || die "takeover over a populated PGDATA was accepted without --confirm-destroy"

    case "$output" in
        *--confirm-destroy*) : ;;
        *) die "refused for the wrong reason: ${output}" ;;
    esac

    # The refusal has to come before the clearing, not after. The journal is
    # where the run records what it did, so absence of the clearing line is the
    # evidence — an exit code alone cannot distinguish "refused" from "refused
    # after wiping the directory".
    ! grep -F 'clearing ' "$journal" >/dev/null 2>&1 \
        || die "the refused run cleared PGDATA before refusing"
    ! grep -E '^  destroyed +yes' "$journal" >/dev/null 2>&1 \
        || die "the refused run recorded a destroyed cluster"

    ensure_running
    RESTART_POSTGRES_ON_EXIT="no"

    found="$(markers_from docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres)"
    [ "$found" = "A" ] \
        || die "the existing cluster did not survive the refusal; expected marker A, got '${found:-<none>}'"

    pass "takeover over a populated PGDATA is refused and leaves the cluster intact"
}

# --- takeover over an existing cluster ---------------------------------------
#
# Distinct from the bare-disk test. That one deletes the volume, so anything
# that comes back proves the archive was read. This one leaves a populated
# PGDATA in place and proves the opposite half: the script clears what is there
# before restoring, rather than layering a restore on top of another
# generation's files.

test_takeover_replaces_existing_cluster() {
    log "=== takeover replaces an existing cluster ==="
    TEST_RUN="${RUN_ID}-replace"
    ensure_running
    ensure_probe_db
    assert_destructive_target

    # Recorded before the replacement. Restoring and promoting always branches
    # onto a new timeline, so this is the assertion that separates "cleared and
    # rebuilt from the archive" from "left the existing files in place" — the
    # markers alone cannot, because they are present either way.
    #
    # An unarchived marker would be the obvious alternative, but it is not
    # reliable here: 'docker compose stop' is a clean shutdown, and Postgres
    # archives the current segment on its way down. That write would come back,
    # and the test would fail for a reason that has nothing to do with takeover.
    local timeline_before timeline_after
    timeline_before="$(pg -tAc 'SELECT timeline_id FROM pg_control_checkpoint()' | tr -d '\r')"
    [ -n "$timeline_before" ] || die "could not read the current timeline"

    write_marker A
    log "taking a base backup"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"
    write_marker B
    seal_and_wait_for_archive

    local journal="/tmp/cara-verify-${RUN_ID}-replace.log"
    local found

    POSTGRES_MUST_STAY_DOWN="yes"
    docker compose stop "$COMPOSE_SERVICE" >/dev/null 2>&1 \
        || die "could not stop ${COMPOSE_SERVICE}"

    if ! RESTORE_JOURNAL="$journal" \
        "${SCRIPT_DIR}/walg-restore.sh" takeover --confirm-destroy >/dev/null 2>&1; then
        tail -5 "$journal" 2>/dev/null
        die "the replacement restore failed; full journal at ${journal}"
    fi
    POSTGRES_MUST_STAY_DOWN="no"

    # The path taken matters as much as the result. 'destroyed yes' is the
    # journal's record that it went through the clearing branch rather than the
    # "target was empty" one, which is the branch the bare-disk test exercises.
    grep -E '^  destroyed +yes' "$journal" >/dev/null \
        || die "the run did not record clearing the existing cluster"

    timeline_after="$(pg -tAc 'SELECT timeline_id FROM pg_control_checkpoint()' | tr -d '\r')"
    [ -n "$timeline_after" ] || die "could not read the timeline after the replacement"
    [ "$timeline_after" -gt "$timeline_before" ] \
        || die "timeline did not advance (${timeline_before} → ${timeline_after}); the cluster was not rebuilt"

    found="$(markers_from docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres)"
    [ "$found" = "A,B" ] \
        || die "expected markers A,B after the replacement, got '${found:-<none>}'"

    pass "takeover cleared a populated PGDATA and rebuilt it from the archive" \
        "(timeline ${timeline_before} → ${timeline_after})"

    assert_post_restore_backup "$journal"
}

# --- bare disk -------------------------------------------------------------

test_bare_disk() {
    log "=== bare-disk restore ==="
    log "this deletes the local Postgres volume and rebuilds it from the archive"
    TEST_RUN="${RUN_ID}-baredisk"
    ensure_running
    ensure_probe_db
    assert_destructive_target

    write_marker A
    log "taking a base backup"
    "${SCRIPT_DIR}/walg-backup.sh" >/dev/null 2>&1 || die "base backup failed"
    write_marker B
    seal_and_wait_for_archive

    # Re-resolve immediately before deletion. A container replaced while the
    # earlier tests were running must not inherit this confirmation.
    assert_destructive_target
    log "stopping and removing the verified container, then deleting its volume"

    # Once PGDATA is gone, starting the Compose service would initialize a new
    # cluster with a different system_identifier and archive into the old prefix.
    # Keep Postgres down on every failure path until takeover succeeds.
    POSTGRES_MUST_STAY_DOWN="yes"
    docker stop "$TARGET_CONTAINER" >/dev/null \
        || die "could not stop verified container ${TARGET_CONTAINER}"
    docker rm -f "$TARGET_CONTAINER" >/dev/null \
        || die "could not remove verified container ${TARGET_CONTAINER}"
    docker volume rm "$TARGET_VOLUME" >/dev/null \
        || die "could not delete verified volume ${TARGET_VOLUME}"

    docker volume inspect "$TARGET_VOLUME" >/dev/null 2>&1 \
        && die "${TARGET_VOLUME} still exists; the test would have read local data"
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
    grep -F 'retain=skipped' "$backup_log" >/dev/null \
        || die "post-restore backup ${name} did not record retain=skipped"
    ! grep -F 'retaining ' "$backup_log" >/dev/null \
        || die "post-restore backup ${name} unexpectedly ran retention"

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

# Refuse from the rendered config before starting anything, then verify the
# running container because it is the environment WAL-G actually uses.
assert_declared_development_stack
ensure_running
assert_runtime_development_stack
assert_archive_cluster_identity

if [ "$CONFIRM_DESTROY" = "yes" ]; then
    # Validate the target before any test pushes backups or applies retention.
    # test_bare_disk resolves it again immediately before deletion.
    assert_destructive_target
fi

test_point_in_time
test_upload_resumed
test_upload_lost
test_verify_mode

test_guard_wrong_compose_service
test_guard_wrong_archive_identity
test_guard_maintenance_lock
test_guard_cleanup_failure
test_guard_takeover_needs_confirmation

if [ "$CONFIRM_DESTROY" = "yes" ]; then
    # Replacement first, bare disk second. Replacement needs a populated PGDATA
    # to clear, which is the state the stack is already in; bare disk deletes the
    # volume outright, so running it first would leave nothing to replace.
    test_takeover_replaces_existing_cluster
    test_bare_disk
else
    log "skipping the replacement and bare-disk tests; pass --confirm-destroy to run them"
    log "without them the archive has not been proven to rebuild a data directory"
fi

[ "$RESTORE_OBJECT_STORE_ON_EXIT" = "no" ] \
    && [ "$RESTART_POSTGRES_ON_EXIT" = "no" ] \
    && [ "$POSTGRES_MUST_STAY_DOWN" = "no" ] \
    || die "verification finished with an armed cleanup state"
log "done"
