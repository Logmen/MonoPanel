# 04. CLI, TUI и REST API

## 1. Принцип

Один API. Web UI, CLI, TUI и внешние интеграции (биллинг, скрипты) ходят в одни и те же эндпоинты `/api/v1`. CLI и TUI — тонкие клиенты: Go-клиент `internal/client` с типами из `internal/apitypes`, из тех же типов `huma` генерирует описание OpenAPI 3.1; Web UI обращается к тем же эндпоинтам через небольшую обёртку над `fetch`. Функция реализуется один раз — в API — и поэтому доступна везде.

## 2. CLI — `monopanel` (alias `mp`)

Подключение: локально — `/run/monopanel/api.sock` (авторизация по uid процесса: root — администратор, uid клиента — его аккаунт, так что клиент с SSH-доступом управляет своим хозяйством тем же `mp`); удалённо — `--server https://host:8443 --token …` или переменные `MP_SERVER` / `MP_TOKEN`, `--insecure` — для самоподписанного сертификата.

Глобальные флаги: `--json` (машинный вывод), `--no-wait` (не ждать задачу), `--server`, `--token`, `--insecure`, `--config` (путь к `config.yaml`), `--version`.

```
mp                                   # TUI-меню (в интерактивном терминале; иначе — справка)
mp setup [--hostname …] [--listen …] [--admin-login …] [--admin-password …|--password-stdin]
mp status                            # панель, хост, сервисы, задачи
mp doctor                            # диагностика
mp version
mp update                            # что стоит и что вышло
mp update check | apply [--version v0.8.10]
mp update settings [--repo owner/name] [--channel …] [--check-hours 24] [--auto-apply] [--token-stdin|--clear-token]
mp update trust --key <публичный ключ> [--restart] | --clear

mp user add <login> [--password …|--password-stdin|--generate] [--email …] [--role user|admin] [--shell]
mp user set <login> [--password …|--generate] [--email …] [--shell|--sftp-only] [--status active|suspended]
mp user list | show <login> | totp-reset <login>
mp user rm <login> [--purge] [--yes]

mp site add <domain> --user <login> [--php 8.4] [--mode fpm|apache|proxy] [--backend http://127.0.0.1:3000]
            [--alias www.<domain>] [--www] [--preset wordpress|joomla|opencart|bitrix] [--ssl auto|none]
            [--ip …] [--docroot public] [--ini key=value] [--allow CIDR] [--pm …] [--max-children 8]
mp site set <domain> [--php …] [--mode …] [--alias …] [--rm-alias …] [--www|--no-www] [--redirect-www …]
            [--https-redirect=false] [--ini key=value] [--allow CIDR|--allow-all] [--preset …]
            [--sessions files|valkey] [--ssl auto|none] [--http2=false] [--http3] [--max-body 64m] [--allow-exec]
mp site list | show <domain> | presets
mp site apply | fix | suspend | unsuspend <domain>   # fix — владелец, права, ACL и метки SELinux файлов сайта
mp site rm <domain> [--purge]
mp site logs <domain> [--type access|error|php|slow|apache-access|apache-error] [-n 100]
mp site nginx <domain> [--set файл | --clear | --generated]   # свои директивы в server {} с проверкой nginx -t и откатом
mp site php <domain>                 # действующие PHP-параметры и откуда каждый взялся
mp site tls <domain> [--dns <провайдер>] [--staging]          # сертификат на домен с алиасами, сайт переходит на HTTPS
mp site move-ip <новый-адрес> [--from <прежний>]   # после смены IP хоста: все сайты пропавших адресов разом
mp selinux [enforcing|permissive] [--yes]
mp cms list
mp cms install <domain> wordpress|joomla|opencart|bitrix [--title …] [--admin-login …] [--admin-password …|--password-stdin]
               [--admin-email …] [--force]   # Битрикс: --edition start|standard|small_business|business, --solution clean|demo|<id>

mp php list [--available]
mp php install <версия> | remove <версия>
mp php ext list <версия> | enable <версия> <расширение> | disable <версия> <расширение>
mp php ini [set key=value … | unset key …]   # PHP-параметры для всех сайтов

mp stack list
mp stack install|remove nginx|apache|percona|mysql|fail2ban|memcached|valkey|jpegoptim|git|composer|sphinx
mp stack memcached [--memory-mb 256] [--max-conn 2048]    # без флагов — показать
mp stack real-ip [--cloudflare|--no-cloudflare] [--from CIDR] [--clear-from]
mp valkey add cache|sessions --user <login> [--memory 128]   # экземпляр аккаунта: создать или сменить память (с перезапуском)
mp valkey list [--user <login>] | restart | rm cache|sessions --user <login>   # restart опустошает кеш

mp db create <name> --user <login> [--password …] [--legacy-auth]   # база и пользователь <login>_<name>; пароль сгенерируется
mp db list | passwd <name> [--password …] | rm <name>
mp db engine                         # сервер СУБД; tune — пересчитать настройки под память
mp db engine tune

mp ssl list | show <id|имя> | rm <id|имя>
mp ssl renew <id|имя> | --all
mp ssl issue <имя> [имя…] [--dns <провайдер>] [--staging] [--rsa] [--email …]
mp ssl import [имя] --cert f --key f [--chain f]
mp ssl panel                         # сертификат самой панели и запись за ним
mp ssl panel issue [--dns …] [--staging] | import --cert f --key f | self-signed
mp web tls                           # какой сертификат панель отдаёт прямо сейчас
mp dns-provider types | list
mp dns-provider add <имя> --type cloudflare --cred CLOUDFLARE_DNS_API_TOKEN=… | rm <имя>

mp backup target add <имя> --type local|sftp|s3|b2|rest --repo … [--password …] [--env KEY=…] [--schedule daily]
                   [--keep-daily 7] [--keep-weekly 4] [--keep-monthly 3]
mp backup target list | rm <имя>
mp backup run [--target …] [--scope server|user:<login>|site:<domain>|db:<name>]
mp backup list | snapshots [--target …]
mp backup restore <snapshot> [--target …] [--include путь…] [--in-place]

mp cron list | add | enable <id> | disable <id> | rm <id> --user <login>
            [--schedule "*/5 * * * *" --command "…" --comment … --disabled]   # list без --user у администратора — задания всех
mp app list | show | add | set | start | stop | restart | logs | rm <name> --user <login>
            [--command … --workdir … --env KEY=… --env-file … --restart … --description …]
mp files ls|get|put|mkdir|rm|mv|chmod|extract|size --user <login> <путь…>   # через helper от имени клиента

mp firewall status | enable | disable | apply
mp firewall allow|deny --port 8443 [--proto tcp] [--source CIDR] [--comment …]
mp firewall rm <id> | ban <ip> | unban <ip>

mp mail install | status | settings | apply | domain … | box … | alias … | webmail …   # подробно — 06-mail.md
mp migrate grant user:<login> | plan | run                                             # подробно — 07-migration.md

mp service status [unit] | start | stop | reload | restart | enable | disable <unit>
mp logs <unit> [-n 100]              # журнал юнита через агент
mp metrics [--range 1h|6h|24h|7d|30d]
mp job list [--status …] [--limit …] | show <id> | wait <id>
mp config show | templates
mp config set <ключ> <значение> [--restart]   # web.hostname, web.listen, log.level, log.format, jobs.workers
mp token create [--name …] [--scopes …] [--expires <дней>] [--user <login>]
mp token list [--user …] | revoke <id>
mp webhook add --url … [--events job.failed,backup.run.*] [--secret …]
mp webhook list | test <id> | rm <id>
mp completion bash|zsh|fish          # скрипт автодополнения
```

