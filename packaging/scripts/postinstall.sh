#!/bin/sh
set -e
systemd-sysusers /usr/lib/sysusers.d/monopanel.conf >/dev/null 2>&1 || true
# mp setup up to 0.8.19 wrote the run directory into /etc/tmpfiles.d without
# the sticky bit, and that copy wins over the package's at boot. Nor did it
# make php/ in it, where the php-fpm masters make the pools' sockets as root:
# the API had the agent write monopanel-php.conf for that, which it no longer
# can. The line goes into the copy, for the run directory it names, so that
# php/ is root's from boot, before the API runs; the API's file, holding just
# that line, goes (two lines for one path are a warning at every boot).
tmpfiles=/etc/tmpfiles.d/monopanel.conf
if [ -f "$tmpfiles" ]; then
	sed -i 's#^d /run/monopanel 0771 root monopanel -$#d /run/monopanel 1771 root monopanel -#' "$tmpfiles" || true
	rundir=$(sed -n 's#^d \(/[^ ]*\) [0-7]* root [^ ]* -$#\1#p' "$tmpfiles" | grep -v '/php$' | head -n 1)
	if [ -n "$rundir" ] && ! grep -q "^d $rundir/php " "$tmpfiles"; then
		[ -z "$(tail -c 1 "$tmpfiles")" ] || echo >>"$tmpfiles" || true
		echo "d $rundir/php 0755 root root -" >>"$tmpfiles" || true
	fi
	if [ -n "$rundir" ] && grep -q "^d $rundir/php 0755 root root -$" "$tmpfiles" &&
		[ "$(cat /etc/tmpfiles.d/monopanel-php.conf 2>/dev/null)" = "d $rundir/php 0755 root root -" ]; then
		rm -f /etc/tmpfiles.d/monopanel-php.conf || true
	fi
fi
# The API could rewrite that file up to 0.8.19, and systemd-tmpfiles applies
# it as root at every boot: anything but the panel's line is worth a look.
if [ -f /etc/tmpfiles.d/monopanel-php.conf ] && grep -Ev '^(#|$)' /etc/tmpfiles.d/monopanel-php.conf | grep -qv "^d ${rundir:-/run/monopanel}/php 0755 root root -\$"; then
	echo "warning: /etc/tmpfiles.d/monopanel-php.conf holds more than the line the panel wrote there, and the panel no longer uses it: check what it makes as root at boot, then remove it" >&2
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
systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true
# The panel's HTTPS port belongs to monopanel-api.socket: PID 1 keeps it bound
# while the API restarts, so no local account can take it in between. Its
# address follows web.listen in config.yaml (before mp setup there is none,
# and the unit's own 8443 stands). Never fatal: without the socket the API
# listens on the port itself. moved: the address in the drop-in changed, which
# a socket that is listening does not follow by itself.
dropin=/etc/systemd/system/monopanel-api.socket.d/listen.conf
moved=0
if [ -f /etc/monopanel/config.yaml ]; then
	before=$(cat "$dropin" 2>/dev/null || true)
	/usr/bin/monopanel api-socket-sync || echo "warning: monopanel-api.socket did not follow web.listen; run mp setup" >&2
	[ "$(cat "$dropin" 2>/dev/null || true)" = "$before" ] || moved=1
fi

# On an upgrade the panel is already running the old binary: restart it, or the
# new version would only take effect at the next reboot. dpkg passes the
# previous version in $2, rpm passes 2 as the number of installed packages.
upgrade=0
[ -n "${2:-}" ] && upgrade=1
[ "${1:-}" = "2" ] && upgrade=1
if [ "$upgrade" = "1" ]; then
	if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then
		# The socket holds the port through the upgrade; the API restarts on it.
		systemctl try-restart monopanel-agent.service monopanel-api.service >/dev/null 2>&1 || true
	else
		# Up to 0.8.19 the API listened on the port itself, and the old one
		# still does; or the socket's address moved, which a socket that is
		# listening does not follow. The socket can bind the port only once no
		# API process holds it, and systemd refuses to start a socket whose
		# service runs. So the socket restarts in one go with the API: systemd
		# orders it after the old API's exit and before the new one's start,
		# and the port is never free for as long as a separate stop of the API
		# and start of the socket would leave it. An API that is not running
		# stays so, with its socket listening.
		if systemctl -q is-active monopanel-api.service; then
			systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true
		else
			systemctl restart monopanel-api.socket >/dev/null 2>&1 || true
			systemctl try-restart monopanel-agent.service >/dev/null 2>&1 || true
		fi
	fi
	echo "MonoPanel updated."
else
	echo "MonoPanel installed. Run: mp setup"
fi
