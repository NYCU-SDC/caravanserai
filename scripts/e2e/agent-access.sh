#!/usr/bin/env bash
set -Eeuo pipefail

# CARA-139 rehearsal: caractl logs and port-forward reach a real agent only
# through cara-server, and the agent refuses everything else.
#
# Unlike the other e2e scripts, cara-agent runs inside the DinD container, next
# to the daemon it drives, as it would on a Node: port-forward dials the
# container's bridge address, which only exists inside DinD. cara-server runs
# on the host, bound to the Docker bridge gateway so the agent can reach it,
# and reaches the agent on the DinD container's bridge address.
#
# E2E_OVERLAY=1 also starts a disposable Headscale; cara-server and cara-agent
# join it, the agent advertises its overlay address, and cara-server dials the
# agent through its tsnet transport. caractl stays off the overlay.
#
#   A. caractl logs              → the service's output, through the server
#   B. caractl logs --follow     → a line written after the stream opened arrives
#   C. caractl port-forward      → nginx answers on the local port
#   D. caractl node probe        → OK, over the signed probe path
#   E. the agent API directly    → 401 on logs, forward and probe; /healthz 200
#   F. caractl without a token   → 401 from cara-server
#   G. --node host:port          → refused by caractl before anything is sent

log() { printf '[agent-access-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

shared_root="${E2E_SHARED_ROOT:-/var/tmp}"
[[ -d "$shared_root" && -w "$shared_root" ]] || fail "E2E_SHARED_ROOT is not a writable directory: $shared_root"
test_root="$(mktemp -d "$shared_root/cara-agent-access-e2e.XXXXXX")"
. "$(dirname "${BASH_SOURCE[0]}")/lib/auth.sh"
suffix="${PPID}-$$"
postgres_name="cara139-postgres-${suffix}"
dind_name="cara139-dind-${suffix}"
headscale_name="cara139-headscale-${suffix}"
overlay="${E2E_OVERLAY:-0}"
server_pid=""
follow_pid=""
forward_pid=""

cleanup() {
	status=$?
	trap - EXIT INT TERM

	for pid in "$forward_pid" "$follow_pid" "$server_pid"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done

	docker exec "$dind_name" cat /var/log/cara-agent.log >"$test_root/agent.log" 2>/dev/null || true
	docker logs "$dind_name" >"$test_root/dind.log" 2>&1 || true

	if (( status != 0 )); then
		for log_file in "$test_root/agent.log" "$test_root/server.log"; do
			if [[ -f "$log_file" ]]; then
				printf '\n--- %s (last 120 lines) ---\n' "$log_file"
				tail -n 120 "$log_file" || true
			fi
		done
	fi

	if [[ "$overlay" == 1 ]]; then
		docker logs "$headscale_name" >"$test_root/headscale.log" 2>&1 || true
	fi
	docker rm -f -v "$dind_name" "$postgres_name" "$headscale_name" >/dev/null 2>&1 || true

	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
			cara-agent-access-e2e.*) rm -rf -- "$test_root" ;;
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

server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }
node_ready() {
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"Ready"'* ]]
}
phase_is() {
	curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["status"].get("phase",""))' 2>/dev/null |
		grep -qx "$2"
}

ctl() { CARA_TOKEN="$e2e_admin_token" "$test_root/bin/caractl" --server "$server_url" "$@"; }

dind_image="${DIND_IMAGE:-docker:27-dind}"
postgres_image="${POSTGRES_IMAGE:-postgres:16}"
mkdir -p "$test_root/bin" "$test_root/server-run"

bridge_gateway="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')"
[[ -n "$bridge_gateway" ]] || fail "cannot find the Docker bridge gateway"

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
docker run -d --privileged --name "$dind_name" -e DOCKER_TLS_CERTDIR= "$dind_image" >/dev/null
dind_ready() { docker exec "$dind_name" docker info >/dev/null 2>&1; }
wait_until 90 "Docker-in-Docker readiness" dind_ready
docker exec "$dind_name" docker pull nginx:1.27-alpine >/dev/null
docker exec "$dind_name" docker pull busybox:latest >/dev/null
dind_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$dind_name")"
[[ -n "$dind_ip" ]] || fail "the DinD container has no bridge address"

