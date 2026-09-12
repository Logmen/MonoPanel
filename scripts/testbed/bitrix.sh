#!/usr/bin/env bash
# Installs the 1C-Bitrix trial ("Старт", start_encode.tar.gz from 1c-bitrix.ru)
# into a site with the bitrix preset and checks the site and the preset's
# rules. Runs on the VM as root (testbed.sh cms copies it there):
#
#   bitrix.sh <vmname>
#
# The install wizard is driven over HTTP by bitrix-wizard.py (Bitrix has no
# CLI installer): trial licence, existing database cms_bitrix, administrator
# "admin", the bundled "furniture company" demo solution. Bitrix's own
# requirements step is part of the run — it fails on wrong PHP settings.
set -u
vm=$1; zone=${TB_DNS_LABEL:-tb}.${TB_ZONE:?TB_ZONE is required}; user=cms
VER=$(mp php list 2>/dev/null | awk 'NR>1 && $3=="installed"{print $1}' | sort -V | tail -1); VER=${VER:-8.4}
ok(){ echo "ok    $*"; }; fail(){ echo "FAIL  $*"; }
code(){ curl -s -o /dev/null -m 60 -w '%{http_code}' "$@"; }; body(){ curl -s -m 60 "$@"; }
rnd(){ echo "$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 14)Aa1!"; }
DOMAIN="bitrix.$vm.$zone"; DOC="/var/www/$user/data/www/$DOMAIN"; DB="${user}_bitrix"; DBPW=$(rnd); APW="Bx$(tr -dc A-Za-z0-9 </dev/urandom | head -c 12)!1"
echo "vm=$vm php=$VER"
mp user list --json 2>/dev/null | grep -q "\"login\": *\"$user\"" || { echo "$(rnd)" | mp user add $user --password-stdin >/dev/null 2>&1 || { fail "user add"; exit 1; }; }
if [ ! -d "$DOC" ]; then
  out=$(mp site add "$DOMAIN" --user $user --php $VER --preset bitrix --ssl none --no-https-redirect 2>&1) || { fail "site add: $(echo "$out" | tail -1)"; exit 1; }
fi
ok "site $DOMAIN (preset bitrix)"
mp db rm "$DB" >/dev/null 2>&1; mp db create bitrix --user $user --password "$DBPW" >/dev/null 2>&1 || { fail "db create"; exit 1; }
ok "db $DB"
[ -s /root/bx.tar.gz ] || curl -fsSL -o /root/bx.tar.gz https://www.1c-bitrix.ru/download/files/start_encode.tar.gz || { fail "download start_encode.tar.gz"; exit 1; }
rm -rf "${DOC:?}"/* "${DOC:?}"/.[!.]* 2>/dev/null
tar -xzf /root/bx.tar.gz -C "$DOC" && chown -R $user:$user "$DOC" || { fail "unpack"; exit 1; }
command -v restorecon >/dev/null && restorecon -R "$DOC" >/dev/null 2>&1
[ -f "$DOC/.access.php" ] && ok "unpacked $(du -sh "$DOC" | cut -f1)" || { fail "unpack: no .access.php"; exit 1; }
O="{\"__wiz_agree_license\":\"Y\",\"__wiz_user_name\":\"Test\",\"__wiz_user_surname\":\"Admin\",\"__wiz_email\":\"admin@example.com\",\"__wiz_host\":\"localhost\",\"__wiz_create_user\":\"N\",\"__wiz_user\":\"$DB\",\"__wiz_password\":\"$DBPW\",\"__wiz_create_database\":\"N\",\"__wiz_database\":\"$DB\",\"__wiz_login\":\"admin\",\"__wiz_admin_password\":\"$APW\",\"__wiz_admin_password_confirm\":\"$APW\",\"__wiz_admin_email\":\"admin@example.com\",\"__wiz_selected_wizard\":\"bitrix.sitecorporate:bitrix:corp_furniture\"}"
start=$(date +%s)
timeout 2400 python3 /root/bitrix-wizard.py "http://$DOMAIN" "$O" 300 > /root/bxwiz.out 2>&1; rc=$?
steps=$(grep -oE '^\[[0-9]+\] step=[a-z_]+' /root/bxwiz.out | sed -E 's/.*step=//' | tr '\n' ',' )
echo "wizard: exit=$rc $(( $(date +%s) - start ))s steps: $steps"
grep -E 'errors|error on|stuck|giving up|unexpected|no wizard form' /root/bxwiz.out | grep -v 'display_errors' | head -5 | sed 's/^/  /'
hdr=$(curl -s -i -m 60 "http://$DOMAIN/"); c=$(echo "$hdr" | head -1 | awk '{print $2}')
if [ "$c" = 200 ] && echo "$hdr" | grep -qi 'X-Powered-CMS: Bitrix' && ! echo "$hdr" | grep -q '__wizard_form'; then ok "bitrix home 200, X-Powered-CMS: Bitrix, title: $(echo "$hdr" | grep -oE '<title>[^<]*' | head -1 | cut -c8-60)"; else fail "bitrix home $c (wizard form: $(echo "$hdr" | grep -c '__wizard_form'))"; fi
c=$(code "http://$DOMAIN/bitrix/admin/"); b=$(body "http://$DOMAIN/bitrix/admin/"); [ "$c" = 200 ] && echo "$b" | grep -qiE 'USER_LOGIN|Авторизация|Authorization' && ok "admin login form" || fail "admin $c"
for u in /bitrix/php_interface/dbconn.php /bitrix/modules/main/include/prolog.php /bitrix/.settings.php /bitrix/cache/x.php; do c=$(code "http://$DOMAIN$u"); b=$(body "http://$DOMAIN$u"); if { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ! echo "$b" | grep -q 'DBLogin'; then ok "closed $u ($c)"; else fail "$u $c"; fi; done
echo '<?php echo "PHP RAN";' > "$DOC/upload/evil.php"; chown $user:$user "$DOC/upload/evil.php"
c=$(code "http://$DOMAIN/upload/evil.php"); ! body "http://$DOMAIN/upload/evil.php" | grep -q 'PHP RAN' && ok "php in upload denied ($c)" || fail "php in upload executed"
c=$(code "http://$DOMAIN/no-such-page-$RANDOM/"); b=$(body "http://$DOMAIN/no-such-page-$RANDOM/"); [ "$c" = 404 ] && echo "$b" | grep -qi 'bitrix' && ok "urlrewrite 404 handled by Bitrix" || fail "urlrewrite $c"
php=$(mp site php "$DOMAIN" 2>/dev/null); for kv in "short_open_tag On" "max_input_vars 20000" "memory_limit 512M"; do set -- $kv; echo "$php" | grep -qE "^$1\s+$2" && ok "php $1=$2" || fail "php $1: $(echo "$php" | grep -E "^$1" | head -1)"; done
echo "CREDS bitrix http://$DOMAIN/bitrix/admin/ admin $APW"
