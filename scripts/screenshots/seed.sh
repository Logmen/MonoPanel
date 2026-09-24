#!/bin/sh
# Примерный сервер для скриншотов документации: ставит последний релиз тем же
# способом, что и README, поднимает стек и заводит данные на example.com / example.org.
# Запускается от root на чистой машине, например на площадке:
#   make testbed-reset VM=ubuntu2404
#   ssh mp-ubuntu2404 sh -s < scripts/screenshots/seed.sh
# Пароли, которые печатают --generate, нигде не сохраняются: скриншоты снимаются по токену.
set -eu

step() { printf '\n== %s\n' "$*"; }

step "hostname"
hostnamectl set-hostname web-01

step "panel"
if ! command -v mp >/dev/null 2>&1; then
	curl -fsSL https://raw.githubusercontent.com/Logmen/MonoPanel/main/packaging/install.sh | sh
fi
mp setup --hostname panel.example.com

step "stack"
mp stack install nginx
mp stack install percona
mp php install 8.4
mp php install 7.4
mp stack install apache
mp stack install fail2ban
mp stack install memcached
mp stack install composer
mp stack install git

step "accounts"
mp user add alex --email alex@example.com --generate --shell
mp user add shop --email shop@example.com --generate
mp user add maria --email maria@example.org --generate

step "sites"
# Сайты — на адресе из диапазона для документации (RFC 5737), чтобы на скриншотах
# не было адреса площадки; наружу он не маршрутизируется, nginx просто слушает его.
dev=$(ip route show default | awk '{print $5; exit}')
ip addr add 203.0.113.10/32 dev "$dev" 2>/dev/null || true
mp site add example.com --user alex --www --preset wordpress --ssl none --ip 203.0.113.10
mp cms install example.com wordpress --title "Example Blog" --force
mp site add shop.example.com --user shop --preset opencart --ssl none --ip 203.0.113.10
mp cms install shop.example.com opencart --title "Example Shop" --force
mp site add old.example.com --user alex --php 7.4 --mode apache --ssl none --ip 203.0.113.10
mp app add api --user maria --command "/usr/bin/python3 -m http.server 3000" --description "бэкенд app.example.org"
mp site add app.example.org --user maria --mode proxy --backend http://127.0.0.1:3000 --ssl none --ip 203.0.113.10

step "databases"
mp db create analytics --user alex
mp db create crm --user shop

step "cron"
mp cron add --user alex --schedule "*/5 * * * *" --command "php ~/data/www/example.com/wp-cron.php" --comment "WordPress cron"
mp cron add --user shop --schedule "@daily" --command "find ~/data/tmp -type f -mtime +7 -delete" --comment "чистка tmp"

step "mail"
mp mail install --hostname mail.example.com
mp mail domain add example.com --user alex
mp mail box add ivan@example.com --name "Иван Петров" --quota 2048
mp mail box add info@example.com --name "Example" --quota 1024
mp mail alias add sales@example.com ivan@example.com info@example.com

step "firewall"
mp firewall enable
mp firewall allow --port 3306 --source 10.10.0.0/24 --comment "MySQL для офиса"
mp firewall ban 203.0.113.45

step "backups"
mp backup target add local --repo /var/backups/monopanel --schedule daily
mp backup run --target local

step "done"
mp status
