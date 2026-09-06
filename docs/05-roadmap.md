# 05. Дорожная карта, матрица тестирования, CI

## Этап 0 — каркас (выполнен 2026-09-05, проверен на Ubuntu 24.04)

- [x] Репозиторий, сборка Go (`make build`), `nfpm.yaml`, units, sysusers/tmpfiles, `install.sh`, `mp setup`.
- [x] OS Profile для Debian/Ubuntu и EL9/EL10 (+ generic для dev-машин); агент с `ApplyConfigSet`, `EnsureGroup`, `EnsureUnixUser`, `EnsureDirs`, `Service`, `Pkg`; peer-cred на обоих сокетах.
- [x] SQLite-схема и миграции; auth (argon2id, сессии, Bearer-токены, CSRF-проверка); job-runner с локами; SSE.
- [x] Установка nginx с nginx.org (`mp stack install nginx`), шаблоны nginx/Apache/php-fpm с golden-тестами, CLI и TUI.
- [x] ACME (lego, HTTP-01 по webroot): `mp ssl issue/list/renew/rm`, таблица `certificates`, автопродление, hot-swap сертификата панели (`mp web tls`). Проверено выпуском боевого сертификата Let's Encrypt на тестовом хосте.
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

### PHP 8.5 (2026-09-06)
- [x] Установка 8.5 падала: панель просила `php8.5-opcache`, которого в Sury нет — начиная с 8.5 opcache собран в ядро (`php8.5 -m` показывает Zend OPcache, `opcache.enable => On`). Отдельный пакет существует только с 7.0 по 8.4; правило и тест на обе границы — в `internal/osprofile/php.go`.

### Адаптация Web UI под телефоны и планшеты (2026-09-06)
- [x] Каркас: боковое меню шириной 240px ниже `lg` превращается в выезжающую панель с затемнением и шапкой с кнопкой; выше — прежняя колонка. Переход по пункту закрывает меню, цели нажатия увеличены.
- [x] Таблицы: приём один на все девять страниц, потому что все используют класс `.tbl`. Ниже 40rem строка становится карточкой, заголовки колонок уходят в подписи слева (`data-label` на ячейках), таблица перестаёт быть таблицей (`display: block`) — иначе она считает ширину по содержимому и длинные значения уезжают за край. Ячейки без подписи — кнопки действий — держатся справа.
- [x] Мелочи: тост во всю ширину на телефоне, перенос длинного docroot, панель файлового менеджера в две строки, отступы страницы меньше на узком экране.
- [x] Проверено на 375×812 и 768×1024 по всем страницам: горизонтального выхода за экран нет нигде, включая редактор Monaco и карточку сайта с вкладками.

### Файловый менеджер в Web UI (2026-09-06)
- [x] `FileManager.svelte`: обзор домашнего каталога аккаунта, хлебные крошки, переход к каталогу сайта, создание папки и файла, загрузка перетаскиванием и выбором, скачивание, переименование, права, распаковка архивов, множественное выделение и удаление — всё поверх существующего `/files`-API, то есть от имени владельца через helper.
- [x] Редактор текстовых файлов: нумерация строк, Ctrl+S, Tab, защита от ухода с несохранёнными правками. Двоичные файлы (по расширению или нулевому байту) и файлы больше 1 МБ открываются только на скачивание. Без подсветки синтаксиса — она означала бы новую npm-зависимость.
- [x] Страница `/files` (с выбором аккаунта для администратора) и вкладка «Файлы» в карточке сайта, открывающаяся в его docroot.
- [x] Редактор — Monaco, то же ядро, что в VS Code (`web/static/monaco`, MIT): подсветка php/html/css/js/ts/json/xml/ini/shell/sql/yaml/markdown/python/dockerfile, поиск и замена, мультикурсор, свёртка, палитра команд, тема следует за темой панели, высота — по содержимому. Сборка урезана с 24 МБ до 4,7: без языковых служб (IntelliSense-воркеры json/css/html/ts), переводов интерфейса и лишних режимов; бинарник вырос с 26 до 31 МБ. Грузится лениво, при сбое загрузки остаётся простое поле. Вендорится в репозиторий, а не тянется из npm или CDN: панель должна ставиться на сервер без интернета, а node-инструментов в репозитории нет. В CSP добавлен `font-src data:` — шрифт иконок вшит в CSS Monaco.
- [x] API: операция `touch` (`fsop touch` с `O_EXCL`) — пустое тело PUT huma не принимает, а создание файла не должно затирать существующий; `force` опустошает файл. Тесты `internal/cli/fsop_test.go` на touch и на то, что «..» упирается в домашний каталог.

### Обновление панели из релизов (2026-09-06)
- [x] `internal/updater`: релизы GitHub (или GitHub Enterprise через `api`), выбор по версии, а не по дате публикации; канал stable/beta; имена артефактов фиксированы (`monopanel_<v>_<arch>.deb`, `monopanel-<v>.<arch>.rpm`, `monopanel-linux-<arch>`, `SHA256SUMS`, `SHA256SUMS.sig`).
- [x] Подпись релиза ed25519: `scripts/release keygen|sign|verify`, приватный ключ — секрет репозитория, публичный — `update.public_key` в `config.yaml` (root-only, менять из панели нельзя). При заданном ключе неподписанный релиз не устанавливается; агент проверяет подпись повторно, а не доверяет хешу от API-процесса.
- [x] Установка вне панели: агент `POST /v1/panel/install` запускает transient-юнит `monopanel-update.service` (`StartTransientUnit`), который переживает перезапуск API и агента; `mp update-run` ставит пакет, перезапускает юниты, ждёт `/health` с новой версией и откатывает прежний бинарник, если она не отвечает. Итог пишется в `<data>/updates/state.json` и попадает в audit после перезапуска.
- [x] API `GET|PUT /system/update`, `POST /system/update/check|apply` (только администратор), задача `panel.update`, ежедневная проверка по расписанию и `auto_apply`; токен репозитория шифруется секрет-боксом.
- [x] `mp update`, `mp update check|apply|settings|trust`; карточка «Обновление панели» в настройках Web UI ждёт перезапуск и перезагружает страницу; проверка `update` в `mp doctor`.
- [x] Пакеты: `make packages` (deb+rpm × amd64+arm64 + бинарники + SHA256SUMS), `make release VERSION=…`, workflow `release.yml` по тегу `v*`; `preremove` больше не выключает панель при обновлении, `postinstall` перезапускает юниты только на апгрейде.

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
