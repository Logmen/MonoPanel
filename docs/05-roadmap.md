# 05. Дорожная карта, матрица тестирования, CI

## Этап 0 — каркас (выполнен 2026-09-05, проверен на Ubuntu 24.04)

- [x] Репозиторий, сборка Go (`make build`), `nfpm.yaml`, units, sysusers/tmpfiles, `install.sh`, `mp setup`.
- [x] OS Profile для Debian/Ubuntu и EL9/EL10 (+ generic для dev-машин); агент с `ApplyConfigSet`, `EnsureGroup`, `EnsureUnixUser`, `EnsureDirs`, `Service`, `Pkg`; peer-cred на обоих сокетах.
- [x] SQLite-схема и миграции; auth (argon2id, сессии, Bearer-токены, CSRF-проверка); job-runner с локами; SSE.
- [x] Установка nginx с nginx.org (`mp stack install nginx`), шаблоны nginx/Apache/php-fpm с golden-тестами, CLI и TUI.
- [x] ACME (lego, HTTP-01 по webroot): `mp ssl issue/list/renew/rm`, таблица `certificates`, автопродление, hot-swap сертификата панели (`mp web tls`). Проверено выпуском боевого сертификата Let's Encrypt для toolkit.onehost.kz.
- [ ] Сборка SvelteKit-приложения (нужен Node; пока заглушка `web/build/index.html`).
- [ ] Репозиторий deb/rpm и подпись пакетов.
- [ ] Установка PHP (Sury/Remi) и Percona/MySQL 8.4 с `auth_socket` — переносится в начало этапа 1.
- [ ] VM-матрица в CI (Debian 13 + AlmaLinux 10) — пока ручная проверка на одном хосте.

## Этап 1 — MVP (выполнен 2026-09-05, проверен на Ubuntu 24.04)

- [x] PHP 5.6–8.5 через Sury/Remi (`mp php`), несколько версий параллельно.
- [x] Сайты в режимах A (nginx → php-fpm), B (nginx → Apache → php-fpm) и proxy; per-site php_value; ACL; страница-заглушка; авто-сертификат.
- [x] Apache 2.4 как компонент (Debian/Ubuntu). EL — не проверено.
- [x] БД: Percona/MySQL 8.4 с `auth_socket`, базы и пользователи, legacy-аутентификация для PHP < 7.4. phpMyAdmin — не сделан.
- [x] Cron, firewall (nftables) + fail2ban, бэкапы restic, метрики, логи, `mp doctor`, DNS-01, TUI-паритет, Web UI (SvelteKit), ru.
- [ ] Полная тест-матрица в CI на 9 ОС (нужен self-hosted раннер с VM).

## Этап 2 — v1.0 (частично, 2026-09-05)

- [x] Файловый менеджер через helper (`mp files`, API `/files`), SFTP-chroot, unix-пароли.
- [x] Webhooks (HMAC), TOTP 2FA, API-токены, история конфигов (`confhistory`), переопределение шаблонов, `mp doctor`.
- [ ] Собственные сборки PHP, терминал в браузере, квоты, self-update, WHMCS-модуль, WebAuthn, SELinux confined-домен.

### Автоматическое тестирование (2026-09-06)
- [x] Фейковый агент `internal/agent/agenttest`: unix-сокет, типизированные ответы, запись всех вызовов — покрывает job-и панели (сайты, пользователи, пресеты) без root и systemd.
- [x] Тесты пайплайна сайта: рендер nginx/пула, пресеты CMS и их PHP-значения, приоритет `php_ini` над пресетом, allow-list, отказы валидации, suspend, удаление, свои nginx-директивы с откатом, каскадное удаление пользователя. Покрытие `internal/api` 9.5% → 21.2%, общее 23.3%.
- [x] Таймауты готовности вынесены в `Server.SetReadinessWaits`: тесты не ждут nginx и сокет php-fpm (набор идёт ~2 с).
- [x] `make check` (fmt + vet + lint + test), `make test-race`, `make cover`, `make web-check`, `make help`.
- [x] `golangci-lint` v2.13.2 с `.golangci.yml`; намеренно игнорируемые ошибки помечены `//nolint:errcheck` с причиной, реальные находки (S1017, S1009, ineffassign, unconvert) исправлены.
- [x] `scripts/check-templates.sh`: golden-конфиги проверяются настоящими `nginx -t` и `apachectl -t` (в CI ставится nginx-core и apache2).
- [x] `e2e/` (тег `e2e`): сценарий против живой панели — аккаунт, сайт с пресетом, PHP отвечает, запрет PHP в uploads работает, база, удаление; чистит за собой. Токен или логин/пароль через переменные окружения.
- [x] Токены по локальному сокету: `POST /tokens` принимает `user`, root без аккаунта получает токен единственного администратора (при нескольких — ошибка со списком), администратор видит и отзывает токены любого аккаунта; `make e2e HOST=…` берёт токен по ssh и отзывает его после прогона.
- [x] CI: параллельные джобы go / lint / templates / web, кэш модулей и pnpm, `-race` с покрытием в summary, сборка amd64 + arm64, e2e по `workflow_dispatch`.

