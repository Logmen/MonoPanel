# 01. Архитектура MonoPanel

Состояние на сентябрь 2026. Версии компонентов указаны на этот момент; их актуальность проверяется отдельным CI-job (см. [05-roadmap.md](05-roadmap.md)).

> Это проектный документ: он описывает решения и их обоснование, в том числе те части, до которых реализация ещё не дошла. Что действительно работает — в [README](../README.md) и в отметках [x] в [05-roadmap.md](05-roadmap.md). Ниже такие места помечены как «планируется».

## 1. Цели и границы

**Что делаем**

- Панель управления веб-хостингом на одном сервере (класс FASTPANEL / ISPmanager Lite).
- ОС: Debian 12/13, Ubuntu 22.04/24.04/26.04 LTS; RHEL 9/10 и клоны (AlmaLinux, Rocky Linux, Oracle Linux).
- Режимы сайта: `nginx → php-fpm` и `nginx → Apache → php-fpm`.
- PHP 5.6 … 8.5 одновременно, версия выбирается на сайт; расширения по версиям.
- СУБД: MySQL 8.4 LTS **или** Percona Server for MySQL 8.4 LTS (выбор при установке, единый провайдер в коде).
- Интерфейсы: Web UI, CLI (скриптуемый, `--json`), TUI-меню (интерактивное, по SSH), REST API (OpenAPI 3.1).
- TLS (ACME), бэкапы, cron, firewall, защита от перебора, метрики, логи, файловый менеджер.

**Что не делаем в v1** (см. roadmap): почта, DNS-сервер, multi-server, контейнерные приложения, Node/Python-приложения, WAF, reseller-роль.

## 2. Архитектурные решения (ADR, кратко)

| # | Решение | Почему |
|---|---|---|
| A1 | Ядро панели — один статический бинарник на **Go** | Нет рантайм-зависимостей на хосте (2 семейства ОС × 9 релизов), один артефакт для daemon + CLI + TUI, простая упаковка в .deb/.rpm, быстрый старт CLI, встроенные HTTP-сервер и TLS |
| A2 | **Разделение привилегий**: `monopaneld api` (пользователь `monopanel`) ↔ `monopaneld agent` (root) через unix-сокет | Код, парсящий чужой ввод и раздающий Web, не работает от root. Агент принимает только типизированные команды с allow-list путей и операций |
| A3 | **Один API для всех клиентов**: Web UI, CLI, TUI и внешние интеграции ходят в один REST API | Нет расхождений в логике; всё, что можно в Web, можно в CLI и наоборот |
| A4 | **Декларативное состояние → рендер → валидация → атомарное применение → reload, с откатом** | Панель — источник истины; ручные правки живут в include-каталогах; сломанный конфиг никогда не доходит до reload |
| A5 | Панель слушает **собственный** HTTPS-порт (8443) на Go `net/http` | Панель доступна, даже если системный nginx сломан или ещё не установлен |
| A6 | Состояние панели — **SQLite** (WAL) | Панель обязана работать, когда MySQL не установлен или упал; бэкап состояния — один файл |
| A7 | PHP: **один php-fpm master на версию, пул на сайт**; Apache — только через `mod_proxy_fcgi` | Одна FPM-инфраструктура для обоих режимов; переключение режима = смена шаблона nginx/Apache. `mod_php` не поддерживается принципиально (одна версия на процесс Apache, работа от `www-data`) |
| A8 | PHP-сборки: этап 1 — репозитории Sury (deb) / Remi (rpm); этап 2 — **собственные сборки** с единым layout `/opt/monopanel/php/<X.Y>` | Быстрый старт без сборочной фермы; затем независимость от сторонних репозиториев (каждый держит один человек) и одинаковые пути на всех ОС |
| A9 | nginx — из **nginx.org** (stable) на всех ОС; Apache — из дистрибутива | Единый layout nginx (`conf.d`, пользователь `nginx`) и свежие версии с HTTP/3 на всех ОС; Apache в дистрибутивах достаточно свежий и хорошо интегрирован с MAC |
| A10 | Абстракция ОС — **OS Profile** (`debian`, `rhel`): пакетный менеджер, пути, имена сервисов, MAC (SELinux/AppArmor), firewall | Вся разница между семействами локализована в одном пакете кода |
| A11 | ACME-клиент встроен (библиотека **lego**) | Без certbot/python на хосте; HTTP-01 и DNS-01 (wildcard) из одного места, единая логика продления |
| A12 | Файловые операции от имени клиента — через **helper с понижением привилегий** | Защита от symlink-атак и подмены путей: агент никогда не трогает файлы клиента от root |
| A13 | Внутренний RPC api↔agent — HTTP/JSON поверх unix-сокета с `SO_PEERCRED` | Переиспользуем те же типы и инструменты, что и во внешнем API; при переходе на multi-server транспорт меняется на mTLS без переписывания операций |

