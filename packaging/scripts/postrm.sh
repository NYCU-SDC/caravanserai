#!/bin/sh
# postrm shared by the cara-server and cara-agent packages (CARA-143). It
# deletes nothing: the agent's data and identity under /var/lib/cara, the
# server's state, and the tokens in /etc/cara stay. dpkg removes the env files
# only on purge.
set -e
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload || true
