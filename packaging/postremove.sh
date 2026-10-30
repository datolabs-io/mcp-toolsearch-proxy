#!/bin/sh
# After removal, drop the unit from systemd. A deb purge also deletes the
# state directory, with the downloaded model; a plain remove keeps it. rpm
# has no purge, so an erase keeps it too.
#
# deb postrm gets "remove", "purge", "upgrade" and others; rpm %postun gets
# 0 on erase and 1 or more on an upgrade.
set -e
case "${1:-}" in
remove | purge | 0) ;;
*) exit 0 ;;
esac
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi
if [ "$1" = purge ]; then
	rm -rf /var/lib/mcp-toolsearch-proxy
fi