### Добавлено при миграции сайтов с FASTPANEL (2026-09-05)
- [x] IP-allow-list на сайт (`sites.allow_from`, `mp site add|set --allow`), ACME-проверка остаётся доступной.
- [x] Импорт готовых сертификатов (`POST /certificates/import`, `mp ssl import`); Let's Encrypt-сертификаты продолжают продлеваться через ACME.
- [x] Доверенные прокси для `real_ip` (`mp stack real-ip --cloudflare`, `--from`), файл `http.d/10-real-ip.conf`.
- [x] App-сервисы пользователей (`apps`, `mp app`): systemd-юнит от имени пользователя, `ProtectSystem=full`, `PrivateTmp`.
- [x] HSTS в server-блоке и в static-локации при принудительном HTTPS.
- [x] Web UI: allow-list в настройках сайта, импорт сертификатов, app-сервисы и cron в карточке пользователя, real-ip в настройках.
- [x] Удаление пользователя (`DELETE /users/{login}?purge=`, job `user.delete`, `mp user rm`): сайты → сертификаты → базы → app-сервисы → crontab → unix-аккаунт (агент `user/remove`: убивает процессы uid, `userdel [-r]` только для home под www_root, uid ≥ 1000).
- [x] Редактирование nginx-директив сайта (`sites/<domain>.d/custom.conf`, `GET|PUT /sites/{domain}/nginx`, `mp site nginx`): `nginx -t`, откат и текст ошибки в ответе; сгенерированный server-блок показывается read-only.
- [x] PHP-параметры сайта: `GET /sites/{domain}/php` (эффективные значения + допустимые ключи), редактор переопределений во вкладке PHP; список ключей расширен (opcache.jit, session.cookie_*, max_file_uploads …).
- [x] Пресеты CMS (`sites.preset`, `templates/nginx/presets/*.conf.tmpl` как `{{ define "preset-…" }}` в общем наборе шаблонов, `presetIni` в `ops_presets.go`): WordPress, Joomla, 1С-Битрикс, OpenCart; выбор в форме создания и в настройках сайта, `GET /sites/presets`; проверены на nginx реальными запросами (ЧПУ, запреты, /api, urlrewrite, _route_).
- [x] Дизайн Web UI 0.4: токены светлой/тёмной темы (`prefers-color-scheme` + `data-theme` из localStorage, инициализация до первой отрисовки с хешем в CSP), анимации (переходы страниц, stagger строк, прогресс задач, skeleton), иконки, модальные подтверждения.

## Этап 3 — v1.x

- [x] Режим `proxy` для Node/Python/Docker-приложений.
- [ ] Изолированные пулы с cgroup-лимитами, FTP, DNS (PowerDNS), почта, WAF, reseller-роль, aarch64-сборка (кросс-компиляция готова: `make build-arm64`), multi-server.

## Матрица CI

| ОС | Режимы | PHP (выборочно) | СУБД |
|---|---|---|---|
| Debian 12, 13 | A, B | 5.6, 7.4, 8.2, 8.5 | Percona 8.4, MySQL 8.4 |
| Ubuntu 22.04, 24.04, 26.04 | A, B | 7.4, 8.3, 8.5 | Percona 8.4 |
| AlmaLinux 9, 10; Rocky Linux 9, 10 | A, B | 7.4, 8.4, 8.5 (+ 5.6 на EL9) | Percona 8.4, MySQL 8.4 |

Раннер: VM (libvirt / cloud-образы + cloud-init). Контейнеров недостаточно для SELinux, nftables, квот и systemd-слайсов.

Сценарий: install → setup → user/site (A) → смена PHP → site (B) → HTTPS через Pebble → DB → backup → restore → suspend → purge → `mp doctor` без ошибок → `mp config apply --all` и `nginx -t` / `apachectl -t`.

Периодический job (еженедельно): доступность пакетов в вендорских репозиториях по матрице и появление новых версий (PHP 8.6/9.0, nginx stable, Percona 8.4.x, релизы ОС).

## Риски и как закрыты

| Риск | Митигация |
|---|---|
| Sury/Remi исчезнут или сломают совместимость | Этап 2: собственные сборки и репозиторий; OS Profile позволяет держать оба источника одновременно |
| MySQL 9.x удалит `mysql_native_password` | Остаёмся на 8.4 LTS до 2032; для legacy PHP — только 8.4; переход на 9.x — ручная процедура |
| Новый релиз ОС без пакетов вендоров | Поддержка объявляется только после прохождения матрицы; еженедельная проверка |
| Ручные правки конфигов администратором | Include-каталоги, `confhistory`, `mp doctor` показывает дрейф, `mp config apply --all` восстанавливает |
| Компрометация Web-слоя | api без привилегий; агент с allow-list; helper с setuid; секреты зашифрованы |
| Рост числа сайтов (сотни) | `pm=ondemand`, один master на версию, SQLite с WAL держит десятки тысяч сущностей; метрики — ролапы |
