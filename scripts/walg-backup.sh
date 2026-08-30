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

# Read once and preserve the distinction between a valid empty archive and a
# transport, credential or format failure. The caller captures stdout, so
# diagnostics stay on stderr.
read_backup_catalogue() {
    local catalogue err_file message
    err_file="$(mktemp)"
    if catalogue="$(pg_exec wal-g backup-list --detail --json 2>"$err_file")"; then
        rm -f "$err_file"
        printf '%s' "$catalogue"
        return 0
    fi

    message="$(cat "$err_file")"
    rm -f "$err_file"
    case "$message" in
        *[Nn]o\ backups\ found*) printf '[]' ;;
        *)
            printf 'cannot read the archive: %s\n' "$message" >&2
            return 1
            ;;
    esac
}

backup_names_from_catalogue() {
    local catalogue="$1"
    printf '%s' "$catalogue" \
        | jq -r '
            if type != "array" then error("catalogue is not an array")
            else .[] | if (.backup_name | type) == "string" and (.backup_name | length) > 0
                then .backup_name else error("record has no backup_name") end
            end' \
        | sort
}

# --- backup ----------------------------------------------------------------

catalogue_before="$(read_backup_catalogue)" \
    || die "could not read backup catalogue before backup-push"
before="$(backup_names_from_catalogue "$catalogue_before")" \
    || die "could not parse backup catalogue before backup-push"

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
catalogue_after="$(read_backup_catalogue)" \
    || die "could not read backup catalogue after backup-push"
after="$(backup_names_from_catalogue "$catalogue_after")" \
    || die "could not parse backup catalogue after backup-push"
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
#
# Two different kinds of failure, so two different responses. Not finding the
# record at all is fatal: it means the catalogue this run just read does not
# contain the backup this run just created, and nothing after that can be
# trusted. Missing size fields are not — the backup exists and is restorable,
# and the sizes are telemetry. Failing here would leave a valid backup behind
# an exit code, skip retention entirely (it runs below), and turn one renamed
# WAL-G field into an archive that grows without bound.
detail="$(printf '%s' "$catalogue_after" \
    | jq -ce --arg n "$created" '
        map(select(.backup_name == $n))
        | if length == 1 then .[0] else error("expected exactly one detail record") end')" \
    || die "backup ${created} exists but its detail record could not be resolved"
log "detail: ${detail}"

compressed="$(printf '%s' "$detail" | jq -r '.compressed_size   // empty')"
uncompressed="$(printf '%s' "$detail" | jq -r '.uncompressed_size // empty')"
if [ -z "$compressed" ] || [ -z "$uncompressed" ]; then
    log "WARNING: backup ${created} succeeded but its size fields could not be read;" \
        "WAL-G's detail format may have changed"
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
