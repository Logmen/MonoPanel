# 04. CLI, TUI и REST API

## 1. Принцип

Один API. Web UI, CLI, TUI и внешние интеграции (биллинг, скрипты) ходят в одни и те же эндпоинты `/api/v1`. CLI и TUI — тонкие клиенты: Go-клиент `internal/client` с типами из `internal/apitypes`, из тех же типов `huma` генерирует описание OpenAPI 3.1; Web UI обращается к тем же эндпоинтам через небольшую обёртку над `fetch`. Функция реализуется один раз — в API — и поэтому доступна везде.

Три клиента умеют одно и то же. У каждой операции API есть команда `mp` и место в Web UI, каждую команду `mp` предлагает меню TUI, каждое поле запроса задаётся флагом или аргументом `mp` (а значит, и полем формы TUI) и есть в запросе, который собирает страница Web UI. Обычному аккаунту ни Web UI, ни TUI не предлагают действий и флагов администратора. Это проверяет `go test ./internal/parity/` — он входит в `make test` и CI: тест сравнивает OpenAPI-описание сервера, дерево команд `mp`, меню TUI для администратора и для обычного аккаунта и исходники `web/src` с манифестом `internal/parity/manifest_*.go` и называет, чего и где не хватает.

Исключения намеренные, и их немного (`internal/parity/exempt.go` и списки в самом тесте):

- вход и выход в браузере (`POST /auth/login`, `/auth/logout`) — только Web UI: `mp` и TUI авторизуют каждый запрос по uid процесса на сокете или API-токеном;
- консоль (`POST /system/console`) — только Web UI: она сама запускает `mp`, а в терминале есть `mp` и TUI;
- справочник API (`/api/v1/docs`, `openapi.json`, `/schemas`) — страницы, а не операции; `GET /health` — проба самих клиентов (экран входа, `mp setup`), версию показывают `mp status` и шапка TUI;
- серверная половина переезда (`GET /migrate/plan`, `state`, `files`, `dump`) — её вызывает задача принимающей панели по токену переезда, а не человек;
- `mp update trust` — только от root на сервере: ключ подписи релизов не проходит через процесс панели, Web UI показывает его состояние только для чтения; `mp config show|set --local` — путь восстановления от root, когда API не запускается, обычный путь идёт через API и есть в Web UI;
- установка и служебные роли бинарника (`mp setup`, `api`, `agent`, `helper`, `fsop`, `update-run`, `app-exec`, `sandbox-exec`, `demo`; `mail-policy` и `mail-guard` — службы почтовой политики; `api-socket-sync` и `prune-unit-copies` — для скриптов пакета; `clean-private-tmp` — `ExecStopPost` юнита агента и запусков пакетного менеджера, она должна оставаться в каждом следующем бинарнике: запуск, начатый до обновления, и юнит агента вызывают уже новый) и удобства оболочки (`completion`, `help`, `version`);
- три чтения, которые API разрешает и обычному аккаунту, — `mp php list`, `mp web tls` и `mp dns-provider types` — в его меню TUI не входят: Web UI показывает их только администратору (ветки PHP обычный аккаунт видит как варианты в форме сайта).

Флагам `mp`, которые не задают поля запроса, — выводу (`--json`, `--no-wait`), передаче секретов (`--…-stdin`) и путям к локальным файлам (`--cert`, `--key`, `--chain`, `--set`, `mp files get|put`) — пара в Web UI не нужна: секреты и содержимое файлов он принимает своими полями, выбором файла и загрузкой.

## 2. CLI — `monopanel` (alias `mp`)

Подключение: локально — `/run/monopanel/api.sock` (авторизация по uid процесса: root — администратор, uid клиента — его аккаунт, так что клиент с SSH-доступом управляет своим хозяйством тем же `mp`; сокет открыт только root и группе `monopanel-cli`, куда панель добавляет аккаунты с shell, а запросы из процессов php-fpm, Apache и nginx отклоняются); удалённо — `--server https://host:8443 --token …` или переменные `MP_SERVER` / `MP_TOKEN`, `--insecure` или `MP_INSECURE=1` — для самоподписанного сертификата. Без API работают только `help`, `completion` и `version`, а от root на сервере — ещё `setup`, `config show|set --local` и `update trust`.

Глобальные флаги: `--json` (машинный вывод), `--no-wait` (не ждать задачу), `--server`, `--token`, `--insecure`, `--config` (путь к `config.yaml`, по умолчанию `/etc/monopanel/config.yaml` или `$MONOPANEL_CONFIG`), `--version`.

