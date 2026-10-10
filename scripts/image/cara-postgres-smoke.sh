#!/usr/bin/env bash
set -Eeuo pipefail

# Smoke test for ghcr.io/nycu-sdc/cara-postgres:16 (CARA-125).
#
# Checks the image contract on the host's Docker: PID 1, archiving off by
# default, recorded versions, the postgres UID, certificate permissions, a
# full + differential backup restored from a local repository, the async
# archiving crash that tini prevents, and repo-host mode.
#
# Needs no repo host, TLS server or S3. Every container it creates carries a
# unique suffix and is removed on exit together with its volumes, including the
# anonymous PGDATA volume the base image declares; the image is left in place.
#
# A negative check ("cannot read", "not initialized", "did not restart") only
# passes on an explicit answer from inside the container. A Docker call that
# fails is a failed test, never a "no".

image="${CARA_POSTGRES_IMAGE:-ghcr.io/nycu-sdc/cara-postgres:16}"

log() { printf '[cara-postgres-smoke] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }
pass() { log "ok: $*"; }

for command in docker python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done
docker image inspect "$image" >/dev/null 2>&1 || fail "image $image not found; run make build-cara-postgres-image"

suffix="${PPID}-$$"
work="$(mktemp -d "${TMPDIR:-/tmp}/cara-postgres-smoke.XXXXXX")"
db="cara-pg-smoke-db-${suffix}"
async_init="cara-pg-smoke-async-init-${suffix}"
async_control="cara-pg-smoke-async-control-${suffix}"
repohost="cara-pg-smoke-repohost-${suffix}"
tls_volume="cara-pg-smoke-tls-${suffix}"
# Anonymous volumes of every container started here, so cleanup can prove they
# are gone rather than assume `docker rm -v` removed them.
anon_volumes=()

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if (( status != 0 )); then
		for c in "$db" "$async_init" "$async_control" "$repohost"; do
			if docker inspect "$c" >/dev/null 2>&1; then
				printf '\n--- %s (last 40 lines) ---\n' "$c"
				docker logs --tail 40 "$c" 2>&1 || true
			fi
		done
	fi
	for c in "$db" "$async_init" "$async_control" "$repohost"; do
		remove_container "$c" || true
	done
	docker volume rm -f "$tls_volume" >/dev/null 2>&1 || true
	for v in "${anon_volumes[@]}"; do
		if docker volume inspect "$v" >/dev/null 2>&1; then
			docker volume rm -f "$v" >/dev/null 2>&1 || true
		fi
		if docker volume inspect "$v" >/dev/null 2>&1; then
			log "FAIL: volume $v created by this run is still present"
			status=1
		fi
	done
	case "$(basename "$work")" in
		cara-postgres-smoke.*) rm -rf -- "$work" ;;
		*) log "refusing to remove unexpected temp path: $work" ;;
	esac
	exit "$status"
}
trap cleanup EXIT INT TERM

# remove_container name — removes the container and its anonymous volumes.
remove_container() {
	docker rm -fv "$1" >/dev/null 2>&1
}

# track_volumes name — records the anonymous volumes Docker created for name.
track_volumes() {
	local mounts
	mounts="$(docker inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$1")" ||
		fail "cannot inspect $1"
	for v in $mounts; do
		[[ "$v" == "$tls_volume" ]] || anon_volumes+=("$v")
	done
}

wait_until() {
	timeout_seconds=$1
	description=$2
	shift 2
	deadline=$((SECONDS + timeout_seconds))
	until "$@"; do
		if (( SECONDS >= deadline )); then
			fail "timed out waiting for $description"
		fi
		sleep 1
	done
}

pg_ready() { docker exec "$1" pg_isready -U postgres -q >/dev/null 2>&1; }
psql_in() { docker exec -u postgres "$1" psql -X -qtAc "$2"; }
as_postgres() { local c=$1; shift; docker exec -u postgres "$c" "$@"; }
postmaster_pid() {
	local pid
	pid="$(docker exec "$1" head -1 /var/lib/postgresql/data/postmaster.pid)" || fail "$1: cannot read postmaster.pid"
	[[ "$pid" =~ ^[0-9]+$ ]] || fail "$1: postmaster.pid holds '$pid'"
	printf '%s' "$pid"
}