## 3. Компоненты

```
                       ┌──────────────────────────────────────────────────────────────┐
 браузер ──HTTPS:8443──▶  monopaneld api      (User=monopanel, без привилегий)          │
 CLI/TUI ──unix sock───▶  ┌──────────────┐ ┌─────────────┐ ┌───────────┐ ┌──────────┐ │
 WHMCS/скрипты ─Bearer─▶  │ HTTP/API     │ │ Auth / RBAC │ │ Jobs      │ │ Render   │ │
                          │ chi + huma   │ │ argon2id    │ │ очередь + │ │ text/    │ │
                          │ SSE, WS      │ │ TOTP/WebAuthn│ │ воркеры   │ │ template │ │
                          └──────────────┘ └─────────────┘ └───────────┘ └──────────┘ │
                          ┌──────────────┐ ┌─────────────┐ ┌───────────┐              │
                          │ Scheduler    │ │ ACME (lego) │ │ Metrics   │  Web UI      │
                          │ renew/backup │ │             │ │ rollups   │  (embed.FS)  │
                          └──────────────┘ └─────────────┘ └───────────┘              │
                          SQLite WAL  /var/lib/monopanel/panel.db                     │
                       └───────────────────────────┬──────────────────────────────────┘
                                                   │ /run/monopanel/agent.sock
                                                   │ HTTP/JSON, SO_PEERCRED, allow-list операций
                       ┌───────────────────────────▼──────────────────────────────────┐
                       │  monopaneld agent   (root)                                     │
                       │  ApplyConfigSet · EnsureUnixUser · SetACL · SetQuota           │
                       │  Service{reload,restart} (systemd D-Bus) · Pkg{apt,dnf}        │
                       │  MySQL{root via auth_socket} · Nft{apply} · SELinux/AppArmor   │
                       │  RunAsUser → helper (setuid) для файлов клиента                │
                       └───┬──────────┬───────────┬──────────┬──────────┬──────────────┘
                           ▼          ▼           ▼          ▼          ▼
                        nginx      apache     php-fpm×N    mysqld    systemd / apt / dnf / nft
```

### 3.1 `monopaneld api` — непривилегированный процесс

- HTTPS-сервер панели (порт 8443, привязка к IP по выбору; сертификат: self-signed при установке → ACME для hostname панели).
- Раздаёт Web UI (SvelteKit static-сборка, вшита через `embed.FS`).
- REST API `/api/v1`: OpenAPI 3.1 генерируется из Go-типов (`huma`), SSE для событий и прогресса задач, WebSocket только для терминала.
- Аутентификация: сессии (cookie `Secure; HttpOnly; SameSite=Strict`), Bearer-токены со scope, TOTP, WebAuthn (passkeys). RBAC: `admin`, `user`.
- Unix-сокет `/run/monopanel/api.sock` для CLI: uid 0 → admin без пароля; uid панельного пользователя → его права (клиент по SSH управляет своими сайтами через `mp`).
- Job-runner: очередь задач в SQLite, N воркеров, каждая задача — идемпотентная последовательность шагов с логом и прогрессом; одна задача на сущность одновременно (lock по `site_id`/`user_id`).
- Рендер конфигураций (`text/template`) из встроенных шаблонов с переопределением в `/etc/monopanel/templates/`.
- Планировщик внутренних задач: продление сертификатов, бэкапы, ротация логов, сбор метрик, проверка обновлений.
- Хранит ключи ACME и секреты клиентов в зашифрованном виде (см. 3.6).

