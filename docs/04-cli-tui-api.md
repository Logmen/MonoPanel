# 04. CLI, TUI и REST API

## 1. Принцип

Один API. Web UI, CLI, TUI и внешние интеграции (WHMCS, скрипты) используют одни и те же эндпоинты `/api/v1`. CLI — Go-клиент, сгенерированный из того же OpenAPI-описания, что и TypeScript-клиент UI. Любая новая функция появляется во всех интерфейсах одновременно, потому что реализована один раз — в API.

## 2. CLI — `monopanel` (alias `mp`)

Подключение: локально — `/run/monopanel/api.sock` (авторизация по uid: root → admin, uid клиента → его аккаунт); удалённо — `--server https://host:8443 --token …` или переменные `MP_SERVER` / `MP_TOKEN`.

Глобальные флаги: `--json` (машинный вывод), `-q/--quiet`, `--wait/--no-wait` (для операций-задач), `--yes` (без подтверждений), `--server`, `--token`.

```
mp                                   # TUI-меню (в интерактивном терминале)
mp setup [--config setup.yaml]       # мастер первичной установки / неинтерактивная установка
mp doctor                            # диагностика
mp status                            # сводка: сервисы, нагрузка, диск, сертификаты, задачи
mp update [--check]                  # обновление панели

mp user add <login> [--password|--password-stdin] [--shell] [--quota 10G] [--email …]
mp user list | show | edit | passwd | suspend | unsuspend | rm [--purge] <login>

mp site add <domain> --user <login> [--php 8.4] [--mode fpm|apache] [--alias www.<domain>] [--ip …] [--ssl auto|none] [--docroot public]
mp site list [--user …] | show <domain> | rm <domain>
mp site php <domain> 8.5             # смена версии PHP
mp site mode <domain> fpm|apache
mp site ini <domain> memory_limit=512M upload_max_filesize=128M
mp site alias add | rm <domain> <alias>
mp site redirect <domain> --https on --www to_root
mp site ssl <domain> issue | renew | import --cert f --key f | off
mp site logs <domain> [--type access|error|php|slow] [-f] [-n 200]
mp site suspend | unsuspend | enable | disable <domain>
mp site apply <domain>               # принудительная перегенерация конфигов сайта
mp site fix <domain>                          # владелец, права, ACL и метки SELinux файлов сайта (после cp -a из /root и т.п.)
mp selinux [enforcing|permissive] [--yes]     # режим SELinux; permissive только после предупреждения и подтверждения
mp cms list                                   # какие CMS панель ставит и откуда берёт дистрибутивы
mp cms install <domain> wordpress|joomla|opencart|bitrix [--title] [--admin-login] [--admin-password|--password-stdin] [--admin-email] [--force]
                                              # Битрикс: --edition start|standard|small_business|business, --solution clean|demo|<vendor.solution из Маркетплейса>
                                              # пресет, база <login>_<cms>, файлы, штатный установщик; доступы печатаются один раз
mp config apply --all                # полный reconcile

mp php list [--available]            # установленные / доступные версии
mp php install 8.5 [--ext imagick,redis] | remove 7.4
mp php ext list | enable | disable 8.4 imagick
mp php ini 8.4 [key=value …]         # глобальный ini версии
mp php default 8.5                   # системный /usr/bin/php

mp stack install|remove memcached|jpegoptim|git|composer|sphinx   # расширения; composer нужна ветка PHP, sphinx — поиск для Bitrix
mp stack memcached [--memory-mb 256] [--max-conn 2048]    # без флагов — показать

mp db create <name> --user <login> [--db-user …] [--password … | --generate] [--remote]
mp db list | rm | passwd | dump | restore …
mp db engine status | tune | restart

mp ssl list [--expiring 30d] | renew --all | account show | register
mp ssl panel                          # сертификат самой панели: что отдаётся и запись за ним
mp ssl panel issue [--dns cf] [--staging] | import --cert … --key … | self-signed
mp site tls <domain> [--dns cf] [--staging]   # сертификат сайта (домен + алиасы), сайт переключается на HTTPS
mp dns-provider add cloudflare --token … --user <login>

mp backup target add local|sftp|s3 … | list
mp backup run [--target …] [--scope server|panel|user:<login>|site:<domain>|db:<name>]
mp backup list | restore <snapshot> [--to /path] | prune

mp cron list | add | rm --user <login> --schedule "*/5 * * * *" --command "…" [--php 8.4]
mp firewall status | allow | deny | rules | ban <ip> | unban <ip>
mp service status | reload | restart nginx|apache|php-fpm@8.4|mysql|panel
mp config show | set <key> <value> | templates list | diff | override | reset
mp job list | show <id> | logs <id> [-f] | wait <id> | cancel <id>
mp token create --name whmcs --scopes users:rw,sites:rw --expires 365d | list | revoke <id>
mp web port 8443 | cert auto|self-signed | bind <ip>
mp files ls|cp|mv|rm|chmod|extract --user <login> <path>   # через helper от имени клиента
```

