# shellcheck shell=bash
# vmtest-proves: A Managed Project with no spec.backup is backed up on the server's default interval and committed; one with backup disabled is never backed up, and is once it is applied again without the section (ADR-0029)
# vmtest-needs: s3
# vmtest-server-env: DEFAULT_BACKUP_INTERVAL=2m
# vmtest-power-offs: 0
# vmtest-minutes: 15
# vmtest-enrol: auto

D=bk-default
X=bk-disabled

# manifest <name> <disabled|-> writes and applies a Project with one Managed
# volume. With "-" it says nothing about backup.
manifest() {
	{
		printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n' "$1"
		printf '  volumes:\n    - name: data\n      type: Managed\n'
		printf '  services:\n    - name: app\n      image: busybox:1.36\n'
		printf '      command: ["sh", "-c", "date >> /data/boots; exec sleep 86400"]\n'
		printf '      volumeMounts:\n        - name: data\n          mountPath: /data\n'
		if [[ $2 == disabled ]]; then
			printf '  backup:\n    disabled: true\n'
		fi
	} | srv "cat > /tmp/$1.yaml"
	ctl "apply -f /tmp/$1.yaml" >/dev/null
}

running() { [[ "$(field "$1" .status.phase)" == Running ]]; }
committed() { field "$1" '.status.recovery.committedBackup.backupID // ""'; }
has_committed() { [[ -n "$(committed "$1")" ]]; }
node_vm() { vm_for_node "$(field "$1" .status.nodeRef)"; }

log "--- S1: apply one Project without spec.backup and one with backup disabled"
for p in "$D" "$X"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
manifest "$D" -
manifest "$X" disabled
expect "S1 the default Project is stored with the server's interval" '{"interval":"2m"}' "$(field "$D" '.spec.backup | tojson')"
expect "S1 the disabled Project is stored as written" '{"disabled":true}' "$(field "$X" '.spec.backup | tojson')"

log "--- S2: both reach Running"
if wait_for 300 "$D Running" running "$D"; then s2d=Running; else s2d="$(field "$D" .status.phase)"; fi
if wait_for 120 "$X Running" running "$X"; then s2x=Running; else s2x="$(field "$X" .status.phase)"; fi
expect "S2 default Project reaches Running" Running "$s2d"
expect "S2 disabled Project reaches Running" Running "$s2x"
xvm=$(node_vm "$X")
since=$(vm_now "$xvm")
t0=$SECONDS

log "--- S3: the default Project is backed up and the backup committed"
if wait_for 420 "$D has a committed backup" has_committed "$D"; then
	observe "S3 seconds from Running to the first committed backup (interval 2m)" "$((SECONDS - t0))"
	s3=yes
else
	s3=no
fi
expect "S3 default Project has a committed backup" yes "$s3"
if wait_for 120 "$D Running again after its backup" running "$D"; then s3r=Running; else s3r="$(field "$D" .status.phase)"; fi
expect "S3 default Project is Running after the backup" Running "$s3r"

log "--- S4: the disabled Project is never backed up"
# At least two default intervals since it became Running.
while ((SECONDS - t0 < 270)); do sleep 10; done
expect "S4 disabled Project has no committed backup" "" "$(committed "$X")"
expect "S4 no backup supervisor started for the disabled Project" 0 \
	"$(agent_log_since "$xvm" "$since" | grep 'Started backup supervisor' | grep -c -- "$X")"
expect "S4 no backup ran for the disabled Project" 0 \
	"$(agent_log_since "$xvm" "$since" | grep 'Starting backup' | grep -c -- "$X")"

log "--- S5: applying it again without the section turns backup on"
manifest "$X" -
expect "S5 the re-applied Project gets the server's interval" '{"interval":"2m"}' "$(field "$X" '.spec.backup | tojson')"
t5=$SECONDS
if wait_for 420 "$X has a committed backup" has_committed "$X"; then
	observe "S5 seconds from re-apply to the first committed backup (interval 2m)" "$((SECONDS - t5))"
	s5=yes
else
	s5=no
fi
expect "S5 re-enabled Project has a committed backup" yes "$s5"

log "--- cleanup"
for p in "$D" "$X"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
