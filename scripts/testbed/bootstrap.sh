#!/usr/bin/env bash
# bootstrap.sh <package|binary> <panel address>
#
# Runs on a fresh testbed VM as root: installs MonoPanel from a .deb / .rpm or
# a bare binary, runs `mp setup`, installs the web stack through the panel the
# way a user would, and finishes with `mp doctor`. Idempotent: on a second run
# the package is upgraded and installed components are left alone.
#
#   TB_PHP    PHP branch to install (default 8.4)
#   TB_DB     database engine: percona (default), mysql or none
#   TB_EXTRA  extra components, space-separated: apache fail2ban firewall mail
set -euo pipefail

pkg=${1:?package or binary}
addr=${2:?panel address (ip or hostname)}
: "${TB_PHP:=8.4}"
: "${TB_DB:=percona}"
: "${TB_EXTRA:=}"

log() { printf '\033[35m[%s %s]\033[0m %s\n' "$(hostname)" "$(date +%H:%M:%S)" "$*" >&2; }
export DEBIAN_FRONTEND=noninteractive

command -v cloud-init >/dev/null && cloud-init status --wait >/dev/null 2>&1 || true
. /etc/os-release
log "$PRETTY_NAME, kernel $(uname -r)"

# ------------------------------------------------------------- install ----
case "$pkg" in
*.deb)
	log "installing $(basename "$pkg")"
	apt-get -q update >/dev/null
	apt-get -q -y install "$pkg" >/dev/null
	;;
*.rpm)
	log "installing $(basename "$pkg")"
	# dnf refuses to reinstall the same version; upgrade or install as needed.
	rpm -q monopanel >/dev/null 2>&1 && dnf -q -y reinstall "$pkg" >/dev/null 2>&1 || dnf -q -y install "$pkg" >/dev/null
	;;
*)
	log "installing binary"
	install -m 0755 "$pkg" /usr/bin/monopanel
	ln -sf /usr/bin/monopanel /usr/bin/mp
	;;
esac
mp version

# --------------------------------------------------------------- setup ----
log "mp setup --hostname $addr"
out=$(mp setup --hostname "$addr" --json)
if grep -q '"admin_created": *true' <<<"$out"; then
	# Shown once, never written anywhere: the tests use tokens from the root socket.
	echo "admin password: $(sed -n 's/.*"admin_password": *"\([^"]*\)".*/\1/p' <<<"$out")"
fi
grep -o 'https://[^"]*' <<<"$out" | head -1 | sed 's/^/panel: /'

# The JSON is pretty-printed, so the newlines go before matching one object.
installed() { mp stack list --json 2>/dev/null | tr -d ' \n' | grep -q "\"name\":\"$1\"[^}]*\"installed\":true"; }
php_installed() { mp php list --json 2>/dev/null | tr -d ' \n' | grep -q "\"version\":\"$1\"[^}]*\"status\":\"installed\""; }

# --------------------------------------------------------------- stack ----
step() { # step <label> <command...>
	local label=$1; shift
	log "$label"
	if ! "$@" >/dev/null 2>"/tmp/bootstrap-$$.err"; then
		echo "--- $label failed ---" >&2
		tail -40 "/tmp/bootstrap-$$.err" >&2
		exit 1
	fi
}
installed nginx || step "mp stack install nginx" mp stack install nginx
# The wanted branch may not exist on this OS (Ubuntu 26.04 has only 8.5 until
# the PPA builds for it): then take the newest branch the panel calls available.
if ! php_installed "$TB_PHP"; then
	if ! mp php install "$TB_PHP" >/dev/null 2>"/tmp/bootstrap-$$.err"; then
		if grep -q "is not available on this OS" "/tmp/bootstrap-$$.err"; then
			log "PHP $TB_PHP: $(grep -o 'is not available on this OS.*' "/tmp/bootstrap-$$.err" | head -1 | cut -c1-140)"
			TB_PHP=$(mp php list --available --json | tr -d ' \n' | grep -o '"version":"[0-9.]*","support":"[a-z]*","available":true' | sed 's/"version":"\([0-9.]*\)".*/\1/' | sort -V | tail -1)
			[ -n "$TB_PHP" ] || { echo "no PHP branch is available here" >&2; exit 1; }
			step "mp php install $TB_PHP (newest available)" mp php install "$TB_PHP"
		else
			echo "--- mp php install $TB_PHP failed ---" >&2; tail -40 "/tmp/bootstrap-$$.err" >&2; exit 1
		fi
	fi
fi
if [ "$TB_DB" != none ]; then
	installed "$TB_DB" || step "mp stack install $TB_DB" mp stack install "$TB_DB"
fi
for x in $TB_EXTRA; do
	case "$x" in
	apache | fail2ban) installed "$x" || step "mp stack install $x" mp stack install "$x" ;;
	firewall) step "mp firewall enable" mp firewall enable ;;
	mail) step "mp mail install" mp mail install ;;
	*) echo "unknown extra: $x" >&2; exit 2 ;;
	esac
done

# -------------------------------------------------------------- report ----
log "mp doctor"
mp doctor || true
mp stack list
mp php list
