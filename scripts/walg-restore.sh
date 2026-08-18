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
#   COMPOSE_SERVICE     compose service running Postgres   (default: postgres)
#   RESTORE_JOURNAL     where to record this run           (default: /tmp/cara-restore-<ts>.log)
#   RESTORE_PORT        host port for scratch instances    (default: 5433)
#   RECOVERY_TIMEOUT    seconds to wait for replay         (default: 600)
#   LOCK_FILE           maintenance lock, shared with the backup script
#                       (default: /tmp/cara-control-plane-maintenance.lock)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
# Started here rather than after argument parsing, so the total duration covers
# the whole run. Anything declared below this line is unavailable to the
# argument checks, which call die() before the first log line is written.
RUN_STARTED="$(date +%s%3N)"
RESTORE_JOURNAL="${RESTORE_JOURNAL:-/tmp/cara-restore-${RUN_ID}.log}"
RESTORE_PORT="${RESTORE_PORT:-5433}"
RECOVERY_TIMEOUT="${RECOVERY_TIMEOUT:-600}"
LOCK_FILE="${LOCK_FILE:-/tmp/cara-control-plane-maintenance.lock}"

# PGDATA inside the one-off container. Production restores land on the service's
# own volume at its usual path; scratch restores mount a separate volume
# somewhere else, so the two can never be confused for one another.
PROD_PGDATA="/var/lib/postgresql/data"
SCRATCH_PGDATA="/restore"

usage() {
    # Everything between the shebang and the first blank comment-free line, so
    # editing the header cannot silently truncate the help text.
    sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//;s/^#$//'
    exit "${1:-1}"
}

# Everything the run learns goes to both the terminal and the journal. During a
# real recovery the journal is the only record of what was chosen and what was
# given up, so it is written as the run goes rather than assembled at the end.
# Scratch resources are removed on any exit path that leaves them behind, not
# only on success. A failed inspect used to leave a stopped container and a
# volume for someone to find later, and the next run would refuse to reuse the
# volume — so the debris also blocked the retry.
#
# 'rm -f -v', not 'rm -f'. The scratch instance is started without --rm, and it
# carries an anonymous volume over the production data directory; plain 'docker
# rm' leaves that behind, one orphan per run. -v only removes anonymous volumes,
# so the named scratch volume and the production one are never at risk.
CLEANUP_SCRATCH="no"
cleanup() {
    [ "$CLEANUP_SCRATCH" = "yes" ] || return 0
    [ -n "${RESTORE_CONTAINER:-}" ] && docker rm -f -v "$RESTORE_CONTAINER" >/dev/null 2>&1
    [ -n "${SCRATCH_VOLUME:-}" ] && docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1
    return 0
}
trap cleanup EXIT

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
        --) shift; break ;;
        -h|--help) usage 0 ;;
        *) echo "unknown option '$1'" >&2; usage 1 ;;
    esac
done

# verify exists to answer "is the newest backup restorable", so a point in time
# is not a question it asks.
if [ "$MODE" = "verify" ] && [ -n "$TARGET_TIME" ]; then
    die "verify restores the latest backup; use inspect for a point in time"
fi

# Accepting it silently would suggest these modes can destroy something.
if [ "$MODE" != "takeover" ] && [ "$CONFIRM_DESTROY" = "yes" ]; then
    die "--confirm-destroy only applies to takeover; ${MODE} never touches production"
fi

if [ -n "$TARGET_TIME" ]; then
    # An offset is required. This string is parsed twice — here by the host's
    # date, and later by Postgres inside the container reading
    # recovery_target_time — and those two do not share a timezone. Without an
    # offset the backup chosen here and the point recovery actually stops at can
    # differ by hours, silently.
    #
    # The offset has to follow a time of day. Looking only for a trailing offset
    # accepts a bare date: '2026-08-18' ends in '-18', which is indistinguishable
    # from an offset by shape alone — and that is exactly the input this check
    # exists to reject.
    case "$TARGET_TIME" in
        *[0-9]:[0-9][0-9]*[+-][0-9][0-9] \
      | *[0-9]:[0-9][0-9]*[+-][0-9][0-9][0-9][0-9] \
      | *[0-9]:[0-9][0-9]*[+-][0-9][0-9]:[0-9][0-9] \
      | *[0-9]:[0-9][0-9]*Z \
      | *[0-9]:[0-9][0-9]*UTC) : ;;
        *) die "--at needs a time of day and an explicit timezone, e.g. '2026-08-18 12:00:00+08'" ;;
    esac

    # Millisecond resolution on both sides. WAL-G records finish_time with
    # fractional seconds, and truncating to whole seconds can rank a backup that
    # finished just after the target as if it finished just before it.
    TARGET_EPOCH="$(date -d "$TARGET_TIME" +%s%3N 2>/dev/null)" \
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
# for scratch modes a separate container is what keeps production's data
# directory out of reach — see the mount handling below.
#
# --no-deps keeps this from starting the compose dependencies; the archive
# reachability check below is what reports an object store that is not up.
restore_exec() { _restore_exec postgres "$@"; }
restore_exec_root() { _restore_exec root "$@"; }

