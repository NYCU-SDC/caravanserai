#!/usr/bin/env bash
#
# Restore the control-plane Postgres from the object store.
#
#   walg-restore.sh <inspect|verify|takeover> [--at '<timestamp>'] [--confirm-destroy]
#
# The mode is not a convenience wrapper over flags. Some flag combinations are
# always wrong — restoring into a scratch directory with archiving left on makes
# a throwaway instance promote onto a new timeline and publish it into the
# production archive — and a flag interface lets someone type them. Choosing a
# mode selects the whole consistent set.
#
#   inspect    scratch volume, archiving off. Recover a dropped table, read old data.
#   verify     scratch volume, archiving off. Prove a backup is restorable.
#   takeover   production volume, archiving on. This machine is taking over.
#
# Only takeover touches production, and only takeover is destructive.
#
# This is stage 2a: preflight and fetch. Configuring recovery and starting
# Postgres is stage 2b.
#
#   COMPOSE_SERVICE     compose service running Postgres   (default: postgres)
#   RESTORE_JOURNAL     where to record this run           (default: /tmp/cara-restore-<ts>.log)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RESTORE_JOURNAL="${RESTORE_JOURNAL:-/tmp/cara-restore-${RUN_ID}.log}"

# PGDATA inside the one-off container. Production restores land on the service's
# own volume at its usual path; scratch restores mount a separate volume
# somewhere else, so the two can never be confused for one another.
PROD_PGDATA="/var/lib/postgresql/data"
SCRATCH_PGDATA="/restore"

usage() {
    sed -n '3,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit "${1:-1}"
}

# Everything the run learns goes to both the terminal and the journal. During a
# real recovery the journal is the only record of what was chosen and what was
# given up, so it is written as the run goes rather than assembled at the end.
log() {
    local line
    line="$(printf '%s  %s' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*")"
    printf '%s\n' "$line"
    printf '%s\n' "$line" >>"$RESTORE_JOURNAL"
}
record() { printf '  %-18s %s\n' "$1" "$2" | tee -a "$RESTORE_JOURNAL"; }
die() { log "ERROR: $*" >&2; log "journal: ${RESTORE_JOURNAL}"; exit 1; }

# --- arguments -------------------------------------------------------------

MODE=""
TARGET_TIME=""
CONFIRM_DESTROY="no"

[ $# -ge 1 ] || usage 1
case "$1" in
    inspect|verify|takeover) MODE="$1"; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown mode '$1'" >&2; usage 1 ;;
esac

while [ $# -gt 0 ]; do
    case "$1" in
        --at) [ $# -ge 2 ] || die "--at needs a timestamp"; TARGET_TIME="$2"; shift 2 ;;
        --confirm-destroy) CONFIRM_DESTROY="yes"; shift ;;
        -h|--help) usage 0 ;;
        *) echo "unknown option '$1'" >&2; usage 1 ;;
    esac
done

# verify exists to answer "is the newest backup restorable", so a point in time
# is not a question it asks.
if [ "$MODE" = "verify" ] && [ -n "$TARGET_TIME" ]; then
    die "verify restores the latest backup; use inspect for a point in time"
fi

if [ -n "$TARGET_TIME" ]; then
    TARGET_EPOCH="$(date -d "$TARGET_TIME" +%s 2>/dev/null)" \
        || die "could not parse --at '${TARGET_TIME}'"
fi

# --- mode settings ---------------------------------------------------------

case "$MODE" in
    takeover)
        TARGET_PGDATA="$PROD_PGDATA"
        SCRATCH_VOLUME=""
        ARCHIVE_MODE="on"
        ;;
    inspect|verify)
        TARGET_PGDATA="$SCRATCH_PGDATA"
        SCRATCH_VOLUME="cara-restore-${RUN_ID}"
        ARCHIVE_MODE="off"
        ;;
esac

log "restore starting"
record "mode" "$MODE"
record "target pgdata" "$TARGET_PGDATA"
record "scratch volume" "${SCRATCH_VOLUME:-(none, production volume)}"
record "archive_mode" "$ARCHIVE_MODE"
record "recovery target" "${TARGET_TIME:-latest}"

# --- container helper ------------------------------------------------------

# All work happens in a one-off container rather than in the running service.
# For takeover the service must be stopped, so exec is not available anyway;
# for scratch modes a separate container is what keeps the scratch volume from
# ever being mounted next to production data.
#
# --no-deps keeps this from starting the compose dependencies; the archive
# reachability check below is what reports an object store that is not up.
restore_exec() { _restore_exec postgres "$@"; }
restore_exec_root() { _restore_exec root "$@"; }

_restore_exec() {
    local as_user="$1" entrypoint="$2"; shift 2
    local -a mount=()
    [ -n "$SCRATCH_VOLUME" ] && mount=(--volume "${SCRATCH_VOLUME}:${SCRATCH_PGDATA}")
    docker compose run --rm --no-deps -u "$as_user" \
        "${mount[@]}" --entrypoint "$entrypoint" "$COMPOSE_SERVICE" "$@" </dev/null
}

# --- preflight -------------------------------------------------------------

if [ "$MODE" = "takeover" ]; then
    # A running postmaster owns the production volume; compose run would mount
    # it underneath a live server. Refuse before anything else.
    if docker compose ps --status running --services 2>/dev/null | grep -qx "$COMPOSE_SERVICE"; then
        die "service '${COMPOSE_SERVICE}' is running; stop it before a takeover restore"
    fi
