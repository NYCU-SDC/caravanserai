# shellcheck shell=bash
# vmtest-proves: The release packages install disabled, restart crashed services, keep settings and the Node identity across reinstall and remove (CARA-143)
# vmtest-needs:
# vmtest-server-env:
# vmtest-power-offs: 0
# vmtest-minutes: 6
# vmtest-enrol: auto
#
# run.sh has installed the packages built from the commit (lib/deploy.sh) and
# enrolled both agents. Checks that read which credential the agent used run on
# node-a: join-tokens, if it ran first, leaves node-b on AGENT_TOKEN_FILE.
# No power-off.

a=$(vm_for_node node-a)
b=$(vm_for_node node-b)

main_pid() { vm "$1" "systemctl show -p MainPID --value $2"; }
# restarted <vm> <service> <old pid> succeeds once the service runs as a new process.
restarted() {
	local pid
	pid=$(main_pid "$1" "$2")
	[[ "$pid" != 0 && "$pid" != "$3" && "$(vm "$1" "systemctl is-active $2")" == active ]]
}
server_healthy() { srv "curl -fsS http://127.0.0.1:8080/api/healthz" >/dev/null 2>&1; }

log "--- P1: what a fresh install leaves (recorded by deploy.sh before anything was enabled)"
state="$VMTEST_OUT/logs/install-state.txt"
expect "P1 cara-server after install" "disabled inactive 600 root" "$(grep '^cara-server ' "$state" | cut -d' ' -f2-)"
for v in "$a" "$b"; do
	expect "P1 cara-agent on $v after install" "disabled inactive" "$(grep "^$v " "$state" | cut -d' ' -f2-)"
done

log "--- P2: one version everywhere"
ver=$(srv "dpkg-query -W -f '\${Version}' cara-server")
observe "P2 package version" "$ver"
expect "P2 caractl reports it" "$ver" "$(srv "caractl version" | awk '{print $2}')"
expect "P2 cara-agent package on $a" "$ver" "$(vm "$a" "dpkg-query -W -f '\${Version}' cara-agent")"
expect "P2 cara-agent logs it at start" yes \
	"$(vm "$a" "sudo journalctl -u cara-agent --no-pager -o cat" | grep -qF "$ver" && echo yes || echo no)"
expect "P2 cara-server logs it at start" yes \
	"$(srv "sudo journalctl -u cara-server --no-pager -o cat" | grep -qF "$ver" && echo yes || echo no)"

log "--- P3: cara-agent killed with SIGKILL"
pid=$(main_pid "$b" cara-agent)
started=$(date +%s)
vm "$b" "sudo kill -9 $pid"
r0=$SECONDS
if wait_for 60 "cara-agent restarted" restarted "$b" cara-agent "$pid"; then p3=yes; else p3=no; fi
expect "P3 systemd restarts cara-agent" yes "$p3"
[[ $p3 == yes ]] && observe "P3 seconds until the new process ran" "$((SECONDS - r0))"
if wait_for 90 "node-b heartbeating" heartbeat_after node-b "$started"; then h=yes; else h=no; fi
expect "P3 node-b heartbeats again" yes "$h"

log "--- P4: cara-server killed with SIGKILL"
pid=$(main_pid "$VMTEST_SERVER" cara-server)
srv "sudo kill -9 $pid"
r0=$SECONDS
if wait_for 60 "cara-server restarted" restarted "$VMTEST_SERVER" cara-server "$pid"; then p4=yes; else p4=no; fi
expect "P4 systemd restarts cara-server" yes "$p4"
if wait_for 60 "cara-server healthy" server_healthy; then
	observe "P4 seconds until healthz answered" "$((SECONDS - r0))"
	expect "P4 cara-server healthy again" yes yes
else
	expect "P4 cara-server healthy again" yes no
fi

log "--- P5: reinstall cara-agent on node-a (the upgrade path of the maintainer scripts, same version)"
pid=$(main_pid "$a" cara-agent)
t=$(vm_now "$a")
started=$(date +%s)
install_package "$a" "$bin" cara-agent --reinstall -o Dpkg::Options::=--force-confold
if wait_for 60 "cara-agent restarted" restarted "$a" cara-agent "$pid"; then p5=yes; else p5=no; fi
expect "P5 a running agent is restarted" yes "$p5"
expect "P5 edited agent.env kept" 1 "$(vm "$a" "sudo grep -c '^NODE_NAME=node-a$' /etc/cara/agent.env")"
if wait_for 90 "node-a heartbeating" heartbeat_after node-a "$started"; then h=yes; else h=no; fi
expect "P5 node-a heartbeats again" yes "$h"
expect "P5 kept Node token used" 1 "$(agent_log_count "$a" "$t" 'Using the Node token kept')"

log "--- P6: remove cara-agent on node-a, then install it again"
vm "$a" "sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y -qq cara-agent >/dev/null"
expect "P6 service after remove" "inactive" "$(vm "$a" 'systemctl is-active cara-agent 2>&1')"
expect "P6 unit file gone" "" "$(vm "$a" 'systemctl list-unit-files cara-agent.service --no-legend 2>&1')"
expect "P6 identity kept" yes "$(vm "$a" 'sudo test -s /var/lib/cara/identity/node-token && echo yes || echo no')"
expect "P6 agent.env kept" yes "$(vm "$a" 'sudo test -f /etc/cara/agent.env && echo yes || echo no')"
install_package "$a" "$bin" cara-agent -o Dpkg::Options::=--force-confold
expect "P6 after installing again" "disabled inactive" "$(install_state "$a" cara-agent)"
t=$(vm_now "$a")
started=$(date +%s)
vm "$a" "sudo systemctl enable --now cara-agent >/dev/null 2>&1"
if wait_for 90 "node-a heartbeating" heartbeat_after node-a "$started"; then h=yes; else h=no; fi
expect "P6 node-a back with its kept identity" yes "$h"
expect "P6 kept Node token used" 1 "$(agent_log_count "$a" "$t" 'Using the Node token kept')"
expect "P6 no new enrolment" 0 "$(agent_log_count "$a" "$t" 'Node enrolled')"

log "--- P7: break-glass token survives the server package's reinstall"
sum=$(srv "sudo sha256sum /etc/cara/break-glass-token" | cut -c1-12)
install_package "$VMTEST_SERVER" "$bin" cara-server --reinstall -o Dpkg::Options::=--force-confold
expect "P7 token unchanged" "$sum" "$(srv "sudo sha256sum /etc/cara/break-glass-token" | cut -c1-12)"
wait_for 60 "cara-server healthy" server_healthy >/dev/null
expect "P7 API accepts the token after reinstall" yes "$(api /api/v1/nodes >/dev/null 2>&1 && echo yes || echo no)"
