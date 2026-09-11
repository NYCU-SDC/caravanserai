#!/usr/bin/env bash
set -Eeuo pipefail

# Isolated CARA-86 rehearsal: tier-1 local container recovery against a real
# Docker daemon, driven through a real cara-server and cara-agent.
#
# The workload Docker daemon runs inside a disposable Docker-in-Docker
# container. This script never prunes the host daemon and cleanup targets only
# the two uniquely named outer containers created below, with their anonymous
# volumes.
#
# The server and agent run with UID_ENFORCEMENT=true, so recovery is exercised
# under the strict UID and assignment-generation fence it depends on.
#
# Managed volume data lives in a directory the agent (on the host) and the
# DinD daemon must both see at the same path. It defaults to /var/tmp: a
# directory under /tmp bind-mounted into DinD was observed to be invisible
# inside it, so the inner containers got an empty directory instead. Set
# E2E_SHARED_ROOT to use somewhere else; the script checks the path really is
# shared before running anything.
#
# Scenarios, in order:
#   A. stopped container      → started again, same container, data intact
#   B. removed container      → recreated, data intact
#   C. Managed data missing   → RecoveryBlocked, nothing recreated, still Running;
#                               data restored → recovered, condition cleared
#   D. container that keeps exiting → three attempts, then LocalRestartExhausted
#   E. running container left by an earlier generation → RecoveryBlocked/StaleContainer;
#                               the container is neither adopted, started nor removed

log() { printf '[recovery-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

shared_root="${E2E_SHARED_ROOT:-/var/tmp}"
[[ -d "$shared_root" && -w "$shared_root" ]] || fail "E2E_SHARED_ROOT is not a writable directory: $shared_root"
test_root="$(mktemp -d "$shared_root/cara-recovery-e2e.XXXXXX")"
suffix="${PPID}-$$"
postgres_name="cara86-postgres-${suffix}"
dind_name="cara86-dind-${suffix}"
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

	docker logs "$dind_name" >"$test_root/dind.log" 2>&1 || true

	if (( status != 0 )); then
		for log_file in "$test_root/agent.log" "$test_root/server.log"; do
			if [[ -f "$log_file" ]]; then
				printf '\n--- %s (last 120 lines) ---\n' "$log_file"
				tail -n 120 "$log_file" || true
			fi
		done
	fi

	# -v removes the anonymous volumes both images declare (DinD's
	# /var/lib/docker, PostgreSQL's data directory). The names are unique to
	# this run, so nothing else is touched.
	docker rm -f -v "$dind_name" "$postgres_name" >/dev/null 2>&1 || true

	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
			cara-recovery-e2e.*) rm -rf -- "$test_root" ;;
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

dind() {
	env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_host" "$@"
}

dind_ready() { dind info >/dev/null 2>&1; }
server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }

node_ready() {
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"Ready"'* ]]
}

container_running() {
	[[ "$(dind inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" == "true" ]]
}

container_id() {
	dind inspect -f '{{.Id}}' "$1" 2>/dev/null || true
}

# project_json NAME prints the Project as the API returns it.
project_json() {
	curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null || true
}

# project_field NAME EXPR evaluates a Python expression over the Project,
# bound to p, and prints the result.
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
blocked_reason() { project_field "$1" 'cond("RecoveryBlocked").get("reason", "")'; }
blocked_message() { project_field "$1" 'cond("RecoveryBlocked").get("message", "")'; }
blocked_is() { [[ "$(blocked_reason "$1")" == "$2" ]]; }
not_blocked() { [[ -z "$(blocked_reason "$1")" ]]; }
failed_with() {
	phase_is "$1" Failed && [[ "$(project_field "$1" 'cond("Phase").get("reason", "")')" == "$2" ]]
}

create_project() {
	curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d "$1" >/dev/null
}

dind_image="${DIND_IMAGE:-docker:27-dind}"
postgres_image="${POSTGRES_IMAGE:-postgres:16}"
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
dind pull busybox:latest >/dev/null

# Every data assertion below depends on the agent and the workload containers
# seeing the same directory. Prove that now, from both sides of DinD, rather
# than let it surface later as a confusing marker timeout.
log "checking that $data_root is shared with the DinD daemon and its containers"
sentinel="$data_root/.shared-bind-sentinel"
printf 'shared-%s\n' "$suffix" >"$sentinel"
seen_by_daemon="$(docker exec "$dind_name" cat "$sentinel" 2>&1 || true)"
[[ "$seen_by_daemon" == "shared-$suffix" ]] ||
	fail "shared bind path unavailable: DinD daemon cannot read $sentinel (got: $seen_by_daemon); set E2E_SHARED_ROOT to a path that bind-mounts into DinD"