### 3.2 `monopaneld agent` — привилегированный процесс

- Тот же бинарник, режим `agent`, unit `monopanel-agent.service`, root.
- Сокет `/run/monopanel/agent.sock` (0660 root:monopanel). Проверка `SO_PEERCRED`: принимаются только uid `monopanel` и root.
- Операции — закрытый типизированный набор (нет «выполни строку в shell»):
  - `ApplyConfigSet{files[], validate[], reload[]}` — транзакционная запись набора файлов: backup → запись во временный файл + `rename` → валидаторы (`nginx -t`, `apachectl -t`, `php-fpm -t`) → при ошибке восстановление из backup и возврат stderr; при успехе reload сервисов и запись в confhistory.
  - `EnsureUnixUser`, `EnsureGroupMembership`, `EnsureDir`, `SetACL`, `SetQuota`, `RemoveUser{purge}`.
  - `Service{Start|Stop|Reload|Restart|Enable|Disable|Status}` — через systemd D-Bus (`go-systemd`), а не `systemctl`.
  - `Pkg{AddRepo|Install|Remove|Upgrade|Query}` — apt/dnf через OS Profile, с глобальной блокировкой и логом в задачу.
  - `MySQL{Exec, Dump, Restore}` — подключение `root@localhost` через unix-сокет и плагин `auth_socket` (пароль root не хранится).
  - `Nft{Apply}` — собственная таблица `inet monopanel`; `Firewalld{...}` при выборе firewalld на EL.
  - `SELinux{Restorecon|SetFcontext|SetPort|SetBool}` / `AppArmor{...}` — по OS Profile.
  - `RunAsUser{uid, gid, op}` — запуск helper с понижением привилегий для файловых операций клиента.
  - `Logrotate{Reopen}` — сигналы USR1 мастерам nginx/php-fpm после ротации.
- Allow-list путей записи: `/etc/nginx/nginx.conf`, `/etc/nginx/monopanel/`, `/etc/apache2/monopanel/` | `/etc/httpd/monopanel/`, каталоги пулов FPM по layout версии, `/etc/monopanel/`, `zz-monopanel.cnf` СУБД, `/etc/nftables.d/monopanel.nft`, `/etc/logrotate.d/monopanel`, `/etc/fail2ban/jail.d/monopanel.local`, `/var/lib/monopanel/`, `/var/spool/cron/*` (через `crontab -u`), `/var/www/<user>/...` — только через `RunAsUser`.

### 3.3 Helper (drop-privileges)

`monopaneld helper --uid N --gid N --groups ... -- <op> <args>`: агент форкает процесс, который до выполнения операции необратимо понижает привилегии (`setgroups` → `setgid` → `setuid`) и работает с файлами как клиент. Через него идут файловый менеджер, загрузка/распаковка архивов, `composer`, `wp-cli`, git-деплой, терминал. Сам helper не имеет привилегированных путей кода.

### 3.4 Web UI

- SvelteKit 2 (Svelte 5, runes), `adapter-static`, TypeScript strict, Tailwind CSS 4. Реализовано на этом наборе без сторонних UI-библиотек: компоненты, графики и таблицы — свои, единственная внешняя зависимость рантайма — `qrcode` (QR для TOTP).
- Планируется: библиотека компонентов (shadcn-svelte / Bits UI), TanStack Query + Table, CodeMirror 6 для редактора конфигов, xterm.js для терминала, Paraglide (i18n ru/en), генерация клиента из OpenAPI (`@hey-api/openapi-ts`).
- Сборка в `web/build` и вшивание в бинарник; SPA, один HTML, целевой размер JS ≈ 200–300 КБ gzip.
- Два режима интерфейса: администратор (весь сервер) и клиент (свои сайты/БД/файлы/cron).

