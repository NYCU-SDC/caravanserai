# shellcheck shell=bash
# vmtest-proves: caractl logs, port-forward and probe reach agents on other hosts only through cara-server, the agent API refuses unsigned requests from every host, and signing survives agent and server restarts, a server that starts after the agent, and clock skew within the tolerance (CARA-139)
# vmtest-needs:
# vmtest-server-env:
# vmtest-power-offs: 0
# vmtest-minutes: 15
# vmtest-enrol: auto
#
# scripts/e2e/agent-access.sh covers the same API on one machine. What only
# separate hosts show: requests crossing a real network, systemd restarts, the
# order in which the server and an agent come up, and two clocks that differ.
# An assertion lives 30 s and the agent accepts 30 s of skew either way
# (internal/agentauth), so a Node whose clock is 20 s off must still work and
# one 120 s off is refused by design.

P=access-demo
AGENT_PORT=9090

other() { [[ $1 == node-a ]] && echo node-b || echo node-a; }
node_ip() { vm "$(vm_for_node "$1")" "hostname -I" | awk '{print $1}'; }
# direct <from-vm> <node> <path> prints the status the node's agent API gives
# an unsigned request sent from another VM.
direct() { vm "$1" "curl -s -o /dev/null -w '%{http_code}' --max-time 10 http://$(node_ip "$2"):$AGENT_PORT$3"; }
# forged <from-vm> <node> <path> sends a request carrying a malformed
# assertion: 503 while the agent has no key yet, 401 once it has one.
forged() { vm "$1" "curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H 'X-Cara-Server-Assertion: x.y' http://$(node_ip "$2"):$AGENT_PORT$3"; }
# on_server <name> <script> runs a shell script as root on the server, keeps
# its stdout and stderr in /tmp/<name>.out there, and prints that file: the
# output does not depend on how the provider's ssh carries stderr, and the
# script needs no extra layer of quoting.
on_server() {
	printf '%s\n' "$2" | srv "cat > /tmp/$1.sh"
	srv "sudo sh /tmp/$1.sh > /tmp/$1.out 2>&1; cat /tmp/$1.out"
}
logs_ok() { ctl "logs $P/talker" 2>/dev/null | grep -q hello-from-talker; }
probe_out() { ctl "node probe $1" 2>&1 | tr '\n' ' ' | head -c 300; }
probe_ok() { probe_out "$1" | grep -q OK; }
signing_key() { api /api/v1/agent-signing-key | jq -r .publicKey; }
server_ready() { srv "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/api/healthz" | grep -q 200; }
shift_clock() { vm "$(vm_for_node "$1")" "sudo sh -c 'timedatectl set-ntp false; date -s @\$((\$(date +%s) + $2)) >/dev/null'"; }
restore_clock() { vm "$(vm_for_node "$1")" "sudo sh -c 'date -s @\$((\$(date +%s) - $2)) >/dev/null; timedatectl set-ntp true'"; }

log "--- R0: clocks and a Project with a talker and a web service"
for v in "$VMTEST_SERVER" "${VMTEST_NODE_VMS[@]}"; do
	observe "R0 $v NTP synchronized" "$(vm "$v" "timedatectl show -p NTPSynchronized --value")"
done
ctl "delete project $P --force" >/dev/null 2>&1 || true
printf '%s\n' \
	'apiVersion: caravanserai/v1' 'kind: Project' "metadata:" "  name: $P" 'spec:' '  size: Small' '  services:' \
	'    - name: web' '      image: nginx:1.27-alpine' \
	'    - name: talker' '      image: busybox:latest' \
	'      command: ["sh", "-c", "echo hello-from-talker; while true; do sleep 1; if [ -f /tmp/more ]; then echo later-line; rm /tmp/more; fi; done"]' |
	srv "cat > /tmp/access.yaml"
ctl "apply -f /tmp/access.yaml" >/dev/null
running() { [[ "$(field "$P" .status.phase)" == Running ]]; }
if wait_for 240 "$P Running" running; then r0=Running; else r0="$(field "$P" .status.phase)"; fi
expect "R0 project reaches Running" Running "$r0"
X=$(field "$P" .status.nodeRef)
Y=$(other "$X")
XV=$(vm_for_node "$X")
YV=$(vm_for_node "$Y")
observe "R0 placed on" "$X"
expect "R0 $X reports agent port $AGENT_PORT" "$AGENT_PORT" "$(api "/api/v1/nodes/$X" | jq -r '.status.network.agentPort // 0')"

log "--- R1: logs, follow, port-forward and probe from the server host"
expect "R1 caractl logs returns the talker's output" yes "$(logs_ok && echo yes || echo no)"
srv "sudo sh -c 'rm -f /tmp/follow.out; (CARA_TOKEN=\$(cat /etc/cara/break-glass-token) timeout 40 caractl --server http://127.0.0.1:8080 logs $P/talker --follow > /tmp/follow.out 2>&1 &)'"
follow_has() { srv "grep -q '$1' /tmp/follow.out"; }
wait_for 20 "the followed stream opened" follow_has hello-from-talker || true
vm "$XV" "sudo docker exec $P-talker touch /tmp/more"
if wait_for 20 "a line written after the stream opened" follow_has later-line; then r1f=yes; else r1f=no; fi
expect "R1 logs --follow delivers a line written later" yes "$r1f"
srv "sudo sh -c 'rm -f /tmp/forward.out; (CARA_TOKEN=\$(cat /etc/cara/break-glass-token) timeout 40 caractl --server http://127.0.0.1:8080 port-forward $P/web 18080:80 > /tmp/forward.out 2>&1 &)'"
fwd_ready() { srv "grep -q Forwarding /tmp/forward.out"; }
wait_for 20 "port-forward listening" fwd_ready || true
page() { srv "curl -s --max-time 10 http://127.0.0.1:18080/" | grep -q 'Welcome to nginx' && echo yes || echo no; }
expect "R1 port-forward first connection serves nginx" yes "$(page)"
expect "R1 port-forward second connection serves nginx" yes "$(page)"
expect "R1 node probe $X" yes "$(probe_ok "$X" && echo yes || echo no)"
expect "R1 node probe $Y" yes "$(probe_ok "$Y" && echo yes || echo no)"

