#!/usr/bin/env bash
# Installs real CMSs into sites created with the panel's presets and checks
# that each works and that the preset's nginx rules hold. Runs on the VM as
# root (testbed.sh cms copies it there):
#
#   cms.sh <vmname> wordpress|joomla|opencart ...
#
# One panel user "cms", a site <cms>.<vm>.<TB_DNS_LABEL>.<TB_ZONE> per CMS
# (HTTP only: the VMs sit in a private network), a database cms_<cms>.
# WordPress installs through wp-cli, Joomla and OpenCart through their own
# CLI installers; the admin credentials are printed on the CREDS lines.
set -u
vm=$1; shift
zone=${TB_DNS_LABEL:-tb}.${TB_ZONE:?TB_ZONE is required}
user=cms
# the newest PHP branch the panel installed, and the matching CLI binary
VER=$(mp php list 2>/dev/null | awk 'NR>1 && $3=="installed"{print $1}' | sort -V | tail -1); VER=${VER:-8.4}
PHP=$(for b in php$VER php${VER/./} php; do command -v $b 2>/dev/null && break; done | head -1)
ok(){ echo "ok    $*"; }
fail(){ echo "FAIL  $*"; }
code(){ curl -s -o /dev/null -m 30 -w '%{http_code}' "$@"; }
body(){ curl -s -m 30 "$@"; }
rnd(){ echo "$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 14)Aa1!"; }  # every class validate_password wants
echo "vm=$vm php=$VER via $PHP ($($PHP -r 'echo PHP_VERSION;'))"
# ------------------------------------------------------------- user ----
if ! mp user list --json 2>/dev/null | grep -q "\"login\": *\"$user\""; then
  upw=$(rnd); echo "$upw" | mp user add $user --password-stdin >/dev/null 2>&1 || { fail "user add"; exit 1; }
