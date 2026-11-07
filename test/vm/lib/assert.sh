# shellcheck shell=bash
# Helpers every scenario uses. Sourced by run.sh after the provider, with
# VMTEST_OUT (the record directory) and VMTEST_SCENARIO set.
#
# A check is recorded with `expect` (pass or fail against a wanted value) or
# `observe` (a measured value with no verdict). Both print a RESULT line and
# append one JSON line to $VMTEST_OUT/results.jsonl, so the record holds only
# values that were actually read.

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# vm <vm> <cmd>... runs a command on a VM.
vm() { provider_ssh "$@"; }
srv() { provider_ssh "$VMTEST_SERVER" "$@"; }

# api <path> GETs a cara-server path as the break-glass admin.
api() {
	srv "sudo sh -c 'curl -fsS -H \"Authorization: Bearer \$(cat /etc/cara/break-glass-token)\" http://127.0.0.1:8080$1'"
}

# ctl <args> runs caractl on the server as the break-glass admin. Its stderr
# comes back on this side's stderr.
ctl() {
	srv "sudo sh -c 'CARA_TOKEN=\$(cat /etc/cara/break-glass-token) caractl --server http://127.0.0.1:8080 $*'"
}

# wait_for <seconds> <what> <cmd>... polls cmd every 3 s until it succeeds.
wait_for() {
	local until=$((SECONDS + $1)) what=$2
	shift 2
	while ((SECONDS < until)); do
		if "$@"; then
			log "reached: $what"
			return 0
		fi
		sleep 3
	done
	log "TIMED OUT: $what"
	return 1
}

# Project helpers.
project() { api "/api/v1/projects/$1" 2>/dev/null; }
field() { project "$1" | jq -r "$2" 2>/dev/null; }
# cond <project> <type> prints the reason of a True condition of that type.
cond() { field "$1" "[.status.conditions[]? | select(.type==\"$2\" and .status==\"True\") | .reason][0] // \"\""; }

# Node helpers.
node_heartbeat() { api "/api/v1/nodes/$1" 2>/dev/null | jq -r '.status.lastHeartbeat // ""'; }
# heartbeat_after <node> <epoch> succeeds once the node heartbeats after epoch.
heartbeat_after() {
	local t
	t="$(node_heartbeat "$1")"
	[[ -n "$t" ]] && (($(date -d "$t" +%s) > $2))
}
# vm_for_node <node> prints the VM the node's agent runs on.
vm_for_node() { echo "${VMTEST_NODE_VMS[$1]}"; }

# Agent log helpers. vm_now prints the VM's clock in journalctl's format.
vm_now() { vm "$1" "date -u '+%Y-%m-%d %H:%M:%S'"; }
agent_log_since() { vm "$1" "sudo journalctl -u cara-agent --no-pager -o cat --since '$2'"; }
agent_log_count() { agent_log_since "$1" "$2" | grep -c -- "$3"; }

_record() {
	local verdict=$1 check=$2 want=$3 got=$4
	jq -nc --arg scenario "$VMTEST_SCENARIO" --arg check "$check" --arg want "$want" \
		--arg got "$got" --arg verdict "$verdict" --arg at "$(date -u +%FT%TZ)" \
		'{scenario: $scenario, check: $check, want: $want, got: $got, verdict: $verdict, at: $at}' \
		>>"$VMTEST_OUT/results.jsonl"
}

# expect <check> <want> <got> records a check and its verdict.
expect() {
	local verdict=pass
	[[ "$2" == "$3" ]] || verdict=FAIL
	_record "$verdict" "$1" "$2" "$3"
	log "RESULT [$verdict] $1: want '$2', got '$3'"
	[[ $verdict == pass ]] && return 0
	VMTEST_FAILURES=$((${VMTEST_FAILURES:-0} + 1))
	return 0
}

# observe <check> <value> records a measured value with no verdict.
observe() {
	_record observed "$1" "" "$2"
	log "RESULT [observed] $1: $2"
}

# power_off <vm> cuts a VM's power from inside the guest (sysrq), then stops it
# on the platform so it stays off. Hard power-offs are rationed: at least
# VMTEST_POWER_OFF_GAP seconds (default 1500) apart within a run, so the
# platform's owner is not asked for back-to-back crashes.
power_off() {
	local gap=${VMTEST_POWER_OFF_GAP:-1500} last=${VMTEST_LAST_POWER_OFF:-0} now
	now=$(date +%s)
	if ((last > 0 && now - last < gap)); then
		if [[ "${VMTEST_ALLOW_WAIT:-}" == 1 ]]; then
			log "waiting $((gap - (now - last))) s before the next hard power-off"
			sleep $((gap - (now - last)))
		else
			echo "power_off: only $((now - last)) s since the last hard power-off (need $gap)" >&2
			return 1
		fi
	fi
	vm "$1" "sudo sh -c 'echo 1 > /proc/sys/kernel/sysrq; echo o > /proc/sysrq-trigger'" >/dev/null 2>&1 || true
	VMTEST_LAST_POWER_OFF=$(date +%s)
	log "hard power-off sent to $1"
	sleep 20
	provider_stop "$1" || true
}
