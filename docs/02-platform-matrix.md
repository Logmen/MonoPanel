# 02. Платформы, источники пакетов, PHP, СУБД

## 1. Поддерживаемые ОС

| Семейство | Дистрибутив | Версии | Примечания |
|---|---|---|---|
| Debian | Debian | 12 (bookworm), 13 (trixie) | Debian 11 не поддерживается (LTS завершён 08.2026) |
| Debian | Ubuntu | 22.04, 24.04, 26.04 LTS | Только LTS-релизы |
| RHEL | RHEL, AlmaLinux, Rocky Linux, Oracle Linux | 9.x, 10.x | EL10 требует x86-64-v3 (у AlmaLinux 10 есть сборка под v2); CentOS Stream не поддерживается (rolling); EL8 не поддерживается |

Архитектуры: amd64 и arm64 — пакеты собираются для обеих, у всех источников ниже есть сборки под arm64; матрица на площадке гоняется на amd64.

Ядро: песочнице restic нужен Landlock (Linux 5.13+, `landlock` в списке `/sys/kernel/security/lsm`); есть ли он на хосте, показывает строка «restic sandbox» в `mp doctor`. Без него restic работает без песочницы, кроме представления без символических ссылок для бэкапа, а `mp doctor` предупреждает. На Oracle Linux с ядром UEK Landlock собран, но не включён (`mp doctor`: «Landlock is built into this kernel but not enabled»): включить — `grubby --update-kernel=ALL --args=lsm=$(cat /sys/kernel/security/lsm),landlock` и перезагрузка, или ядро RHCK (`dnf install kernel`, `grubby --set-default`). До тех пор бэкап защищён только представлением без символических ссылок. Для этого представления сохраняемых деревьев агенту нужно монтирование (своё пространство имён монтирования и `mount_setattr` с `nosymfollow`, Linux 5.14), собственному `/etc` restic (tmpfs) — тоже; в контейнере, профиль которого запрещает монтирование, `mp doctor` предупреждает, в том числе что замена `resolv.conf` хостом во время бэкапа оставит restic без DNS. Подробно — [01 §7](01-architecture.md#7-безопасность). На AlmaLinux, Rocky Linux и Oracle Linux restic из EPEL собран Go от Red Hat и при старте читает `/proc/sys/crypto/fips_enabled`; песочница даёт ему этот файл, и больше ничего из `/proc`. Строка «restic sandbox» называет версию restic, которая запустилась в песочнице, ошибку restic, если не запустилась (сбой), или что проверка не уложилась в 30 с (предупреждение). Общие папки ([10](10-shared-folders.md)) агент монтирует через `open_tree`, `move_mount` и `mount_setattr`, а монтирование каталогов на пути к папке узнаёт через `statx` (`STATX_MNT_ID`) (Linux 5.12+; на всех ОС из таблицы ядро 5.14 и новее). В контейнере (LXC, OpenVZ, Docker; агент узнаёт это у `systemd-detect-virt --container`, из `/run/systemd/container` или `/proc/vz`) модули грузит ядро хоста по конфигурации modprobe хоста, и файл панели в контейнере ничего не меняет: строка «kernel modules» в `mp doctor` предупреждает об этом, а `install sctp /bin/false` (и то же для dccp, rds, tipc) нужно положить в `/etc/modprobe.d` хоста. Тип контейнера `GET /system/status` отдаёт в `host.container`.

Минимальные требования: 1 vCPU / 1 ГБ RAM / 10 ГБ диска (панель + nginx + одна версия PHP + СУБД с buffer pool 128 МБ). Рекомендуется 2 vCPU / 2 ГБ.

## 2. Источники пакетов

| Компонент | Debian / Ubuntu | EL9 / EL10 | Комментарий |
|---|---|---|---|
| nginx | nginx.org, репозиторий `nginx` (stable) | nginx.org `nginx-stable` | Одинаковая раскладка на всех ОС: `/etc/nginx/nginx.conf` + `conf.d/`, пользователь `nginx`. HTTP/3 (QUIC) в сборках nginx.org. Возможная замена — Angie (форк со встроенными ACME и API), встаёт на то же место |
| Apache 2.4 | дистрибутив (`apache2`) | — | Только Debian/Ubuntu: свой экземпляр на аккаунт (`monopanel-apache@<логин>`), `mpm_event` + `mod_proxy_fcgi`; `mod_php` не устанавливается. На EL режима apache нет |
| PHP (этап 1) | Debian и Ubuntu: `packages.sury.org/php` (релиз Ubuntu, которого Sury ещё не публикует, — PHP самой Ubuntu) | Remi (`remi-release-9` / `remi-release-10`), SCL-style пакеты `php{56..85}-php-*` | Параллельные версии из коробки. Каждый из этих репозиториев держит один человек — это риск, отсюда этап 2 |
| PHP (этап 2, планируется) | собственный репозиторий | собственный репозиторий | Сборочная ферма, раскладка `/opt/monopanel/php/<X.Y>`, одинаковая на всех ОС |
| MySQL 8.4 LTS | MySQL APT repo (`mysql-apt-config`) | MySQL Yum repo (`mysql84-community-release-el9` / `-el10`) | Community Server 8.4.x |
| Percona Server 8.4 LTS | `percona-release setup ps-84-lts` | `percona-release setup ps-84-lts` | + Percona XtraBackup 8.4, Percona Toolkit |
| phpMyAdmin 5.2.3 | upstream-tarball (sha256 закреплён в панели) | то же | Дистрибутивные версии отстают |
| restic | дистрибутив | EPEL | Бэкапы: дедупликация, шифрование, local/SFTP/S3/B2/REST |
| fail2ban | дистрибутив | EPEL | + фильтры панели |
| Valkey | дистрибутив: `valkey-server` (Debian 13, Ubuntu 24.04/26.04; Debian 12 — backports), иначе `redis-server` (Ubuntu 22.04 — 6.0, Debian 12 без backports — 7.0) | AppStream: `valkey` (8.0) | Экземпляры на аккаунт, общий экземпляр пакета выключен, см. §9 |
| Прочее | `acl quota cron logrotate unzip nftables openssh-server ca-certificates` | `acl quota cronie logrotate unzip nftables openssh-server policycoreutils-python-utils` + EPEL | |

Обновления: то, что панель поставила из репозиториев вендоров (nginx.org, Sury, Remi, Percona, MySQL), и Apache она обновляет сама — `mp stack upgrade`, а nginx, PHP и Apache ещё и ежедневно ([01 §9](01-architecture.md#9-упаковка-установка-обновление)); `mp doctor` называет каждый компонент, у которого ждёт обновление. Пакеты самого дистрибутива (restic, fail2ban, Valkey, memcached, почта) остаются на обновлениях дистрибутива: unattended-upgrades на Debian/Ubuntu (источники вендоров он не берёт); на EL по умолчанию ничего — рекомендуется dnf-automatic. Агент запускает apt с `NEEDRESTART_MODE=l` (так же ставят пакет панели самообновление и `install.sh`), и needrestart, где он стоит (на Ubuntu), только перечисляет службы на старых библиотеках и сам ничего не перезапускает, а панель перезапускает только службы того, что обновляет (nginx, php-fpm, Apache аккаунтов, сервер БД), после проверки конфигурации; службы стека, которые ещё работают на заменённом бинарнике, называет `mp doctor`.

Разработчики компонентов добавляют новые релизы ОС с задержкой, поэтому поддержка новой ОС объявляется только после прогона [площадки](08-testbed.md); еженедельная проверка доступности пакетов в CI — планируется.

Что показала площадка:

- Percona 8.4 и nginx.org есть для всех ОС матрицы (проверено 2026-09-09, Oracle Linux добавлен 2026-09-10).
- PHP на Ubuntu — из `packages.sury.org/php`, как на Debian. Сопровождающий пометил `ppa:ondrej/php` устаревшим («PPA for PHP (deprecated)», ноябрь 2026), набор resolute в нём пуст, а Sury публикует jammy, noble и resolute (проверено 2026-11-22: подпись ключом `15058500A0235D97F5D10063B188E2B695BD4743`, rsa3072 до 2028-02-04, тем же, что для Debian; plucky и questing — нет). Перед подключением на Ubuntu панель проверяет `dists/<codename>/Release` Sury; нет релиза — ставит PHP из самой Ubuntu (на 26.04 там только 8.5), помечает недоступные ветки с причиной и при следующей установке проверяет снова. Если после записи источника PHP apt не может прочитать индекс — ключ не принят, репозиторий пропал, — панель удаляет только что записанный источник: оставленный, он ломал бы `apt-get update` всему остальному, установке почты и nginx тоже. Следующая установка PHP записывает его снова. Так же, если `apt-get update` закончился успешно, а индекса Sury у apt нет (сбой сети, который apt считает временным, — только предупреждение, источник пропущен): установка падает с этим предупреждением, источник удаляется, репозиторий не считается готовым. Если apt уже читает Sury из источника, который на сервере есть, — добавленного администратором (по README Sury — `/etc/apt/sources.list.d/php.list` с ключом `/usr/share/keyrings/deb.sury.org-php.gpg`) или с зеркала Sury (сборки `+debsuryorg` с другого адреса), — панель своего источника не пишет, а перед установкой обновляет индекс: два источника одного репозитория с разными Signed-By apt отвергает вместе со всеми остальными («Conflicting values set for option Signed-By»). Если apt источник администратора ещё не читал, установка PHP падает с этой ошибкой apt и советом: после `apt-get update` следующая установка его оставляет. Источник по старому README Sury — без signed-by, ключ в `/etc/apt/trusted.gpg.d` — apt принимает рядом с панельным, если читает его раньше (`php.list`), и лишь предупреждает при каждом обновлении («configured multiple times»); прочитанный позже, он конфликтует на apt 2.x и не конфликтует на apt 3. Свой `sury-php.list`, оставшийся с прошлого раза (панель поставлена заново), панель пишет заново вместе с ключом.
- Сервер, которому панель до 0.8.19 подключила PPA (`ondrej-php.list`, ключ `ondrej-php.asc`), переходит на Sury при старте API, последним шагом стартовой работы: ключ и источник Sury кладутся рядом с PPA, индекс читается с обоими, и PPA удаляется, только если `apt-cache policy php-common` после этого называет packages.sury.org с кодовым именем релиза, а apt в этом обновлении индекса прочитал Sury и ничего против него не сказал. Исход самого `apt-get update` ничего не решает: он падает из-за любого источника (в том числе самого PPA) и кончается успешно, когда Sury пропущен из-за временного сбоя сети (это только предупреждение); индекс Sury, оставшийся от прошлого раза, не в счёт — и тогда, когда обновление не прошло вовсе (блокировка списков дольше десяти минут). Затем индекс обновляется ещё раз, уже без PPA. Не вышло — файлы Sury удаляются, PPA остаётся, причина — в `mp doctor` (`php repository`, настройка `php.repo.switch`) и журнале API, попытка — при каждом старте. Если apt уже читает Sury из источника сервера (администратора или зеркала), панель обновляет индекс ещё с PPA и удаляет только PPA, если apt прочитал Sury и ничего против него не сказал; свой источник не пишет. Если такой источник есть, но apt его ещё не читал, запись панели упирается в «Conflicting values set for option Signed-By»: файлы Sury панели удаляются, PPA остаётся, а `mp doctor` советует `apt-get update` (при следующем старте API источник администратора остаётся, удаляется только PPA) или удалить тот источник (тогда панель поставит свой). Источник PPA, который добавил сам администратор (`add-apt-repository`: `ondrej-ubuntu-php-<релиз>.sources`/`.list`), панель не трогает. По нынешнему адресу PPA (`ppa.launchpadcontent.net`) такой источник рядом с `ondrej-php.list` панели делает apt неработоспособным уже с 0.8.19 (у источника администратора другой Signed-By или никакого — так его пишет `add-apt-repository` на Ubuntu 22.04, ключ в `/etc/apt/trusted.gpg.d`, — и apt не читает ни одного источника): переезд не проходит, пока один из двух не удалён, и `mp doctor` говорит это — удалите свой, и следующий старт API переведёт PHP на Sury, или удалите `ondrej-php.list` и `ondrej-php.asc` панели, и источником PHP останется ваш PPA. Источник по старому адресу PPA (`ppa.launchpad.net`) apt принимает рядом, и переезд идёт как обычно. Агент PPA больше не записывает (config/apply отклоняет его источник и ключ), а удалить их два файла даёт (paths/remove). Пакеты не переустанавливаются: сборки Sury новее (`8.4.28-3+debsuryorg24.04.1+1` против `8.4.28-2+ubuntu24.04.1+deb.sury.org+1`, `php-common` `2:102+…` против `2:101~+…`), их ставит ближайшее обновление стека. Всё, что ставит панель, у Sury есть для всех веток и всех трёх релизов (кроме `phpX.Y-sodium` с 7.3: sodium встроен в `phpX.Y-common`, у PPA этих пакетов тоже нет); у PPA сверх того были `php-pear`, `php-imap`, `php-pspell`, сборки swoole/tideways и на jammy свои `libpcre2`/`libmemcached` — их панель не ставит, установленные остаются, сборки Sury с ними совместимы.
- На EL пакеты Percona/MySQL стартуют сервер с временным паролем root в `/var/log/mysqld.log`, а не с `auth_socket`; панель читает пароль из лога и переводит root на сокет сама.
- Oracle Linux: EPEL включается пакетом `oracle-epel-release-el9`, а на OL10 — официальным `epel-release` с dl.fedoraproject.org, потому что пакет Oracle не предоставляет `epel-release = 10`, которого требует `remi-release-10`. Образы Oracle идут с включённым firewalld.

## 3. PHP

### 3.1 Матрица версий (состояние на 09.2026)

| Версия | Статус у разработчиков PHP | Sury (Debian 12/13, Ubuntu 22.04–26.04) | Remi EL9 | Remi EL10 | Собственная сборка (план) |
|---|---|---|---|---|---|
| 5.6 | EOL (2018) | да, урезанный набор расширений | да | нет или не все | да (OpenSSL 1.1 статически) |
| 7.0–7.3 | EOL | да | да | нет или не все | да (OpenSSL 1.1 статически) |
| 7.4 | EOL (2022) | да | да | да | да |
| 8.0 | EOL (2023) | да | да | да | да |
| 8.1 | security-фиксы завершены 12.2025 | да | да | да | да |
| 8.2 | security до 12.2026 | да | да | да | да |
| 8.3 | security до 12.2027 | да | да | да | да |
| 8.4 | active до 12.2026, security до 12.2028 | да | да | да | да |
| 8.5 | active до 12.2027, security до 12.2029 | да | да | да | да |
| 8.6 / 9.0 | ожидается 11.2026 | появится после релиза | появится | появится | добавить в ферму |

Какие старые ветки есть у Remi для EL10, нужно сверять с его актуальной таблицей; для отсутствующих остаётся только собственная сборка. В UI версии помечаются: «актуальная», «только security», «EOL — небезопасно». По умолчанию для нового сайта — последняя stable (8.5).

### 3.2 Стандартный набор расширений

Для каждой версии, где расширение собирается:

`opcache, mysqli, pdo_mysql, mbstring, intl, gd (webp/avif), imagick, curl, zip, xml, dom, simplexml, xmlreader, xmlwriter, soap, bcmath, gmp, exif, fileinfo, iconv, sockets, calendar, ctype, tokenizer, json, redis, memcached, apcu, igbinary, msgpack, xsl, ldap, imap (≤ 8.3 в ядре, 8.4+ через PECL), sodium (≥ 7.2), mcrypt (≤ 7.1), ioncube loader (опционально, 5.6–8.5), xdebug (только для dev-включения)`.

Composer — общий бинарник, запускается через `php` выбранной версии.

### 3.3 Раскладка собственных сборок (этап 2, планируется)

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

Реализовано (этап 1): `internal/osprofile/php.go` описывает раскладку Sury/Remi (пакеты ядра и расширения, которые есть не у всех веток, пути, юнит), `mp php install <ver>` подключает репозиторий один раз (ключ Sury в `/etc/apt/keyrings/` на Debian и Ubuntu, remi-release на EL), ставит пакеты, пишет `99-monopanel.ini` и включает `phpX.Y-fpm`. Поле `php_versions.source` в БД говорит, какую раскладку использовать. В переходный период обе схемы будут жить на одном сервере; перевод сайта — смена `source`, перегенерация пула и reload обоих мастеров. Необязательные расширения ветки (imagick, redis, apcu, memcached и другие — у старых веток не все) ставятся одним запуском apt или dnf: каждый запуск с пакетами ветки перезапускает её php-fpm (на Debian — триггер пакета fpm в конце запуска), и при одном пакете за запуск php-fpm перезапускался чаще, чем позволяет лимит запусков systemd (5 за 10 с), — systemd отклонял reload самой панели, и установка падала (площадка, Debian 12). Панель один раз спрашивает репозитории, какие пакеты у них есть, и ставит их вместе; если этот запуск не прошёл, ставит по одному — сломанный пакет не мешает остальным — и пишет это в журнал задачи. Имена, которых в репозиториях нет как отдельных пакетов, идут ещё одним общим запуском: их может давать другой пакет (у собственного PHP Ubuntu `php8.3-imagick` даёт пакет `php-imagick`, в `php8.4-xml` Debian входит `php8.4-xsl`). Расширение, которое уже есть в установленном пакете ветки, пропуском не считается; остальные журнал называет строкой «not available for this branch, skipped».

### 3.4 Версия PHP для CLI

- Глобально: `/usr/bin/php` → `update-alternatives` / `alternatives` на версию по умолчанию.
- Для клиента: `/var/www/<user>/data/bin/php` → симлинк на версию его основного сайта; каталог добавляется в `PATH` через `/etc/profile.d/monopanel.sh`; cron-задачи панели запускаются с явным путём к выбранной версии.

## 4. СУБД

| | MySQL Community 8.4 LTS | Percona Server 8.4 LTS |
|---|---|---|
| Совместимость | эталон | полная замена: тот же протокол, формат файлов и клиенты |
| Плюсы | «официальный» MySQL | XtraBackup (горячий физический бэкап), расширенная диагностика (slow log, PFS), Percona Toolkit, thread pool, MyRocks |
| Рекомендация | по желанию | **по умолчанию** |

Общее для обоих (провайдер `DBEngine` в коде):

- Доступ панели: `root@localhost` через unix-сокет с плагином `auth_socket` — пароль root не хранится; запросы выполняет агент.
- Именование: БД `<user>_<name>`, пользователь `<user>_<name>@localhost`. Доступ снаружи (пользователь с хостом `%`, правило firewall на 3306 и `bind-address` на внешний IP) — планируется.
- `character_set_server=utf8mb4`, `collation_server=utf8mb4_0900_ai_ci`.
- Шаблон `zz-monopanel.cnf` по объёму RAM: `innodb_buffer_pool_size` (25–50 %), `innodb_redo_log_capacity`, `max_connections`, `table_open_cache`, `tmp_table_size`; `bind-address=127.0.0.1`; `local_infile=OFF`; `innodb_strict_mode=OFF` (строгий режим отвергает таблицы с размером строки выше лимита InnoDB и дампы со старых серверов — на это натыкаются установщики CMS и переезды); `transaction_isolation=READ-COMMITTED` и `sql_mode=NO_ENGINE_SUBSTITUTION` (требование 1С-Битрикс, остальным CMS не мешают); `max_allowed_packet=64M`, `thread_cache_size=32`, `sort_buffer_size`/`join_buffer_size` 2M; `performance_schema=ON`; бинлог по умолчанию выключен (`disable_log_bin`) — на single-node он только занимает диск; включается флагом с `binlog_expire_logs_seconds=259200`.
- **Старые версии PHP**: в 8.4 плагин `mysql_native_password` выключен по умолчанию (удалён в 9.0). PHP < 7.4 (mysqlnd) не поддерживает `caching_sha2_password`. При наличии сайтов на PHP ≤ 7.3 панель включает `mysql_native_password=ON` и `authentication_policy=mysql_native_password,,` и создаёт пользователей таких сайтов с `IDENTIFIED WITH mysql_native_password`; остальные — `caching_sha2_password`. Одного плагина мало: без `authentication_policy` сервер в рукопожатии предлагает `caching_sha2_password`, и mysqlnd PHP 5.6/7.0 рвёт соединение ещё до смены метода — даже у аккаунта с `mysql_native_password`. После обновления панели конфиг таких серверов перерисовывается при старте API (MySQL перезапускается, только если файл изменился). Поле `db_users.auth_plugin` хранит выбор.
- Бэкапы: дампы `mysqldump --single-transaction --routines --triggers --events` по базам. `mysqlsh util.dumpSchemas` (параллельно, быстрее на больших объёмах) и физические копии XtraBackup 8.4 всего сервера (Percona) — планируются.
- Обновления в пределах 8.4.x — `mp stack upgrade db`, только по имени и не в ежедневном обновлении: перезапуск сервера — простой сайтов; конфигурация проверяется `mysqld --validate-config` до обновления и перед перезапуском. Переход на 9.x не автоматизируется (отдельная процедура с проверкой старой аутентификации и пользователей).
- phpMyAdmin (0.9.1): свой порт (по умолчанию 2083) с именем и сертификатом панели, отдельный пул FPM `monopanel-phpmyadmin` от `monopanel-pma` на новейшей подходящей ветке PHP (7.4–8.5); вход формой пользователем базы или кнопкой из панели (signon, временная учётка MySQL только на эту базу) — [09](09-web-ui.md#базы-данных).

## 5. OS Profile — различия ОС в одном месте

| Параметр | Debian / Ubuntu | EL9 / EL10 |
|---|---|---|
| Пакетный менеджер | apt/dpkg, `DEBIAN_FRONTEND=noninteractive` | dnf/rpm |
| Пользователь nginx | `nginx` (пакет nginx.org) | `nginx` |
| Пользователь Apache | `www-data` (панель выводит его из `monopanel-web`; дочерние процессы Apache аккаунта — от имени аккаунта) | `apache` (режима apache нет) |
| Apache: пакет / сервис / каталог | `apache2` / `monopanel-apache@<логин>.service` (`apache2.service` выключен) / `/etc/apache2/monopanel/accounts/<логин>/` | — |
| Apache: проверка | `apache2 -t -f /etc/apache2/monopanel/accounts/<логин>/httpd.conf` | — |
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
| Корневые сертификаты | `/etc/ssl/certs/ca-certificates.crt` | `/etc/pki/tls/certs/ca-bundle.crt` |

Штатный пул `www` пакета на обеих раскладках — пустой пул панели (панель делает его таким один раз на ветку, см. [03 §5](03-web-stack.md#5-шаблон-пула-php-fpm)). На EL мастер php-fpm пишет журнал в syslog, а не в `/var/opt/remi/phpXY/log/php-fpm/` (каталог принадлежит apache). PHP из AppStream панель не ставит и не трогает.

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

Сделано (2026-09-09, проверено на площадке на AlmaLinux 9/10 и Rocky 9/10): при первой установке nginx или PHP панель один раз готовит хост под хостинг — ставит `policycoreutils-python-utils`, добавляет `fcontext` для сокетов FPM (`/var/run/monopanel(/.*)?` → `httpd_var_run_t`; при правиле эквивалентности `/run` ↔ `/var/run` semanage сам подсказывает написание, и панель ему следует) и для логов сайтов (`/var/www/[^/]+/data/logs(/.*)?` → `httpd_log_t`), включает булевы `httpd_unified`, `httpd_can_network_connect`, `httpd_can_network_connect_db`, `httpd_can_sendmail`, `httpd_execmem`, `httpd_setrlimit` и делает `restorecon` по `/var/www`, `/run/monopanel`, `/etc/nginx`, `/var/log/nginx`.

Логи сайтов с переносом из `data/logs` в дерево root (после 0.8.19, [03](03-web-stack.md#2-файловая-структура-на-сервере)) получили своё правило: `/var/log/monopanel/sites(/.*)?` → `httpd_log_t` (путь идёт за `log_dir`). Оно входит в подготовку хоста, а хост, подготовленный прежней версией, — или хост с сайтами, на котором SELinux включили позже, — получает его при старте панели вместе с `restorecon` этого дерева — раньше, чем сайты переходят на новые пути: каталог, созданный под `/var/log`, наследует `var_log_t`, а в него nginx и php-fpm из `httpd_t` писать не могут. Если `semanage` при старте не сработал, панель повторяет правило каждый час (перед повтором переноса логов) и перед применением любого сайта: починенный `semanage` подхватывается в течение часа, без перезапуска панели. Пока правила нет, сайт на хосте в enforcing не применяется — задача завершается ошибкой с причиной от `semanage`, а сайт работает на прежней конфигурации (новый сайт так и не появляется; сайт 0.8.19 остаётся с логами в `data/logs` — новую конфигурацию с логами там агент не примет, чтобы root не открывал логи в каталоге аккаунта); в permissive сайт применяется, в журнале задачи — предупреждение. Приостановку это не задерживает: заглушка логов не пишет. Так же ждёт правила вебпочта на своём порту: на хосте в enforcing без него её серверный блок остаётся прежним (у вебпочты, опубликованной 0.8.19, — с логами в `data/logs`; перенос логов перепишет его, когда правило появится), а остальное изменение почты применяется; в журнале задачи и панели — предупреждение `the webmail on port N stays as it is`. Вебпочту, впервые открытую на порту, после появления правила опубликует следующее изменение почты (`mp mail apply`). Правило для `data/logs` остаётся — старым каталогам оно не мешает.

Агент после каждой записи конфигов и создания каталогов восстанавливает метки сам, а в наборе конфигов есть поле `restore`. Оно появилось из-за `nginx -t`: проверяя конфигурацию, агент создавал `/run/nginx.pid` со своей меткой (`var_run_t`), и nginx в домене `httpd_t` не мог его открыть — теперь перед запуском файл перемечается. Сама панель пока работает без своего домена: агент (root, без NoNewPrivileges) — в `unconfined_service_t`. Изменения пакетного менеджера systemd запускает в `monopanel-pkg-<run>.service` с ограничениями агента, ни одно из которых не включает для root NoNewPrivileges, поэтому dnf там переходит в `rpm_t`, как из юнитов dnf-automatic, а скриптлеты пакетов — в `rpm_script_t`.

Вместе с этой подготовкой (и при старте на хостах, подготовленных прежними версиями) панель ставит свой модуль политики `monopanel` — `/etc/monopanel/selinux/monopanel.cil`, `semodule -i`. Сейчас в нём два правила: `allow httpd_t self:capability sys_ptrace` и `allow httpd_t self:process ptrace`. slow-лог php-fpm пишет трассировку PHP, подключаясь к рабочему процессу через `ptrace`; мастер работает от root в `httpd_t`, рабочий — от пользователя сайта, и без `CAP_SYS_PTRACE` штатная политика это молча запрещает: на EL в slow-логах были только предупреждения «executing too slow» в журнале php-fpm, без трассировок. Рабочему процессу правило ничего не даёт: без самой возможности `CAP_SYS_PTRACE` он может трассировать только процессы своего аккаунта и только в `httpd_t`.

Запуски пакетного менеджера (`monopanel-pkg-<run>.service`) и сам агент очищают свои `/tmp` и `/var/tmp` (`PrivateTmp=`) до того, как их удалит systemd: `ExecStopPost` — `monopanel clean-private-tmp <юнит>`, и только там, где смонтирован собственный `/tmp` этого юнита. Причина — `%post` Percona: он оставляет в `/tmp` каталог с меткой `rpm_script_tmp_t`, а systemd удаляет каталоги юнита в `init_t`, которому политика не даёт удалить такой каталог, — это был AVC после каждой установки и обновления Percona и каталог, который оставался до перезагрузки. Очистка работает в `unconfined_service_t`, которому это можно, поэтому правила для неё в модуле панели нет; поэтому же у обоих юнитов нет NoNewPrivileges: с ним dnf не перешёл бы в `rpm_t`, а очистка осталась бы в `init_t`. Очистка сдаётся через 30 секунд, а `TimeoutStopSec` запуска — всегда 90 секунд (меньший `DefaultTimeoutStopSec` администратора на него не действует), так что неудачей запуска она не становится. Проверено на EL9 и EL10.

Планируемый пакет `monopanel-selinux` с остальными правилами политики и `fcontext`:

- Docroot `/var/www(/.*)?` → `httpd_sys_content_t` (уже в базовой политике). Каталоги для записи (`uploads`, `cache`, `tmp`, `logs`) → `httpd_sys_rw_content_t` через `semanage fcontext` + `restorecon` при создании сайта; пользователь может пометить произвольный каталог как «записываемый» из UI.
- Сокеты FPM `/run/monopanel/php(/.*)?` → `httpd_var_run_t`.
- Собственные сборки PHP: `/opt/monopanel/php/[^/]+/sbin/php-fpm` → `httpd_exec_t`, библиотеки → `lib_t`, конфиги → `httpd_config_t`, логи → `httpd_log_t`.
- Режима apache на EL нет: Apache аккаунтов (`monopanel-apache@<логин>`) панель ставит и запускает только на Debian и Ubuntu, поэтому правила SELinux для них не нужны.
- Булевы: `httpd_can_network_connect_db=1`, `httpd_can_sendmail=1`; `httpd_can_network_connect=1` — только если хотя бы один сайт делает исходящие HTTP-запросы (флаг сайта, по умолчанию включён: так делает почти любая CMS); `httpd_execmem=1` — только при включении ionCube/JIT.
- Панель (`monopaneld`) — в первом релизе unconfined, в v1.0 — собственный домен `monopanel_t` в поставляемом модуле.
- `mp doctor` показывает AVC-denials из `ausearch` за последние сутки с подсказкой по исправлению.

### 6.1 Работа в enforcing

Панель держит SELinux в enforcing и рассчитана на это: политика выше проверена полной матрицей, включая установку Битрикса, Sphinx 3 и memcached. Единственный источник обращений — метки файлов, которые попали в docroot в обход панели: `cp -a` и `rsync -X` из `/root` сохраняют `admin_home_t`, и nginx отвечает 403, неотличимым от ошибки прав. Что для этого есть:

- `mp doctor` показывает режим SELinux и число отказов AVC для веб-домена за сегодня (nginx и php-fpm оба работают как `httpd_t`) с последним отказом и подсказкой `mp site fix <домен>`, если путь лежит в каталоге сайта; в вебе на дашборде рядом с этой строкой кнопка «починить <домен>» (или «починить все сайты», когда в записи только имя файла, а не путь), а у permissive — «вернуть enforcing». Агент читает audit через `ausearch`.
- `mp site fix <домен>` восстанавливает каталоги и ACL как при создании, делает клиента владельцем всего дерева сайта и делает `restorecon -R` по `data/`. Те же метки панель восстанавливает сама после переноса с другой панели и после распаковки архива в файловом менеджере.
- Файлы, которые создаёт сам агент, метятся сразу (`restorecon` после записи); файлы, загруженные через SFTP или файловый менеджер, наследуют метку каталога и в починке не нуждаются.
- Всё под docroot сайта — веб-контент: своё правило `/var/www/[^/]+/data/www(/.*)?` → `httpd_sys_content_t` перекрывает правила базовой политики для каталогов `logs` и `cgi-bin` внутри сайта (OpenCart пишет `system/storage/logs/error.log`, базовое правило делало его `httpd_log_t`, php-fpm получал отказ, а `restorecon` возвращал ту же метку). Набор правил версионирован: `mp site fix` применяет актуальный набор, если хост ставил nginx со старым. После починки doctor считает отказы с момента починки, а не с полуночи.

Что SELinux даёт при наших булевых переключателях (`httpd_unified`, `httpd_can_network_connect`, `httpd_can_sendmail`, `httpd_execmem`): взломанный сайт не прочитает `/etc/shadow`, `/root`, чужие домашние каталоги, данные почты и базы, а postfix, dovecot, mysqld и fail2ban остаются в своих доменах. Сайты друг от друга SELinux не изолирует — это делают отдельные пользователи, пулы php-fpm, ACL и open_basedir.

Если всё же нужен permissive (например, стороннее ПО без политики): `mp selinux permissive` или кнопка в «Настройках». Панель сначала показывает, чего это стоит, и просит подтверждения (`--yes` в скриптах), затем делает `setenforce 0` и пишет `SELINUX=permissive` в `/etc/selinux/config`, чтобы режим пережил перезагрузку. Отказы дальше только записываются в audit; `mp doctor` и страница настроек предупреждают, пока режим не вернут: `mp selinux enforcing`. `SELINUX=disabled` панель не предлагает: на EL9 и EL10 он не действует без параметра ядра `selinux=0` и перезагрузки, а панели он не нужен.

## 7. Firewall и защита от перебора

- Собственная таблица `inet monopanel` в nftables: цепочка `input` с правилами панели (ssh, 80/443, порт панели всегда открыты; allow, deny и баны из UI и CLI). Правила хранятся в `/etc/nftables.d/monopanel.nft`, их применяет юнит `monopanel-firewall`, так что после перезагрузки они на месте. Управление через библиотеку `google/nftables` вместо файла правил и `nft`, ограничение частоты соединений на ssh и порт панели, блокировки по странам — планируются.
- EL: firewalld не удаляется. Пока панель им не управляет: если он запущен (образы Oracle Linux поставляются с ним; облачные образы Alma и Rocky — нет, но серверная установка с ISO включает его), `mp setup` открывает в нём порт панели, установка nginx — 80 и 443, установка почты — её порты (постоянное правило `firewall-cmd --permanent` и то же правило в текущей конфигурации, без `--reload`: он сбросил бы правила, добавленные без `--permanent`, в том числе чужие), первый сайт с HTTP/3 — 443/udp; когда HTTP/3 не остаётся ни у одного сайта, панель закрывает 443/udp, если постоянное правило добавила она, а правило администратора оставляет (другие порты она в firewalld не закрывает); `mp firewall enable` останавливает и выключает firewalld, дальше таблицу ведёт панель. Управление зоной через D-Bus и выбор при `mp setup` — позже. Ubuntu: ufw аналогично.
- fail2ban: jail'ы `sshd`, `monopanel` (лог панели), `nginx-http-auth`, `nginx-botsearch`; действие — `nftables-multiport`.
  - Jail'ы nginx читают общий лог и логи каждого сайта (`<домен>.access.log`, `<домен>.error.log` в `/var/log/monopanel/sites/<логин>/`). Список явный, а не маска: `*.error.log` захватила бы и `<домен>.php.error.log`. Панель переписывает его при создании и удалении сайта.
  - После изменения списка панель перезапускает fail2ban, а не делает `reload`: reload идёт через `fail2ban-client`, который SELinux держит в своём домене (`fail2ban_client_t`), — он не видел логи сайтов в каталогах аккаунтов и молча выбрасывал их из jail'ов. Сам сервер (`fail2ban_t`) их читает. Баны перезапуск переживает: fail2ban хранит их в своей базе.
  - fail2ban, который включён (`enabled`), но не работает — systemd отказал в перезапуске из-за лимита запусков (каждый новый сайт перезапускает fail2ban, а перенос создаёт их подряд) или он упал, — панель перезапускает при следующем обновлении jail'ов (сайт создан или удалён, старт панели), даже если список не изменился: один раз за обновление, с записью в журнал панели. Выключенный администратором (`systemctl disable --now fail2ban`) остаётся выключенным. Лог сайта из списка jail'ов, появившийся после старта fail2ban (сайт, приостановленный при 0.8.19, снова включён), один раз перезапускает работающий fail2ban, чтобы тот начал его читать.
  - Доверенные адреса (`mp firewall trust`, страница Firewall) попадают в `ignoreip`.
  - Файл `/etc/fail2ban/jail.d/monopanel.local` панель переписывает целиком. Свои настройки — в `/etc/fail2ban/jail.d/zz-local.local`: fail2ban читает его позже, панель его не трогает.
  - На EL ставится только `fail2ban-server`: мета-пакет `fail2ban` тянет `fail2ban-sendmail` (а с ним любой почтовый сервер, на хосте без postfix — exim) и `fail2ban-firewalld` вместе с firewalld. Панель банит через nftables и писем не шлёт. Где мета-пакет уже стоит, `mp doctor` называет лишнее и команды: `dnf mark install fail2ban-server fail2ban-selinux`, затем `dnf remove fail2ban fail2ban-sendmail fail2ban-firewalld` (без первой команды удаление унесло бы и сам сервер — он числится зависимостью).

## 8. Sphinx для 1С-Битрикс

Расширение `sphinx` ставит поисковый сервер, который Битрикс ждёт в «Настройки → Поиск → Sphinx» (соединение `127.0.0.1:9306`, индекс `bitrix`). Что выяснилось на стенде:

- **Debian/Ubuntu**: пакет `sphinxsearch` (2.2.11) из дистрибутива, конфиг по эталону Битрикса «for 2.X» (`rt_field`/`rt_attr_*`), `/etc/default/sphinxsearch` с `START=yes`. Юнит сгенерирован из SysV-скрипта: `systemctl enable` его не принимает («generated»), панель это игнорирует — rc-ссылки пакета и так запускают демон.
- **EL**: пакета нет. Manticore не подходит: его `SHOW TABLES` отдаёт колонку `Table`, а `search/tools/sphinx.php` читает `$res['Index']` — Битрикс отвечает «Указанный индекс не найден». Ставится сборка Sphinx 3.9.1 с sphinxsearch.com (tar.gz, контрольная сумма зашита в панель), пользователь `sphinx`, `/opt/monopanel/sphinx`, юнит `monopanel-sphinx.service`, конфиг `/etc/sphinx/sphinx.conf`.
- **Sphinx 3 молча игнорирует написание 2.x** (`rt_attr_timestamp` и остальные `rt_attr_*`): индекс поднимался без `date_change`/`date_to`/`date_from`. Конфиг для 3.x повторяет эталон Битрикса «for 3.X»: `field`, `attr_uint`, `attr_string`, `attr_uint_set`, даты как `attr_uint` (Битрикс принимает `uint` и `timestamp`).
- **Существующий индекс на диске главнее конфига**: searchd пишет `attribute count mismatch … EXISTING INDEX TAKES PRECEDENCE` и оставляет старую схему. Поэтому после установки панель делает `DESCRIBE bitrix`, и если не хватает колонок из списка Битрикса — останавливает демон, удаляет `bitrix.*` и binlog из каталога данных и запускает заново (Битрикс наполнит индекс переиндексацией). Удаление расширения тоже убирает файлы индекса.
- `systemctl restart` возвращается раньше, чем searchd начинает слушать порты: проверка `SHOW TABLES` повторяется до 15 с, пока клиент отвечает «Can't connect».
- Символ `_` нельзя ставить одновременно в `charset_table` и `blend_chars` — индекс не поднимается (NOT SERVING).
- sphinxsearch.com отдаёт архив (40 МБ) медленно, до полутора минут: загрузки панели ограничены не общим таймаутом, а простоем (минута без данных).

## 9. Valkey на аккаунт

`mp stack install valkey` ставит сервер, `mp valkey add cache|sessions --user <login>` — экземпляр аккаунта. Что выяснилось на площадке (Debian 13 — Valkey 8.1, Ubuntu 22.04 — Redis 6.0, AlmaLinux 10 и Rocky 9 — Valkey 8.0, SELinux enforcing):

- **Общий экземпляр пакета** слушает 127.0.0.1:6379 без пароля для всех аккаунтов хоста: Debian и Ubuntu запускают его при установке, EL — нет. Панель его останавливает и выключает (`valkey-server`, `valkey`, `redis-server`).
- **Конфигурация — в аргументах** `ExecStart`: процесс работает от имени аккаунта, а `/etc/monopanel` ему не читается (0750 root:monopanel). `--port 0`, `--unixsocket … --unixsocketperm 600`, каталог сокета — `RuntimeDirectory=` с правами 0700, снимки — `StateDirectory=`. Одни и те же аргументы понимают Redis 6.0, Valkey 7.2, 8.0 и 8.1.
- **`Type=notify` и `--supervised systemd`** — как в юнитах самих пакетов. С `Type=simple` перезапуск возвращался раньше, чем сервер загрузит снимок и откроет сокет, и первый запрос PHP терял сессию.
- **Сессии** держит экземпляр `sessions`: `volatile-lru` (вытесняются только ключи с TTL, а у сессий phpredis он равен `session.gc_maxlifetime`) и `--save 60 1`; при остановке сервер пишет последний снимок, поэтому сессии переживают перезапуск. У `cache` — `allkeys-lru` и `--save ""`.
- **SELinux: метка каталога снимков.** Сокеты — `redis_var_run_t`, снимки — `redis_var_lib_t`, как у пакета: политика и так пускает php-fpm (`httpd_t`) к сокету `redis_t`. Но у init_t нет `add_name` в каталогах `redis_var_lib_t` (в `redis_var_run_t` есть — через атрибут `pidfile`), поэтому правило стоит на `/var/lib/monopanel-valkey/[^/]+(/.*)?`, а сам `/var/lib/monopanel-valkey` остаётся `var_lib_t`. С меткой на родителе каждый `StateDirectory=` падал с `238/STATE_DIRECTORY` и EACCES от `mkdirat`, причём без AVC, даже с `semodule -DB`.
- **SELinux: NoNewPrivileges.** `ProtectKernelTunables=`, `ProtectKernelModules=` и `RestrictAddressFamilies=` при `User=` не-root включают NoNewPrivileges. Под ним переход init_t → redis_t разрешён только правилом `nnp_transition`: в политике EL10 оно есть, в EL9 (selinux-policy 38.1) нет — ядро молча оставляло сервер в init_t, и тот не мог записать снимок («Failed opening the temp RDB file … Permission denied»), а при остановке отказывался выходить до SIGKILL. Поэтому в юните только «монтажная» изоляция: `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `ProtectControlGroups`.
- **Изоляция проверена**: другой аккаунт, `www-data`, `nginx` и `apache` получают «Permission denied» на сокет, TCP-портов нет; PHP-сессия через php-fpm сайта на всех четырёх ОС пишется в экземпляр и переживает `mp valkey restart sessions`.
