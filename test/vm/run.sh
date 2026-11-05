#!/usr/bin/env bash
# Runs VM scenarios against one commit of cara (ADR-0032, CARA-142).
#
#   test/vm/run.sh [--commit <sha>] [--out <dir>] [--allow-wait] <scenario>...
#   test/vm/run.sh --list
#
# Creates three VMs (a server and two Nodes), deploys the commit, runs the
# scenarios in order, collects every log, writes a record, and deletes the VMs,
# also when a scenario fails or the run is interrupted. See test/vm/README.md.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
cd "$repo" || exit 1

usage() {
	sed -n '2,9p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 2
}

# meta <scenario file> <key> prints a "# vmtest-<key>:" header value.
meta() { sed -n "s/^# vmtest-$2: *//p" "$1" | head -1; }

list_scenarios() {
	local f
	for f in "$here"/scenarios/*.sh; do
		printf '%-20s %3s min  power-offs %s  needs %-12s %s\n' "$(basename "$f" .sh)" \
			"$(meta "$f" minutes)" "$(meta "$f" power-offs)" "$(meta "$f" needs || true)" "$(meta "$f" proves)"
	done
}

commit="" out="" scenarios=()
while (($# > 0)); do
	case $1 in
	--commit) commit=$2; shift 2 ;;
	--out) out=$2; shift 2 ;;
	--allow-wait) export VMTEST_ALLOW_WAIT=1; shift ;;
	--list) list_scenarios; exit 0 ;;
	-h | --help) usage ;;
	-*) echo "unknown option $1" >&2; usage ;;
	*) scenarios+=("$1"); shift ;;
	esac
done
((${#scenarios[@]} > 0)) || usage

# ── What is being tested ──────────────────────────────────────────────────────
# A record must name the code it tested. Without --commit that is HEAD, and only
# if nothing uncommitted could make the binaries differ from it.
if [[ -z "$commit" ]]; then
	if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
		echo "the working tree has uncommitted changes: commit them, or name a commit with --commit" >&2
		exit 2
	fi
	commit=HEAD
fi
sha="$(git rev-parse --verify --quiet "$commit^{commit}")" || { echo "no such commit: $commit" >&2; exit 2; }
short="${sha:0:7}"

files=() power_offs=0 needs="" server_env="" minutes=0
for s in "${scenarios[@]}"; do
	f="$here/scenarios/$s.sh"
	[[ -f "$f" ]] || { echo "no scenario $s (see --list)" >&2; exit 2; }
	files+=("$f")
	power_offs=$((power_offs + $(meta "$f" power-offs)))
	minutes=$((minutes + $(meta "$f" minutes)))
	needs+=" $(meta "$f" needs)"
	e="$(meta "$f" server-env)"
	[[ -n "$e" ]] && server_env+="${server_env:+$'\n'}${e// /$'\n'}"
done
if ((power_offs > 1)) && [[ "${VMTEST_ALLOW_WAIT:-}" != 1 ]]; then
	echo "these scenarios need $power_offs hard power-offs, at least 25 min apart; pass --allow-wait to accept the wait" >&2
	exit 2
fi
[[ " $needs " == *" s3 "* ]] && export VMTEST_WITH_S3=1
[[ " $needs " == *" pgdiff "* ]] && export VMTEST_WITH_PGDIFF=1 VMTEST_WITH_S3=1
export VMTEST_SERVER_ENV="$server_env"

out="${out:-$repo/.vmtest/$(date -u +%Y-%m-%d)-$short}"
mkdir -p "$out/logs" || exit 1
: >"$out/results.jsonl"
export VMTEST_OUT="$out"

# ── VMs ───────────────────────────────────────────────────────────────────────
VMTEST_SERVER=vt-server
declare -A VMTEST_NODE_VMS=([node-a]=vt-node-a [node-b]=vt-node-b)
all_vms=("$VMTEST_SERVER" "${VMTEST_NODE_VMS[@]}")

# shellcheck source=lib/provider-hangar.sh
. "$here/lib/provider-${VMTEST_PROVIDER:-hangar}.sh"
# shellcheck source=lib/assert.sh
. "$here/lib/assert.sh"
# shellcheck source=lib/deploy.sh
. "$here/lib/deploy.sh"

provider_check || exit 1
for v in "${all_vms[@]}"; do
	if provider_exists "$v"; then
		echo "$v already exists: an earlier run did not clean up. Delete it first ($HANGAR delete vm $v)" >&2
		exit 1
	fi
done

collect() {
	local v
	srv "sudo journalctl -u cara-server --no-pager" >"$out/logs/server.log" 2>&1
	for v in "${VMTEST_NODE_VMS[@]}"; do
		vm "$v" "sudo journalctl -u cara-agent --no-pager" >"$out/logs/agent-$v.log" 2>&1
	done
	api /api/v1/nodes >"$out/logs/nodes.json" 2>&1
	api /api/v1/projects >"$out/logs/projects.json" 2>&1
}

cleaned=0
cleanup() {
	((cleaned)) && return
	cleaned=1
	trap - EXIT INT TERM
	if [[ -n "${VMTEST_SERVER_IP:-}" ]]; then
		log "collecting logs"
		collect
	fi
	log "deleting the VMs"
	provider_down "${all_vms[@]}"
	[[ -n "${bin:-}" ]] && rm -rf "$bin"
	local left
	left="$(provider_list | grep -E '^vt-' || true)"
	if [[ -n "$left" ]]; then
		log "VMs LEFT BEHIND, delete them by hand: $left"
	else
		log "no VM left"
	fi
}
trap cleanup EXIT
trap 'log "interrupted"; exit 130' INT TERM

log "commit $sha; scenarios ${scenarios[*]}; about $minutes min plus about 5 min of setup; $power_offs hard power-off(s)"
log "record: $out"

# The binaries are not part of the record: the commit names them.
bin="$(mktemp -d)"
log "building $short"
deploy_build "$sha" "$bin" || { log "build failed"; exit 1; }

log "creating ${all_vms[*]}"
provider_up "${all_vms[@]}" || exit 1
VMTEST_SERVER_IP="$(srv "hostname -I" | awk '{print $1}')"
export VMTEST_SERVER_IP

log "deploying (s3=${VMTEST_WITH_S3:-0}, pgdiff=${VMTEST_WITH_PGDIFF:-0})"
deploy_hosts || { log "installing Docker failed"; exit 1; }
deploy_server "$bin" || { log "deploying the server failed"; exit 1; }
for node in "${!VMTEST_NODE_VMS[@]}"; do
	deploy_agent "${VMTEST_NODE_VMS[$node]}" "$node" "$bin" || { log "deploying $node failed"; exit 1; }
done

# ── Scenarios ─────────────────────────────────────────────────────────────────
VMTEST_FAILURES=0
for i in "${!files[@]}"; do
	f=${files[$i]}
	VMTEST_SCENARIO=${scenarios[$i]}
	export VMTEST_SCENARIO
	if [[ "$(meta "$f" enrol)" != manual ]]; then
		for node in node-a node-b; do
			[[ -n "$(node_heartbeat "$node")" ]] || enrol_agent "$node" || { log "enrolling $node failed"; exit 1; }
		done
	fi
	log "=== scenario $VMTEST_SCENARIO: $(meta "$f" proves)"
	# Sourced in this shell (not through a pipe, which would be a subshell) so
	# the time of the last hard power-off carries over to the next scenario.
	# shellcheck disable=SC1090
	. "$f" > >(tee "$out/logs/scenario-$VMTEST_SCENARIO.out") 2>&1
done
failed=$(jq -s '[.[] | select(.verdict == "FAIL")] | length' "$out/results.jsonl")

# ── Record ────────────────────────────────────────────────────────────────────
{
	echo "# VM 測試：${scenarios[*]}"
	echo
	echo "- 版本：\`$sha\`"
	echo "- 日期：$(date -u '+%Y-%m-%d %H:%M') UTC"
	echo "- 環境：${VMTEST_PROVIDER:-hangar}，$($HANGAR config view 2>/dev/null | sed -n 's/^\* *//p' | awk '{print $NF}')；3 台 ${VMTEST_IMAGE:-ubuntu-24-04}、${VMTEST_CPU:-2} vCPU、${VMTEST_MEMORY:-4Gi}"
	echo "- 選項：s3=${VMTEST_WITH_S3:-0}、pgdiff=${VMTEST_WITH_PGDIFF:-0}；server 額外設定：${VMTEST_SERVER_ENV:-（無）}"
	echo "- 由 \`test/vm/run.sh\` 產生；每項只跑一次，耗時都是單次值"
	echo
	echo "## 結果"
	echo
	echo "通過 $(jq -s '[.[] | select(.verdict == "pass")] | length' "$out/results.jsonl")、失敗 $failed、觀測值 $(jq -s '[.[] | select(.verdict == "observed")] | length' "$out/results.jsonl")。"
	echo
	echo "| 情境 | 檢查 | 期望 | 實際 | 判定 |"
	echo "|---|---|---|---|---|"
	jq -r '"| \(.scenario) | \(.check) | \(.want) | \(.got | gsub("\\|"; "\\\\|")) | \(.verdict) |"' "$out/results.jsonl"
	echo
	echo "## 發現的缺陷"
	echo
	echo "（由執行的人填寫）"
	echo
	echo "## 這次沒有測到的"
	echo
	echo "（由執行的人填寫）"
} >"$out/README.md"

log "$failed check(s) failed; record in $out"
((failed == 0))