### 3.5 CLI и TUI

- `monopanel` (alias `mp`) — Cobra-команды, вывод таблицей или `--json`.
- Без аргументов в интерактивном терминале — TUI-меню (Bubble Tea v2 + Huh + Lip Gloss): дашборд, сайты, пользователи, PHP, БД, SSL, бэкапы, сервисы, задачи, настройки. Работает по SSH в 80×24, стримит прогресс задач.
- CLI/TUI — тонкие клиенты API (локально `/run/monopanel/api.sock`, удалённо `https://host:8443` с токеном). Ни одна операция не реализована «только в CLI».
- Детали — в [04-cli-tui-api.md](04-cli-tui-api.md).

### 3.6 Хранилище состояния

- `/var/lib/monopanel/panel.db` — SQLite, WAL, `foreign_keys=ON`, `busy_timeout`. Драйвер `modernc.org/sqlite` (pure Go → CGO-free статический бинарник). Запросы написаны руками в `internal/store`, миграции — встроенные SQL-файлы, применяются при старте.
- Секреты (пароли БД клиентов, токены DNS-провайдеров, ключи ACME, пароли SFTP/S3 бэкапов) шифруются AES-256-GCM ключом из `/etc/monopanel/secret.key` (0640 root:monopanel). Бэкап SQLite без ключа для атакующего бесполезен.
- История конфигов: `/var/lib/monopanel/confhistory/<escaped-path>/<timestamp>` — последние N версий каждого сгенерированного файла, diff в UI, ручной откат.
- Снимок состояния: `mp backup run --scope panel` = `panel.db` + `/etc/monopanel` + `/var/lib/monopanel/{certs,acme}` — этого достаточно для восстановления панели на новом сервере с последующим `mp config apply --all`.

## 4. Стек панели и обоснование

### 4.1 Бэкенд / ядро

| Кандидат | Плюсы | Минусы | Вердикт |
|---|---|---|---|
| **Go 1.26+** | Статический бинарник, быстрый старт, goroutines для задач, stdlib HTTP/TLS, зрелые библиотеки для systemd/ACME/nftables/TUI, кросс-сборка amd64/arm64, nfpm | GC-паузы несущественны для панели | **выбран** |
| Rust | Максимальная производительность и безопасность памяти | В 2–3 раза дольше разработка; узкое место панели — nginx/PHP, а не ядро | нет |
| Python (FastAPI) | Скорость разработки | Интерпретатор на хосте: конфликты версий на 9 релизах ОС, venv, медленный старт CLI | нет |
| PHP / Node.js | Знакомы веб-разработчикам | Рантайм на хосте, плохая пригодность для системного демона с root-операциями | нет |

Библиотеки: `go-chi/chi` (роутер), `danielgtaylor/huma/v2` (OpenAPI 3.1 из типов, валидация), `modernc.org/sqlite`, `coreos/go-systemd/v22` (D-Bus), `go-acme/lego/v4` (ACME), `x/crypto/argon2`, `pquerna/otp` (TOTP), `spf13/cobra`, `charmbracelet/bubbletea` v2 + `lipgloss`, `miekg/dns`, `robfig/cron`, `log/slog`, `goccy/go-yaml`, `goreleaser/nfpm` (deb/rpm, отдельный инструмент). Планируется: `go-webauthn/webauthn`, `google/nftables` (сейчас nftables управляется через файл правил и `nft`), `yookoala/gofast` (FastCGI-клиент для phpMyAdmin через панель).

### 4.2 Фронтенд

| Кандидат | Вердикт |
|---|---|
| **Svelte 5 + SvelteKit 2** | **выбран**: самый маленький бандл и лучшая runtime-скорость среди мейнстрима, компилируемая реактивность (runes), статический адаптер, зрелая экосистема админ-компонентов (shadcn-svelte, Bits UI, TanStack) |
| React 19 + Vite | Больше экосистема, но бандл и runtime тяжелее; приемлемая альтернатива, если команда — React |
| Vue 3 / Nuxt | Хорош, но без преимуществ перед Svelte для этой задачи |

