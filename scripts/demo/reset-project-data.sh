#!/usr/bin/env bash
set -Eeuo pipefail

# Remove one Project's Managed volume data and restore staging from this Node.
#
# This exists because the alternative an operator reaches for is
#
#     rm -rf /var/lib/cara/volumes/default/$project
#
# typed at 2am with $project possibly unset, which deletes every Project's data
# on the Node. Everything below is that command with the mistakes taken out:
# both names are validated, both paths are proved to resolve inside the data
# root, and nothing is removed without being shown first.
#
# It is a rehearsal and recovery tool, not part of any automatic flow. cara
# never deletes Managed volume data on its own — data that outlives its Project
# is deliberate, and reclaiming it is a decision a person makes.
#
# Usage:
#   reset-project-data.sh --project NAME [--namespace default]
#                         [--data-root /var/lib/cara] [--check] [--yes]
#
#   --check   report what is present and exit non-zero if anything is; removes
#             nothing. This is the form to use as a precondition.
#   --yes     skip the confirmation prompt. For scripted rehearsals only.

log() { printf '[reset-project-data] %s\n' "$*"; }
fail() { log "ERROR: $*" >&2; exit 1; }

namespace=default
project=
data_root="${AGENT_DATA_ROOT:-/var/lib/cara}"
check_only=0
assume_yes=0

while (( $# )); do
	case "$1" in
		--namespace) namespace="${2:-}"; shift 2 ;;
		--project)   project="${2:-}"; shift 2 ;;
		--data-root) data_root="${2:-}"; shift 2 ;;
		--check)     check_only=1; shift ;;
		--yes)       assume_yes=1; shift ;;
		-h|--help)   sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*)           fail "unknown argument: $1" ;;
	esac
done

# A DNS-style name, which is what the API accepts. The point is not politeness
# about formatting: it is that a value containing a slash, a dot-dot or a
# newline could otherwise steer the deletion somewhere else entirely.
valid_name() { [[ "$1" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; }

[[ -n "$project" ]] || fail "--project is required"
valid_name "$project" || fail "project name is not a DNS-style name: $project"
[[ -n "$namespace" ]] || fail "--namespace must not be empty"
valid_name "$namespace" || fail "namespace is not a DNS-style name: $namespace"

[[ -n "$data_root" ]] || fail "--data-root must not be empty"
[[ "$data_root" == /* ]] || fail "--data-root must be absolute: $data_root"
[[ -d "$data_root" ]] || fail "--data-root is not a directory: $data_root"
resolved_root="$(cd "$data_root" && pwd -P)"
[[ "$resolved_root" != "/" ]] || fail "refusing to operate with / as the data root"

# Both targets are derived, never taken from an argument, and each is then
# proved to sit under the data root. The second check is what catches a symlink
# planted where a Project directory should be.
volumes_target="$resolved_root/volumes/$namespace/$project"
staging_target="$resolved_root/restore-staging/$namespace/$project"

inside_root() {
	local path=$1 parent
	parent="$(dirname "$path")"
	[[ -d "$parent" ]] || return 0 # nothing to remove; containment is moot
	local real_parent
	real_parent="$(cd "$parent" && pwd -P)"
	[[ "$real_parent/$(basename "$path")" == "$resolved_root"/* ]]
}

for target in "$volumes_target" "$staging_target"; do
	inside_root "$target" || fail "refusing to remove a path that resolves outside $resolved_root: $target"
done

present=()
for target in "$volumes_target" "$staging_target"; do
	# -e is false for a broken symlink; -L keeps that filesystem entry visible
	# so the containment check and removal path cannot silently skip it.
	[[ -e "$target" || -L "$target" ]] && present+=("$target")
done

if (( ${#present[@]} == 0 )); then
	log "nothing present for $namespace/$project under $resolved_root"
	exit 0
fi

log "found for $namespace/$project:"
for target in "${present[@]}"; do
	printf '    %s  (%s)\n' "$target" "$(du -sh "$target" 2>/dev/null | cut -f1 || echo '?')"
done

if (( check_only )); then
	log "--check: these must be absent before a clean-destination placement"
	exit 1
fi

if (( ! assume_yes )); then
	# The volume directory may hold the only copy of data that was never
	# backed up, so the prompt names the Project rather than accepting a bare
	# yes: an operator who has the wrong terminal focused types the wrong name.
	printf '[reset-project-data] type the project name to confirm deletion: '
	read -r confirmation
	[[ "$confirmation" == "$project" ]] || fail "confirmation did not match; nothing was removed"
fi

for target in "${present[@]}"; do
	rm -rf -- "$target"
	log "removed $target"
done

for target in "${present[@]}"; do
	[[ ! -e "$target" && ! -L "$target" ]] || fail "$target still exists after removal"
done
log "done"