_restore_exec() { _run_in_container /dev/null "$@"; }

# Same thing but with stdin attached, for content that must not be threaded
# through a shell command string. Quoting inside sh -c "... \"$var\" ..." is
# resolved by the inner shell, which strips the quotes the content itself needs
# — restore_command lost the quotes around %f and %p that way, silently.
restore_exec_stdin() { _run_in_container - postgres "$@"; }

_run_in_container() {
    local stdin_src="$1" as_user="$2" entrypoint="$3"; shift 3
    local -a mount=()
    # compose run keeps every volume the service declares, so without the second
    # mount the production data directory would be visible — and writable — from
    # a scratch container that outlives the restore. An anonymous volume over
    # that path hides it; isolation then comes from the mount table rather than
    # from PGDATA happening to point elsewhere.
    [ -n "$SCRATCH_VOLUME" ] && mount=(
        --volume "${SCRATCH_VOLUME}:${SCRATCH_PGDATA}"
        --volume "${PROD_PGDATA}"
    )
    if [ "$stdin_src" = "-" ]; then
        docker compose run --rm --no-deps -T -u "$as_user" \
            "${mount[@]}" --entrypoint "$entrypoint" "$COMPOSE_SERVICE" "$@"
    else
        docker compose run --rm --no-deps -u "$as_user" \
            "${mount[@]}" --entrypoint "$entrypoint" "$COMPOSE_SERVICE" "$@" <"$stdin_src"
    fi
}

# --- preflight -------------------------------------------------------------

# The same lock the backup script takes. Two concurrent takeovers would clear
# and refill one PGDATA at the same time, and a backup reading PGDATA while a
# takeover deletes it wastes an upload at best. Scratch modes take it too: they
# read the same archive and would otherwise race retention.
exec 9>"$LOCK_FILE"
flock -n 9 || die "another backup or restore holds ${LOCK_FILE}; wait for it to finish"

if [ "$MODE" = "takeover" ]; then
    # A running postmaster owns the production volume; compose run would mount
    # it underneath a live server. Refuse before anything else.
    if docker compose ps --status running --services 2>/dev/null | grep -qx "$COMPOSE_SERVICE"; then
        die "service '${COMPOSE_SERVICE}' is running; stop it before a takeover restore"
    fi

    # cara-server keeps its own view of the database. If it is still running it
    # will reconnect to a cluster that has been replaced underneath it. This
    # only catches the case where it is a compose service; when it runs as a
    # host binary or a separate unit, stopping it is a runbook step and nothing
    # here can verify it.
    if docker compose ps --status running --services 2>/dev/null | grep -qx "cara-server"; then
        die "cara-server is running; stop it before replacing the database"
    fi
else
    docker volume create "$SCRATCH_VOLUME" >/dev/null \
        || die "could not create scratch volume ${SCRATCH_VOLUME}"
    CLEANUP_SCRATCH="yes"
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

# --- resolve the backup ----------------------------------------------------

# Resolved before anything is destroyed. Reachability alone is not enough: a
# point-in-time target may have no backup old enough to serve it, and finding
# that out after clearing the data directory would leave nothing on either
# side. Know what is going in before removing what is there.
#
# Never hand LATEST to backup-fetch either. It is a moving reference, and the
# whole point of recording a name is to know which object this run used.
backups="$(restore_exec wal-g backup-list --detail --json 2>/dev/null | tr '{' '\n' | grep '"backup_name"')"
[ -n "$backups" ] || die "backup-list returned no usable records"