seen_by_container="$(dind run --rm -v "$data_root:$data_root:ro" busybox:latest cat "$sentinel" 2>&1 || true)"
[[ "$seen_by_container" == "shared-$suffix" ]] ||
	fail "shared bind path unavailable: a container inside DinD cannot read $sentinel (got: $seen_by_container); set E2E_SHARED_ROOT to a path that bind-mounts into DinD"
rm -f "$sentinel"

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
		UID_ENFORCEMENT=true \
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
		UID_ENFORCEMENT=true \
		"$test_root/bin/cara-agent"
) >"$test_root/agent.log" 2>&1 &
agent_pid=$!
wait_until 60 "node-a readiness" node_ready

# ── Target Project ───────────────────────────────────────────────────────────

project=recover-target
container="$project-web"
managed_dir="$data_root/volumes/default/$project/managed/data"
marker_in_container=/usr/share/nginx/html/marker.txt

log "creating $project with a Managed and an Ephemeral volume"
create_project '{
	"apiVersion":"caravanserai/v1",
	"kind":"Project",
	"metadata":{"name":"'"$project"'","namespace":"default"},
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
}'
wait_until 90 "$project Running" phase_is "$project" Running
wait_until 30 "$container running" container_running "$container"

printf 'managed-data-must-survive\n' >"$managed_dir/marker.txt"

marker_visible() {
	[[ "$(dind exec "$container" cat "$marker_in_container" 2>/dev/null || true)" == "managed-data-must-survive" ]]
}

# diagnose_marker prints the Managed directory as each layer sees it: the host
# the agent runs on, the Docker-in-Docker daemon that resolves bind sources,
# and the workload container. Where the three disagree is where the data went.
diagnose_marker() {
	log "diagnostics — host view of $managed_dir:"
	ls -la "$managed_dir" 2>&1 | sed 's/^/    /' || true
	log "diagnostics — DinD daemon view of the same path:"
	docker exec "$dind_name" ls -la "$managed_dir" 2>&1 | sed 's/^/    /' || true
	log "diagnostics — container mounts:"
	dind inspect -f '{{json .Mounts}}' "$container" 2>&1 | sed 's/^/    /' || true
	log "diagnostics — container view of $(dirname "$marker_in_container"):"
	dind exec "$container" ls -la "$(dirname "$marker_in_container")" 2>&1 | sed 's/^/    /' || true
	log "diagnostics — cat inside the container:"
	dind exec "$container" cat "$marker_in_container" 2>&1 | sed 's/^/    /' || true
	log "diagnostics — identity inside the container:"
	dind exec "$container" id 2>&1 | sed 's/^/    /' || true
}

# require_marker STAGE fails with diagnostics if the container cannot see the
# data the host wrote.
require_marker() {
	deadline=$((SECONDS + 10))
	until marker_visible; do
		if (( SECONDS >= deadline )); then
			diagnose_marker
			fail "$1: Managed data not visible inside the container"
		fi
		sleep 1
	done
}

require_marker "setup"

# ── A. Stopped container ─────────────────────────────────────────────────────

log "A: stopping $container; expecting it to be started again"
id_before="$(container_id "$container")"
dind stop "$container" >/dev/null
wait_until 45 "A: $container running again" container_running "$container"

[[ "$(container_id "$container")" == "$id_before" ]] ||
	fail "A: a stopped container should be started, not replaced"
phase_is "$project" Running || fail "A: phase should have stayed Running"
require_marker "A"
log "A: PASS — same container started, phase stayed Running, data intact"

# ── B. Removed container ─────────────────────────────────────────────────────

log "B: removing $container; expecting it to be recreated"
dind rm -f "$container" >/dev/null
wait_until 45 "B: $container recreated and running" container_running "$container"

[[ "$(container_id "$container")" != "$id_before" ]] || fail "B: expected a new container"
phase_is "$project" Running || fail "B: phase should have stayed Running"
require_marker "B"
log "B: PASS — container recreated against the existing data"

# ── C. Managed data missing ──────────────────────────────────────────────────

log "C: moving the Managed data away, then stopping $container"
# Move first, stop second: the running container keeps its mount, so the
# agent cannot slip a recovery in between the two steps.
mv "$managed_dir" "$managed_dir.moved"
dind stop "$container" >/dev/null

wait_until 45 "C: RecoveryBlocked/VolumeUnavailable" blocked_is "$project" VolumeUnavailable

container_running "$container" && fail "C: container was started with its data missing"
[[ -e "$managed_dir" ]] && fail "C: the Managed directory was recreated (Docker would start it empty)"
phase_is "$project" Running || fail "C: a blocked Project must stay Running, not go Failed"

