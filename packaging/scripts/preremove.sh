#!/bin/sh
# Stop the panel when the package is being removed — but not when it is being
# replaced by a newer one: an upgrade restarts the units in postinstall.
# dpkg passes remove/upgrade/deconfigure, rpm passes 0 for the last removal.
# The socket goes too: left listening, it would start the API again at the
# next connection.
case "${1:-}" in
remove | purge | 0)
	systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service >/dev/null 2>&1 || true
	;;
esac
exit 0
