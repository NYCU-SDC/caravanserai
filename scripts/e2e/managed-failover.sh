#!/usr/bin/env bash
set -Eeuo pipefail

# Isolated rehearsal of losing the Agent under Projects with Managed volume
# data. Since CARA-84 it proves the Projects are not moved while the lost Node
# may still be running them. The CARA-87 scenarios — a clean destination
# restores the backed-up bytes, and a destination that already holds
# unprovable bytes refuses to start rather than adopt them — need an
# authorized takeover and are reported NOT RUN until one exists.
#
# Two Docker-in-Docker daemons stand in for two Nodes. They are separate on
# purpose: stopping an Agent does not stop its containers, and container names
# are derived from (project, service), so a single shared daemon would make
# Node B collide with containers Node A never got to release.
#
# Node A's Agent stops while its containers keep running. Since CARA-84 the
# control plane must not reassign on that alone (ADR-0017): the run first
# proves both Projects stay on node-a, marked TakeoverRequired, with nothing
# started on node-b.
#
# Two destination scenarios then need the control plane to reassign. That now
# takes an authorized takeover, which does not exist yet, so they are kept
# below and reported NOT RUN until it does:
#
#   clean B  Node B holds nothing for the Project.
#            → restores the generation A backed up, serves the same bytes.
#
#   stale B  Node B holds bytes from an earlier placement and a v1 marker.
#            → RecoveryBlocked. No container, no rewritten marker, no deleted
#              bytes, no new backup generation.
#
# The second is the regression test. Before CARA-87 a v1 marker was proof
# enough to skip the restore, so that Project would have started on the stale
# bytes and then had them archived over the good generation. Verify that by
# running this script against the parent commit: the stale scenario must fail
# there, or it is not testing the defect.
#
# Managed volume data must be visible at the same path to the Agent (on the
# host) and to the workload containers (inside DinD). It defaults to /var/tmp;
# a directory under /tmp was observed not to bind through. The script proves
# the path really is shared before running anything.

log() { printf '[failover-e2e] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

for command in docker curl go python3 sha256sum; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done

shared_root="${E2E_SHARED_ROOT:-/var/tmp}"
[[ -d "$shared_root" && -w "$shared_root" ]] || fail "E2E_SHARED_ROOT is not a writable directory: $shared_root"
test_root="$(mktemp -d "$shared_root/cara-failover-e2e.XXXXXX")"
suffix="${PPID}-$$"
postgres_name="cara87-postgres-${suffix}"
minio_name="cara87-minio-${suffix}"
dind_a_name="cara87-dind-a-${suffix}"
dind_b_name="cara87-dind-b-${suffix}"
server_pid=""
agent_a_pid=""
agent_b_pid=""

cleanup() {
	status=$?
	trap - EXIT INT TERM

	for pid in "$agent_b_pid" "$agent_a_pid" "$server_pid"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done

	if (( status != 0 )); then
		# The Agents decide everything this script asserts, so their logs come
		# first and whole. The server's are filtered to what a failure needs:
		# its request log is one line per poll per Project and would bury the
		# failure message that sent anyone here.
		for log_file in "$test_root/agent-a.log" "$test_root/agent-b.log"; do
			if [[ -f "$log_file" ]]; then
				printf '\n--- %s (last 60 lines) ---\n' "$log_file"
				tail -n 60 "$log_file" || true
			fi
		done
		if [[ -f "$test_root/server.log" ]]; then
			printf '\n--- %s (decisions only) ---\n' "$test_root/server.log"
			grep -v '"msg":"Request completed"' "$test_root/server.log" | tail -n 40 || true
		fi
	fi

	# -v removes the anonymous volumes these images declare. The names are
	# unique to this run, so nothing else on the host daemon is touched.
	docker rm -f -v "$dind_a_name" "$dind_b_name" "$minio_name" "$postgres_name" >/dev/null 2>&1 || true

	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
			cara-failover-e2e.*) rm -rf -- "$test_root" ;;
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
		sleep 2
	done
}