Инструменты: Node 24 LTS, pnpm, Vite (последний стабильный), TypeScript strict, ESLint + Prettier, Vitest (unit), Playwright (e2e).

### 4.3 Транспорт и протоколы

- REST + JSON, OpenAPI 3.1, ошибки в формате RFC 9457 (`application/problem+json`).
- SSE для стриминга (события задач, логи, метрики): проходит через любые прокси, нет проблем с таймаутами WebSocket.
- WebSocket только для интерактивного терминала.
- Внутренний RPC api↔agent — тот же HTTP/JSON поверх unix-сокета (переиспользуем huma-типы), с обязательной проверкой peer-cred.

## 5. Модель данных (SQLite)

Основные сущности (упрощённо):

```
users            id, login, role(admin|user), unix_uid, unix_gid, home, shell(bool), status,
                 quota_mb, email, created_at
sites            id, user_id, domain, aliases(json), mode(fpm|apache), php_version, docroot, ip_id,
                 http2, http3, redirect_https, redirect_www(none|to_www|to_root),
                 fpm_pm(ondemand|dynamic|static), fpm_max_children, php_ini(json), disable_functions(json),
                 allow_exec(bool), static_by_nginx(bool), cert_id, status(active|suspended|disabled)
certificates     id, user_id, kind(acme|custom|selfsigned), names(json), acme_account_id, dns_provider_id,
                 not_before, not_after, key_type(ec256|rsa2048), auto_renew, last_error
acme_accounts    id, directory_url, email, key(enc), registered_at
dns_providers    id, user_id, type(cloudflare|route53|hetzner|rfc2136|...), credentials(enc)
ip_addresses     id, ip, iface, is_default, note
php_versions     version, source(sury|remi|monopanel), fpm_service, bin_path, fpm_conf_dir,
                 extensions(json), eol_status, status
db_instances     id, engine(mysql|percona), version, socket, native_password(bool), status
databases        id, user_id, name, charset, collation, size_bytes
db_users         id, user_id, name, host, password(enc), auth_plugin, grants(json)
cron_jobs        id, user_id, schedule, command, enabled, php_version, mail_to
backup_targets   id, type(local|sftp|s3), config(enc), retention(json), schedule
backups          id, target_id, scope(server|panel|user|site|db), snapshot_id, size, status, started_at
jobs             id, type, payload(json), status(queued|running|done|failed|cancelled), progress,
                 log_path, requested_by, lock_key, idempotency_key, created_at, finished_at
audit_log        id, ts, actor, action, target, ip, result, details(json)
sessions         id, user_id, expires_at, ip, ua
api_tokens       id, user_id, name, hash, scopes(json), last_used, expires_at
settings         key, value(json)
metrics_*        ролапы 10s/1m/1h: cpu, mem, disk, net, load; per-site requests/traffic
```

Инварианты:
- `domain` уникален (punycode, нижний регистр, без завершающей точки); алиас не может совпадать с доменом другого сайта.
- Один сайт = один пул FPM = один unix-сокет `/run/monopanel/php/<domain>.sock`.
- `php_version` ссылается на установленную версию; удаление версии блокируется, пока есть сайты на ней.
- Удаление пользователя каскадно удаляет сайты/БД/cron/сертификаты только с явным `purge` (иначе — `suspend`).
- Задачи с одинаковым `lock_key` выполняются строго последовательно.

## 6. Конвейер применения конфигурации

