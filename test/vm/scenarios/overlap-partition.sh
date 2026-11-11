# shellcheck shell=bash
# vmtest-proves: While a Node is cut off from the server, a Project with spec.overlap Forbid stays assigned to it and keeps running there, while an Allow Project on the same Node is moved after the grace period; once the Node is back, the moved Project's old copy is removed (ADR-0036)
# vmtest-needs:
# vmtest-server-env: GRACE_EXPIRED_RELEASE=true RUNNING_GRACE_PERIOD=1m
# vmtest-power-offs: 0
# vmtest-minutes: 20
# vmtest-enrol: auto

A=ov-allow
F=ov-forbid

# manifest <name> <Forbid|-> writes and applies a stateless Project. Small, so
# both fit on one 2-vCPU Node (1500m allocatable): two Medium Projects do not,
# and the second would be placed on node-b before the partition starts.
manifest() {
	{
		printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  size: Small\n' "$1"
		[[ $2 == Forbid ]] && printf '  overlap: Forbid\n'
		printf '  services:\n    - name: app\n      image: busybox:1.36\n'
		printf '      command: ["sh", "-c", "exec sleep 86400"]\n'
	} | srv "cat > /tmp/$1.yaml"
	ctl "apply -f /tmp/$1.yaml" >/dev/null
}

node_of() { field "$1" .status.nodeRef; }
phase_of() { field "$1" .status.phase; }
running_on() { [[ "$(phase_of "$1")" == Running && "$(node_of "$1")" == "$2" ]]; }
# copies <vm> <project>: running containers of the Project on that VM.
copies() { vm "$1" "sudo docker ps -q --filter label=cara.project=$2 --filter status=running | wc -l"; }
node_state() { api "/api/v1/nodes/$1" 2>/dev/null | jq -r '.status.state // ""'; }
node_is() { [[ "$(node_state "$1")" == "$2" ]]; }
no_copies() { [[ "$(copies "$1" "$2")" == 0 ]]; }

va=$(vm_for_node node-a)
vb=$(vm_for_node node-b)
cut() { vm "$va" "sudo iptables -I OUTPUT -d $VMTEST_SERVER_IP -j DROP && sudo iptables -I INPUT -s $VMTEST_SERVER_IP -j DROP"; }
heal() { vm "$va" "sudo iptables -D OUTPUT -d $VMTEST_SERVER_IP -j DROP; sudo iptables -D INPUT -s $VMTEST_SERVER_IP -j DROP" || true; }

log "--- S1: both Projects run on node-a"
ctl "node set-tier node-a primary" >/dev/null
ctl "node set-tier node-b backup" >/dev/null
for p in "$A" "$F"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
manifest "$A" -
manifest "$F" Forbid
expect "S1 Forbid is stored" Forbid "$(field "$F" .spec.overlap)"
wait_for 300 "$A Running on node-a" running_on "$A" node-a
wait_for 120 "$F Running on node-a" running_on "$F" node-a
expect "S1 Allow Project placed on node-a" node-a/Running "$(node_of "$A")/$(phase_of "$A")"
expect "S1 Forbid Project placed on node-a" node-a/Running "$(node_of "$F")/$(phase_of "$F")"
if [[ "$(node_of "$A")/$(node_of "$F")" != node-a/node-a ]]; then
	log "both Projects must start on node-a for the partition to mean anything; stopping"
	for p in "$A" "$F"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
	return 0
fi

log "--- S2: cut node-a off from the server"
cut
t0=$SECONDS
if wait_for 240 "node-a NotReady" node_is node-a NotReady; then
	observe "S2 seconds from the cut to node-a NotReady" "$((SECONDS - t0))"
fi
expect "S2 node-a is NotReady" NotReady "$(node_state node-a)"

log "--- S3: the Allow Project moves, the Forbid one stays"
if wait_for 420 "$A Running on node-b" running_on "$A" node-b; then
	observe "S3 seconds from the cut to the Allow Project Running on node-b" "$((SECONDS - t0))"
fi
expect "S3 Allow Project moved to node-b" node-b/Running "$(node_of "$A")/$(phase_of "$A")"
expect "S3 Forbid Project still assigned to node-a" node-a "$(node_of "$F")"
expect "S3 Forbid Project's condition names spec.overlap" yes \
	"$(field "$F" '[.status.conditions[]? | select(.type=="TakeoverRequired" and .status=="True") | .message][0] // ""' | grep -q 'spec.overlap is Forbid' && echo yes || echo no)"
expect "S3 Forbid Project has no copy on node-b" 0 "$(copies "$vb" "$F")"
expect "S3 Forbid Project keeps running on node-a" 1 "$(copies "$va" "$F")"
# The overlap the Allow Project accepts: its old copy is still running.
observe "S3 Allow Project copies still running on cut-off node-a" "$(copies "$va" "$A")"

log "--- S4: node-a comes back"
heal
t4=$SECONDS
if wait_for 240 "node-a Ready" node_is node-a Ready; then
	observe "S4 seconds from healing to node-a Ready" "$((SECONDS - t4))"
fi
expect "S4 node-a is Ready" Ready "$(node_state node-a)"
if wait_for 300 "$A's old copy gone from node-a" no_copies "$va" "$A"; then
	observe "S4 seconds from healing to the Allow Project's old copy removed" "$((SECONDS - t4))"
fi
expect "S4 Allow Project's old copy removed from node-a" 0 "$(copies "$va" "$A")"
expect "S4 Allow Project still Running on node-b" node-b/Running "$(node_of "$A")/$(phase_of "$A")"
expect "S4 Forbid Project Running on node-a" node-a/Running "$(node_of "$F")/$(phase_of "$F")"
expect "S4 Forbid Project has one copy on node-a" 1 "$(copies "$va" "$F")"
observe "S4 Forbid Project TakeoverRequired after the Node returned" \
	"$(field "$F" '[.status.conditions[]? | select(.type=="TakeoverRequired") | .status][0] // "absent"')"

log "--- cleanup"
heal
for p in "$A" "$F"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