```
mp                                   # TUI-меню (в интерактивном терминале; иначе — справка)
mp setup [--hostname …] [--listen …] [--admin-login …] [--admin-password …|--password-stdin]
         [--no-start] [--install-units] [--binary …]   # от root; повторный запуск создаёт только недостающее
                                     # --install-units: перезаписать юниты в /etc/systemd/system, кроме тех, что ставит пакет
mp status                            # панель, хост, сервисы, задачи
mp doctor                            # диагностика (в т.ч. песочница restic, общие папки, Apache аккаунтов и их секрет, сокет
                                     # порта панели, переезды обновления, ждущие обновления пакетов, модули ядра)
mp whoami                            # кто вы для панели: аккаунт, роль, способ входа, токен, 2FA
mp version
mp update                            # что стоит и что вышло; после отката — что версия уже числится в базе пакетов
mp update check | apply [--version v0.9.7]    # apply ставит заново и версию, чья установка откатилась
mp update settings [--repo owner/name] [--channel stable|beta] [--check-hours 24] [--auto-apply] [--api …]
                   [--token …|--token-stdin|--clear-token]   # --repo "-" выключает обновления
mp update trust [--key <ключ форка>] [--restart] | --clear | --off   # только root на сервере; без флагов — какой ключ проверяет подпись
mp config show [--local]             # имя, адрес HTTPS-порта, журнал, число задач, каталоги
mp config set <ключ> <значение> [--restart] [--local]   # web.hostname, web.listen, log.level, log.format, jobs.workers
mp config templates                  # встроенные шаблоны конфигураций и их переопределения

mp user add <login> [--password …|--password-stdin|--generate] [--email …] [--role user|admin] [--shell] [--quota МБ]   # квота пока только хранится
mp user set <login> [--password …|--password-stdin|--generate] [--current-password …|--current-password-stdin] [--email …] [--shell|--sftp-only] [--status active|suspended]   # свой пароль меняют с текущим (спрашивается в терминале)
mp user list | show <login> | totp-reset <login>
mp user rm <login> [--purge] [--yes]
mp user share list <login> | add <login> --site <домен> --path <папка> [--name …] [--no-php] [--entry] | entry <login> <name> [--off] | rm <login> <name>   # папка сайта другого аккаунта по SFTP; entry — сессия начинается в ней; IN HOME — путь в доме, --json — ещё mount_point и статус (share.status, share.last_error: почему агент папку не смонтировал)
mp totp status | setup [--qr] | enable <код> | disable [--password-stdin]   # 2FA своего аккаунта; настроить заново — после disable

mp site add <domain> --user <login> [--php 8.4] [--mode fpm|apache|proxy] [--backend http://127.0.0.1:3000]
            [--alias www.<domain>] [--www] [--redirect-www none|to_www|to_root] [--preset wordpress|joomla|opencart|bitrix]
            [--ssl auto|none] [--no-https-redirect] [--http2=false] [--http3] [--ip …] [--ipv6 …|none] [--docroot public]
            [--ini key=value] [--allow CIDR] [--pm ondemand|dynamic|static] [--max-children 8] [--max-body 64m]
            [--static-by-nginx=false] [--allow-exec] [--security-headers] [--rate-limit 10]
mp site set <domain> [флаги site add, кроме --user и --no-https-redirect] [--rm-alias …] [--no-www]
            [--https-redirect=false] [--allow-all] [--sessions files|valkey]   # --docroot '' — корень, --preset '' — универсальный
            [--cert <id|имя>|auto]                                            # сертификат вручную (администратор); auto — выбирает панель
mp site list | show <domain> | presets
mp site apply | fix | suspend | unsuspend <domain>   # fix — владелец, права, ACL и метки SELinux файлов сайта
mp site rm <domain> [--purge]
mp site logs <domain> [--type access|error|php|slow|apache-access|apache-error] [-n 100]
mp site nginx <domain> [--set файл | --clear | --generated]   # свои директивы в server {}: проверка агентом, nginx -t и откат
mp site php <domain>                 # действующие PHP-параметры и откуда каждый взялся
mp site tls <domain> [--dns <провайдер>] [--staging] [--email …]   # сертификат на домен с алиасами, сайт переходит на HTTPS
mp site move-ip <новый-адрес> [--from <прежний>]   # после смены IP хоста: все сайты пропавших адресов разом; каждый сайт — под своей блокировкой: меняется только адрес, удалённые и удаляемые сайты пропускаются, блок вебпочты на порту переезжает вместе с её сайтом
mp selinux [enforcing|permissive] [--yes]
mp cms list
mp cms install <domain> wordpress|joomla|opencart|bitrix [--title …] [--admin-login …] [--admin-password …|--password-stdin]
               [--admin-email …] [--force]   # Битрикс: --edition start|standard|small_business|business, --solution clean|demo|<id>

mp php list [--available]            # PACKAGE — установленная сейчас версия пакета FPM, как в mp stack list
mp php install <версия> | remove <версия>
mp php ext list <версия> | enable <версия> <расширение> | disable <версия> <расширение>
mp php ini [set key=value … | unset key …]   # PHP-параметры для всех сайтов

mp stack list                        # у apache — «запущено N из M»: по Apache на аккаунт с сайтами в этом режиме; UPDATE — что
                                     # предлагает репозиторий; STATE «на заменённом бинарнике» — служба работает на старом коде
                                     # до перезапуска (mp stack upgrade перезапустит её после проверки, даже когда ставить нечего);
                                     # строка на заменённом бинарнике получает свой перезапуск: mp stack upgrade <компонент> у nginx,
                                     # PHP, Apache и сервера БД, systemctl restart <юнит> у остальных; при выключенном ежедневном
                                     # обновлении — «mp stack auto-upgrade --on»; пока переезд на Apache аккаунтов не закончен —
                                     # «сайтов в ожидании переезда: K · apache2.service <состояние>» (Apache аккаунтов без
                                     # переехавшего сайта не считаются), а остановленный общий Apache — подсказка
                                     # «systemctl start apache2.service»: ждущие сайты до тех пор отвечают 502
mp stack install nginx|apache|percona|mysql|fail2ban|memcached|valkey|jpegoptim|git|composer|sphinx
mp stack remove memcached|valkey|jpegoptim|git|composer|sphinx
mp stack memcached [--memory-mb 256] [--max-conn 2048]    # без флагов — показать
mp stack real-ip [--cloudflare|--no-cloudflare] [--from CIDR] [--clear-from]
mp stack upgrade [nginx|php|apache|db|all]   # обновить пакеты панели из их репозиториев (по умолчанию all = nginx, PHP, Apache;
                                     # сервер БД — только как db): сначала конфигурация nginx, PHP и сервера БД проверяется
                                     # работающей версией — если она уже не проходит, компонент не обновляется (его пакет
                                     # перезапустил бы службу и уронил её); после обновления — новой (nginx -t, php-fpm -t,
                                     # apache2 -t, mysqld --validate-config), затем службы перезагружаются или перезапускаются;
                                     # при ошибке проверки служба работает как была (если её пакет уже не уронил её своим
                                     # перезапуском: тогда она остаётся остановленной, задача это называет), ничего не откатывается;
                                     # перезапуск, после которого служба не поднялась, задача так и называет: «<юнит> was
                                     # restarted but did not come up»; пока не закончен переезд на Apache аккаунтов — ещё и
                                     # apache2ctl -t общего apache2.service до обновления (при ошибке Apache не обновляется) и
                                     # его перезапуск после (мягкий, если бинарник не заменён); с обновлением панели не идёт
                                     # одновременно (пришедшая второй задача ждёт в очереди)
mp stack auto-upgrade [--on|--off] [--hour 4]   # ежедневное обновление nginx, PHP и Apache (включено по умолчанию, 04:00 по
                                     # времени сервера), раз в сутки; без флагов — показать его, следующий и последний запуск
                                     # (ошибка — одной строкой, целиком — mp job show <id>)
mp valkey add cache|sessions [--user <login>] [--memory 128]   # экземпляр аккаунта: создать или сменить память (с перезапуском)
mp valkey list [--user <login>] | restart cache|sessions | rm cache|sessions   # restart опустошает кеш

mp db create <name> --user <login> [--password …|--password-stdin] [--legacy-auth] [--remote]   # база и пользователь <login>_<name>; пароль сгенерируется; --remote API пока отклоняет
mp db list | passwd <name> [--password …|--password-stdin] | rm <name>
mp db engine                         # сервер СУБД; tune — пересчитать настройки под память
mp db engine tune
mp db config [set key=value … | unset key …]   # параметры сервера MySQL; при неудачном запуске вернутся прежние
mp db phpmyadmin [status]                         # версия, порт, адрес phpMyAdmin
mp db phpmyadmin install [--port N] [--php V]     # поставить или перенести на другой порт/ветку PHP (по умолчанию 2083, новейшая подходящая PHP 7.4–8.5)
mp db phpmyadmin remove                           # убрать: временные учётки, пул, порт, файлы
mp db phpmyadmin open <name>                      # одноразовая ссылка (минута, один раз): база открывается с входом, временная учётка mp.pma.… только на неё; четвёртое открытие той же базы закрывает самый старый сеанс
# TUI: «Базы данных» → o — открыть базу в phpMyAdmin, m — экран phpMyAdmin (i — установить/перенести, d — удалить).
# API: GET/POST/DELETE /api/v1/db/phpmyadmin, POST /api/v1/databases/{name}/phpmyadmin.

mp ssl list | show <id|имя> | rm <id|имя>
mp ssl renew <id|имя> | --all
mp ssl issue <имя> [имя…] [--dns <провайдер>] [--staging] [--rsa] [--email …] [--no-auto-renew] [--directory URL]
mp ssl import [имя] --cert f --key f [--chain f] [--no-auto-renew] [--site <домен>…]   # --site: сайт держит этот сертификат
mp ssl panel                         # сертификат самой панели и запись за ним
mp ssl panel issue [--dns …] [--staging] [--rsa] [--email …] | import --cert f --key f | self-signed
mp web tls                           # какой сертификат панель отдаёт прямо сейчас
mp dns-provider types | list
mp dns-provider add <имя> --type cloudflare --cred CLOUDFLARE_DNS_API_TOKEN=…|--cred-stdin | rm <имя>

mp backup target add <имя> --type local|sftp|s3|b2|rest --repo … [--password …|--password-stdin] [--env KEY=…|--env-stdin]
                   [--schedule daily] [--keep-daily 7] [--keep-weekly 4] [--keep-monthly 3]
mp backup target list | rm <имя>
mp backup run [--target …] [--scope server|user:<login>|site:<domain>|db:<name>]
mp backup list [--target …] [-n 50] | snapshots [--target …]   # статус: running, done, partial (снимок без части файлов), failed
mp backup restore <snapshot> [--target …] [--include путь…] [--in-place]   # снимок с файлами устройств, FIFO, сокетами или
                                     # setuid/setgid в выбранных путях задача отклоняет, называя первый, — выберите пути без них
                                     # --in-place в дом удаляемого аккаунта или дерево удаляемого сайта — отказ; mp user rm и
                                     # mp site rm отказывают (409), пока такое восстановление в очереди или идёт

mp cron list | add --schedule "*/5 * * * *" --command "…" [--comment …] [--disabled]
mp cron set <id> [--schedule …] [--command …] [--comment …] [--enable|--disable] | enable <id> | disable <id> | rm <id>
mp app add <name> --command … [--workdir …] [--env KEY=…|--env-stdin] [--env-file …] [--restart always|on-failure|no]
           [--description …] [--disabled]
mp app set <name> [--command …] [--workdir …] [--env …|--env-stdin] [--env-file …] [--restart …] [--description …]
           [--enable|--disable]
mp app list | show | start | stop | restart | logs [-n 100] | rm <name>   # у cron, app и valkey: [--user <login>], без него — свой аккаунт
mp files ls|get|put|mkdir|touch|rm|mv|chmod|extract|size --user <login> <путь…>   # через helper от имени клиента; touch --force опустошает файл

mp firewall status | enable | disable | apply
mp firewall allow|deny --port 8443 [--proto tcp] [--source CIDR] [--comment …]
mp firewall rm <id> | ban <ip> | unban <ip> | trust <ip|cidr> | untrust <ip|cidr>

mp mail install [--hostname …] [--no-pop3] | status | apply   # подробно — 06-mail.md
mp mail settings [--hostname …] [--max-size МБ] [--pop3 true|false] [--port25 true|false] [--dkim true|false]   # --dkim true после установки без DKIM: затем ещё раз mp mail install (06 §12)
                 [--rbl …] [--webmail-port 2096]
mp mail domain list | add <домен> --user <login> [--send-only] [--lenient] [--no-dkim] | dkim <домен> | dns <домен> | rm <домен>
mp mail domain set <домен> [--active true|false] [--send-only true|false] [--lenient true|false]
mp mail box list [--domain …] | add <ящик> [--password …|--password-stdin] [--name …] [--quota 1024] | rm <ящик> [--purge]
mp mail box set <ящик> [--password …|--password-stdin] [--name …] [--quota …] [--active true|false]   # без флагов — новый пароль; --quota 0 — без ограничения
mp mail alias list [--domain …] | add <алиас> <куда> [куда…] | rm <алиас>   # @example.com — catch-all
mp mail webmail <домен> --user <login> [--php …] [--port 2096]   # Roundcube отдельным сайтом

mp migrate grant user:<login> [--hours 24]   # на источнике: токен на один аккаунт, только чтение; подробно — 07-migration.md
mp migrate plan | run --source https://old:8443 --token …|--token-stdin --scope user:<login> [--as <login>] [--insecure]
mp migrate plan | run --from fastpanel|bitrixvm --source root@old.example.com[:22] [--key …|--password-stdin]
                      [--scope user:<login>] [--domain …] [--as <login>]   # --domain — основной сайт BitrixVM

mp service status [unit] | start | stop | reload | restart | reload-or-restart | enable | disable <unit>
                                     # экземпляры шаблонов (кроме monopanel-apache@<логин>), цели, юниты оболочек/питания и
                                     # systemd-tmpfiles-* — только status; php-fpm, nginx, Apache: отказ, пока
                                     # /var/log/monopanel/sites не только root'а или не вычищен, php-fpm — и пока /run/monopanel/php
                                     # не только root'а (установка, обновление и удаление пакетов — тот же отказ, что у php-fpm);
                                     # monopanel-update и monopanel-pkg-* (запуски самой панели) — только status; mount-юниты ниже
                                     # веб-корня не запускаются, не перечитываются и не включаются (управляйте ими через systemctl)
mp service status|stop|start monopanel-apache@<логин>   # Apache аккаунта
mp logs <unit> [-n 100]              # журнал юнита через агент: mp logs monopanel-apache@<логин>
mp metrics [--range 1h|6h|24h|7d|30d]
mp job list [--status …] [--limit …] | show <id> | wait <id>
mp audit [-n 50] [--actor <login>] [--action auth.|token.|site.]   # журнал действий: входы, токены, изменения
mp token create [--name …] [--scopes …] [--expires <дней>] [--user <login>]
mp token list [--user …] | revoke <id>
mp webhook add --url … [--events job.failed,backup.run.*] [--secret …|--secret-stdin]
mp webhook list | test <id> | rm <id>
mp completion bash|zsh|fish|powershell   # скрипт автодополнения
```