choose_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

dind_a() { env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_a_host" "$@"; }
dind_b() { env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_b_host" "$@"; }

# count_containers prints how many containers the given daemon (dind_a or
# dind_b) lists under name, passing any further arguments to `docker ps`. It
# returns non-zero when the daemon cannot be queried: an unreachable daemon
# prints nothing, and counted that would read as "no containers" — the very
# result several checks below want to see.
count_containers() {
	daemon=$1; name=$2; shift 2
	listed="$("$daemon" ps "$@" --filter "name=$name" --format '{{.Names}}')" || return 1
	printf '%s' "$listed" | grep -c . || true
}

server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }

node_state() {
	curl -fsS "$server_url/api/v1/nodes/$1" 2>/dev/null |
		python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("state",""))' 2>/dev/null || true
}
node_is() { [[ "$(node_state "$1")" == "$2" ]]; }

project_field() {
	curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null | python3 -c '
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
node_ref() { project_field "$1" 'p["status"].get("nodeRef","")'; }
assigned_to() { [[ "$(node_ref "$1")" == "$2" ]]; }
blocked_reason() { project_field "$1" 'cond("RecoveryBlocked").get("reason","")'; }
blocked_is() { [[ "$(blocked_reason "$1")" == "$2" ]]; }
takeover_required() { [[ "$(project_field "$1" 'cond("TakeoverRequired").get("status","")')" == "True" ]]; }

create_project() {
	curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d "$1" >/dev/null
}

mc() { docker exec "$minio_name" mc "$@"; }

latest_backup_id() {
	mc cat "local/$bucket/cara/v1/projects/default/$1/latest.json" 2>/dev/null |
		python3 -c 'import json,sys; print(json.load(sys.stdin).get("backupID",""))' 2>/dev/null || true
}

backup_published() { [[ -n "$(latest_backup_id "$1")" ]]; }

marker_field() {
	python3 -c '
import json, sys
try:
    m = json.load(open(sys.argv[1]))
except Exception:
    print(""); sys.exit(0)
print(m.get(sys.argv[2], ""))
' "$1" "$2"
}

dind_image="${DIND_IMAGE:-docker:27-dind}"
postgres_image="${POSTGRES_IMAGE:-postgres:16}"
# Chainguard, pinned by digest, as docker-compose.yaml uses: MinIO's own Quay
# repositories refuse anonymous pulls. The image ships mc alongside the server.
minio_image="${MINIO_IMAGE:-cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}"
bucket=cara-e2e

data_root_a="$test_root/agent-a-data"
data_root_b="$test_root/agent-b-data"
mkdir -p "$data_root_a" "$data_root_b" "$test_root/bin" \
	"$test_root/server-run" "$test_root/agent-a-run" "$test_root/agent-b-run"

# ── Infrastructure ───────────────────────────────────────────────────────────

log "starting isolated PostgreSQL ($postgres_name)"
docker run -d --name "$postgres_name" \
	-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=password -e POSTGRES_DB=caravanserai \
	-p 127.0.0.1::5432 "$postgres_image" >/dev/null
wait_until 60 "PostgreSQL readiness" docker exec "$postgres_name" pg_isready -U postgres
postgres_port="$(docker port "$postgres_name" 5432/tcp | head -1)"; postgres_port="${postgres_port##*:}"

log "starting isolated MinIO ($minio_name)"
docker run -d --name "$minio_name" \
	-e MINIO_ROOT_USER=caraadmin -e MINIO_ROOT_PASSWORD=caraadmin123 \
	-e MC_HOST_local=http://caraadmin:caraadmin123@127.0.0.1:9000 \
	-p 127.0.0.1::9000 "$minio_image" server /data >/dev/null
minio_port="$(docker port "$minio_name" 9000/tcp | head -1)"; minio_port="${minio_port##*:}"
minio_endpoint="http://127.0.0.1:$minio_port"
wait_until 90 "MinIO readiness" \
	bash -c "curl -fsS '$minio_endpoint/minio/health/live' >/dev/null 2>&1"
# The local alias comes from MC_HOST_local on the container, not from mc's
# config file: the image runs as a non-root user, and nothing guarantees it
# can write one.
mc mb --ignore-existing "local/$bucket" >/dev/null

for name in "$dind_a_name:$data_root_a" "$dind_b_name:$data_root_b"; do
	dind_name="${name%%:*}"; dind_data="${name##*:}"
	log "starting isolated Docker daemon ($dind_name)"
	docker run -d --privileged --name "$dind_name" \
		-e DOCKER_TLS_CERTDIR= \
		-p 127.0.0.1::2375 \
		-v "$dind_data:$dind_data" \
		"$dind_image" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null
done
dind_a_host="tcp://127.0.0.1:$(docker port "$dind_a_name" 2375/tcp | head -1 | sed 's/.*://')"
dind_b_host="tcp://127.0.0.1:$(docker port "$dind_b_name" 2375/tcp | head -1 | sed 's/.*://')"
wait_until 90 "Node A Docker readiness" bash -c "env -u DOCKER_TLS_VERIFY docker -H '$dind_a_host' info >/dev/null 2>&1"
wait_until 90 "Node B Docker readiness" bash -c "env -u DOCKER_TLS_VERIFY docker -H '$dind_b_host' info >/dev/null 2>&1"
dind_a pull nginx:alpine >/dev/null
dind_b pull nginx:alpine >/dev/null
dind_a pull busybox:latest >/dev/null

# Every byte assertion below depends on the Agent and the workload containers
# seeing the same directory. Prove it now rather than let it surface later as a
# confusing timeout.
log "checking that the Managed volume roots bind through to both DinD daemons"
for pair in "$dind_a_name:$data_root_a" "$dind_b_name:$data_root_b"; do
	dind_name="${pair%%:*}"; dind_data="${pair##*:}"
	sentinel="$dind_data/.shared-bind-sentinel"
	printf 'shared-%s\n' "$suffix" >"$sentinel"
	seen="$(docker exec "$dind_name" cat "$sentinel" 2>&1 || true)"
	[[ "$seen" == "shared-$suffix" ]] ||
		fail "shared bind path unavailable: $dind_name cannot read $sentinel (got: $seen); set E2E_SHARED_ROOT to a path that bind-mounts into DinD"
	rm -f "$sentinel"
done

log "building cara-server and cara-agent"
go build -o "$test_root/bin/cara-server" ./cmd/cara-server
go build -o "$test_root/bin/cara-agent" ./cmd/cara-agent

server_port="$(choose_port)"
server_url="http://127.0.0.1:$server_port"
(
	cd "$test_root/server-run"
	env HOST=127.0.0.1 PORT="$server_port" UID_ENFORCEMENT=true \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

start_agent() {
	agent_node=$1; agent_root=$2; agent_docker=$3; agent_log=$4; agent_dir=$5
	cat >"$agent_dir/config.yaml" <<EOF
data_root: "$agent_root"
EOF
	(
		cd "$agent_dir"
		env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
			SERVER_URL="$server_url" \
			NODE_NAME="$agent_node" \
			HEARTBEAT_INTERVAL=1s \
			DOCKER_HOST="$agent_docker" \
			AGENT_LISTEN_PORT=0 \
			PROXY_LISTEN_ADDR=127.0.0.1:0 \
			AGENT_ADVERTISE_IP=127.0.0.1 \
			UID_ENFORCEMENT=true \
			S3_ENDPOINT="$minio_endpoint" \
			S3_BUCKET="$bucket" \
			S3_ACCESS_KEY=caraadmin \
			S3_SECRET_KEY=caraadmin123 \
			"$test_root/bin/cara-agent"
	) >"$agent_log" 2>&1 &
}

# ── Fail fast on the preconditions the assertions depend on ──────────────────
#
# Each of these has silently produced a green run that proved nothing: an Agent
# with no object store skips every provenance check, a destination that already
# holds the Project's data is not the clean destination the first scenario
# claims to test, and a Project with no completed backup has nothing for the
# second Node to restore.

log "preflight: the object store both Agents will use must be reachable and hold the bucket"
mc ls "local/$bucket" >/dev/null 2>&1 ||
	fail "bucket $bucket is not reachable through $minio_endpoint; an Agent with no object store skips every provenance check and this run would prove nothing"

log "preflight: Node B must hold nothing for the clean-destination Project"
clean_project=failover-clean
stale_project=failover-stale
for root in "$data_root_b/volumes/default/$clean_project" "$data_root_b/restore-staging/default/$clean_project"; do
	[[ ! -e "$root" ]] || fail "Node B already holds $root; the clean-destination scenario would not be testing a clean destination"
done

# ── Projects on Node A ───────────────────────────────────────────────────────

log "starting Agent on node-a"
start_agent node-a "$data_root_a" "$dind_a_host" "$test_root/agent-a.log" "$test_root/agent-a-run"
agent_a_pid=$!
wait_until 60 "node-a readiness" node_is node-a Ready

for project in "$clean_project" "$stale_project"; do
	log "creating $project on node-a"
	create_project '{
		"apiVersion":"caravanserai/v1",
		"kind":"Project",
		"metadata":{"name":"'"$project"'","namespace":"default"},
		"spec":{
			"services":[{
				"name":"web","image":"nginx:alpine",
				"volumeMounts":[{"name":"content","mountPath":"/usr/share/nginx/html"}]
			}],
			"volumes":[{"name":"content","type":"Managed"}],
			"backup":{"interval":"15s","onMissing":"InitializeEmpty"}
		}
	}'
	wait_until 120 "$project Running on node-a" phase_is "$project" Running
	assigned_to "$project" node-a || fail "$project was not placed on node-a"
done

# Deterministic content: the failover is only proven if the exact bytes come
# back, so compare a checksum rather than "the file exists".
declare -A expected_sum
for project in "$clean_project" "$stale_project"; do
	payload="$data_root_a/volumes/default/$project/content/data/index.html"
	printf 'cara-87 failover payload for %s, run %s\n' "$project" "$suffix" >"$payload"
	expected_sum["$project"]="$(sha256sum "$payload" | cut -d' ' -f1)"
	log "$project payload checksum ${expected_sum[$project]}"
done

log "waiting for node-a to publish a complete backup of each Project"
for project in "$clean_project" "$stale_project"; do
	wait_until 120 "a backup generation for $project" backup_published "$project"
done
declare -A backup_before
for project in "$clean_project" "$stale_project"; do
	backup_before["$project"]="$(latest_backup_id "$project")"
	[[ -n "${backup_before[$project]}" ]] || fail "$project has no latest.json to restore from"
	log "$project latest generation ${backup_before[$project]}"
done

# The backup runs against a stopped Project, so wait for it to be serving again
# before the Agent is taken away — otherwise the loss is of a Project that was
# already mid-operation and the reassignment proves less.
for project in "$clean_project" "$stale_project"; do
	wait_until 90 "$project Running after its backup" phase_is "$project" Running
done

# ── Seed Node B ──────────────────────────────────────────────────────────────
#
# This is the regression fixture, not a compatibility case. It reproduces what
# an earlier placement leaves behind: bytes under the expected path and a v1
# marker that records nothing about which assignment produced them.

log "seeding node-b with stale bytes and a v1 marker for $stale_project"
stale_dir="$data_root_b/volumes/default/$stale_project/content/data"
mkdir -p "$stale_dir"
printf 'stale bytes from an earlier placement\n' >"$stale_dir/index.html"
stale_sum="$(sha256sum "$stale_dir/index.html" | cut -d' ' -f1)"
stale_marker="$data_root_b/volumes/default/$stale_project/.cara-restore.json"
cat >"$stale_marker" <<EOF
{
  "namespace": "default",
  "project": "$stale_project",
  "backupID": "20200101T000000Z-legacy",
  "restoredAt": "2020-01-01T00:00:00Z"
}
EOF
stale_marker_sum="$(sha256sum "$stale_marker" | cut -d' ' -f1)"

log "starting Agent on node-b"
start_agent node-b "$data_root_b" "$dind_b_host" "$test_root/agent-b.log" "$test_root/agent-b-run"
agent_b_pid=$!
wait_until 60 "node-b readiness" node_is node-b Ready

# ── The event under test ─────────────────────────────────────────────────────

log "stopping the Agent on node-a (its containers keep running, as in a real loss)"
kill "$agent_a_pid"
wait "$agent_a_pid" >/dev/null 2>&1 || true
agent_a_pid=""

log "waiting for the control plane to mark node-a NotReady (heartbeat timeout)"
wait_until 180 "node-a NotReady" node_is node-a NotReady

# ── The release gate: an unconfirmed Node keeps its Projects ────────────────
#
# node-a's containers are still running, and its Agent cannot confirm them
# stopped. ADR-0017 forbids handing the Projects to node-b on that basis: the
# Rescheduler must keep each assignment on node-a and mark it
# TakeoverRequired once the grace period is over, and node-b must start
# nothing. Before CARA-84 this was the point where both Projects moved.

log "waiting for both Projects to be marked TakeoverRequired (grace period applies)"
for project in "$clean_project" "$stale_project"; do
	wait_until 300 "$project TakeoverRequired" takeover_required "$project"
done

# The mark proves the grace period is over and the release was attempted.
# Hold for two more 30-second resync rounds to prove the refusal holds rather
# than being the first of several attempts.
log "holding for two more resync rounds"
sleep 65

for project in "$clean_project" "$stale_project"; do
	assigned_to "$project" node-a ||
		fail "$project left node-a without a release authorization (nodeRef now '$(node_ref "$project")')"
	takeover_required "$project" ||
		fail "$project lost TakeoverRequired while node-a is still NotReady"
	on_b="$(count_containers dind_b "$project-web" -a)" ||
		fail "$project: could not list node-b's containers, so cannot show it started none"
	[[ "$on_b" == "0" ]] ||
		fail "$project: node-b created a container while node-a may still run it ($on_b found)"
	on_a="$(count_containers dind_a "$project-web")" ||
		fail "$project: could not list node-a's containers"
	[[ "$on_a" != "0" ]] ||
		fail "$project: node-a's container is gone; the run no longer reproduces a Node that may still be serving"
	log "$project kept on node-a: TakeoverRequired, node-a container running, nothing on node-b"
done

# ── CARA-87 destination scenarios ────────────────────────────────────────────
#
# Both need the Projects reassigned to node-b, which now requires a takeover
# authorization ADR-0017 accepts for a Node that may still be running the
# workload: an audited operator override (CARA-89) or provider fencing. Neither
# exists yet. The scenarios are kept, unchanged in what they assert, and run
# only once such an authorization can be applied here; until then they are
# reported NOT RUN — never as passed.
#
# To enable them, implement authorize_takeover for node-a's assignments and
# set takeover_supported=1.

takeover_supported=0

authorize_takeover() {
	fail "authorize_takeover is not implemented: no takeover authorization exists yet"
}

# Scenario 1: unprovable destination data is refused, not adopted.
#
# This one runs first on purpose. It is the regression test — the clean
# destination already worked before CARA-87 — so when this script is run
# against an older commit to prove it exercises the defect, this is the
# assertion that has to be the one reporting.
scenario_stale_destination() {
	log "scenario: destination holding stale bytes and a v1 marker"
	wait_until 180 "$stale_project blocked on node-b" blocked_is "$stale_project" LegacyProvenance

	if phase_is "$stale_project" Running; then
		fail "$stale_project: a refused placement must not report Running"
	fi

	containers="$(count_containers dind_b "$stale_project-web" -a)" ||
		fail "$stale_project: could not list node-b's containers, so cannot show it started none"
	[[ "$containers" == "0" ]] ||
		fail "$stale_project: node-b started a container against data it could not prove ($containers found)"

	got_stale="$(sha256sum "$stale_dir/index.html" | cut -d' ' -f1)"
	[[ "$got_stale" == "$stale_sum" ]] ||
		fail "$stale_project: refusing to use the bytes is not a licence to change them"

	got_marker="$(sha256sum "$stale_marker" | cut -d' ' -f1)"
	[[ "$got_marker" == "$stale_marker_sum" ]] ||
		fail "$stale_project: the v1 marker was rewritten; unprovable data must never be stamped as owned"

	after="$(latest_backup_id "$stale_project")"
	[[ "$after" == "${backup_before[$stale_project]}" ]] ||
		fail "$stale_project: the authoritative generation moved while the Project was blocked (was ${backup_before[$stale_project]}, now $after)"

	log "$stale_project blocked with reason $(blocked_reason "$stale_project"), generation unchanged at $after"
}

# Scenario 2: a clean destination restores the selected generation.
scenario_clean_destination() {
	log "scenario: clean destination"
	wait_until 180 "$clean_project Running on node-b" phase_is "$clean_project" Running

	restored="$data_root_b/volumes/default/$clean_project/content/data/index.html"
	[[ -f "$restored" ]] || fail "$clean_project: node-b has no restored payload"
	got_sum="$(sha256sum "$restored" | cut -d' ' -f1)"
	[[ "$got_sum" == "${expected_sum[$clean_project]}" ]] ||
		fail "$clean_project: restored bytes differ (want ${expected_sum[$clean_project]}, got $got_sum)"

	clean_marker="$data_root_b/volumes/default/$clean_project/.cara-restore.json"
	[[ -f "$clean_marker" ]] || fail "$clean_project: node-b wrote no provenance marker"
	[[ "$(marker_field "$clean_marker" version)" == "2" ]] || fail "$clean_project: marker is not v2"
	[[ "$(marker_field "$clean_marker" nodeName)" == "node-b" ]] || fail "$clean_project: marker does not name node-b"
	restored_from="$(marker_field "$clean_marker" initializedFromBackupID)"
	[[ -n "$restored_from" ]] || fail "$clean_project: marker records no source generation"

	log "$clean_project restored generation $restored_from, checksum $got_sum"
	log "$clean_project source generation was ${backup_before[$clean_project]}"
}

if (( takeover_supported )); then
	authorize_takeover
	log "waiting for both Projects to be reassigned to node-b"
	for project in "$clean_project" "$stale_project"; do
		wait_until 300 "$project assigned to node-b" assigned_to "$project" node-b
	done
	scenario_stale_destination
	scenario_clean_destination
else
	log "NOT RUN: CARA-87 destination scenarios (stale, clean) need an authorized takeover, which does not exist yet"
fi

# ── Summary ──────────────────────────────────────────────────────────────────

cat <<EOF

[failover-e2e] result
  agent loss            node-a stopped, NotReady
  release gate          both Projects kept on node-a with TakeoverRequired
                        node-a containers still running, node-b started none
EOF

if (( takeover_supported )); then
	cat <<EOF
  clean destination     $clean_project
                        source generation   ${backup_before[$clean_project]}
                        restored generation $restored_from
                        checksum            $got_sum (matches node-a)
  stale destination     $stale_project
                        blocked reason      $(blocked_reason "$stale_project")
                        containers started  0
                        stale bytes         unchanged
                        v1 marker           unchanged
                        latest generation   $after (unchanged)
EOF
	log "PASS"
else
	cat <<EOF
  clean destination     NOT RUN (needs an authorized takeover)
  stale destination     NOT RUN (needs an authorized takeover)
EOF
	log "PASS (release gate only; CARA-87 destination scenarios NOT RUN)"
fi
