#!/usr/bin/env bash
# checks.sh <vm name>
#
# The hardening of 0.8.20 as it shows on a testbed VM, checked as root.
# testbed.sh checks copies this file and doctor-expected.sh to /root/tb-checks/
# and runs it: the matrix after the e2e tests, testbed.sh upgrade after the
# package upgrade (with TB_CARRIED=1).
#
# Every check prints one line — PASS, FAIL, SKIP (does not apply to this host),
# EXPECTED (a shortfall doctor-expected.sh names for this host, with why) or
# INFO — and TOTAL closes the run; the exit code is 1 when anything failed.
#
#   panel         mp, the API and the agent answer, on the same build
#   carried       TB_CARRIED=1: what upgrade-seed.sh made with the previous
#                 release came along (shares moved, the Apache site on its
#                 account's Apache, the old backup restorable); the units of
#                 0.8.19's shares taken over, each once, none left, records
#                 for the folders; the folder its owner swapped for a symlink
#                 that 0.8.19's unit mounted by path taken off, failed, named
#                 by the doctor, and mounted again once the swap is undone
#   api socket    monopanel-api.socket holds the port: the API serves on it,
#                 the port stays bound through restarts, nobody cannot take it
#                 meanwhile, a connection starts a stopped API
#   unit policy   mp service refuses debug-shell, template instances and
#                 targets, and still acts on nginx
#   shares        a guest with a shell has its folder below <www>/.shares and a
#                 symlink to it, an SFTP-only guest in its chroot; a switch
#                 between the two moves it, and the agent's records with it;
#                 no unit left in a home. The agent binds them itself: no mount
#                 unit file, a root 0600 record per folder, a peer group of
#                 its own (a tmpfs below the guest's mount point shows neither
#                 below the owner's folder nor in php-fpm's and nginx's
#                 namespaces), every thread of the agent in its own namespace;
#                 straight at the agent's socket a mount unit, a start of the
#                 share's mount unit, a bind of the restore areas (no home)
#                 and of a folder its owner swapped for a symlink (for the
#                 symlink) are refused (403), mp site fix marks such a share
#                 failed, leaves the symlink's target alone and mounts the
#                 share again once undone; a folder in use is not unmounted;
#                 the doctor's line is ok
#   apache        Debian/Ubuntu: an Apache per account, apache2 off, www-data
#                 out of the web group, an .htaccess reaches nothing of another
#                 account, requests straight to an account's Apache get 403
#                 (with Satisfy Any and a nested <If> too), PHP sees no secret
#   php-fpm       the distribution's [www] pool idle: nobody, a root-only socket
#   composer      refuses root, runs as an account
#   repositories  the panel's vendor sources are https
#   restic        a backup through the panel, restored aside, a repository
#                 directory holding other files refused
#   restore nodes a snapshot with a FIFO and a set-uid file: restores in place
#                 and aside that would bring them back are refused naming the
#                 node, with nothing staged; a subtree beside them restores
#   package files the tmpfiles lines of the run directory (monopanel-php.conf
#                 kept after an upgrade), no "Duplicate line"; no copy of the
#                 panel's units in /etc/systemd/system; the package's modprobe
#                 rule alone, and sctp cannot load (from a bare binary: mp
#                 setup's own units, rule and monopanel-php.conf)
#   doctor        restic sandbox ok (or EXPECTED), the API's own api socket
#                 line (counted once), no failure, no warning
#                 doctor-expected.sh does not list
#   package runs  the deploy's installs ran in monopanel-pkg-<run>.service
#                 units (with none in the journal, after an upgrade, an index
#                 refresh does — after the doctor, which must judge the index
#                 as it was), none left; the daily upgrade on at 04:00 with a
#                 next run; one version of each PHP branch everywhere
#   selinux       EL: no AVC denial since boot
#
# What the checks make goes again at the end, also after a failure: the
# accounts tbcka, tbckb, tbcksh and tbcksf with their sites and shares (and
# what they put in their homes: the swapped folder's target, the FIFO and the
# set-uid file), the tmpfs mounts tbck-tmpfs, the backup targets tbck and
# tbckmix with /var/backups/tbck, the snapshots restored aside, composer when
# the checks installed it. Leftovers of a run that was cut short go first.
# What upgrade-seed.sh made (tbup*) stays. The passwords of the accounts and
# repositories are random, go to mp on stdin and are printed nowhere.
#
# shellcheck disable=SC2012,SC2016,SC2329 # ls -l only in messages; the awk, python and sh -c programs are single-quoted on purpose; wait_until runs functions by name
set -u
vm=${1:?usage: checks.sh <vm name>}
here=$(cd "$(dirname "$0")" && pwd)
export MP_LANG=en
# shellcheck disable=SC1091
. /etc/os-release
deb=0
case " $ID ${ID_LIKE:-} " in *debian* | *ubuntu*) deb=1 ;; esac
top() { sed -n "s/^$1: *//p" /etc/monopanel/config.yaml 2>/dev/null | tr -d "\"'" | head -1; }
www=$(top www_root)
www=${www:-/var/www}
run=$(top run_dir)
run=${run:-/run/monopanel}
webgroup=$(top web_group)
webgroup=${webgroup:-monopanel-web}
ip=$(hostname -I | cut -d' ' -f1)
kernel=$(uname -r)
context=deploy
[ -z "${TB_CARRIED:-}" ] || context=upgrade
restore_root=/var/lib/monopanel-agent/restore
# conf <section> <key>: a key of a section of config.yaml, "" when unset
conf() { awk -v s="$1:" -v k="$2:" '/^[^ #]/ { in_s = ($1 == s) } in_s && $1 == k { print $2; exit }' /etc/monopanel/config.yaml 2>/dev/null | tr -d "\"'"; }
agent_sock=$(conf agent socket)
agent_sock=${agent_sock:-$run/agent.sock}
agent_state=$(conf agent state_dir)
agent_state=${agent_state:-/var/lib/monopanel-agent}
# the web root as systemd names the mount units below it (var-www)
wesc=$(systemd-escape --path "$www")
umask 077
tmp=$(mktemp -d /root/tb-checks.XXXXXX) || exit 2

# The accounts, sites and targets the checks make for themselves.
A=tbcka B=tbckb SH=tbcksh SF=tbcksf SHARE=froma
DA=$A.$vm.test     # A's site on php-fpm; its folder shared/ goes to both guests
AA=apa.$A.$vm.test # A's site in Apache mode (Debian, Ubuntu)
BA=apa.$B.$vm.test # B's, the other account's
docA=$www/$A/data/www/$DA
docAA=$www/$A/data/www/$AA
docBA=$www/$B/data/www/$BA
probe='<?php echo "tbck-ok uid=", function_exists("posix_getuid") ? posix_getuid() : "?", " ", isset($_SERVER["HTTP_X_MONOPANEL_BACKEND"]) ? "LEAK" : "nosecret", " ", isset($_SERVER["SCRIPT_URI"]) ? "uri" : "nouri", "\n";'
victim=$www/$SH/data/tbck-victim # another account's directory A points its folder at
nodes=$www/$A/data/tbck-nodes    # a FIFO and a set-uid file of A's, for the restores
sub=tbck-sub                     # where the shell guest mounts a tmpfs inside its folder
objects=0 shares=0 apache=0 apache_sites=0 composer_ours=0 cleaned=0 backed=0
restored=()
area="" # what the web root's restore area holds before a restore

# ---------------------------------------------------------------- output ----
npass=0 nfail=0 nskip=0 nexp=0
pass() { echo "PASS      $*"; npass=$((npass + 1)); }
fail() { echo "FAIL      $*"; nfail=$((nfail + 1)); }
skip() { echo "SKIP      $*"; nskip=$((nskip + 1)); }
expected() { echo "EXPECTED  $*"; nexp=$((nexp + 1)); }
info() { echo "INFO      $*"; }
section() { printf '\n== %s\n' "$*"; }
# verdict <what> <command...>: PASS when the command succeeds, FAIL when not
verdict() {
	local what=$1
	shift
	if "$@" </dev/null >/dev/null 2>&1; then pass "$what"; else fail "$what"; fi
}
total() {
	echo
	echo "TOTAL pass=$npass fail=$nfail skip=$nskip expected=$nexp"
	[ "$nfail" -eq 0 ] && exit 0
	exit 1
}

# --------------------------------------------------------------- helpers ----
# wait_until <seconds> <command...>: true as soon as the command is
wait_until() {
	local left=$1
	shift
	until "$@" </dev/null >/dev/null 2>&1; do
		[ "$left" -gt 0 ] || return 1
		sleep 2
		left=$((left - 2))
	done
}
# last <file> [n]: its last lines on one line, for a message
last() { tail -n "${2:-1}" "$1" 2>/dev/null | tr '\n' ' ' | cut -c1-300; }
rand() { head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
secret() { printf '%sAa1-\n' "$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')"; }
# pj <python expression over d>: a value out of the JSON on stdin, "" on any error
pj() {
	python3 -c 'import json, sys
try:
    d = json.load(sys.stdin)
    v = eval(sys.argv[1], {"d": d})
except Exception:
    v = ""
if isinstance(v, bool):
    v = "true" if v else "false"
elif v is None:
    v = ""
elif isinstance(v, (list, tuple)):
    v = "\n".join(str(x) for x in v)
print(v)' "$1"
}
# as_user <login> <command...>: runs it as the account (its groups, from /)
as_user() {
	local u=$1
	shift
	(cd / && setpriv --reuid="$(id -u "$u")" --regid="$(id -g "$u")" --init-groups -- "$@")
}
# put <owner> <path> <content>: a file of the account's
put() { printf '%s\n' "$3" | install -o "$1" -g "$(id -gn "$1")" -m 0644 /dev/stdin "$2"; }
# get <host> <path> [curl args]: the status code through nginx, the body in $tmp/body
get() {
	local h=$1 p=$2
	shift 2
	curl -s -m 20 -o "$tmp/body" -w '%{http_code}' -H "Host: $h" "$@" "http://$ip$p"
}
# tbup_answers <host>: upgrade-seed.sh's probe there answers 200 through nginx
tbup_answers() { [ "$(get "$1" /tbup.php)" = 200 ] && grep -q '^tbup-ok' "$tmp/body"; }
# mounted_at <path>: something is mounted at exactly that path — mountinfo
# as it is: findmnt would resolve a symlink there (the guest's) to its target
mounted_at() { awk -v m="$1" '$5 == m { found = 1 } END { exit !found }' /proc/self/mountinfo; }
# links_to <path> <target>: path is a symlink to exactly that target
links_to() { [ -L "$1" ] && [ "$(readlink "$1")" = "$2" ]; }
installed() { [ "$(mp stack list --json </dev/null 2>/dev/null | pj "[c[\"installed\"] for c in d if c[\"name\"] == \"$1\"][0]")" = true ]; }
listen_of() { awk '$1 == "Listen" {print $2; exit}' "/etc/apache2/monopanel/accounts/$1/httpd.conf" 2>/dev/null; }
# agent_call <path> <json> [seconds]: the HTTP status of a call straight to
# the agent's socket as root — what a compromised API could send it — with
# the answer in $tmp/agent
agent_call() {
	curl -s -m "${3:-60}" -o "$tmp/agent" -w '%{http_code}' --unix-socket "$agent_sock" -X POST -H 'Content-Type: application/json' --data-binary "$2" "http://agent$1" </dev/null 2>/dev/null
}
# agent_said: the agent's answer to the last call, one line
agent_said() { pj 'd.get("error", "") + (": " + d["output"] if d.get("output") else "")' <"$tmp/agent" | tr '\n' ' ' | cut -c1-200; }
# agent_error: the whole error of the agent's answer to the last call, for a
# look at why it refused
agent_error() { pj 'd.get("error", "")' <"$tmp/agent"; }
# jsn <python expression over a, the arguments>: JSON for the agent
jsn() { python3 -c 'import json, sys; a = sys.argv[2:]; print(json.dumps(eval(sys.argv[1])))' "$@"; }
# mount_ids <path>: the ids of the mounts at exactly that path, bottom first
mount_ids() { awk -v m="$1" '$5 == m { printf "%s ", $1 }' /proc/self/mountinfo; }
# mount_top <path>: the mountinfo line of the mount on top at exactly that path
mount_top() { awk -v m="$1" '$5 == m { l = $0 } END { print l }' /proc/self/mountinfo; }
# mount_of <path>: the mountinfo line of the mount the path lies on
mount_of() { awk -v p="$1" 'BEGIN { best = -1 } { m = $5; if ((m == "/" || p == m || index(p, m "/") == 1) && length(m) >= best) { best = length(m); l = $0 } } END { print l }' /proc/self/mountinfo; }
# peer <mountinfo line>: its peer group (shared:N), "" for a mount in none
peer() { awk '{ for (i = 7; i <= NF && $i != "-"; i++) if ($i ~ /^shared:/) print $i }' <<<"$1"; }
# tbck_tmpfs: the mount points of the checks' own tmpfs, deepest first
tbck_tmpfs() { awk '{ for (i = 7; i < NF; i++) if ($i == "-") { if ($(i + 1) == "tmpfs" && $(i + 2) == "tbck-tmpfs") print $5; break } }' /proc/self/mountinfo | sort -r; }
unmount_tbck_tmpfs() {
	local m
	while IFS= read -r m; do
		[ -z "$m" ] || umount -l "$m" 2>/dev/null
	done < <(tbck_tmpfs)
}
# unit_files: the mount unit files below the web root in /etc/systemd/system
# and its .wants directories: systemd would mount them by path at boot
unit_files() {
	local f b
	for f in /etc/systemd/system/*.mount /etc/systemd/system/*.wants/*.mount; do
		[ -e "$f" ] || [ -L "$f" ] || continue
		b=${f##*/}
		[ "${b#"$wesc"-}" = "$b" ] || echo "${f#/etc/systemd/system/}"
	done
}
# record_of <mount point>: the agent's record of the folder it bound there
record_of() { echo "$agent_state/shares/$(systemd-escape --path "$1").share"; }
# share_state <guest> <name>: "<status> <last error>" of the share as the API keeps it
share_state() { mp user share list "$1" --json </dev/null 2>/dev/null | pj "[s[\"share\"].get(\"status\", \"\") + \" \" + \" \".join(s[\"share\"].get(\"last_error\", \"\").split()) for s in d if s[\"share\"][\"name\"] == \"$2\"][0]"; }
share_failed() { [ "$(share_state "$1" "$2" | cut -d' ' -f1)" = failed ]; }
# doctor_line <check>: "<status> <detail>" of one line of mp --json doctor
doctor_line() { mp --json doctor </dev/null 2>/dev/null | pj "[c[\"status\"] + \" \" + \" \".join(c.get(\"detail\", \"\").split()) for c in d[\"checks\"] if c[\"name\"] == \"$1\"][0]"; }
# settle: the panel and its agent answer and the job queue stays empty for
# five polls in a row. After a package upgrade or a restart the API answers
# at once, while the agent may still do its own start-up work for a minute
# and more; only once the agent answers does the API's start-up refresh run,
# and it queues jobs of its own (apache.migrate, site.apply) a few seconds
# later — an empty queue seen once proves nothing then.
settle() {
	local st q ok quiet=0
	for _ in $(seq 120); do
		if st=$(mp status --json </dev/null 2>/dev/null); then
			q=$(pj 'd["panel"]["jobs"].get("queued", 0) + d["panel"]["jobs"].get("running", 0)' <<<"$st")
			ok=$(pj 'd["panel"].get("agent_ok", False)' <<<"$st")
			if [ "$q" = 0 ] && [ "$ok" = true ]; then
				quiet=$((quiet + 1))
				[ "$quiet" -lt 5 ] || return 0
			else
				quiet=0
			fi
		else
			quiet=0
		fi
		sleep 2
	done
	return 1
}

# The warnings the doctor may give here (doctor-expected.sh).
ex_where=() ex_name=() ex_detail=() ex_why=()
expect() {
	ex_where+=("$1")
	ex_name+=("$2")
	ex_detail+=("$3")
	ex_why+=("$4")
}
if [ -r "$here/doctor-expected.sh" ]; then
	# shellcheck source-path=SCRIPTDIR source=doctor-expected.sh
	. "$here/doctor-expected.sh"
fi
# expected_why <check> <detail>: why doctor-expected.sh allows this warning on
# this host ("<vm> <kernel> <context>"); false when it does not
expected_why() {
	local i
	for i in "${!ex_name[@]}"; do
		[ "${ex_name[$i]}" = "$1" ] || continue
		[[ "$vm $kernel $context" =~ ${ex_where[$i]} ]] || continue
		[[ $2 =~ ${ex_detail[$i]} ]] || continue
		echo "${ex_why[$i]}"
		return 0
	done
	return 1
}
# rule_for <check>: whether doctor-expected.sh has a rule for it on this host
rule_for() {
	local i
	for i in "${!ex_name[@]}"; do
		[ "${ex_name[$i]}" = "$1" ] && [[ "$vm $kernel $context" =~ ${ex_where[$i]} ]] && return 0
	done
	return 1
}

# --------------------------------------------------------------- cleanup ----
# stale_restores: snapshots of the checks' own backups that a run cut short
# left restored aside — their ids went with it, the homes in them tell them;
# this run's own (restored) are not stale
stale_restores() {
	local d r
	for d in "$restore_root"/*; do
		[[ ${d##*/} =~ ^[0-9a-f]{8,64}$ ]] || continue
		[ -d "$d$www/$A" ] || [ -d "$d$www/tbupown" ] || continue
		for r in "${restored[@]}"; do
			[ "$r" != "$d" ] || continue 2
		done
		echo "$d"
	done
}
leftovers() {
	local u
	for u in "$A" "$B" "$SH" "$SF"; do
		mp user show "$u" </dev/null >/dev/null 2>&1 && return 0
	done
	[ -e /var/backups/tbck ] && return 0
	[ -n "$(stale_restores)" ] && return 0
	[ -n "$(tbck_tmpfs)" ] && return 0
	[ -n "$(mp backup target list --json </dev/null 2>/dev/null | pj '[t["name"] for t in d if t["name"] in ("tbck", "tbckmix")]')" ]
}
cleanup_objects() {
	local u t d
	# a tmpfs inside a shared folder, and a process of the guest's in it,
	# keep the folder busy: its unbind would fail
	unmount_tbck_tmpfs
	id "$SH" >/dev/null 2>&1 && pkill -u "$SH" -x sleep 2>/dev/null
	for u in "$SH" "$SF"; do
		mp user share rm "$u" "$SHARE" </dev/null >/dev/null 2>&1
	done
	for u in "$SH" "$SF" "$B" "$A"; do
		mp user show "$u" </dev/null >/dev/null 2>&1 || continue
		timeout 900 mp user rm "$u" --purge --yes </dev/null >"$tmp/rm" 2>&1 || info "cleanup: mp user rm $u --purge: $(last "$tmp/rm" 2)"
	done
	for t in tbck tbckmix; do
		mp backup target rm "$t" </dev/null >/dev/null 2>&1
	done
	rm -rf /var/backups/tbck
	for d in "${restored[@]}"; do
		case "$d" in "$restore_root"/?*) rm -rf "$d" ;; esac
	done
	restored=()
	while IFS= read -r d; do
		rm -rf "$d"
	done < <(stale_restores)
}
cleanup() {
	[ "$cleaned" = 0 ] || return 0
	cleaned=1
	cleanup_objects
	if [ "$composer_ours" = 1 ]; then
		timeout 600 mp stack remove composer </dev/null >"$tmp/rm" 2>&1 || info "cleanup: mp stack remove composer: $(last "$tmp/rm")"
	fi
}
trap 'cleanup; rm -rf "$tmp"' EXIT
trap 'echo "interrupted"; exit 130' INT TERM
# the ssh session went: clean up all the same, with nobody left to read it
trap 'exec >/dev/null 2>&1; exit 129' HUP PIPE

