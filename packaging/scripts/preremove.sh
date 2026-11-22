#!/bin/sh
# Stop the panel when the package is being removed — but not when it is being
# replaced by another version: postinstall restarts the units then.
# dpkg passes remove/upgrade/deconfigure, rpm passes 0 for the last removal.
# The socket goes too: left listening, it would start the API again at the
# next connection.
#
# Nothing here may fail the transaction: a failing prerm leaves dpkg with a
# package it can neither upgrade nor remove cleanly, and rpm with both
# versions installed. So no set -e, and the script ends with exit 0.

# Where the units are, the panel's binary, the mark a removal leaves for the
# next install (postinstall.sh) in the agent's directory, which only root
# writes, the configurations of the accounts' Apaches
# (confcheck.ApacheInstanceConf), the mount points of the shell guests'
# shared folders (below the default www_root), the boot-time files of the
# run directory (mp setup's monopanel.conf, setup.tmpfilesConf) and postfix's
# main.cf, which names the mail policy.
units=/etc/systemd/system
packaged=/usr/lib/systemd/system
bin=/usr/bin/monopanel
removed=/var/lib/monopanel-agent/package-removed
accounts=/etc/apache2/monopanel/accounts
shares=/var/www/.shares
tmpfiles=/etc/tmpfiles.d
maincf=/etc/postfix/main.cf

# older=1: the version that replaces this one is older than 0.9.0. dpkg runs
# this before it unpacks that version and names it, "upgrade <version>"
# (0.9.0~ counts 0.9.0's own prereleases as 0.9.0; its development builds
# carry the tag before it, 0.8.19~<n>-g<commit>, and count as older, as
# updater.Predates counts them). rpm runs it once the other version's files
# and its %post are in place and names nothing, so the binary tells: every
# version from 0.9.0 on answers the probe, an older one knows no such
# command — except a development build of 0.9.0 made since the probe came,
# which passes for 0.9.0 here. removal=1: nothing replaces it — though an
# older version may be installed next, after a removal (dnf remove, then dnf
# install), which runs no script of this one.
older=0
removal=0
case "${1:-}" in
remove | purge | 0)
	# The mail policy goes with the binary it runs, and postfix, told to
	# keep mail waiting while it is down (default_action=tempfail), would
	# keep every message waiting for good: main.cf loses it, and postfix
	# reloads, before its units stop (postremove.sh does the same after an
	# older version replaced this one).
	if [ -f "$maincf" ] && grep -Eq 'unix:[^ ,]*/monopanel-mailpolicy|inet:127\.0\.0\.1:8890' "$maincf"; then
		sed -i -E 's#\{ (unix:[^ ,]*/monopanel-mailpolicy|inet:127\.0\.0\.1:8890), default_action=tempfail \}(, )?##' "$maincf" || true
		postfix reload >/dev/null 2>&1 || true
	fi
	systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service monopanel-mailpolicy.service monopanel-mailpolicy-unix.socket monopanel-mailpolicy-inet.socket monopanel-mailguard.service >/dev/null 2>&1 || true
	# The removal turned the panel off, not the administrator. dpkg gives a
	# package installed again over what the removal left the removed version,
	# as it gives an upgrade the replaced one, and an upgrade enables nothing:
	# the mark has that install enable the panel, as a fresh one does.
	touch "$removed" 2>/dev/null || true
	removal=1
	;;
upgrade)
	if [ -n "${2:-}" ] && dpkg --compare-versions "$2" lt 0.9.0~ >/dev/null 2>&1; then
		older=1
	fi
	;;
[1-9]*)
	"$bin" api-socket-sync --probe >/dev/null 2>&1 || older=1
	;;
esac
[ "$older" = 1 ] || [ "$removal" = 1 ] || exit 0