```
API: PATCH /sites/{domain} ─▶ валидация ─▶ desired state в SQLite ─▶ job "site.apply" (lock_key=site:<id>)
                                                                      │
worker: 1) собрать модель сайта из БД (site + user + php_version + cert + ip + settings)
        2) отрендерить: nginx sites/<domain>.conf, (apache sites/<domain>.conf), пул FPM, (default-server IP)
        3) agent.ApplyConfigSet(
              files    = [...],
              validate = ["nginx -t", "apachectl -t"?, "php-fpm -t -y <fpm.conf>"],
              reload   = [nginx, apache?, php-fpm@X.Y (или старый и новый master при смене версии)])
           агент: backup старых файлов → запись во временные → rename → валидаторы;
                  ошибка → восстановить backup → вернуть stderr валидатора в лог задачи;
                  ok     → reload через D-Bus → confhistory
        4) пост-шаги: restorecon (SELinux), ACL, симлинк data/bin/php при смене версии
        5) job=done, событие SSE ⇒ UI обновляет карточку сайта
```

Принципы:
- Панель владеет только файлами в своих каталогах и главным `nginx.conf` (по шаблону, с `include conf.d/*.conf` для ручных правок). Чужие конфиги не редактируются построчно.
- Пользовательские правки — только через include-каталоги `<domain>.d/*.conf`; панель их не перезаписывает, но валидирует вместе со всем набором.
- Шаблоны переопределяются в `/etc/monopanel/templates/` (`mp config templates override nginx/site.conf` копирует встроенный, `diff` показывает расхождение с новой версией панели).
- Полный reconcile (`mp config apply --all`) перегенерирует всё из БД — восстановление после ручных поломок, миграция на новый сервер, обновление шаблонов.
- Смена версии PHP: новый пул создаётся до удаления старого, оба master перезагружаются, затем переключается nginx/Apache — простоя нет.

## 7. Безопасность

- Панель: TLS 1.2/1.3, HSTS, CSP без inline-скриптов, CSRF-токен на мутациях, rate-limit входа + fail2ban-jail по логу панели, argon2id, TOTP/WebAuthn, аудит всех мутаций, API-токены со scope и сроком, опциональная привязка панели к отдельному IP/VPN.
- Процессы: api — `User=monopanel`, `ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`, `CapabilityBoundingSet=` (пусто), `ReadWritePaths=/var/lib/monopanel /run/monopanel`; agent — root, но `ProtectHome=read-only` кроме `/var/www` через `ReadWritePaths`, allow-list операций и путей, все exec — массив `argv` без shell.
- Клиенты: отдельный unix-пользователь и группа, домашний каталог `0710`, доступ веб-серверов через ACL для группы `monopanel-web`; пулы FPM от имени клиента; `open_basedir`, отдельные `tmp`/`session`; `disable_functions` по умолчанию; защита от symlink (`disable_symlinks if_not_owner` в nginx, `SymLinksIfOwnerMatch` в Apache); опционально cgroup-лимиты через изолированные пулы (см. [03-web-stack.md](03-web-stack.md) §6).
- SSH/SFTP: клиент получает shell только по флагу; SFTP-only через `Match Group monopanel-sftp` + `ChrootDirectory`.
- Обновления панели и собственные пакеты — только из подписанного (GPG) репозитория; компоненты стека — только из официальных репозиториев вендоров.
- SELinux на EL остаётся в enforcing (см. [02-platform-matrix.md](02-platform-matrix.md) §6).
- Секреты в БД зашифрованы, ключ вне БД; логи не содержат паролей/токенов (редактор `slog`).

## 8. Наблюдаемость

- Логи панели: `slog` JSON → journald (`monopanel-api`, `monopanel-agent`), плюс `/var/log/monopanel/audit.log` и логи задач `/var/log/monopanel/jobs/<id>.log`.
- Метрики: агент раз в 10 с читает `/proc` (CPU, RAM, swap, диск, сеть, load), состояние сервисов, срок сертификатов; api инкрементально парсит access-логи nginx (запросы, трафик, время ответа по сайтам). Ролапы 10s/1m/1h в SQLite, хранение 30 дней; endpoint `/metrics` (Prometheus) по флагу.
- Логи сайтов: `/var/www/<user>/data/logs/<domain>.{access,error}.log`, `<domain>.php.error.log`, `<domain>.php.slow.log`; ротация logrotate-конфигом панели.
- Уведомления: email/webhook/Telegram о падении сервиса, истекающем сертификате, неудачном бэкапе, заканчивающемся диске.
- Диагностика: `mp doctor` — сервисы, порты, валидность конфигов, репозитории, SELinux-denials, свободное место, расхождения desired state ↔ файлы на диске.

