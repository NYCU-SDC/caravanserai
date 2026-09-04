#!/usr/bin/env bash
set -Eeuo pipefail

# Isolated CARA-69 rehearsal.
#
# The workload Docker daemon runs inside a disposable Docker-in-Docker
# container. This script never prunes the host daemon and cleanup targets only
# the two uniquely named outer containers created below.

log() { printf '[orphan-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cara-orphan-e2e.XXXXXX")"
suffix="${PPID}-$$"
postgres_name="cara69-postgres-${suffix}"
dind_name="cara69-dind-${suffix}"
server_pid=""
agent_pid=""
node_b_heartbeat_pid=""

cleanup() {
	status=$?
	trap - EXIT INT TERM

	for pid in "$node_b_heartbeat_pid" "$agent_pid" "$server_pid"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done

	docker logs "$dind_name" >"$test_root/dind.log" 2>&1 || true

	if (( status != 0 )); then
		for log_file in "$test_root/agent.log" "$test_root/server.log" "$test_root/dind.log"; do
			if [[ -f "$log_file" ]]; then
				printf '\n--- %s (last 120 lines) ---\n' "$log_file"
				tail -n 120 "$log_file" || true
			fi
		done
	fi

	docker rm -f "$dind_name" "$postgres_name" >/dev/null 2>&1 || true

	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
			cara-orphan-e2e.*) rm -rf -- "$test_root" ;;
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
			fail "timed out waiting for $description"
		fi
		sleep 1
	done
}

choose_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

dind() {
	env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_host" "$@"
}

dind_ready() {
	dind info >/dev/null 2>&1
}

server_ready() {
	curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1
}

container_running() {
	[[ "$(dind inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" == "true" ]]
}

container_stopped() {
	[[ "$(dind inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" == "false" ]]
}

container_absent() {
	! dind inspect "$1" >/dev/null 2>&1
}

project_is() {
	project_name=$1
	expected_phase=$2
	expected_node=$3
	body="$(curl -fsS "$server_url/api/v1/projects/$project_name" 2>/dev/null || true)"
	[[ "$body" == *"\"phase\":\"$expected_phase\""* && "$body" == *"\"nodeRef\":\"$expected_node\""* ]]
}

project_node_ready() {
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"Ready"'* ]]
}

assert_same_running_container() {
	name=$1
	expected_id=$2
	actual_id="$(dind inspect -f '{{.Id}}' "$name" 2>/dev/null || true)"
	[[ "$actual_id" == "$expected_id" ]] || fail "foreign container $name was replaced or removed"
	container_running "$name" || fail "foreign container $name was stopped"
}

postgres_image="${POSTGRES_IMAGE:-postgres:16}"
dind_image="${DIND_IMAGE:-docker:27-dind}"
data_root="$test_root/agent-data"
mkdir -p "$data_root" "$test_root/bin" "$test_root/server-run" "$test_root/agent-run"

log "starting isolated PostgreSQL ($postgres_name)"
docker run -d --name "$postgres_name" \
	-e POSTGRES_USER=postgres \
	-e POSTGRES_PASSWORD=password \
	-e POSTGRES_DB=caravanserai \
	-p 127.0.0.1::5432 \
	"$postgres_image" >/dev/null
wait_until 60 "PostgreSQL readiness" docker exec "$postgres_name" pg_isready -U postgres
postgres_mapping="$(docker port "$postgres_name" 5432/tcp)"
postgres_port="${postgres_mapping##*:}"

log "starting isolated Docker daemon ($dind_name)"
docker run -d --privileged --name "$dind_name" \
	-e DOCKER_TLS_CERTDIR= \
	-p 127.0.0.1::2375 \
	-v "$data_root:$data_root" \
	"$dind_image" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null
dind_mapping="$(docker port "$dind_name" 2375/tcp)"
dind_port="${dind_mapping##*:}"
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
	env \
		HOST=127.0.0.1 \
		PORT="$server_port" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

cat >"$test_root/agent-run/config.yaml" <<EOF
data_root: "$data_root"
EOF

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
		"$test_root/bin/cara-agent"
) >"$test_root/agent.log" 2>&1 &
agent_pid=$!
wait_until 60 "node-a readiness" project_node_ready

log "creating foreign sentinels inside the isolated daemon"
unmanaged_name="foreign-unmanaged-${suffix}"
partial_name="foreign-partial-label-${suffix}"
partial_volume="foreign-partial-volume-${suffix}"
foreign_network="foreign-network-${suffix}"
dind run -d --name "$unmanaged_name" nginx:alpine >/dev/null
dind run -d --name "$partial_name" --label cara.project=orphan-target nginx:alpine >/dev/null
dind volume create --label cara.project=orphan-target "$partial_volume" >/dev/null
dind network create "$foreign_network" >/dev/null
unmanaged_id="$(dind inspect -f '{{.Id}}' "$unmanaged_name")"
partial_id="$(dind inspect -f '{{.Id}}' "$partial_name")"

