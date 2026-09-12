# 02. Платформы, источники пакетов, PHP, СУБД

## 1. Поддерживаемые ОС (v1)

| Семейство | Дистрибутив | Версии | Примечания |
|---|---|---|---|
| Debian | Debian | 12 (bookworm), 13 (trixie) | Debian 11 не поддерживается (LTS завершён 08.2026) |
| Debian | Ubuntu | 22.04, 24.04, 26.04 LTS | Только LTS-релизы |
| RHEL | RHEL, AlmaLinux, Rocky Linux, Oracle Linux | 9.x, 10.x | EL10 требует x86-64-v3 (AlmaLinux 10 имеет сборку под v2); CentOS Stream не поддерживается (rolling); EL8 — не в v1 |

Архитектуры: x86_64 (v1), aarch64 (v1.x — все источники пакетов ниже имеют arm64).

Минимальные требования: 1 vCPU / 1 ГБ RAM / 10 ГБ диска (панель + nginx + одна версия PHP + СУБД с buffer pool 128 МБ). Рекомендуется 2 vCPU / 2 ГБ.

## 2. Источники пакетов

| Компонент | Debian / Ubuntu | EL9 / EL10 | Комментарий |
|---|---|---|---|
| nginx | nginx.org, репозиторий `nginx` (stable) | nginx.org `nginx-stable` | Одинаковый layout на всех ОС: `/etc/nginx/nginx.conf` + `conf.d/`, пользователь `nginx`. HTTP/3 (QUIC) в сборках nginx.org. Опция: Angie (форк с встроенным ACME и API) как drop-in |
| Apache 2.4 | дистрибутив (`apache2`) | дистрибутив (`httpd`) | Только `mpm_event` + `mod_proxy_fcgi`. `mod_php` не устанавливается |
| PHP (этап 1) | Debian: `packages.sury.org/php`; Ubuntu: `ppa:ondrej/php` | Remi (`remi-release-9` / `remi-release-10`), SCL-style пакеты `php{56..85}-php-*` | Параллельные версии из коробки. Оба репозитория держит по одному человеку — риск, поэтому этап 2 |
| PHP (этап 2) | собственный репозиторий | собственный репозиторий | Сборочная ферма в Docker, layout `/opt/monopanel/php/<X.Y>`, одинаковый на всех ОС |
| MySQL 8.4 LTS | MySQL APT repo (`mysql-apt-config`) | MySQL Yum repo (`mysql84-community-release-el9` / `-el10`) | Community Server 8.4.x |
| Percona Server 8.4 LTS | `percona-release setup ps-84-lts` | `percona-release setup ps-84-lts` | + Percona XtraBackup 8.4, Percona Toolkit |
| phpMyAdmin | собственный пакет из upstream-tarball | собственный пакет | Дистрибутивные версии отстают |
| restic | собственный пакет (upstream binary) | собственный пакет | Бэкапы: дедупликация, шифрование, local/SFTP/S3 |
| fail2ban | дистрибутив | EPEL | + фильтры панели |
| Прочее | `acl quota cron logrotate unzip nftables openssh-server ca-certificates` | `acl quota cronie logrotate unzip nftables openssh-server policycoreutils-python-utils` + EPEL | |

Вендоры добавляют новые релизы ОС с задержкой (MySQL и Percona для Ubuntu 26.04 и Debian 13 — проверить перед объявлением поддержки). Доступность пакетов по всей матрице проверяет еженедельный CI-job.

Проверено на [площадке](08-testbed.md) 2026-09-09: Percona 8.4 есть для всех девяти ОС матрицы, nginx.org — тоже. У `ppa:ondrej/php` ещё нет сборок для Ubuntu 26.04 (resolute): панель это видит (HEAD на `dists/<codename>/Release`), не подключает несуществующий источник, ставит PHP из самой Ubuntu (там только 8.5) и в списке веток честно помечает остальные недоступными с причиной; при следующей установке PPA проверяется снова. На EL пакеты Percona/MySQL стартуют сервер с временным паролем root в `/var/log/mysqld.log`, а не с `auth_socket` — панель читает его и переводит root на сокет сама. Oracle Linux (2026-09-10): EPEL включается пакетом `oracle-epel-release-el9`, а на OL10 — официальным `epel-release` из dl.fedoraproject.org, потому что пакет Oracle не предоставляет `epel-release = 10`, которого требует `remi-release-10`; образы Oracle идут с включённым firewalld.