# Going back below 0.9.0, or the package going: what comes after does not
# know what 0.9.0 changed on this host, and what the panel set up keeps
# running without it. The downgrade or the removal goes ahead — refusing it
# here would only wedge the package manager — and what it keeps from
# breaking is kept here; what it breaks, and how to mend it, is said.
#
# php/ in the run directory, where the php-fpm masters make the pools'
# sockets as root, has to be there at boot. 0.9.0 makes it from the line in
# mp setup's copy (/etc/tmpfiles.d/monopanel.conf), which an older version's
# mp setup writes without it, and the older version itself makes php/ only
# when it installs PHP or applies a site: without monopanel-php.conf — which
# a host whose PHP came with 0.9.0 never had — every php-fpm master fails
# at the next boot, and every PHP site answers 502. After a removal for good
# the sites' pools stay, and need it all the same. One line for one path in
# two files is no warning to systemd-tmpfiles. The run directory is the one
# the copy names, the default without one; a file of that name already
# there is left as it is. A removal from a host mp setup never set up (no
# copy) writes nothing: no pool runs there, and the file would outlive the
# package for nothing — an older version installed next writes it itself
# when it installs PHP.
rundir=$(sed -n 's#^d \(/[^ ]*\) [0-7]* root [^ ]* -$#\1#p' "$tmpfiles/monopanel.conf" 2>/dev/null | grep -v '/php$' | head -n 1)
rundir=${rundir:-/run/monopanel}
phpline="d $rundir/php 0755 root root -"
php=$tmpfiles/monopanel-php.conf
setup=0
[ -f "$tmpfiles/monopanel.conf" ] && setup=1
phpmade=0
if { [ "$removal" = 0 ] || [ "$setup" = 1 ]; } && [ ! -e "$php" ] && [ ! -L "$php" ]; then
	if printf '%s\n' "$phpline" >"$tmpfiles/.monopanel-php.conf.new" 2>/dev/null && mv -f "$tmpfiles/.monopanel-php.conf.new" "$php" 2>/dev/null; then
		phpmade=1
	else
		rm -f "$tmpfiles/.monopanel-php.conf.new" 2>/dev/null || true
	fi
fi
phpkept=0
grep -qxF "$phpline" "$php" 2>/dev/null && phpkept=1

# The accounts' Apaches (Debian and Ubuntu): the older package ships no unit
# template for them, and the removal of this one takes it away — the
# instances would find no unit at the next boot, and the sites in Apache
# mode would answer 502. A copy in /etc/systemd/system keeps them; mp setup
# and the next upgrade remove it once the package's own is back
# (monopanel prune-unit-copies).
apache=0
for f in "$accounts"/*/httpd.conf; do
	[ -f "$f" ] && apache=1 && break
done
tmpl=monopanel-apache@.service
if [ "$apache" = 1 ] && [ -f "$packaged/$tmpl" ] && [ ! -e "$units/$tmpl" ] && [ ! -L "$units/$tmpl" ]; then
	if cp "$packaged/$tmpl" "$units/.$tmpl.new" 2>/dev/null && mv -f "$units/.$tmpl.new" "$units/$tmpl" 2>/dev/null; then
		systemctl daemon-reload >/dev/null 2>&1 || true
	else
		rm -f "$units/.$tmpl.new" 2>/dev/null || true
	fi
fi

# The apps whose env file mp app-exec reads: the older binary has no such
# command, nor is there one after a removal, and they fail at their next
# start. Named as mp app knows them, from the Description= the panel writes
# ("MonoPanel app <login>/<name>").
apps=""
for f in "$units"/monopanel-app-*.service; do
	[ -f "$f" ] || continue
	grep -qF "ExecStart=$bin app-exec " "$f" 2>/dev/null || continue
	app=$(sed -n 's#^Description=MonoPanel app \([^/ ]*/[^ ]*\).*#\1#p' "$f" 2>/dev/null | head -n 1)
	apps="$apps ${app:-${f##*/}}"
done

