#!/bin/sh
# postinst shared by the cara-server and cara-agent packages (CARA-143); dpkg
# names the package in DPKG_MAINTSCRIPT_PACKAGE.
set -e
[ "$1" = configure ] || exit 0
pkg=$DPKG_MAINTSCRIPT_PACKAGE

# cara-server refuses to start without the break-glass token its env file
# names. Generate one on first install; never replace one that exists.
if [ "$pkg" = cara-server ] && [ ! -e /etc/cara/break-glass-token ]; then
	(umask 077 && head -c 32 /dev/urandom | base64 >/etc/cara/break-glass-token)
fi

[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload
# $2 is the version configured before; empty on a first install. A first
# install leaves the service disabled and stopped, since it cannot run until
# its env file is filled in. An upgrade restarts a running service so the new
# binary takes over; a stopped one stays stopped.
if [ -n "$2" ]; then
	systemctl try-restart "$pkg.service"
fi
