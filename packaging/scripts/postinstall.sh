#!/bin/sh
set -e
systemd-sysusers /usr/lib/sysusers.d/monopanel.conf >/dev/null 2>&1 || true
# mp setup up to 0.8.19 wrote the run directory into /etc/tmpfiles.d without
# the sticky bit, and that copy wins over the package's at boot. Nor did it
# make php/ in it, where the php-fpm masters make the pools' sockets as root:
# the API had the agent write monopanel-php.conf for that, which it no longer
# can. The line goes into the copy, for the run directory it names, so that
# php/ is root's from boot, before the API runs. monopanel-php.conf stays:
# going back below 0.8.20, whose mp setup writes the copy without the line,
# php/ is made at boot by that file alone — the preremove writes it then
# where it is missing — and one line for one path in two files is no warning
# to systemd-tmpfiles (only lines that differ are).
tmpfiles=/etc/tmpfiles.d
if [ -f "$tmpfiles/monopanel.conf" ]; then
	sed -i 's#^d /run/monopanel 0771 root monopanel -$#d /run/monopanel 1771 root monopanel -#' "$tmpfiles/monopanel.conf" || true
	rundir=$(sed -n 's#^d \(/[^ ]*\) [0-7]* root [^ ]* -$#\1#p' "$tmpfiles/monopanel.conf" | grep -v '/php$' | head -n 1)
	if [ -n "$rundir" ] && ! grep -q "^d $rundir/php " "$tmpfiles/monopanel.conf"; then
		[ -z "$(tail -c 1 "$tmpfiles/monopanel.conf")" ] || echo >>"$tmpfiles/monopanel.conf" || true
		echo "d $rundir/php 0755 root root -" >>"$tmpfiles/monopanel.conf" || true
	fi
fi
# The API could rewrite that file up to 0.8.19, and systemd-tmpfiles applies
# it as root at every boot: anything but the panel's line is worth a look.
if [ -f "$tmpfiles/monopanel-php.conf" ] && grep -Ev '^(#|$)' "$tmpfiles/monopanel-php.conf" | grep -qv "^d ${rundir:-/run/monopanel}/php 0755 root root -\$"; then
	echo "warning: $tmpfiles/monopanel-php.conf holds more than the line the panel wrote there: check what it makes as root at boot, and leave the panel's line alone in it (d ${rundir:-/run/monopanel}/php 0755 root root -), which a version older than 0.8.20 needs" >&2
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
# sites/ is made only when it is missing, so that one somebody else made is
# not handed to root (the agent takes it over and clears it at its start).
# Up to 0.8.19 the directory was monopanel's. One that is somebody else's is
# left to the agent, which takes it over at its start through the open
# directory — owner, ACLs, then mode — before it makes or trusts sites/
# (openLogDir): install -d changes owner and mode only, and a default ACL its
# old owner left on it would reach sites/ and every log made there.
if [ "$(stat -c %u /var/log/monopanel 2>/dev/null || echo 0)" = "0" ]; then
	install -d -m 1771 -o root -g monopanel /var/log/monopanel
	[ -e /var/log/monopanel/sites ] || [ -L /var/log/monopanel/sites ] || install -d -m 0711 -o root -g root /var/log/monopanel/sites
fi

# On an upgrade the panel is already running the old binary: restart it, or the
# new version would only take effect at the next reboot. dpkg passes the
# previous version in $2, rpm passes 2 as the number of installed packages.
upgrade=0
[ -n "${2:-}" ] && upgrade=1
[ "${1:-}" = "2" ] && upgrade=1
# reinstall=1: the package comes back after a removal, which disabled and
# stopped the panel and left this mark (preremove.sh). dpkg names the removed
# version then, as it names the replaced one on an upgrade.
removed=/var/lib/monopanel-agent/package-removed
reinstall=0
[ -e "$removed" ] && reinstall=1

# mp setup up to 0.8.19 copied the panel's units into /etc/systemd/system on
# every host it set up, a removal or a downgrade below 0.8.20 keeps Apache's
# template there (preremove.sh), and mp setup of an install without the
# package writes the modprobe rule into /etc/modprobe.d: such a copy
# overrides the package's file for good, and what a release changes in it
# never arrives. The copies that are provably the panel's own and no newer
# than the package's go; units that run take the package's at their restart
# below. Never fatal.
/usr/bin/monopanel prune-unit-copies || echo "warning: monopanel prune-unit-copies failed; copies of the panel's units in /etc/systemd/system, or of its modprobe rule in /etc/modprobe.d, may still override the package's" >&2
systemctl daemon-reload >/dev/null 2>&1 || true

# A fresh install enables the panel, which mp setup starts, and so does an
# install after a removal: the removal turned the units off, not the
# administrator. An upgrade leaves enabled and disabled units as the
# administrator left them — with one exception, the socket of the panel's
# port the first time round: it came with 0.8.20, and the drop-in it gets
# below is not there yet. It follows the API then, enabled only with it.
dropin=/etc/systemd/system/monopanel-api.socket.d/listen.conf
if [ "$upgrade" = "0" ] || [ "$reinstall" = "1" ]; then
	systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true
elif [ ! -e "$dropin" ] && systemctl -q is-enabled monopanel-api.service 2>/dev/null; then
	systemctl enable monopanel-api.socket >/dev/null 2>&1 || true
fi
rm -f "$removed" 2>/dev/null || true

# The panel's HTTPS port belongs to monopanel-api.socket: PID 1 keeps it bound
# while the API restarts, so no local account can take it in between. Its
# address follows web.listen in config.yaml (before mp setup there is none,
# and the unit's own 8443 stands). Never fatal: without the socket the API
# listens on the port itself. moved: the address in the drop-in changed, which
# a socket that is listening does not follow by itself.
moved=0
if [ -f /etc/monopanel/config.yaml ]; then
	before=$(cat "$dropin" 2>/dev/null || true)
	/usr/bin/monopanel api-socket-sync || echo "warning: monopanel-api.socket did not follow web.listen; run mp setup" >&2
	[ "$(cat "$dropin" 2>/dev/null || true)" = "$before" ] || moved=1
fi

if [ "$upgrade" = "1" ]; then
	if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then
		# The socket holds the port through the upgrade; the API restarts on it.
		systemctl try-restart monopanel-agent.service monopanel-api.service >/dev/null 2>&1 || true
	elif systemctl -q is-active monopanel-api.service; then
		# Up to 0.8.19 the API listened on the port itself, and the old one
		# still does; or the socket's address moved, which a socket that is
		# listening does not follow. The socket can bind the port only once no
		# API process holds it, and systemd refuses to start a socket whose
		# service runs. So the socket restarts in one go with the API: systemd
		# orders it after the old API's exit and before the new one's start,
		# and the port is never free for as long as a separate stop of the API
		# and start of the socket would leave it.
		systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true
	else
		# The API is not running: the administrator stopped it, or it failed.
		# It stays down, and so does a socket that was not listening — started
		# now, it would start the API at the first connection to the port.
		# One that listens (on an address that moved) restarts to follow it.
		systemctl try-restart monopanel-api.socket monopanel-agent.service >/dev/null 2>&1 || true
	fi
fi
# After a removal nothing of the panel runs: mp setup starts it.
if [ "$upgrade" = "1" ] && [ "$reinstall" = "0" ]; then
	echo "MonoPanel updated."
else
	echo "MonoPanel installed. Run: mp setup"
fi
