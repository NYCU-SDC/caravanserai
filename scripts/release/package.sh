#!/usr/bin/env bash
# Builds cara for linux/<arch> and packages it (CARA-143).
#
#   scripts/release/package.sh <amd64|arm64> <outdir>
#
# writes into <outdir>:
#   cara-server_<version>_<arch>.deb
#   cara-agent_<version>_<arch>.deb
#   caractl_<version>_<arch>.deb
#   caractl_<version>_linux_<arch>.tar.gz
#   caractl_<version>_darwin_<arch>.tar.gz
#
# make package, test/vm and .github/workflows/release.yml all call this, so a VM
# test installs the same packages a release ships. The version comes from
# version.sh unless CARA_VERSION is set.
set -euo pipefail

# nfpm turns packaging/*.yaml into .deb files. Pinned; `go run pkg@version`
# builds it into the Go cache and ignores this module's go.mod.
NFPM=github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0

(($# == 2)) || { sed -n '4,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
arch=$1
case $arch in amd64 | arm64) ;; *) echo "unsupported architecture $arch: amd64 or arm64" >&2; exit 2 ;; esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
mkdir -p "$2"
out="$(cd "$2" && pwd)"
version="${CARA_VERSION:-$("$root/scripts/release/version.sh")}"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ldflags="-s -w -X main.Version=$version -X main.CommitHash=$commit -X main.BuildTime=$built"
# File names use - where the version has ~ (a prerelease), which some download
# tools and GitHub release assets handle badly.
fileversion="${version//\~/-}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

build() { # <goos> <command>
	(cd "$root" && CGO_ENABLED=0 GOOS=$1 GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$work/$1/$2" "./cmd/$2")
}
for c in cara-server cara-agent caractl; do
	build linux "$c"
done
build darwin caractl

# nfpm does not expand variables in file paths, so it runs in a staging
# directory laid out as packaging/*.yaml expect: packaging/ and bin/.
cp -r "$root/packaging" "$work/packaging"
ln -s linux "$work/bin"
for p in cara-server cara-agent caractl; do
	(cd "$work" && CARA_VERSION="$version" CARA_ARCH="$arch" \
		go run "$NFPM" package --packager deb --config "packaging/$p.yaml" \
		--target "$out/${p}_${fileversion}_${arch}.deb" >/dev/null)
done
for os in linux darwin; do
	tar -C "$work/$os" -czf "$out/caractl_${fileversion}_${os}_${arch}.tar.gz" caractl
done
echo "cara $version for $arch:"
ls -1 "$out" | grep -F "_${fileversion}_" | sed 's/^/  /'
