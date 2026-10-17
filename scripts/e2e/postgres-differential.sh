#!/usr/bin/env bash
set -Eeuo pipefail

# Isolated CARA-126 stage 1 rehearsal: a PostgresDifferential Project is
# accepted and runs, but nothing backs it up yet and nothing may restore it.
#
# Real cara-server and cara-agent, a real Docker daemon (Docker-in-Docker) and a
# real object store (MinIO). The PostgreSQL image is pulled by the Agent like
# any other image, so it must be published somewhere the isolated daemon can
# pull it anonymously. PGDIFF_IMAGE names it (default: the published
# ghcr.io/nycu-sdc/cara-postgres:16); the server is configured to accept the
# same reference.
#
# Scenarios, in order:
#   A. with an object store: the PostgreSQL accepts connections and the Project
#      reports BackupActive=False/StrategyNotActive
#   B. backups run for an Offline control Project (proving the interval elapsed
#      and the store works), while the PostgresDifferential Project gets no
#      latest.json and its PostgreSQL is never stopped
#   C. with an object store: a new assignment of the Project, with no local data,
#      is refused with StrategyRestoreUnavailable; no container, no PGDATA
#   D. the same with no object store configured on the agent
#   E. with no object store, a brand-new PostgresDifferential Project still
#      starts (first placement needs no backup)
#
# C and D stand in for a reassignment by writing what the Scheduler writes
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
pullable=yes
DOCKER_CONFIG="$anonymous_config" docker manifest inspect "$pg_image" >/dev/null 2>&1 || pullable=no
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
mc() { docker exec "$minio_name" mc "$@"; }
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
   "backup":{"interval":"10s","items":[{"volume":"pgdata","strategy":"PostgresDifferential"}]}}}
EOF
}

# pg_accepts NAME succeeds when the Project's PostgreSQL answers a query.
pg_accepts() {
	[[ "$(dind exec "$1-db" psql -U postgres -qtAc 'SELECT 1' 2>/dev/null || true)" == "1" ]]
}

# has_latest NAME reports whether the bucket holds NAME's latest.json. The
# listing runs in this shell, not a command substitution, so a listing that
# fails ends the test instead of reading as "nothing there".
has_latest() {
	local listing_file="$test_root/bucket-listing"
	mc ls --recursive "local/$bucket" >"$listing_file" || fail "cannot list bucket $bucket"
	grep -q "cara/v1/projects/default/$1/latest\.json\$" "$listing_file"
}

