#!/usr/bin/env bash
set -Eeuo pipefail

# Isolated PostgresDifferential rehearsal: a PostgreSQL Project archives WAL
# through the Node's pgBackRest repo host, is backed up full and then
# differential while it runs, and the differential restores into an isolated
# container with the rows written before it.
#
# Real cara-server and cara-agent, a real Docker daemon (Docker-in-Docker) and a
# real object store (MinIO over TLS with a private CA, since pgBackRest reaches
# S3 only over HTTPS). MinIO is published on the host's Docker bridge gateway,
# the one address both the Agent on the host and the repo host inside the
# isolated daemon can reach. The PostgreSQL image is pulled by the Agent like
# any other image, so it must be published somewhere the isolated daemon can
# pull it anonymously. PGDIFF_IMAGE names it (default: the published
# ghcr.io/nycu-sdc/cara-postgres:16); the server is configured to accept the
# same reference. E2E_INSECURE_REGISTRY=<host:port> lets the isolated daemon
# pull it from a plain-HTTP registry, such as a local copy.
#
# Scenarios, in order:
#   A. placement: the Project reaches Running with its stanza recorded and WAL
#      archived, and reports BackupActive=False/NoAcceptedBackup
#   B. backups: a full, then a differential after more rows, each accepted with
#      a manifest under the Project UID; BackupActive turns True; PostgreSQL is
#      never stopped, and nothing is written under the v1 layout
#   C. verification restore: the differential restores into a throwaway
#      container, with archiving off, holding every row; the stanza's backups
#      and timeline are unchanged by it
#   D. a new assignment of the Project, with no local data, is refused with
#      StrategyRestoreUnavailable; no container, no PGDATA
#   E. the same with no object store configured on the agent
#   F. with no object store, a new PostgresDifferential Project is held with
#      BackupRepositoryUnavailable and nothing is started
#
# D and E stand in for a reassignment by writing what the Scheduler writes
# (Scheduled, the same Node, the next generation) directly into the isolated
# control-plane database. Moving a Project between real Nodes is exercised by
# the managed-failover rehearsal; what this one checks is the agent's decision
# when such an assignment arrives without local data.