overlay_env=()
if [[ "$overlay" == 1 ]]; then
	headscale_image="${HEADSCALE_IMAGE:-headscale/headscale:v0.29.2}"
	headscale_port="$(choose_port)"
	headscale_url="http://$bridge_gateway:$headscale_port"
	log "starting isolated Headscale ($headscale_name, $headscale_url)"
	sed "s#^server_url: .*#server_url: $headscale_url#" deploy/dev/headscale/config.yaml >"$test_root/headscale.yaml"
	# cara-server joins through 127.0.0.1: tsnet on the host would not reach
	# Headscale on the bridge gateway address (its control dial hung there
	# in this setup), while the agent inside DinD reaches it only there.
	docker run -d --name "$headscale_name" \
		-p "127.0.0.1:$headscale_port:8080" \
		-p "$bridge_gateway:$headscale_port:8080" \
		-v "$test_root/headscale.yaml:/etc/headscale/config.yaml:ro" \
		"$headscale_image" serve >/dev/null
	headscale_ready() { docker exec "$headscale_name" headscale health >/dev/null 2>&1; }
	wait_until 60 "Headscale readiness" headscale_ready
	docker exec "$headscale_name" headscale users create cara-node >/dev/null
	headscale_user_id="$(docker exec "$headscale_name" headscale users list -o json |
		python3 -c 'import json,sys; print([u["id"] for u in json.load(sys.stdin) if u["name"]=="cara-node"][0])')"
	new_preauth_key() {
		docker exec "$headscale_name" headscale preauthkeys create --user "$headscale_user_id" --expiration 1h -o json |
			python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])'
	}
	(umask 077 && new_preauth_key >"$test_root/server.preauth-key" && new_preauth_key >"$test_root/agent.preauth-key")
	overlay_env=(
		HEADSCALE_URL="http://127.0.0.1:$headscale_port"
		HEADSCALE_PREAUTH_KEY_FILE="$test_root/server.preauth-key"
		OVERLAY_STATE_DIR="$test_root/server-tsnet"
	)
fi

log "building cara-server, cara-agent and caractl"
go build -o "$test_root/bin/cara-server" ./cmd/cara-server
CGO_ENABLED=0 go build -o "$test_root/bin/cara-agent" ./cmd/cara-agent
go build -o "$test_root/bin/caractl" ./cmd/caractl

server_port="$(choose_port)"
server_url="http://$bridge_gateway:$server_port"
(
	cd "$test_root/server-run"
	env "${overlay_env[@]}" \
		HOST="$bridge_gateway" \
		PORT="$server_port" \
		BREAK_GLASS_TOKEN_FILE="$e2e_admin_token_file" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 150 "cara-server readiness" server_ready

log "starting cara-agent inside DinD ($dind_ip:9090)"
docker cp "$test_root/bin/cara-agent" "$dind_name:/usr/local/bin/cara-agent" >/dev/null
docker cp "$(join_token_file node-a)" "$dind_name:/run/node-a.join-token" >/dev/null
agent_overlay_env=""
if [[ "$overlay" == 1 ]]; then
	docker cp "$test_root/agent.preauth-key" "$dind_name:/run/node-a.preauth-key" >/dev/null
	agent_overlay_env="HEADSCALE_URL='$headscale_url' HEADSCALE_PREAUTH_KEY_FILE=/run/node-a.preauth-key"
fi
docker exec -d "$dind_name" sh -c "cd /tmp && exec env $agent_overlay_env \
	SERVER_URL='$server_url' \
	JOIN_TOKEN_FILE=/run/node-a.join-token \
	NODE_NAME=node-a \
	HEARTBEAT_INTERVAL=1s \
	DOCKER_HOST=unix:///var/run/docker.sock \
	AGENT_LISTEN_PORT=9090 \
	PROXY_LISTEN_ADDR=127.0.0.1:0 \
	AGENT_ADVERTISE_IP='$dind_ip' \
	AGENT_DATA_ROOT=/var/lib/cara \
	cara-agent >/var/log/cara-agent.log 2>&1"
wait_until 60 "node-a readiness" node_ready

node_address="$(curl -fsS "$server_url/api/v1/nodes/node-a" |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["network"]["overlayIP"])')"
if [[ "$overlay" == 1 ]]; then
	[[ "$node_address" == 100.* ]] || fail "with overlay, node-a advertises $node_address, not an overlay address"
	log "node-a is reached over the overlay at $node_address"
else
	[[ "$node_address" == "$dind_ip" ]] || fail "node-a advertises $node_address, want $dind_ip"
fi

project=access-target
curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d '{
	"apiVersion": "caravanserai/v1", "kind": "Project",
	"metadata": {"name": "'"$project"'"},
	"spec": {"size": "Small", "services": [
		{"name": "web", "image": "nginx:1.27-alpine"},
		{"name": "talker", "image": "busybox:latest",
		 "command": ["sh", "-c", "echo hello-from-talker; while true; do sleep 1; if [ -f /tmp/more ]; then echo later-line; rm /tmp/more; fi; done"]}
	]}
}' >/dev/null
wait_until 120 "$project Running" phase_is "$project" Running