# start_agent with-s3|without-s3 starts node-a's agent.
start_agent() {
	local mode=$1 s3=()
	if [[ "$mode" == with-s3 ]]; then
		s3=(S3_ENDPOINT="$minio_endpoint" S3_BUCKET="$bucket" S3_ACCESS_KEY=caraadmin S3_SECRET_KEY=caraadmin123)
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
			UID_ENFORCEMENT=true \
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
	rm -rf -- "$data_root/volumes/default/$name"
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

log "starting isolated MinIO ($minio_name)"
# The same pinned Chainguard image as the managed-failover rehearsal: Quay no
# longer serves MinIO anonymously. Its mc reads the local alias from
# MC_HOST_local, so no alias needs to be set.
docker run -d --name "$minio_name" \
	-e MINIO_ROOT_USER=caraadmin -e MINIO_ROOT_PASSWORD=caraadmin123 \
	-e MC_HOST_local=http://caraadmin:caraadmin123@127.0.0.1:9000 \
	-p 127.0.0.1::9000 "${MINIO_IMAGE:-cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}" server /data >/dev/null
minio_port="$(docker port "$minio_name" 9000/tcp | head -1)"; minio_port="${minio_port##*:}"
minio_endpoint="http://127.0.0.1:$minio_port"
wait_until 60 "MinIO readiness" bash -c "curl -fsS '$minio_endpoint/minio/health/live' >/dev/null 2>&1"
mc mb --ignore-existing "local/$bucket" >/dev/null || fail "cannot create bucket $bucket"

log "starting isolated Docker daemon ($dind_name)"
docker run -d --privileged --name "$dind_name" -e DOCKER_TLS_CERTDIR= \
	-p 127.0.0.1::2375 -v "$data_root:$data_root" \
	"${DIND_IMAGE:-docker:27-dind}" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null
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
	env HOST=127.0.0.1 PORT="$server_port" UID_ENFORCEMENT=true \
		POSTGRES_DIFFERENTIAL_IMAGE="$pg_image" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

cat >"$test_root/agent-run/config.yaml" <<EOF
data_root: "$data_root"
EOF
start_agent with-s3

# ── A. Accepted, running, and visibly unprotected ───────────────────────────────
pg=pgdiff
log "A: creating $pg"
create_project "$(pg_project "$pg")"
wait_until 120 "$pg Running" running_or_fail "$pg"
wait_until 60 "$pg's PostgreSQL to accept connections" pg_accepts "$pg"
[[ "$(project_field "$pg" 'cond("BackupActive").get("status", "")')" == "False" ]] ||
	fail "$pg: BackupActive is not False"
[[ "$(project_field "$pg" 'cond("BackupActive").get("reason", "")')" == "StrategyNotActive" ]] ||
	fail "$pg: BackupActive reason is not StrategyNotActive"
log "A: PASS — PostgreSQL answers queries and the Project reports StrategyNotActive"

# ── B. No offline backup of a PostgresDifferential volume ───────────────────────
control=offline-control
log "B: creating $control, an Offline Project on the same interval"
create_project '{"apiVersion":"caravanserai/v1","kind":"Project",
 "metadata":{"name":"'"$control"'","namespace":"default"},
 "spec":{"services":[{"name":"web","image":"nginx:alpine",
   "volumeMounts":[{"name":"data","mountPath":"/usr/share/nginx/html"}]}],
   "volumes":[{"name":"data","type":"Managed"}],
   "backup":{"interval":"10s"}}}'
pg_started="$(dind inspect -f '{{.State.StartedAt}}' "$pg-db")" || fail "cannot inspect $pg-db"
wait_until 120 "$control to publish a backup" has_latest "$control"
# The control has completed at least one run, so the supervisor has had a full
# interval and more; give the PostgresDifferential Project two more.
sleep 25
has_latest "$control" || fail "$control's backup disappeared"
if has_latest "$pg"; then
	fail "$pg has a latest.json: the offline runner backed up a PostgresDifferential volume"
fi
[[ "$(dind inspect -f '{{.State.StartedAt}}' "$pg-db")" == "$pg_started" ]] ||
	fail "$pg-db was restarted: something stopped PostgreSQL for a backup"
pg_accepts "$pg" || fail "$pg's PostgreSQL stopped answering"
log "B: PASS — $control was backed up, $pg was neither stopped nor uploaded"

# ── C. Reassignment without local data, object store configured ───────────────
log "C: reassigning $pg with its local data gone (object store configured)"
stop_agent
reassign_without_data "$pg"
start_agent with-s3
assert_refused "$pg"
has_latest "$pg" && fail "$pg gained a latest.json"
log "C: PASS — refused with StrategyRestoreUnavailable; no container, no empty database"

# ── D. The same with no object store ────────────────────────────────────────────
log "D: reassigning $pg again, agent restarted without an object store"
stop_agent
reassign_without_data "$pg"
start_agent without-s3
grep -q "Object store not configured" "$test_root/agent-without-s3.log" ||
	fail "the agent did not start without an object store"
assert_refused "$pg"
log "D: PASS — refused without an object store as well"

# ── E. First placement needs no backup ──────────────────────────────────────────
fresh=pgdiff-fresh
log "E: creating $fresh on the agent without an object store"
create_project "$(pg_project "$fresh")"
wait_until 120 "$fresh Running" running_or_fail "$fresh"
wait_until 60 "$fresh's PostgreSQL to accept connections" pg_accepts "$fresh"
log "E: PASS — a new PostgresDifferential Project starts without an object store"

log "all scenarios passed"
