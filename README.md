# MonoPanel

[English](README.en.md) · Русский

Панель управления веб-сервером для Debian/Ubuntu и RHEL-семейства: сайты, PHP, базы,
TLS, почта, бэкапы, firewall и переезд между серверами — одним статическим бинарником,
без рантайма, агентов на других языках и внешних зависимостей.

[![ci](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml/badge.svg)](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Logmen/MonoPanel)](https://github.com/Logmen/MonoPanel/releases)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)

Панель ставится одним пакетом, поднимает свой HTTPS-порт и не зависит от системного
nginx: если конфигурация сайта сломается, панель останется доступной и починит её.
Web UI, CLI, TUI и интеграции работают через один и тот же REST API — то, что можно
сделать мышкой, можно сделать и скриптом.

Состояние: **0.7.0**, ежедневно используется на боевом сервере с несколькими сайтами
и почтой. Разработка идёт быстро, ломающие изменения до 1.0 возможны.

## Установка

```bash
curl -fsSL https://raw.githubusercontent.com/Logmen/MonoPanel/main/packaging/install.sh | sh
mp setup
```

Скрипт определяет ОС и архитектуру, скачивает пакет последнего релиза, сверяет
контрольную сумму и ставит его. Можно и вручную — `.deb` и `.rpm` для amd64 и arm64
лежат в [релизах](https://github.com/Logmen/MonoPanel/releases).

`mp setup` создаёт служебного пользователя, каталоги, базу, самоподписанный TLS и
администратора, после чего печатает адрес панели и пароль.

Требуется root, systemd и одна из: Debian 12/13, Ubuntu 22.04/24.04/26.04,
AlmaLinux, Rocky Linux или Oracle Linux 9/10. Вся матрица прогоняется на
[тестовой площадке](docs/08-testbed.md): установка панели, nginx, PHP, Percona и
сценарий e2e проходят на всех одиннадцати ОС.
На EL SELinux остаётся в enforcing — панель сама настраивает контексты и булевы под
хостинг. Для Ubuntu 26.04 у `ppa:ondrej/php` пока нет сборок, поэтому там ставится
PHP 8.5 из самой Ubuntu; как только PPA появится, панель подключит его сама.

## Быстрый старт

```bash
mp setup --admin-password '…'   # или без пароля: сгенерирует и покажет
mp status                       # панель, хост, сервисы, задачи
mp stack install nginx
mp config set web.hostname panel.example.com --restart
mp ssl issue panel.example.com  # Let's Encrypt; панель сразу отдаёт его на :8443

mp user add alex --generate --shell
mp php install 8.4              # + 7.4, 5.6 … параллельно
mp stack install percona        # Percona Server 8.4, root через auth_socket

mp site add example.com --user alex --www --preset wordpress
mp site add old.example.com --user alex --php 7.4 --mode apache
mp site add app.example.com --user alex --mode proxy --backend http://127.0.0.1:3000
mp db create shop --user alex --generate
mp firewall enable && mp stack install fail2ban
mp backup target add local1 --repo /var/backups/monopanel --schedule daily

mp mail install --hostname mail.example.com   # postfix + dovecot + opendkim
mp mail domain add example.com --user alex    # + ключ DKIM
mp mail box add ivan@example.com --quota 2048
mp mail domain dns example.com                # что прописать в DNS
mp mail webmail webmail.example.com --user alex --port 2096   # Roundcube

# переезд аккаунта с другого сервера с MonoPanel
mp migrate grant user:alex                        # на старом сервере
mp migrate plan --source https://old:8443 --token … --scope user:alex
mp migrate run  --source https://old:8443 --token … --scope user:alex

mp                              # TUI-меню
```

Удалённо: `mp --server https://host:8443 --token <token> status`
(токен: `mp token create`).

## Что умеет

Всё перечисленное проходит через один REST API и доступно в Web UI, CLI и TUI.

| Область | Команды | Как устроено |
|---|---|---|
| PHP 5.6–8.5 | `mp php list\|install\|remove`, `mp php ext list\|enable\|disable` | Sury / PPA ondrej / Remi, несколько веток параллельно, свой php-fpm на каждую, `99-monopanel.ini`; расширения ветки можно выключать и включать (Debian/Ubuntu — `phpenmod`/`phpdismod`, EL — правка `extension=` в ini ветки Remi; затем перезапуск php-fpm) — действует на все сайты ветки, потому что мастер php-fpm один на версию; расширения, которые репозиторий предлагает, но они не установлены (memcache, redis, imagick …), показываются в том же списке, и включение ставит пакет |
| Сайты | `mp site add\|set\|apply\|suspend\|rm\|logs` | режимы `fpm`, `apache` (loopback 8080, mod_proxy_fcgi), `proxy` (nginx → backend); пул на сайт, ACL для `monopanel-web`, страница-заглушка, авто-сертификат, suspend = 503-заглушка; IP-allow-list на сайт (`--allow`), HSTS при принудительном HTTPS, свои директивы в `sites/<domain>.d/*.conf`; `mp site nginx <domain> --set файл` — свои директивы с проверкой `nginx -t` и откатом; `mp site php <domain>` — действующие PHP-параметры; пресеты CMS `--preset wordpress\|joomla\|bitrix\|opencart` (`mp site presets`): свои nginx-локации (ЧПУ, /api Joomla, urlrewrite Bitrix, `_route_` OpenCart, закрытые служебные каталоги, запрет PHP в uploads) и PHP-значения по умолчанию; Bitrix — по правилам BitrixVM, с opcache на 100000 файлов и без open_basedir |
| App-сервисы | `mp app add\|set\|start\|stop\|restart\|logs\|rm` | systemd-юнит `monopanel-app-<login>-<name>` от имени пользователя (gunicorn, node, боты): команда, workdir и env-file внутри домашнего каталога, автозапуск, журнал через journalctl |
| Apache 2.4 | `mp stack install apache` | Debian/Ubuntu: mpm_event + proxy_fcgi, `conf-available/monopanel.conf` |
| Расширения | `mp stack install\|remove memcached\|jpegoptim\|git\|composer\|sphinx`, `mp stack memcached --memory-mb --max-conn` | страница «Расширения» в Web UI: memcached (только 127.0.0.1, память и соединения настраиваются, перезапуск при смене), jpegoptim, git, composer (с getcomposer.org с проверкой контрольной суммы, запускается на новейшей ветке PHP панели, повторная установка обновляет), sphinx для 1С-Битрикс (Sphinx 2.2 из дистрибутива на Debian/Ubuntu, сборка Sphinx 3 с sphinxsearch.com на EL; индекс `bitrix` по эталону из настроек Битрикса, устаревший индекс на диске пересоздаётся, SphinxQL на 127.0.0.1:9306) |
| СУБД | `mp stack install percona\|mysql`, `mp db create\|list\|passwd\|rm` | Percona Server / MySQL 8.4 LTS, root по `auth_socket` (на EL панель сама переводит его с временного пароля пакета), тюнинг по RAM, `mysql_native_password` только при PHP < 7.4, базы `<login>_<name>`, сгенерированные пароли проходят `validate_password` |
| TLS | `mp ssl panel issue\|import\|self-signed`, `mp site tls <domain>`, `mp ssl issue\|list\|renew\|rm`, `mp dns-provider add` | lego: HTTP-01 по webroot nginx, DNS-01 (Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap, RFC2136) для wildcard, автопродление за 30 дней; сертификат самой панели и сертификаты сайтов ведутся раздельно — панель заказывает только для своего имени и подхватывает его на лету, сайт заказывает для домена с алиасами и сам переключается на HTTPS; занятый сертификат не удалить; `mp ssl import --cert --key` для готовых |
| Перенос между панелями | `mp migrate grant\|plan\|run` | аккаунт целиком переезжает на другой сервер с MonoPanel: источник выдаёт токен с областью на один аккаунт и только читает, приёмник разбирает конфликты (`plan` ничего не меняет) и забирает — файлы и дамп идут потоком насквозь, пароли панели, SFTP, MySQL и почты переезжают хешами, поэтому пользователи смены сервера не замечают ([docs/07-migration.md](docs/07-migration.md)) |
| Обновление панели | `mp update`, `mp update apply` | релизы этого репозитория: панель находит новую версию, скачивает пакет для своей ОС, проверяет подпись ed25519 и ставит его отдельным systemd-юнитом с откатом на прежний бинарник, если новая версия не отвечает |
| API-токены | `mp token create\|list\|revoke` | токен принадлежит аккаунту; администратор выпускает его и для другого аккаунта (`--user`), root по локальному сокету — для единственного администратора без флагов |
| Cron | `mp cron add\|list\|enable\|disable\|rm` | crontab пользователя целиком из БД, PATH с `~/data/bin` (php нужной версии) |
| Real IP | `mp stack real-ip --cloudflare [--from CIDR]` | доверенные прокси для nginx `real_ip` (сети Cloudflare встроены): allow-list и логи видят адрес клиента, а не прокси |
| Firewall | `mp firewall enable\|allow\|deny\|ban\|unban`, `mp stack install fail2ban` | nftables `inet monopanel`, policy drop, SSH/80/443/панель всегда открыты, unit `monopanel-firewall`; fail2ban с jail'ами sshd, nginx и панели |
| Почта | `mp mail install\|status\|domain\|box\|alias\|dns\|webmail` | postfix + dovecot + opendkim: домены, ящики (пароли и квоты в панели, Maildir у `vmail`), алиасы и catch-all, IMAP/POP3/submission с TLS панели, подпись DKIM, sieve-фильтры; `mp mail domain dns` показывает нужные MX/SPF/DKIM/DMARC/PTR и проверяет их по публичным резолверам; вебпочта Roundcube ставится отдельным сайтом или на порту почтового хоста (`--port 2096` — без своей записи в DNS и своего сертификата); домен можно объявить приёмником (`--lenient`) — он примет письма и от криво настроенных отправителей ([docs/06-mail.md](docs/06-mail.md)) |
| Бэкапы | `mp backup target add\|run\|list\|snapshots\|restore` | restic (local/SFTP/S3/B2/REST), дампы MySQL, копия panel.db, retention, ежедневное расписание, восстановление в `<data>/restore/<snapshot>` или in-place |
| Файлы | `mp files ls\|put\|get\|mkdir\|rm\|mv\|chmod\|extract\|size` | `monopanel fsop` через helper с необратимым сбросом привилегий, пути относительно домашнего каталога; в Web UI — файловый менеджер с редактором: обзор каталога, загрузка перетаскиванием, права, распаковка архивов и правка файлов в редакторе VS Code (Monaco: подсветка php/html/css/js/sql/yaml/ini, поиск и замена, мультикурсор, свёртка, F1 — палитра команд), вкладка «Файлы» в карточке сайта открывается сразу в его docroot |
| SFTP / SSH | `mp user add`, `mp user set --shell\|--sftp-only --password` | SFTP-only = chroot в `/var/www/<login>` через `sshd_config.d/monopanel.conf`, пароль общий для панели и SFTP; `mp user rm <login> [--purge]` — удаление вместе с сайтами, базами, cron, app-сервисами и сертификатами |
| Метрики и логи | `mp metrics`, `mp site logs`, `mp logs <unit>`, `mp doctor` | сэмплер раз в 10 с → точки по минутам (30 дней), хвост логов сайтов и journald через агент; doctor проверяет сервисы, конфиги, диск, сертификаты, DNS, задачи и дрейф файлов |
| Безопасность | `mp user totp-reset`, `mp webhook add` | TOTP 2FA (QR в Web UI), Bearer-токены, webhooks с HMAC-SHA256 на события задач |

### Чего пока нет

Собственные сборки PHP (используются Sury/Remi), проверенный Apache на EL, phpMyAdmin,
дисковые квоты, cgroup-лимиты на сайт, DNS-сервер, WAF, несколько серверов из одной
панели, apt/yum-репозиторий (пакеты выкладываются релизами, панель ставит их сама).
Почта работает на Debian/Ubuntu с dovecot 2.3; для EL и для dovecot 2.4 (Debian 13,
Ubuntu 26.04) конфигурация ещё не написана, контент-фильтра (rspamd) нет. Переезд пока
только между двумя MonoPanel и без досинхронизации перед переключением DNS; адаптеры
для чужих панелей — в планах ([docs/07-migration.md](docs/07-migration.md)).

## Как устроено

Один бинарник `monopanel` (он же `mp`) работает в нескольких ролях:

| Роль | Права | Зачем |
|---|---|---|
| `api` | обычный пользователь `monopanel` | HTTPS :8443, unix-сокет для CLI, очередь задач, планировщики |
| `agent` | root | закрытый набор типизированных операций по unix-сокету (никаких строк с командами) |
| `helper` | setuid, необратимый сброс прав | файловые операции от имени клиента |
| `fsop` | права клиента | сама файловая операция |

Состояние живёт в SQLite. Изменение сайта — это не правка файла на диске, а запись в
базу, из которой рендерятся конфиги: шаблон → валидация (`nginx -t`, `apachectl -t`,
`php-fpm -t`) → атомарная запись всех файлов сразу → reload. Любая неудача на этом
пути откатывает весь набор файлов и возвращает текст ошибки.

Принципы, коротко:

1. Один бинарник Go, без рантайма на хосте.
2. Разделение привилегий: непривилегированный API-процесс ↔ root-агент с allow-list операций и путей.
3. Один API для Web, CLI, TUI и интеграций.
4. Декларативное состояние → рендер → валидация → атомарное применение → reload, с откатом.
5. Панель слушает свой порт и не зависит от системного nginx.
6. Один php-fpm master на версию, пул на сайт; Apache только через `mod_proxy_fcgi`.

## Обновление панели

Версия выпускается тегом; всё остальное делает CI. Тег `v0.7.0` собирает `.deb` и
`.rpm` под amd64 и arm64, подписывает список контрольных сумм ключом ed25519 из
секрета репозитория и публикует релиз; текст аннотированного тега становится
описанием релиза.

```bash
make keygen                  # один раз: ключ подписи (приватный — в секрет MONOPANEL_RELEASE_KEY)
make release VERSION=0.7.0   # тег + push, дальше CI собирает и публикует
make packages VERSION=0.7.0  # то же локально, без публикации
```

На сервере:

```bash
mp update trust --key <публичный ключ>   # пишется в config.yaml, доступен только root
mp update settings --repo owner/name     # --token-stdin для приватного репозитория
mp update                                # что установлено и что доступно
mp update apply                          # скачать, проверить, установить, перезапуститься
```

Проверка идёт по расписанию (по умолчанию раз в сутки), `--auto-apply` ставит
обновление без участия человека. Устанавливает не сама панель: агент запускает
transient-юнит `monopanel-update.service`, который переживает перезапуск API и агента
и возвращает прежний бинарник, если новая версия не отвечает. Пока задан ключ,
неподписанный релиз не установится. Медленный канал не помеха: у загрузки пакета нет
общего таймаута, обрывается только застывший поток.

## Стек

| Слой | Выбор |
|---|---|
| Ядро, CLI, TUI | Go 1.27, один статический бинарник (`CGO_ENABLED=0`) |
| HTTP / API | `chi` + `huma` v2 (OpenAPI 3.1 из типов), SSE для прогресса задач |
| Состояние | SQLite (WAL) через `modernc.org/sqlite`, встроенные SQL-миграции |
| Web UI | Svelte 5 + SvelteKit 2 (static), TypeScript, Tailwind 4 — собирается в `web/build` и вшивается в бинарник; адаптивен: на телефоне меню выезжает, а таблицы становятся карточками |
| CLI / TUI | `cobra`, Bubble Tea v2 + Lip Gloss v2 |
| ACME | `lego` как библиотека |
| Почта | postfix + dovecot (IMAP/POP3/LMTP/sieve) + opendkim, вебпочта Roundcube |
| systemd | D-Bus (`go-systemd`) |
| Упаковка | `nfpm` → .deb/.rpm, релизы с подписанными контрольными суммами |

## Разработка

```bash
make build            # dist/monopanel
make check            # gofmt + go vet + golangci-lint + тесты (≈2 с)
make web              # Web UI в web/build (нужен node + pnpm)
make help             # все цели
```

Прогон всего набора занимает пару секунд, поэтому его дёшево запускать на каждое
изменение.

| Команда | Что делает |
| --- | --- |
| `make test` | юнит-тесты; вся логика панели проверяется через фейковый агент, без root и systemd |
| `make check` | то же плюс `gofmt`, `go vet` и `golangci-lint` — то, что гоняет CI |
| `make test-race` | детектор гонок (≈1 мин): очередь задач, SSE-брокер, кеш сертификатов |
| `make cover` | покрытие по пакетам; `make cover-html` — отчёт в браузере |
| `make web-check` | типы и разметка Web UI (`svelte-check`) |
| `scripts/check-templates.sh` | скармливает сгенерированные конфиги настоящим `nginx -t` и `apachectl -t` |
| `make e2e` | сценарий на живой панели: пользователь → сайт с пресетом → база → удаление |
| `make testbed-matrix` | тот же сценарий на девяти VM площадки (по одной на каждую ОС матрицы) параллельно, с откатом к чистому снимку; `make testbed-migrate SRC= DST=` — настоящий переезд аккаунта между двумя из них с проверкой |

Фейковый агент (`internal/agent/agenttest`) поднимает unix-сокет и отвечает на операции
привилегированного агента, записывая всё, что панель попыталась сделать. Тест видит
содержимое сгенерированного server-блока nginx и пула php-fpm и проверяет поведение
целиком: пресеты CMS, allow-list по IP, приостановку сайта, каскадное удаление
пользователя, проверку подписи обновления.

End-to-end гоняется против настоящей панели и убирает за собой всё, что создал:

```bash
make e2e HOST=<ssh-алиас>   # токен берётся по ssh и отзывается после прогона
# или явно:
MONOPANEL_URL=https://panel:8443 MONOPANEL_TOKEN='…' make e2e
```

CI на каждый push: тесты с детектором гонок и покрытием, линтер, проверка шаблонов
реальными nginx и Apache, сборка Web UI с проверкой типов, сборка бинарника под amd64
и arm64. E2E запускается вручную (`workflow_dispatch`) — ему нужен доступ к живому хосту.
Матрица ОС гоняется с рабочего места на площадке Proxmox
([docs/08-testbed.md](docs/08-testbed.md)): скрипты в `scripts/testbed/`, адрес хоста и
сеть — в git-игнорируемом `.dev/testbed.env`.

### Структура

```
cmd/monopanel/        точка входа
internal/api/         HTTP API (huma + chi), SSE, UI, TLS, job-хендлеры
internal/agent/       привилегированный агент: ApplyConfigSet, EnsureUnixUser, Service, Pkg
internal/jobs/        очередь задач, воркеры, брокер событий
internal/store/       SQLite, миграции, модели
internal/osprofile/   различия Debian/RHEL
internal/render/      рендер шаблонов + golden-тесты
internal/updater/     поиск релиза, проверка подписи, установка пакета с откатом
internal/cli/ tui/    команды mp и TUI-меню
internal/client/      Go-клиент API (CLI, TUI, setup)
templates/            nginx/, apache/, php-fpm/, systemd/
web/                  SvelteKit-приложение (build/ вшивается в бинарник)
web/static/monaco/    редактор VS Code (Monaco), урезанная сборка — см. его README
packaging/            nfpm.yaml, units, sysusers/tmpfiles, install.sh
scripts/release/      генерация ключа и подпись SHA256SUMS для релиза
scripts/testbed/      площадка: VM на Proxmox, bootstrap панели, прогон матрицы и переезда
```

## Документация

| Документ | Содержание |
|---|---|
| [docs/01-architecture.md](docs/01-architecture.md) | Цели, архитектурные решения, компоненты, модель данных, конвейер применения конфигов, безопасность, наблюдаемость, упаковка |
| [docs/02-platform-matrix.md](docs/02-platform-matrix.md) | Поддерживаемые ОС, источники пакетов, матрица PHP и расширений, MySQL/Percona, OS Profile, SELinux, firewall |
| [docs/03-web-stack.md](docs/03-web-stack.md) | Режимы nginx+php-fpm и nginx+Apache, файловая структура, шаблоны, изоляция и лимиты, TLS/ACME, HTTP/3, логи |
| [docs/04-cli-tui-api.md](docs/04-cli-tui-api.md) | Команды CLI, экраны TUI, REST API, интеграция с биллингом (WHMCS) |
| [docs/05-roadmap.md](docs/05-roadmap.md) | Этапы разработки, матрица CI, тестовые сценарии, риски |
| [docs/06-mail.md](docs/06-mail.md) | Почта: postfix + dovecot + opendkim, путь письма, файлы и порты, DNS-записи, вебпочта, границы |
| [docs/07-migration.md](docs/07-migration.md) | Перенос между панелями: что уже работает, порядок с переключением DNS, пакет переезда и адаптеры для чужих панелей (проект) |
| [docs/08-testbed.md](docs/08-testbed.md) | Тестовая площадка: по машине на каждый дистрибутив матрицы на Proxmox, прогон e2e и переноса между панелями, что она нашла |

Справочник API живёт в самой панели: `/api/v1/docs` (OpenAPI 3.1).

## Безопасность

О найденных уязвимостях сообщайте приватно — [SECURITY.md](SECURITY.md).

## Лицензия

[Apache License 2.0](LICENSE).
