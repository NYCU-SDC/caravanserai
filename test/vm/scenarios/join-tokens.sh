# shellcheck shell=bash
# vmtest-proves: Agents enrol with one-time join tokens and keep their Node token across restarts, reboots, revocation and re-enrolment (CARA-138)
# vmtest-needs:
# vmtest-server-env:
# vmtest-power-offs: 0
# vmtest-minutes: 12
# vmtest-enrol: manual
#
# Ported from the 2026-11-06 manual trial (test-records/2026-11-06-cara138).
# Needs a commit with CARA-138 (#108). One graceful reboot, no power-off.

jt_restart() { # <node>: restart the agent and wait for a heartbeat after it
	local v started
	v="$(vm_for_node "$1")"
	started=$(date +%s)
	vm "$v" "sudo systemctl restart cara-agent"
	wait_for 90 "$1 heartbeating after restart" heartbeat_after "$1" "$started"
}

a=$(vm_for_node node-a)
b=$(vm_for_node node-b)

log "--- A: first enrolment with a join token"
for node in node-a node-b; do
	v=$(vm_for_node "$node")
	t=$(vm_now "$v")
	enrol_agent "$node"
	expect "A $node enrolled" yes "$( (($(agent_log_count "$v" "$t" 'Node enrolled') == 1)) && echo yes || echo no)"
	expect "A $node identity permissions" "700 root;600 root;600 root" \
		"$(vm "$v" "sudo stat -c '%a %U' /var/lib/cara/identity /var/lib/cara/identity/node-token /var/lib/cara/identity/joined-with.sha256" | paste -sd ';')"
	expect "A $node keeps a Node token, not the join token" yes \
		"$([[ "$(vm "$v" 'sudo cat /var/lib/cara/identity/node-token')" != "$(vm "$v" 'sudo cat /etc/cara/join-token')" ]] && echo yes || echo no)"
done

log "--- B: systemctl restart uses the kept token"
t=$(vm_now "$a")
jt_restart node-a
expect "B kept token used" 1 "$(agent_log_count "$a" "$t" 'Using the Node token kept')"
expect "B no new enrolment" 0 "$(agent_log_count "$a" "$t" 'Enrolling')"

log "--- C: join token file deleted after enrolment, JOIN_TOKEN_FILE still set"
vm "$a" "sudo rm -f /etc/cara/join-token"
t=$(vm_now "$a")
if jt_restart node-a; then c=runs; else c="does not run"; fi
expect "C agent runs without the join token file" runs "$c"
expect "C kept token used" 1 "$(agent_log_count "$a" "$t" 'Using the Node token kept')"

log "--- D: graceful reboot of node-b"
started=$(date +%s)
vm "$b" "sudo systemctl reboot" >/dev/null 2>&1
sleep 15
reachable() { vm "$b" true >/dev/null 2>&1; }
wait_for 300 "node-b reachable" reachable
if wait_for 120 "node-b heartbeating after reboot" heartbeat_after node-b "$started"; then d=yes; else d=no; fi
expect "D node-b back after reboot" yes "$d"
expect "D kept token used since boot" 1 "$(vm "$b" "sudo journalctl -u cara-agent -b --no-pager -o cat | grep -c 'Using the Node token kept'")"

log "--- E: a join token for another Node placed on node-a"
put_join_token "$a" "$(ctl 'node token create node-x' 2>/dev/null)"
t=$(vm_now "$a")
jt_restart node-a
expect "E refused join keeps the held token" 1 "$(agent_log_count "$a" "$t" 'refused; keeping the Node token')"

log "--- F: caractl node revoke node-a"
t=$(vm_now "$a")
observe "F caractl output" "$(ctl 'node revoke node-a')"
refused() { (($(agent_log_count "$a" "$t" "refused this Node's token") >= 1)); }
r0=$SECONDS
if wait_for 120 "node-a logs the refusal" refused; then
	observe "F seconds from revoke to the agent's log" "$((SECONDS - r0))"
fi
sleep 70
expect "F refusal logged once over 70 s more" 1 "$(agent_log_count "$a" "$t" "refused this Node's token")"
expect "F agent keeps running" active "$(vm "$a" 'systemctl is-active cara-agent')"

log "--- G: re-enrol node-a with a new join token"
put_join_token "$a" "$(ctl 'node token create node-a' 2>/dev/null)"
t=$(vm_now "$a")
jt_restart node-a
expect "G re-enrolment started" 1 "$(agent_log_count "$a" "$t" 'enrolling this Node again')"
expect "G Node enrolled" 1 "$(agent_log_count "$a" "$t" 'Node enrolled')"

log "--- H: no credential at all"
vm "$b" "sudo sh -c 'systemctl stop cara-agent; mv /var/lib/cara/identity /var/lib/cara/identity.bak; rm -f /etc/cara/join-token'"
t=$(vm_now "$b")
vm "$b" "sudo systemctl start cara-agent"
sleep 8
expect "H agent refuses to start" no "$([[ "$(vm "$b" 'systemctl is-active cara-agent')" == active ]] && echo yes || echo no)"
expect "H error names the missing file" yes \
	"$(agent_log_since "$b" "$t" | grep -q 'join-token does not exist' && echo yes || echo no)"
vm "$b" "sudo sh -c 'systemctl stop cara-agent; mv /var/lib/cara/identity.bak /var/lib/cara/identity'"

log "--- I: AGENT_TOKEN_FILE takes precedence"
printf '%s\n' "$(ctl 'token create node node-b' 2>/dev/null)" |
	vm "$b" "sudo sh -c 'umask 077; cat > /etc/cara/node-token; echo AGENT_TOKEN_FILE=/etc/cara/node-token >> /etc/cara/agent.env'"
t=$(vm_now "$b")
jt_restart node-b
expect "I kept token not used" 0 "$(agent_log_count "$b" "$t" 'Using the Node token kept')"
