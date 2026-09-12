#!/usr/bin/env bash
# Installs the CMSs through the panel itself (mp cms install) into sites
# created with the matching presets and checks that each works and that the
# preset's nginx rules hold. Runs on the VM as root (testbed.sh cms copies it):
#
#   cms.sh <vmname> wordpress|joomla|opencart|bitrix ...
#
# One panel user "cms", a site <cms>.<vm>.<TB_DNS_LABEL>.<TB_ZONE> per CMS
# (HTTP only: the VMs sit in a private network). A site that already has
# files is reinstalled with --force. The admin credentials the panel returns
# are printed on the CREDS lines.
set -u
vm=$1; shift
zone=${TB_DNS_LABEL:-tb}.${TB_ZONE:?TB_ZONE is required}
user=cms
ok(){ echo "ok    $*"; }
fail(){ echo "FAIL  $*"; }
code(){ curl -s -o /dev/null -m 60 -w '%{http_code}' "$@"; }
body(){ curl -s -m 60 "$@"; }
rnd(){ echo "$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 14)Aa1!"; }
echo "vm=$vm panel=$(mp version | head -1 | cut -d' ' -f2)"
if ! mp user list --json 2>/dev/null | grep -q "\"login\": *\"$user\""; then
  echo "$(rnd)" | mp user add $user --password-stdin >/dev/null 2>&1 || { fail "user add"; exit 1; }