## 3. PHP

### 3.1 Матрица версий (состояние на 09.2026)

| Версия | Статус upstream | Sury (Debian 12/13, Ubuntu 22.04–26.04) | Remi EL9 | Remi EL10 | Собственная сборка |
|---|---|---|---|---|---|
| 5.6 | EOL (2018) | да, урезанный набор расширений | да | нет / best-effort | да (OpenSSL 1.1 статически) |
| 7.0–7.3 | EOL | да | да | нет / best-effort | да (OpenSSL 1.1 статически) |
| 7.4 | EOL (2022) | да | да | да | да |
| 8.0 | EOL (2023) | да | да | да | да |
| 8.1 | security-фиксы завершены 12.2025 | да | да | да | да |
| 8.2 | security до 12.2026 | да | да | да | да |
| 8.3 | security до 12.2027 | да | да | да | да |
| 8.4 | active до 12.2026, security до 12.2028 | да | да | да | да |
| 8.5 | active до 12.2027, security до 12.2029 | да | да | да | да |
| 8.6 / 9.0 | ожидается 11.2026 | появится после релиза | появится | появится | добавить в ферму |

Наличие старых веток у Remi для EL10 нужно проверить по его актуальной таблице; для отсутствующих — только собственная сборка. В UI версии помечаются: «актуальная», «только security», «EOL — небезопасно». По умолчанию для нового сайта — последняя stable (8.5).

### 3.2 Стандартный набор расширений

Для каждой версии, где расширение собирается:

`opcache, mysqli, pdo_mysql, mbstring, intl, gd (webp/avif), imagick, curl, zip, xml, dom, simplexml, xmlreader, xmlwriter, soap, bcmath, gmp, exif, fileinfo, iconv, sockets, calendar, ctype, tokenizer, json, redis, memcached, apcu, igbinary, msgpack, xsl, ldap, imap (≤ 8.3 в ядре, 8.4+ через PECL), sodium (≥ 7.2), mcrypt (≤ 7.1), ioncube loader (опционально, 5.6–8.5), xdebug (только для dev-включения)`.

Composer — общий бинарник, запускается через `php` выбранной версии.

### 3.3 Layout собственных сборок (этап 2)

```
/opt/monopanel/php/8.4/{bin/php, bin/phpize, bin/php-config, sbin/php-fpm, lib/php/extensions/…}
/etc/monopanel/php/8.4/php.ini          # глобальный ini версии (шаблон панели)
/etc/monopanel/php/8.4/php-fpm.conf     # include pool.d/*.conf
/etc/monopanel/php/8.4/pool.d/<domain>.conf
/etc/monopanel/php/8.4/conf.d/*.ini     # включение расширений
/run/monopanel/php/<domain>.sock        # сокет пула
/var/log/monopanel/php/8.4-fpm.log
systemd: monopanel-php-fpm@8.4.service
  ExecStart=/opt/monopanel/php/%i/sbin/php-fpm -y /etc/monopanel/php/%i/php-fpm.conf --nodaemonize
  ExecReload=/bin/kill -USR2 $MAINPID
```

Сборка: Docker-образы под каждую целевую ОС (линковка с системными libc/ICU/libxml2), `nfpm` → deb/rpm, пакеты `monopanel-php-8.4`, `monopanel-php-8.4-imagick` и т.д. Для 5.6–7.3 статически линкуется OpenSSL 1.1.1 и, где нужно, старые libxml2/ICU — с явной пометкой «EOL, без security-обновлений». Для 7.4/8.0 — патчи совместимости с OpenSSL 3 (как у Sury/Remi).

