#!/bin/sh
# Prints the version cara is built as (CARA-143). It is stamped into the
# binaries and is also the .deb version, so it follows dpkg's ordering:
#
#   on tag v0.1.0              0.1.0
#   on tag v0.1.0-rc.1         0.1.0~rc.1      ~ sorts before 0.1.0
#   anywhere else              <last tag>+git<commit time, UTC>.<sha>[.dirty]
#                              e.g. 0.1.0+git20261106093000.4d6b201, which sorts
#                              after 0.1.0; 0.0.0+git... before the first tag
set -eu
cd "$(dirname "$0")/../.."

# deb <tag> turns vX.Y.Z[-pre] into X.Y.Z[~pre].
deb() {
	v=${1#v}
	case $v in *-*) v="${v%%-*}~${v#*-}" ;; esac
	printf '%s' "$v"
}

sha=$(git rev-parse --short HEAD 2>/dev/null) || { echo 0.0.0+unknown; exit 0; }
dirty=""
[ -z "$(git status --porcelain --untracked-files=no)" ] || dirty=.dirty

if [ -z "$dirty" ] && tag=$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null); then
	deb "$tag"
	echo
	exit 0
fi
base=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo v0.0.0)
when=$(TZ=UTC git show -s --format=%cd --date=format-local:%Y%m%d%H%M%S HEAD)
printf '%s+git%s.%s%s\n' "$(deb "$base")" "$when" "$sha" "$dirty"
