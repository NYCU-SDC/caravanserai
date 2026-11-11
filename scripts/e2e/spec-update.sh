#!/usr/bin/env bash
set -Eeuo pipefail

# CARA-134 rehearsal: in-place spec updates against a real Docker daemon,
# driven through a real cara-server, cara-agent and caractl. The setup is the
# one scripts/e2e/local-recovery.sh uses: PostgreSQL and the workload Docker
# daemon run in disposable containers, and only those are removed afterwards.
#
# The VM scenario test/vm/scenarios/spec-update.sh covers the plain cases on
# real hosts. This script covers the ones it cannot arrange:
#
#   A. one service's image changes → only it is recreated, the other keeps its
#                               container, the phase is Running at every poll,
#                               Managed and Ephemeral data survive
#   B. an image that does not exist → the old container keeps running,
#                               SpecApplied False/ImagePullFailed, still Running
#   C. an update cut off after the old container was removed (agent stopped,
#                               container removed by hand, agent started) →
#                               the update creates it, tier-1 recovery is never
#                               called
#   D. another assignment's container present → nothing is recreated, the
#                               Project stays RecoveryBlocked/StaleContainer;
#                               once it is gone, the update runs
#   E. a service removed from the spec → its container is removed
#   F. caractl rollout restart → every service is recreated

log() { printf '[spec-update-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

shared_root="${E2E_SHARED_ROOT:-/var/tmp}"
[[ -d "$shared_root" && -w "$shared_root" ]] || fail "E2E_SHARED_ROOT is not a writable directory: $shared_root"
test_root="$(mktemp -d "$shared_root/cara-spec-update-e2e.XXXXXX")"
. "$(dirname "${BASH_SOURCE[0]}")/lib/auth.sh"
suffix="${PPID}-$$"
postgres_name="cara134-postgres-${suffix}"
dind_name="cara134-dind-${suffix}"
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
			cara-spec-update-e2e.*) rm -rf -- "$test_root" ;;
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
dind pull nginx:1.27-alpine >/dev/null
dind pull nginx:1.28-alpine >/dev/null
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
go build -o "$test_root/bin/caractl" ./cmd/caractl