Соглашения:
- Команды, за которыми стоит задача, по умолчанию ждут её завершения и печатают прогресс и лог (`--no-wait` — вернуть номер задачи сразу).
- Коды выхода: 0 — ok, 1 — ошибка (в том числе неизвестная команда или флаг и нехватка аргументов), 2 — неверный ввод (значение отклонила команда или API с 400/422, на stdin пусто, на вопрос нет ответа), 3 — задача завершилась с ошибкой (так же — `mp doctor` с проваленной проверкой и `mp migrate plan`, когда переезд пока невозможен), 4 — нет прав (сокет закрыт, API ответил 401/403, `--local` не от root). `mp --json doctor` завершается с 0 и при проваленной проверке: её статус — в JSON.
- `--json` печатает один JSON-объект (или массив), без прогресса и подсказок.
- Секрет не обязательно писать в командную строку, где его видно в `ps` и в истории: у каждого пароля, токена, секрета webhook, учётных данных DNS-провайдера и переменных репозитория бэкапов и app-сервиса есть пара `--…-stdin` (`--password-stdin`, `--token-stdin`, `--secret-stdin`, `--cred-stdin`, `--env-stdin`). Значение берётся из первой строки stdin, а в терминале спрашивается без эха; списки `KEY=VALUE` идут по строке на значение, до пустой строки или конца — после строки с паролем, если команда читает и его. Свой API-токен для `--server` передаётся в `MP_TOKEN`.
- `--user <login>` — чей объект. Администратор называет аккаунт (у `site add`, `db create`, `mail domain add`, `mail webmail` и `files` — обязательно), обычный аккаунт работает со своим. У `cron`, `app` и `valkey` без `--user` — свой аккаунт, а `list` администратора показывает все; у root на сокете аккаунта панели нет, и кроме `list` он указывает `--user` всегда.
- Команды, которые спрашивают подтверждение (`user rm`, `selinux permissive`), принимают `--yes`; если ответить некому (stdin закрыт), они завершаются с кодом 2.
- У `migrate plan|run` флаги `--token` и `--insecure` относятся к источнику, у `update settings` `--token` — токен репозитория с релизами; поэтому свой токен для `--server` этим командам передают в `MP_TOKEN`, а `migrate plan|run` — и `MP_INSECURE=1` вместо `--insecure`.
- Язык CLI и TUI — по локали терминала, как язык Web UI по браузеру: язык стран СНГ в `LC_ALL`, `LC_MESSAGES`, `LANG` или `LANGUAGE` — русский, любой другой, в том числе `C`/`POSIX`, — английский; `MP_LANG=ru` или `MP_LANG=en` задаёт язык явно. Сообщения самого сервера — на английском.
- Автодополнение печатает `mp completion <shell>`; в пакет оно пока не входит.
- Свои директивы nginx (`mp site nginx --set`) агент проверяет как сгенерированные: `fastcgi_pass` — только на сокет пула сайта, без `fastcgi_param PHP_VALUE/PHP_ADMIN_VALUE`; `proxy_method` — только слово, `uwsgi_modifier1` — только 0, `include` — без `/etc/nginx/default.d` и с шаблоном только в имени файла (подробно — [03 §3](03-web-stack.md#3-шаблон-nginx-режим-a-сокращённо)). Отклоняются и логи глубже `sites/<логин>/` или прямо в `sites/` (`access_log`, `error_log`), и location, который проксирует на Apache аккаунта без `include monopanel/apache/<логин>.conf;` — Apache ответил бы на его запросы 403 ([03 §3](03-web-stack.md#3-шаблон-nginx-режим-a-сокращённо)). Такой отказ API возвращает как 422 «the panel does not take these directives (nothing was changed)» — до записи и без `nginx -t`; «nginx rejected the configuration» значит, что директивы записались, `nginx -t` их не принял, и прежняя версия восстановлена.
- Строку `api socket` в `mp doctor` даёт API, о своём хосте (она же — в `--json`, на дашборде, в TUI). `mp doctor` предупреждает строкой `[WARN] api socket`, если API слушает порт сам, хотя `monopanel-api.socket` мог бы его держать (сокет упал, его остановили или он слушает не там, где `web.listen`; включён он или нет — неважно: юнит API запускает его вместе с собой), — пока API перезапускается, порт может занять любой локальный аккаунт. Причину ищут в `journalctl -u monopanel-api -u monopanel-api.socket`, сокет возвращает `mp setup`. Предупреждает и тогда, когда API работает на порту, который ему передал сокет, а сам сокет с тех пор остановлен («Остановить» на странице «Службы» или `systemctl restart monopanel-api.socket` вручную, пока API работает, — такой сокет systemd снова не запускает): как только API остановится или упадёт, порт никто не держит; сокет возвращают `mp setup` или `systemctl restart monopanel-api`. Чтобы API слушал порт сам намеренно, сокет маскируют: `systemctl mask monopanel-api.socket` — строка тогда ok, и `mp setup` его не трогает; вернуть — `systemctl unmask monopanel-api.socket`, затем `mp setup`. При установке без пакета `mp setup` пишет юнит сокета в `/etc/systemd/system`: перед `systemctl mask` этот файл удаляют. Для порта ниже `net.ipv4.ip_unprivileged_port_start` сокет нарочно выключен, и предупреждения нет. Для API, который такой строки не даёт, `mp doctor` считает её сам. Подробности проверок `mp doctor` печатает в одну строку, без управляющих последовательностей.
- Строки `mp doctor` об обновлении до 0.9.0, стеке и других механизмах 0.9.0 (тексты сервера — на английском):
  - `service apache2.service` — общий Apache 0.8.19 ещё работает (и где стоит переезд сайтов на Apache аккаунтов: задание в очереди, неудачи и время следующей попытки) или выключен, но включён в автозагрузку: при следующей загрузке он стартовал бы с общей конфигурацией 0.8.19 — `systemctl disable apache2`; остановленный, пока на нём ещё есть непереехавшие сайты, — провал: они отвечают 502, нужен `systemctl start apache2.service`;
  - `apache move` — переезд на Apache аккаунтов не закончен: какие сайты ещё обслуживает общий `apache2.service` и у каких аккаунтов свой Apache появится с первым переехавшим сайтом (до тех пор он не проверяется);
  - `backend monopanel-apache@<логин>.service` — Apache аккаунта настроен до секрета nginx: отвечает — или ответит после запуска — любому локальному процессу; исправит следующий старт панели или `mp stack install apache`;
  - `site logs` — сайты, которые ещё пишут логи в `data/logs`, и почему: задание в очереди, неудачи с ошибкой и временем следующей попытки, ожидание правила SELinux;
  - `php X.Y pool www` — штатный пул ветки ещё не пуст или изменён администратором; `php-fpm pools` — пулы с `catch_workers_output = yes`;
  - `update <строка>` (`update nginx`, `update apache`, `update php-8.4`, `update percona`) — в репозитории ждёт версия новее установленной (`mp stack upgrade nginx|php|apache` или ежедневное обновление; сервер БД — только `mp stack upgrade db`), `updates` — сводка: новее ничего нет или менеджер пакетов не сказал, что предлагают репозитории, `held <строка>` — новые версии удержаны через `apt-mark hold`, `stack upgrade` — последнее обновление не удалось; `service <юнит>` с «on the binary an upgrade replaced» — служба работает на старом коде до перезапуска; у находок об ожидающем обновлении и о службах nginx, PHP, Apache и сервера БД на старом бинарнике в Web UI — ссылка «к обновлению»;
  - `kernel modules` — есть ли правило, которое не даёт загружаться sctp, dccp, rds и tipc, и не загружен ли (не встроен ли в ядро) какой-то из них; в контейнере — предупреждение, что модули грузит ядро хоста и правило нужно на хосте ([02](02-platform-matrix.md#1-поддерживаемые-ос));
  - `restic sandbox` — как работает restic и с какой версией он запустился в песочнице бэкапа («restic starts as a backup's restic would (…)»); без Landlock, если он собран в ядро, но не включён (Oracle Linux с UEK), — ещё и как его включить; «restic does not start in its sandbox» — установленный restic в песочнице не запускается, бэкапы падают с той же ошибкой: сообщите её разработчикам панели (это сбой — `mp doctor` завершается с кодом 3); «restic took too long to start in its sandbox» — предупреждение: `restic version` не уложился в 30 с (перегруженный хост); агент повторит проверку при первом запросе состояния через 10 минут, а если тайм-аут повторяется, бэкапы будут зависать так же — смотрите нагрузку, память и диск хоста и `time restic version` вне панели;
  - `shared folders` — смонтированы ли общие папки: «N mounted» или несмонтированные с причиной (владелец подменил папку ссылкой, гость или владелец в системной группе…) и те, где смонтирован другой каталог, чем у общей папки в базе («another folder is mounted there, <каталог>»); `mp site fix <домен>` монтирует папки сайта снова ([10](10-shared-folders.md)). Ещё строка называет папки, которые агент держит, а ни одна общая папка базы не называет («… kept by the agent that no shared folder of the panel names — <точка монтирования>: in use»): их панель размонтирует при старте, как только ими никто не пользуется (`systemctl restart monopanel-api`);
  - `mail policy` — на сервере с почтой: служба `monopanel-mailpolicy` отвечает postfix на своём сокете, на EL работает `monopanel-mailguard`, и `main.cf` спрашивает политику; сбой называет, что не так, и что выполнить (`mp mail apply`, `systemctl start …`); предупреждение — конфигурация почты этой версии ещё не на месте или файл политики не записан при последнем изменении (повтор — раз в час) ([06 §11](06-mail.md#11-почтовая-политика));
  - `jobs` — упавшие за сутки задачи (сутки считаются от падения, а не от постановки в очередь: бэкап или переезд идут часами), кроме тех, что позже исправила та же работа (тот же тип и ключ блокировки, у задач стека — и компонент; переезд на Apache аккаунтов исправляет и `mp stack install apache`), и тех, чей объект удаление убрало уже после них, — сайт или аккаунт её ключа блокировки, у `mail.webmail` ещё и сайт вебпочты и, пока сайта с таким именем нет, аккаунт, для которого её просили (прошёл `site.delete` сайта или `user.delete` аккаунта, который уносит и его сайты, — поставленный в очередь после задачи или, если шёл рядом с ней под другим ключом блокировки, закончившийся уже после её старта); `site.apply` уже удалённого сайта завершается успешно («the site is gone: nothing to apply»); упавший `backup.run` считается, пока не пройдёт следующий бэкап в ту же цель, какой бы аккаунт или сайт он ни сохранял; неудачи сайтов, которые унёс `user.delete`, поставленный прежней версией панели, считаются, пока не пройдут сутки;
  - `update attempt` — установка обновления панели не удалась или откатилась; если версия уже числится в базе пакетов, строка говорит, что `mp update apply` поставит её ещё раз (сама панель её не повторяет).
- Действие над юнитом, которое агент не выполняет (у экземпляров шаблонов, целей, оболочек, юнитов питания, `systemd-tmpfiles-*` и запусков самой панели — `monopanel-update`, `monopanel-pkg-*` — только состояние; mount-юниты ниже веб-корня не запускаются, не перечитываются и не включаются; запуск, перезапуск и reload php-fpm, nginx и Apache — пока дерево логов не только root'а или не вычищено, а php-fpm — и пока каталог сокетов пулов не только root'а), `mp service` получает как отказ API с причиной агента и кодом выхода 4, а не как ошибку агента.

## 3. TUI-меню

- Запуск: `mp` без аргументов в интерактивном терминале (иначе — справка), в том числе по SSH. TUI — клиент того же API: локально — через сокет с правами процесса (root — администратор, аккаунт с shell — он сам), удалённо — `mp --server https://host:8443 --token …` (или `MP_SERVER` / `MP_TOKEN`, `--insecure`).
- Шапка: версия панели, хост, нагрузка и память, кто вошёл (логин и роль, удалённо — и адрес панели); обновляется раз в 15 секунд и говорит, если панель или агент недоступны.

Разделы — как в навигации Web UI и в том же порядке. В каждом — свои экраны и все команды его групп `mp` (`*`), так что в меню есть каждая команда. Консоли в TUI нет: в терминале команды и так под рукой — палитра `:`. Группа команд, которой нет ни в одном разделе, попала бы в раздел «Другое»; сейчас таких нет, и этого раздела в меню нет.

| Раздел | Что в нём | Группы `mp` |
|---|---|---|
| Дашборд | сводка, диагностика с действиями «починить сайт», «починить все сайты», «включить enforcing» и «обновить» (`u` на строке ждущего обновления или службы на заменённом бинарнике: `mp stack upgrade <компонент>`), метрики, SELinux | `status`, `metrics`, `doctor`, `selinux` |
| Сайты | список сайтов; страница сайта: настройки, PHP-параметры, сертификат, CMS, логи, свои и сгенерированные nginx-директивы, apply, fix, приостановка, удаление, файлы сайта | `site`, `cms` |
| Пользователи | список аккаунтов; страница аккаунта: изменение, пароль, shell или только SFTP, сброс 2FA, удаление; его cron, app-сервисы, Valkey, общие папки, API-токены и файлы | `user`, `cron`, `app`, `valkey`, `token` |
| PHP | ветки: установка, удаление, расширения; php.ini сервера | `php` |
| Расширения | компоненты стека: установка, переустановка, удаление, обновление из репозиториев (u — компонент строки, в том числе строки, служба которой или один из Apache аккаунтов работает на заменённом бинарнике: тогда перезапуск после проверки; U — nginx, PHP и Apache), ежедневное обновление (a); memcached, real-ip | `stack` |
| Базы данных | базы и их пароли, сервер БД и его параметры | `db` |
| Почта | состояние, домены, ящики, алиасы, настройки, вебпочта | `mail` |
| Файлы | файловый менеджер аккаунта | `files` |
| SSL | сертификаты, сертификат панели, DNS-провайдеры | `ssl`, `web`, `dns-provider` |
| Задачи | задачи с фильтром, журнал задачи, ожидание | `job` |
| Журнал | журнал действий с фильтром | `audit` |
| Firewall | состояние, правила, баны, доверенные адреса | `firewall` |
| Службы | юниты панели и стека: запуск, остановка, перезапуск, автозапуск, журнал; любой юнит по имени | `service`, `logs` |
| Бэкапы | репозитории, запуски, снимки, восстановление | `backup` |
| Перенос | токен для другой панели, разбор, перенос | `migrate` |
| Настройки | API-токены, 2FA, `whoami`; у администратора — webhooks, обновления, конфигурация панели, шаблоны | `token`, `totp`, `whoami`, `webhook`, `update`, `config` |

- Экраны — таблицы и карточки с данными API, их читает `internal/client`, как и CLI. Действия привязаны к выбранной строке и вызываются буквой (подсказки внизу), Enter открывает строку.
- Каждое действие — команда `mp`. Форма строится из её аргументов и флагов, значения выбранной строки уже подставлены; у флага с перечнем значений — выбор, у `--php` — установленные ветки. Форма изменения открывается с текущими значениями, и в команду попадает только изменённое — такие поля отмечены ●. Пока форма заполняется, под ней видна строка `mp …`, которая выполнится, с секретами под `•••`.
- Пароли, токены и учётные данные форма передаёт команде через stdin (`--password-stdin` и другие пары), а не в командной строке; для флага без такой пары она предупреждает, что значение видно в `ps`.
- Команда запускается дочерним процессом того же бинарника: адрес панели и токен он получает через окружение (`MP_SERVER`, `MP_TOKEN`, `MP_INSECURE`), не в аргументах. Вывод идёт в окно по мере появления, команды с задачей ждут её, как в CLI. Просмотры (`show`, `status`, логи) выполняются сразу, без формы, если всё нужное известно.
- Подтверждения — там же, где их спрашивает Web UI, и перед всем, что удаляет, останавливает или прерывает: команда целиком, пояснение, что произойдёт, и `y/N`. Удаление аккаунта, удаление сайта с файлами (`--purge`) и восстановление снимка поверх файлов (`--in-place`) требуют ввести логин, домен или номер снимка. Команде, которая спрашивает сама (`user rm`), TUI после своего вопроса передаёт `--yes`.
- Файлы и свои nginx-директивы сайта правятся в `$VISUAL` или `$EDITOR` (без них — `editor`, nano или vi): `mp files get` → редактор → `mp files put` (или `mp site nginx --set` с проверкой `nginx -t`). Если сохранить не удалось, `e` открывает правку снова; копию, которая могла не сохраниться, главное меню покажет при следующем запуске, через сутки она удаляется. `mp totp setup` выполняется прямо в терминале, чтобы показать QR-код. После `mp migrate plan` окно вывода предлагает `mp migrate run` с теми же значениями.
- `?` или F1 — справка: действия экрана с их командами `mp`, все команды раздела и клавиши; Enter выполняет выбранное. В форме F1 — справка по команде и каждому полю. `*` — все команды раздела. `:` — палитра: любая строка `mp` с дополнением команд и флагов по Tab; чего в строке не хватает, спросит форма, а группа (`site`) открывает список своих команд.
- Роли: обычный аккаунт видит разделы, которые ему показывает Web UI, — Дашборд, Сайты, Аккаунт (свои cron, app-сервисы, Valkey, общие папки, токены, пароль и 2FA), Базы данных, Почта, Файлы, Задачи, Журнал и Настройки; команд и флагов администратора (`--user` и других) в его меню и формах нет. Решает всё равно API.
- Локально и удалённо: с `--server` меню не предлагает того, что меняет машину напрямую, в обход API, — `mp update trust` и флаг `--local` у `mp config show|set`.

Клавиши:

| Где | Клавиши |
|---|---|
| Меню и списки | ↑/↓ или j/k, Home/End, в списках и PgUp/PgDn — выбор; Enter — открыть (в файлах — войти в каталог, Backspace — на уровень выше); буква — действие; `/` — поиск по списку; `f` — фильтр (задачи, журнал, метрики); `r` — обновить; `*` — все команды раздела; Esc или q — назад, в главном меню — выход |
| Форма | Tab, Shift+Tab, ↑/↓ — поля; пробел, ←/→ — переключатель и выбор; Enter — следующее поле, на последнем — выполнить; Ctrl+S — выполнить; Ctrl+R — показать секрет; Ctrl+P — передать флаг, даже если значение не менялось; F1 — справка по команде; Esc — отмена. Буквы в форме — текст, поэтому `?`, `:` и `q` там не действуют |
| Вывод | ↑/↓, PgUp/PgDn, Home/End — прокрутка; Ctrl+C — прервать команду (задачу на сервере это не отменяет); r — ещё раз (просмотры); e — править снова; n — следующая команда (`migrate run` после `plan`); Esc, q или Enter — назад, к форме, если команда не удалась |
| Подтверждение | y или Enter — да, n или Esc — нет; где просят ввести логин, домен или номер снимка — ввести и Enter |
| Справка и палитра | ↑/↓ — выбор, Enter — выполнить; в палитре Tab или Enter на подсказке дополняют строку; Esc — закрыть |
| Везде | `?` или F1 — справка, `:` — палитра, Ctrl+C — выход (в окне работающей команды — прервать её) |

## 4. REST API

- Базовый путь `/api/v1`, спецификация `/api/v1/openapi.json`, интерактивная документация `/api/v1/docs` (Stoplight Elements, вшит в бинарник). Документация, спецификация и `/schemas/*` отдаются только вошедшим (сессия или API-токен; токен переезда — нет): `curl -H "Authorization: Bearer $TOKEN" https://host:8443/api/v1/openapi.json`.
- Аутентификация: `Authorization: Bearer <token>`, cookie-сессия для UI (+ CSRF), unix peer-cred для CLI на локальном сокете. Токен действует с правами своего аккаунта (`admin` или `user`); scope `migrate:user:<login>` ограничивает его чтением переезда одного аккаунта, остальные scope — пометки. Scope переезда бывает только у токена администратора (обычно его выдаёт `mp migrate grant`); такие токены обычных аккаунтов панель отзывает при запуске.
- Мутации, которым нужна работа на сервере, асинхронны: `202 Accepted` и `job_id` в ответе; `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE: прогресс, строки лога, завершение). Быстрые операции (токен, настройка, экземпляр Valkey) — синхронные `200/201/204`. Администратор видит все задачи, другой аккаунт — те, что запускал он; секреты из параметров задачи (пароли, токены, секреты, зашифрованные поля `*_enc`) в ответах заменены на `[redacted]`.
- Действия, которые перезапускают саму панель, выполняются после ответа: `PUT /system/config` с `restart: true`, остановка и перезапуск `monopanel-api` и `monopanel-agent` и перезапуск `monopanel-api.socket` через `POST /services/{unit}`. Клиенту нужно переподключиться через несколько секунд — по новому адресу, если сменился `web.listen`.
- Порт панели держит `monopanel-api.socket` ([01 §3.1](01-architecture.md#31-monopaneld-api--непривилегированный-процесс)). Перезапуск, который не меняет адрес, перезапускает только `monopanel-api.service` — порт всё это время держит сокет. Перезапуск, который применяет новый `web.listen` (вместе с изменением или позже отдельным `restart`), и перезапуск API на странице «Службы» сначала останавливают сокет, и он поднимается вместе с API на новом адресе. Остановка `monopanel-api` или `monopanel-agent` сокет не трогает: следующее соединение с портом панели снова запустит API, а вместе с ним, через `Requires=`, и агента. Остановить панель целиком: `systemctl stop monopanel-api.socket monopanel-api monopanel-agent`; обновление пакета такую панель не запускает — postinstall перезапускает только то, что работало (`try-restart`). Перезапуск `monopanel-api.socket` (`mp service restart monopanel-api.socket`, «Службы») выполняется после ответа как остановка сокета и перезапуск API; `mp` ждёт, пока панель снова ответит. `mp config set --local web.listen …` пишет и drop-in сокета; без `--restart` применить так: `systemctl stop monopanel-api.socket && systemctl restart monopanel-api`. `monopanel api`, запущенный вручную, порт не получит, пока слушает сокет. `GET /system/status` отдаёт источник порта в `panel.listen_via`: `socket` или `itself`. Drop-in `/etc/systemd/system/monopanel-api.service.d/socket.conf` перед каждым стартом API спрашивает бинарник (`monopanel api-socket-sync --probe`), и версия 0.8.19 или ниже — после отката самообновления или даунгрейда пакета — получает порт без сокета, сама ([01 §9](01-architecture.md#9-упаковка-установка-обновление)).
- `PUT /system/config` меняет пять ключей `config.yaml` — `web.hostname`, `web.listen`, `log.level`, `log.format`, `jobs.workers`; файл пишет агент, ключ подписи релизов сюда не входит. Новый `web.listen` должен быть адресом, который сервер может слушать, и его порт открывается в firewall до записи. Изменения действуют после перезапуска monopanel-api, `restart_pending` в `GET /system/config` говорит, что файл уже не совпадает с тем, что загрузил работающий процесс.
- Ошибки — RFC 9457 `application/problem+json` с полем `errors[]` для валидации по полям. `/services/{unit}` передаёт отказ агента с его же статусом (403, 409, 400, 422) и причиной; 502 — только когда агент недоступен или сама операция не удалась. Так же выдача и удаление общей папки (`POST /users/{login}/shares`, `DELETE /users/{login}/shares/{name}`): 403 — папку или точку монтирования агент не принимает (у `mp user share add|rm` — код выхода 4), 409 — папка занята, 503 — блокировку общих папок держат распаковка архива, восстановление или другой долгий обход дерева ниже веб-корня (chown и ACL в `mp site fix`), повторите, когда они закончатся.
- Списки отдаются целиком; у задач есть `?limit` и `?status`, у журнала действий — `?limit`, `?actor` и `?action`, у бэкапов — `?limit` и `?target`. Курсорная пагинация, фильтры, выбор полей и `Idempotency-Key` — планируются.
- Webhooks: `POST` JSON на URL при завершении задач — события `job.done`, `job.failed` и `<тип задачи>.done|failed` (`site.apply.done`, `cert.issue.failed`, `backup.run.*`, `*`), заголовки `X-MonoPanel-Event` и `X-MonoPanel-Signature: sha256=<HMAC тела>`, три попытки. События вне задач (`service.down`, истекающий сертификат) — планируются.

Ресурсы:

```
POST            /auth/login | logout              GET /auth/me
GET             /auth/totp                        POST /auth/totp/setup | enable | disable
GET/POST        /users                            GET/PATCH/DELETE /users/{login}     POST /users/{login}/totp/reset
GET/POST        /users/{login}/cron               PATCH/DELETE /users/{login}/cron/{id}         GET /cron
GET/POST        /users/{login}/apps               GET/PATCH/DELETE /users/{login}/apps/{name}   GET /apps
GET             /users/{login}/apps/{name}/logs   POST /users/{login}/apps/{name}/{start|stop|restart}
GET             /users/{login}/valkey             PUT/DELETE /users/{login}/valkey/{cache|sessions}   GET /valkey
GET/POST        /users/{login}/shares             PATCH/DELETE /users/{login}/shares/{name}   (mount_point — где папка смонтирована;
                                                  share.status failed и share.last_error — почему агент её не смонтировал)
POST            /users/{login}/valkey/{purpose}/restart
GET/POST        /sites                            GET/PATCH/DELETE /sites/{domain}   GET /sites/presets
POST            /sites/{domain}/apply | fix | suspend | unsuspend | cms | tls/issue    POST /sites/move-ip
GET             /sites/{domain}/logs/{type}       GET/PUT /sites/{domain}/nginx      GET /sites/{domain}/php
GET             /cms
GET/POST        /php/versions                     DELETE /php/versions/{version}     GET/PUT /php/settings
GET/POST        /php/versions/{version}/extensions
GET/POST        /databases                        DELETE /databases/{name}           POST /databases/{name}/password
GET             /db/engine                        POST /db/engine/tune               GET/PUT /db/engine/config
GET             /stack                            POST /stack/install                DELETE /stack/{component}
GET/PUT         /stack/memcached | /stack/nginx/real-ip | /stack/auto-upgrade   ({enabled, hour}: ежедневное обновление)
POST            /stack/upgrade                    (задача stack.upgrade, тело {component}: nginx | php | apache | db | all;
                                                  тело необязательно — без него или с {} обновляется all: nginx, PHP и Apache)
                (строки GET /stack несут upgrade {component, from, to, origin, packages}; статус службы — binary_replaced;
                 instances.replaced у apache — сколько запущенных Apache аккаунтов работает на заменённом бинарнике;
                 пока переезд на Apache аккаунтов не закончен, instances.waiting у apache — сколько сайтов ждут переезда,
                 а service — общий apache2.service, который их обслуживает)
GET/POST        /certificates                     GET/DELETE /certificates/{id}      POST /certificates/import | /{id}/renew
GET/DELETE      /ssl/panel                        POST /ssl/panel/issue | import      GET /web/tls
GET/POST        /dns-providers                    DELETE /dns-providers/{name}        GET /dns-providers/types
GET/POST        /backups/targets                  DELETE /backups/targets/{ref}       GET /backups/targets/{ref}/snapshots
GET             /backups                          POST /backups/run | restore
GET             /firewall                         POST /firewall/enable | disable | apply | rules | ban | unban | trust | untrust
DELETE          /firewall/rules/{id}
GET             /mail                             POST /mail/install | apply | webmail    PUT /mail/settings
GET/POST        /mail/domains | mailboxes | aliases    PATCH/DELETE /mail/domains/{name} | mailboxes/{address}
POST            /mail/domains/{name}/dkim         GET /mail/domains/{name}/dns        DELETE /mail/aliases/{address}
GET             /files?user=&path=                GET/PUT /files/content              POST /files/op   (через helper)
POST            /migrate/grant | plan | run       GET /migrate/plan | state | files | dump   (источник — только по токену переезда)
GET             /services                         GET/POST /services/{unit}   (start | stop | reload | restart | reload-or-restart | enable | disable)
GET             /jobs | /jobs/{id} | /jobs/{id}/events
GET/POST        /tokens                           DELETE /tokens/{id}
GET/POST        /webhooks                         DELETE /webhooks/{id}               POST /webhooks/{id}/test
GET             /system/status | doctor | metrics | logs/{unit} | audit    GET/PUT /system/selinux
                (status: host.container — тип контейнера, в котором работает агент; пусто на хосте и в VM)
GET/PUT         /system/update                    POST /system/update/check | apply
                (last_attempt.reinstall — откатившаяся версия уже числится в базе пакетов: apply поставит её ещё раз)
GET/PUT         /system/config                    GET /system/templates   (настройки панели в config.yaml, шаблоны и их переопределения)
POST            /system/console                   (команды mp с потоковым выводом, только администратор)
GET             /health
```

## 5. Интеграция с биллингом (WHMCS и др.)

Модуль WHMCS ещё не написан; API закрывает его действия так:

| Действие WHMCS | Вызовы API |
|---|---|
| `CreateAccount` | `POST /users` → `POST /sites` (+ `POST /databases` по пакету) |
| `SuspendAccount` / `UnsuspendAccount` | `PATCH /users/{login}` со `status: suspended\|active` (вход в панель, сеансы, API-токены и локальный сокет; SFTP/SSH остаётся; приостановка завершает сессии аккаунта в браузере); заглушка 503 на сайтах — `POST /sites/{domain}/suspend` / `unsuspend` |
| `TerminateAccount` | `DELETE /users/{login}?purge=true` |
| `ChangePassword` | `PATCH /users/{login}` с `password` (сессии аккаунта в браузере завершаются, API-токены остаются) |
| `ChangePackage` | `PATCH /sites/{domain}` (ветка PHP, `php_ini`, `fpm_max_children`…); квоты и лимиты пакета — планируются |
| `ServiceSingleSignOn` | планируется (одноразовая ссылка входа) |
| `UsageUpdate` | планируется (учёт диска и трафика по аккаунту) |

Требования к API, которые это закрывает: токены с правами аккаунта, webhooks на завершение задач, стабильные идентификаторы (`login`, `domain`).
