# MonoPanel

Панель управления веб-сервером (single-node) для Debian/Ubuntu и RHEL-семейства.

- Стек сайтов: `nginx → php-fpm` и `nginx → Apache → php-fpm`, переключение на уровне сайта.
- PHP 5.6 … 8.5 параллельно, версия и php.ini на каждый сайт.
- СУБД: MySQL 8.4 LTS или Percona Server for MySQL 8.4 LTS (выбор при установке).
- Управление: Web UI, CLI (`mp`, скриптуемый, `--json`), TUI-меню по SSH, REST API (OpenAPI 3.1). Всё — через один API.
- TLS (ACME: HTTP-01 / DNS-01, wildcard), бэкапы (restic), cron, firewall (nftables) + fail2ban, метрики, логи, файловый менеджер.

## Стек панели

| Слой | Выбор |
|---|---|
| Ядро / демон / CLI / TUI | Go 1.26+, один статический бинарник (`monopaneld api` / `agent` / `helper`, `mp`) |
| HTTP / API | `chi` + `huma` (OpenAPI 3.1 из типов), SSE, WebSocket для терминала |
| Состояние панели | SQLite (WAL) через `modernc.org/sqlite`, `sqlc`, `goose` |
| Web UI | Svelte 5 + SvelteKit 2 (static), TypeScript, Tailwind 4, shadcn-svelte, TanStack Query/Table, uPlot, CodeMirror 6, xterm.js; светлая/тёмная тема по системной настройке с ручным переключателем, анимации переходов и списков, вкладки сайта «Настройки / PHP / nginx / Логи», удаление пользователей с подтверждением |
| CLI / TUI | `cobra` + Bubble Tea v2 / Huh / Lip Gloss |
| ACME | `lego` (библиотека) |
| systemd / nftables | D-Bus (`go-systemd`), `google/nftables` |
| Упаковка | `goreleaser` + `nfpm` → .deb/.rpm, собственный подписанный репозиторий |

## Состояние: этап 0 (каркас) — реализован

Один бинарник `monopanel` (alias `mp`) в трёх ролях: `api` (без привилегий, HTTPS :8443 + unix-сокет для CLI), `agent` (root, типизированные операции по unix-сокету с проверкой peer-cred), `helper` (сброс привилегий для файлов клиентов). Что уже работает и проверено на Ubuntu 24.04:

- `mp setup` — служебный пользователь и группы, каталоги, `config.yaml`, секретный ключ, самоподписанный TLS, SQLite с миграциями, администратор, systemd-units, запуск.
- REST API (OpenAPI 3.1, `/api/v1/docs`): health, status, auth (сессии, Bearer-токены, TOTP позже), users, jobs (+ SSE), tokens, services, stack.
- Очередь задач в SQLite с локами на сущность и стримом прогресса; `mp user add` создаёт unix-пользователя и каталоги `/var/www/<login>/data/{www,logs,tmp,bin}` через агент.
- `mp stack install nginx` — репозиторий nginx.org, установка, `nginx.conf` и сниппеты из шаблонов, default-server на каждый IP, `nginx -t`, reload, enable. Транзакционная запись конфигов с откатом и историей.
- Рендер шаблонов nginx / Apache / php-fpm с golden-тестами; OS Profile для Debian/Ubuntu и EL9/EL10.
- CLI на Cobra и TUI-меню на Bubble Tea v2 поверх того же API; заглушка Web UI (вход, статус) до сборки SvelteKit.
- **TLS / ACME** (`internal/acme`, lego): `mp ssl issue <host>` выпускает сертификат Let's Encrypt по HTTP-01 через webroot nginx (`/var/lib/monopanel/acme/webroot`, сниппет `acme.conf` в каждом `:80` server), хранит его в `/var/lib/monopanel/certs/<host>/`, продлевает за 30 дней до истечения (планировщик в API-процессе, повтор при ошибке не чаще раза в 12 ч). Сертификат для `web.hostname` панель подхватывает на лету для своего HTTPS-порта без перезапуска (`mp web tls`). Перед заказом задача проверяет публичный DNS и то, что порт 80 отдаёт webroot. `--staging` для тестов, `--email` для контакта аккаунта, `--rsa` для RSA-2048.