log "--- R2: unsigned requests to $X's agent API from every host"
for from in "$VMTEST_SERVER" "$YV" "$XV"; do
	expect "R2 from $from: logs" 401 "$(direct "$from" "$X" "/api/v1/logs/$P/talker")"
	expect "R2 from $from: forward" 401 "$(direct "$from" "$X" "/api/v1/forward/$P/web/80")"
	expect "R2 from $from: probe" 401 "$(direct "$from" "$X" /api/v1/probe)"
	expect "R2 from $from: /healthz" 200 "$(direct "$from" "$X" /healthz)"
done
notoken=$(on_server notoken "caractl --server http://127.0.0.1:8080 logs $P/talker")
observe "R2 caractl without a token says" "$(head -c 200 <<<"$notoken" | tr '\n' ' ')"
expect "R2 caractl without a token is refused with 401" yes "$(grep -qiE '401|unauthorized' <<<"$notoken" && echo yes || echo no)"
nodeaddr=$(on_server nodeaddr "CARA_TOKEN=\$(cat /etc/cara/break-glass-token) caractl --server http://127.0.0.1:8080 logs $P/talker --node $(node_ip "$X"):$AGENT_PORT")
observe "R2 caractl --node host:port says" "$(head -c 200 <<<"$nodeaddr" | tr '\n' ' ')"
expect "R2 caractl --node host:port is refused before sending" yes "$(grep -q 'agents accept no one else' <<<"$nodeaddr" && echo yes || echo no)"
observe "R2 refusals logged by $X's agent" "$(agent_log_count "$XV" "$(date -u -d '-10 min' '+%Y-%m-%d %H:%M:%S')" 'Refused a request without a valid server assertion')"

log "--- R3: restart $X's agent"
t0=$(vm_now "$XV")
vm "$XV" "sudo systemctl restart cara-agent"
r0=$SECONDS
if wait_for 120 "logs work after the agent restart" logs_ok; then r3=yes; else r3=no; fi
expect "R3 logs work after the agent restart" yes "$r3"
[[ $r3 == yes ]] && observe "R3 seconds from restart to working logs" "$((SECONDS - r0))"
expect "R3 agent fetched the key again" 1 "$(agent_log_count "$XV" "$t0" "Fetched the server's public key")"

log "--- R4: restart cara-server; the key must not change"
k0=$(signing_key)
srv "sudo systemctl restart cara-server"
wait_for 120 "cara-server ready" server_ready || true
expect "R4 signing key unchanged across a server restart" "$k0" "$(signing_key)"
if wait_for 60 "logs work after the server restart" logs_ok; then r4=yes; else r4=no; fi
expect "R4 logs work after the server restart" yes "$r4"

log "--- R5: $X's agent starts while cara-server is down"
srv "sudo systemctl stop cara-server"
t0=$(vm_now "$XV")
vm "$XV" "sudo systemctl restart cara-agent"
sleep 15
expect "R5 unsigned request before the key arrives" 401 "$(direct "$YV" "$X" /api/v1/probe)"
expect "R5 request with an assertion before the key arrives" 503 "$(forged "$YV" "$X" /api/v1/probe)"
expect "R5 /healthz before the key arrives" 200 "$(direct "$YV" "$X" /healthz)"
observe "R5 'cannot fetch the key yet' warnings in 15 s" "$(agent_log_count "$XV" "$t0" "Cannot fetch the server's public key yet")"
srv "sudo systemctl start cara-server"
wait_for 120 "cara-server ready" server_ready || true
r0=$SECONDS
if wait_for 120 "logs work once the server is back" logs_ok; then r5=yes; else r5=no; fi
expect "R5 request with a malformed assertion once the key is back" 401 "$(forged "$YV" "$X" /api/v1/probe)"
expect "R5 logs work once the server is back" yes "$r5"
[[ $r5 == yes ]] && observe "R5 seconds from server ready to working logs" "$((SECONDS - r0))"

log "--- R6: $Y's clock 20 s ahead, then 120 s ahead"
shift_clock "$Y" 20
expect "R6 probe $Y with its clock 20 s ahead" yes "$(probe_ok "$Y" && echo yes || echo no)"
restore_clock "$Y" 20
shift_clock "$Y" 120
t0=$(vm_now "$YV")
skewed=$(probe_out "$Y")
expect "R6 probe $Y with its clock 120 s ahead is refused" no "$(grep -q OK <<<"$skewed" && echo yes || echo no)"
observe "R6 what caractl shows at 120 s skew" "$skewed"
observe "R6 refusals logged by $Y's agent at 120 s skew" "$(agent_log_since "$YV" "$t0" | grep 'Refused a request' | grep -o 'error.*' | head -1)"
restore_clock "$Y" 120
expect "R6 probe $Y after its clock is restored" yes "$(probe_ok "$Y" && echo yes || echo no)"
observe "R6 $YV NTP synchronized after restore" "$(vm "$YV" "timedatectl show -p NTPSynchronized --value")"

log "--- R7: delete the Project"
ctl "delete project $P" >/dev/null
gone_p() { [[ -z "$(project "$P")" ]]; }
if wait_for 180 "$P gone" gone_p; then r7=yes; else r7=no; fi
expect "R7 project deleted" yes "$r7"
