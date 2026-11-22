#!/bin/sh
# After this package's files went — a removal, or another version that
# replaced this one: postfix must not keep waiting for a mail policy nobody
# runs. main.cf has postfix keep mail waiting while the policy is down
# (default_action=tempfail), so once the binary that stays knows no policy
# — the package is gone, or a version older than 0.9.0 replaced this one —
# the panel's exact token leaves main.cf, postfix reloads, and what is left
# of the policy's units stops. That version's own mp mail apply would write
# main.cf without the policy too, but only when somebody runs it; until then
# every message, inbound mail too, would get 451 as soon as the policy's
# last process ended. Nothing to do where the binary knows the policy (an
# upgrade, or a version that has it) or where main.cf does not name it.
#
# dpkg passes remove/purge/upgrade <version> (and abort-*/failed-upgrade,
# which change nothing here), rpm the number of packages left: 0 for the
# last removal, 1 when another version replaced this one. dpkg runs this
# after it unpacked the other version and before it removes this one's
# files; rpm after both. Nothing here may fail the transaction: no set -e,
# and the script ends with exit 0.

bin=/usr/bin/monopanel
maincf=/etc/postfix/main.cf
policy='unix:[^ ,]*/monopanel-mailpolicy|inet:127\.0\.0\.1:8890'

case "${1:-}" in
remove | purge | upgrade | [0-9]*) ;;
*) exit 0 ;;
esac
# The binary that stays: one that knows the policy keeps it (an older one
# knows no such command and fails, as does a binary that is gone).
"$bin" mail-policy --exists >/dev/null 2>&1 && exit 0
[ -f "$maincf" ] && grep -Eq "$policy" "$maincf" || exit 0
sed -i -E "s#\{ ($policy), default_action=tempfail \}(, )?##" "$maincf" || true
postfix reload >/dev/null 2>&1 || true
systemctl disable --now monopanel-mailpolicy.service monopanel-mailpolicy-unix.socket monopanel-mailpolicy-inet.socket >/dev/null 2>&1 || true
echo "MonoPanel: postfix no longer asks the panel's mail policy, which the panel installed now does not have (main.cf: the policy left smtpd_milters and non_smtpd_milters)." >&2
exit 0
