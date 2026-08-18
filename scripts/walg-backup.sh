#!/usr/bin/env bash
#
# Take a full base backup of the control-plane Postgres and apply retention.
#
# WAL-G runs inside the Postgres container because that is where PGDATA and the
# WAL-G environment live. This script stays outside so that scheduling, logging
# and retention policy are not baked into the image.
#
# Everything it prints on stdout is meant to be captured by cron or a systemd
# timer; the duration and size lines are the measurements CARA-70 owes.
#
#   COMPOSE_SERVICE     compose service running Postgres     (default: postgres)
#   PGDATA              data directory inside the container  (default: /var/lib/postgresql/data)
#   WALG_RETAIN_FULL    how many full backups to keep        (default: 7)
#   LOCK_FILE           maintenance lock, shared with the restore script
#                       (default: /tmp/cara-control-plane-maintenance.lock)

set -euo pipefail

# Resolve and enter the repo root. Every docker compose call needs to find
# docker-compose.yaml and the .env beside it, and cron does not run from here.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_SERVICE="${COMPOSE_SERVICE:-postgres}"
PGDATA="${PGDATA:-/var/lib/postgresql/data}"
WALG_RETAIN_FULL="${WALG_RETAIN_FULL:-7}"
LOCK_FILE="${LOCK_FILE:-/tmp/cara-control-plane-maintenance.lock}"

log() { printf '%s  %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

# -u postgres matters twice over. 'docker compose exec' runs as root by
# default, and libpq derives the database user from the OS user, so wal-g would
# connect as 'root' and be rejected — there is no such role. Running as postgres
# also matches the ownership of PGDATA, which backup-push has to read.
#
# stdin comes from /dev/null because 'docker compose exec' attaches it even
# with -T. Backgrounded, that earns the whole script a SIGTTIN and it stops
# holding the lock instead of finishing. Nothing here reads input, and an
# unattended job should behave the same whether or not it has a terminal.
pg_exec() { docker compose exec -T -u postgres "$COMPOSE_SERVICE" "$@" </dev/null; }

# --- preflight -------------------------------------------------------------

# Reject 0 as firmly as a non-number: 'delete retain FULL 0' is a request to
# keep nothing, and this script must never be one typo away from emptying the
# archive it exists to maintain.
case "$WALG_RETAIN_FULL" in
    ''|*[!0-9]*|0) die "WALG_RETAIN_FULL must be a positive integer, got '${WALG_RETAIN_FULL}'" ;;
esac

# One maintenance lock for the whole family of scripts, held for the entire run
# including retention. It stops cron starting a second backup on top of a slow
# one, and it stops a restore clearing PGDATA while this is still reading it.
# A caller that already holds the lock says so through the environment, and this
# skips taking it. flock is per open file description, so opening the same file
# here would create a second one that this script's own flock then refuses —
# verified: under walg-verify.sh the child is refused and the run dies, rather
# than sharing the lock it was meant to cooperate with.
if [ "${CARA_MAINTENANCE_LOCK_HELD:-0}" = "1" ]; then
    log "maintenance lock already held by the caller"
else
    exec 9>"$LOCK_FILE"
    flock -n 9 || die "another run holds ${LOCK_FILE}; a backup is already in progress"
fi

docker compose ps --status running --services 2>/dev/null | grep -qx "$COMPOSE_SERVICE" \
    || die "service '$COMPOSE_SERVICE' is not running; a base backup needs a live cluster"

pg_exec wal-g --version >/dev/null 2>&1 \
    || die "wal-g is not available inside '$COMPOSE_SERVICE'"

# jq reads the backup catalogue. Checked here, with the other preflight
# requirements, so a machine without it fails before anything is touched rather
# than part-way through.
#
# A dependency is acceptable at this level; a compiled one would not be. Moving
# this parsing into a Go helper would mean needing a working build to restore
# the database you are in the middle of losing.
command -v jq >/dev/null \
    || die "jq is required to read the backup catalogue; install it (apt install jq)"

