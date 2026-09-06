#!/bin/sh
set -e
systemd-sysusers /usr/lib/sysusers.d/monopanel.conf >/dev/null 2>&1 || true
systemd-tmpfiles --create /usr/lib/tmpfiles.d/monopanel.conf >/dev/null 2>&1 || true
install -d -m 0750 -o root -g monopanel /etc/monopanel /etc/monopanel/templates
install -d -m 0751 -o monopanel -g monopanel /var/lib/monopanel
install -d -m 0750 -o monopanel -g monopanel /var/log/monopanel
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl enable monopanel-agent.service monopanel-api.service >/dev/null 2>&1 || true

# On an upgrade the panel is already running the old binary: restart it, or the
# new version would only take effect at the next reboot. dpkg passes the
# previous version in $2, rpm passes 2 as the number of installed packages.
upgrade=0
[ -n "${2:-}" ] && upgrade=1
[ "${1:-}" = "2" ] && upgrade=1
if [ "$upgrade" = "1" ]; then
	systemctl try-restart monopanel-agent.service monopanel-api.service >/dev/null 2>&1 || true
	echo "MonoPanel updated."
else
	echo "MonoPanel installed. Run: mp setup"
fi