# start_db name config [docker run args...] [-- command...] — a PostgreSQL-mode
# container with the given pgBackRest config mounted where the Agent will
# mount it. A command is only needed alongside --entrypoint, which makes Docker
# drop the image's default command.
start_db() {
	local name=$1 config=$2
	shift 2
	local run_args=() command=()
	while (( $# )); do
		if [[ "$1" == "--" ]]; then
			shift
			command=("$@")
			break
		fi
		run_args+=("$1")
		shift
	done
	docker run -d --name "$name" \
		-e POSTGRES_PASSWORD=smoke \
		-v "$config:/etc/pgbackrest/pgbackrest.conf:ro" \
		"${run_args[@]}" "$image" "${command[@]}" >/dev/null
	track_volumes "$name"
	wait_until 60 "$name to accept connections" pg_ready "$name"
}

# enable_archiving name — turns archiving on after initialization, as placement
# will. Doing it at first start would break initdb's temporary server.
enable_archiving() {
	local name=$1
	psql_in "$name" "ALTER SYSTEM SET archive_mode = 'on'"
	psql_in "$name" "ALTER SYSTEM SET archive_command = 'pgbackrest --stanza=smoke archive-push %p'"
	docker restart "$name" >/dev/null
	wait_until 60 "$name to accept connections after restart" pg_ready "$name"
	[[ "$(psql_in "$name" 'SHOW archive_mode')" == "on" ]] || fail "$name: archive_mode did not turn on"
}

# --- certificates -------------------------------------------------------------
# Generated by root in a throwaway container so the files can be owned by the
# image's postgres UID without sudo on the host, exactly as the Agent will
# deliver them.
docker volume create "$tls_volume" >/dev/null
docker run --rm --user 0 --entrypoint sh -v "$tls_volume:/tls" "$image" -c '
	set -e
	cd /tls
	openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=smoke-ca" \
		-keyout ca.key -out ca.crt 2>/dev/null
	for name in server client; do
		openssl req -newkey rsa:2048 -nodes -subj "/CN=$name" \
			-keyout "$name.key" -out "$name.csr" 2>/dev/null
		openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
			-days 1 -out "$name.crt" 2>/dev/null
	done
	rm -f ca.key ./*.csr ca.srl
	chown 999:999 server.key client.key
	chmod 0600 server.key client.key
	chmod 0644 ca.crt server.crt client.crt
'

# --- PostgreSQL mode: contract ------------------------------------------------
cat >"$work/local.conf" <<'EOF'
[global]
repo1-path=/var/lib/postgresql/smoke-repo
repo1-retention-full=2
compress-type=zst
start-fast=y
log-level-console=warn

[smoke]
pg1-path=/var/lib/postgresql/data
EOF
chmod 0644 "$work/local.conf"

log "starting $db"
start_db "$db" "$work/local.conf" -v "$tls_volume:/etc/cara/pgbackrest/tls:ro"

[[ "$(docker exec "$db" cat /proc/1/comm)" == "tini" ]] || fail "PID 1 is not tini"
db_postmaster="$(postmaster_pid "$db")"
[[ "$db_postmaster" != "1" ]] || fail "the postmaster is PID 1"
pass "PID 1 is tini, postmaster is PID $db_postmaster"

[[ "$(psql_in "$db" 'SHOW archive_mode')" == "off" ]] || fail "archive_mode is not off by default"
pass "archive_mode is off by default"

[[ "$(docker exec "$db" id -u postgres):$(docker exec "$db" id -g postgres)" == "999:999" ]] ||
	fail "postgres is not UID/GID 999"
pass "postgres is 999:999"

versions_file="$(docker exec "$db" cat /usr/share/cara/image-versions)" || fail "cannot read the version file"
file_value() { printf '%s\n' "$versions_file" | sed -n "s/^$1=//p"; }
label_value() { docker image inspect "$image" --format "{{index .Config.Labels \"$1\"}}"; }
running_pg="$(docker exec "$db" postgres --version | awk '{print $3}')" || fail "cannot run postgres --version"
running_pgbr="$(docker exec "$db" pgbackrest version | awk '{print $2}')" || fail "cannot run pgbackrest version"
[[ -n "$running_pg" && -n "$running_pgbr" ]] || fail "could not parse the running versions"
[[ "$running_pg" == "$(file_value postgres)" ]] || fail "postgres $running_pg differs from version file $(file_value postgres)"
[[ "$running_pgbr" == "$(file_value pgbackrest)" ]] || fail "pgbackrest $running_pgbr differs from version file $(file_value pgbackrest)"
# Labels carry the Debian package versions, which extend the binaries' own.
[[ "$(label_value dev.cara.postgres.version)" == "$running_pg-"* ]] ||
	fail "postgres label $(label_value dev.cara.postgres.version) does not match $running_pg"
[[ "$(label_value dev.cara.pgbackrest.version)" == "$running_pgbr-"* ]] ||
	fail "pgbackrest label $(label_value dev.cara.pgbackrest.version) does not match $running_pgbr"
[[ "$(label_value dev.cara.base.image)" == "$(file_value base)" ]] || fail "base label differs from version file"
pass "versions agree: postgres $running_pg, pgbackrest $running_pgbr"

# key_access uid — "readable" or "denied" as seen by uid inside the container;
# "missing" if the key is not there at all.
key_access() {
	docker exec -u "$1" "$db" sh -c '
		key=/etc/cara/pgbackrest/tls/client.key
		[ -e "$key" ] || { echo missing; exit 0; }
		if cat "$key" >/dev/null 2>&1; then echo readable; else echo denied; fi'
}
access="$(key_access 999:999)" || fail "cannot check key access as postgres"
[[ "$access" == "readable" ]] || fail "postgres sees the client key as $access"
access="$(key_access 1000:1000)" || fail "cannot check key access as UID 1000"
[[ "$access" == "denied" ]] || fail "UID 1000 sees the client key as $access, expected denied"
pass "client key is readable by postgres only"

# --- PostgreSQL mode: full + differential backup, restore ----------------------
as_postgres "$db" mkdir -p -m 0750 /var/lib/postgresql/smoke-repo
as_postgres "$db" pgbackrest --stanza=smoke stanza-create
enable_archiving "$db"
as_postgres "$db" pgbackrest --stanza=smoke check
pass "stanza created and check passed with archiving on"

psql_in "$db" 'CREATE TABLE smoke (id int PRIMARY KEY)'
psql_in "$db" 'INSERT INTO smoke SELECT generate_series(1, 1000)'
as_postgres "$db" pgbackrest --stanza=smoke --type=full backup
psql_in "$db" 'INSERT INTO smoke SELECT generate_series(1001, 2000)'
as_postgres "$db" pgbackrest --stanza=smoke --type=diff backup
# Written after the differential backup, so a restore of it must not contain these.
psql_in "$db" 'INSERT INTO smoke SELECT generate_series(2001, 2500)'

labels="$(as_postgres "$db" pgbackrest --stanza=smoke --output=json info | python3 -c '
import json, sys
backups = json.load(sys.stdin)[0]["backup"]
print(" ".join(b["type"] + "=" + b["label"] for b in backups))
')"
full_label="$(tr ' ' '\n' <<<"$labels" | sed -n 's/^full=//p' | tail -1)"
diff_label="$(tr ' ' '\n' <<<"$labels" | sed -n 's/^diff=//p' | tail -1)"
[[ -n "$full_label" && -n "$diff_label" ]] || fail "expected a full and a diff backup, got: $labels"
[[ "$diff_label" == "$full_label"_* ]] || fail "diff $diff_label does not depend on full $full_label"
pass "full $full_label and diff $diff_label"

zst_file="$(docker exec "$db" find /var/lib/postgresql/smoke-repo -name '*.zst' -print -quit)" ||
	fail "cannot search the repository"
[[ -n "$zst_file" ]] || fail "no zstd-compressed file in the repository"
# backup.history keeps a gzip copy of each manifest whatever compress-type is,
# so only data files and archived WAL say which compression was used.
gz_file="$(docker exec "$db" find /var/lib/postgresql/smoke-repo -path '*/backup.history' -prune -o -name '*.gz' -print -quit)" ||
	fail "cannot search the repository"
[[ -z "$gz_file" ]] || fail "gzip-compressed file $gz_file in the repository; compress-type=zst was not used"
pass "repository is zstd-compressed"

as_postgres "$db" mkdir -m 0700 /var/lib/postgresql/restore
as_postgres "$db" pgbackrest --stanza=smoke --set="$diff_label" --type=immediate \
	--target-action=promote --pg1-path=/var/lib/postgresql/restore restore
# The restored configuration still enables archiving into the live stanza.
# Forcing it off is what keeps a verification restore from writing a new
# timeline into the repository it is verifying.
as_postgres "$db" pg_ctl -D /var/lib/postgresql/restore -l /var/lib/postgresql/restore.log \
	-o "-p 5433 -c archive_mode=off" -w start >/dev/null
restored_promoted() {
	[[ "$(docker exec -u postgres "$db" psql -X -qtA -p 5433 -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" == "f" ]]
}
wait_until 60 "the restored cluster to finish recovery" restored_promoted
restored="$(docker exec -u postgres "$db" psql -X -qtA -p 5433 -c 'SELECT count(*), max(id) FROM smoke')" ||
	fail "cannot query the restored cluster"
restored_archive="$(docker exec -u postgres "$db" psql -X -qtA -p 5433 -c 'SHOW archive_mode')" ||
	fail "cannot query the restored cluster"
as_postgres "$db" pg_ctl -D /var/lib/postgresql/restore -m fast -w stop >/dev/null
[[ "$restored" == "2000|2000" ]] || fail "restored diff holds $restored, expected 2000|2000"
[[ "$restored_archive" == "off" ]] || fail "restored cluster has archive_mode $restored_archive"
pass "diff restore holds the rows written before it and none after"

remove_container "$db" || fail "cannot remove $db"

# --- async archiving: an init process keeps PostgreSQL up ---------------------
# The repository is unreachable (TEST-NET-1 address), so pgBackRest's
# background archiver keeps trying while the case kills it. With the postmaster
# as PID 1 the orphaned archiver is its child and its abnormal exit restarts the
# database; the control run without tini proves this test would notice.
cat >"$work/async.conf" <<'EOF'
[global]
repo1-type=s3
repo1-s3-endpoint=192.0.2.1
repo1-s3-bucket=smoke
repo1-s3-region=us-east-1
repo1-s3-key=smoke
repo1-s3-key-secret=smoke
repo1-path=/smoke
archive-async=y
spool-path=/var/spool/pgbackrest
log-level-console=off
log-level-file=off

[smoke]
pg1-path=/var/lib/postgresql/data
EOF
chmod 0644 "$work/async.conf"

async_orphan_pid() {
	docker exec "$1" sh -c '
		for d in /proc/[0-9]*; do
			cmd=$(tr "\0" " " <"$d/cmdline" 2>/dev/null) || continue
			case "$cmd" in *archive-push:async*) ;; *) continue ;; esac
			ppid=$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)
			[ "$ppid" = 1 ] && { echo "${d#/proc/}"; exit 0; }
		done
		exit 1'
}

