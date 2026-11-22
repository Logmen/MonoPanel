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
#                 account's Apache, the old backup restorable)
#   api socket    monopanel-api.socket holds the port: the API serves on it,
#                 the port stays bound through restarts, nobody cannot take it
#                 meanwhile, a connection starts a stopped API
#   unit policy   mp service refuses debug-shell, template instances and
#                 targets, and still acts on nginx
#   shares        a guest with a shell has its folder below <www>/.shares and a
#                 symlink to it, an SFTP-only guest in its chroot; a switch
#                 between the two moves it; no unit left in a home
#   apache        Debian/Ubuntu: an Apache per account, apache2 off, www-data
#                 out of the web group, an .htaccess reaches nothing of another
#                 account, requests straight to an account's Apache get 403
#                 (with Satisfy Any and a nested <If> too), PHP sees no secret
#   php-fpm       the distribution's [www] pool idle: nobody, a root-only socket
#   composer      refuses root, runs as an account
#   repositories  the panel's vendor sources are https
#   restic        a backup through the panel, restored aside, a repository
#                 directory holding other files refused
#   doctor        restic sandbox ok (or EXPECTED), no api socket warning, no
#                 failure, no warning doctor-expected.sh does not list
#   selinux       EL: no AVC denial since boot
#
# What the checks make goes again at the end, also after a failure: the
# accounts tbcka, tbckb, tbcksh and tbcksf with their sites and shares, the
# backup targets tbck and tbckmix with /var/backups/tbck, the snapshots
# restored aside, composer when the checks installed it. Leftovers of a run
# that was cut short go first. What upgrade-seed.sh made (tbup*) stays. The
# passwords of the accounts and repositories are random, go to mp on stdin
# and are printed nowhere.
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
objects=0 shares=0 apache=0 apache_sites=0 composer_ours=0 cleaned=0
restored=()

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
	[ -n "$(mp backup target list --json </dev/null 2>/dev/null | pj '[t["name"] for t in d if t["name"] in ("tbck", "tbckmix")]')" ]
}
cleanup_objects() {
	local u t d
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
carried_section() {
	section "carried over from the previous release (upgrade-seed.sh)"
	if [ ! -r /root/tbup.env ]; then
		fail "/root/tbup.env is missing: upgrade-seed.sh did not run here"
		return
	fi
	local from mark snap cli own=tbupown dom apa doc c st d got
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
		if grep -qx "$snap" <<<"$got"; then pass "the snapshot ${snap:0:8} that $from made is in the repository"; else fail "the snapshot ${snap:0:8} of $from is not listed: $(tr '\n' ' ' <<<"$got" | cut -c1-200)"; fi
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
# switch_checks: the SFTP guest gets a shell and loses it again; its folder moves along
switch_checks() {
	local mpt=$www/.shares/$SF/$SHARE home=$www/$SF/$SHARE
	if timeout 300 mp user set "$SF" --shell </dev/null >"$tmp/out" 2>&1; then
		if wait_until 60 mounted_at "$mpt" && ! mounted_at "$home" && [ "$(readlink "$home")" = "$mpt" ]; then
			pass "$SF given a shell: $SHARE moved to $mpt, a symlink in the home"
		else
			fail "$SF given a shell: $SHARE is not at $mpt behind a symlink ($(ls -ld "$home" 2>&1 | cut -c1-120))"
		fi
	else
		fail "mp user set $SF --shell: $(last "$tmp/out" 2)"
	fi
	if timeout 300 mp user set "$SF" --sftp-only </dev/null >"$tmp/out" 2>&1; then
		if wait_until 60 mounted_at "$home" && ! mounted_at "$mpt" && [ ! -L "$home" ]; then
			pass "$SF SFTP-only again: $SHARE back at $home"
		else
			fail "$SF SFTP-only again: $SHARE is not back at $home ($(ls -ld "$home" 2>&1 | cut -c1-120))"
		fi
	else
		fail "mp user set $SF --sftp-only: $(last "$tmp/out" 2)"
	fi
}
shares_section() {
	section "shared folders (H7)"
	if [ "$shares" != 1 ]; then
		fail "no shared folders to check (see above)"
		return
	fi
	share_checks "$SH" "$SF" "$SHARE" "$docA/shared" tbck-shared.txt
	switch_checks
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

# ---------------------------------------------------------------- doctor ----
# doctor_verdicts: "TOKEN<tab>message" lines for what the doctor says
doctor_verdicts() {
	local json text status name detail why found=0
	json=$(mp --json doctor </dev/null 2>/dev/null)
	text=$(mp doctor </dev/null 2>&1)
	if [ -z "$json" ]; then
		printf 'FAIL\tmp doctor gave nothing: %s\n' "$(tail -1 <<<"$text" | cut -c1-200)"
		return
	fi
	while IFS=$'\t' read -r status name detail; do
		[ -n "$name" ] || continue
		case "$status" in
		ok)
			if [ "$name" = "restic sandbox" ]; then
				found=1
				printf 'PASS\tdoctor: restic sandbox ok (%s)\n' "$detail"
				if rule_for "restic sandbox"; then printf 'INFO\tdoctor-expected.sh allows a restic sandbox warning here that is gone: its rule can go\n'; fi
			fi
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
	if grep -q '^\[WARN\] api socket' <<<"$text"; then
		printf 'FAIL\tdoctor: %s\n' "$(grep -m1 '^\[WARN\] api socket' <<<"$text" | cut -c1-240)"
	else
		printf 'PASS\tdoctor: no api socket warning\n'
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
section "cleanup"
cleanup
info "the accounts, sites, shares, targets and restored snapshots of the checks are gone$([ "$composer_ours" = 1 ] && echo ', and composer')"
doctor_section
selinux_section
total