# Prove the archive is readable before doing anything expensive. Without this,
# an unreachable object store, wrong credentials or a missing bucket all look
# identical to "there are no backups yet" further down.
#
# An empty archive is a legitimate state on the first ever run, and WAL-G may
# report it either as success with no rows or as an error. Both are accepted;
# anything else is fatal and reported with WAL-G's own message.
if ! archive_err="$(pg_exec wal-g backup-list 2>&1 >/dev/null)"; then
    case "$archive_err" in
        *[Nn]o\ backups\ found*) : ;;
        *) die "cannot read the archive: ${archive_err}" ;;
    esac
fi

# Reads the JSON rather than the table: the table's first column is only the
# name by convention, and nothing warns if that layout moves. Reachability is
# established above, so an empty result here is the empty-archive case and is
# correct; '|| true' is scoped to jq alone so a genuinely unreadable catalogue
# still fails rather than being reported as "no new backup appeared".
list_backup_names() {
    local catalogue
    catalogue="$(pg_exec wal-g backup-list --detail --json 2>/dev/null)" || return 0
    printf '%s' "$catalogue" | jq -r '.[].backup_name' 2>/dev/null | sort || true
}

# --- backup ----------------------------------------------------------------

before="$(list_backup_names)"

log "starting base backup of $PGDATA"

# Millisecond resolution: a backup of the current dev database finishes in well
# under a second, and whole-second timing reports that as 0s. The number is one
# of this ticket's measurements, so it has to stay meaningful at both ends of
# the range it will eventually cover.
started="$(date +%s%3N)"

pg_exec wal-g backup-push "$PGDATA" \
    || die "backup-push failed; the archive may hold an incomplete backup — list it and clean up before retrying"

elapsed_ms=$(( $(date +%s%3N) - started ))
log "backup-push finished in ${elapsed_ms}ms"

# Resolve the name by diffing the list rather than reading LATEST. LATEST is a
# moving reference, and the point of recording a name is to know exactly which
# object this run produced.
after="$(list_backup_names)"
created="$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") | tr -d '\r')"

if [ -z "$created" ]; then
    die "backup-push reported success but no new backup appeared in backup-list"
fi

if [ "$(printf '%s\n' "$created" | wc -l)" -gt 1 ]; then
    # The lock prevents this between scheduled runs, but not against someone
    # running backup-push by hand in the container at the same moment.
    log "WARNING: more than one new backup appeared: $(printf '%s ' $created)"
    created="$(printf '%s\n' "$created" | tail -n 1)"
fi

log "created backup ${created}"

# Sizes come from WAL-G rather than being measured here, so the recorded numbers
# are the ones WAL-G itself reports. The whole record is logged as well as the
# parsed sizes: the extra fields (LSNs, pg_version, system_identifier) cost
# nothing to keep and are exactly what a later investigation wants.
#
# Still pinned to the field names WAL-G v3.0.8 emits — the version the image
# installs — but selecting on the name rather than matching text means a record
# that does not exist reads as absent instead of as a near-miss on some other
# backup's fields.
detail="$(pg_exec wal-g backup-list --detail --json 2>/dev/null \
    | jq -c --arg n "$created" '.[] | select(.backup_name == $n)' 2>/dev/null || true)"

if [ -n "$detail" ]; then
    log "detail: ${detail}"
    compressed="$(printf '%s' "$detail"   | jq -r '.compressed_size   // empty')"
    uncompressed="$(printf '%s' "$detail" | jq -r '.uncompressed_size // empty')"
else
    # Not fatal — the backup itself succeeded — but the sizes are one of this
    # ticket's deliverables, so losing them silently is not acceptable either.
    log "WARNING: could not read the backup's detail record; sizes not captured"
    compressed=""
    uncompressed=""
fi

# --- retention -------------------------------------------------------------

# Retention must go through 'delete retain FULL', never by age. WAL is only
# safe to remove once no retained base backup still needs it, and WAL-G is what
# knows that relationship.
log "retaining ${WALG_RETAIN_FULL} full backups"
pg_exec wal-g delete retain FULL "$WALG_RETAIN_FULL" --confirm \
    || die "retention failed; the new backup exists but old ones were not pruned"

log "done: backup=${created} duration=${elapsed_ms}ms" \
    "compressed=${compressed:-?} uncompressed=${uncompressed:-?}" \
    "retain=${WALG_RETAIN_FULL}"
