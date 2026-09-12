#!/usr/bin/env bash
# The source machines for the foreign-panel migration (docs/07 §6): a
# BitrixVM (bitrix-env 9 on AlmaLinux 9) and a FASTPANEL 2 (Debian 12), each
# on its own VM of the testbed (scripts/testbed/pve.sh, SOURCES table).
#
#   sources.sh install bitrixvm|fastpanel        put the panel on a fresh VM (testbed.sh up <name> first)
#   sources.sh seed bitrixvm|fastpanel [FROM]    populate it with sites taken from a MonoPanel VM
#                                                (FROM defaults to alma9; needs `testbed.sh cms FROM` there)
#   sources.sh migrate bitrixvm|fastpanel DST    mp migrate plan + run from the MonoPanel VM DST, with checks
#
# Passwords the seeding invents land in .dev/sources.txt (git-ignored).
# FASTPANEL's API and UI refuse to work without a licence from its billing
# account, so its account, sites, database, cron and certificate rows are
# written straight into its SQLite the way the panel itself writes them
# (checked against a live FASTPANEL 1.11); the files, nginx, php-fpm and
# MySQL parts are real.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
env_file="$root/.dev/testbed.env"
# shellcheck disable=SC1090
[ -f "$env_file" ] && . "$env_file"
: "${TB_ZONE:?TB_ZONE in $env_file: the sites need public names}"
: "${TB_DNS_LABEL:=tb}"
: "${TB_PREFIX:?TB_PREFIX in $env_file}"
: "${TB_IP_BASE:=200}"
secrets="$root/.dev/sources.txt"
log() { printf '\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { echo "sources.sh: $*" >&2; exit 1; }
vm() { echo "mp-$1"; }
fqdn() { echo "$1.$TB_DNS_LABEL.$TB_ZONE"; }
ip_of() { ssh -o BatchMode=yes "$(vm "$1")" 'hostname -I | cut -d" " -f1'; }
password() { echo "$(openssl rand -base64 15 | tr -d '/+=' | cut -c1-16)$1"; }
remember() { umask 077; echo "$*" >> "$secrets"; }
recall() { sed -n "s/^$1: //p" "$secrets" | tail -1; }

# ------------------------------------------------------------- install ----

install_bitrixvm() {
	local h pw
	h=$(vm bitrixvm)
	pw=$(recall "bitrixvm mysql root password")
	[ -n "$pw" ] || { pw=$(password 'Aa1!'); remember "bitrixvm mysql root password: $pw"; }
	# The installer refuses to run with SELinux on: it switches the config
	# to disabled and asks for a reboot, so run it twice around one.
	ssh -o BatchMode=yes "$h" "hostnamectl set-hostname $(fqdn bitrixvm); grep -q '$(fqdn bitrixvm)' /etc/hosts || echo '$(ip_of bitrixvm) $(fqdn bitrixvm)' >> /etc/hosts; dnf -y -q install wget >/dev/null; wget -q http://repo.bitrix.info/dnf/bitrix-env-9.sh -O /root/bitrix-env-9.sh; chmod +x /root/bitrix-env-9.sh; /root/bitrix-env-9.sh -s -M '$pw' >/dev/null 2>&1 || true; getenforce"
	if [ "$(ssh -o BatchMode=yes "$h" getenforce)" != Disabled ]; then
		log "bitrixvm: rebooting to disable SELinux (the installer insists)"
		ssh -o BatchMode=yes "$h" systemctl reboot || true
		sleep 20
		for _ in $(seq 1 40); do ssh -o BatchMode=yes -o ConnectTimeout=5 "$h" true 2>/dev/null && break; sleep 5; done
	fi
	log "bitrixvm: installing bitrix-env 9 (takes ~15 minutes)"
	ssh -o BatchMode=yes "$h" "/root/bitrix-env-9.sh -s -p -H $(fqdn bitrixvm) -M '$pw'" >/dev/null 2>&1 || true
	ssh -o BatchMode=yes "$h" 'rpm -q bitrix-env' || die "bitrix-env did not install; see /opt/webdir/logs on the VM"
	# The management pool: without it bx-sites cannot manage sites.
	ssh -o BatchMode=yes "$h" "grep -q bitrix-hosts /etc/ansible/hosts 2>/dev/null || /opt/webdir/bin/wrapper_ansible_conf -a create -H $(fqdn bitrixvm) -I \$(ip -o -4 route get 1.1.1.1 | sed -n 's/.* dev \([^ ]*\).*/\1/p') -o json" >/dev/null
	log "bitrixvm: ready"
}

install_fastpanel() {
	local h out
	h=$(vm fastpanel)
	log "fastpanel: installing FASTPANEL 2 (takes ~10 minutes)"
	out=$(ssh -o BatchMode=yes "$h" 'export DEBIAN_FRONTEND=noninteractive; apt-get -qq update && apt-get -qq -y install wget >/dev/null && wget -q https://repo.fastpanel.direct/install_fastpanel.sh -O - | bash -' 2>&1) || true
	pw=$(grep -o 'Password: [A-Za-z0-9+/=]*' <<<"$out" | tail -1 | cut -d' ' -f2)
	[ -n "$pw" ] || die "FASTPANEL did not print its admin password: $(tail -5 <<<"$out")"
	remember "fastpanel admin: fastuser / $pw  https://$(ip_of fastpanel):8888/"
	log "fastpanel: ready (fastuser password in $secrets)"
}

# ---------------------------------------------------------------- seed ----

# link_vms gives FROM a temporary key into TO so the copy runs over the LAN.
link_vms() {
	local pub
	pub=$(ssh -o BatchMode=yes "$(vm "$1")" 'rm -f /root/.ssh/seed_tmp /root/.ssh/seed_tmp.pub; ssh-keygen -q -t ed25519 -N "" -f /root/.ssh/seed_tmp; cat /root/.ssh/seed_tmp.pub')
	ssh -o BatchMode=yes "$(vm "$2")" "grep -q seed_tmp /root/.ssh/authorized_keys 2>/dev/null || echo '$pub seed_tmp' >> /root/.ssh/authorized_keys"
}
unlink_vms() {
	ssh -o BatchMode=yes "$(vm "$1")" 'rm -f /root/.ssh/seed_tmp /root/.ssh/seed_tmp.pub'
	ssh -o BatchMode=yes "$(vm "$2")" 'sed -i "/ seed_tmp$/d" /root/.ssh/authorized_keys'
}
sshx() { echo "ssh -i /root/.ssh/seed_tmp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR root@$1"; }

# seed_bitrixvm copies the Bitrix site mp cms install made on FROM into
# /home/bitrix/www and its database into sitemanager: a BitrixVM the way
# people run it, main site with server_name _ and the credentials in
# bitrix/.settings.php.
seed_bitrixvm() {
	local from=$1 h site dstip
	h=$(vm bitrixvm); dstip=$(ip_of bitrixvm)
	site="/var/www/cms/data/www/bitrix.$(fqdn "$from")"
	ssh -o BatchMode=yes "$(vm "$from")" "test -d $site/bitrix" || die "no Bitrix site on $from: testbed.sh cms $from bitrix"
	link_vms "$from" bitrixvm
	ssh -o BatchMode=yes "$h" 'cp -n /home/bitrix/www/bitrix/php_interface/dbconn.php /root/dbconn.bitrixenv.php; cp -n /home/bitrix/www/bitrix/.settings.php /root/settings.bitrixenv.php'
	log "bitrixvm: files from $from"
	ssh -o BatchMode=yes "$(vm "$from")" "tar -C $site --exclude=./bitrix/cache --exclude=./bitrix/managed_cache --exclude=./bitrix/stack_cache -cf - . | $(sshx "$dstip") 'tar -C /home/bitrix/www -xf -'"
	log "bitrixvm: database from $from"
	ssh -o BatchMode=yes "$(vm "$from")" "mysqldump --single-transaction --quick --routines --triggers cms_bitrix | $(sshx "$dstip") 'mysql sitemanager'"
	unlink_vms "$from" bitrixvm
	ssh -o BatchMode=yes "$h" 'python3 - <<"PY"
import re
pw = re.search(r"'"'"'password'"'"'\s*=>\s*'"'"'((?:[^'"'"'\\\\]|\\\\.)*)'"'"'", open("/root/settings.bitrixenv.php").read()).group(1)
f = "/home/bitrix/www/bitrix/.settings.php"
s = open(f).read()
i = s.index("'"'"'connections'"'"'")
head, tail = s[:i], s[i:]
for k, v in (("host", "localhost"), ("database", "sitemanager"), ("login", "bitrix0"), ("password", pw)):
    tail = re.sub(r"'"'"'%s'"'"'\s*=>\s*'"'"'[^'"'"']*'"'"'" % k, lambda m, v=v: "'"'"'%s'"'"' => '"'"'%s'"'"'" % (k, v), tail, 1)
open(f, "w").write(head + tail)
PY
cp /root/dbconn.bitrixenv.php /home/bitrix/www/bitrix/php_interface/dbconn.php
mysql sitemanager -e "UPDATE b_lang SET SERVER_NAME=\"main.'"$(fqdn bitrixvm)"'\"; UPDATE b_option SET VALUE=\"main.'"$(fqdn bitrixvm)"'\" WHERE MODULE_ID=\"main\" AND NAME=\"server_name\";"
chown -R bitrix:bitrix /home/bitrix/www'
	local pw
	pw=$(recall "bitrixvm unix user bitrix")
	[ -n "$pw" ] || { pw=$(password 'Dd4!'); remember "bitrixvm unix user bitrix: $pw"; }
	ssh -o BatchMode=yes "$h" "echo 'bitrix:$pw' | chpasswd"
	ssh -o BatchMode=yes "$h" "code=\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: main.$(fqdn bitrixvm)' http://127.0.0.1/); echo \"main site: HTTP \$code\"; [ \"\$code\" = 200 ]" || die "the seeded Bitrix site does not answer"
	log "bitrixvm: seeded (main site main.$(fqdn bitrixvm), database sitemanager)"
}

# seed_fastpanel makes the account shop with a WordPress site (from FROM),
# a plain PHP site with docroot public/ and an allow-list, a database,
# cron, a real Let's Encrypt certificate (DNS-01 through FROM) and a
# mailbox row.
seed_fastpanel() {
	local from=$1 h dstip wp shoppw dbpw d
	h=$(vm fastpanel); dstip=$(ip_of fastpanel)
	wp="/var/www/cms/data/www/wp.$(fqdn "$from")"
	ssh -o BatchMode=yes "$(vm "$from")" "test -f $wp/wp-config.php" || die "no WordPress site on $from: testbed.sh cms $from wordpress"
	shoppw=$(recall "fastpanel unix user shop"); [ -n "$shoppw" ] || { shoppw=$(password 'Aa1!'); remember "fastpanel unix user shop: $shoppw"; }
	dbpw=$(recall "fastpanel db shop_wp"); [ -n "$dbpw" ] || { dbpw=$(password 'Bb2!'); remember "fastpanel db shop_wp: $dbpw"; }
	link_vms "$from" fastpanel
	log "fastpanel: account shop, sites, database"
	ssh -o BatchMode=yes "$h" "set -e
id shop >/dev/null 2>&1 || { mkdir -p /var/www/shop; useradd -d /var/www/shop/data -m -k /usr/local/fastpanel2/skel -s /bin/bash shop; }
echo 'shop:$shoppw' | chpasswd
chown shop:fastsecure /var/www/shop; chmod 501 /var/www/shop
mkdir -p /var/www/shop/data/{www,logs,bin,tmp,email,php-bin}
ln -sfn /usr/bin/php8.2 /var/www/shop/data/bin/php
mkdir -p /var/www/shop/data/www/wp.$(fqdn fastpanel) /var/www/shop/data/www/app.$(fqdn fastpanel)/public
printf '%s\n' '<?php echo \"app ok \", php_sapi_name(), \" \", PHP_VERSION, \"\\n\";' > /var/www/shop/data/www/app.$(fqdn fastpanel)/public/index.php
printf '%s\n' '<?php return [\"log\" => \"/var/www/shop/data/www/app.$(fqdn fastpanel)/app.log\"];' > /var/www/shop/data/www/app.$(fqdn fastpanel)/config.php
chown -R shop:shop /var/www/shop/data
mysql -e \"CREATE DATABASE IF NOT EXISTS shop_wp CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; CREATE USER IF NOT EXISTS 'shop_wp'@'localhost' IDENTIFIED WITH mysql_native_password BY '$dbpw'; GRANT ALL ON shop_wp.* TO 'shop_wp'@'localhost'; FLUSH PRIVILEGES;\""
	ssh -o BatchMode=yes "$(vm "$from")" "tar -C $wp -cf - . | $(sshx "$dstip") 'tar -C /var/www/shop/data/www/wp.$(fqdn fastpanel) -xf - && chown -R shop:shop /var/www/shop/data/www'"
	ssh -o BatchMode=yes "$(vm "$from")" "mysqldump --single-transaction --quick cms_wordpress | $(sshx "$dstip") 'mysql shop_wp'"
	ssh -o BatchMode=yes "$h" "set -e
f=/var/www/shop/data/www/wp.$(fqdn fastpanel)/wp-config.php
sed -i \"s/define( 'DB_NAME', '[^']*' );/define( 'DB_NAME', 'shop_wp' );/; s/define( 'DB_USER', '[^']*' );/define( 'DB_USER', 'shop_wp' );/; s/define( 'DB_PASSWORD', '[^']*' );/define( 'DB_PASSWORD', '$dbpw' );/\" \$f
mysql shop_wp -e \"UPDATE wp_options SET option_value='http://wp.$(fqdn fastpanel)' WHERE option_name IN ('siteurl','home')\""
	log "fastpanel: nginx, php-fpm, crontab"
	for d in wp app; do
		local docroot="/var/www/shop/data/www/$d.$(fqdn fastpanel)" allow="" alias=""
		[ $d = app ] && { docroot="$docroot/public"; allow="        allow $TB_PREFIX.0/24;
        allow 83.97.77.0/24;
        deny all;"; alias=" alias.$(fqdn fastpanel)"; }
		ssh -o BatchMode=yes "$h" "mkdir -p /etc/nginx/fastpanel2-sites/shop; cat > /etc/php/8.2/fpm/pool.d/$d.$(fqdn fastpanel).conf <<'POOL'
[$d.$(fqdn fastpanel)]
user = shop
group = shop
listen = /var/run/$d.$(fqdn fastpanel).sock
listen.owner = www-data
listen.group = www-data
pm = ondemand
pm.max_children = 5
pm.process_idle_timeout = 10s
POOL
cat > /etc/nginx/fastpanel2-sites/shop/$d.$(fqdn fastpanel).conf <<'CONF'
server {
    server_name $d.$(fqdn fastpanel)$alias ;
    listen $dstip:80;
    charset utf-8;
    set \$root_path $docroot;
    root \$root_path;
    disable_symlinks if_not_owner from=\$root_path;
    location / {
        index index.php index.html;
        try_files \$uri \$uri/ /index.php?\$args;
$allow
    }
    location ~ \\.php\$ {
        include /etc/nginx/fastcgi_params;
        fastcgi_pass unix:/var/run/$d.$(fqdn fastpanel).sock;
        fastcgi_param SCRIPT_FILENAME \$realpath_root\$fastcgi_script_name;
        fastcgi_param DOCUMENT_ROOT \$realpath_root;
$allow
    }
    include /etc/nginx/fastpanel2-includes/*.conf;
    error_log /var/www/shop/data/logs/$d.$(fqdn fastpanel)-frontend.error.log;
    access_log /var/www/shop/data/logs/$d.$(fqdn fastpanel)-frontend.access.log;
}
CONF"
	done
	ssh -o BatchMode=yes "$h" "systemctl restart php8.2-fpm && nginx -t >/dev/null 2>&1 && systemctl reload nginx
crontab -u shop - <<'CRON'
PATH=/var/www/shop/data/bin:/usr/local/bin:/usr/bin:/bin
*/10 * * * * /var/www/shop/data/bin/php /var/www/shop/data/www/wp.$(fqdn fastpanel)/wp-cron.php
# clean tmp
0 3 * * * /usr/bin/find /var/www/shop/data/tmp -type f -mtime +7 -delete
CRON"
	log "fastpanel: Let's Encrypt certificate for wp.$(fqdn fastpanel) through $from (DNS-01)"
	ssh -o BatchMode=yes "$(vm "$from")" "test -f /var/lib/monopanel/certs/wp.$(fqdn fastpanel)/fullchain.pem || mp ssl issue wp.$(fqdn fastpanel) --dns cf >/dev/null"
	local cert="wp.$(fqdn fastpanel)_$(date +%Y-%m-%d-%H-%M)_01"
	ssh -o BatchMode=yes "$(vm "$from")" "cat /var/lib/monopanel/certs/wp.$(fqdn fastpanel)/fullchain.pem" | ssh -o BatchMode=yes "$h" "mkdir -p /var/www/httpd-cert; cat > /var/www/httpd-cert/$cert.crt"
	ssh -o BatchMode=yes "$(vm "$from")" "cat /var/lib/monopanel/certs/wp.$(fqdn fastpanel)/privkey.pem" | ssh -o BatchMode=yes "$h" "cat > /var/www/httpd-cert/$cert.key; chmod 600 /var/www/httpd-cert/*.key"
	unlink_vms "$from" fastpanel
	log "fastpanel: rows in fastpanel2.db"
	sed "s/@ZONE@/$(fqdn fastpanel)/g; s/@CERT@/$cert/g" "$root/scripts/testbed/fastpanel-rows.py" | ssh -o BatchMode=yes "$h" 'python3 -'
	ssh -o BatchMode=yes "$h" "code=\$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: wp.$(fqdn fastpanel)' http://$dstip/); echo \"wp: HTTP \$code\"; curl -s -H 'Host: app.$(fqdn fastpanel)' http://$dstip/"
	log "fastpanel: seeded (account shop, sites wp.$(fqdn fastpanel) and app.$(fqdn fastpanel), database shop_wp)"
}

# ------------------------------------------------------------- migrate ----

# key_access lets DST's root ssh into the source VM (the migration runs
# from the target panel).
key_access() {
	local pub
	pub=$(ssh -o BatchMode=yes "$(vm "$1")" 'test -f /root/.ssh/id_ed25519 || ssh-keygen -q -t ed25519 -N "" -f /root/.ssh/id_ed25519; cat /root/.ssh/id_ed25519.pub')
	ssh -o BatchMode=yes "$(vm "$2")" "grep -qF '$pub' /root/.ssh/authorized_keys || echo '$pub' >> /root/.ssh/authorized_keys"
}

check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fails=$((fails + 1)); fi; }

migrate_bitrixvm() {
	local dst=$1 src srcip dstip main pw dbpw fails=0
	src=$(vm bitrixvm); srcip=$(ip_of bitrixvm); dstip=$(ip_of "$dst"); main="main.$(fqdn bitrixvm)"
	key_access "$dst" bitrixvm
	ssh -o BatchMode=yes "$(vm "$dst")" "mp php list | grep -q '^8.2 ' || mp php install 8.2 >/dev/null"
	log "$dst: mp migrate plan/run --from bitrixvm"
	ssh -o BatchMode=yes "$(vm "$dst")" "mp migrate plan --from bitrixvm --source root@$srcip --domain $main && mp migrate run --from bitrixvm --source root@$srcip --domain $main" | tail -25
	pw=$(ssh -o BatchMode=yes "$src" 'getent shadow bitrix | cut -d: -f2')
	dbpw=$(ssh -o BatchMode=yes "$src" "python3 -c \"import re;print(re.search(r\\\"'password'\\s*=>\\s*'([^']*)'\\\", open('/home/bitrix/www/bitrix/.settings.php').read()).group(1))\"")
	log "$dst: checks"
	check "site answers with Bitrix over the new server" "ssh -o BatchMode=yes $(vm "$dst") \"for i in 1 2 3 4 5 6; do curl -sI --resolve $main:80:$dstip http://$main/ | grep -q 'X-Powered-CMS: Bitrix' && exit 0; sleep 5; done; exit 1\""
	check "old /home/bitrix paths rewritten in dbconn.php" "ssh -o BatchMode=yes $(vm "$dst") \"grep -q '/var/www/bitrix/.bx_temp' /var/www/bitrix/data/www/$main/bitrix/php_interface/dbconn.php\""
	check "database user logs in with the password from .settings.php" "ssh -o BatchMode=yes $(vm "$dst") \"mysql -u bitrix0 -p'$dbpw' -N -e 'select 1' sitemanager\" >/dev/null 2>&1"
	check "unix password hash identical" "[ \"\$(ssh -o BatchMode=yes $(vm "$dst") 'getent shadow bitrix | cut -d: -f2')\" = '$pw' ]"
	check "site listed with the bitrix preset" "ssh -o BatchMode=yes $(vm "$dst") 'mp site list' | grep '$main' | grep -q bitrix"
	echo "bitrixvm → $dst: $fails failures"
	[ "$fails" -eq 0 ]
}

migrate_fastpanel() {
	local dst=$1 src srcip dstip z pw dbpw fails=0
	src=$(vm fastpanel); srcip=$(ip_of fastpanel); dstip=$(ip_of "$dst"); z=$(fqdn fastpanel)
	key_access "$dst" fastpanel
	ssh -o BatchMode=yes "$(vm "$dst")" "mp php list | grep -q '^8.2 ' || mp php install 8.2 >/dev/null"
	log "$dst: mp migrate plan/run --from fastpanel"
	ssh -o BatchMode=yes "$(vm "$dst")" "mp migrate plan --from fastpanel --source root@$srcip --scope user:shop && mp migrate run --from fastpanel --source root@$srcip --scope user:shop" | tail -30
	pw=$(ssh -o BatchMode=yes "$src" 'getent shadow shop | cut -d: -f2')
	dbpw=$(recall "fastpanel db shop_wp")
	log "$dst: checks"
	check "WordPress answers over HTTPS with the migrated certificate" "ssh -o BatchMode=yes $(vm "$dst") \"for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w '%{http_code} %{ssl_verify_result}' --resolve wp.$z:443:$dstip https://wp.$z/ | grep -q '^200 0' && exit 0; sleep 5; done; exit 1\""
	check "app site serves from docroot public/" "ssh -o BatchMode=yes $(vm "$dst") \"curl -s --resolve app.$z:80:$dstip http://app.$z/\" | grep -q 'app ok'"
	check "alias answers" "ssh -o BatchMode=yes $(vm "$dst") \"curl -s -o /dev/null -w '%{http_code}' --resolve alias.$z:80:$dstip http://alias.$z/\" | grep -q 200"
	check "allow-list rendered into nginx" "ssh -o BatchMode=yes $(vm "$dst") \"grep -q 'deny  *all' /etc/nginx/monopanel/sites/app.$z.conf\""
	check "database user logs in with the old (native) password hash" "ssh -o BatchMode=yes $(vm "$dst") \"mysql -u shop_wp -p'$dbpw' -N -e 'select 1' shop_wp\" >/dev/null 2>&1"
	check "unix password hash identical" "[ \"\$(ssh -o BatchMode=yes $(vm "$dst") 'getent shadow shop | cut -d: -f2')\" = '$pw' ]"
	check "two cron jobs with data/bin/php in place" "[ \"\$(ssh -o BatchMode=yes $(vm "$dst") 'crontab -l -u shop | grep -c \"^[0-9*]\"')\" = 2 ] && ssh -o BatchMode=yes $(vm "$dst") 'test -L /var/www/shop/data/bin/php'"
	check "certificate imported as valid" "ssh -o BatchMode=yes $(vm "$dst") 'mp ssl list' | grep 'wp.$z' | grep -q valid"
	echo "fastpanel → $dst: $fails failures"
	[ "$fails" -eq 0 ]
}

# ---------------------------------------------------------------- main ----

cmd=${1:-}; shift || true
case "$cmd" in
install) case "${1:-}" in bitrixvm) install_bitrixvm ;; fastpanel) install_fastpanel ;; *) die "install bitrixvm|fastpanel" ;; esac ;;
seed) case "${1:-}" in bitrixvm) seed_bitrixvm "${2:-alma9}" ;; fastpanel) seed_fastpanel "${2:-alma9}" ;; *) die "seed bitrixvm|fastpanel [FROM]" ;; esac ;;
migrate) [ $# -eq 2 ] || die "migrate bitrixvm|fastpanel DST"; case "$1" in bitrixvm) migrate_bitrixvm "$2" ;; fastpanel) migrate_fastpanel "$2" ;; *) die "migrate bitrixvm|fastpanel DST" ;; esac ;;
*) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