message="$(blocked_message "$project")"
[[ "$message" == *'"managed"'* ]] || fail "C: condition message should name the volume: $message"
[[ "$message" != *"$data_root"* ]] || fail "C: condition message leaks the host path: $message"

log "C: holding for three polls to confirm the block neither clears nor exhausts"
sleep 30
blocked_is "$project" VolumeUnavailable || fail "C: block disappeared while the data was still missing"
phase_is "$project" Running || fail "C: blocked Project went Failed — attempts were spent"
container_running "$container" && fail "C: container started while blocked"
[[ -e "$managed_dir" ]] && fail "C: Managed directory appeared while blocked"

log "C: restoring the Managed data"
mv "$managed_dir.moved" "$managed_dir"
wait_until 45 "C: $container running after the data returned" container_running "$container"
wait_until 20 "C: RecoveryBlocked cleared" not_blocked "$project"
require_marker "C"
phase_is "$project" Running || fail "C: phase should be Running after recovery"
log "C: PASS — blocked without touching Docker, visible as a condition, recovered once data returned"

# ── D. A container that keeps exiting ────────────────────────────────────────

crashloop=recover-crashloop
log "D: creating $crashloop (busybox exits as soon as it starts)"
create_project '{
	"apiVersion":"caravanserai/v1",
	"kind":"Project",
	"metadata":{"name":"'"$crashloop"'","namespace":"default"},
	"spec":{"services":[{"name":"sh","image":"busybox:latest"}]}
}'
wait_until 150 "D: LocalRestartExhausted" failed_with "$crashloop" LocalRestartExhausted

# Matches the agent log in either zap encoding, JSON or console.
attempts="$(grep 'Recovering containers locally' "$test_root/agent.log" | grep -c "\"$crashloop\"" || true)"
log "D: agent logged $attempts recovery attempts"
[[ "$attempts" -eq 3 ]] || fail "D: expected exactly 3 attempts before exhaustion, saw $attempts"

phase_is "$project" Running || fail "D: the unrelated Project was disturbed"
log "D: PASS — three attempts, then Failed/LocalRestartExhausted"

# ── E. Running container from an earlier generation ─────────────────────────

log "E: bumping $project's assignment generation under its running container"
wait_until 45 "E: $container running before the bump" container_running "$container"
id_before="$(container_id "$container")"
label_generation="$(dind inspect -f '{{index .Config.Labels "cara.generation"}}' "$container")"
server_generation="$(project_field "$project" 'p["status"]["assignmentGeneration"]')"
[[ -n "$label_generation" && "$label_generation" == "$server_generation" ]] ||
	fail "E: setup expected the container label ($label_generation) to match the server ($server_generation)"

# The same node keeps the Project, but under a new grant of ownership — the
# state an A→B→A reassignment leaves behind. Written straight to the store, as
# the orphan rehearsal does, because no API moves a generation on its own.
bump="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET status = jsonb_set(status, '{assignmentGeneration}', to_jsonb((status->>'assignmentGeneration')::bigint + 1), true), updated_at = now() WHERE kind = 'Project' AND name = '$project';")"
[[ "$bump" == "UPDATE 1" ]] || fail "E: failed to bump the generation: $bump"
new_generation="$(project_field "$project" 'p["status"]["assignmentGeneration"]')"
log "E: container carries generation $label_generation, server now at $new_generation"

wait_until 45 "E: RecoveryBlocked/StaleContainer" blocked_is "$project" StaleContainer

[[ "$(container_id "$container")" == "$id_before" ]] || fail "E: the stale container was replaced"
container_running "$container" || fail "E: the stale container was stopped"
[[ "$(dind inspect -f '{{index .Config.Labels "cara.generation"}}' "$container")" == "$label_generation" ]] ||
	fail "E: the stale container was relabelled or adopted"
phase_is "$project" Running || fail "E: a blocked Project must stay Running"
message="$(blocked_message "$project")"
[[ "$message" == *'"web"'* ]] || fail "E: condition message should name the service: $message"
[[ "$message" != *"generation"* ]] || fail "E: condition message leaks label detail: $message"

log "E: holding for two polls to confirm the stale container is not judged healthy"
sleep 20
blocked_is "$project" StaleContainer || fail "E: the block was cleared while the container was still stale"
[[ "$(container_id "$container")" == "$id_before" ]] || fail "E: the stale container was replaced during the hold"
container_running "$container" || fail "E: the stale container was stopped during the hold"
log "E: PASS — stale running container blocked, left untouched, never counted as healthy"

log "PASS: stop and remove recovered; missing data blocked visibly and recovered on restore; crash loop exhausted; stale container blocked"