## Этапы 1–3: реализовано и проверено на Ubuntu 24.04

Всё ниже проходит через тот же REST API и доступно в Web UI, CLI и TUI.

| Область | Команды | Как устроено |
|---|---|---|
| PHP 5.6–8.5 | `mp php list\|install\|remove` | Sury / PPA ondrej / Remi, несколько веток параллельно, свой php-fpm на каждую, `99-monopanel.ini` |
| Сайты | `mp site add\|set\|apply\|suspend\|rm\|logs` | режимы `fpm`, `apache` (loopback 8080, mod_proxy_fcgi), `proxy` (nginx → backend); пул на сайт, ACL для `monopanel-web`, страница-заглушка, авто-сертификат, suspend = 503-заглушка; IP-allow-list на сайт (`--allow`), HSTS при принудительном HTTPS, свои директивы в `sites/<domain>.d/*.conf`; `mp site nginx <domain> --set файл` — свои директивы с проверкой `nginx -t` и откатом; `mp site php <domain>` — действующие PHP-параметры (панель + переопределения сайта); пресеты CMS `--preset wordpress|joomla|bitrix|opencart` (`mp site presets`): свои nginx-локации (ЧПУ, /api Joomla, urlrewrite Bitrix, `_route_` OpenCart, закрытые служебные каталоги, запрет PHP в uploads) и PHP-значения по умолчанию (лимиты, short_open_tag/max_input_vars для Битрикс) |
| App-сервисы | `mp app add\|set\|start\|stop\|restart\|logs\|rm` | systemd-юнит `monopanel-app-<login>-<name>` от имени пользователя (gunicorn, node, боты): команда, workdir и env-file внутри домашнего каталога, автозапуск, журнал через journalctl |
| Apache 2.4 | `mp stack install apache` | Debian/Ubuntu: mpm_event + proxy_fcgi, `conf-available/monopanel.conf` |
| СУБД | `mp stack install percona\|mysql`, `mp db create\|list\|passwd\|rm` | Percona Server / MySQL 8.4 LTS, root по `auth_socket`, тюнинг по RAM, `mysql_native_password` только при PHP < 7.4, базы `<login>_<name>` |
| API-токены | `mp token create\|list\|revoke` | токен принадлежит аккаунту; администратор выпускает его и для другого аккаунта (`--user`), root по локальному сокету — для единственного администратора без флагов |
| TLS | `mp ssl issue\|list\|renew\|rm`, `mp dns-provider add`, `mp web tls` | lego: HTTP-01 по webroot nginx, DNS-01 (Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap, RFC2136) для wildcard, автопродление за 30 дней, hot-swap сертификата панели; `mp ssl import --cert --key` для готовых сертификатов (Let's Encrypt дальше продлеваются через ACME) |
| Cron | `mp cron add\|list\|enable\|disable\|rm` | crontab пользователя целиком из БД, PATH с `~/data/bin` (php нужной версии) |
| Real IP | `mp stack real-ip --cloudflare [--from CIDR]` | доверенные прокси для nginx `real_ip` (сети Cloudflare встроены): allow-list и логи видят адрес клиента, а не прокси |
| Firewall | `mp firewall enable\|allow\|deny\|ban\|unban`, `mp stack install fail2ban` | nftables `inet monopanel`, policy drop, SSH/80/443/панель всегда открыты, unit `monopanel-firewall`; fail2ban с jail'ами sshd, nginx и панели (по журналу `login denied`) |
| Бэкапы | `mp backup target add\|run\|list\|snapshots\|restore` | restic (local/SFTP/S3/B2/REST), дампы MySQL, копия panel.db, retention, ежедневное расписание, восстановление в `<data>/restore/<snapshot>` или in-place |
| Файлы | `mp files ls\|put\|get\|mkdir\|rm\|mv\|chmod\|extract\|size` | `monopanel fsop` через helper с необратимым сбросом привилегий, пути относительно домашнего каталога |
| SFTP / SSH | `mp user add`, `mp user set --shell\|--sftp-only --password` | SFTP-only = chroot в `/var/www/<login>` (root-owned 0750) через `sshd_config.d/monopanel.conf`, пароль общий для панели и SFTP; `mp user rm <login> [--purge]` — удаление вместе с сайтами, базами, cron, app-сервисами, сертификатами и unix-аккаунтом |
| Метрики и логи | `mp metrics`, `mp site logs`, `mp logs <unit>`, `mp doctor` | сэмплер раз в 10 с → точки по минутам (30 дней), хвост логов сайтов и journald через агент, 17+ проверок doctor |
| Безопасность | `mp user totp-reset`, `mp token create`, `mp webhook add` | TOTP 2FA (QR в Web UI), Bearer-токены, webhooks с HMAC-SHA256 на события задач |

Не реализовано: собственные сборки PHP (пока Sury/Remi), Apache и СУБД на EL не проверялись (нет тестового хоста), phpMyAdmin, квоты диска, cgroup-лимиты на сайт, почта, PowerDNS, WAF, multi-server, пакеты deb/rpm с репозиторием.

## Сборка и запуск

```bash
make build            # dist/monopanel (статический, CGO_ENABLED=0)
make check            # gofmt + go vet + golangci-lint + тесты (≈2 с)
make deploy-dev       # scp на тестовый сервер + mp setup (см. Makefile: DEV_HOST)
make help             # все цели
```

## Тестирование

Прогон всего набора занимает пару секунд, поэтому его дёшево запускать на каждое изменение.

| Команда | Что делает |
| --- | --- |
| `make test` | юнит-тесты; вся бизнес-логика панели проверяется через фейковый агент, без root и systemd |
| `make check` | то же плюс `gofmt`, `go vet` и `golangci-lint` — то, что гоняет CI |
| `make test-race` | детектор гонок (≈1 мин): очередь задач, SSE-брокер, кеш сертификатов |
| `make cover` | покрытие по пакетам и суммарное; `make cover-html` — отчёт в браузере |
| `make web-check` | типы и разметка Web UI (`svelte-check`) |
| `scripts/check-templates.sh` | скармливает сгенерированные конфиги настоящим `nginx -t` и `apachectl -t` |
| `make e2e` | сценарий на живой панели: пользователь → сайт с пресетом → база → удаление |

Фейковый агент (`internal/agent/agenttest`) поднимает unix-сокет и отвечает на операции
привилегированного агента, записывая всё, что панель попыталась сделать. Благодаря этому
тест видит содержимое сгенерированного server-блока nginx и пула php-fpm и проверяет
поведение целиком: пресеты CMS, allow-list по IP, приостановку сайта, каскадное удаление
пользователя. Ожидания готовности nginx и сокета php-fpm в тестах отключаются
(`SetReadinessWaits(0, 0)`), поэтому набор идёт секунды, а не минуты.

End-to-end гоняется против настоящей панели и убирает за собой всё, что создал:

```bash
make e2e HOST=toolkit      # токен берётся по ssh и отзывается после прогона
# или явно:
MONOPANEL_URL=https://panel:8443 MONOPANEL_TOKEN='…' make e2e
```

CI (GitHub Actions) на каждый push: тесты с детектором гонок и покрытием, линтер,
проверка шаблонов реальными nginx и Apache, сборка Web UI с проверкой типов, сборка
бинарника под amd64 и arm64. E2E запускается вручную (`workflow_dispatch`) — ему нужен
доступ к хосту, URL и учётные данные берутся из секретов репозитория.

На сервере (root):

```bash
mp setup --admin-password '…'   # или без пароля: сгенерирует и покажет
mp status                       # панель, хост, сервисы, задачи
mp user add alex --generate --shell
mp stack install nginx
mp config set web.hostname panel.example.com --restart
mp ssl issue panel.example.com  # Let's Encrypt; панель сразу отдаёт его на :8443
mp php install 8.4              # + 7.4, 5.6 … параллельно
mp stack install percona        # Percona Server 8.4, root через auth_socket
mp site add example.com --user alex --www          # nginx + php-fpm, сертификат сам
mp site add old.example.com --user alex --php 7.4 --mode apache
mp site add app.example.com --user alex --mode proxy --backend http://127.0.0.1:3000
mp db create shop --user alex --generate
mp firewall enable && mp stack install fail2ban
mp backup target add local1 --repo /var/backups/monopanel --schedule daily
mp                              # TUI-меню
```

Удалённо: `mp --server https://host:8443 --token <token> status` (токен: `mp token create` от имени аккаунта).

## Структура

```
cmd/monopanel/        точка входа
internal/api/         HTTP API (huma + chi), SSE, UI, TLS, job-хендлеры (user.provision, stack.install)
internal/agent/       привилегированный агент: ApplyConfigSet, EnsureUnixUser, EnsureDirs, Service, Pkg
internal/jobs/        очередь задач, воркеры, брокер событий
internal/store/       SQLite, миграции, модели
internal/osprofile/   различия Debian/RHEL
internal/render/      рендер шаблонов + golden-тесты
internal/cli/ tui/    команды mp и TUI-меню
internal/setup/       mp setup
internal/client/      Go-клиент API (CLI, TUI, setup)
templates/            nginx/, apache/, php-fpm/, systemd/
web/                  SvelteKit-приложение (build/ вшивается в бинарник)
packaging/            nfpm.yaml, units, sysusers/tmpfiles, install.sh
```

## Документация

| Документ | Содержание |
|---|---|
| [docs/01-architecture.md](docs/01-architecture.md) | Цели, архитектурные решения, компоненты, стек и обоснование, модель данных, конвейер применения конфигов, безопасность, наблюдаемость, упаковка, структура репозитория, тестирование |
| [docs/02-platform-matrix.md](docs/02-platform-matrix.md) | Поддерживаемые ОС, источники пакетов, матрица PHP-версий и расширений, MySQL 8.4 / Percona 8.4, OS Profile (различия ОС), SELinux, firewall |
| [docs/03-web-stack.md](docs/03-web-stack.md) | Режимы nginx+php-fpm и nginx+Apache, файловая структура, шаблоны nginx/Apache/php-fpm, изоляция и лимиты, TLS/ACME, HTTP/3, логи |
| [docs/04-cli-tui-api.md](docs/04-cli-tui-api.md) | Команды CLI, экраны TUI, REST API, интеграция с биллингом (WHMCS) |
| [docs/05-roadmap.md](docs/05-roadmap.md) | Этапы разработки, матрица CI, тестовые сценарии |

## Ключевые решения в одну строку

1. Один бинарник Go, без рантайма на хосте.
2. Разделение привилегий: непривилегированный API-процесс ↔ root-агент с закрытым набором типизированных операций.
3. Один API для Web, CLI, TUI и интеграций.
4. Декларативное состояние в SQLite → рендер шаблонов → валидация (`nginx -t` и др.) → атомарное применение → reload, с откатом.
5. Панель слушает свой HTTPS-порт (8443) и не зависит от системного nginx.
6. Один php-fpm master на версию, пул на сайт; Apache только через `mod_proxy_fcgi`, `mod_php` не поддерживается.
7. PHP: этап 1 — Sury (deb) / Remi (rpm), этап 2 — собственные сборки с единым layout `/opt/monopanel/php/<X.Y>`.
8. nginx — с nginx.org на всех ОС (единый layout, HTTP/3); Apache — из дистрибутива.
9. Вся разница между Debian и RHEL — в одном пакете `osprofile`.
