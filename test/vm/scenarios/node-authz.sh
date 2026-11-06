# shellcheck shell=bash
# vmtest-proves: Agents run, move and clean up Projects under Node authorization, and a Node token reaches only its own Projects and Secrets (CARA-140)
# vmtest-needs:
# vmtest-server-env: GRACE_EXPIRED_RELEASE=true
# vmtest-power-offs: 0
# vmtest-minutes: 12
# vmtest-enrol: auto
#
# The Project moves by stopping its Node's agent (systemctl stop, not a power
# cut): after 90 s without a heartbeat the Node is NotReady, after the 3 min
# grace period the Project is released and placed on the other Node. The old
# agent, started again, must clean up through its cleanup obligation, which is
# the one path where a Node reads a Project assigned elsewhere.
#
# Run it first or alone: it reads each agent's kept Node token, which
# join-tokens scenario I replaces on node-b with AGENT_TOKEN_FILE.

P=authz-demo
SECRET=authz-secret

node_token() { vm "$(vm_for_node "$1")" "sudo cat /var/lib/cara/identity/node-token"; }
# as_node <node> <method> <path> prints the HTTP status cara-server gives the
# node's own token, sent from the node's own VM.
as_node() {
	local token
	token="$(node_token "$1")"
	vm "$(vm_for_node "$1")" "curl -s -o /dev/null -w '%{http_code}' -X $2 -H 'Authorization: Bearer $token' -H 'Content-Type: application/json' -d '{}' http://$VMTEST_SERVER_IP:8080$3"
}
forbidden_logs() { vm "$(vm_for_node "$1")" "sudo journalctl -u cara-agent --no-pager -o cat | grep -c 'forbade a request' || true"; }
phase_on() { [[ "$(field "$P" .status.phase)" == Running && "$(field "$P" .status.nodeRef)" == "$1" ]]; }
other() { [[ $1 == node-a ]] && echo node-b || echo node-a; }
containers_on() { vm "$(vm_for_node "$1")" "sudo docker ps -aq --filter label=cara.project=$P | wc -l"; }
obligation_resolved() {
	[[ "$(field "$P" "[.status.recovery.cleanupObligations[]? | select(.assignment.nodeName==\"$1\") | (.resourcesRemoved != null)][0] // false")" == true ]]
}

log "--- Q1: a Project with a Secret runs under authorization"
ctl "delete project $P --force" >/dev/null 2>&1 || true
printf 'apiVersion: caravanserai/v1\nkind: Secret\nmetadata:\n  name: %s\nspec:\n  data:\n    - key: greeting\n      value: hello\n' "$SECRET" |
	srv "cat > /tmp/secret.yaml"
printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  services:\n    - name: web\n      image: nginx:alpine\n      env:\n        - name: GREETING\n          valueFrom:\n            secretKeyRef:\n              name: %s\n              key: greeting\n' "$P" "$SECRET" |
	srv "cat > /tmp/project.yaml"
ctl "apply -f /tmp/secret.yaml" >/dev/null
ctl "apply -f /tmp/project.yaml" >/dev/null
running_anywhere() { [[ "$(field "$P" .status.phase)" == Running ]]; }
if wait_for 240 "$P Running" running_anywhere; then q1=Running; else q1="$(field "$P" .status.phase)"; fi
expect "Q1 project with a Secret reaches Running" Running "$q1"
X=$(field "$P" .status.nodeRef)
Y=$(other "$X")
observe "Q1 placed on" "$X"
expect "Q1 the container got the Secret's value" hello \
	"$(vm "$(vm_for_node "$X")" "sudo docker ps -q --filter label=cara.project=$P | head -1 | xargs -r sudo docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^GREETING=//p'")"

log "--- Q2: what each Node's own token reaches ($X holds $P, $Y does not)"
expect "Q2 $X reads the Secret" 200 "$(as_node "$X" GET "/api/v1/secrets/$SECRET")"
expect "Q2 $Y reads the Secret" 403 "$(as_node "$Y" GET "/api/v1/secrets/$SECRET")"
expect "Q2 $X reads the Project" 200 "$(as_node "$X" GET "/api/v1/projects/$P")"
expect "Q2 $Y reads the Project" 404 "$(as_node "$Y" GET "/api/v1/projects/$P")"
expect "Q2 $Y creates a Project" 403 "$(as_node "$Y" POST /api/v1/projects)"
expect "Q2 $Y deletes the Project" 403 "$(as_node "$Y" DELETE "/api/v1/projects/$P")"
expect "Q2 $Y heartbeats as $X" 403 "$(as_node "$Y" POST "/api/v1/nodes/$X/heartbeat")"
expect "Q2 $Y lists tokens" 403 "$(as_node "$Y" GET /api/v1/tokens)"
expect "Q2 $X agent forbidden so far" 0 "$(forbidden_logs "$X")"
expect "Q2 $Y agent forbidden so far" 0 "$(forbidden_logs "$Y")"

log "--- Q3: stop $X's agent; the Project moves to $Y"
vm "$(vm_for_node "$X")" "sudo systemctl stop cara-agent"
r0=$SECONDS
if wait_for 600 "$P Running on $Y" phase_on "$Y"; then q3=yes; else q3=no; fi
expect "Q3 project Running on $Y" yes "$q3"
[[ $q3 == yes ]] && observe "Q3 seconds from stopping $X's agent to Running on $Y" "$((SECONDS - r0))"
expect "Q3 $Y reads the Secret now" 200 "$(as_node "$Y" GET "/api/v1/secrets/$SECRET")"
expect "Q3 $X reads the Secret now" 403 "$(as_node "$X" GET "/api/v1/secrets/$SECRET")"
expect "Q3 $X still reads the Project (owes cleanup)" 200 "$(as_node "$X" GET "/api/v1/projects/$P")"
observe "Q3 containers of $P still on $X" "$(containers_on "$X")"

log "--- Q4: $X's agent comes back and cleans up through its obligation"
vm "$(vm_for_node "$X")" "sudo systemctl start cara-agent"
r0=$SECONDS
if wait_for 300 "$X reported ResourcesRemoved" obligation_resolved "$X"; then q4=yes; else q4=no; fi
expect "Q4 $X resolved its cleanup obligation" yes "$q4"
[[ $q4 == yes ]] && observe "Q4 seconds from restart to ResourcesRemoved" "$((SECONDS - r0))"
expect "Q4 containers of $P left on $X" 0 "$(containers_on "$X")"
expect "Q4 $X reads the Project once cleanup is done" 404 "$(as_node "$X" GET "/api/v1/projects/$P")"
expect "Q4 project still Running on $Y" yes "$(phase_on "$Y" && echo yes || echo no)"

log "--- Q5: delete the Project"
ctl "delete project $P" >/dev/null
gone_p() { [[ -z "$(project "$P")" ]]; }
if wait_for 180 "$P gone" gone_p; then q5=yes; else q5=no; fi
expect "Q5 project deleted through the agent" yes "$q5"
expect "Q5 containers of $P left on $Y" 0 "$(containers_on "$Y")"
expect "Q5 $X agent forbidden in the whole run" 0 "$(forbidden_logs "$X")"
expect "Q5 $Y agent forbidden in the whole run" 0 "$(forbidden_logs "$Y")"
