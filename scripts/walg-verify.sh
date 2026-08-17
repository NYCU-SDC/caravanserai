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
#   COMPOSE_SERVICE   compose service running Postgres   (default: postgres)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
PROD_PGDATA="/var/lib/postgresql/data"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
CONFIRM_DESTROY="no"

[ "${1:-}" = "--confirm-destroy" ] && CONFIRM_DESTROY="yes"

log()  { printf '%s  %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
pass() { printf '\n  PASS  %s\n\n' "$*"; }
die()  { printf '\n  FAIL  %s\n\n' "$*" >&2; exit 1; }

pg() { docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres "$@" </dev/null; }

# The point-in-time test restores in inspect mode, which publishes a port so the
# instance can be queried. Somebody else's inspect instance left running is a
# normal state and should not fail a verification run, so take the first port
# nothing is listening on.
free_port() {
    local port=5433
    while ss -tln 2>/dev/null | grep -q ":${port} "; do
        port=$(( port + 1 ))
        [ "$port" -gt 5460 ] && die "no free port between 5433 and 5460"
    done
    printf '%s' "$port"
}

ensure_running() {
    docker compose up -d --wait >/dev/null 2>&1 || die "could not bring the stack up"
}

ensure_probe_table() {
    pg -qc "CREATE TABLE IF NOT EXISTS walg_verify_probe(
                id serial primary key,
                run text not null,
                marker text not null,
                at timestamptz not null default now())" >/dev/null
}

# Each test tags its rows with its own run label. Sharing one label across both
# tests means the second one also sees the first one's markers, which reads as a
# restore fault when it is only bookkeeping.
TEST_RUN=""

write_marker() {
    pg -qc "INSERT INTO walg_verify_probe(run, marker) VALUES ('${TEST_RUN}', '$1')" >/dev/null
}

markers() {
    pg -tAc "SELECT string_agg(marker, ',' ORDER BY id)
             FROM walg_verify_probe WHERE run = '${TEST_RUN}'" | tr -d '\r'
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
    ensure_probe_table

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

    local container volume found
    container="$(awk '/^  container /{print $2}' "$journal")"
    volume="$(awk '/^  scratch volume /{print $3}' "$journal")"
    [ -n "$container" ] || die "could not find the restored container in ${journal}"

    found="$(docker exec -u postgres "$container" psql -U postgres -tAc \
        "SELECT string_agg(marker, ',' ORDER BY id)
         FROM walg_verify_probe WHERE run = '${TEST_RUN}'" | tr -d '\r')"

    docker rm -f "$container" >/dev/null 2>&1 || true
    [ -n "$volume" ] && docker volume rm "$volume" >/dev/null 2>&1 || true

    # A alone. Both markers would mean the target was ignored; neither would
    # mean replay never reached the base backup's contents.
    [ "$found" = "A" ] || die "expected marker A only, got '${found:-<none>}'"
    pass "restored to ${target}: marker A present, marker B correctly absent"
}

# --- bare disk -------------------------------------------------------------

test_bare_disk() {
    log "=== bare-disk restore ==="
    log "this deletes the local Postgres volume and rebuilds it from the archive"
    TEST_RUN="${RUN_ID}-baredisk"
    ensure_running
    ensure_probe_table

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

    local takeover_journal="/tmp/cara-verify-${RUN_ID}-baredisk.log"
    if ! RESTORE_JOURNAL="$takeover_journal" "${SCRIPT_DIR}/walg-restore.sh" takeover --confirm-destroy >/dev/null 2>&1; then
        tail -5 "$takeover_journal" 2>/dev/null
        die "the restore itself failed; full journal at ${takeover_journal}"
    fi

    local found lsn
    found="$(markers)"
    lsn="$(pg -tAc "SELECT pg_current_wal_lsn()" | tr -d '\r')"

    [ "$found" = "A,B" ] || die "expected markers A,B, got '${found:-<none>}'"
    pass "restored from an empty volume: both markers present, LSN ${lsn}"
}

# --- run -------------------------------------------------------------------

log "verification run ${RUN_ID}"

test_point_in_time

if [ "$CONFIRM_DESTROY" = "yes" ]; then
    test_bare_disk
else
    log "skipping the bare-disk test; pass --confirm-destroy to run it"
    log "without it the archive has not been proven to work on an empty disk"
fi

log "done"