log() { printf '[pgdiff-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

pg_image="${PGDIFF_IMAGE:-ghcr.io/nycu-sdc/cara-postgres:16}"
# Asked anonymously, as the Agent will: a reference that needs a login would
# pass here only to fail inside the isolated daemon.
anonymous_config="$(mktemp -d)"
insecure_registry="${E2E_INSECURE_REGISTRY:-}"
inspect_flags=()
[[ -n "$insecure_registry" && "$pg_image" == "$insecure_registry"/* ]] && inspect_flags=(--insecure)
pullable=yes
DOCKER_CONFIG="$anonymous_config" docker manifest inspect "${inspect_flags[@]}" "$pg_image" >/dev/null 2>&1 || pullable=no
rm -rf -- "$anonymous_config"
[[ "$pullable" == yes ]] ||
	fail "$pg_image cannot be pulled anonymously; publish it or set PGDIFF_IMAGE to a copy that can be"

shared_root="${E2E_SHARED_ROOT:-/var/tmp}"
[[ -d "$shared_root" && -w "$shared_root" ]] || fail "E2E_SHARED_ROOT is not a writable directory: $shared_root"
test_root="$(mktemp -d "$shared_root/cara-pgdiff-e2e.XXXXXX")"
suffix="${PPID}-$$"
postgres_name="cara126-postgres-${suffix}"
dind_name="cara126-dind-${suffix}"
minio_name="cara126-minio-${suffix}"
bucket="cara-pgdiff-e2e"
dind_host=""
server_pid=""
agent_pid=""

cleanup() {
	status=$?
	trap - EXIT INT TERM

	for pid in "$agent_pid" "$server_pid"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done

	if (( status != 0 )); then
		for log_file in "$test_root"/agent-*.log "$test_root/server.log"; do
			if [[ -f "$log_file" ]]; then
				printf '\n--- %s (last 80 lines) ---\n' "$log_file"
				tail -n 80 "$log_file" || true
			fi
		done
	fi

	# PGDATA and the pgBackRest identities belong to UID 999 inside the
	# isolated daemon; remove them from there before the daemon goes.
	if [[ -n "$dind_host" ]]; then
		dind run --rm --user 0 --entrypoint sh -v "$data_root:$data_root" "$pg_image" \
			-c 'rm -rf "$1"/*' sh "$data_root" >/dev/null 2>&1 || true
	fi

	# -v removes the anonymous volumes the images declare. Every name is unique
	# to this run.
	docker rm -f -v "$dind_name" "$postgres_name" "$minio_name" >/dev/null 2>&1 || true

	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
			cara-pgdiff-e2e.*) rm -rf -- "$test_root" ;;
			*) log "refusing to remove unexpected temp path: $test_root" ;;
		esac
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

wait_until() {
	timeout_seconds=$1
	description=$2
	shift 2
	deadline=$((SECONDS + timeout_seconds))
	until "$@"; do
		if (( SECONDS >= deadline )); then
			fail "timed out after ${timeout_seconds}s waiting for $description"
		fi
		sleep 1
	done
}

choose_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

dind() { env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_host" "$@"; }
dind_ready() { dind info >/dev/null 2>&1; }
mc() { docker exec "$minio_name" mc --insecure "$@"; }
psql_cp() { docker exec "$postgres_name" psql -U postgres -d caravanserai -v ON_ERROR_STOP=1 -qtAc "$1"; }
server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }
node_ready() {
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"Ready"'* ]]
}

project_json() { curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null || true; }
project_field() {
	project_json "$1" | python3 -c '
import json, sys
try:
    p = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
def cond(t):
    for c in p.get("status", {}).get("conditions", []) or []:
        if c.get("type") == t:
            return c
    return {}
try:
    print(eval(sys.argv[1]))
except Exception:
    print("")
' "$2"
}
phase_is() { [[ "$(project_field "$1" 'p["status"]["phase"]')" == "$2" ]]; }

# running_or_fail NAME succeeds once NAME is Running, and ends the test at once
# if it went Failed instead, with the reason the agent gave (typically that
# the image could not be pulled).
running_or_fail() {
	if phase_is "$1" Failed; then
		fail "$1 went Failed: $(project_field "$1" 'cond("Phase").get("message", "")')"
	fi
	phase_is "$1" Running
}
blocked_is() { [[ "$(project_field "$1" 'cond("RecoveryBlocked").get("reason", "")')" == "$2" ]]; }

create_project() {
	curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d "$1" >/dev/null ||
		fail "creating a Project was rejected: $1"
}

# pg_project NAME prints a PostgresDifferential Project manifest.
pg_project() {
	cat <<EOF
{"apiVersion":"caravanserai/v1","kind":"Project",
 "metadata":{"name":"$1","namespace":"default"},
 "spec":{
   "services":[{"name":"db","image":"$pg_image",
     "env":[{"name":"POSTGRES_PASSWORD","value":"e2e"}],
     "volumeMounts":[{"name":"pgdata","mountPath":"/var/lib/postgresql/data"}]}],
   "volumes":[{"name":"pgdata","type":"Managed"}],
   "backup":{"interval":"15s","items":[{"volume":"pgdata","strategy":"PostgresDifferential"}]}}}
EOF
}

# pg_accepts NAME succeeds when the Project's PostgreSQL answers a query.
pg_accepts() {
	[[ "$(dind exec "$1-db" psql -U postgres -qtAc 'SELECT 1' 2>/dev/null || true)" == "1" ]]
}

# bucket_has PATTERN reports whether a key in the bucket matches the extended
# regular expression PATTERN. The listing runs in this shell, not a command
# substitution, so a listing that fails ends the test instead of reading as
# "nothing there".
bucket_has() {
	local listing_file="$test_root/bucket-listing"
	mc ls --recursive "local/$bucket" >"$listing_file" || fail "cannot list bucket $bucket"
	grep -Eq "$1" "$listing_file"
}

# has_latest NAME reports whether the bucket holds NAME's v1 latest.json.
has_latest() { bucket_has "cara/v1/projects/default/$1/latest\.json\$"; }

uid_of() { project_field "$1" 'p["metadata"]["uid"]'; }
completions() { project_field "$1" 'len(p["status"].get("backup", {}).get("completions", []))'; }
# completion_field NAME INDEX EXPR evaluates EXPR on completion INDEX (c).
completion_field() {
	project_field "$1" "(lambda c: $3)(p['status']['backup']['completions'][$2])"
}
completions_at_least() { [[ "$(completions "$1")" -ge "$2" ]] 2>/dev/null; }

# start_agent with-s3|without-s3 starts node-a's agent.
start_agent() {
	local mode=$1 s3=()
	if [[ "$mode" == with-s3 ]]; then
		s3=(S3_ENDPOINT="$minio_endpoint" S3_CA_FILE="$certs/ca.crt" S3_BUCKET="$bucket"
			S3_ACCESS_KEY=caraadmin S3_SECRET_KEY=caraadmin123)
	fi
	(
		cd "$test_root/agent-run"
		env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
			SERVER_URL="$server_url" \
			NODE_NAME=node-a \
			HEARTBEAT_INTERVAL=1s \
			DOCKER_HOST="$dind_host" \
			AGENT_LISTEN_PORT=0 \
			PROXY_LISTEN_ADDR=127.0.0.1:0 \
			AGENT_ADVERTISE_IP=127.0.0.1 \
			"${s3[@]}" \
			"$test_root/bin/cara-agent"
	) >"$test_root/agent-$mode.log" 2>&1 &
	agent_pid=$!
	wait_until 60 "node-a readiness" node_ready
}

stop_agent() {
	kill "$agent_pid" >/dev/null 2>&1 || true
	wait "$agent_pid" >/dev/null 2>&1 || true
	agent_pid=""
}

# reassign_without_data NAME stands in for the Scheduler giving the Project a
# new assignment on node-a, after its containers and local data are gone. The
# agent must be stopped so it cannot act on the half-made state.
reassign_without_data() {
	local name=$1
	dind rm -f -v "$name-db" >/dev/null 2>&1 || true
	[[ -z "$(dind ps -aq --filter "name=^$name-db$")" ]] || fail "$name-db could not be removed"
	# PGDATA belongs to UID 999 inside the isolated daemon.
	dind run --rm --user 0 --entrypoint rm -v "$data_root:$data_root" "$pg_image" \
		-rf -- "$data_root/volumes/default/$name" || fail "cannot remove the local data of $name"
	[[ ! -e "$data_root/volumes/default/$name" ]] || fail "local data of $name could not be removed"
	local updated
	updated="$(psql_cp "
		UPDATE resources
		SET phase = 'Scheduled',
		    resource_version = resource_version + 1,
		    status = jsonb_set(jsonb_set(jsonb_set(jsonb_set(
		        status,
		        '{phase}', '\"Scheduled\"'),
		        '{nodeRef}', '\"node-a\"'),
		        '{assignmentHistory}', '\"Known\"'),
		        '{assignmentGeneration}', to_jsonb(COALESCE((status->>'assignmentGeneration')::int, 0) + 1))
		    || jsonb_build_object('conditions', COALESCE((
		        SELECT jsonb_agg(c) FROM jsonb_array_elements(COALESCE(status->'conditions', '[]'::jsonb)) c
		        WHERE c->>'type' <> 'RecoveryBlocked'), '[]'::jsonb))
		WHERE kind = 'Project' AND namespace = 'default' AND name = '$name'
		RETURNING status->>'assignmentGeneration'")" || fail "cannot reassign $name"
	[[ -n "$updated" ]] || fail "reassigning $name changed no row"
	log "$name reassigned to node-a at generation $updated with no local data"
}

# assert_refused NAME checks the agent refused to place NAME and started nothing.
assert_refused() {
	local name=$1
	wait_until 60 "$name blocked with StrategyRestoreUnavailable" blocked_is "$name" StrategyRestoreUnavailable
	# A few more polls: the refusal must hold, not be followed by a start.
	sleep 5
	blocked_is "$name" StrategyRestoreUnavailable || fail "$name: the block did not hold"
	phase_is "$name" Scheduled || fail "$name: phase moved to $(project_field "$name" 'p["status"]["phase"]')"
	local containers
	containers="$(dind ps -aq --filter "name=^$name-db$")" || fail "cannot list containers in the isolated daemon"
	[[ -z "$containers" ]] || fail "$name: a container was created for a refused placement"
	[[ ! -e "$data_root/volumes/default/$name/pgdata/data/PG_VERSION" ]] ||
		fail "$name: an empty database was initialised"
}

data_root="$test_root/agent-data"
mkdir -p "$data_root" "$test_root/bin" "$test_root/server-run" "$test_root/agent-run"

log "starting isolated PostgreSQL ($postgres_name)"
docker run -d --name "$postgres_name" \
	-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=password -e POSTGRES_DB=caravanserai \
	-p 127.0.0.1::5432 "${POSTGRES_IMAGE:-postgres:16}" >/dev/null
wait_until 60 "PostgreSQL readiness" docker exec "$postgres_name" pg_isready -U postgres
postgres_port="$(docker port "$postgres_name" 5432/tcp | head -1)"; postgres_port="${postgres_port##*:}"

# The address both the Agent on this host and the repo host inside the isolated
# daemon reach MinIO at: this host's end of Docker's default bridge.
gateway="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')" ||
	fail "cannot read the Docker bridge gateway"
[[ -n "$gateway" ]] || fail "the Docker bridge has no gateway address"

log "issuing a private CA and MinIO's certificate for $gateway"
certs="$test_root/certs"
mkdir -p "$certs"
docker run --rm --user 0 --entrypoint sh -v "$certs:/c" "$pg_image" -c '
	set -e; cd /c
	openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=cara-e2e-s3-ca -keyout ca.key -out ca.crt 2>/dev/null
	openssl req -newkey rsa:2048 -nodes -subj /CN=minio -keyout private.key -out s.csr 2>/dev/null
	printf "subjectAltName=IP:%s,IP:127.0.0.1\n" "$1" > ext
	openssl x509 -req -in s.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile ext -out public.crt 2>/dev/null
	rm -f ca.key s.csr ext ca.srl; chmod 644 *' sh "$gateway" || fail "cannot issue the MinIO certificate"

log "starting isolated MinIO ($minio_name)"
# The same pinned Chainguard image as the managed-failover rehearsal: Quay no
# longer serves MinIO anonymously. Its mc reads the local alias from
# MC_HOST_local, so no alias needs to be set.
docker run -d --name "$minio_name" \
	-e MINIO_ROOT_USER=caraadmin -e MINIO_ROOT_PASSWORD=caraadmin123 \
	-e MC_HOST_local=https://caraadmin:caraadmin123@127.0.0.1:9000 \
	-v "$certs:/certs:ro" -p "$gateway::9000" \
	"${MINIO_IMAGE:-cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}" \
	server /data --certs-dir /certs >/dev/null
minio_port="$(docker port "$minio_name" 9000/tcp | head -1)"; minio_port="${minio_port##*:}"
minio_endpoint="https://$gateway:$minio_port"
wait_until 60 "MinIO readiness" bash -c "curl -fsS --cacert '$certs/ca.crt' '$minio_endpoint/minio/health/live' >/dev/null 2>&1"
mc mb --ignore-existing "local/$bucket" >/dev/null || fail "cannot create bucket $bucket"

log "starting isolated Docker daemon ($dind_name)"
dind_flags=()
[[ -n "$insecure_registry" ]] && dind_flags=(--insecure-registry="$insecure_registry")
docker run -d --privileged --name "$dind_name" -e DOCKER_TLS_CERTDIR= \
	-p 127.0.0.1::2375 -v "$data_root:$data_root" \
	"${DIND_IMAGE:-docker:27-dind}" --host=tcp://0.0.0.0:2375 --tls=false "${dind_flags[@]}" >/dev/null
dind_port="$(docker port "$dind_name" 2375/tcp | head -1)"; dind_port="${dind_port##*:}"
dind_host="tcp://127.0.0.1:$dind_port"
wait_until 90 "Docker-in-Docker readiness" dind_ready
dind pull nginx:alpine >/dev/null

log "building cara-server and cara-agent"
go build -o "$test_root/bin/cara-server" ./cmd/cara-server
go build -o "$test_root/bin/cara-agent" ./cmd/cara-agent

server_port="$(choose_port)"
server_url="http://127.0.0.1:$server_port"
(
	cd "$test_root/server-run"
	env HOST=127.0.0.1 PORT="$server_port"		POSTGRES_DIFFERENTIAL_IMAGE="$pg_image" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

cat >"$test_root/agent-run/config.yaml" <<EOF
data_root: "$data_root"
EOF
start_agent with-s3


pg=pgdiff
db_sql() { dind exec "$pg-db" psql -X -U postgres -d postgres -v ON_ERROR_STOP=1 -qtAc "$1"; }
# stanza_info prints the stanza's `pgbackrest info` as JSON, from the database.
stanza_info() { dind exec -u postgres "$pg-db" pgbackrest --stanza="$stanza" --output=json info; }

# ── A. Placement archives before Running ────────────────────────────────────────
log "A: creating $pg"
create_project "$(pg_project "$pg")"
wait_until 180 "$pg Running" running_or_fail "$pg"
wait_until 60 "$pg's PostgreSQL to accept connections" pg_accepts "$pg"
uid="$(uid_of "$pg")"
[[ -n "$uid" ]] || fail "$pg has no UID"
stanza="$uid-pgdata"
[[ "$(project_field "$pg" 'cond("BackupActive").get("reason", "")')" == "NoAcceptedBackup" ]] ||
	fail "$pg: BackupActive reason is not NoAcceptedBackup before any backup"
[[ "$(project_field "$pg" '[s["name"] for s in p["status"]["backup"]["stanzas"]]')" == "['$stanza']" ]] ||
	fail "$pg: the stanza inventory does not list $stanza"
[[ "$(db_sql 'SHOW archive_mode')" == "on" ]] || fail "$pg: archive_mode is not on"
dind exec -u postgres "$pg-db" pgbackrest --stanza="$stanza" check >/dev/null || fail "$pg: the archive check fails"
pg_started="$(dind inspect -f '{{.State.StartedAt}}' "$pg-db")" || fail "cannot inspect $pg-db"
log "A: PASS — Running with $stanza archiving through the repo host"

# ── B. Full, then differential, while PostgreSQL runs ───────────────────────────
log "B: writing rows and waiting for a full backup"
db_sql "CREATE TABLE notes (id int PRIMARY KEY)" >/dev/null
db_sql "INSERT INTO notes SELECT generate_series(1, 1000)" >/dev/null
wait_until 120 "the first accepted backup" completions_at_least "$pg" 1
[[ "$(completion_field "$pg" 0 'c["items"][0]["type"]')" == "full" ]] || fail "$pg: the first backup is not full"
full_label="$(completion_field "$pg" 0 'c["items"][0]["label"]')"
[[ "$(project_field "$pg" 'cond("BackupActive").get("status", "")')" == "True" ]] ||
	fail "$pg: BackupActive is not True after an accepted backup"

db_sql "INSERT INTO notes SELECT generate_series(1001, 2000)" >/dev/null
seen="$(completions "$pg")"
wait_until 120 "a backup started after the second write" completions_at_least "$pg" $((seen + 1))
diff_label="$(completion_field "$pg" "$seen" 'c["items"][0]["label"]')"
[[ "$(completion_field "$pg" "$seen" 'c["items"][0]["type"]')" == "diff" ]] || fail "$pg: the backup after the full is not differential"
[[ "$diff_label" == "${full_label}_"* ]] || fail "$pg: $diff_label does not depend on $full_label"
backup_id="$(completion_field "$pg" "$seen" 'c["backupID"]')"

bucket_has "cara/v2/projects/$uid/generations/$backup_id/manifest\.json\$" || fail "$pg: no manifest for $backup_id"
bucket_has "cara/v2/projects/$uid/latest\.json\$" && fail "$pg: a v2 latest.json was written"
bucket_has "cara/v1/projects/default/$pg/" && fail "$pg: something was written under the v1 layout"
[[ "$(dind inspect -f '{{.State.StartedAt}}' "$pg-db")" == "$pg_started" ]] ||
	fail "$pg-db was restarted: something stopped PostgreSQL for a backup"
log "B: PASS — $full_label, then $diff_label, accepted with a manifest; PostgreSQL kept running"

# ── C. The differential restores, without touching the stanza ───────────────────
log "C: restoring $diff_label into an isolated container"
info_before="$(stanza_info)" || fail "cannot read the stanza before verification"
verify_net="cara-pgdiff-verify-$suffix"
dind network create "$verify_net" >/dev/null || fail "cannot create the verification network"
dind network connect --alias pgbackrest-repo "$verify_net" cara-pgbackrest-repo ||
	fail "cannot attach the repo host to the verification network"
identity="$data_root/pgbackrest/projects/$uid"
restored="$(dind run --rm --network "$verify_net" --user 999:999 --entrypoint sh \
	-v "$identity/conf:/etc/pgbackrest:ro" -v "$identity/tls:/etc/cara/pgbackrest/tls:ro" \
	"$pg_image" -c '
		set -e
		mkdir -m 700 /tmp/restore
		pgbackrest --stanza="$1" --set="$2" --pg1-path=/tmp/restore \
			--type=immediate --target-action=promote --archive-mode=off restore >/dev/null
		pg_ctl -D /tmp/restore -o "-c listen_addresses= -c archive_mode=off" -w -t 120 -l /tmp/pg.log start >/dev/null
		until [ "$(psql -X -qtA -U postgres -d postgres -c "SELECT pg_is_in_recovery()")" = f ]; do sleep 1; done
		psql -X -qtA -U postgres -d postgres -c "SELECT count(*) FROM notes"' sh "$stanza" "$diff_label")" ||
	fail "the verification restore failed"
dind network disconnect "$verify_net" cara-pgbackrest-repo >/dev/null || fail "cannot detach the repo host"
dind network rm "$verify_net" >/dev/null || fail "cannot remove the verification network"
[[ "$restored" == "2000" ]] || fail "the restored database holds $restored rows, not 2000"

info_after="$(stanza_info)" || fail "cannot read the stanza after verification"
python3 - "$info_before" "$info_after" <<'PY' || fail "the verification changed the stanza"
import json, sys
before, after = (json.loads(a)[0] for a in sys.argv[1:3])
labels = lambda s: [b["label"] for b in s["backup"]]
assert labels(after) == labels(before), (labels(before), labels(after))
timelines = {a["max"][:8] for a in after["archive"]} | {a["min"][:8] for a in after["archive"]}
assert timelines == {"00000001"}, timelines
PY
log "C: PASS — the differential holds all 2000 rows; the stanza is unchanged"

# ── D. Reassignment without local data, object store configured ───────────────
log "D: reassigning $pg with its local data gone (object store configured)"
stop_agent
reassign_without_data "$pg"
start_agent with-s3
assert_refused "$pg"
log "D: PASS — refused with StrategyRestoreUnavailable; no container, no empty database"

# ── E. The same with no object store ────────────────────────────────────────────
log "E: reassigning $pg again, agent restarted without an object store"
stop_agent
reassign_without_data "$pg"
start_agent without-s3
# node-a can still read Ready from the previous agent's heartbeats, so wait
# for this agent to say how it started.
wait_until 30 "the agent to start without an object store" \
	grep -q "Object store not configured" "$test_root/agent-without-s3.log"
assert_refused "$pg"
log "E: PASS — refused without an object store as well"

# ── F. No object store, no placement ────────────────────────────────────────────
fresh=pgdiff-fresh
log "F: creating $fresh on the agent without an object store"
create_project "$(pg_project "$fresh")"
wait_until 60 "$fresh blocked with BackupRepositoryUnavailable" blocked_is "$fresh" BackupRepositoryUnavailable
sleep 5
phase_is "$fresh" Scheduled || fail "$fresh: phase moved to $(project_field "$fresh" 'p["status"]["phase"]')"
[[ -z "$(dind ps -aq --filter "name=^$fresh-db$")" ]] || fail "$fresh: a container was started without a repository"
log "F: PASS — held with BackupRepositoryUnavailable; nothing started"

log "all scenarios passed"