# run_async_case name expect_restart [docker run args...] [-- command...]
run_async_case() {
	local name=$1 expect_restart=$2
	shift 2
	start_db "$name" "$work/async.conf" "$@"
	enable_archiving "$name"
	local before
	before="$(postmaster_pid "$name")"
	psql_in "$name" 'CREATE TABLE churn AS SELECT generate_series(1, 10000) AS id'
	psql_in "$name" 'SELECT pg_switch_wal()' >/dev/null
	wait_until 60 "an orphaned async archiver in $name" async_orphan_pid "$name"
	local orphan logs restarted=no
	orphan="$(async_orphan_pid "$name")" || fail "$name: lost the async archiver"
	docker exec "$name" sh -c "kill -9 $orphan" || fail "$name: cannot kill PID $orphan"
	sleep 8
	logs="$(docker logs "$name" 2>&1)" || fail "$name: cannot read the container log"
	# The startup banner is always there; without it the log is not the one
	# this case needs to judge.
	[[ "$logs" == *"database system is ready to accept connections"* ]] ||
		fail "$name: container log does not show PostgreSQL starting"
	if [[ "$logs" == *reinitializing* ]]; then
		restarted=yes
	fi
	if [[ "$expect_restart" == "no" ]]; then
		[[ "$restarted" == "no" ]] || fail "$name: PostgreSQL restarted after the async archiver was killed"
		[[ "$(postmaster_pid "$name")" == "$before" ]] || fail "$name: postmaster PID changed"
		pass "with tini, killing the async archiver (PID $orphan) left PostgreSQL running"
	else
		[[ "$restarted" == "yes" ]] ||
			fail "$name: control without tini did not restart; this test cannot detect the regression"
		pass "control: without tini, the same kill restarts PostgreSQL"
	fi
	remove_container "$name" || fail "cannot remove $name"
}

