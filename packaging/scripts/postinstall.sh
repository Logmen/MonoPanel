#!/bin/sh
set -e
systemd-sysusers /usr/lib/sysusers.d/monopanel.conf >/dev/null 2>&1 || true
# mp setup up to 0.8.19 wrote the run directory into /etc/tmpfiles.d without
# the sticky bit, and that copy wins over the package's at boot.
if [ -f /etc/tmpfiles.d/monopanel.conf ]; then
	sed -i 's#^d /run/monopanel 0771 root monopanel -$#d /run/monopanel 1771 root monopanel -#' /etc/tmpfiles.d/monopanel.conf || true
fi
systemd-tmpfiles --create /usr/lib/tmpfiles.d/monopanel.conf >/dev/null 2>&1 || true
install -d -m 0750 -o root -g monopanel /etc/monopanel /etc/monopanel/templates
install -d -m 0751 -o monopanel -g monopanel /var/lib/monopanel
# The agent's own directory and the configuration history in it: root's
# alone, not below the data directory the API owns.
install -d -m 0700 -o root -g root /var/lib/monopanel-agent /var/lib/monopanel-agent/confhistory
# root:monopanel 1771: the API writes its own files here (the audit log), and
# the sticky bit keeps it from renaming or removing root's entries — sites/
# above all, whose logs nginx, Apache and php-fpm open as root. o+x: the
# accounts and the nginx workers pass through to their logs in sites/<login>.
# Up to 0.8.19 the directory was monopanel's; sites/ is made only when it is
# missing, so that one somebody else made is not handed to root (the agent
# refuses it then, with a message).
install -d -m 1771 -o root -g monopanel /var/log/monopanel
[ -e /var/log/monopanel/sites ] || [ -L /var/log/monopanel/sites ] || install -d -m 0711 -o root -g root /var/log/monopanel/sites
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