if [ "$removal" = 1 ]; then
	# phpgap: a host mp setup set up where the line could not be kept — the
	# file holds something else, or /etc/tmpfiles.d took no file.
	phpgap=0
	[ "$setup" = 1 ] && [ "$phpkept" = 0 ] && phpgap=1
	[ -n "$apps" ] || [ "$apache" = 1 ] || [ "$phpmade" = 1 ] || [ "$phpgap" = 1 ] || exit 0
	{
		echo "MonoPanel: the package goes; what the panel set up on this host keeps running, but:"
		if [ -n "$apps" ]; then
			echo "  - apps that read an env file start through 'monopanel app-exec', which goes with the"
			echo "    package, and fail at their next start:$apps"
			echo "    (installing the panel again brings them back; otherwise remove them:"
			echo "    systemctl disable --now monopanel-app-<login>-<name>);"
		fi
		if [ "$apache" = 1 ]; then
			echo "  - sites in Apache mode run on their accounts' Apaches (monopanel-apache@<login>);"
			if [ -f "$units/$tmpl" ]; then
				echo "    their unit template is in $units/$tmpl, so they keep running"
				echo "    (systemctl disable --now monopanel-apache@<login> stops one for good);"
			else
				echo "    their unit template goes with the package, and they do not start at the next boot;"
			fi
		fi
		if [ "$phpmade" = 1 ]; then
			echo "  - $php makes $rundir/php at boot, where the sites'"
			echo "    php-fpm pools listen."
		elif [ "$phpgap" = 1 ]; then
			echo "  - the sites' php-fpm pools listen in $rundir/php, which the mp setup of a version older"
			echo "    than 0.9.0 leaves out of $tmpfiles/monopanel.conf: should one be installed next, put"
			echo "    \"$phpline\" into $php."
		fi
		echo "Should an older version be installed next, see \"Going back to an older version\":"
		echo "https://monopanel.app/en/docs/update/"
	} >&2
	exit 0
fi

{
	echo "MonoPanel: the version being installed${2:+ ($2)} is older than 0.9.0 and does not know what"
	echo "0.9.0 changed on this host:"
	echo "  - the site logs are in <log_dir>/sites (/var/log/monopanel/sites), while its logrotate and"
	echo "    fail2ban configuration watch the accounts' data/logs: nothing rotates or watches them;"
	echo "  - php-fpm makes the pools' sockets in $rundir/php, which its mp setup leaves out of"
	if [ "$phpkept" = 1 ]; then
		echo "    $tmpfiles/monopanel.conf: $php makes it at boot, keep that file;"
	else
		echo "    $tmpfiles/monopanel.conf, and every php-fpm fails to start after a reboot: put"
		echo "    \"$phpline\" into $php;"
	fi
	if [ "$apache" = 1 ]; then
		echo "  - sites in Apache mode run on their accounts' Apaches (monopanel-apache@<login>), and the"
		echo "    distribution's apache2 is off, which is where the older panel sends a site it applies;"
		if [ -f "$units/$tmpl" ]; then
			echo "    their unit template is in $units/$tmpl, so they keep running;"
		else
			echo "    their unit template goes with this package, and they do not start at the next boot;"
		fi
	fi
	if [ -n "$apps" ]; then
		echo "  - apps that read an env file start through 'monopanel app-exec', which it lacks, and fail"
		echo "    at their next start:$apps"
	fi
	if [ -f "$maincf" ] && grep -Eq 'unix:[^ ,]*/monopanel-mailpolicy|inet:127\.0\.0\.1:8890' "$maincf"; then
		echo "  - its mail server runs without the panel's mail policy, which it does not have: postfix"
		echo "    stops asking it once this version's files are gone, and an account can send as another"
		echo "    account's domain again (an upgrade to 0.9.0 or later puts the policy back);"
	fi
	echo "Once it runs, as root:"
	if [ "$apache" = 1 ]; then
		echo "  mp stack install apache             # the shared Apache again, before the sites"
	fi
	echo "  mp site apply <domain>              # every site (mp site list): logs, pool, Apache"
	echo "  mp mail apply                       # if the webmail has a port of its own"
	if [ -n "$apps" ]; then
		echo "  mp app set <name> --user <login>    # every app above"
	fi
	if [ "$apache" = 1 ]; then
		echo "  systemctl disable --now monopanel-apache@<login>   # each account's, once its sites are applied"
	fi
	if [ -d "$shares" ]; then
		echo "Shared folders stay mounted until a reboot — the shell guests' in $shares, reachable from"
		echo "their homes, though the older panel shows them unmounted — and after one none is: the older"
		echo "panel's mp site fix <domain> mounts the SFTP-only guests' again, the shell guests' it cannot:"
		echo "leave them be until the next upgrade."
	fi
	echo "See \"Going back to an older version\": https://monopanel.app/en/docs/update/"
} >&2
exit 0