server_port="$(choose_port)"
server_url="http://127.0.0.1:$server_port"
(
	cd "$test_root/server-run"
	env \
		HOST=127.0.0.1 \
		PORT="$server_port" \
		BREAK_GLASS_TOKEN_FILE="$e2e_admin_token_file" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

cat >"$test_root/agent-run/config.yaml" <<EOF
data_root: "$data_root"
EOF

start_agent() {
	local token_file
	token_file="$(join_token_file node-a)"
	(
		cd "$test_root/agent-run"
		env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
			SERVER_URL="$server_url" \
			JOIN_TOKEN_FILE="$token_file" \
			NODE_NAME=node-a \
			HEARTBEAT_INTERVAL=1s \
			DOCKER_HOST="$dind_host" \
			AGENT_LISTEN_PORT=0 \
			PROXY_LISTEN_ADDR=127.0.0.1:0 \
			AGENT_ADVERTISE_IP=127.0.0.1 \
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

project=spec-target
web="$project-web"
side="$project-side"
managed_dir="$data_root/volumes/default/$project/managed/data"
ctl() { CARA_TOKEN="$e2e_admin_token" "$test_root/bin/caractl" --server "$server_url" "$@"; }

# put_spec <web image> <with side: yes|no> sends the whole spec.
put_spec() {
	local services='{"name":"web","image":"'"$1"'","volumeMounts":[{"name":"managed","mountPath":"/usr/share/nginx/html"},{"name":"scratch","mountPath":"/scratch"}]}'
	if [[ $2 == yes ]]; then
		services+=',{"name":"side","image":"busybox:latest","command":["sh","-c","exec sleep 86400"]}'
	fi
	local body='{"apiVersion":"caravanserai/v1","kind":"Project","metadata":{"name":"'"$project"'","namespace":"default"},
		"spec":{"services":['"$services"'],"volumes":[{"name":"managed","type":"Managed"},{"name":"scratch","type":"Ephemeral"}]}}'
	if [[ -z "$(project_json "$project")" ]]; then
		create_project "$body"
	else
		curl -fsS -X PUT "$server_url/api/v1/projects/$project" -H 'Content-Type: application/json' -d "$body" >/dev/null
	fi
}

revision() { project_field "$project" 'p["metadata"].get("specRevision", 0)'; }
observed() { project_field "$project" 'p["status"].get("observedSpecRevision", 0)'; }
applied() { project_field "$project" 'cond("SpecApplied").get("status", "") + "/" + cond("SpecApplied").get("reason", "")'; }
image_of() { dind inspect -f '{{.Config.Image}}' "$1" 2>/dev/null || true; }

# wait_applied <revision> <stage> waits for revision to be reported running and
# fails if the phase was anything but Running on the way.
wait_applied() {
	local deadline=$((SECONDS + 120)) phase
	until [[ "$(observed)" == "$1" && "$(applied)" == True/Applied ]]; do
		phase="$(project_field "$project" 'p["status"]["phase"]')"
		[[ "$phase" == Running ]] || fail "$2: phase was $phase while revision $1 was applied"
		(( SECONDS < deadline )) || fail "$2: revision $1 not applied after 120s (observed $(observed), SpecApplied $(applied))"
		sleep 1
	done
	log "$2: revision $1 applied"
}
recoveries() { grep -c 'Recovering containers locally' "$test_root/agent.log" || true; }

log "placing $project"
put_spec nginx:1.27-alpine yes
wait_until 120 "$project Running" phase_is "$project" Running
wait_applied 1 setup
printf 'managed-data-must-survive\n' >"$managed_dir/marker.txt"
dind exec "$web" sh -c 'echo ephemeral-data-must-survive > /scratch/marker.txt'

log "A: change web's image"
web_before="$(container_id "$web")"
side_before="$(container_id "$side")"
put_spec nginx:1.28-alpine yes
wait_applied 2 A
[[ "$(container_id "$web")" != "$web_before" ]] || fail "A: web was not recreated"
[[ "$(image_of "$web")" == nginx:1.28-alpine ]] || fail "A: web runs $(image_of "$web")"
[[ "$(container_id "$side")" == "$side_before" ]] || fail "A: side was recreated though it did not change"
[[ "$(dind exec "$web" cat /usr/share/nginx/html/marker.txt)" == managed-data-must-survive ]] || fail "A: Managed data lost"
[[ "$(dind exec "$web" cat /scratch/marker.txt)" == ephemeral-data-must-survive ]] || fail "A: Ephemeral data lost"
[[ "$(recoveries)" == 0 ]] || fail "A: tier-1 recovery ran during the update"
log "A: passed"

log "B: an image that does not exist"
web_before="$(container_id "$web")"
put_spec nginx:cara-no-such-tag yes
pull_failed() { [[ "$(applied)" == False/ImagePullFailed ]]; }
wait_until 60 "SpecApplied False/ImagePullFailed" pull_failed
[[ "$(container_id "$web")" == "$web_before" ]] || fail "B: web was replaced"
container_running "$web" || fail "B: the old web container stopped"
phase_is "$project" Running || fail "B: phase is not Running"
[[ "$(observed)" == 2 ]] || fail "B: observed moved to $(observed)"
log "B: passed"

log "C: an update cut off after the old container was removed"
stop_agent
put_spec nginx:1.27-alpine yes
dind rm -f "$web" >/dev/null
recoveries_before="$(recoveries)"
start_agent
wait_applied 4 C
[[ "$(image_of "$web")" == nginx:1.27-alpine ]] || fail "C: web runs $(image_of "$web")"
[[ "$(recoveries)" == "$recoveries_before" ]] || fail "C: tier-1 recovery took the missing container"
log "C: passed"

log "D: another assignment's container is present"
uid="$(project_field "$project" 'p["metadata"]["uid"]')"
generation="$(project_field "$project" 'p["status"]["assignmentGeneration"]')"
dind run -d --name "$project-old" \
	--label "cara.project=$project" --label cara.namespace=default --label cara.service=old \
	--label "cara.uid=$uid" --label "cara.generation=$((generation - 1))" \
	nginx:1.27-alpine >/dev/null
web_before="$(container_id "$web")"
put_spec nginx:1.28-alpine yes
stale_blocked() { blocked_is "$project" StaleContainer; }
wait_until 60 "RecoveryBlocked/StaleContainer" stale_blocked
sleep 15
[[ "$(container_id "$web")" == "$web_before" ]] || fail "D: web was recreated beside another assignment's container"
[[ "$(observed)" == 4 ]] || fail "D: observed moved to $(observed)"
container_running "$project-old" || fail "D: the other assignment's container was touched"
dind rm -f "$project-old" >/dev/null
wait_applied 5 D
log "D: passed"

log "E: remove side from the spec"
put_spec nginx:1.28-alpine no
wait_applied 6 E
[[ -z "$(container_id "$side")" ]] || fail "E: side's container is still there"
log "E: passed"

log "F: caractl rollout restart"
web_before="$(container_id "$web")"
ctl rollout restart "$project" | tee -a "$test_root/caractl.log"
wait_applied 7 F
[[ "$(container_id "$web")" != "$web_before" ]] || fail "F: web was not recreated"
[[ "$(dind exec "$web" cat /usr/share/nginx/html/marker.txt)" == managed-data-must-survive ]] || fail "F: Managed data lost"
log "F: passed"

log "all scenarios passed"
