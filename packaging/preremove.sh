#!/bin/sh
# Stop and disable the service when the package is removed. An upgrade leaves
# it running; post-install restarts it. State and the user are kept.
#
# deb prerm gets "remove" on removal and "upgrade", "deconfigure" or
# "failed-upgrade" otherwise; rpm %preun gets 0 on erase and 1 on an upgrade.
set -e
case "${1:-}" in
remove | 0) ;;
*) exit 0 ;;
esac
if [ -d /run/systemd/system ]; then
	systemctl disable --now mcp-toolsearch-proxy.service >/dev/null 2>&1 || true
fi