fi
ok "user $user"
site(){ # site <sub> <preset> -> creates site+db, sets DOC/DOMAIN/DB/DBPW
  local sub=$1 preset=$2
  DOMAIN="$sub.$vm.$zone"; DOC="/var/www/$user/data/www/$DOMAIN"; DB="${user}_$sub"; DBPW=$(rnd)
  if [ ! -d "$DOC" ]; then
    out=$(mp site add "$DOMAIN" --user $user --php $VER --preset "$preset" --ssl none --no-https-redirect 2>&1) || { fail "site add $DOMAIN: $(echo "$out" | tail -1)"; return 1; }
  fi
  ok "site $DOMAIN (preset $preset)"
  mp db rm "$DB" >/dev/null 2>&1 || true
  out=$(mp db create "$sub" --user $user --password "$DBPW" 2>&1) || { fail "db create $DB: $(echo "$out" | tail -1)"; return 1; }
  ok "db $DB"
  rm -rf "${DOC:?}"/* "${DOC:?}"/.[!.]* 2>/dev/null
  return 0
}
asuser(){ runuser -u $user -- "$@"; }
APW="Adm$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 12)!1"
# --------------------------------------------------------- wordpress ----
do_wordpress(){
  site wp wordpress || return
  curl -fsSL -o /root/wordpress.tar.gz https://wordpress.org/latest.tar.gz || { fail "download wordpress"; return; }
  curl -fsSL -o /root/wp-cli.phar https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar || { fail "download wp-cli"; return; }
  tar -xzf /root/wordpress.tar.gz -C "$DOC" --strip-components=1 && chown -R $user:$user "$DOC" || { fail "unpack wordpress"; return; }
  cp /root/wp-cli.phar /var/www/$user/wp-cli.phar; chown $user:$user /var/www/$user/wp-cli.phar
  wp(){ asuser $PHP /var/www/$user/wp-cli.phar --path="$DOC" "$@"; }
  wp config create --dbname="$DB" --dbuser="$DB" --dbpass="$DBPW" --dbhost=localhost --skip-check >/dev/null 2>&1 || { fail "wp config create"; return; }
  out=$(wp core install --url="http://$DOMAIN" --title="WP $vm" --admin_user=admin --admin_password="$APW" --admin_email=admin@example.com --skip-email 2>&1) || { fail "wp core install: $(echo "$out" | tail -1)"; return; }
  ok "wordpress installed $(wp core version 2>/dev/null)"
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -q 'wp-content' && ok "wp home 200" || fail "wp home $(code http://$DOMAIN/)"
  [ "$(code http://$DOMAIN/wp-login.php)" = 200 ] && ok "wp login page" || fail "wp login page"
  c=$(code http://$DOMAIN/wp-admin/); [ "$c" = 302 ] && ok "wp-admin redirects to login" || fail "wp-admin $c"
  c=$(code -X POST http://$DOMAIN/xmlrpc.php); [ "$c" = 403 ] && ok "xmlrpc.php closed ($c)" || fail "xmlrpc.php $c"
  echo '<?php echo "PHP RAN";' > "$DOC/wp-content/uploads/evil.php"; chown $user:$user "$DOC/wp-content/uploads/evil.php"
  c=$(code http://$DOMAIN/wp-content/uploads/evil.php); r=$(body http://$DOMAIN/wp-content/uploads/evil.php); [ "$c" = 403 ] || [ "$c" = 404 ] && ! echo "$r" | grep -q 'PHP RAN' && ok "php in uploads denied ($c)" || fail "php in uploads $c"
  c=$(code "http://$DOMAIN/sample-page/"); [ "$c" = 200 ] && ok "pretty permalink" || fail "pretty permalink $c"
  echo "CREDS wordpress http://$DOMAIN/wp-admin/ admin $APW"
}
# ------------------------------------------------------------ joomla ----
do_joomla(){
  site joomla joomla || return
  tag=$(curl -sI -m 20 https://github.com/joomla/joomla-cms/releases/latest | tr -d '\r' | sed -n 's|^location: .*/tag/||p')
  curl -fsSL -o /root/joomla.tar.gz "https://github.com/joomla/joomla-cms/releases/download/$tag/Joomla_${tag}-Stable-Full_Package.tar.gz" || { fail "download joomla $tag"; return; }
  tar -xzf /root/joomla.tar.gz -C "$DOC" && chown -R $user:$user "$DOC" || { fail "unpack joomla"; return; }
  out=$(cd "$DOC" && asuser $PHP installation/joomla.php install --site-name="Joomla $vm" --admin-user=Admin --admin-username=admin --admin-password="$APW" --admin-email=admin@example.com --db-type=mysqli --db-host=localhost --db-user="$DB" --db-pass="$DBPW" --db-name="$DB" --db-prefix=j_ --db-encryption=0 --no-interaction 2>&1) || { fail "joomla cli install: $(echo "$out" | grep -v '^\s*$' | tail -2 | tr '\n' ' ')"; return; }
  rm -rf "$DOC/installation"
  ok "joomla $tag installed"
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -qi 'joomla' && ok "joomla home 200" || fail "joomla home $(code http://$DOMAIN/)"
  c=$(code http://$DOMAIN/administrator/index.php); [ "$c" = 200 ] && ok "administrator" || fail "administrator $c"
  c=$(code http://$DOMAIN/configuration.php); [ "$c" = 403 ] || [ "$c" = 404 ] && ok "configuration.php closed ($c)" || fail "configuration.php $c"
  c=$(code http://$DOMAIN/api/index.php/v1/content/articles); [ "$c" = 401 ] || [ "$c" = 403 ] && ok "/api answers ($c)" || fail "/api $c"
  echo '<?php echo "PHP RAN";' > "$DOC/images/evil.php"; chown $user:$user "$DOC/images/evil.php"
  c=$(code http://$DOMAIN/images/evil.php); ! body http://$DOMAIN/images/evil.php | grep -q 'PHP RAN' && ok "php in images denied ($c)" || fail "php in images $c"
  c=$(code http://$DOMAIN/index.php/component/users/login); [ "$c" = 200 ] && ok "SEF route" || fail "SEF route $c"
  echo "CREDS joomla http://$DOMAIN/administrator/ admin $APW"
}
# ---------------------------------------------------------- opencart ----
do_opencart(){
  site opencart opencart || return
  tag=$(curl -sI -m 20 https://github.com/opencart/opencart/releases/latest | tr -d '\r' | sed -n 's|^location: .*/tag/||p')
  curl -fsSL -o /root/opencart.zip "https://github.com/opencart/opencart/releases/download/$tag/opencart-$tag.zip" || { fail "download opencart $tag"; return; }
  command -v unzip >/dev/null || { (apt-get -qq install -y unzip || dnf -q -y install unzip) >/dev/null 2>&1; }
  rm -rf /root/oc && mkdir -p /root/oc && unzip -q /root/opencart.zip -d /root/oc && cp -a /root/oc/*/upload/. "$DOC"/ 2>/dev/null || cp -a /root/oc/upload/. "$DOC"/ || { fail "unpack opencart"; return; }
  cp "$DOC/config-dist.php" "$DOC/config.php"; cp "$DOC/admin/config-dist.php" "$DOC/admin/config.php"; chown -R $user:$user "$DOC"
  # cp -a from /root keeps the admin_home_t SELinux label: relabel to the docroot's own
  command -v restorecon >/dev/null && restorecon -R "$DOC" >/dev/null 2>&1
  out=$(cd "$DOC" && asuser $PHP install/cli_install.php install --username admin --email admin@example.com --password "$APW" --http_server "http://$DOMAIN/" --db_driver mysqli --db_hostname localhost --db_username "$DB" --db_password "$DBPW" --db_database "$DB" --db_port 3306 --db_prefix oc_ 2>&1) || { fail "opencart cli install: $(echo "$out" | tail -2 | tr '\n' ' ')"; return; }
  echo "$out" | grep -qi 'success' || { fail "opencart cli install: $(echo "$out" | tail -2 | tr '\n' ' ')"; return; }
  rm -rf "$DOC/install"
  ok "opencart $tag installed"
  [ "$(code http://$DOMAIN/)" = 200 ] && body http://$DOMAIN/ | grep -qi 'opencart\|Your Store' && ok "opencart home 200" || fail "opencart home $(code http://$DOMAIN/)"
  c=$(code "http://$DOMAIN/admin/"); [ "$c" = 200 ] && ok "admin" || fail "admin $c"
  c=$(code http://$DOMAIN/system/config/catalog.php); [ "$c" = 403 ] || [ "$c" = 404 ] && ok "system/ closed ($c)" || fail "system/ $c"
  c=$(code http://$DOMAIN/storage/); [ "$c" = 403 ] || [ "$c" = 404 ] && ok "storage/ closed ($c)" || fail "storage/ $c"
  c=$(code http://$DOMAIN/catalog/view/template/common/home.twig); [ "$c" = 403 ] || [ "$c" = 404 ] && ok ".twig not served ($c)" || fail ".twig $c"
  c=$(code "http://$DOMAIN/desktops"); [ "$c" = 200 ] && ok "SEO url via _route_" || fail "SEO url $c"
  echo "CREDS opencart http://$DOMAIN/admin/ admin $APW"
}
for cms in "$@"; do echo "=== $cms"; do_$cms; done
