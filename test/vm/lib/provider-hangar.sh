# shellcheck shell=bash
# The only layer that knows the VMs come from Hangar (ADR-0032). Another IaaS
# needs another file with the same functions:
#
#   provider_up <vm>...          create the VMs and wait until each answers SSH
#   provider_down <vm>...        delete the VMs and wait until they are gone
#   provider_ssh <vm> <cmd>...   run a command on a VM
#   provider_stop <vm>           power off (the guest is asked first)
#   provider_start <vm>          power on
#   provider_exists <vm>         succeed when the VM exists
#   provider_list                names of the harness's VMs that exist
#
# HANGAR names the CLI (default: hangar on PATH). The context's project is the
# one used; run `hangar config view` to see it.

HANGAR="${HANGAR:-hangar}"

# Every VM this harness creates carries this label, and provider_down refuses
# a VM without it, so a typo cannot delete someone else's machine.
VMTEST_LABEL_KEY=purpose
VMTEST_LABEL_VALUE=cara-test

provider_check() {
	command -v "$HANGAR" >/dev/null 2>&1 || [[ -x "$HANGAR" ]] || {
		echo "hangar CLI not found (set HANGAR=/path/to/hangar)" >&2
		return 1
	}
	if ! "$HANGAR" get virtualmachines >/dev/null 2>&1; then
		echo "hangar is not logged in, or its project cannot list VMs: run '$HANGAR login --device'" >&2
		return 1
	fi
}

provider_exists() {
	"$HANGAR" get virtualmachine "$1" -o json >/dev/null 2>&1
}

# provider_list prints the names of the VMs this harness created and still exist.
provider_list() {
	"$HANGAR" get virtualmachines -l "$VMTEST_LABEL_KEY=$VMTEST_LABEL_VALUE" -o json 2>/dev/null |
		jq -r '(.items // [.])[] | .metadata.name // empty'
}

provider_up() {
	local manifest vm
	manifest="$(mktemp)"
	for vm in "$@"; do
		cat >>"$manifest" <<YAML
apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata:
  name: $vm
  labels: {$VMTEST_LABEL_KEY: $VMTEST_LABEL_VALUE}
spec:
  rootDisk:
    fromImage:
      image: {name: ${VMTEST_IMAGE:-ubuntu-24-04}}
      size: 30Gi
  resources: {cpu: "${VMTEST_CPU:-2}", memory: ${VMTEST_MEMORY:-4Gi}}
---
YAML
	done
	"$HANGAR" apply -f "$manifest" >/dev/null || { rm -f "$manifest"; return 1; }
	rm -f "$manifest"

	# Ready only means the VM runs; SSH comes a little later.
	local deadline=$((SECONDS + 600)) pending=("$@") still
	while ((${#pending[@]} > 0)); do
		still=()
		for vm in "${pending[@]}"; do
			timeout 20 "$HANGAR" ssh vm "$vm" -- true >/dev/null 2>&1 || still+=("$vm")
		done
		pending=("${still[@]}")
		((${#pending[@]} == 0)) && break
		((SECONDS < deadline)) || { echo "VMs not reachable over SSH: ${pending[*]}" >&2; return 1; }
		sleep 10
	done
}

provider_down() {
	local vm label
	for vm in "$@"; do
		provider_exists "$vm" || continue
		label="$("$HANGAR" get virtualmachine "$vm" -o json 2>/dev/null | jq -r ".metadata.labels[\"$VMTEST_LABEL_KEY\"] // \"\"")"
		if [[ "$label" != "$VMTEST_LABEL_VALUE" ]]; then
			echo "refusing to delete $vm: it has no $VMTEST_LABEL_KEY: $VMTEST_LABEL_VALUE label" >&2
			continue
		fi
		timeout 180 "$HANGAR" stop vm "$vm" --wait >/dev/null 2>&1 || true
		timeout 120 "$HANGAR" delete vm "$vm" >/dev/null 2>&1 || true
	done
	for vm in "$@"; do
		timeout 300 "$HANGAR" wait vm "$vm" --for=delete >/dev/null 2>&1 || true
	done
}

provider_ssh() {
	local vm=$1
	shift
	timeout "${VMTEST_SSH_TIMEOUT:-120}" "$HANGAR" ssh vm "$vm" -- "$@"
}

provider_stop() { "$HANGAR" stop vm "$1" --wait >/dev/null; }
provider_start() { "$HANGAR" start vm "$1" >/dev/null; }
