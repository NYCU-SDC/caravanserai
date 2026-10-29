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
#   F. the same, while the Project is Scheduled → not reported Running, not
#                               reconciled, and the container survives (CARA-93)
#   G. a container for a service the spec no longer declares → still blocks the
#                               new assignment, which no per-service check can see (CARA-93)
#   H. deleting a Project      → every generation of its lifetime removed, nothing else;
#                               an earlier Node's open obligation holds it Terminated
#                               with ResourcesRemoved already recorded, until that Node
#                               reports (CARA-80)
#   I. deleting while the node is lost → kept Terminating with TerminationBlocked,
#                               never finalized; the node returns → cleared, torn
#                               down and finalized (CARA-80). The server runs with
#                               TERMINATING_TIMEOUT=15s so this takes minutes, not ten

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
		TERMINATING_TIMEOUT=15s \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

cat >"$test_root/agent-run/config.yaml" <<EOF
data_root: "$data_root"
EOF

start_agent() {
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
	) >>"$test_root/agent.log" 2>&1 &
	agent_pid=$!
}

stop_agent() {
	kill "$agent_pid" >/dev/null 2>&1 || true
	wait "$agent_pid" >/dev/null 2>&1 || true
	agent_pid=""
}

start_agent
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

# ── F. The same stale container, seen by a Scheduled Project ─────────────────

log "F: moving $project back to Scheduled while generation $label_generation's container still runs"
# Scheduled is what a fresh assignment looks like before the agent reports it
# Running; reconcileOne handles it, not the health check. Written straight to
# the store like the generation bump above.
reschedule="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET phase = 'Scheduled', status = jsonb_set(status, '{phase}', to_jsonb('Scheduled'::text), true), updated_at = now() WHERE kind = 'Project' AND name = '$project';")"
[[ "$reschedule" == "UPDATE 1" ]] || fail "F: failed to move the Project to Scheduled: $reschedule"
wait_until 10 "F: $project reads Scheduled" phase_is "$project" Scheduled

log "F: holding for three polls; a stale running container must not satisfy the new assignment"
sleep 30
phase_is "$project" Scheduled ||
	fail "F: the Project left Scheduled — generation $label_generation's container was taken as generation $new_generation's"
[[ "$(container_id "$container")" == "$id_before" ]] || fail "F: the stale container was replaced or removed"
container_running "$container" || fail "F: the stale container was stopped"
blocked_is "$project" StaleContainer || fail "F: the StaleContainer block was cleared"
log "F: PASS — Scheduled Project not adopted onto the stale container, which survived untouched"

# ── G. A container for a service the spec no longer declares ────────────────

dropped=recover-dropped
dropped_container="$dropped-worker"

log "G: creating $dropped with a single service, worker"
create_project '{
	"apiVersion":"caravanserai/v1",
	"kind":"Project",
	"metadata":{"name":"'"$dropped"'","namespace":"default"},
	"spec":{"services":[{"name":"worker","image":"nginx:alpine"}]}
}'
wait_until 90 "G: $dropped Running" phase_is "$dropped" Running
wait_until 30 "G: $dropped_container running" container_running "$dropped_container"
dropped_id="$(container_id "$dropped_container")"

# The next assignment declares a different service and carries a new
# generation. worker is gone from the spec, so nothing that walks the spec can
# see the container it left behind.
log "G: replacing the spec's worker with web and bumping the generation"
respec="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET spec = jsonb_set(spec, '{services}', '[{\"name\": \"web\", \"image\": \"nginx:alpine\"}]'::jsonb, true), status = jsonb_set(status, '{assignmentGeneration}', to_jsonb((status->>'assignmentGeneration')::bigint + 1), true), updated_at = now() WHERE kind = 'Project' AND name = '$dropped';")"
[[ "$respec" == "UPDATE 1" ]] || fail "G: failed to replace the spec: $respec"

wait_until 45 "G: RecoveryBlocked/StaleContainer" blocked_is "$dropped" StaleContainer

container_running "$dropped_container" || fail "G: the dropped service's container was stopped"
[[ "$(container_id "$dropped_container")" == "$dropped_id" ]] || fail "G: it was replaced or removed"
if dind inspect "$dropped-web" >/dev/null 2>&1; then
	fail "G: the new assignment started beside generation 1's worker"
fi
message="$(blocked_message "$dropped")"
[[ "$message" == *'"worker"'* ]] || fail "G: the condition should name the dropped service: $message"