else
    docker volume create "$SCRATCH_VOLUME" >/dev/null \
        || die "could not create scratch volume ${SCRATCH_VOLUME}"
    log "created scratch volume ${SCRATCH_VOLUME}"

    # A freshly created volume's mount point belongs to root, and everything
    # else here runs as postgres. backup-fetch chmods the data directory as it
    # extracts, so without this it fails on the first file — and retries the
    # failure fifteen times, because WAL-G cannot tell a permission problem from
    # a transient one. The production volume needs no equivalent: Postgres
    # created it and already owns it.
    restore_exec_root sh -c "chown postgres:postgres '${SCRATCH_PGDATA}' && chmod 700 '${SCRATCH_PGDATA}'" \
        || die "could not hand ${SCRATCH_PGDATA} to the postgres user"
fi

restore_exec wal-g --version >/dev/null 2>&1 \
    || die "wal-g is not available in the ${COMPOSE_SERVICE} image"

# Reachability before anything expensive: an unreachable object store, wrong
# credentials and a missing bucket otherwise all look like "no backups".
if ! archive_err="$(restore_exec wal-g backup-list 2>&1 >/dev/null)"; then
    case "$archive_err" in
        *[Nn]o\ backups\ found*) die "the archive holds no backups; there is nothing to restore" ;;
        *) die "cannot read the archive: ${archive_err}" ;;
    esac
fi

# The check is against the target data directory, not "is any Postgres
# running". An inspect restore beside a live production database is the most
# common restore there is, and a global check would block it.
target_state="$(restore_exec sh -c "
    if [ -f '${TARGET_PGDATA}/postmaster.pid' ]; then echo locked
    elif [ -n \"\$(ls -A '${TARGET_PGDATA}' 2>/dev/null)\" ]; then echo nonempty
    else echo empty
    fi" | tr -d '\r')"

case "$target_state" in
    locked)
        die "${TARGET_PGDATA} has a postmaster.pid; a live server owns it"
        ;;
    nonempty)
        [ "$MODE" = "takeover" ] \
            || die "${TARGET_PGDATA} is not empty (scratch volumes should be; refusing to reuse one)"
        [ "$CONFIRM_DESTROY" = "yes" ] \
            || die "${TARGET_PGDATA} holds an existing cluster; pass --confirm-destroy to replace it"

        # The existing data may be newer than the backup. If the old server was
        # still accepting writes, those writes are about to be lost for good,
        # and this is the only record of what was given up.
        log "existing cluster found; recording its state before destroying it"
        restore_exec pg_controldata "$TARGET_PGDATA" 2>/dev/null \
            | grep -E "Database cluster state|Latest checkpoint location|Latest checkpoint's REDO location|Time of latest checkpoint" \
            | tee -a "$RESTORE_JOURNAL" \
            || log "WARNING: pg_controldata produced nothing; the directory may not be a cluster"

        log "clearing ${TARGET_PGDATA}"
        restore_exec sh -c "rm -rf '${TARGET_PGDATA}'/* '${TARGET_PGDATA}'/.[!.]* 2>/dev/null; true" \
            || die "could not clear ${TARGET_PGDATA}"
        record "destroyed" "yes"
        ;;
    empty)
        record "destroyed" "no (target was empty)"
        ;;
    *)
        die "could not determine the state of ${TARGET_PGDATA}"
        ;;
esac

# --- resolve the backup ----------------------------------------------------

# Never hand LATEST to backup-fetch. It is a moving reference, and the whole
# point of recording a name is to know which object this run actually used.
backups="$(restore_exec wal-g backup-list --detail --json 2>/dev/null | tr '{' '\n' | grep '"backup_name"')"
[ -n "$backups" ] || die "backup-list returned no usable records"

RESOLVED=""
RESOLVED_TIME=""
while IFS= read -r rec; do
    name="$(printf '%s' "$rec" | sed -n 's/.*"backup_name":"\([^"]*\)".*/\1/p')"
    finish="$(printf '%s' "$rec" | sed -n 's/.*"finish_time":"\([^"]*\)".*/\1/p')"
    [ -n "$name" ] && [ -n "$finish" ] || continue

    if [ -n "$TARGET_TIME" ]; then
        # A backup is only usable as a starting point if it finished before the
        # target; WAL replay covers the rest of the way.
        finish_epoch="$(date -d "$finish" +%s 2>/dev/null)" || continue
        [ "$finish_epoch" -le "$TARGET_EPOCH" ] || continue
    fi

    # backup-list is ordered oldest first, so the last one that passes is the
    # newest one that qualifies.
    RESOLVED="$name"
    RESOLVED_TIME="$finish"
done <<<"$backups"

if [ -z "$RESOLVED" ]; then
    die "no backup finished before ${TARGET_TIME}; the oldest available one starts later than the requested point"
fi

record "resolved backup" "$RESOLVED"
record "backup finished" "$RESOLVED_TIME"
if [ -n "$TARGET_TIME" ]; then
    record "chosen because" "newest backup finishing at or before ${TARGET_TIME}"
fi

# --- fetch -----------------------------------------------------------------

log "fetching ${RESOLVED} into ${TARGET_PGDATA}"
fetch_started="$(date +%s%3N)"

if ! restore_exec wal-g backup-fetch "$TARGET_PGDATA" "$RESOLVED"; then
    [ -n "$SCRATCH_VOLUME" ] && log "remove the scratch volume with: docker volume rm ${SCRATCH_VOLUME}"
    die "backup-fetch failed; ${TARGET_PGDATA} now holds a partial restore and must be cleared before retrying"
fi

fetch_ms=$(( $(date +%s%3N) - fetch_started ))
record "fetch duration" "${fetch_ms}ms"

log "fetch complete"
log "stage 2a ends here: recovery is not configured and Postgres has not been started"
log "journal: ${RESTORE_JOURNAL}"