Реализовано (этап 1): `internal/osprofile/php.go` описывает layout Sury/Remi (пакеты ядра и best-effort расширения по веткам, пути, unit), `mp php install <ver>` подключает репозиторий один раз (Sury-ключ в `/etc/apt/keyrings/`, PPA на Ubuntu, remi-release на EL), ставит пакеты, пишет `99-monopanel.ini` и включает `phpX.Y-fpm`. Поле `php_versions.source` в БД говорит, какой layout использовать. Обе схемы сосуществуют на одном сервере в переходный период; миграция сайта — смена `source` + перегенерация пула + reload обоих master.

### 3.4 Версия PHP для CLI

- Глобально: `/usr/bin/php` → `update-alternatives` / `alternatives` на версию по умолчанию.
- Для клиента: `/var/www/<user>/data/bin/php` → симлинк на версию его основного сайта; каталог добавляется в `PATH` через `/etc/profile.d/monopanel.sh`; cron-задачи панели запускаются с явным путём к выбранной версии.

## 4. СУБД

| | MySQL Community 8.4 LTS | Percona Server 8.4 LTS |
|---|---|---|
| Совместимость | эталон | drop-in, тот же протокол, формат файлов и клиенты |
| Плюсы | «официальный» MySQL | XtraBackup (горячий физический бэкап), расширенная диагностика (slow log, PFS), Percona Toolkit, thread pool, MyRocks |
| Рекомендация | по требованию | **по умолчанию** |

Общее для обоих (реализуется в провайдере `DBEngine`):

- Доступ панели: `root@localhost` через unix-сокет с плагином `auth_socket` — пароль root не хранится; запросы выполняет агент.
- Именование: БД `<user>_<name>`, пользователь `<user>_<name>@localhost`; доступ с `%` — только по флагу с автоправилом firewall на 3306 и `bind-address` на внешний IP.
- `character_set_server=utf8mb4`, `collation_server=utf8mb4_0900_ai_ci`.
- Шаблон `zz-monopanel.cnf` по объёму RAM: `innodb_buffer_pool_size` (25–50 %), `innodb_redo_log_capacity`, `max_connections`, `table_open_cache`, `tmp_table_size`; `bind-address=127.0.0.1`; `local_infile=OFF`; `innodb_strict_mode=OFF` (строгий режим отвергает таблицы с размером строки выше лимита InnoDB и дампы со старых серверов — на это натыкаются установщики CMS и переезды); `transaction_isolation=READ-COMMITTED` и `sql_mode=NO_ENGINE_SUBSTITUTION` (требование 1С-Битрикс, остальным CMS не мешают); `max_allowed_packet=64M`, `thread_cache_size=32`, `sort_buffer_size`/`join_buffer_size` 2M; `performance_schema=ON`; бинлог по умолчанию выключен (`disable_log_bin`) — на single-node он только занимает диск; включается флагом с `binlog_expire_logs_seconds=259200`.
- **Legacy PHP**: в 8.4 плагин `mysql_native_password` выключен по умолчанию (удалён в 9.0). PHP < 7.4 (mysqlnd) не поддерживает `caching_sha2_password`. При наличии сайтов на PHP ≤ 7.3 панель включает `mysql_native_password=ON` и создаёт пользователей таких сайтов с `IDENTIFIED WITH mysql_native_password`; остальные — `caching_sha2_password`. Поле `db_users.auth_plugin` хранит выбор.
- Бэкапы: логические — `mysqldump --single-transaction --routines --triggers --events` по БД или `mysqlsh util.dumpSchemas` (параллельно, быстрее на больших объёмах); физические (Percona) — XtraBackup 8.4 всего инстанса.
- Обновления в пределах 8.4.x — через панель; переход на 9.x не автоматизируется (отдельная процедура с проверкой legacy-аутентификации и пользователей).
- phpMyAdmin: SSO из панели (signon-auth), отдельный пул FPM `monopanel-pma` на новейшей установленной PHP, отдаётся панелью через FastCGI-клиент по адресу `https://host:8443/pma/` — не зависит от системного nginx.

## 5. OS Profile — различия ОС в одном месте