log "G: holding for two polls to confirm the block holds"
sleep 20
blocked_is "$dropped" StaleContainer || fail "G: the block was cleared"
if dind inspect "$dropped-web" >/dev/null 2>&1; then
	fail "G: the new assignment started during the hold"
fi
log "G: PASS — a container for a dropped service still blocks the new assignment"

# ── H. Deleting a Project takes its whole lifetime, and nothing else ─────────
#
# recover-target still has generation 1's container running beside the
# generation-2 assignment E and F left it with. Deleting the Project must take
# every generation of its lifetime — the network and the Ephemeral volume
# too — keep the Managed data, and leave alone a container that carries the
# same Project name under another lifetime's UID: that one is not this
# deletion's to remove.

foreign_container="$project-sidecar"
scratch_volume="cara-$project-scratch"

log "H: planting a container under $project's name for another lifetime"
dind run -d --name "$foreign_container" \
	--label "cara.project=$project" --label cara.namespace=default \
	--label cara.service=sidecar --label cara.uid=uid-of-another-lifetime \
	--label cara.generation=1 \
	nginx:alpine >/dev/null
foreign_id="$(container_id "$foreign_container")"
[[ -n "$foreign_id" ]] || fail "H: could not create the other lifetime's container"

container_running "$container" || fail "H: setup expected generation $label_generation's container to be running"
dind volume inspect "$scratch_volume" >/dev/null || fail "H: setup expected the Ephemeral volume $scratch_volume"

ghost_uid="$(project_field "$project" 'p["metadata"]["uid"]')"
[[ -n "$ghost_uid" ]] || fail "H: could not read $project's UID"
ghost="$(docker exec "$postgres_name" psql -U postgres -d caravanserai -Atc \
	"UPDATE resources SET status = jsonb_set(status, '{recovery}',
		COALESCE(status->'recovery', '{}'::jsonb) || jsonb_build_object('cleanupObligations',
			COALESCE(status->'recovery'->'cleanupObligations', '[]'::jsonb) || jsonb_build_array(jsonb_build_object(
				'assignment', jsonb_build_object('projectUID', '$ghost_uid', 'nodeName', 'node-ghost', 'assignmentGeneration', 99),
				'releasedAt', to_jsonb(now()), 'releaseAuthorization', 'WorkloadStopped')))),
		resource_version = resource_version + 1, updated_at = now()
	WHERE kind = 'Project' AND name = '$project';")"
[[ "$ghost" == "UPDATE 1" ]] || fail "H: failed to plant node-ghost's obligation: $ghost"
held_by_ghost() {
	phase_is "$project" Terminated &&
		[[ "$(project_field "$project" 'cond("FinalizationBlocked").get("message", "")')" == *"node node-ghost generation 99"* ]]
}

log "H: deleting $project"
delete_status="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$server_url/api/v1/projects/$project")"
[[ "$delete_status" == "202" ]] || fail "H: delete returned $delete_status, want 202"

# Terminated lasts milliseconds on its own: the record is finalized as soon
# as the predicate holds, too fast to observe. An earlier Node that still owes
# its cleanup holds it there, so the order of the reports can be checked at the
# transition rather than inferred from the final 404. No API adds an
# obligation, and this rehearsal has one Node, so it is written straight to the
# store, for a Node that does not exist.
wait_until 90 "H: $project Terminated and held by node-ghost" held_by_ghost

removal_evidence='p["status"]["phase"] + ("+removed" if any(
    o.get("resourcesRemoved")
    and o["assignment"]["nodeName"] == p["status"].get("nodeRef")
    and o["assignment"].get("assignmentGeneration", 0) == p["status"].get("assignmentGeneration", 0)
    for o in (p["status"].get("recovery") or {}).get("cleanupObligations") or []) else "")'
[[ "$(project_field "$project" "$removal_evidence")" == "Terminated+removed" ]] ||
	fail "H: $project is Terminated without ResourcesRemoved for its own assignment"
log "H: $project held at Terminated by node-ghost, with ResourcesRemoved already recorded for its assignment"

log "H: node-ghost reports its cleanup done"
ghost_status="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
	"$server_url/api/v1/projects/$project/recovery/resources-removed" -H 'Content-Type: application/json' \
	-d "{\"uid\":\"$ghost_uid\",\"nodeRef\":\"node-ghost\",\"assignmentGeneration\":99}")"