Соглашения:
- Команды-мутации возвращают `job_id` и по умолчанию ждут завершения с прогресс-баром (`--no-wait` — вернуть сразу).
- Коды выхода: 0 — ok, 1 — ошибка, 2 — неверные аргументы, 3 — задача завершилась с ошибкой, 4 — нет прав.
- `--json` всегда печатает один JSON-объект (или массив), без прогресса и подсказок.
- Completion для bash/zsh/fish поставляется в пакете.

## 3. TUI-меню

- Запуск: `mp` без аргументов в интерактивном терминале (иначе — `mp --help`).
- Экраны: Дашборд (сервисы, нагрузка, диск, истекающие сертификаты, последние задачи) → Пользователи → Сайты (список с фильтром; карточка: общие, PHP, SSL, редиректы, логи, cron) → PHP-версии → Базы данных → SSL → Бэкапы → Firewall → Сервисы → Задачи → Настройки → Обновление.
- Формы — `huh` (валидация на лету теми же правилами, что в API); длинные операции — стрим лога задачи через SSE в панели прогресса.
- Полностью клавиатурное управление, работает в 80×24, темы light/dark по терминалу.
- Каждое действие в TUI показывает эквивалентную CLI-команду в подсказке (обучение скриптованию).

## 4. REST API

- Базовый путь `/api/v1`, спецификация `/api/v1/openapi.json`, интерактивная документация `/api/docs` (Scalar).
- Аутентификация: `Authorization: Bearer <token>` (scopes: `users:r|rw`, `sites:r|rw`, `db:r|rw`, `ssl:rw`, `backups:rw`, `files:rw`, `system:rw`, `admin`), cookie-сессия для UI (+ CSRF), unix peer-cred для CLI.
- Мутации, требующие работы на сервере, асинхронны: `202 Accepted` + `{ "job_id": … }`; `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE: `progress`, `log`, `done`, `failed`). Быстрые операции (создать токен, изменить настройку) — синхронные `200/201`.
- Ошибки — RFC 9457 `application/problem+json` с полем `errors[]` для валидации по полям.
- Пагинация `?limit&cursor`, фильтры `?user=&status=&q=`, сортировка `?sort=-created_at`, выбор полей `?fields=`.
- Идемпотентность: заголовок `Idempotency-Key` на POST (повтор возвращает тот же job).
- Webhooks: `POST` на URL при `job.done|failed`, `cert.renewed|failed`, `backup.done|failed`, `site.suspended`, `service.down` — для биллинга и мониторинга. Подпись HMAC.

Ресурсы (сокращённо):

```
GET/POST        /users                          GET/PATCH/DELETE /users/{login}
POST            /users/{login}/suspend | unsuspend | password
GET/POST        /sites                          GET/PATCH/DELETE /sites/{domain}
POST            /sites/{domain}/apply | suspend | unsuspend
GET/PUT         /sites/{domain}/php             (version, ini, disable_functions, allow_exec, pm)
GET/POST/DELETE /sites/{domain}/aliases
GET/PUT         /sites/{domain}/redirects
GET/POST/DELETE /sites/{domain}/ssl             (issue / import / remove)
GET             /sites/{domain}/logs/{type}?tail=&follow=   (SSE при follow)
GET/POST/DELETE /sites/{domain}/cron
GET/POST        /databases                      GET/PATCH/DELETE /databases/{name}
GET/POST        /databases/{name}/users         POST /databases/{name}/dump | restore
GET/POST/DELETE /php/versions[/{ver}]           GET/PUT /php/versions/{ver}/ini | extensions
GET/POST        /certificates[/{id}]            POST /certificates/{id}/renew
GET/POST/DELETE /dns-providers
GET/POST        /backups/targets                GET/POST /backups   POST /backups/{id}/restore
GET/PUT         /firewall/rules                 POST /firewall/ban | unban
GET             /services                       POST /services/{name}/reload | restart
GET             /jobs   GET /jobs/{id}   GET /jobs/{id}/events   POST /jobs/{id}/cancel
GET/PUT         /settings                       GET /system/status | metrics | updates   POST /system/update
GET/POST/DELETE /tokens                         GET /audit
POST            /auth/login | logout | totp | sso    GET /auth/me   /auth/webauthn/*
GET/POST/...    /files?user=&path=              (список, upload, download, mkdir, mv, rm, chmod, extract — через helper)
WS              /terminal?user=                 (xterm от имени пользователя, только с флагом shell)
```

## 5. Интеграция с биллингом (WHMCS и др.)

Серверный модуль WHMCS (PHP) поверх REST API:

| Действие WHMCS | Вызовы API |
|---|---|
| `CreateAccount` | `POST /users` → `POST /sites` (+ `POST /databases` по пакету) |
| `SuspendAccount` / `UnsuspendAccount` | `POST /users/{login}/suspend` / `unsuspend` |
| `TerminateAccount` | `DELETE /users/{login}?purge=true` |
| `ChangePassword` | `POST /users/{login}/password` |
| `ChangePackage` | `PATCH /users/{login}` (квота, лимиты) + `PUT /sites/{domain}/php` |
| `ServiceSingleSignOn` | `POST /auth/sso` → одноразовая ссылка в клиентскую панель |
| `UsageUpdate` | `GET /users?fields=disk,traffic` |

Требования к API, которые это закрывает: токен со scope, идемпотентность, webhooks, стабильные идентификаторы (`login`, `domain`).