| Параметр | Debian / Ubuntu | EL9 / EL10 |
|---|---|---|
| Пакетный менеджер | apt/dpkg, `DEBIAN_FRONTEND=noninteractive` | dnf/rpm |
| Пользователь nginx | `nginx` (пакет nginx.org) | `nginx` |
| Пользователь Apache | `www-data` | `apache` |
| Apache: пакет / сервис / каталог | `apache2` / `apache2.service` / `/etc/apache2/` (`mods-enabled`, `conf-enabled`, `ports.conf`, `a2enmod`) | `httpd` / `httpd.service` / `/etc/httpd/` (`conf.modules.d/`, `conf.d/`) |
| Apache: проверка | `apache2ctl -t` | `apachectl -t` |
| PHP (этап 1) | `/etc/php/X.Y/fpm/{php.ini,pool.d/}`, бинарь `php-fpmX.Y`, сервис `phpX.Y-fpm.service`, CLI `phpX.Y` | `/etc/opt/remi/phpXY/{php.ini,php-fpm.d/}`, бинарь `/opt/remi/phpXY/root/usr/sbin/php-fpm`, сервис `phpXY-php-fpm.service`, CLI `phpXY` |
| PHP (этап 2) | `/opt/monopanel/php/X.Y` | `/opt/monopanel/php/X.Y` |
| Конфиг СУБД панели | `/etc/mysql/mysql.conf.d/zz-monopanel.cnf` (MySQL) / `/etc/mysql/conf.d/zz-monopanel.cnf` (Percona) | `/etc/my.cnf.d/zz-monopanel.cnf`; у Percona на EL `/etc/my.cnf` ничего не включает, и `mysqld` читает только `/etc/my.cnf`, `/etc/mysql/my.cnf`, `/usr/etc/my.cnf` — панель пишет `/etc/mysql/my.cnf` с `!includedir /etc/my.cnf.d/` (найдено площадкой 2026-09-10: до этого настройки панели на EL не применялись) |
| Сервис СУБД | `mysql.service` | `mysqld.service` |
| MAC | AppArmor (профили для nginx/php не поставляются, вмешательство не требуется) | SELinux enforcing — см. §6 |
| Firewall | nftables напрямую (ufw при наличии — отключить или сосуществовать по выбору) | firewalld (nftables-backend) — см. §7 |
| cron | `cron` | `cronie` |
| Квоты | `quota` (ext4: `usrquota`; xfs: `uquota`) | `quota` / `xfs_quota` |
| fail2ban | дистрибутив | EPEL |
| Shell клиентов | `/bin/bash`, `/usr/sbin/nologin` | `/bin/bash`, `/sbin/nologin` |
| Certificates bundle | `/etc/ssl/certs/ca-certificates.crt` | `/etc/pki/tls/certs/ca-bundle.crt` |

Интерфейс в коде:

```go
type Profile interface {
    Family() Family                   // Debian | RHEL
    Release() Release                 // id, version, codename, arch
    Packages() PackageManager         // AddRepo / Install / Remove / Upgrade / Query
    Web() WebLayout                   // пользователи nginx/apache, каталоги, сервисы, команды валидации
    PHP(source string) PHPLayout      // sury | remi | monopanel
    DB(engine string) DBLayout        // mysql | percona
    MAC() MACHandler                  // selinux | apparmor | none
    Firewall() FirewallHandler        // nft | firewalld
    Cron() CronHandler
    Quota() QuotaHandler
}
```

## 6. SELinux (EL9 / EL10)

Панель работает в режиме **enforcing**; отключение SELinux — вне политики проекта.

Сделано (2026-09-09, проверено на площадке на AlmaLinux 9/10 и Rocky 9/10): при первой установке nginx или PHP панель один раз готовит хост под хостинг — ставит `policycoreutils-python-utils`, добавляет `fcontext` для сокетов FPM (`/var/run/monopanel(/.*)?` → `httpd_var_run_t`; semanage сам подсказывает написание при правиле эквивалентности `/run` ↔ `/var/run`, панель следует подсказке) и для логов сайтов (`/var/www/[^/]+/data/logs(/.*)?` → `httpd_log_t`), включает булевы `httpd_unified`, `httpd_can_network_connect`, `httpd_can_network_connect_db`, `httpd_can_sendmail`, `httpd_execmem`, `httpd_setrlimit` и делает `restorecon` по `/var/www`, `/run/monopanel`, `/etc/nginx`, `/var/log/nginx`. Агент после каждой записи конфигов и создания каталогов восстанавливает метки сам, а в наборе конфигов есть поле `restore`: `nginx -t`, которым агент проверяет конфигурацию, создаёт `/run/nginx.pid` с меткой агента (`var_run_t`), и nginx в домене `httpd_t` не мог его открыть — перед запуском файл перемечается. Панель сама пока unconfined (`unconfined_service_t`).