log "async archiving with tini"
run_async_case "$async_init" no
log "async archiving without tini (control)"
run_async_case "$async_control" yes --entrypoint docker-entrypoint.sh -- postgres

# --- repo-host mode -------------------------------------------------------------
cat >"$work/server.conf" <<'EOF'
[global]
tls-server-address=*
tls-server-ca-file=/etc/cara/pgbackrest/tls/ca.crt
tls-server-cert-file=/etc/cara/pgbackrest/tls/server.crt
tls-server-key-file=/etc/cara/pgbackrest/tls/server.key
tls-server-auth=client=smoke
repo1-path=/tmp/pgbackrest-repo
log-level-console=warn
log-level-file=off
EOF
chmod 0644 "$work/server.conf"

log "starting $repohost"
docker run -d --name "$repohost" --user 999:999 \
	-v "$work/server.conf:/etc/pgbackrest/pgbackrest.conf:ro" \
	-v "$tls_volume:/etc/cara/pgbackrest/tls:ro" \
	"$image" pgbackrest server >/dev/null
track_volumes "$repohost"
server_answers() { docker exec "$repohost" pgbackrest server-ping 127.0.0.1 >/dev/null 2>&1; }
wait_until 30 "the repo host to answer" server_answers

[[ "$(docker exec "$repohost" cat /proc/1/comm)" == "tini" ]] || fail "repo host: PID 1 is not tini"
# Prints how many processes it saw and which run as root, so an empty answer
# can never be mistaken for "none".
proc_report="$(docker exec "$repohost" sh -c '
	seen=0 root=""
	for d in /proc/[0-9]*; do
		uid=$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" "$d/status" 2>/dev/null) || continue
		[ -n "$uid" ] || continue
		seen=$((seen + 1))
		[ "$uid" = 0 ] && root="$root ${d#/proc/}"
	done
	echo "seen=$seen root=$root"')" || fail "repo host: cannot list processes"
[[ "$proc_report" =~ ^seen=([0-9]+)\ root=(.*)$ ]] || fail "repo host: unexpected process report '$proc_report'"
(( BASH_REMATCH[1] >= 2 )) || fail "repo host: saw only ${BASH_REMATCH[1]} processes"
[[ -z "${BASH_REMATCH[2]// /}" ]] || fail "repo host: processes running as root:${BASH_REMATCH[2]}"
pgdata_state="$(docker exec "$repohost" sh -c '
	if [ -e /var/lib/postgresql/data/PG_VERSION ]; then echo initialized; else echo empty; fi')" ||
	fail "repo host: cannot inspect PGDATA"
[[ "$pgdata_state" == "empty" ]] || fail "repo host: PGDATA is $pgdata_state"
pass "repo host runs pgbackrest server as 999 under tini without initializing PGDATA"

log "all checks passed"