## 9. Упаковка, установка, обновление

- Сборка: `make packages` → бинарники amd64/arm64 → `nfpm` → `monopanel_<ver>_amd64.deb` / `monopanel-<ver>.x86_64.rpm` (планируется `monopanel-selinux` для EL). Внутри: бинарник, unit-файлы (`monopanel-api.service`, `monopanel-agent.service`, `monopanel-php-fpm@.service`), `/etc/monopanel/config.yaml`, встроенные шаблоны, SELinux-модуль, logrotate, fail2ban-фильтры, bash/fish-completion.
- Репозитории: `deb https://repo.<domain>/deb <codename> main` и `https://repo.<domain>/rpm/el$releasever/$basearch` (aptly / `createrepo_c`, подпись GPG). Там же — собственные сборки PHP, phpMyAdmin, restic.
- Установка: `curl -fsSL https://get.<domain> | bash` → определяет ОС и архитектуру → подключает репозиторий → `apt/dnf install monopanel` → `mp setup` (TUI-мастер: hostname панели, admin-пароль, выбор СУБД, версии PHP, режим по умолчанию, IP, firewall) → установка стека из официальных репозиториев. Полностью неинтерактивный режим — `mp setup --config setup.yaml`.
- Обновление панели: `mp update` или из UI (задача `self-update`: `apt/dnf upgrade monopanel` → миграции БД → рестарт api, затем agent). Обновление компонентов стека — по инициативе администратора; мажорные апгрейды СУБД не автоматизируются.

## 10. Структура репозитория

```
monopanel/
├── cmd/monopanel/            # единая точка входа: api | agent | helper | CLI/TUI
├── internal/
│   ├── api/                  # HTTP-хендлеры (huma), auth, SSE, WS, middleware
│   ├── agent/                # привилегированные операции + сервер unix-сокета
│   ├── core/                 # доменные сервисы: users, sites, php, db, certs, backups, cron, firewall, files
│   ├── jobs/                 # очередь, воркеры, шаги, локи
│   ├── render/               # рендер шаблонов, модель конфигов, golden-тесты
│   ├── osprofile/            # debian/, rhel/: пакеты, пути, сервисы, MAC, firewall
│   ├── store/                # запросы, модели, миграции
│   ├── cli/                  # cobra-команды
│   └── tui/                  # bubbletea-экраны
├── templates/                # nginx/, apache/, php-fpm/, mysql/, systemd/, logrotate/, fail2ban/, nftables/
├── web/                      # SvelteKit-приложение (build → embed)
├── packaging/                # nfpm.yaml, units, selinux/, install.sh, repo/
├── build/php/                # Dockerfile'ы и скрипты сборки PHP 5.6–8.5 (этап 2)
├── test/                     # integration: VM/контейнеры по матрице ОС
└── docs/
```

## 11. Тестирование

- Unit: рендер шаблонов (golden-файлы на каждую ОС × режим × версию PHP), валидаторы ввода (домены/IDN, логины, ini-значения), планировщик задач, OS Profile.
- Integration (обязательная матрица в CI): Debian 12, 13; Ubuntu 22.04, 24.04, 26.04; AlmaLinux 9, 10; Rocky Linux 9, 10 — VM (SELinux, nftables, квоты, systemd-слайсы в контейнерах не проверить). Сценарий: install → setup → пользователь/сайт в режиме A → смена PHP → сайт в режиме B → HTTPS через Pebble (тестовый ACME) → БД → бэкап → восстановление → suspend → purge → `mp doctor` без ошибок.
- e2e UI: Playwright против тестового сервера.
- Регресс шаблонов: `nginx -t` / `apachectl -t` / `php-fpm -t` на всех golden-файлах в контейнерах с реальными бинарниками.
