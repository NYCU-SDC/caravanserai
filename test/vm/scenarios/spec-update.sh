# shellcheck shell=bash
# vmtest-proves: A Running Project's spec changes in place: only changed services are recreated, the phase stays Running, a bad image leaves the old container serving, data survives, removed services go, and an agent restart mid-update converges (CARA-134)
# vmtest-needs:
# vmtest-server-env:
# vmtest-power-offs: 0
# vmtest-minutes: 10
# vmtest-enrol: auto

P=spec-demo
GOOD1=nginx:1.27-alpine
GOOD2=nginx:1.28-alpine
BAD=nginx:cara-no-such-tag

# manifest <web image> <app command marker|-> writes the Project manifest. With
# "-" the app service is left out.
manifest() {
	{
		printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n' "$P"
		printf '  volumes:\n    - name: data\n      type: Managed\n'
		printf '  services:\n    - name: web\n      image: %s\n' "$1"
		if [[ $2 != - ]]; then
			printf '    - name: app\n      image: busybox:1.36\n'
			printf '      command: ["sh", "-c", "echo %s >> /data/boots; exec sleep 86400"]\n' "$2"
			printf '      volumeMounts:\n        - name: data\n          mountPath: /data\n'
		fi
	} | srv "cat > /tmp/spec-project.yaml"
	ctl "apply -f /tmp/spec-project.yaml" >/dev/null
}

node_vm() { vm_for_node "$(field "$P" .status.nodeRef)"; }
cid() { vm "$(node_vm)" "sudo docker ps -aq --no-trunc --filter label=cara.project=$P --filter label=cara.service=$1"; }
state() { vm "$(node_vm)" "sudo docker inspect -f '{{.State.Status}}' $(cid "$1") 2>/dev/null || echo none"; }
revision() { field "$P" .metadata.specRevision; }
observed() { field "$P" '.status.observedSpecRevision // 0'; }
applied() { field "$P" '[.status.conditions[]? | select(.type=="SpecApplied") | "\(.status)/\(.reason)"][0] // ""'; }
boots() { vm "$(node_vm)" "sudo cat /var/lib/cara/volumes/default/$P/data/data/boots 2>/dev/null | wc -l"; }

# wait_applied <revision> <label> waits until revision is reported running,
# records every phase seen on the way in PHASES, and sets APPLIED to yes or
# no. It runs in this shell, not a subshell, so PHASES survives.
PHASES=""
APPLIED=""
caught_up() {
	PHASES+="$(field "$P" .status.phase) "
	[[ "$(observed)" == "$1" && "$(applied)" == True/Applied ]]
}
wait_applied() {
	PHASES=""
	APPLIED=no
	local t0=$SECONDS
	if wait_for 300 "revision $1 applied" caught_up "$1"; then
		observe "$2 seconds from apply to revision $1 applied" "$((SECONDS - t0))"
		APPLIED=yes
	fi
}
only_running() { [[ -z "$(tr ' ' '\n' <<<"$PHASES" | grep -v -x -e Running -e '' | sort -u)" ]] && echo yes || echo "no: $PHASES"; }

log "--- S1: place the Project"
ctl "delete project $P --force" >/dev/null 2>&1 || true
manifest "$GOOD1" one
running() { [[ "$(field "$P" .status.phase)" == Running ]]; }
if wait_for 300 "$P Running" running; then s1=Running; else s1="$(field "$P" .status.phase)"; fi
expect "S1 project reaches Running" Running "$s1"
wait_applied 1 S1
expect "S1 revision 1 reported running" yes "$APPLIED"
web1=$(cid web)
app1=$(cid app)
expect "S1 app wrote one line to the Managed volume" 1 "$(boots)"

log "--- S2: change web's image"
manifest "$GOOD2" one
expect "S2 spec revision" 2 "$(revision)"
wait_applied 2 S2
expect "S2 revision 2 applied" yes "$APPLIED"
expect "S2 phase stayed Running throughout" yes "$(only_running)"
expect "S2 web was recreated" yes "$([[ "$(cid web)" != "$web1" && -n "$(cid web)" ]] && echo yes || echo no)"
expect "S2 web runs the new image" "$GOOD2" "$(vm "$(node_vm)" "sudo docker inspect -f '{{.Config.Image}}' $(cid web)")"
expect "S2 app kept its container" "$app1" "$(cid app)"
web2=$(cid web)

log "--- S3: change app's command; its Managed data survives the recreate"
manifest "$GOOD2" two
wait_applied 3 S3
expect "S3 revision 3 applied" yes "$APPLIED"
expect "S3 app was recreated" yes "$([[ "$(cid app)" != "$app1" ]] && echo yes || echo no)"
expect "S3 web kept its container" "$web2" "$(cid web)"
expect "S3 the volume holds both boots" 2 "$(boots)"

log "--- S4: an image that does not exist"
manifest "$BAD" two
pull_failed() { [[ "$(applied)" == False/ImagePullFailed ]]; }
if wait_for 120 "SpecApplied False/ImagePullFailed" pull_failed; then s4=yes; else s4="$(applied)"; fi
expect "S4 the failed pull is reported" yes "$s4"
expect "S4 web keeps its old container" "$web2" "$(cid web)"
expect "S4 which is still running" running "$(state web)"
expect "S4 phase" Running "$(field "$P" .status.phase)"
expect "S4 observed stays at 3" 3 "$(observed)"

log "--- S5: back to a good image, and app removed"
manifest "$GOOD1" -
wait_applied 5 S5
expect "S5 revision 5 applied" yes "$APPLIED"
expect "S5 app's container is gone" "" "$(cid app)"
expect "S5 web runs" running "$(state web)"
expect "S5 phase stayed Running throughout" yes "$(only_running)"

log "--- S6: restart the agent right after an update"
web5=$(cid web)
manifest "$GOOD2" -
vm "$(node_vm)" "sudo systemctl restart cara-agent"
wait_applied 6 S6
expect "S6 revision 6 applied after the restart" yes "$APPLIED"
expect "S6 web was recreated" yes "$([[ "$(cid web)" != "$web5" && -n "$(cid web)" ]] && echo yes || echo no)"
expect "S6 phase stayed Running throughout" yes "$(only_running)"
expect "S6 one web container" 1 "$(vm "$(node_vm)" "sudo docker ps -aq --filter label=cara.project=$P | wc -l")"

log "--- S7: delete"
nodevm=$(node_vm)
ctl "delete project $P" >/dev/null
gone() { [[ -z "$(project "$P")" ]]; }
if wait_for 180 "$P gone" gone; then s7=yes; else s7=no; fi
expect "S7 project deleted" yes "$s7"
expect "S7 containers left" 0 "$(vm "$nodevm" "sudo docker ps -aq --filter label=cara.project=$P | wc -l")"
