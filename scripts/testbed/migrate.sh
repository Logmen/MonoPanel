#!/usr/bin/env bash
# migrate.sh <source vm> <target vm>
#
# Moves a real account between two testbed panels and checks that everything
# arrived: the account with both password hashes, the site with its PHP file
# answering over HTTP, the database with its rows and its user's password,
# the cron job. Runs locally over the ssh aliases (mp-<name>); both VMs need
# a deployed panel with nginx, PHP and a database engine.
#
#   scripts/testbed/migrate.sh ubuntu2404 debian13
#
# Leaves the account on both sides for inspection; `make testbed-reset` clears it.
set -uo pipefail
export PATH="$HOME/go/bin:$HOME/.local/go/bin:$PATH"

src="mp-${1:?source vm}"; dst="mp-${2:?target vm}"
root=$(cd "$(dirname "$0")/../.." && pwd)
suffix=$(date +%H%M%S)
login="mig$suffix"; domain="mig-$suffix.test"; password="Mig-$suffix-passw0rd"
results=()
log() { printf '\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
check() { # check <label> <command...>: records ok/FAIL, never aborts
	local label=$1; shift
	if "$@" >/dev/null 2>&1; then results+=("ok    $label"); else results+=("FAIL  $label"); log "FAIL: $label"; fi
}
S() { ssh -o BatchMode=yes "$src" "$@"; }
D() { ssh -o BatchMode=yes "$dst" "$@"; }

src_ip=$(S 'hostname -I | cut -d" " -f1'); dst_ip=$(D 'hostname -I | cut -d" " -f1')
src_url=$(S 'mp config show --json' | (cd "$root" && go run ./scripts/jsonfield panel-url))
log "source $src ($src_url) → target $dst ($dst_ip): account $login, site $domain"

# ---------------------------------------------------------- source side ----
set -e
S "mp user add $login --password '$password' --shell" >/dev/null
S "mp site add $domain --user $login --ssl none" >/dev/null
S "install -o $login -g $login -m 0644 /dev/stdin /var/www/$login/data/www/$domain/probe.php" <<<'<?php echo "migrated-ok ", PHP_VERSION;'
db_json=$(S "mp db create shop --user $login --json")
db_pass=$(sed -n 's/.*"password": *"\([^"]*\)".*/\1/p' <<<"$db_json")
S "mysql ${login}_shop -e 'CREATE TABLE t (id INT); INSERT INTO t VALUES (42);'"
S "mp cron --user $login add --schedule '@daily' --command 'echo migrated' --comment 'migration test'" >/dev/null
set +e
src_shadow=$(S "getent shadow $login | cut -d: -f2")
check "source serves probe.php" grep -q migrated-ok <(curl -s -m 10 -H "Host: $domain" "http://$src_ip/probe.php")

# ---------------------------------------------------------------- move ----
token=$(S "mp migrate grant user:$login --json" | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
[ -n "$token" ] || { log "no token from $src"; exit 1; }
plan=$(D "mp migrate plan --source $src_url --token $token --scope user:$login --insecure --json")
check "plan is ok" grep -q '"ok": *true' <<<"$plan"
log "plan: $(grep -o '"text": *"[^"]*"' <<<"$plan" | sed 's/"text": *//' | tr '\n' ' ')"
log "mp migrate run on $dst"
if ! D "mp migrate run --source $src_url --token $token --scope user:$login --insecure"; then
	results+=("FAIL  migrate run")
fi

# --------------------------------------------------------- target side ----
check "unix account exists" D "id $login"
check "unix password hash moved" [ "$(D "getent shadow $login | cut -d: -f2")" = "$src_shadow" ]
check "panel login works" grep -q '"login"' <(curl -sk -m 10 -X POST "https://$dst_ip:8443/api/v1/auth/login" -H 'Content-Type: application/json' -d "{\"login\":\"$login\",\"password\":\"$password\"}")
check "site is active" grep -q '"status": *"active"' <(D "mp site show $domain --json")
check "probe.php owned by $login" [ "$(D "stat -c %U /var/www/$login/data/www/$domain/probe.php")" = "$login" ]
check "target serves probe.php" grep -q migrated-ok <(curl -s -m 10 -H "Host: $domain" "http://$dst_ip/probe.php")
check "database rows moved" grep -q 42 <(D "mysql -N ${login}_shop -e 'SELECT id FROM t'")
check "database password moved" D "mysql -N -u ${login}_shop -p'$db_pass' -e 'SELECT 1'"
check "cron job moved" grep -q 'echo migrated' <(D "crontab -u $login -l")
check "doctor has no failures" sh -c "! $(printf %q ssh) $dst 'mp doctor --json' | grep -q '\"status\": *\"fail\"'"

echo; printf '%s\n' "${results[@]}"; echo
if printf '%s\n' "${results[@]}" | grep -q '^FAIL'; then echo "migration $src → $dst: FAILED"; exit 1; fi
echo "migration $src → $dst: all checks passed"