Планируемый пакет `monopanel-selinux` с модулем политики и остальными `fcontext`-правилами:

- Docroot `/var/www(/.*)?` → `httpd_sys_content_t` (уже в базовой политике). Каталоги для записи (`uploads`, `cache`, `tmp`, `logs`) → `httpd_sys_rw_content_t` через `semanage fcontext` + `restorecon` при создании сайта; пользователь может пометить произвольный каталог как «записываемый» из UI.
- Сокеты FPM `/run/monopanel/php(/.*)?` → `httpd_var_run_t`.
- Собственные сборки PHP: `/opt/monopanel/php/[^/]+/sbin/php-fpm` → `httpd_exec_t`, библиотеки → `lib_t`, конфиги → `httpd_config_t`, логи → `httpd_log_t`.
- Apache на `127.0.0.1:8080` — порт уже в `http_port_t`; нестандартные порты — `semanage port -a -t http_port_t -p tcp <port>`.
- Булевы: `httpd_can_network_connect_db=1`, `httpd_can_sendmail=1`; `httpd_can_network_connect=1` — только если хотя бы один сайт делает исходящие HTTP-запросы (флаг сайта, по умолчанию включён, т.к. это почти любой CMS); `httpd_execmem=1` — только при включении ionCube/JIT.
- Панель (`monopaneld`) — в первом релизе unconfined, в v1.0 — собственный домен `monopanel_t` в поставляемом модуле.
- `mp doctor` показывает AVC-denials из `ausearch` за последние сутки с подсказкой по исправлению.

### 6.1 Жить с enforcing

Панель держит SELinux в enforcing и рассчитана на это: политика выше проверена полной матрицей, включая установку Битрикса, Sphinx 3 и memcached. Единственный источник тикетов — метки файлов, которые попали в docroot в обход панели: `cp -a` и `rsync -X` из `/root` сохраняют `admin_home_t`, и nginx отвечает 403, неотличимым от ошибки прав. Что для этого есть:

- `mp doctor` показывает режим SELinux и число отказов AVC для веб-домена за сегодня (nginx и php-fpm оба работают как `httpd_t`) с последним отказом и подсказкой `mp site fix <домен>`, если путь лежит в каталоге сайта; в вебе на дашборде рядом с этой строкой кнопка «починить <домен>» (или «починить все сайты», когда в записи только имя файла, а не путь), а у permissive — «вернуть enforcing». Агент читает audit через `ausearch`.
- `mp site fix <домен>` восстанавливает каталоги и ACL как при создании, делает клиента владельцем всего дерева сайта и делает `restorecon -R` по `data/`. Те же метки панель восстанавливает сама после переноса с другой панели и после распаковки архива в файловом менеджере.
- Файлы, которые создаёт сам агент, метятся сразу (`restorecon` после записи); файлы, загруженные через SFTP или файловый менеджер, наследуют метку каталога и в починке не нуждаются.

Что SELinux даёт при наших булевых переключателях (`httpd_unified`, `httpd_can_network_connect`, `httpd_can_sendmail`, `httpd_execmem`): взломанный сайт не прочитает `/etc/shadow`, `/root`, чужие домашние каталоги, данные почты и базы, а postfix, dovecot, mysqld и fail2ban остаются в своих доменах. Сайты друг от друга SELinux не изолирует — это делают отдельные пользователи, пулы php-fpm, ACL и open_basedir.

Если всё же нужен permissive (например, стороннее ПО без политики): `mp selinux permissive` или кнопка в «Настройках». Панель сначала показывает, чего это стоит, и просит подтверждения (`--yes` в скриптах), затем делает `setenforce 0` и пишет `SELINUX=permissive` в `/etc/selinux/config`, чтобы режим пережил перезагрузку. Отказы дальше только записываются в audit; `mp doctor` и страница настроек предупреждают, пока режим не вернут: `mp selinux enforcing`. `SELINUX=disabled` панель не предлагает: на EL9 и EL10 он не действует без параметра ядра `selinux=0` и перезагрузки, а панели он не нужен.

