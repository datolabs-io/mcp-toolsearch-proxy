#!/bin/sh
# Create the service user and reload units. A fresh install does not start
# the service: it needs TOOLSEARCH_UPSTREAM set and, for the built-in model,
# fetch-model run first (see /usr/share/doc/mcp-toolsearch-proxy/deploy.md).
# An upgrade restarts it if it is running, so it picks up the new binary.
#
# deb postinst gets "configure" and, on an upgrade, the previous version;
# rpm %post gets the number of installed instances: 1 on install, 2 or more
# on an upgrade.
set -e
upgrade=
case "${1:-}" in
configure) if [ -n "${2:-}" ]; then upgrade=1; fi ;;
[0-9]*) if [ "$1" -ge 2 ]; then upgrade=1; fi ;;
esac
if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers mcp-toolsearch-proxy.conf
fi
# The state directory holds the model, which fetch-model downloads as the
# service user before the service has ever started: create it now rather
# than leave it to StateDirectory= on the first start.
if getent passwd mcp-toolsearch-proxy >/dev/null 2>&1; then
	install -d -m 0700 -o mcp-toolsearch-proxy -g mcp-toolsearch-proxy /var/lib/mcp-toolsearch-proxy
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	if [ -n "$upgrade" ]; then
		systemctl try-restart mcp-toolsearch-proxy.service >/dev/null 2>&1 || true
	fi
fi