log "creating target and Failed-guard projects while node-a is the only Ready node"
curl -fsS -X POST "$server_url/api/v1/projects" \
	-H 'Content-Type: application/json' \
	-d '{
		"apiVersion":"caravanserai/v1",
		"kind":"Project",
		"metadata":{"name":"orphan-target","namespace":"default"},
		"spec":{
			"services":[{
				"name":"web","image":"nginx:alpine",
				"volumeMounts":[
					{"name":"managed","mountPath":"/usr/share/nginx/html"},
					{"name":"scratch","mountPath":"/tmp/scratch"}
				]
			}],
			"volumes":[
				{"name":"managed","type":"Managed"},
				{"name":"scratch","type":"Ephemeral"}
			]
		}
	}' >/dev/null

curl -fsS -X POST "$server_url/api/v1/projects" \
	-H 'Content-Type: application/json' \
	-d '{
		"apiVersion":"caravanserai/v1",
		"kind":"Project",
		"metadata":{"name":"failed-guard","namespace":"default"},
		"spec":{"services":[{"name":"web","image":"nginx:alpine"}]}
	}' >/dev/null

wait_until 90 "orphan-target Running on node-a" project_is orphan-target Running node-a
wait_until 90 "failed-guard Running on node-a" project_is failed-guard Running node-a
target_container="orphan-target-web"
guard_container="failed-guard-web"
wait_until 30 "target container running" container_running "$target_container"
wait_until 30 "guard container running" container_running "$guard_container"

managed_marker="$data_root/volumes/default/orphan-target/managed/data/marker.txt"
printf 'managed-data-must-survive\n' >"$managed_marker"

log "forcing failed-guard to Failed while it remains assigned to node-a"
guard_update="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET phase='Failed', status=jsonb_set(status, '{phase}', to_jsonb('Failed'::text), true), updated_at=now() WHERE kind='Project' AND name='failed-guard';")"
[[ "$guard_update" == "UPDATE 1" ]] || fail "failed to update failed-guard fixture: $guard_update"

log "registering and heartbeating destination node-b"
curl -fsS -X POST "$server_url/api/v1/nodes" \
	-H 'Content-Type: application/json' \
	-d '{"apiVersion":"caravanserai/v1","kind":"Node","metadata":{"name":"node-b"},"spec":{"hostname":"node-b"}}' >/dev/null
heartbeat_node_b() {
	curl -fsS -X POST "$server_url/api/v1/nodes/node-b/heartbeat" \
		-H 'Content-Type: application/json' \
		-d '{"state":"Ready","network":{"overlayIP":"127.0.0.1","agentPort":0}}' >/dev/null
}
heartbeat_node_b
(
	while true; do
		heartbeat_node_b || true
		sleep 5
	done
) &
node_b_heartbeat_pid=$!

log "moving orphan-target ownership to node-b"
target_update="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET status=jsonb_set(status, '{nodeRef}', to_jsonb('node-b'::text), true), updated_at=now() WHERE kind='Project' AND name='orphan-target';")"
[[ "$target_update" == "UPDATE 1" ]] || fail "failed to transfer target fixture: $target_update"

wait_until 40 "target container to stop after ownership loss" container_stopped "$target_container"
dind inspect "$target_container" >/dev/null || fail "target was deleted during reversible stop stage"
assert_same_running_container "$unmanaged_name" "$unmanaged_id"
assert_same_running_container "$partial_name" "$partial_id"
container_running "$guard_container" || fail "Failed project assigned to node-a was stopped"

log "verifying the target still exists well inside the 3-minute deletion grace"
sleep 120
dind inspect "$target_container" >/dev/null || fail "target was deleted before the grace period elapsed"
assert_same_running_container "$unmanaged_name" "$unmanaged_id"
assert_same_running_container "$partial_name" "$partial_id"
container_running "$guard_container" || fail "Failed project assigned to node-a was stopped during grace"

log "waiting for permanent cleanup after continuously confirmed ownership loss"
wait_until 100 "target resource deletion after grace" container_absent "$target_container"

container_absent "$target_container" || fail "target container still exists"
if dind volume inspect cara-orphan-target-scratch >/dev/null 2>&1; then
	fail "target Ephemeral volume still exists"
fi
if dind network inspect cara-orphan-target >/dev/null 2>&1; then
	fail "target Cara network still exists"
fi
[[ -f "$managed_marker" ]] || fail "Managed host data was removed"

assert_same_running_container "$unmanaged_name" "$unmanaged_id"
assert_same_running_container "$partial_name" "$partial_id"
dind volume inspect "$partial_volume" >/dev/null || fail "partial-label foreign volume was removed"
dind network inspect "$foreign_network" >/dev/null || fail "foreign network was removed"
container_running "$guard_container" || fail "Failed project assigned to node-a was touched"

log "PASS: target stopped then deleted; Failed/foreign containers, foreign volume/network, and Managed data survived"