## 7. Firewall и защита от перебора

- Собственная таблица `inet monopanel` в nftables (через `google/nftables`, без парсинга текстового вывода): цепочка `input` с правилами панели (ssh, 80/443, 8443, 3306 при удалённом доступе, ftp по флагу), rate-limit на ssh и 8443, чёрный/белый списки из UI, geo-блокировки (по спискам). Правила сохраняются в `/etc/nftables.d/monopanel.nft` для восстановления при загрузке.
- EL: firewalld не удаляется. Пока панель им не управляет: если он запущен (образы Oracle Linux поставляются с ним, Alma и Rocky — нет), `mp setup` открывает в нём порт панели, установка nginx — 80 и 443, установка почты — её порты (`firewall-cmd --permanent` + `--reload`); `mp firewall enable` останавливает и выключает firewalld, дальше таблицу ведёт панель. Управление зоной через D-Bus и выбор при `mp setup` — позже. Ubuntu: ufw аналогично.
- fail2ban: jail'ы `sshd`, `monopanel` (лог панели), `nginx-http-auth`, `nginx-botsearch`, `mysqld-auth`, позже `proftpd`/`postfix`; действие — `nftables-multiport` в таблице панели, чтобы баны были видны в UI.

## 8. Sphinx для 1С-Битрикс

Расширение `sphinx` ставит поисковый сервер, который Битрикс ждёт в «Настройки → Поиск → Sphinx» (соединение `127.0.0.1:9306`, индекс `bitrix`). Что выяснилось на стенде:

- **Debian/Ubuntu**: пакет `sphinxsearch` (2.2.11) из дистрибутива, конфиг по эталону Битрикса «for 2.X» (`rt_field`/`rt_attr_*`), `/etc/default/sphinxsearch` с `START=yes`. Юнит сгенерирован из SysV-скрипта: `systemctl enable` его не принимает («generated»), панель это игнорирует — rc-ссылки пакета и так запускают демон.
- **EL**: пакета нет. Manticore не подходит: его `SHOW TABLES` отдаёт колонку `Table`, а `search/tools/sphinx.php` читает `$res['Index']` — Битрикс отвечает «Указанный индекс не найден». Ставится сборка Sphinx 3.9.1 с sphinxsearch.com (tar.gz, контрольная сумма зашита в панель), пользователь `sphinx`, `/opt/monopanel/sphinx`, юнит `monopanel-sphinx.service`, конфиг `/etc/sphinx/sphinx.conf`.
- **Sphinx 3 молча игнорирует написание 2.x** (`rt_attr_timestamp` и остальные `rt_attr_*`): индекс поднимался без `date_change`/`date_to`/`date_from`. Конфиг для 3.x повторяет эталон Битрикса «for 3.X»: `field`, `attr_uint`, `attr_string`, `attr_uint_set`, даты как `attr_uint` (Битрикс принимает `uint` и `timestamp`).
- **Существующий индекс на диске главнее конфига**: searchd пишет `attribute count mismatch … EXISTING INDEX TAKES PRECEDENCE` и оставляет старую схему. Поэтому после установки панель делает `DESCRIBE bitrix`, и если не хватает колонок из списка Битрикса — останавливает демон, удаляет `bitrix.*` и binlog из каталога данных и запускает заново (Битрикс наполнит индекс переиндексацией). Удаление расширения тоже убирает файлы индекса.
- `systemctl restart` возвращается раньше, чем searchd начинает слушать порты: проверка `SHOW TABLES` повторяется до 15 с, пока клиент отвечает «Can't connect».
- Символ `_` нельзя ставить одновременно в `charset_table` и `blend_chars` — индекс не поднимается (NOT SERVING).
- sphinxsearch.com отдаёт архив (40 МБ) медленно, до полутора минут: загрузки панели ограничены не общим таймаутом, а простоем (минута без данных).