RESOLVED=""
RESOLVED_TIME=""
PARSED=0
while IFS= read -r rec; do
    name="$(printf '%s' "$rec" | sed -n 's/.*"backup_name":"\([^"]*\)".*/\1/p')"
    finish="$(printf '%s' "$rec" | sed -n 's/.*"finish_time":"\([^"]*\)".*/\1/p')"
    [ -n "$name" ] || continue
    PARSED=$(( PARSED + 1 ))

    if [ -n "$TARGET_TIME" ]; then
        # Only needed to compare against a target; a restore-to-latest does not
        # care when the backup finished.
        [ -n "$finish" ] || continue
        # A backup is only usable as a starting point if it finished before the
        # target; WAL replay covers the rest of the way.
        finish_epoch="$(date -d "$finish" +%s%3N 2>/dev/null)" || continue
        [ "$finish_epoch" -le "$TARGET_EPOCH" ] || continue
    fi

    # backup-list is ordered oldest first, so the last one that passes is the
    # newest one that qualifies.
    RESOLVED="$name"
    RESOLVED_TIME="$finish"
done <<<"$backups"

if [ "$PARSED" -eq 0 ]; then
    # Distinct from "no backup is old enough". The field names come from WAL-G
    # v3.0.8; if that moved, this is where it shows up, and reporting it as a
    # missing backup would send the search in the wrong direction entirely.
    die "backup-list returned records but none could be parsed; check whether WAL-G's output format changed"
fi

if [ -z "$RESOLVED" ]; then
    die "no backup finished at or before ${TARGET_TIME}; the oldest available one is newer than the requested point"
fi

record "resolved backup" "$RESOLVED"
record "backup finished" "$RESOLVED_TIME"
if [ -n "$TARGET_TIME" ]; then
    record "chosen because" "newest backup finishing at or before ${TARGET_TIME}"
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
        # The trailing 'true' is needed — an already-empty directory leaves the
        # globs unexpanded and rm exits non-zero — but it also makes the exit
        # status meaningless, so emptiness is checked rather than assumed. A
        # half-cleared directory would have backup-fetch layer a restore on top
        # of leftovers and produce a cluster mixing two generations.
        restore_exec sh -c "rm -rf '${TARGET_PGDATA}'/* '${TARGET_PGDATA}'/.[!.]* 2>/dev/null; true"
        remaining="$(restore_exec sh -c "ls -A '${TARGET_PGDATA}' 2>/dev/null | wc -l" | tr -d '\r')"
        [ "${remaining:-1}" = "0" ] \
            || die "could not clear ${TARGET_PGDATA}; ${remaining} entries remain"
        record "destroyed" "yes"
        ;;
    empty)
        record "destroyed" "no (target was empty)"
        ;;
    *)
        die "could not determine the state of ${TARGET_PGDATA}"
        ;;
esac

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

# --- configure -------------------------------------------------------------
#
# Everything here happens after the fetch, never before. postgresql.auto.conf
# lives inside PGDATA and backup-fetch replaces PGDATA wholesale, so settings
# written earlier are overwritten with no warning — and the only symptom is
# Postgres refusing to start with "must specify restore_command", which reads
# like a corrupt backup rather than a script ordering bug.

conf="${TARGET_PGDATA}/postgresql.auto.conf"

# recovery.signal is what puts Postgres into archive recovery; restore_command
# is how it gets the WAL. Both are required — archive recovery never falls back
# to reading WAL already present in pg_wal/, that is crash recovery's behaviour.
settings="restore_command = 'wal-g wal-fetch \"%f\" \"%p\"'"

if [ -n "$TARGET_TIME" ]; then
    # Without recovery_target_action the default is 'pause': the server reaches
    # the target, stops, accepts no writes and logs nothing. It looks hung.
    settings="${settings}
recovery_target_time = '${TARGET_TIME}'
recovery_target_action = 'promote'"
fi

printf '%s\n' "$settings" \
    | restore_exec_stdin sh -c "touch '${TARGET_PGDATA}/recovery.signal' && cat >> '${conf}'" \
    || die "could not write recovery settings to ${conf}"

written="$(restore_exec sh -c "grep '^restore_command' '${conf}' | tail -1" | tr -d '\r')"
[ -n "$written" ] || die "restore_command did not reach ${conf}"

log "recovery configured"
# The line as it actually landed, not as it was intended. Quoting around %f and
# %p has been lost here before, by a shell in the middle rewriting the string —
# and with paths that contain no spaces the loss is invisible until it is not.
record "restore_command" "$written"
[ -n "$TARGET_TIME" ] && record "recovery_target_action" "promote"

# --- start -----------------------------------------------------------------

if [ "$MODE" = "takeover" ]; then
    startup_started="$(date +%s%3N)"
    log "starting ${COMPOSE_SERVICE}"
    # takeover restarts the existing container, so its log still holds
    # everything from before the restore. Anchoring to this moment keeps the
    # journal to lines that describe this recovery — during an incident that
    # record is the only account of what happened, and stale checkpoint lines
    # from days earlier read as if they were part of it.
    START_MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    # 'up -d' rather than 'start': the container may not exist at all. On a
    # fresh machine it never has, and the bare-disk verification removes it in
    # order to delete the volume underneath. 'start' fails in both cases.
    up_err="$(mktemp)"
    if ! docker compose up -d "$COMPOSE_SERVICE" >/dev/null 2>"$up_err"; then
        log "$(cat "$up_err")"
        rm -f "$up_err"
        die "could not start ${COMPOSE_SERVICE}"
    fi
    rm -f "$up_err"
    target_psql() { docker compose exec -T -u postgres "$COMPOSE_SERVICE" psql -U postgres "$@" </dev/null; }
    target_logs() { docker compose logs --no-log-prefix --since "$START_MARK" "$COMPOSE_SERVICE"; }
    target_alive() { docker compose ps --status running --services 2>/dev/null | grep -qx "$COMPOSE_SERVICE"; }
else
    # A scratch instance must not archive. It promotes onto a new timeline when
    # recovery ends, and with archiving on it would publish that timeline into
    # the production archive — two throwaway instances would then collide on
    # identically named segments holding different data.
    startup_started="$(date +%s%3N)"
    if [ "$MODE" = "inspect" ]; then
        log "starting scratch instance on port ${RESTORE_PORT}"
    else
        log "starting scratch instance (no published port; queried through docker exec)"
    fi
    # Let compose name the container. Passing --name produces a name compose
    # does not recognise as one of its services, so every later compose command
    # — including ones unrelated to a restore — warns about an orphan container
    # for as long as an inspect instance is left running. Noise during a
    # recovery is worth avoiding; the id is captured instead.
    # stderr is captured rather than discarded: the usual failure here is the
    # port already being held by an earlier inspect instance, and that reason
    # only exists in Docker's message.
    # Only inspect publishes a port. verify is queried through 'docker exec' and
    # nobody connects to it from the host, so publishing would give it a way to
    # collide with a long-lived inspect instance for no benefit at all.
    publish=()
    [ "$MODE" = "inspect" ] && publish=(--publish "${RESTORE_PORT}:5432")

    start_err="$(mktemp)"
    # '|| true' is load-bearing under 'set -e': a failing command substitution
    # in an assignment aborts the script at that line, which is how the real
    # Docker message ended up being discarded rather than reported.
    # This one does not go through _run_in_container, so it needs the same
    # anonymous volume over the production data directory — and needs it more
    # than the short-lived helpers do. An inspect instance is left running on
    # purpose, so it is the only container here that outlives the restore with a
    # live postmaster in it, and without this it would have production's PGDATA
    # mounted and writable the whole time.
    RESTORE_CONTAINER="$(docker compose run -d \
        "${publish[@]}" \
        --env PGDATA="$SCRATCH_PGDATA" \
        --volume "${SCRATCH_VOLUME}:${SCRATCH_PGDATA}" \
        --volume "${PROD_PGDATA}" \
        "$COMPOSE_SERVICE" postgres -c archive_mode=off 2>"$start_err" | tr -d '\r')" || true
    if [ -z "$RESTORE_CONTAINER" ]; then
        log "$(cat "$start_err")"
        rm -f "$start_err"
        die "could not start the scratch instance; if port ${RESTORE_PORT} is taken, find the holder with: docker ps --filter publish=${RESTORE_PORT}"
    fi
    rm -f "$start_err"
    record "container" "$RESTORE_CONTAINER"
    target_psql() { docker exec -u postgres "$RESTORE_CONTAINER" psql -U postgres "$@" </dev/null; }
    target_logs() { docker logs "$RESTORE_CONTAINER" 2>&1; }
    target_alive() { [ "$(docker inspect -f '{{.State.Running}}' "$RESTORE_CONTAINER" 2>/dev/null)" = "true" ]; }
fi

# An open port is not a finished recovery. Postgres accepts connections while
# still replaying, and querying it then would report a database that is only
# partly caught up.
log "waiting for recovery to finish"
replay_started="$(date +%s%3N)"
deadline=$(( $(date +%s) + RECOVERY_TIMEOUT ))

while :; do
    # Check liveness before the query. A startup failure kills Postgres within a
    # second or two, and polling only for pg_is_in_recovery() would turn that
    # into a wait for the whole timeout before reporting something unhelpful.
    if ! target_alive; then
        log "the instance stopped during recovery:"
        target_logs 2>&1 | tail -30 | tee -a "$RESTORE_JOURNAL" || true

        # Postgres refuses to promote when it runs out of WAL before seeing a
        # transaction later than the target — it cannot claim to have reached a
        # point it never observed. The usual cause is a target after the last
        # write that was archived, not a damaged backup.
        if target_logs 2>&1 | grep -q "recovery ended before configured recovery target"; then
            die "no transaction after ${TARGET_TIME} has been archived, so that point cannot be reached. Pick an earlier target, or restore to latest."
        fi
        die "Postgres exited during recovery; see the log above"
    fi

    state="$(target_psql -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d '\r' || true)"
    [ "$state" = "f" ] && break

    if [ "$(date +%s)" -ge "$deadline" ]; then
        log "recovery log:"
        target_logs 2>&1 | tail -30 | tee -a "$RESTORE_JOURNAL" || true
        die "recovery did not finish within ${RECOVERY_TIMEOUT}s (last state: ${state:-unreachable})"
    fi
    sleep 2
done

replay_ms=$(( $(date +%s%3N) - replay_started ))
record "recovery duration" "${replay_ms}ms"
record "startup duration" "$(( replay_started - startup_started ))ms"

# The recovery lines are the evidence that the restore did what was asked: which
# segments were fetched, where replay stopped, and on which timeline it came up.
log "recovery log:"
target_logs 2>&1 | grep -iE "recovery|redo|consistent|promot|restored log file|selected new timeline" \
    | tail -20 | tee -a "$RESTORE_JOURNAL" || true

if [ -z "$TARGET_TIME" ]; then
    log "note: a wal-fetch failure at the end of the log above is normal — it is how"
    log "      Postgres detects the end of the archive, not a fault"
fi

timeline="$(target_psql -tAc 'SELECT timeline_id FROM pg_control_checkpoint()' 2>/dev/null | tr -d '\r' || true)"
lsn="$(target_psql -tAc 'SELECT pg_current_wal_lsn()' 2>/dev/null | tr -d '\r' || true)"
record "finished" "timeline ${timeline:-?}, LSN ${lsn:-?}"

# --- postflight ------------------------------------------------------------

# recovery.signal clears itself — Postgres renames it to recovery.done — but the
# settings do not. restore_command is appended on every restore, so without this
# postgresql.auto.conf grows a duplicate line each time; Postgres takes the last
# one so behaviour is unaffected, but the file becomes misleading to read and
# drifts from the "managed by ALTER SYSTEM" convention it claims in its header.
target_psql -c 'ALTER SYSTEM RESET restore_command' >/dev/null 2>&1 || true
if [ -n "$TARGET_TIME" ]; then
    target_psql -c 'ALTER SYSTEM RESET recovery_target_time' >/dev/null 2>&1 || true
    target_psql -c 'ALTER SYSTEM RESET recovery_target_action' >/dev/null 2>&1 || true
fi
log "cleared the now-inert recovery settings"

case "$MODE" in
    takeover)
        # Re-baselines onto the new timeline so the next restore is a single
        # replay rather than one that has to cross a branch point. It runs in
        # the background on purpose: the control plane is already serving, and
        # RTO ends here. Time to backup completion is a separate measurement.
        # Release the maintenance lock first. The backup script takes the same
        # one, and with flock -n it would fail immediately rather than wait —
        # silently, in the background, where nobody would see it.
        exec 9>&-
        log "triggering a background base backup (does not gate service restoration)"
        nohup "${SCRIPT_DIR}/walg-backup.sh" >>"${RESTORE_JOURNAL}.backup" 2>&1 &
        record "post-restore backup" "pid $!, log ${RESTORE_JOURNAL}.backup"
        log "RTO ends here. Start cara-server once this database is confirmed healthy."
        ;;
    verify)
        # Reaching a non-recovery state proves the mechanism; answering a query
        # proves the data is usable rather than merely present.
        dbs="$(target_psql -tAc 'SELECT count(*) FROM pg_database' 2>/dev/null | tr -d '\r' || true)"
        [ -n "$dbs" ] || die "the restored instance did not answer a query"
        record "verified" "instance answers queries, ${dbs} databases"
        log "cleaning up"
        docker rm -f -v "$RESTORE_CONTAINER" >/dev/null 2>&1 || true
        docker volume rm "$SCRATCH_VOLUME" >/dev/null 2>&1 || true
        CLEANUP_SCRATCH="no"
        log "verify passed"
        ;;
    inspect)
        # The whole point of inspect is to leave something to query.
        CLEANUP_SCRATCH="no"
        record "connect with" "psql -h localhost -p ${RESTORE_PORT} -U postgres"
        log "the instance is left running so you can query it"
        log "when finished: docker rm -f -v ${RESTORE_CONTAINER} && docker volume rm ${SCRATCH_VOLUME}"
        ;;
esac

record "total duration" "$(( $(date +%s%3N) - RUN_STARTED ))ms"
log "journal: ${RESTORE_JOURNAL}"
