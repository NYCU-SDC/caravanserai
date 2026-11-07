#!/bin/sh
# prerm shared by the cara-server and cara-agent packages (CARA-143).
# Removing the package stops and disables its service. An upgrade leaves it
# running; postinst restarts it on the new binary.
set -e
[ "$1" = remove ] || exit 0
[ -d /run/systemd/system ] || exit 0
systemctl disable --now "$DPKG_MAINTSCRIPT_PACKAGE.service" >/dev/null 2>&1 || true