fi
ok "user $user"
# site <sub> <preset>: the site, then mp cms install; sets DOMAIN, ADMIN_URL, APW
install(){
  local sub=$1 cms=$2
  DOMAIN="$sub.$vm.$zone"; DOC="/var/www/$user/data/www/$DOMAIN"
  if [ ! -d "$DOC" ]; then
    out=$(mp site add "$DOMAIN" --user $user --preset "$cms" --ssl none --no-https-redirect 2>&1) || { fail "site add $DOMAIN: $(echo "$out" | tail -1)"; return 1; }
  fi
  ok "site $DOMAIN"
  start=$(date +%s)
  out=$(mp cms install "$DOMAIN" "$cms" --force --json 2>/root/cms-install.err); rc=$?
  if [ $rc -ne 0 ]; then fail "mp cms install $cms: $(tail -3 /root/cms-install.err | tr '\n' ' ' | cut -c1-300)"; return 1; fi
  APW=$(echo "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("admin_password",""))' 2>/dev/null)
  ADMIN_URL=$(echo "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["admin_url"])' 2>/dev/null)
  ver=$(mp site show "$DOMAIN" --json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("cms","?"), d.get("cms_version",""))' 2>/dev/null)
  ok "mp cms install $cms: $ver in $(( $(date +%s) - start ))s"
  echo "CREDS $cms $ADMIN_URL admin $APW"
}
do_wordpress(){
  install wp wordpress || return
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -q 'wp-content' && ok "wp home 200" || fail "wp home $(code http://$DOMAIN/)"
  [ "$(code http://$DOMAIN/wp-login.php)" = 200 ] && ok "wp login page" || fail "wp login page"
  c=$(code http://$DOMAIN/wp-admin/); [ "$c" = 302 ] && ok "wp-admin redirects to login" || fail "wp-admin $c"
  c=$(code -X POST http://$DOMAIN/xmlrpc.php); [ "$c" = 403 ] && ok "xmlrpc.php closed ($c)" || fail "xmlrpc.php $c"
  echo '<?php echo "PHP RAN";' > "$DOC/wp-content/uploads/evil.php" 2>/dev/null || { mkdir -p "$DOC/wp-content/uploads"; echo '<?php echo "PHP RAN";' > "$DOC/wp-content/uploads/evil.php"; }; chown $user:$user "$DOC/wp-content/uploads/evil.php"
  c=$(code http://$DOMAIN/wp-content/uploads/evil.php); ! body http://$DOMAIN/wp-content/uploads/evil.php | grep -q 'PHP RAN' && ok "php in uploads denied ($c)" || fail "php in uploads $c"
  c=$(code "http://$DOMAIN/sample-page/"); [ "$c" = 200 ] && ok "pretty permalink" || fail "pretty permalink $c"
}
do_joomla(){
  install joomla joomla || return
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -qi 'joomla' && ok "joomla home 200" || fail "joomla home $(code http://$DOMAIN/)"
  c=$(code http://$DOMAIN/administrator/index.php); [ "$c" = 200 ] && ok "administrator" || fail "administrator $c"
  c=$(code http://$DOMAIN/configuration.php); { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ok "configuration.php closed ($c)" || fail "configuration.php $c"
  c=$(code http://$DOMAIN/api/index.php/v1/content/articles); { [ "$c" = 401 ] || [ "$c" = 403 ]; } && ok "/api answers ($c)" || fail "/api $c"
  echo '<?php echo "PHP RAN";' > "$DOC/images/evil.php"; chown $user:$user "$DOC/images/evil.php"
  c=$(code http://$DOMAIN/images/evil.php); ! body http://$DOMAIN/images/evil.php | grep -q 'PHP RAN' && ok "php in images denied ($c)" || fail "php in images $c"
  c=$(code http://$DOMAIN/index.php/component/users/login); [ "$c" = 200 ] && ok "SEF route" || fail "SEF route $c"
  [ -d "$DOC/installation" ] && fail "installation/ still there" || ok "installation/ removed"
}
do_opencart(){
  install opencart opencart || return
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -qi 'opencart\|Your Store' && ok "opencart home 200" || fail "opencart home $(code http://$DOMAIN/)"
  c=$(code "http://$DOMAIN/admin/"); [ "$c" = 200 ] && ok "admin" || fail "admin $c"
  c=$(code http://$DOMAIN/system/config/catalog.php); { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ok "system/ closed ($c)" || fail "system/ $c"
  c=$(code http://$DOMAIN/storage/); { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ok "storage/ closed ($c)" || fail "storage/ $c"
  c=$(code http://$DOMAIN/catalog/view/template/common/home.twig); { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ok ".twig not served ($c)" || fail ".twig $c"
  c=$(code "http://$DOMAIN/desktops"); [ "$c" = 200 ] && ok "SEO url via _route_" || fail "SEO url $c"
  [ -d "$DOC/install" ] && fail "install/ still there" || ok "install/ removed"
}
do_bitrix(){
  install bitrix bitrix || return
  hdr=$(curl -s -i -m 60 "http://$DOMAIN/"); c=$(echo "$hdr" | head -1 | awk '{print $2}')
  if [ "$c" = 200 ] && echo "$hdr" | grep -qi 'X-Powered-CMS: Bitrix' && ! echo "$hdr" | grep -q '__wizard_form'; then ok "bitrix home 200, X-Powered-CMS: Bitrix, title: $(echo "$hdr" | grep -oE '<title>[^<]*' | head -1 | cut -c8-60)"; else fail "bitrix home $c (wizard form: $(echo "$hdr" | grep -c '__wizard_form'))"; fi
  c=$(code "http://$DOMAIN/bitrix/admin/"); b=$(body "http://$DOMAIN/bitrix/admin/"); [ "$c" = 200 ] && echo "$b" | grep -qiE 'USER_LOGIN|Авторизация|Authorization' && ok "admin login form" || fail "admin $c"
  for u in /bitrix/php_interface/dbconn.php /bitrix/modules/main/include/prolog.php /bitrix/.settings.php /bitrix/cache/x.php; do c=$(code "http://$DOMAIN$u"); b=$(body "http://$DOMAIN$u"); if { [ "$c" = 403 ] || [ "$c" = 404 ]; } && ! echo "$b" | grep -q 'DBLogin'; then ok "closed $u ($c)"; else fail "$u $c"; fi; done
  echo '<?php echo "PHP RAN";' > "$DOC/upload/evil.php"; chown $user:$user "$DOC/upload/evil.php"
  c=$(code "http://$DOMAIN/upload/evil.php"); ! body "http://$DOMAIN/upload/evil.php" | grep -q 'PHP RAN' && ok "php in upload denied ($c)" || fail "php in upload executed"
  c=$(code "http://$DOMAIN/no-such-page-$RANDOM/"); b=$(body "http://$DOMAIN/no-such-page-$RANDOM/"); [ "$c" = 404 ] && echo "$b" | grep -qi 'bitrix' && ok "urlrewrite 404 handled by Bitrix" || fail "urlrewrite $c"
  php=$(mp site php "$DOMAIN" 2>/dev/null); for kv in "short_open_tag On" "max_input_vars 20000" "memory_limit 512M"; do set -- $kv; echo "$php" | grep -qE "^$1\s+$2" && ok "php $1=$2" || fail "php $1: $(echo "$php" | grep -E "^$1" | head -1)"; done
}
for cms in "$@"; do echo "=== $cms"; do_$cms; done