log "A: caractl logs"
out="$(ctl logs "$project/talker")" || fail "caractl logs failed: $out"
[[ "$out" == *hello-from-talker* ]] || fail "caractl logs output: $out"
log "A: passed"

log "B: caractl logs --follow"
ctl logs "$project/talker" --follow >"$test_root/follow.out" 2>&1 &
follow_pid=$!
wait_until 20 "the followed stream to open" grep -q hello-from-talker "$test_root/follow.out"
docker exec "$dind_name" docker exec "$project-talker" touch /tmp/more
wait_until 20 "a line written after the stream opened" grep -q later-line "$test_root/follow.out"
kill "$follow_pid" >/dev/null 2>&1 || true
wait "$follow_pid" >/dev/null 2>&1 || true
follow_pid=""
log "B: passed"

log "C: caractl port-forward"
local_port="$(choose_port)"
ctl port-forward "$project/web" "$local_port:80" >"$test_root/forward.out" 2>&1 &
forward_pid=$!
wait_until 20 "the local listener" grep -q Forwarding "$test_root/forward.out"
page="$(command curl -fsS --max-time 10 "http://127.0.0.1:$local_port/")" || fail "port-forward: curl failed; $(cat "$test_root/forward.out")"
[[ "$page" == *"Welcome to nginx"* ]] || fail "port-forward returned: $page"
# A second connection opens a second tunnel, with its own signature.
command curl -fsS --max-time 10 "http://127.0.0.1:$local_port/" | grep -q "Welcome to nginx" || fail "second port-forward connection"
kill "$forward_pid" >/dev/null 2>&1 || true
wait "$forward_pid" >/dev/null 2>&1 || true
forward_pid=""
log "C: passed"

log "D: caractl node probe"
out="$(ctl node probe node-a)" || fail "node probe failed: $out"
[[ "$out" == *OK* ]] || fail "node probe output: $out"
log "D: passed"

log "E: the agent API directly"
agent_url="http://$dind_ip:9090"
for path in "/api/v1/logs/$project/talker" "/api/v1/forward/$project/web/80" /api/v1/probe; do
	code="$(command curl -s -o /dev/null -w '%{http_code}' "$agent_url$path")"
	[[ "$code" == 401 ]] || fail "direct $path answered $code, want 401"
done
code="$(command curl -s -o /dev/null -w '%{http_code}' "$agent_url/healthz")"
[[ "$code" == 200 ]] || fail "direct /healthz answered $code, want 200"
log "E: passed"

log "F: caractl without a token"
if out="$("$test_root/bin/caractl" --server "$server_url" logs "$project/talker" 2>&1)"; then
	fail "caractl logs without a token succeeded: $out"
fi
[[ "$out" == *401* || "$out" == *Unauthorized* ]] || fail "caractl logs without a token: $out"
log "F: passed"

log "G: --node with an agent address"
if out="$(ctl logs "$project/talker" --node "$dind_ip:9090" 2>&1)"; then
	fail "caractl logs --node host:port succeeded: $out"
fi
[[ "$out" == *"agents accept no one else"* ]] || fail "caractl logs --node host:port: $out"
log "G: passed"

log "all scenarios passed"