# ----------------------------------------------------------------- panel ----
panel_section() {
	section "panel"
	local st cli api agent
	if ! settle; then
		fail "the panel and its agent did not answer with an empty job queue within 4 minutes: $(mp status </dev/null 2>&1 | head -3 | tr '\n' ' ' | cut -c1-200)"
		return 1
	fi
	cli=$(mp version </dev/null 2>/dev/null | awk 'NR == 1 {print $2}')
	st=$(mp status --json </dev/null 2>/dev/null)
	api=$(pj 'd["panel"]["version"]' <<<"$st")
	agent=$(pj 'd["panel"].get("agent_version", "")' <<<"$st")
	if [ -n "$cli" ] && [ "$api" = "$cli" ] && [ "$agent" = "$cli" ]; then
		pass "mp, the API and the agent are $cli, the queue is empty"
	else
		fail "versions differ: mp ${cli:-?}, the API ${api:-?}, the agent ${agent:-?}"
	fi
	if [ ! -r "$here/doctor-expected.sh" ]; then info "no doctor-expected.sh next to this script: every doctor warning counts"; fi
	return 0
}

# --------------------------------------------------------------- carried ----
# carried_shares <docroot> <domain>: what the agent made of the previous
# release's shared folders at its first start (H19a): each mount unit taken
# over, once, as its journal says, and none left; a record for each folder;
# and the folder tbupown swapped for a symlink to a directory of tbupsh's,
# which the previous release's unit mounted by path in tbupsf's chroot
# (upgrade-seed.sh): taken off, the share failed with why, the doctor naming
# it. Then the swap is undone and mp site fix mounts the folder again — the
# end of the scenario, and what keeps the doctor's verdicts at the end free
# of a warning that only this planted folder gives (no rule for it in
# doctor-expected.sh). A run after the first finds the swap undone.
carried_shares() {
	local doc=$1 dom=$2 left u n mpt folder st order got=""
	left=$(unit_files)
	if [ -z "$left" ]; then
		pass "no mount unit of a shared folder left in /etc/systemd/system or its .wants directories"
	else
		fail "mount units below $www left after the upgrade: $(tr '\n' ' ' <<<"$left" | cut -c1-200)"
	fi
	[ -n "${units:-}" ] || fail "/root/tbup.env names no unit of the previous release's shared folders (units=): an upgrade-seed.sh older than these checks"
	for u in ${units:-}; do
		n=$(journalctl -u monopanel-agent --no-pager -o cat 2>/dev/null | grep 'shared folder was taken over' | grep -cF "unit=$u ")
		if [ "$n" = 1 ]; then
			pass "journalctl -u monopanel-agent: $u of the previous release taken over, once"
		else
			fail "journalctl -u monopanel-agent: $n lines 'taken over' for $u, not one"
		fi
	done
	record_check "$www/.shares/tbupsh/fromown" "$doc/shared"
	record_check "$www/tbupsf/fromown" "$doc/shared" taken
	if [ -z "${attack:-}" ]; then
		fail "/root/tbup.env names no folder swapped for a symlink (attack=): an upgrade-seed.sh older than these checks"
		return
	fi
	mpt=$www/tbupsf/$attack folder=$doc/$attack
	# the line numbers of the first "taken off" for it and of the first
	# "taken over": the mounts are checked before the slow removals
	order=$(journalctl -u monopanel-agent --no-pager -o cat 2>/dev/null | awk -v m="mount_point=$mpt " '/was taken off/ && index($0, m) && !off { off = NR } /shared folder was taken over/ && !over { over = NR } END { print off + 0, over + 0 }')
	if [ "${order% *}" = 0 ]; then
		fail "journalctl -u monopanel-agent: no line 'taken off' for $mpt, where the previous release's unit mounted the target of tbupown's symlink"
	elif [ "${order#* }" != 0 ] && [ "${order% *}" -gt "${order#* }" ]; then
		fail "journalctl -u monopanel-agent: the mount at $mpt was taken off after the units were taken over (lines $order)"
	else
		pass "journalctl -u monopanel-agent: the mount at $mpt, which showed another directory than the folder, taken off before the units went"
	fi
	if [ -L "$folder" ]; then
		if wait_until 240 share_failed tbupsf "$attack"; then
			st=$(share_state tbupsf "$attack")
			if grep -qi symlink <<<"$st"; then pass "tbupsf's $attack is failed: $(cut -c1-200 <<<"${st#failed }")"; else fail "tbupsf's $attack is failed, but not for the symlink: $(cut -c1-200 <<<"$st")"; fi
		else
			fail "tbupsf's $attack is not failed four minutes after the upgrade: $(share_state tbupsf "$attack" | cut -c1-200)"
		fi
		if ! mounted_at "$mpt" && [ ! -e "$mpt/tbup-victim.txt" ]; then
			pass "nothing mounted at $mpt, where the previous release's unit mounted tbupsh's directory through tbupown's symlink"
		else
			fail "$mpt after the upgrade: $(mount_top "$mpt" | cut -c1-160); tbup-victim.txt $([ -e "$mpt/tbup-victim.txt" ] && echo shows || echo does not show)"
		fi
		st=$(doctor_line "shared folders")
		if [ "${st%% *}" = warn ] && grep -qF "$attack" <<<"$st" && grep -qF tbupsf <<<"$st"; then
			pass "doctor: shared folders names it: $(cut -c1-200 <<<"${st#warn }")"
		else
			fail "doctor: shared folders: ${st:-no line}"
		fi
		if as_user tbupown sh -c 'rm "$1" && mv "$1.real" "$1"' sh "$folder" 2>"$tmp/err" && timeout 600 mp site fix "$dom" </dev/null >"$tmp/out" 2>&1; then
			info "tbupown undid the swap of $folder, and mp site fix $dom ran"
		else
			fail "undoing the swap of $folder and mp site fix $dom: $(last "$tmp/err") $(last "$tmp/out" 2)"
		fi
	else
		info "$folder is a directory again: an earlier run of these checks undid tbupown's swap after it checked what the upgrade made of it"
	fi
	if wait_until 60 mounted_at "$mpt"; then got=$(as_user tbupsf cat "$mpt/tbup-attack.txt" 2>&1); fi
	if [ "$(api_mount tbupsf "$attack")" = "$mpt true" ] && [ "$got" = "tbup-attack-${mark:-}" ]; then
		pass "the swap undone: tbupsf's $attack mounted at $mpt again, and tbupsf reads the folder"
	else
		fail "tbupsf's $attack with the swap undone: $(api_mount tbupsf "$attack"), reading: $(cut -c1-120 <<<"${got:-nothing mounted}")"
	fi
	record_check "$mpt" "$folder"
}
carried_section() {
	section "carried over from the previous release (upgrade-seed.sh)"
	if [ ! -r /root/tbup.env ]; then
		fail "/root/tbup.env is missing: upgrade-seed.sh did not run here"
		return
	fi
	local from mark snap units attack cli own=tbupown dom apa doc c st d got
	# shellcheck disable=SC1091
	. /root/tbup.env
	dom=$own.$vm.test apa=apa.$own.$vm.test doc=$www/$own/data/www/$own.$vm.test
	info "the site $dom with the folder shared/, the guests tbupsh and tbupsf, the target tbupbak$([ "$deb" = 1 ] && echo ", the Apache site $apa")"
	cli=$(mp version </dev/null 2>/dev/null | awk 'NR == 1 {print $2}')
	if [ "$cli" != "${from:-}" ]; then
		pass "upgraded by the package alone, without mp setup: $from → $cli"
	else
		fail "mp is still $cli, the release upgrade-seed.sh ran with"
	fi
	info "units: $(for u in monopanel-agent.service monopanel-api.service monopanel-api.socket; do printf '%s=%s ' "$u" "$(systemctl show -p FragmentPath --value "$u" 2>/dev/null)"; done)"
	info "drop-ins: $(systemctl show -p DropInPaths --value monopanel-api.service monopanel-api.socket 2>/dev/null | tr '\n' ' ')"

	share_checks tbupsh tbupsf fromown "$doc/shared" tbup-shared.txt
	carried_shares "$doc" "$dom"

	if [ "$deb" = 1 ]; then
		# how the move is carried out is the API's business (a job of its first
		# start today): what counts is where the site runs afterwards — given
		# the time the API's start-up refresh needs to queue and run the move
		if wait_until 300 systemctl is-active --quiet "monopanel-apache@$own.service"; then
			pass "monopanel-apache@$own.service runs the site $apa had on the shared Apache"
		else
			fail "monopanel-apache@$own.service: $(systemctl is-active "monopanel-apache@$own.service" 2>&1) five minutes after the upgrade"
		fi
		st=$(mp job list --json --limit 200 </dev/null 2>/dev/null | pj '[j["type"] + " " + j["status"] for j in d if j["type"].startswith("apache")]')
		info "Apache jobs since the upgrade: $(tr '\n' ',' <<<"${st:-none}" | sed 's/,$//')"
		if wait_until 120 tbup_answers "$apa"; then pass "$apa answers through nginx and the account's Apache"; else fail "$apa: $(get "$apa" /tbup.php) $(head -c 160 "$tmp/body")"; fi
		apache2_off
	fi

	if [ -n "${snap:-}" ]; then
		got=$(mp backup snapshots --target tbupbak --json </dev/null 2>/dev/null | pj '[s["id"] for s in d]')
		# restic up to 0.14 (Debian 12, Ubuntu 22.04) names a backup's snapshot
		# by its short id in the summary the run keeps; the listing has the
		# whole one, which starts with it (a hex id: no regex characters)
		if grep -q "^$snap" <<<"$got"; then pass "the snapshot ${snap:0:8} that $from made is in the repository"; else fail "the snapshot ${snap:0:8} of $from is not listed: $(tr '\n' ' ' <<<"$got" | cut -c1-200)"; fi
		d=$restore_root/$snap
		restored+=("$d")
		if timeout 1800 mp backup restore "$snap" --target tbupbak </dev/null >"$tmp/out" 2>&1; then
			if [ "$(cat "$d$doc/tbup-marker.txt" 2>/dev/null)" = "tbup-$mark" ]; then
				pass "that snapshot restored aside: the file from before the upgrade is back in $d"
			else
				fail "restored aside, but $d$doc/tbup-marker.txt is not what was backed up"
			fi
		else
			fail "mp backup restore ${snap:0:8} --target tbupbak: $(last "$tmp/out" 3)"
		fi
		if timeout 1800 mp backup run --scope "user:$own" --target tbupbak </dev/null >"$tmp/out" 2>&1; then
			pass "a new backup into the repository $from made"
		else
			fail "a backup into the repository of $from: $(last "$tmp/out" 3)"
		fi
	else
		fail "/root/tbup.env names no snapshot"
	fi
}

# ------------------------------------------------------------ api socket ----
socket_section() {
	section "the panel's port on monopanel-api.socket (H4)"
	local listen port host paddr via c pid sampler grabber
	listen=$(systemctl show -p Listen --value monopanel-api.socket 2>/dev/null)
	port=$(sed -nE 's/.*:([0-9]+) \(Stream\).*/\1/p' <<<"$listen" | head -1)
	port=${port:-8443}
	host=$(sed -nE 's/^\[?([^] ]*)\]?:[0-9]+ \(Stream\).*/\1/p' <<<"$listen" | head -1)
	case "$host" in '' | '::' | 0.0.0.0) paddr=127.0.0.1 ;; *:*) paddr="[$host]" ;; *) paddr=$host ;; esac
	health() { [ "$(curl -sk -o /dev/null -m 5 -w '%{http_code}' "https://$paddr:$port/api/v1/health")" = 200 ]; }

	if [ "$(systemctl is-enabled monopanel-api.socket 2>/dev/null)" = enabled ] && [ "$(systemctl is-active monopanel-api.socket 2>/dev/null)" = active ]; then
		pass "monopanel-api.socket enabled and active on ${listen:-?}"
	else
		fail "monopanel-api.socket: $(systemctl is-enabled monopanel-api.socket 2>&1), $(systemctl is-active monopanel-api.socket 2>&1), listening on ${listen:-nothing}"
	fi
	via=$(mp status --json </dev/null 2>/dev/null | pj 'd["panel"].get("listen_via", "")')
	if [ "$via" = socket ]; then pass "the API serves on the socket (listen_via=socket)"; else fail "the API serves on ${via:-?}, not on the socket (listen_via)"; fi
	info "journal: $(journalctl -u monopanel-api -b --no-pager 2>/dev/null | grep 'api listening' | tail -1 | cut -c1-220)"

	# listeners on the port every 20 ms while the API restarts
	(for _ in $(seq 150); do
		ss -ltnH "sport = :$port" 2>/dev/null | grep -c .
		sleep 0.02
	done) >"$tmp/held" &
	sampler=$!
	sleep 0.3
	systemctl restart monopanel-api.service
	wait "$sampler"
	if grep -qx 0 "$tmp/held"; then
		fail "port $port free in $(grep -cx 0 "$tmp/held") of 150 samples during systemctl restart monopanel-api"
	elif [ "$(grep -cE '^[0-9]+$' "$tmp/held")" -ne 150 ]; then
		# a sampler that did not run proves nothing either way
		fail "the port sampler took $(grep -cE '^[0-9]+$' "$tmp/held") of 150 samples: $(last "$tmp/held")"
	else
		pass "port $port bound in all 150 samples (20 ms apart) through systemctl restart monopanel-api"
	fi

	# an unprivileged process tries to take the port meanwhile
	cat >"$tmp/grab.py" <<'EOF'
import socket, sys, time
port, end, tries = int(sys.argv[1]), time.time() + float(sys.argv[2]), 0
while time.time() < end:
    tries += 1
    for fam, addr in ((socket.AF_INET6, "::"), (socket.AF_INET, "0.0.0.0")):
        try:
            s = socket.socket(fam, socket.SOCK_STREAM)
        except OSError:
            continue
        try:
            s.bind((addr, port))
            s.listen(1)
            print("GRABBED", addr, port, "after", tries, "tries")
            sys.exit(1)
        except OSError:
            pass
        finally:
            s.close()
print("could not bind %d in %d tries" % (port, tries))
EOF
	(cd / && setpriv --reuid="$(id -u nobody)" --regid="$(id -g nobody)" --clear-groups -- python3 - "$port" 14 <"$tmp/grab.py") >"$tmp/grab.out" 2>&1 &
	grabber=$!
	for _ in 1 2 3; do
		sleep 1
		systemctl restart monopanel-api.service
	done
	sleep 1
	systemctl restart monopanel-agent.service
	sleep 2
	pid=$(systemctl show -p MainPID --value monopanel-api.service 2>/dev/null)
	# MainPID is 0 while the API is down: kill -9 0 would hit this script
	if [ "${pid:-0}" -gt 1 ] 2>/dev/null; then kill -9 "$pid"; fi
	wait "$grabber"
	if grep -q GRABBED "$tmp/grab.out"; then
		fail "nobody took port $port: $(last "$tmp/grab.out")"
	elif grep -q '^could not bind' "$tmp/grab.out"; then
		pass "the user nobody could not take port $port through 3 API restarts, an agent restart and a kill -9 of the API: $(last "$tmp/grab.out")"
	else
		# setpriv or python3 failed: nothing tried to take the port
		fail "the bind loop as nobody did not run: $(last "$tmp/grab.out" 2)"
	fi
	if wait_until 60 health; then pass "the panel answers on https://$paddr:$port/ afterwards"; else fail "the panel does not answer on https://$paddr:$port/ a minute later"; fi

	# stopped, the API starts again with the first connection to its socket
	systemctl stop monopanel-api.service
	sleep 1
	if [ "$(systemctl is-active monopanel-api.service)" != active ] && [ "$(systemctl is-active monopanel-api.socket)" = active ]; then
		c=$(curl -sk -o /dev/null -m 30 -w '%{http_code}' "https://$paddr:$port/api/v1/health")
		if [ "$c" = 200 ] && [ "$(systemctl is-active monopanel-api.service)" = active ]; then
			pass "a stopped API is started by the first connection to its socket"
		else
			fail "after systemctl stop monopanel-api a connection got $c, the API is $(systemctl is-active monopanel-api.service 2>&1)"
		fi
	else
		fail "after systemctl stop monopanel-api: the API $(systemctl is-active monopanel-api.service 2>&1), the socket $(systemctl is-active monopanel-api.socket 2>&1)"
	fi
	systemctl start monopanel-api.service
	wait_until 60 mp status </dev/null
	via=$(mp status --json </dev/null 2>/dev/null | pj 'd["panel"].get("listen_via", "")')
	if [ "$via" = socket ]; then pass "after all of it the API serves on the socket still"; else fail "after the restarts the API serves on ${via:-?} (listen_via)"; fi
	# The agent restarted, and every start of the API runs its start-up
	# refresh (nginx and php-fpm reloads among it): the checks after this one
	# go on once the agent answers and that work is done.
	if ! settle; then
		fail "four minutes after the restarts the agent does not answer, or the job queue is not empty: $(mp status </dev/null 2>&1 | grep -i -m1 'agent' | cut -c1-160)"
	fi
}

# ----------------------------------------------------------- unit policy ----
# refused <what> <part of the reason> <mp args...>: mp says no, and why
refused() {
	local what=$1 why=$2 out rc
	shift 2
	out=$(timeout 120 mp "$@" </dev/null 2>&1)
	rc=$?
	if [ "$rc" -ne 0 ] && grep -q "is not allowed" <<<"$out" && grep -qi -- "$why" <<<"$out"; then
		pass "$what refused: $(grep -m1 'is not allowed' <<<"$out" | sed 's/^error: //' | cut -c1-200)"
	else
		fail "$what: exit $rc, $(tail -1 <<<"$out" | cut -c1-200)"
	fi
}
policy_section() {
	section "which units the panel acts on"
	local dccp=0
	lsmod 2>/dev/null | grep -q '^dccp ' && dccp=1
	refused "mp service start debug-shell" "root shell" service start debug-shell
	if [ "$(systemctl is-active debug-shell.service 2>/dev/null)" = active ]; then
		systemctl stop debug-shell.service
		fail "debug-shell.service was started (stopped again)"
	fi
	refused "mp service start modprobe@dccp.service" "template unit" service start modprobe@dccp.service
	if [ "$dccp" = 0 ] && lsmod 2>/dev/null | grep -q '^dccp '; then
		modprobe -r dccp 2>/dev/null
		fail "modprobe@dccp.service loaded dccp (unloaded again)"
	fi
	refused "mp service start timers.target" "only reports its state" service start timers.target
	verdict "mp service status debug-shell still reports its state" timeout 60 mp service status debug-shell
	verdict "mp service reload-or-restart nginx still acts" timeout 120 mp service reload-or-restart nginx
}

# --------------------------------------------------------------- objects ----
mkuser() { # mkuser <login> [--shell]
	if secret | timeout 300 mp user add "$@" --password-stdin >"$tmp/out" 2>&1; then return 0; fi
	fail "mp user add $*: $(last "$tmp/out" 2)"
	return 1
}
mksite() { # mksite <domain> <owner> [mp site add flags]
	if timeout 600 mp site add "$1" --user "$2" "${@:3}" --ssl none --no-https-redirect </dev/null >"$tmp/out" 2>&1; then return 0; fi
	fail "mp site add $1 ${*:3}: $(last "$tmp/out" 2)"
	return 1
}
mkshare() { # mkshare <guest>: A's shared/ as $SHARE
	if timeout 300 mp user share add "$1" --site "$DA" --path shared --name "$SHARE" </dev/null >"$tmp/out" 2>&1; then return 0; fi
	fail "mp user share add $1 --site $DA --path shared: $(last "$tmp/out" 2)"
	return 1
}
objects_section() {
	section "accounts and sites of the checks"
	local c
	if leftovers; then
		info "leftovers of an earlier run of the checks: removed first"
		cleanup_objects
	fi
	if mkuser "$A" && mkuser "$SH" --shell && mkuser "$SF" && mksite "$DA" "$A"; then
		objects=1
		put "$A" "$docA/tbck.php" "$probe"
		pass "accounts $A, $SH (shell) and $SF (SFTP-only), the site $DA on php-fpm"
		c=$(get "$DA" /tbck.php)
		if [ "$c" = 200 ] && grep -q '^tbck-ok ' "$tmp/body"; then pass "$DA answers through nginx and php-fpm"; else fail "$DA: $c $(head -c 160 "$tmp/body")"; fi
		if mkshare "$SH" && mkshare "$SF"; then
			shares=1
			put "$A" "$docA/shared/tbck-shared.txt" "tbck-shared-$(rand)"
		fi
	fi
	if [ "$deb" = 1 ] && installed apache; then
		apache=1
		if [ "$objects" = 1 ] && mkuser "$B" && mksite "$AA" "$A" --mode apache && mksite "$BA" "$B" --mode apache; then
			apache_sites=1
			put "$A" "$docAA/tbck.php" "$probe"
			put "$B" "$docBA/tbck.php" "$probe"
			pass "the sites $AA of $A and $BA of $B in Apache mode"
		fi
	fi
}

# ---------------------------------------------------------------- shares ----
# api_mount <guest> <name>: "<mount_point> <mounted>" as the API reports it
api_mount() {
	mp user share list "$1" --json </dev/null 2>/dev/null | pj "[s[\"mount_point\"] + \" \" + str(s[\"mounted\"]).lower() for s in d if s[\"share\"][\"name\"] == \"$2\"][0]"
}
# share_checks <shell guest> <sftp guest> <name> <site folder> <file in it>
share_checks() {
	local sh=$1 sf=$2 name=$3 folder=$4 file=$5 want mpt unit old st mode cfg got
	want=$(cat "$folder/$file" 2>/dev/null)
	# the guest with a shell: below .shares, a symlink in its home. After an
	# upgrade the API's start-up refresh moves the folder there, the last of
	# its steps, and makes the symlink right after the mount: both are
	# waited for.
	mpt=$www/.shares/$sh/$name
	unit=$(systemd-escape --path --suffix=mount "$mpt")
	if wait_until 240 mounted_at "$mpt"; then
		pass "$sh (shell): $name mounted at $mpt ($unit $(systemctl is-active "$unit" 2>&1))"
	else
		fail "$sh (shell): nothing mounted at $mpt ($unit $(systemctl is-active "$unit" 2>&1))"
	fi
	if wait_until 30 links_to "$www/$sh/$name" "$mpt"; then
		pass "$sh: $www/$sh/$name is a symlink to it"
	else
		fail "$sh: $(ls -ld "$www/$sh/$name" 2>&1 | cut -c1-160)"
	fi
	old=$(systemd-escape --path --suffix=mount "$www/$sh/$name")
	if [ -z "$(systemctl list-units --all --no-legend "$old" 2>/dev/null)" ] && [ ! -e "/etc/systemd/system/$old" ] && ! mounted_at "$www/$sh/$name"; then
		pass "$sh: no mount and no unit in its home ($old gone)"
	else
		fail "$sh: $old still there: $(systemctl list-units --all --no-legend "$old" 2>&1 | cut -c1-120)"
	fi
	st=$(stat -c '%U:%G %a' "$www/.shares" 2>&1)
	if [ "$st" = "root:root 711" ]; then pass "$www/.shares: root:root 711"; else fail "$www/.shares: $st"; fi
	st=$(stat -c '%U:%G %a' "$www/.shares/$sh" 2>&1)
	if [ "$st" = "root:$sh 750" ]; then pass "$www/.shares/$sh: root:$sh 750"; else fail "$www/.shares/$sh: $st"; fi
	got=$(as_user "$sh" cat "$www/$sh/$name/$file" 2>&1)
	if [ -n "$want" ] && [ "$got" = "$want" ]; then pass "$sh reads the folder through the symlink"; else fail "$sh reading $www/$sh/$name/$file: $(cut -c1-160 <<<"$got")"; fi
	rm -f "$folder/tbck-probe-$sh"
	if as_user "$sh" sh -c 'echo probe >"$1"' sh "$www/$sh/$name/tbck-probe-$sh" 2>/dev/null && [ -f "$folder/tbck-probe-$sh" ]; then
		pass "$sh writes into it: the file lands in the site's folder"
	else
		fail "$sh cannot write into $www/$sh/$name, or it did not land in $folder"
	fi
	rm -f "$folder/tbck-probe-$sh"
	if as_user "$sf" ls "$www/.shares/$sh" >/dev/null 2>&1; then
		fail "another account ($sf) can list $www/.shares/$sh"
	else
		pass "another account ($sf) cannot look into $www/.shares/$sh"
	fi
	got=$(api_mount "$sh" "$name")
	if [ "$got" = "$mpt true" ]; then pass "the API reports $sh's $name mounted at $mpt"; else fail "the API reports $sh's $name: ${got:-nothing}"; fi

	# the SFTP-only guest: in its chroot, the home root's
	mpt=$www/$sf/$name
	if wait_until 240 mounted_at "$mpt"; then pass "$sf (SFTP-only): $name mounted at $mpt"; else fail "$sf (SFTP-only): nothing mounted at $mpt"; fi
	st=$(stat -c '%U %a' "$www/$sf" 2>&1)
	mode=${st#root }
	# an arithmetic error would end the script: the mode is checked first
	if [ "${st%% *}" = root ] && [[ $mode =~ ^[0-7]{3,4}$ ]] && [ $((8#$mode & 8#022)) -eq 0 ]; then pass "$sf: its home, the chroot, is root's and not writable by others ($st)"; else fail "$sf: its home is $st"; fi
	if id -nG "$sf" 2>/dev/null | tr ' ' '\n' | grep -qx monopanel-sftp; then pass "$sf is in monopanel-sftp"; else fail "$sf is not in monopanel-sftp: $(id -nG "$sf" 2>&1)"; fi
	cfg=$(sshd -T -C "user=$sf,host=localhost,addr=127.0.0.1" 2>&1)
	if grep -qixE "chrootdirectory (%h|$www/$sf)" <<<"$cfg" && grep -qi '^forcecommand internal-sftp' <<<"$cfg"; then
		pass "sshd gives $sf internal-sftp in a chroot of its home"
	else
		fail "sshd -T for $sf: $(grep -iE '^(chrootdirectory|forcecommand) ' <<<"$cfg" | tr '\n' ' ' | cut -c1-200)"
	fi
	got=$(as_user "$sf" cat "$mpt/$file" 2>&1)
	if [ -n "$want" ] && [ "$got" = "$want" ]; then pass "$sf reads the folder"; else fail "$sf reading $mpt/$file: $(cut -c1-160 <<<"$got")"; fi
	got=$(api_mount "$sf" "$name")
	if [ "$got" = "$mpt true" ]; then pass "the API reports $sf's $name mounted at $mpt"; else fail "the API reports $sf's $name: ${got:-nothing}"; fi
}
# switch_checks: the SFTP guest gets a shell and loses it again; its folder
# moves along, and the agent's record of it with it
switch_checks() {
	local mpt=$www/.shares/$SF/$SHARE home=$www/$SF/$SHARE
	if timeout 300 mp user set "$SF" --shell </dev/null >"$tmp/out" 2>&1; then
		if wait_until 60 mounted_at "$mpt" && ! mounted_at "$home" && [ "$(readlink "$home")" = "$mpt" ]; then
			pass "$SF given a shell: $SHARE moved to $mpt, a symlink in the home"
		else
			fail "$SF given a shell: $SHARE is not at $mpt behind a symlink ($(ls -ld "$home" 2>&1 | cut -c1-120))"
		fi
		if [ -f "$(record_of "$mpt")" ] && [ ! -e "$(record_of "$home")" ]; then pass "the agent's record moved along: $(record_of "$mpt")"; else fail "the agent's records after the move: $(ls "$agent_state/shares" 2>&1 | tr '\n' ' ' | cut -c1-200)"; fi
	else
		fail "mp user set $SF --shell: $(last "$tmp/out" 2)"
	fi
	if timeout 300 mp user set "$SF" --sftp-only </dev/null >"$tmp/out" 2>&1; then
		if wait_until 60 mounted_at "$home" && ! mounted_at "$mpt" && [ ! -L "$home" ]; then
			pass "$SF SFTP-only again: $SHARE back at $home"
		else
			fail "$SF SFTP-only again: $SHARE is not back at $home ($(ls -ld "$home" 2>&1 | cut -c1-120))"
		fi
		if [ -f "$(record_of "$home")" ] && [ ! -e "$(record_of "$mpt")" ]; then pass "the agent's record moved back: $(record_of "$home")"; else fail "the agent's records after the move back: $(ls "$agent_state/shares" 2>&1 | tr '\n' ' ' | cut -c1-200)"; fi
	else
		fail "mp user set $SF --sftp-only: $(last "$tmp/out" 2)"
	fi
}
# record_check <mount point> <folder> [taken]: the agent's record of the
# folder it bound there, root's 0600 in its own directory (root 0700), names
# the folder, the mount point and the directory bound (device:inode); taken:
# a unit of the previous release taken over, whose record names no directory
# until the folder is bound anew (its mount was checked by the folder's path)
record_check() {
	local mpt=$1 folder=$2 rec dir st got want
	rec=$(record_of "$mpt")
	dir=$(stat -c '%U %a' "$agent_state/shares" 2>&1)
	st=$(stat -c '%U %a' "$rec" 2>&1)
	got=$(pj '"%s %s %s:%s" % (d["what"], d["where"], d.get("dev", 0), d.get("ino", 0))' 2>/dev/null <"$rec")
	want="$folder $mpt $(stat -c '%d:%i' "$folder" 2>/dev/null)"
	if [ "$dir" = "root 700" ] && [ "$st" = "root 600" ] && { [ "$got" = "$want" ] || { [ -n "${3:-}" ] && [ "$got" = "$folder $mpt 0:0" ]; }; }; then
		pass "the agent's record of $mpt: root 600 in $agent_state/shares (root 700), the folder $folder, device:inode ${got##* }"
	else
		fail "the agent's record of $mpt ($rec): ${st:-?} in a directory ${dir:-?}, '${got:-unreadable}', want '$want'"
	fi
}
# bind_checks <mount point> <folder>: how the agent bound the folder (H19a):
# a record of it, the mount showing the folder, no unit file behind the
# mount unit systemd shows for it, a peer group of its own
bind_checks() {
	local mpt=$1 folder=$2 unit top fs fsroot fsmnt rel ps pf frag act
	record_check "$mpt" "$folder"
	unit=$(systemd-escape --path --suffix=mount "$mpt")
	top=$(mount_top "$mpt")
	fs=$(mount_of "$folder")
	fsroot=$(awk '{print $4}' <<<"$fs")
	fsmnt=$(awk '{print $5}' <<<"$fs")
	rel=${folder#"$fsmnt"}
	[ "$fsmnt" != / ] || rel=$folder
	# the root of the bind is the folder's path in its file system
	if [ -n "$top" ] && [ "$(awk '{print $4}' <<<"$top")" = "${fsroot%/}$rel" ]; then
		pass "findmnt $mpt: $(findmnt -rn -o SOURCE,PROPAGATION --mountpoint "$mpt" 2>/dev/null | tail -1 | cut -c1-160)"
	else
		fail "$mpt does not show $folder: ${top:-nothing mounted there} (want the root ${fsroot%/}$rel of $fsmnt)"
	fi
	frag=$(systemctl show -p FragmentPath --value "$unit" 2>/dev/null)
	act=$(systemctl show -p ActiveState --value "$unit" 2>/dev/null)
	if [ -z "$frag" ] && [ "$act" = active ]; then
		pass "$unit: active, from the mount table alone (FragmentPath empty)"
	else
		fail "$unit: ${act:-?}, FragmentPath ${frag:-empty}"
	fi
	ps=$(peer "$top")
	pf=$(peer "$fs")
	if [ -n "$top" ] && { [ -z "$ps" ] || [ "$ps" != "$pf" ]; }; then
		pass "$mpt in a peer group of its own (${ps:-none}), not ${pf:-none} of $fsmnt where the folder lies"
	else
		fail "$mpt in the peer group ${pf:-?} of $fsmnt: a mount the guest makes inside shows in the owner's site"
	fi
}
# guest_mount_checks: a mount the guest with a shell makes inside its folder
# (root stands in for its fusermount) stays at the guest's mount point: it
# shows neither below the owner's folder nor at the other guest's mount
# point, in the host's mount namespace nor in php-fpm's and nginx's
guest_mount_checks() {
	local in=$www/.shares/$SH/$SHARE/$sub own=$docA/shared/$sub other=$www/$SF/$SHARE/$sub pid name r n=0
	if ! as_user "$SH" mkdir "$www/$SH/$SHARE/$sub" 2>"$tmp/err"; then
		fail "$SH cannot make $sub in its folder: $(last "$tmp/err")"
		return
	fi
	if ! mount -t tmpfs -o size=1m,mode=0755 tbck-tmpfs "$in" 2>"$tmp/err"; then
		fail "mount -t tmpfs at $in: $(last "$tmp/err")"
	elif ! mounted_at "$in"; then
		fail "the tmpfs mounted at $in is not in /proc/self/mountinfo"
	else
		if mounted_at "$own" || mounted_at "$other"; then
			fail "a tmpfs mounted at $in shows at$(mounted_at "$own" && echo " $own")$(mounted_at "$other" && echo " $other")"
		else
			pass "a tmpfs mounted at $in (the guest's own mount) shows neither at $own nor at $other"
		fi
		for pid in $(pgrep -f '^php-fpm: master' | head -4) $(pgrep -o -x nginx); do
			n=$((n + 1))
			name=$(ps -o comm= -p "$pid" 2>/dev/null)
			r=$(nsenter -t "$pid" -m -- awk -v o="$own" -v i="$in" '$5 == o { b = 1 } $5 == i { g = 1 } END { r = (b ? "below" : "clear") " " (g ? "seen" : "unseen"); print r }' /proc/self/mountinfo 2>&1)
			case "$r" in
			"clear seen") pass "in the mount namespace of $name ($pid): the tmpfs at the guest's mount point, not below the owner's folder" ;;
			"clear unseen") pass "in the mount namespace of $name ($pid): the tmpfs not below the owner's folder (nor at the guest's mount point: that namespace does not see the guest's mounts)" ;;
			below*) fail "in the mount namespace of $name ($pid): the tmpfs shows at $own" ;;
			*) fail "nsenter -t $pid -m ($name): $(tail -1 <<<"$r" | cut -c1-160)" ;;
			esac
		done
		[ "$n" -gt 0 ] || fail "no php-fpm master and no nginx to look into"
	fi
	unmount_tbck_tmpfs
	rmdir "$docA/shared/$sub" 2>/dev/null
}
# agent_one_ns <pid>: every thread of the process is in its mount namespace,
# which is not PID 1's
agent_one_ns() {
	local ns t n
	ns=$(readlink "/proc/$1/ns/mnt" 2>/dev/null)
	if [ -z "$ns" ] || [ "$ns" = "$(readlink /proc/1/ns/mnt)" ]; then
		return 1
	fi
	for t in /proc/"$1"/task/*; do
		n=$(readlink "$t/ns/mnt" 2>/dev/null) || continue
		[ "$n" = "$ns" ] || return 1
	done
}
# thread_checks: the agent joins the host's mount namespace for a bind or a
# look at a mount point on a thread of its own that ends with the call, never
# the main thread, which Go would park there for good (H19a): after 300 such
# calls every thread of the agent is in the agent's own namespace
thread_checks() {
	local pid ok t n=0 spread
	pid=$(systemctl show -p MainPID --value monopanel-agent.service 2>/dev/null)
	if ! [ "${pid:-0}" -gt 1 ] 2>/dev/null; then
		fail "monopanel-agent has no MainPID"
		return
	fi
	cat >"$tmp/status.py" <<'EOF'
import http.client, json, socket, sys
class Conn(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect(sys.argv[1])
c, body, ok = Conn("agent"), json.dumps({"paths": sys.argv[3:]}), 0
for _ in range(int(sys.argv[2])):
    c.request("POST", "/v1/shares/status", body=body, headers={"Content-Type": "application/json"})
    r = c.getresponse()
    r.read()
    ok += r.status == 200
print(ok)
EOF
	ok=$(python3 "$tmp/status.py" "$agent_sock" 300 "$www/.shares/$SH/$SHARE" "$www/$SF/$SHARE" 2>&1 | tail -1)
	for t in /proc/"$pid"/task/*; do n=$((n + 1)); done
	if [ "$ok" != 300 ]; then
		fail "300 shares/status calls straight at the agent: $(cut -c1-120 <<<"${ok:-none}") answered 200"
	elif wait_until 10 agent_one_ns "$pid"; then
		pass "after 300 calls that join the host's mount namespace all $n threads of the agent are in its own $(readlink "/proc/$pid/ns/mnt"), not PID 1's $(readlink /proc/1/ns/mnt)"
	else
		spread=$(for t in /proc/"$pid"/task/*; do readlink "$t/ns/mnt"; done 2>/dev/null | sort | uniq -c | awk '{ printf "%s%s x %s", (NR > 1 ? ", " : ""), $1, $2 }')
		fail "the agent's threads: $spread; PID 1: $(readlink /proc/1/ns/mnt), the agent: $(readlink "/proc/$pid/ns/mnt")"
	fi
}
# refusal_checks: what a compromised API might ask of the agent for a shared
# folder, straight at its socket as root (H19a): a mount unit, which systemd
# would mount by its What= path at boot; a start, restart, reload,
# reload-or-restart or enable of a share's mount unit, after which systemd
# would; a bind of the agent's own restore areas. Each is refused (403),
# status passes (stop is not tried: it would unmount the folder), and the
# mount point holds what it held
refusal_checks() {
	local mpt=$www/$SF/$SHARE unit shunit before had=0 c a codes="" what why content
	unit=$(systemd-escape --path --suffix=mount "$mpt")
	shunit=$(systemd-escape --path --suffix=mount "$www/.shares/$SH/$SHARE")
	before=$(mount_ids "$mpt")
	[ ! -e "/etc/systemd/system/$unit" ] || had=1
	content=$(printf '[Mount]\nWhat=%s\nWhere=%s\nType=none\nOptions=bind\n' "$docA/shared" "$mpt")
	c=$(agent_call /v1/units/apply "$(jsn '{"name": a[0], "content": a[1]}' "$unit" "$content")")
	if [ "$c" = 403 ] && { [ "$had" = 1 ] || [ ! -e "/etc/systemd/system/$unit" ]; }; then
		pass "units/apply of $unit straight at the agent: 403, $(agent_said | cut -c1-120)"
	else
		fail "units/apply of $unit straight at the agent: $c $(agent_said)"
		if [ "$had" = 0 ] && [ -e "/etc/systemd/system/$unit" ]; then
			rm -f "/etc/systemd/system/$unit"
			systemctl daemon-reload
		fi
	fi
	for a in start restart reload reload-or-restart enable; do
		c=$(agent_call /v1/service "$(jsn '{"unit": a[0], "action": a[1]}' "$unit" "$a")")
		codes+=" $a=$c"
	done
	c=$(agent_call /v1/service "$(jsn '{"unit": a[0], "action": "start"}' "$shunit")")
	codes+=" start($shunit)=$c"
	if [ "$codes" = " start=403 restart=403 reload=403 reload-or-restart=403 enable=403 start($shunit)=403" ]; then
		pass "/v1/service start, restart, reload, reload-or-restart and enable of $unit, start of $shunit: 403 each, $(agent_said | cut -c1-100)"
	else
		fail "/v1/service of the shares' mount units:$codes"
	fi
	c=$(agent_call /v1/service "$(jsn '{"unit": a[0], "action": "status"}' "$unit")")
	if [ "$c" = 200 ]; then pass "/v1/service status of $unit still passes"; else fail "/v1/service status of $unit: $c $(agent_said)"; fi
	# The web root's restore area is made by the first in-place restore, and
	# nothing of the checks' lies in it: a bind of a path there that does not
	# exist is refused whatever the agent makes of the area. The refusal has
	# to say why — the areas are no account's home.
	for what in "$www/.restore/tbck" "$restore_root"; do
		case "$what" in
		"$www"/.*) why="the agent's own|no account's home|not the home of a panel account" ;;
		*) why="not inside an account's home|not below the web root" ;;
		esac
		c=$(agent_call /v1/shares/bind "$(jsn '{"what": a[0], "where": a[1]}' "$what" "$mpt")")
		if [ "$c" = 403 ] && agent_error | grep -qE "$why"; then
			pass "shares/bind of $what at $mpt: 403, $(agent_said | cut -c1-120)"
		else
			fail "shares/bind of $what at $mpt: $c, not refused as no account's home: $(agent_said)"
		fi
	done
	if mounted_at "$mpt" && [ "$(mount_ids "$mpt")" = "$before" ]; then
		pass "$mpt holds the mount it held before (id $before)"
	else
		fail "the mounts at $mpt: '$before' before, '$(mount_ids "$mpt")' now"
	fi
}
# sleeper_in <dir>: a process of the shell guest works in that directory
sleeper_in() {
	local p
	for p in $(pgrep -u "$SH" -x sleep); do
		[ "$(readlink "/proc/$p/cwd")" != "$1" ] || return 0
	done
	return 1
}
# busy_checks: a folder in use — a process of the guest's works in it — is
# not unmounted: mp user share rm says so, and the folder stays mounted and
# recorded (H19a)
busy_checks() {
	local mpt=$www/.shares/$SH/$SHARE out rc bg
	as_user "$SH" sh -c 'cd "$1" && exec sleep 120' sh "$www/$SH/$SHARE" >/dev/null 2>&1 &
	bg=$!
	if ! wait_until 10 sleeper_in "$mpt"; then
		fail "no process of $SH working in $mpt to keep it in use"
		pkill -u "$SH" -x sleep 2>/dev/null
		wait "$bg" 2>/dev/null
		return
	fi
	out=$(timeout 120 mp user share rm "$SH" "$SHARE" </dev/null 2>&1)
	rc=$?
	pkill -u "$SH" -x sleep 2>/dev/null
	wait "$bg" 2>/dev/null
	if [ "$rc" -ne 0 ] && grep -q 'in use' <<<"$out" && mounted_at "$mpt" && [ -f "$(record_of "$mpt")" ]; then
		pass "mp user share rm $SH $SHARE while $SH works in it: refused ($(grep -o 'it is in use[^)]*)' <<<"$out" | head -1)), mounted and recorded still"
	else
		fail "mp user share rm $SH $SHARE while the folder is in use: exit $rc, $(tail -1 <<<"$out" | cut -c1-200)"
		if [ "$rc" -eq 0 ] && mkshare "$SH"; then info "$SH's $SHARE made again for the checks that follow"; fi
	fi
}
# swap_checks: A swaps its shared folder for a symlink to a directory of
# another account's ($SH's; open to all but behind $SH's own data/): its bind
# straight at the agent is refused for the symlink and mounts nothing, mp site
# fix marks both shares failed with why, leaves that directory as it was and
# what is mounted — the folder bound before the swap — as it is; once A undoes
# the swap, mp site fix mounts them again (H19a)
swap_checks() {
	local mpt=$www/$SF/$SHARE shm=$www/.shares/$SH/$SHARE before c st u want got mode
	want=$(cat "$docA/shared/tbck-shared.txt" 2>/dev/null)
	install -d -o "$SH" -g "$(id -gn "$SH")" -m 0755 "$victim"
	put "$SH" "$victim/tbck-victim.txt" "tbck-victim-$(rand)"
	mode="$(stat -c '%U:%G %a' "$victim" 2>&1) $(stat -c '%U:%G %a' "$victim/tbck-victim.txt" 2>&1)"
	before=$(mount_ids "$mpt")
	if ! as_user "$A" sh -c 'mv "$1" "$1.tbck-real" && ln -s "$2" "$1"' sh "$docA/shared" "$victim" 2>"$tmp/err"; then
		fail "$A cannot swap $docA/shared for a symlink: $(last "$tmp/err")"
		return
	fi
	# the directory is another account's: the agent's look at its owner
	# refuses it too, and only the reason tells that the walk to the folder
	# stopped at the symlink
	c=$(agent_call /v1/shares/bind "$(jsn '{"what": a[0], "where": a[1]}' "$docA/shared" "$mpt")")
	if [ "$c" = 403 ] && agent_error | grep -qF "$docA/shared is a symlink" && [ "$(mount_ids "$mpt")" = "$before" ] && [ ! -e "$mpt/tbck-victim.txt" ]; then
		pass "shares/bind of $docA/shared, which $A swapped for a symlink to $victim: 403, nothing mounted anew ($(agent_said | cut -c1-140))"
	else
		fail "shares/bind of the swapped $docA/shared: $c, not refused for the symlink: $(agent_said); the mounts at $mpt: '$before' before, '$(mount_ids "$mpt")' now"
	fi
	timeout 600 mp site fix "$DA" </dev/null >"$tmp/out" 2>&1 || info "mp site fix $DA with the folder swapped: $(last "$tmp/out" 2)"
	# the site's tree, its owner, ACLs and the guests' entries go through
	# the symlink to nothing: what it points at stays the other account's
	st="$(stat -c '%U:%G %a' "$victim" 2>&1) $(stat -c '%U:%G %a' "$victim/tbck-victim.txt" 2>&1)"
	if [ "$st" = "$mode" ]; then
		pass "mp site fix with the folder swapped left $victim as $SH made it ($st)"
	else
		fail "mp site fix with the folder swapped changed $victim and its file: $st, made $mode"
	fi
	for u in "$SH" "$SF"; do
		st=$(share_state "$u" "$SHARE")
		if [ "${st%% *}" = failed ] && grep -qi symlink <<<"$st"; then
			pass "after mp site fix $u's $SHARE is failed: $(cut -c1-200 <<<"${st#failed }")"
		else
			fail "after mp site fix $u's $SHARE: ${st:-not listed}"
		fi
	done
	if [ -e "$mpt/tbck-victim.txt" ] || [ -e "$shm/tbck-victim.txt" ]; then
		fail "a guest's mount point shows $victim"
	elif [ -n "$want" ] && [ "$(cat "$mpt/tbck-shared.txt" 2>/dev/null)" = "$want" ] && [ "$(cat "$shm/tbck-shared.txt" 2>/dev/null)" = "$want" ]; then
		pass "both mount points still show the folder bound before the swap, nothing of $victim"
	else
		fail "the mount points after mp site fix: $mpt holds $(ls -A "$mpt" 2>&1 | tr '\n' ' ' | cut -c1-120)"
	fi
	if ! as_user "$A" sh -c 'rm "$1" && mv "$1.tbck-real" "$1"' sh "$docA/shared" 2>"$tmp/err"; then
		fail "$A cannot undo the swap: $(last "$tmp/err")"
		return
	fi
	timeout 600 mp site fix "$DA" </dev/null >"$tmp/out" 2>&1 || fail "mp site fix $DA after the swap was undone: $(last "$tmp/out" 2)"
	for u in "$SH" "$SF"; do
		st=$(share_state "$u" "$SHARE")
		got=$(as_user "$u" cat "$www/$u/$SHARE/tbck-shared.txt" 2>&1)
		if [ "${st%% *}" = active ] && [ "$(api_mount "$u" "$SHARE" | cut -d' ' -f2)" = true ] && [ -n "$want" ] && [ "$got" = "$want" ]; then
			pass "the swap undone, mp site fix: $u's $SHARE active and mounted, $u reads the folder"
		else
			fail "after the swap was undone and mp site fix: $u's $SHARE ${st:-not listed}, $(api_mount "$u" "$SHARE"), reading: $(cut -c1-100 <<<"$got")"
		fi
	done
}
# share_doctor_check: the doctor's line on the shared folders: ok, with the count
share_doctor_check() {
	local st n
	st=$(doctor_line "shared folders")
	n=$(grep -oE '[0-9]+' <<<"${st#* }" | head -1)
	if [ "${st%% *}" = ok ] && [ "${n:-0}" -ge 2 ]; then
		pass "doctor: shared folders ok: ${st#ok }"
	else
		fail "doctor: shared folders: ${st:-no line}"
	fi
}
shares_section() {
	section "shared folders (H7, H19a)"
	if [ "$shares" != 1 ]; then
		fail "no shared folders to check (see above)"
		return
	fi
	share_checks "$SH" "$SF" "$SHARE" "$docA/shared" tbck-shared.txt
	switch_checks
	local left
	left=$(unit_files)
	if [ -z "$left" ]; then pass "no mount unit below $www in /etc/systemd/system or its .wants directories"; else fail "mount units below $www: $(tr '\n' ' ' <<<"$left" | cut -c1-200)"; fi
	bind_checks "$www/.shares/$SH/$SHARE" "$docA/shared"
	bind_checks "$www/$SF/$SHARE" "$docA/shared"
	guest_mount_checks
	thread_checks
	refusal_checks
	busy_checks
	swap_checks
	share_doctor_check
}

# ---------------------------------------------------------------- apache ----
hta() { printf '%s\n' "$1" >"$docAA/.htaccess" && chown "$A:$(id -gn "$A")" "$docAA/.htaccess"; }
apache2_stopped() { [ "$(systemctl is-active apache2.service 2>/dev/null)" != active ] && ! pgrep -u www-data apache2 >/dev/null 2>&1; }
apache2_off() {
	if wait_until 120 apache2_stopped; then
		pass "the shared apache2.service is off ($(systemctl is-enabled apache2.service 2>&1)), no apache2 of www-data"
	else
		fail "apache2.service $(systemctl is-active apache2.service 2>&1), apache2 processes of www-data: $(pgrep -c -u www-data apache2)"
	fi
}
instance_checks() { # instance_checks <login>
	local u=$1 unit="monopanel-apache@$1.service" all mine roots
	if [ "$(systemctl is-active "$unit")" = active ]; then pass "$unit active on $(listen_of "$u")"; else fail "$unit: $(systemctl is-active "$unit" 2>&1)"; fi
	all=$(pgrep -fc "accounts/$u/httpd.conf")
	mine=$(pgrep -fc -u "$u" "accounts/$u/httpd.conf")
	roots=$(pgrep -fc -u root "accounts/$u/httpd.conf")
	if [ "${mine:-0}" -ge 1 ] && [ "${roots:-0}" -eq 1 ] && [ "${all:-0}" -eq $((mine + roots)) ]; then
		pass "$unit: the parent is root's, its $mine children $u's"
	else
		fail "$unit: ${all:-0} processes, $u's ${mine:-0}, root's ${roots:-0}"
	fi
}
# ran_only_as <uid>: the probe did not run, or ran as that uid
ran_only_as() { ! grep -q 'tbck-ok' "$tmp/body" || grep -q "^tbck-ok uid=$1 " "$tmp/body"; }
apache_section() {
	section "an Apache per account (H8, H11)"
	if [ "$deb" != 1 ]; then
		skip "sites in Apache mode run on Debian and Ubuntu only"
		return
	fi
	if [ "$apache" != 1 ]; then
		skip "Apache is not installed (testbed.sh deploy installs it on Debian and Ubuntu unless TB_EXTRA says otherwise)"
		return
	fi
	if [ "$apache_sites" != 1 ]; then
		fail "no sites in Apache mode to check (see above)"
		return
	fi
	local uidA uidB addrA addrB c s f st secretB out trick first zeros sockB=$run/php/$BA.sock
	uidA=$(id -u "$A")
	uidB=$(id -u "$B")
	addrA=$(listen_of "$A")
	addrB=$(listen_of "$B")
	if ! [[ $addrA =~ ^127\.[0-9.]+:[0-9]+$ && $addrB =~ ^127\.[0-9.]+:[0-9]+$ ]]; then
		fail "no Listen address in the httpd.conf of $A (${addrA:-none}) or $B (${addrB:-none})"
		return
	fi
	instance_checks "$A"
	instance_checks "$B"
	apache2_off
	if id -nG www-data 2>/dev/null | tr ' ' '\n' | grep -qx "$webgroup"; then fail "www-data is in $webgroup"; else pass "www-data is not in $webgroup"; fi

	c=$(get "$AA" /tbck.php)
	if [ "$c" = 200 ] && grep -q "^tbck-ok uid=$uidA nosecret nouri" "$tmp/body"; then
		pass "$AA through nginx: 200, PHP as $A, neither the backend secret nor SCRIPT_URI in it"
	else
		fail "$AA through nginx: $c $(head -c 160 "$tmp/body")"
	fi
	c=$(get "$BA" /tbck.php)
	if [ "$c" = 200 ] && grep -q "^tbck-ok uid=$uidB " "$tmp/body"; then pass "$BA through nginx: PHP as $B"; else fail "$BA through nginx: $c $(head -c 160 "$tmp/body")"; fi

	# A's .htaccess against B's pool, the root and nobody pools, the server
	if [ -S "$sockB" ]; then
		hta "<FilesMatch \"\\.php\$\">
    SetHandler \"proxy:unix:$sockB|fcgi://localhost\"
</FilesMatch>
SetEnv PHP_ADMIN_VALUE \"open_basedir=/\""
		c=$(get "$AA" /tbck.php)
		if ran_only_as "$uidA"; then pass "A's .htaccess cannot send PHP to B's pool ($c)"; else fail "A's .htaccess ran PHP in B's pool: $c $(head -c 160 "$tmp/body")"; fi
	else
		fail "no php-fpm socket of $BA at $sockB"
	fi
	for s in "$run"/php/.idle-*.sock /run/php/php*-fpm.sock; do
		[ -S "$s" ] || continue
		hta "<FilesMatch \"\\.php\$\">
    SetHandler \"proxy:unix:$s|fcgi://localhost\"
</FilesMatch>"
		c=$(get "$AA" /tbck.php)
		if ran_only_as "$uidA"; then pass "A's .htaccess cannot send PHP to $s ($c)"; else fail "A's .htaccess ran PHP on $s: $c $(head -c 160 "$tmp/body")"; fi
	done
	hta 'SetHandler server-status'
	c=$(get "$AA" /tbck-status)
	if grep -qiE 'Server Version|Apache Status|Scoreboard' "$tmp/body"; then fail "A's .htaccess shows Apache's status page ($c)"; else pass "SetHandler server-status in an .htaccess shows no status page ($c)"; fi
	secretB="tbck-secret-$(rand)"
	put "$B" "$docBA/tbck-secret.txt" "$secretB"
	ln -sfn "$docBA/tbck-secret.txt" "$docAA/tbck-leak" && chown -h "$A:$(id -gn "$A")" "$docAA/tbck-leak"
	hta 'Options -SymLinksIfOwnerMatch +FollowSymLinks'
	c=$(get "$AA" /tbck-leak)
	if grep -qF "$secretB" "$tmp/body"; then fail "a symlink in A's site with FollowSymLinks serves B's file ($c)"; else pass "a symlink with FollowSymLinks reaches nothing of B's ($c)"; fi
	rm -f "$docAA/tbck-leak" "$docBA/tbck-secret.txt"
	hta 'Options -Indexes -ExecCGI -Includes -MultiViews'
	c=$(get "$AA" /tbck.php)
	if [ "$c" = 200 ] && grep -q "^tbck-ok uid=$uidA " "$tmp/body"; then pass "an .htaccess that takes options away (Drupal, Magento) leaves the site working"; else fail "with Options -Indexes -ExecCGI -Includes -MultiViews: $c $(head -c 160 "$tmp/body")"; fi
	hta 'RewriteEngine On
RewriteRule ^tbck-pretty$ tbck.php [L]'
	c=$(get "$AA" /tbck-pretty)
	if [ "$c" = 200 ] && grep -q "^tbck-ok uid=$uidA nosecret nouri" "$tmp/body"; then
		pass "a front controller's RewriteRule works through nginx, still without the secret and SCRIPT_URI"
	else
		fail "RewriteRule through nginx: $c $(head -c 160 "$tmp/body")"
	fi
	rm -f "$docAA/.htaccess"

	# straight to A's Apache from another account, past nginx
	direct() { as_user "$SH" curl -s -m 10 -o /dev/null -w '%{http_code}' -H "Host: $AA" "$@" "http://$addrA/tbck.php"; }
	zeros=$(printf '%064d' 0)
	c=$(direct)
	if [ "$c" = 403 ]; then pass "straight to the Apache of $A ($addrA) from $SH: 403"; else fail "straight to the Apache of $A from $SH: $c"; fi
	c=$(direct -H 'X-Real-IP: 203.0.113.5' -H "X-MonoPanel-Backend: $zeros")
	if [ "$c" = 403 ]; then pass "... with a guessed secret and a forged X-Real-IP: 403"; else fail "... with a guessed secret: $c"; fi
	for trick in 'Satisfy Any' \
		$'Order deny,allow\nDeny from all\nAllow from 203.0.113.5\nSatisfy Any\nAuthType Basic\nAuthName x\nAuthUserFile /dev/null\nRequire valid-user' \
		$'<If "true">\n<If "true">\nRequire all granted\n</If>\n</If>' \
		'RewriteEngine Off'; do
		hta "$trick"
		first=${trick%%$'\n'*}
		c=$(direct -H 'X-Real-IP: 203.0.113.5')
		if [ "$c" = 403 ]; then pass "... with A's .htaccess [$first ...]: 403"; else fail "... with A's .htaccess [$first ...]: $c"; fi
	done
	rm -f "$docAA/.htaccess"

	# the instances' ports: only root binds below 1024
	cat >"$tmp/bind.py" <<'EOF'
import socket, sys, time
host, port, tries, why = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), ""
for _ in range(tries):
    s = socket.socket()
    try:
        s.bind((host, port))
        print("GRABBED", host, port)
        sys.exit(1)
    except OSError as e:
        why = str(e)
    finally:
        s.close()
    time.sleep(0.01)
print("could not bind %s:%d (%s)" % (host, port, why))
EOF
	# a loop that did not run (python3 or setpriv failed) proves nothing:
	# only its own "could not bind" passes
	out=$(as_user "$A" python3 - "${addrA%:*}" "${addrA##*:}" 1 <"$tmp/bind.py" 2>&1)
	if grep -q GRABBED <<<"$out"; then
		fail "$A bound $addrA"
	elif grep -q '^could not bind' <<<"$out"; then
		pass "$A cannot bind its Apache's address: $out"
	else
		fail "the bind attempt as $A did not run: $(tail -1 <<<"$out" | cut -c1-160)"
	fi
	systemctl restart "monopanel-apache@$B.service" &
	out=$(as_user "$A" python3 - "${addrB%:*}" "${addrB##*:}" 300 <"$tmp/bind.py" 2>&1)
	wait
	if grep -q GRABBED <<<"$out"; then
		fail "$A took $addrB while the Apache of $B restarted"
	elif grep -q '^could not bind' <<<"$out"; then
		pass "while the Apache of $B restarts, $A cannot take $addrB"
	else
		fail "the bind attempt as $A during the restart of $B's Apache did not run: $(tail -1 <<<"$out" | cut -c1-160)"
	fi
	if ! wait_until 60 systemctl is-active --quiet "monopanel-apache@$B.service"; then
		fail "monopanel-apache@$B.service is $(systemctl is-active "monopanel-apache@$B.service" 2>&1) after its restart"
	fi

	for f in "/etc/apache2/monopanel/accounts/$A/backend.conf" "/etc/nginx/monopanel/apache/$A.conf"; do
		st=$(stat -c '%U:%G %a' "$f" 2>&1)
		if [ "$st" = "root:root 600" ]; then pass "$f: root:root 600"; else fail "$f: $st"; fi
	done
}

# --------------------------------------------------------------- php-fpm ----
fpm_section() {
	section "the distribution's php-fpm pool idle (H9)"
	local versions v f s st u n
	versions=$(mp php list --json </dev/null 2>/dev/null | pj '[v["version"] for v in d.get("installed") or [] if v.get("status") == "installed"]')
	if [ -z "$versions" ]; then
		fail "no PHP branch installed"
		return
	fi
	for v in $versions; do
		if [ "$deb" = 1 ]; then f=/etc/php/$v/fpm/pool.d/www.conf; else f=/etc/opt/remi/php${v/./}/php-fpm.d/www.conf; fi
		s=$run/php/.idle-$v.sock
		if grep -qxF "listen = $s" "$f" 2>/dev/null && grep -qxF 'user = nobody' "$f" && grep -qxF 'listen.mode = 0600' "$f"; then
			pass "$f: [www] as nobody on $s, 0600"
		else
			fail "$f: $(grep -E '^(user|listen|listen\.mode) *=' "$f" 2>&1 | tr '\n' ' ' | cut -c1-200)"
		fi
		st=$(stat -c '%U:%G %a' "$s" 2>&1)
		if [ "$st" = "root:root 600" ]; then pass "$s: root:root 600"; else fail "$s: $st"; fi
	done
	for u in www-data apache; do
		id "$u" >/dev/null 2>&1 || continue
		n=$(pgrep -c -u "$u" -f php-fpm)
		if [ "${n:-0}" = 0 ]; then pass "no php-fpm process of $u"; else fail "$n php-fpm processes of $u"; fi
	done
}

# -------------------------------------------------------------- composer ----
composer_section() {
	section "composer (R6)"
	local out rc
	if [ ! -x /usr/local/bin/composer ]; then
		if timeout 900 mp stack install composer </dev/null >"$tmp/out" 2>&1; then
			composer_ours=1
			info "composer installed for the checks: removed again at the end"
		else
			fail "mp stack install composer: $(last "$tmp/out" 2)"
			return
		fi
	fi
	out=$(cd / && timeout 60 /usr/local/bin/composer --version </dev/null 2>&1)
	rc=$?
	if [ "$rc" -ne 0 ] && grep -q 'not as root' <<<"$out"; then pass "composer refuses root: $(head -1 <<<"$out" | cut -c1-120)"; else fail "composer as root: exit $rc, $(head -1 <<<"$out" | cut -c1-160)"; fi
	if [ "$objects" = 1 ]; then
		out=$(as_user "$A" env HOME="$www/$A/data" COMPOSER_HOME="$www/$A/data/.composer" timeout 120 /usr/local/bin/composer --version --no-interaction </dev/null 2>&1)
		rc=$?
		if [ "$rc" -eq 0 ] && grep -qi '^composer' <<<"$out"; then pass "composer runs as $A: $(grep -i -m1 '^composer' <<<"$out" | cut -c1-80)"; else fail "composer as $A: exit $rc, $(tail -1 <<<"$out" | cut -c1-160)"; fi
	fi
}

# ---------------------------------------------------------- repositories ----
repos_section() {
	section "the panel's vendor repositories over https (R6)"
	local f bad n=0
	for f in /etc/apt/sources.list.d/nginx.list /etc/apt/sources.list.d/mysql.list /etc/apt/sources.list.d/ondrej-php.list /etc/apt/sources.list.d/sury-php.list /etc/yum.repos.d/nginx.repo /etc/yum.repos.d/mysql-community.repo; do
		[ -f "$f" ] || continue
		n=$((n + 1))
		bad=$(grep -vE '^[[:space:]]*#' "$f" | grep -m1 'http://')
		if [ -n "$bad" ]; then fail "$f names a plain http source: $(cut -c1-160 <<<"$bad")"; else pass "$f: https only"; fi
	done
	if [ "$n" -eq 0 ]; then fail "none of the panel's repository files is here (nginx.list, nginx.repo)"; fi
}

# ---------------------------------------------------------------- restic ----
restic_section() {
	section "restic through the panel (H10)"
	if [ "$objects" != 1 ]; then
		fail "no account to back up (see above)"
		return
	fi
	local repo=/var/backups/tbck/repo mixed=/var/backups/tbck/mixed text sb sandbox=on why snap d out rc st
	sb=$(mp --json doctor </dev/null 2>/dev/null | pj '[c["status"] + " " + c.get("detail", "") for c in d["checks"] if c["name"] == "restic sandbox"][0]')
	case "$sb" in "warn no Landlock"*) sandbox=off ;; esac
	info "restic sandbox: ${sb:-no line in mp doctor}"
	text="tbck-backup-$(rand)"
	put "$A" "$docA/tbck-backup.txt" "$text"
	ln -sfn /etc/monopanel "$www/$A/data/tbck-link" && chown -h "$A:$(id -gn "$A")" "$www/$A/data/tbck-link"
	if ! secret | timeout 120 mp backup target add tbck --type local --repo "$repo" --password-stdin >"$tmp/out" 2>&1; then
		fail "mp backup target add tbck: $(last "$tmp/out" 2)"
		return
	fi
	if timeout 1800 mp backup run --scope "user:$A" --target tbck </dev/null >"$tmp/out" 2>&1; then
		pass "a backup of user:$A into a local repository through the panel"
		backed=1
	else
		fail "mp backup run --scope user:$A --target tbck: $(last "$tmp/out" 3)"
		return
	fi
	st=$(mp backup list --target tbck --json </dev/null 2>/dev/null | pj '[b["status"] + " " + b.get("snapshot_id", "") + " " + b.get("error", "") for b in d][0]')
	snap=$(cut -d' ' -f2 <<<"$st")
	if [ "${st%% *}" = "done" ] && [[ $snap =~ ^[0-9a-f]{8,64}$ ]]; then
		pass "the run is done, snapshot ${snap:0:8}"
	else
		fail "the run: ${st:-not listed}"
		return
	fi
	d=$restore_root/$snap
	restored+=("$d")
	if timeout 1800 mp backup restore "$snap" --target tbck </dev/null >"$tmp/out" 2>&1; then
		pass "snapshot ${snap:0:8} restored aside through the panel, into $d"
	else
		fail "mp backup restore ${snap:0:8} --target tbck: $(last "$tmp/out" 3)"
	fi
	if [ "$(cat "$d$docA/tbck-backup.txt" 2>/dev/null)" = "$text" ]; then pass "the restored copy holds the file as it was backed up"; else fail "$d$docA/tbck-backup.txt is not what was backed up"; fi
	if [ -L "$d$www/$A/data/tbck-link" ] && [ "$(readlink "$d$www/$A/data/tbck-link")" = /etc/monopanel ]; then
		pass "a symlink of the account's to /etc/monopanel came back as the symlink"
	else
		fail "the symlink tbck-link: $(ls -ld "$d$www/$A/data/tbck-link" 2>&1 | cut -c1-160)"
	fi
	if [ -e "$d/etc/monopanel" ]; then fail "the account's snapshot holds $d/etc/monopanel"; else pass "nothing of /etc/monopanel in the account's snapshot"; fi
	st=$(stat -c '%U %a' "$restore_root" 2>&1)
	if [ "$st" = "root 700" ]; then pass "$restore_root: root's, 700"; else fail "$restore_root: $st"; fi

	# a repository directory with files of somebody else's in it
	mkdir -p "$mixed"
	echo "not restic's" >"$mixed/tbck-other.txt"
	if ! secret | timeout 120 mp backup target add tbckmix --type local --repo "$mixed" --password-stdin >"$tmp/out" 2>&1; then
		fail "mp backup target add tbckmix: $(last "$tmp/out" 2)"
		return
	fi
	out=$(timeout 900 mp backup run --scope "site:$DA" --target tbckmix </dev/null 2>&1)
	rc=$?
	if [ "$rc" -ne 0 ] && grep -q 'holds files restic did not make' <<<"$out"; then
		pass "a repository directory that holds other files is refused: $(grep -o 'the repository [^:]*: it holds files restic did not make' <<<"$out" | head -1)"
		if [ ! -e "$mixed/config" ]; then
			pass "nothing written into it"
		elif [ "$sandbox" = off ] && why=$(expected_why "restic sandbox" "${sb#warn }"); then
			# without Landlock only a backup may go through the agent's
			# sandbox (for its view without symlinks), while restic init,
			# unconfined, already made the repository there
			expected "restic wrote its repository into it before the refusal: restic runs without its sandbox here ($why)"
		else
			fail "restic wrote into it all the same"
		fi
		# the same directory emptied is a repository like any: and the doctor
		# counts the refused run as one a later run of the target fixed
		rm -f "$mixed/tbck-other.txt"
		if timeout 900 mp backup run --scope "site:$DA" --target tbckmix </dev/null >"$tmp/out" 2>&1; then
			pass "the same directory, emptied, is taken"
		else
			fail "the emptied directory: $(last "$tmp/out" 3)"
		fi
	elif [ "$sandbox" = off ] && [ "$rc" -eq 0 ] && why=$(expected_why "restic sandbox" "${sb#warn }"); then
		expected "a repository directory that holds other files is taken here: restic runs without its sandbox ($why)"
	elif [ "$sandbox" = off ] && [ "$rc" -eq 0 ]; then
		# a host doctor-expected.sh does not name: not a known shortfall
		fail "a repository directory that holds other files is taken: restic runs without its sandbox here, and doctor-expected.sh has no rule for this host ($(cut -c1-120 <<<"$sb"))"
	else
		fail "a repository directory that holds other files: exit $rc, $(tail -2 <<<"$out" | tr '\n' ' ' | cut -c1-240)"
	fi
}

# --------------------------------------------------------- restore nodes ----
# restore_area: what the web root's restore area holds, where an in-place
# restore stages
restore_area() { find "$www/.restore" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | sort | tr '\n' ' '; }
# refused_restore <what> <nodes it may name, an ERE> <snapshot> <mp backup
# restore args...>: the job fails naming the node, and nothing is staged: the
# restore area as it was, nothing aside, nothing restored in place
refused_restore() {
	local what=$1 want=$2 snap=$3 out rc now
	shift 3
	out=$(timeout 900 mp backup restore "$snap" --target tbck "$@" </dev/null 2>&1)
	rc=$?
	now=$(restore_area)
	if [ "$rc" -ne 0 ] && grep -qE "it holds ($want)" <<<"$out" && [ "$now" = "$area" ] && [ ! -e "$restore_root/$snap" ] && [ "$(cat "$docA/tbck-inplace.txt" 2>/dev/null)" = changed ]; then
		pass "restore $what: refused, $(grep -oE 'it holds [^,]+, [^,]+' <<<"$out" | head -1 | cut -c1-160); nothing staged in $www/.restore, nothing aside"
	else
		fail "restore $what: exit $rc, $(tail -1 <<<"$out" | cut -c1-200); $www/.restore holds '$now' (before: '$area')"
	fi
}
# nodes_section: a backup of a tree holding a FIFO and a set-uid file; the
# restores in place and aside that would bring them back are refused before
# restic writes, naming the node, and a subtree beside them is restored in
# place — a later job of the same type and lock than the refused ones, which
# the doctor's jobs line then counts as fixed (H19a)
nodes_section() {
	section "restores bring back no FIFO and no set-uid file (H19a)"
	if [ "$backed" != 1 ]; then
		fail "no backup target of $A's to restore from (see above)"
		return
	fi
	local snap want refused="$nodes/tbck-fifo, a FIFO|$nodes/tbck-suid, a set-user-ID file"
	rm -rf "$nodes"
	if ! as_user "$A" mkdir "$nodes" 2>"$tmp/err" || ! as_user "$A" mkfifo "$nodes/tbck-fifo" 2>"$tmp/err" ||
		! install -o "$A" -g "$(id -gn "$A")" -m 4755 /bin/true "$nodes/tbck-suid" 2>"$tmp/err" || [ ! -p "$nodes/tbck-fifo" ] || [ ! -u "$nodes/tbck-suid" ]; then
		fail "a FIFO and a set-uid file of $A's in $nodes: $(last "$tmp/err") $(ls -l "$nodes" 2>&1 | tr '\n' ' ' | cut -c1-160)"
		rm -rf "$nodes"
		return
	fi
	want="tbck-inplace-$(rand)"
	put "$A" "$docA/tbck-inplace.txt" "$want"
	if ! timeout 1800 mp backup run --scope "user:$A" --target tbck </dev/null >"$tmp/out" 2>&1; then
		fail "mp backup run --scope user:$A --target tbck with the FIFO and the set-uid file: $(last "$tmp/out" 3)"
		rm -rf "$nodes"
		return
	fi
	snap=$(mp backup list --target tbck --json </dev/null 2>/dev/null | pj 'max((b for b in d if b.get("status") == "done"), key=lambda b: b["id"])["snapshot_id"]')
	if ! [[ $snap =~ ^[0-9a-f]{8,64}$ ]]; then
		fail "no snapshot of the backup with the FIFO and the set-uid file: ${snap:-none}"
		rm -rf "$nodes"
		return
	fi
	info "snapshot ${snap:0:8} holds $nodes/tbck-fifo and $nodes/tbck-suid (4755)"
	restored+=("$restore_root/$snap")
	put "$A" "$docA/tbck-inplace.txt" changed
	area=$(restore_area)
	refused_restore "in place of $www/$A" "$refused" "$snap" --include "$www/$A" --in-place
	refused_restore "aside of $www/$A" "$refused" "$snap" --include "$www/$A"
	refused_restore "in place of the set-uid file alone" "$nodes/tbck-suid, a set-user-ID file" "$snap" --include "$nodes/tbck-suid" --in-place
	if timeout 1800 mp backup restore "$snap" --target tbck --include "$www/$A/data/www" --in-place </dev/null >"$tmp/out" 2>&1 &&
		[ "$(cat "$docA/tbck-inplace.txt" 2>/dev/null)" = "$want" ] && [ "$(restore_area)" = "$area" ]; then
		pass "restore in place of $www/$A/data/www beside them: done, $docA/tbck-inplace.txt back as backed up, nothing left staged"
	else
		fail "restore in place of $www/$A/data/www: $(last "$tmp/out" 2); tbck-inplace.txt: $(head -c 60 "$docA/tbck-inplace.txt" 2>&1); $www/.restore holds '$(restore_area)'"
	fi
	rm -rf "$nodes" "$docA/tbck-inplace.txt"
}

# --------------------------------------------------------- package files ----
# pkgfiles_section: what the package keeps or prunes across an upgrade and
# a deploy alike (H17, H19b): the tmpfiles lines of the run directory, read
# as at boot without a "Duplicate line"; no copy of the panel's units in
# /etc/systemd/system (mp setup up to 0.8.19 made them, a downgrade keeps
# the Apache template there); the package's modprobe rule alone, and sctp
# kept from loading — modprobe's word and a socket of the protocol, opened
# by nobody. A panel set up from a bare binary (testbed.sh deploy --binary)
# has mp setup's own files instead: its units in /etc/systemd/system, its
# rule in /etc/modprobe.d, and monopanel-php.conf for the way back
pkgfiles_section() {
	section "the package's files: tmpfiles, unit copies, the modprobe rule (H17, H19b)"
	local f=/etc/tmpfiles.d/monopanel.conf p=/etc/tmpfiles.d/monopanel-php.conf line dup u left="" frag out loaded=0 c rc pkg=0
	line="d $run/php 0755 root root -"
	if [ "$deb" = 1 ]; then
		case "$(dpkg-query -W -f '${Status}' monopanel 2>/dev/null)" in *" installed") pkg=1 ;; esac
	elif rpm -q monopanel >/dev/null 2>&1; then
		pkg=1
	fi
	[ "$pkg" = 1 ] || info "no package monopanel here: the panel came as a bare binary, and mp setup's own files stand for the package's"
	if grep -qxF "$line" "$f" 2>/dev/null && grep -qE "^d $run 1771 root [^ ]+ -\$" "$f"; then
		pass "$f: the run directory 1771 and its php/ line"
	else
		fail "$f: $(grep -v '^#' "$f" 2>&1 | tr '\n' '|' | cut -c1-200)"
	fi
	if [ -f "$p" ] && [ ! -L "$p" ]; then
		if [ "$(cat "$p")" = "$line" ]; then pass "$p kept, exactly '$line': what a version before 0.8.20 needs at boot"; else fail "$p: $(tr '\n' '|' <"$p" | cut -c1-200)"; fi
	elif [ -e "$p" ] || [ -L "$p" ]; then
		fail "$p is no plain file: $(ls -ld "$p" 2>&1 | cut -c1-160)"
	elif [ "$context" = upgrade ]; then
		fail "$p is gone: the previous release's API wrote it with PHP, and the package keeps it for the way back below 0.8.20"
	elif [ "$pkg" = 0 ]; then
		fail "no $p: mp setup writes it on a host without the package, for the way back below 0.8.20, which no preremove writes there"
	else
		pass "no $p on a fresh deploy: nothing of this build writes one ($f holds the line)"
	fi
	# --clean touches only lines with an age (these have none), and --prefix
	# parses the files as at boot but keeps the lines of the run directory.
	# Exit 65 is a line of some other file it could not parse; any other
	# failure, and it read nothing — no "Duplicate line" proves nothing then
	out=$(systemd-tmpfiles --clean --prefix="$run" 2>&1)
	rc=$?
	dup=$(grep -i 'duplicate line' <<<"$out")
	if [ -n "$dup" ]; then
		fail "systemd-tmpfiles: $(head -2 <<<"$dup" | tr '\n' ' ' | cut -c1-240)"
	elif [ "$rc" != 0 ] && [ "$rc" != 65 ]; then
		fail "systemd-tmpfiles --clean --prefix=$run did not read the files: exit $rc, $(tail -1 <<<"$out" | cut -c1-200)"
	else
		pass "systemd-tmpfiles reads the lines for $run as at boot without a 'Duplicate line' ($(systemd-tmpfiles --cat-config 2>/dev/null | grep -c "^d ${run}[/ ]") lines)"
	fi
	for u in monopanel-agent.service monopanel-api.service monopanel-api.socket monopanel-apache@.service; do
		if [ -f "/etc/systemd/system/$u" ] && [ ! -L "/etc/systemd/system/$u" ]; then left+=" $u"; fi
	done
	for u in monopanel-agent.service monopanel-api.service monopanel-api.socket; do
		frag=$(systemctl show -p FragmentPath --value "$u" 2>/dev/null)
		case "$frag" in /etc/*) left+=" $u ($frag)" ;; esac
	done
	if [ "$pkg" = 0 ]; then
		skip "no package: the panel's units in /etc/systemd/system are mp setup's, the only ones, no copies"
	elif [ -z "$left" ]; then
		pass "no copy of the panel's units in /etc/systemd/system, the Apache template's included: the package's apply ($(systemctl show -p FragmentPath --value monopanel-agent.service 2>/dev/null))"
	else
		fail "copies of the panel's units in /etc/systemd/system:$left"
	fi
	if [ "$pkg" = 0 ]; then
		if [ -f /etc/modprobe.d/monopanel.conf ]; then
			pass "no package: mp setup's /etc/modprobe.d/monopanel.conf applies"
		else
			fail "no package, and no /etc/modprobe.d/monopanel.conf, which mp setup writes on a host without it"
		fi
	elif [ ! -f /usr/lib/modprobe.d/monopanel.conf ]; then
		fail "/usr/lib/modprobe.d/monopanel.conf, the package's rule, is missing"
	elif [ -e /etc/modprobe.d/monopanel.conf ] || [ -L /etc/modprobe.d/monopanel.conf ]; then
		fail "/etc/modprobe.d/monopanel.conf is there and replaces the package's rule: $(grep -c '^install ' /etc/modprobe.d/monopanel.conf 2>&1) install lines"
	else
		pass "the package's /usr/lib/modprobe.d/monopanel.conf applies, no /etc/modprobe.d/monopanel.conf"
	fi
	if lsmod 2>/dev/null | grep -q '^sctp '; then loaded=1; fi
	out=$(modprobe -n -v sctp 2>&1)
	if [ "$loaded" = 1 ]; then
		fail "sctp is loaded: no rule unloads it"
	elif grep -q '^install /bin/false' <<<"$out"; then
		pass "modprobe -n -v sctp: $(head -1 <<<"$out" | cut -c1-80)"
	else
		fail "modprobe -n -v sctp: $(tr '\n' ' ' <<<"${out:-nothing}" | cut -c1-160)"
	fi
	out=$(cd / && setpriv --reuid="$(id -u nobody)" --regid="$(id -g nobody)" --clear-groups -- python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_STREAM, 132); print("OPENED")' 2>&1)
	if grep -q OPENED <<<"$out"; then
		fail "nobody opened a socket of IPPROTO_SCTP"
	elif grep -qE 'Errno (93|94)|not supported' <<<"$out"; then
		pass "socket(AF_INET, SOCK_STREAM, IPPROTO_SCTP) as nobody: $(tail -1 <<<"$out" | cut -c1-80)"
	else
		fail "the SCTP socket as nobody did not get as far as the kernel: $(tail -1 <<<"$out" | cut -c1-160)"
	fi
	if [ "$loaded" = 0 ] && lsmod 2>/dev/null | grep -q '^sctp '; then
		modprobe -r sctp 2>/dev/null
		fail "sctp got loaded (unloaded again)"
	fi
	c=$(mp --json status </dev/null 2>/dev/null | pj '"container=" + (d["host"].get("container") or "")')
	if [ "$c" = container= ]; then pass "the agent sees no container: the kernel loads modules with this host's rule"; else fail "the agent's container: ${c:-unknown (mp --json status)}"; fi
}

# ---------------------------------------------------------- package runs ----
# journal_has <unit> <text>: the unit's journal holds the text
journal_has() { journalctl -u "$1" --no-pager -o cat 2>/dev/null | grep -qF "$2"; }
no_pkg_units() { [ -z "$(systemctl list-units --all --no-legend 'monopanel-pkg-*' 2>/dev/null)" ]; }
# pkgrun_section: the package manager's changes run in a transient unit of
# their own, monopanel-pkg-<run>.service (H18). What only reads (query,
# available, candidates — the Stack page, the doctor, mp stack list) runs as
# the agent's child, so no read goes through such a unit. A deploy installed
# nginx, PHP and the database through the panel: its runs are in the
# journal. After an upgrade this build has run none yet; then an index
# refresh straight at the agent shows one, which installs nothing — after
# the doctor, which judges the updates as the index last saw them, and once:
# a later run of the checks finds it in the journal. Either way no unit and
# no output file of a run may be left. The daily upgrade of nginx, PHP and
# Apache is on at 04:00 with a next run; one version of each PHP branch
# everywhere (H19c)
pkgrun_section() {
	section "the package manager in a unit of its own, the daily upgrade (H18, H19c)"
	local run unit c n st out pv sl v ver sv qv
	n=$(journalctl -u 'monopanel-pkg-*' --no-pager -o cat 2>/dev/null | grep -c '^Starting .*MonoPanel package manager')
	if [ "$n" -gt 0 ]; then
		pass "journalctl -u 'monopanel-pkg-*': $n runs of the package manager, each in a unit of its own$([ "$context" = deploy ] && echo ": the deploy's installs")"
	else
		# after an upgrade (or a reboot that took a volatile journal along)
		info "no run of the package manager in a unit of its own in the journal ($context): an index refresh shows one"
		run=$(rand)
		unit=monopanel-pkg-$run.service
		c=$(agent_call /v1/pkg "$(jsn '{"action": "update-index", "run": a[0]}' "$run")" 900)
		if [ "$c" = 200 ]; then
			info "update-index: $(pj 'd.get("output", "")' <"$tmp/agent" | grep -v '^ *$' | tail -1 | cut -c1-160)"
		else
			info "update-index: $c $(agent_said)"
		fi
		if wait_until 20 journal_has "$unit" 'MonoPanel package manager'; then
			pass "the index refresh ran as $unit: $(journalctl -u "$unit" --no-pager -o cat 2>/dev/null | grep -m1 -F 'MonoPanel package manager' | cut -c1-160)"
		else
			fail "no journal of $unit: the index refresh did not run in a unit of its own (the agent: $c $(agent_said))"
		fi
	fi
	if wait_until 20 no_pkg_units && [ -z "$(find "$agent_state/pkg" -name '*.out' 2>/dev/null)" ]; then
		pass "no unit of a run left (systemctl list-units --all 'monopanel-pkg-*'), nor an output file in $agent_state/pkg"
	else
		fail "left of the runs: $(systemctl list-units --all --no-legend 'monopanel-pkg-*' 2>&1 | tr '\n' ' ' | cut -c1-160) $(find "$agent_state/pkg" -name '*.out' 2>/dev/null | tr '\n' ' ')"
	fi
	cat >"$tmp/auto.py" <<'EOF'
import datetime, json, re, sys
try:
    d = json.load(sys.stdin)
except Exception as e:
    print("bad: not JSON:", e)
    sys.exit()
n = d.get("next_run") or ""
if d.get("enabled") is not True or d.get("hour") != 4 or not n:
    print("bad: enabled %s, hour %s, next run %s" % (d.get("enabled"), d.get("hour"), n or "none"))
    sys.exit()
# fromisoformat before 3.11 takes neither Z nor nine digits of a second
t = datetime.datetime.fromisoformat(re.sub(r"\.\d+", "", n).replace("Z", "+00:00"))
left = (t - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
if t.hour != 4 or t.minute != 0 or not -3600 < left <= 25 * 3600:
    print("bad: next run %s, in %.1f h" % (n, left / 3600))
else:
    print("ok %s %s, in %.1f h" % (t.strftime("%Y-%m-%d %H:%M"), d.get("timezone", ""), left / 3600))
EOF
	st=$(mp --json stack auto-upgrade </dev/null 2>/dev/null | python3 "$tmp/auto.py" 2>&1)
	out=$(mp stack auto-upgrade </dev/null 2>&1)
	if [ "${st%% *}" = ok ] && grep -q '^daily upgrade of nginx, PHP and Apache: on, at 04:00 ' <<<"$out" && grep -qE '^next run: [0-9-]+ 04:00$' <<<"$out"; then
		pass "mp stack auto-upgrade: on by default, at 04:00, next run ${st#ok }"
	else
		fail "mp stack auto-upgrade: ${st:-no answer}; $(head -2 <<<"$out" | tr '\n' ' ' | cut -c1-160)"
	fi
	pv=$(mp --json php list </dev/null 2>/dev/null | pj '[v["version"] + " " + (v.get("package_version") or "-") for v in d.get("installed") or [] if v.get("status") == "installed"]')
	sl=$(mp --json stack list </dev/null 2>/dev/null)
	[ -n "$pv" ] || fail "mp php list names no installed PHP branch"
	while read -r v ver; do
		[ -n "$v" ] || continue
		sv=$(pj "[c.get(\"version\") or \"-\" for c in d if c[\"name\"] == \"php-$v\"][0]" <<<"$sl")
		if [ "$deb" = 1 ]; then
			# shellcheck disable=SC2016 # dpkg-query's own field
			qv=$(dpkg-query -W -f '${Version}' "php$v-fpm" 2>/dev/null)
		else
			qv=$(rpm -q --qf '%{VERSION}-%{RELEASE}' "php${v/./}-php-fpm" 2>/dev/null)
		fi
		if [ "$ver" != - ] && [ "$ver" = "$sv" ] && [ "$ver" = "$qv" ]; then
			pass "PHP $v: one version in mp php list, mp stack list and the package database, $ver"
		else
			fail "PHP $v: mp php list ${ver:-?}, mp stack list ${sv:-?}, the package database ${qv:-nothing}"
		fi
	done <<<"$pv"
}

# ---------------------------------------------------------------- doctor ----
# doctor_verdicts: "TOKEN<tab>message" lines for what the doctor says. The
# API judges its socket itself (its "api socket" line, in --json too), and
# mp prints that line as it is: it goes through the loop like any other, and
# counts once. Only for an API without the line does mp judge the socket
# itself, in the text alone — which is looked at then.
doctor_verdicts() {
	local json text status name detail why found=0 sock=0 n
	json=$(mp --json doctor </dev/null 2>/dev/null)
	text=$(mp doctor </dev/null 2>&1)
	if [ -z "$json" ]; then
		printf 'FAIL\tmp doctor gave nothing: %s\n' "$(tail -1 <<<"$text" | cut -c1-200)"
		return
	fi
	while IFS=$'\t' read -r status name detail; do
		[ -n "$name" ] || continue
		[ "$name" != "api socket" ] || sock=$((sock + 1))
		case "$status" in
		ok)
			case "$name" in
			"restic sandbox")
				found=1
				printf 'PASS\tdoctor: restic sandbox ok (%s)\n' "$detail"
				if rule_for "restic sandbox"; then printf 'INFO\tdoctor-expected.sh allows a restic sandbox warning here that is gone: its rule can go\n'; fi
				;;
			"api socket" | "kernel modules" | "shared folders") printf 'PASS\tdoctor: %s ok (%s)\n' "$name" "$(cut -c1-160 <<<"$detail")" ;;
			esac
			;;
		warn)
			[ "$name" = "restic sandbox" ] && found=1
			if why=$(expected_why "$name" "$detail"); then
				printf 'EXPECTED\tdoctor: [WARN] %s: %s — %s\n' "$name" "$(cut -c1-160 <<<"$detail")" "$why"
			else
				printf 'FAIL\tdoctor: [WARN] %s: %s\n' "$name" "$(cut -c1-240 <<<"$detail")"
			fi
			;;
		fail) printf 'FAIL\tdoctor: [FAIL] %s: %s\n' "$name" "$(cut -c1-240 <<<"$detail")" ;;
		esac
	done < <(pj '"\n".join(c["status"] + "\t" + c["name"] + "\t" + " ".join(c.get("detail", "").split()) for c in d["checks"])' <<<"$json")
	[ "$found" = 1 ] || printf 'FAIL\tdoctor: no restic sandbox line (an agent older than 0.8.20?)\n'
	n=$(grep -c '^\[.\{4\}\] api socket' <<<"$text")
	if [ "$sock" -gt 0 ]; then
		if [ "$sock" = 1 ] && [ "$n" = 1 ]; then
			printf 'PASS\tdoctor: the api socket line is the one the API judged, in --json and printed once by mp doctor\n'
		else
			printf 'FAIL\tdoctor: %s api socket lines in mp --json doctor, %s printed by mp doctor\n' "$sock" "$n"
		fi
	else
		printf 'FAIL\tdoctor: no api socket line in mp --json doctor: an API older than this build does not judge its socket\n'
		if grep -q '^\[WARN\] api socket' <<<"$text"; then
			printf 'FAIL\tdoctor (mp judged): %s\n' "$(grep -m1 '^\[WARN\] api socket' <<<"$text" | cut -c1-240)"
		else
			printf 'PASS\tdoctor: no api socket warning from mp itself\n'
		fi
	fi
	printf 'INFO\tdoctor: %s\n' "$(tail -1 <<<"$text")"
}
doctor_section() {
	section "mp doctor"
	local v tok msg
	settle >/dev/null
	v=$(doctor_verdicts)
	if grep -q '^FAIL' <<<"$v"; then
		info "doctor: findings; once more in 20 s, a reload may have been under way"
		sleep 20
		settle >/dev/null
		v=$(doctor_verdicts)
	fi
	while IFS=$'\t' read -r tok msg; do
		case "$tok" in
		PASS) pass "$msg" ;;
		FAIL) fail "$msg" ;;
		EXPECTED) expected "$msg" ;;
		INFO) info "$msg" ;;
		esac
	done <<<"$v"
	# the doctor only counts the failed jobs: which ones, for whoever reads the log
	if grep -q $'^FAIL\tdoctor: \\[WARN\\] jobs' <<<"$v"; then
		mp job list --status failed --limit 10 </dev/null 2>&1 | tail -n +2 | cut -c1-200 | sed 's/^/INFO      failed job: /'
	fi
}

# --------------------------------------------------------------- selinux ----
selinux_section() {
	section "SELinux"
	local mode n
	if ! command -v getenforce >/dev/null 2>&1; then
		skip "no SELinux on this host"
		return
	fi
	mode=$(getenforce 2>/dev/null)
	if [ "$mode" = Disabled ]; then
		skip "SELinux is disabled"
		return
	fi
	info "the agent's domain: $(ps -o label= -p "$(systemctl show -p MainPID --value monopanel-agent.service 2>/dev/null)" 2>/dev/null)"
	if ! command -v ausearch >/dev/null 2>&1; then
		fail "SELinux $mode, but ausearch (audit) is missing: AVC denials cannot be counted"
		return
	fi
	n=$(ausearch -m avc -ts boot 2>/dev/null | grep -c 'type=AVC')
	if [ "$n" = 0 ]; then
		pass "SELinux $mode: no AVC denial since boot (ausearch -m avc -ts boot)"
	else
		fail "SELinux $mode: $n AVC denials since boot"
		ausearch -m avc -ts boot 2>/dev/null | grep 'type=AVC' | tail -5 | cut -c1-300 | sed 's/^/INFO      /'
	fi
}

echo "checks.sh $vm: $PRETTY_NAME, kernel $kernel, $(mp version </dev/null 2>/dev/null | head -1), context $context"
if ! panel_section; then
	selinux_section
	total
fi
[ -z "${TB_CARRIED:-}" ] || carried_section
socket_section
policy_section
objects_section
shares_section
apache_section
fpm_section
composer_section
repos_section
restic_section
nodes_section
pkgfiles_section
section "cleanup"
cleanup
info "the accounts, sites, shares, targets and restored snapshots of the checks are gone$([ "$composer_ours" = 1 ] && echo ', and composer')"
doctor_section
# after the doctor: the index refresh of an upgraded host may bring news of
# updates, which the doctor would warn of as waiting
pkgrun_section
selinux_section
total