[[ "$ghost_status" == 2* ]] || fail "H: node-ghost's ResourcesRemoved report returned $ghost_status"

project_gone() { [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$server_url/api/v1/projects/$project")" == "404" ]]; }
wait_until 30 "H: $project finalized once node-ghost reported" project_gone

# A failed listing is not an absent resource: the helpers below return non-zero
# when the daemon cannot be asked, so wait_until keeps waiting and fails.
lifetime_containers() {
	listed="$(dind ps -a --filter "label=cara.project=$project" --format '{{.Names}}')" || return 1
	grep -vxF -- "$foreign_container" <<<"$listed" | grep -c . || true
}
no_lifetime_containers() { n="$(lifetime_containers)" || return 1; [[ "$n" == "0" ]]; }
volume_absent() {
	listed="$(dind volume ls --format '{{.Name}}')" || return 1
	! grep -qxF -- "$1" <<<"$listed"
}
network_absent() {
	listed="$(dind network ls --format '{{.Name}}')" || return 1
	! grep -qxF -- "$1" <<<"$listed"
}

wait_until 30 "H: every generation's container removed" no_lifetime_containers
wait_until 30 "H: Ephemeral volume removed" volume_absent "$scratch_volume"
wait_until 30 "H: network removed" network_absent "cara-$project"
[[ -f "$managed_dir/marker.txt" ]] || fail "H: Managed data was removed with the Project"
[[ "$(container_id "$foreign_container")" == "$foreign_id" ]] ||
	fail "H: the other lifetime's container was removed by this Project's deletion"
log "H: PASS — every generation removed, network and Ephemeral volume gone, Managed data kept, other lifetime untouched"

# ── I. Deleting a Project while its node is lost ─────────────────────────────
#
# Nothing but the node can confirm that its resources are gone, so a deletion
# that cannot reach the node must keep the record — visibly blocked, not
# finalized — until the node returns and finishes the teardown.

lost=lost-delete
lost_container="$lost-web"

termination_blocked() {
	[[ "$(project_field "$lost" 'cond("TerminationBlocked").get("status", "")')" == "True" ]]
}
not_termination_blocked() {
	[[ -n "$(project_json "$lost")" ]] &&
		[[ -z "$(project_field "$lost" 'cond("TerminationBlocked").get("status", "")')" ]]
}
node_not_ready() {
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"NotReady"'* ]]
}
lost_gone() { [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$server_url/api/v1/projects/$lost")" == "404" ]]; }
lost_container_absent() {
	listed="$(dind ps -a --format '{{.Names}}')" || return 1
	! grep -qxF -- "$lost_container" <<<"$listed"
}

log "I: creating $lost"
create_project '{
	"apiVersion":"caravanserai/v1",
	"kind":"Project",
	"metadata":{"name":"'"$lost"'","namespace":"default"},
	"spec":{"services":[{"name":"web","image":"nginx:alpine"}]}
}'
wait_until 90 "I: $lost Running" phase_is "$lost" Running
wait_until 30 "I: $lost_container running" container_running "$lost_container"

log "I: stopping the agent, then deleting $lost"
stop_agent
delete_status="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$server_url/api/v1/projects/$lost")"
[[ "$delete_status" == "202" ]] || fail "I: delete returned $delete_status, want 202"
phase_is "$lost" Terminating || fail "I: $lost did not enter Terminating"

# The heartbeat timeout is 90s, then the 15s termination timeout runs.
wait_until 240 "I: node-a NotReady" node_not_ready
wait_until 120 "I: $lost marked TerminationBlocked" termination_blocked

log "I: holding past the termination timeout to confirm the record is kept"
sleep 30
phase_is "$lost" Terminating || fail "I: $lost left Terminating while its node was lost"
container_running "$lost_container" || fail "I: setup expected $lost_container to still be running"

log "I: restarting the agent"
start_agent
wait_until 60 "I: node-a Ready again" node_ready
wait_until 60 "I: TerminationBlocked cleared" not_termination_blocked
wait_until 90 "I: $lost finalized" lost_gone
wait_until 30 "I: $lost_container removed" lost_container_absent
log "I: PASS — a deletion on a lost node was kept and reported blocked, then finished when the node returned"

log "PASS: stop and remove recovered; missing data blocked visibly and recovered on restore; crash loop exhausted; stale containers blocked on the Running and Scheduled paths, including one whose service the spec no longer declares; deletion removed every generation of the lifetime and nothing of another; a deletion on a lost node was kept until the node returned"
