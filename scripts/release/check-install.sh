#!/usr/bin/env bash
# Installs the packages built by package.sh on this machine and checks what an
# operator gets (CARA-143). The release workflow runs it on its runner, which
# has systemd; without systemd the unit checks are skipped.
#
#   sudo scripts/release/check-install.sh <dir> <arch>
#
# It changes the machine: run it only where cara is not installed.
set -uo pipefail
dir="$(cd "$1" && pwd)" arch=$2
fail=0
check() { # <what> <want> <got>
	if [[ "$2" == "$3" ]]; then echo "ok    $1: $3"; else echo "FAIL  $1: want $2, got $3"; fail=1; fi
}

if dpkg-query -W cara-server cara-agent >/dev/null 2>&1; then
	echo "cara is already installed here; refusing to touch it" >&2
	exit 2
fi
shopt -s nullglob
debs=("$dir"/cara-server_*_"$arch".deb "$dir"/cara-agent_*_"$arch".deb "$dir"/caractl_*_"$arch".deb)
((${#debs[@]} == 3)) || { echo "expected 3 packages for $arch in $dir, found ${#debs[@]}" >&2; exit 2; }
# Absolute paths: apt reads a bare file name as a package name.
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${debs[@]}" >/dev/null || exit 1

check "caractl reports the package version" \
	"$(dpkg-query -W -f '${Version}' caractl)" "$(caractl version | awk '{print $2}')"
check "/etc/cara" "700 root" "$(stat -c '%a %U' /etc/cara)"
check "server.env" "600 root" "$(stat -c '%a %U' /etc/cara/server.env)"
check "agent.env" "600 root" "$(stat -c '%a %U' /etc/cara/agent.env)"
check "break-glass token generated" "600 root" "$(stat -c '%a %U' /etc/cara/break-glass-token 2>&1)"
# Compared by hash, so the token never reaches a CI log.
token_sum() { sha256sum /etc/cara/break-glass-token 2>&1 | cut -c1-12; }
token=$(token_sum)

if [[ -d /run/systemd/system ]]; then
	for s in cara-server cara-agent; do
		check "$s enabled after install" disabled "$(systemctl is-enabled $s 2>&1)"
		check "$s active after install" inactive "$(systemctl is-active $s 2>&1)"
		if out=$(systemd-analyze verify "/usr/lib/systemd/system/$s.service" 2>&1); then
			check "$s unit verifies" "" ""
		else
			check "$s unit verifies" "" "$out"
		fi
	done
else
	echo "skip  no systemd here: unit checks not run"
fi

# A reinstall runs the upgrade path of the maintainer scripts.
echo 'NODE_NAME=edited' >>/etc/cara/agent.env
apt-get install -y -qq --reinstall -o Dpkg::Options::=--force-confold "${debs[@]}" >/dev/null || {
	echo "FAIL  reinstall"; fail=1; }
check "reinstall keeps an edited agent.env" 1 "$(grep -c '^NODE_NAME=edited$' /etc/cara/agent.env)"
check "reinstall keeps the break-glass token" "$token" "$(token_sum)"

apt-get remove -y -qq cara-server cara-agent caractl >/dev/null 2>&1 || { echo "FAIL  remove"; fail=1; }
check "remove keeps the break-glass token" "$token" "$(token_sum)"
check "remove keeps agent.env" yes "$([[ -f /etc/cara/agent.env ]] && echo yes || echo no)"
if [[ -d /run/systemd/system ]]; then
	check "cara-agent unit gone after remove" "" "$(systemctl list-unit-files cara-agent.service --no-legend 2>&1)"
fi
apt-get purge -y -qq cara-server cara-agent >/dev/null 2>&1 || { echo "FAIL  purge"; fail=1; }
check "purge removes agent.env" no "$([[ -f /etc/cara/agent.env ]] && echo yes || echo no)"
check "purge keeps the break-glass token" "$token" "$(token_sum)"
rm -f /etc/cara/break-glass-token
rmdir /etc/cara 2>/dev/null || true

((fail == 0)) && echo "all checks passed"
exit $fail