Соглашения:
- Команды, за которыми стоит задача, по умолчанию ждут её завершения и печатают прогресс и лог (`--no-wait` — вернуть номер задачи сразу).
- Коды выхода: 0 — ok, 1 — ошибка, 2 — неверные аргументы, 3 — задача завершилась с ошибкой, 4 — нет прав.
- `--json` печатает один JSON-объект (или массив), без прогресса и подсказок.
- Автодополнение печатает `mp completion <shell>`; в пакет оно пока не входит.

## 3. TUI-меню

- Запуск: `mp` без аргументов в интерактивном терминале (иначе — справка).
- Меню: сайты, пользователи, PHP, базы данных, SSL, firewall, сервисы и задачи — таблицами для просмотра, через тот же API, что и CLI. «Бэкапы» и «Настройки» подсказывают команды `mp backup` и `mp config`.
- Клавиши: ↑/↓ или j/k, Enter — открыть, r — обновить, q или Esc — назад, Ctrl+C — выход.
- Планируется: формы с валидацией теми же правилами, что в API, дашборд, прогресс задач и подсказка эквивалентной CLI-команды к каждому действию.

## 4. REST API

- Базовый путь `/api/v1`, спецификация `/api/v1/openapi.json`, интерактивная документация `/api/v1/docs` (Stoplight Elements, вшит в бинарник). Документация, спецификация и `/schemas/*` отдаются только вошедшим (сессия или API-токен; токен переезда — нет): `curl -H "Authorization: Bearer $TOKEN" https://host:8443/api/v1/openapi.json`.
- Аутентификация: `Authorization: Bearer <token>`, cookie-сессия для UI (+ CSRF), unix peer-cred для CLI на локальном сокете. Токен действует с правами своего аккаунта (`admin` или `user`); scope `migrate:user:<login>` ограничивает его чтением переезда одного аккаунта, остальные scope — пометки.
- Мутации, которым нужна работа на сервере, асинхронны: `202 Accepted` и `job_id` в ответе; `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE: прогресс, строки лога, завершение). Быстрые операции (токен, настройка, экземпляр Valkey) — синхронные `200/201/204`.
- Ошибки — RFC 9457 `application/problem+json` с полем `errors[]` для валидации по полям.
- Списки отдаются целиком; у задач есть `?limit` и `?status`. Курсорная пагинация, фильтры, выбор полей и `Idempotency-Key` — планируются.
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
POST            /users/{login}/valkey/{purpose}/restart
GET/POST        /sites                            GET/PATCH/DELETE /sites/{domain}   GET /sites/presets
POST            /sites/{domain}/apply | fix | suspend | unsuspend | cms | tls/issue    POST /sites/move-ip
GET             /sites/{domain}/logs/{type}       GET/PUT /sites/{domain}/nginx      GET /sites/{domain}/php
GET             /cms
GET/POST        /php/versions                     DELETE /php/versions/{version}     GET/PUT /php/settings
GET/POST        /php/versions/{version}/extensions
GET/POST        /databases                        DELETE /databases/{name}           POST /databases/{name}/password
GET             /db/engine                        POST /db/engine/tune
GET             /stack                            POST /stack/install                DELETE /stack/{component}
GET/PUT         /stack/memcached | /stack/nginx/real-ip
GET/POST        /certificates                     GET/DELETE /certificates/{id}      POST /certificates/import | /{id}/renew
GET/DELETE      /ssl/panel                        POST /ssl/panel/issue | import      GET /web/tls
GET/POST        /dns-providers                    DELETE /dns-providers/{name}        GET /dns-providers/types
GET/POST        /backups/targets                  DELETE /backups/targets/{ref}       GET /backups/targets/{ref}/snapshots
GET             /backups                          POST /backups/run | restore
GET             /firewall                         POST /firewall/enable | disable | apply | rules | ban | unban
DELETE          /firewall/rules/{id}
GET             /mail                             POST /mail/install | apply | webmail    PUT /mail/settings
GET/POST        /mail/domains | mailboxes | aliases    PATCH/DELETE /mail/domains/{name} | mailboxes/{address}
POST            /mail/domains/{name}/dkim         GET /mail/domains/{name}/dns        DELETE /mail/aliases/{address}
GET             /files?user=&path=                GET/PUT /files/content              POST /files/op   (через helper)
POST            /migrate/grant | plan | run       GET /migrate/plan | state | files | dump   (источник — только по токену переезда)
GET             /services                         GET/POST /services/{unit}
GET             /jobs | /jobs/{id} | /jobs/{id}/events
GET/POST        /tokens                           DELETE /tokens/{id}
GET/POST        /webhooks                         DELETE /webhooks/{id}               POST /webhooks/{id}/test
GET             /system/status | doctor | metrics | logs/{unit}    GET/PUT /system/selinux
GET/PUT         /system/update                    POST /system/update/check | apply
POST            /system/console                   (команды mp с потоковым выводом, только администратор)
GET             /health
```

## 5. Интеграция с биллингом (WHMCS и др.)

Модуль WHMCS ещё не написан; API закрывает его действия так:

| Действие WHMCS | Вызовы API |
|---|---|
| `CreateAccount` | `POST /users` → `POST /sites` (+ `POST /databases` по пакету) |
| `SuspendAccount` / `UnsuspendAccount` | `PATCH /users/{login}` со `status: suspended\|active` (вход в панель и SFTP/SSH); заглушка 503 на сайтах — `POST /sites/{domain}/suspend` / `unsuspend` |
| `TerminateAccount` | `DELETE /users/{login}?purge=true` |
| `ChangePassword` | `PATCH /users/{login}` с `password` |
| `ChangePackage` | `PATCH /sites/{domain}` (ветка PHP, `php_ini`, `fpm_max_children`…); квоты и лимиты пакета — планируются |
| `ServiceSingleSignOn` | планируется (одноразовая ссылка входа) |
| `UsageUpdate` | планируется (учёт диска и трафика по аккаунту) |

Требования к API, которые это закрывает: токены с правами аккаунта, webhooks на завершение задач, стабильные идентификаторы (`login`, `domain`).
