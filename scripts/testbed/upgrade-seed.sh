#!/usr/bin/env bash
# upgrade-seed.sh <vm name>
#
# Runs as root on a testbed VM that carries the previous release (testbed.sh
# upgrade pipes it over ssh after that release's own bootstrap.sh) and makes,
# with that release's own mp, what a package upgrade has to carry along:
#
#   tbupown   an SFTP-only account with the site tbupown.<vm>.test (php-fpm),
#             a probe and a marker file in it, the folder shared/
#   tbupsh    an account with a shell that gets shared/ as fromown (up to
#             0.8.19 mounted inside its home, which it owns)
#   tbupsf    an SFTP-only account that gets it as well (in its chroot)
#   apa.tbupown.<vm>.test   Debian/Ubuntu: a site in Apache mode (up to 0.8.19
#             on the shared apache2.service)
#   tbupbak   a local backup target, /var/backups/tbup/repo, with one backup
#             of user:tbupown
#   composer  its wrapper as that release writes it
#
# and leaves /root/tbup.env (the release, the marker, the snapshot) for
# checks.sh, which TB_CARRIED=1 makes look at all of it after the upgrade.
# Only commands and flags that 0.8.19 already has. Passwords are random and
# go to mp on stdin; the one mp backup target add makes up and prints goes
# to /dev/null, as nothing needs it — the panel restores through itself.
set -euo pipefail
export MP_LANG=en DEBIAN_FRONTEND=noninteractive
umask 077
vm=${1:?usage: upgrade-seed.sh <vm name>}
# shellcheck disable=SC1091
. /etc/os-release
deb=0
case " $ID ${ID_LIKE:-} " in *debian* | *ubuntu*) deb=1 ;; esac
www=/var/www
own=tbupown sh=tbupsh sf=tbupsf name=fromown
dom=$own.$vm.test apa=apa.$own.$vm.test
doc=$www/$own/data/www/$dom
out=$(mktemp /root/tbup-seed.XXXXXX)
trap 'rm -f "$out"' EXIT

log() { printf '\033[35m[seed %s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
secret() { printf '%sAa1-\n' "$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')"; }
put() { # put <owner> <path> <content>
	printf '%s\n' "$3" | install -o "$1" -g "$(id -gn "$1")" -m 0644 /dev/stdin "$2"
}
# step <label> <command...>: quiet, but the tail of what failed is shown
step() {
	local label=$1
	shift
	log "$label"
	if ! "$@" </dev/null >"$out" 2>&1; then
		echo "--- $label failed ---" >&2
		tail -20 "$out" >&2
		exit 1
	fi
}

from=$(mp version | awk 'NR == 1 {print $2}')
log "mp $from on $PRETTY_NAME"
mark=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')

for u in "$own" "$sh" "$sf"; do
	if mp user show "$u" </dev/null >/dev/null 2>&1; then
		echo "$u exists already: upgrade-seed.sh runs once on a fresh deploy (testbed.sh upgrade resets the VM)" >&2
		exit 1
	fi
done
log "mp user add $own, $sh (shell), $sf"
secret | mp user add "$own" --password-stdin >/dev/null
secret | mp user add "$sh" --shell --password-stdin >/dev/null
secret | mp user add "$sf" --password-stdin >/dev/null
step "mp site add $dom" mp site add "$dom" --user "$own" --ssl none --no-https-redirect
put "$own" "$doc/tbup.php" '<?php echo "tbup-ok ", PHP_VERSION, "\n";'
put "$own" "$doc/tbup-marker.txt" "tbup-$mark"
step "mp user share add $sh (shared/ as $name)" mp user share add "$sh" --site "$dom" --path shared --name "$name"
step "mp user share add $sf (shared/ as $name)" mp user share add "$sf" --site "$dom" --path shared --name "$name"
put "$own" "$doc/shared/tbup-shared.txt" "tbup-shared-$mark"
for p in "$www/$sh/$name" "$www/$sf/$name"; do
	log "$p: $(findmnt -rn -o SOURCE --mountpoint "$p" 2>/dev/null || echo 'not a mount point') ($(systemd-escape --path --suffix=mount "$p") $(systemctl is-active "$(systemd-escape --path --suffix=mount "$p")" 2>&1))"
done

if [ "$deb" = 1 ]; then
	if ! mp stack list --json </dev/null 2>/dev/null | tr -d ' \n' | grep -q '"name":"apache"[^}]*"installed":true'; then
		echo "Apache is not installed: testbed.sh upgrade deploys $from with TB_EXTRA=apache on Debian and Ubuntu" >&2
		exit 1
	fi
	step "mp site add $apa --mode apache" mp site add "$apa" --user "$own" --mode apache --ssl none --no-https-redirect
	put "$own" "$www/$own/data/www/$apa/tbup.php" '<?php echo "tbup-ok ", PHP_VERSION, "\n";'
	log "apache2.service $(systemctl is-active apache2.service 2>&1)"
fi

# The password this release makes up for the repository is printed on
# stdout: straight to /dev/null, not into a file. The panel keeps it and
# restores through itself.
log "mp backup target add tbupbak (local)"
mp backup target add tbupbak --type local --repo /var/backups/tbup/repo </dev/null >/dev/null
step "mp backup run --scope user:$own --target tbupbak" mp backup run --scope "user:$own" --target tbupbak
snap=$(mp backup list --target tbupbak --json </dev/null | python3 -c 'import json, sys
runs = [b for b in json.load(sys.stdin) if b.get("status") == "done" and b.get("snapshot_id")]
print(runs[0]["snapshot_id"] if runs else "")')
[ -n "$snap" ] || { echo "mp backup list: no snapshot of the backup just made" >&2; exit 1; }
log "snapshot ${snap:0:8}"

if [ ! -x /usr/local/bin/composer ]; then
	step "mp stack install composer" mp stack install composer
fi

umask 077
cat >/root/tbup.env <<EOF
# written by upgrade-seed.sh with mp $from, read by checks.sh (TB_CARRIED=1)
from=$from
mark=$mark
snap=$snap
EOF
log "seeded with mp $from: $own, $sh, $sf$([ "$deb" = 1 ] && echo ", $apa"), tbupbak, composer"
