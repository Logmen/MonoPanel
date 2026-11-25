# 03. Веб-стек: nginx + php-fpm и nginx + Apache

## 1. Режимы сайта

### Режим A — `nginx → php-fpm` (по умолчанию)

```
клиент ──HTTPS (h2/h3)──▶ nginx ──статика напрямую
                              └─ *.php ──FastCGI unix:/run/monopanel/php/<domain>.sock──▶ php-fpm pool (user: <user>)
```

### Режим B — `nginx → Apache → php-fpm`

```
клиент ──HTTPS──▶ nginx ──статика напрямую (по расширениям, отключаемо)
                      └─ остальное ──proxy_pass http://127.100.x.y:81──▶ apache аккаунта (mpm_event, .htaccess, от его имени)
                                                                        └─ *.php ──mod_proxy_fcgi──▶ тот же php-fpm pool
```

Переключение режима — одно поле `sites.mode`; пул FPM, сокет, пользователь и php.ini не меняются. У каждого аккаунта с сайтами в режиме B свой Apache — `monopanel-apache@<логин>.service`: родитель от root слушает адрес аккаунта `127.<100+uid>>16>.<(uid>>8)&255>.<uid&255>:81` (у uid 1001 — `127.100.3.233:81`), дочерние процессы работают от имени аккаунта и читают только то, что читает он. Порт ниже 1024 занимает только root, а адрес — только этот Apache, так что и пока он перезапускается, чужой процесс не примет запросы его сайтов; nginx передаёт `Host`. Реальный IP клиента в Apache — `mod_remoteip` (`RemoteIPHeader X-Real-IP`, `RemoteIPInternalProxy 127.0.0.1`). Режим B есть только на Debian и Ubuntu; панель отказывается ставить Apache на хост, где `net.ipv4.ip_unprivileged_port_start` не выше 81 — там порт Apache мог бы занять любой аккаунт.

Apache аккаунта отвечает только nginx. Каждый запрос, который nginx шлёт на адрес Apache аккаунта (`127.100.x.y:81`), несёт секрет этого аккаунта — заголовок `X-MonoPanel-Backend` из `/etc/nginx/monopanel/apache/<логин>.conf`; всё, что его не несёт, Apache отклоняет с 403 (`/etc/apache2/monopanel/accounts/<логин>/backend.conf`, его `httpd.conf` подключает файл на верхнем уровне). Без этого любой локальный процесс, подключившись к адресу напрямую, обходил бы список `--allow` сайта и лимит запросов и сам называл бы адрес клиента (`X-Real-IP` от `127.0.0.1`), который видят `.htaccess` (`Require ip`, `Allow from`) и PHP. Оба файла — root 0600. Секрет — 32 случайных байта; панель создаёт его, когда впервые настраивает Apache аккаунта, хранит в базе зашифрованным ключом `/etc/monopanel/secret.key` и удаляет вместе с этим Apache. Отказ — правило mod_rewrite уровня сервера: оно срабатывает при разборе URL, до чтения любого `.htaccess`, и каждый виртуальный хост наследует его раньше своих правил, поэтому `.htaccess` сайта его не отменит — ни `Satisfy Any`, ни вложенный `<If>`. Вторая линия — `<If>` в `<Location "/">` (`Require all denied`, `Satisfy All`). PHP заголовка не видит (`ProxyFCGISetEnvIf`). Свой секрет аккаунт узнать может (его `.htaccess` может скопировать заголовок в ответ), но открывает им только свой же Apache. Подробно — в [§4](#4-шаблон-apache-режим-b).

Ограничение режима B: директивы `php_value` / `php_flag` в `.htaccess` работают только с `mod_php`, который не поставляется. Замена — настройки PHP в панели (`php_admin_value` в пуле) и `.user.ini` в docroot (FPM читает его благодаря `user_ini.filename`). Подсказка об этом при выборе режима и поиск `php_*`-директив в `.htaccess` с предложением перенести их в панель — планируются.

## 2. Файловая структура на сервере

```
/var/www/                                0711 root:root
/var/www/<user>/                         0710 <user>:<user>  + ACL g:monopanel-web:x   (только проход)
└── data/                                0750
    ├── www/<domain>/                    docroot; default ACL g:monopanel-web:rX
    ├── tmp/                             0700; upload_tmp_dir / session.save_path / sys_temp_dir
    ├── bin/php -> /opt/monopanel/php/8.4/bin/php
    └── .ssh/ .composer/ .wp-cli/ …
/var/www/.shares/                        0711 root:root          общие папки гостей с shell ([10](10-shared-folders.md))
└── <guest>/                             0750 root:<guest>
    └── <name>/                          0755 root:root          точка монтирования папки сайта; в доме гостя — ссылка на неё
/var/www/.restore/                       0700 root:root          раскладка восстановлений на место (только агент; с первого такого восстановления)

/var/log/monopanel/                      1771 root:monopanel        (log_dir; там же audit.log панели; sticky-бит)
└── sites/                               0711 root:root             проход есть, список не виден
    └── <user>/                          0751 root:<user>           клиент читает через свою группу
        ├── <domain>.access.log          0640 root:<user>           nginx; после USR1 владелец — его пользователь
        ├── <domain>.error.log           0640 root:<user>
        ├── <domain>.apache.{access,error}.log  0640 root:<user>    режим apache
        ├── apache.error.log             0640 root:<user>           журнал самого Apache аккаунта
        ├── <domain>.php.slow.log        0640 root:<user>           пишет мастер php-fpm
        ├── <domain>.php.error.log       0660 <user>:<user>         пишет сам PHP от имени клиента
        └── webmail.{access,error}.log   0640 root:<user>           вебпочта на своём порту

/run/monopanel/                          1771 root:monopanel        (run_dir; сокеты API и агента; sticky-бит)
└── php/                                 0755 root:root             сокеты пулов FPM; с загрузки (tmpfiles пакета и mp setup)
```

Группа `monopanel-web` = { `nginx` }: nginx читает статику через ACL, сокеты FPM создаются с `listen.group = monopanel-web`, `listen.mode = 0660`. Apache аккаунта работает от имени аккаунта и читает его файлы как он сам; учётку Apache дистрибутива (`www-data`) панель из группы выводит. Клиенты не видят друг друга. Файлы, которые создаёт PHP (uploads, cache), принадлежат клиенту и наследуют default ACL — ручной `chown`/`chmod 777` не нужен.

Логи сайтов лежат не в домашнем каталоге, а в `/var/log/monopanel/sites/<логин>/` (`log_dir` в `config.yaml`). Их открывают от root мастер nginx (при старте, reload и USR1; переоткрывая лог, он ещё и меняет его владельца по имени), родительский процесс Apache аккаунта и мастер php-fpm (slow-лог — на каждую запись). До 0.8.19 включительно логи были в `data/logs`, а этот каталог и `data/` над ним принадлежат клиенту: подменив лог символьной ссылкой на `/etc/ld.so.preload` или `/etc/cron.d/…`, клиент или любой PHP-код его сайтов заставлял серверы писать туда от root — и получал root. В `sites/` создаёт, переименовывает и удаляет записи только root. Клиент читает свои логи через свою группу (по SSH; SFTP-only-аккаунт заперт в домашнем каталоге и смотрит их в панели), панель и `mp site logs` — через агент.

Каталоги и файлы делает агент — до того, как их откроют nginx, Apache и php-fpm (nginx сам создаёт недостающий лог, но не каталог, а php-fpm не стартует, если не может открыть slow-лог). Он проходит дерево через открытые каталоги, не следуя символьным ссылкам, отказывается работать в каталоге, куда может писать кто-то кроме root, и меняет владельца и права через открытый файл; кому принадлежит файл, решает сам по имени. Логи серверов (nginx, Apache, slow-лог php-fpm) — `root:<логин>` 0640; nginx, переоткрывая лог, отдаёт его своему пользователю, но группу и права оставляет, так что клиент читает его и дальше. Журнал ошибок PHP пишет сам PHP от имени клиента, открывая файл на каждую запись, поэтому он клиента — `<логин>:<логин>` 0660; заменить его клиент не может, каталог не его. Серверы пишут логи только файлами прямо в `sites/<логин>/`: путь глубже (`sites/<логин>/<каталог>/…`) или прямо в `sites/` confcheck отклоняет, в том числе в своих директивах nginx.

Логи сайтов ротирует `/etc/logrotate.d/monopanel-sites`: по два блока на аккаунт, от root, без `su` — каталог принадлежит root, и на пути logrotate нет ничего, что контролирует клиент. Ротация раз в неделю или на ежедневном прогоне, если лог перевалил за 100 МБ; хранится восемь копий (`example.com.access.log.1`, `.2.gz` …), сжатие начиная со второй. Первый блок — логи серверов с `create 0640 root <логин>`, второй — журналы ошибок PHP с `create 0660 <логин> <логин>`. Файл, который называют два блока, logrotate не принимает, поэтому вместо `*.error.log` в первом блоке четыре маски, которые берут все логи ошибок, кроме `*.php.error.log`.

После ротации nginx и Apache аккаунта получают USR1 (у Apache это graceful restart), оба через pid-файлы — `/run/nginx.pid` и `/run/monopanel-apache/<логин>/httpd.pid`: спрашивать systemd из logrotate SELinux не разрешает. Новые файлы после USR1 открывают рабочие процессы nginx, а не мастер, поэтому и `/var/log/monopanel`, и каталог аккаунта открыты всем на проход (`x` без права читать список); без него рабочие продолжали бы писать в переименованный файл. Журнал ошибок PHP и slow-лог php-fpm открывают файл заново на каждую запись, им сигнал не нужен.

Панель, обновлённая с 0.8.19 или раньше, при первом старте записывает список сайтов, чья конфигурация ещё пишет логи в `data/logs` (`sites.moves`: активные, с ошибкой и ожидающие применения — таким может быть сайт с ошибкой, применённый заново, чьё задание прервало обновление; у приостановленного сайта логов там нет), создаёт каталоги и логи их аккаунтов и ставит в очередь `site.apply` для каждого (и `mail.apply`, если вебпочта опубликована на своём порту). Сайт выходит из списка после первого успешного `site.apply` — неважно, кто его поставил. Неудавшийся переезд повторяется при каждом старте и раз в час, но не раньше чем через 10 минут после первой неудачи, затем вчетверо дольше после каждой следующей и не реже раза в сутки; `mp doctor` (строка `site logs`) называет такие сайты и причину. Пока сайт не переехал, его логи в `data/logs` не ротируются и fail2ban их не читает: агент принимает строфы logrotate только для дерева логов (и без `su`), а пути журналов fail2ban — только в `/var/log` и дереве, потому что root не должен работать с файлами в каталоге клиента. Джейлы fail2ban следят за логами такого сайта в дереве с тех пор, как переезд их создал, — пустыми, пока сайт не переедет, — поэтому переезд сайтов меняет джейлы один раз, а не по разу на сайт: иначе fail2ban перезапускался бы на каждом сайте и на большом сервере упирался бы в ограничение systemd на частые запуски. Лог сайта из списка джейлов, появившийся после старта fail2ban (сайт, приостановленный при 0.8.19, снова включён), один раз перезапускает работающий fail2ban, чтобы тот начал его читать. Старые файлы в `data/logs` остаются на месте: теперь это файлы клиента, их никто не ротирует и, как только сайт переехал, никто не пишет и не показывает. Пока сайт не переехал (его `site.apply` после обновления не прошёл — см. строку `site logs` в `mp doctor`), его серверы пишут в старые файлы, и хвост его лога (вкладка «Логи», `mp site logs`) показывает тот из двух файлов, в который писали последним: `~/data/logs/<файл>` (его читает аккаунт, а не root) или файл в дереве, если в нём есть записи новее, — после частичного применения в режиме apache Apache уже пишет в дерево, а nginx ещё в `data/logs`. На EL каталог получает метку `httpd_log_t` (правило `fcontext` для `/var/log/monopanel/sites(/.*)?`, [02](02-platform-matrix.md#6-selinux-el9--el10)); пока правило не добавлено, сайты и вебпочта на своём порту на дерево не переводятся и остаются на прежних путях — nginx и php-fpm в `httpd_t` не открыли бы логи с меткой `var_log_t`. Если `semanage` не сработал, панель повторяет правило каждый час и перед применением сайта или настроек почты.

До 0.8.19 `/var/log/monopanel` принадлежал пользователю панели. Такой каталог скрипт пакета оставляет агенту, и тот при старте забирает его через открытый каталог — сначала владелец, потом снимаются ACL, потом права 1771 — и только затем создаёт `sites/` или доверяет ему. Самообновление ставит тот же пакет через apt-get или dnf, так что скрипт пакета выполняется и там. Каталог `sites/`, который мог оставить пользователь панели 0.8.19, агент при старте тоже забирает себе и вычищает: каталоги, которых там быть не должно, уходят в `/var/log/monopanel/untrusted-<время>-<случайное>/`, а ссылки, FIFO и файлы со вторым именем удаляются. `mp setup` и `sitelogs/ensure` такой каталог не трогают и просят перезапустить агента: каталог, отданный root'у вручную, сохранил бы то, что в нём оставили. Так же агент при старте готовит каталог сокетов пулов `<run_dir>/php` ([01 §7](01-architecture.md#7-безопасность)).

Трассировку в slow-лог php-fpm снимает через `ptrace` рабочего процесса; на EL это разрешает модуль политики панели ([02](02-platform-matrix.md#6-selinux-el9--el10)), без него slow-логи оставались пустыми.

Конфиги:

```
/etc/nginx/nginx.conf                          шаблон панели (worker_processes auto, http{} с include)
/etc/nginx/monopanel/http.d/*.conf             глобальные: ssl defaults, gzip/brotli, maps, limit zones, log format,
                                               ip-<ip>.conf — default-server на IP (acme, quic reuseport, 444 чужим именам)
/etc/nginx/monopanel/snippets/*.conf           фрагменты: fastcgi, static-cache, deny-dotfiles, acme, ssl, proxy-apache
/etc/nginx/monopanel/sites/<domain>.conf       сгенерированный server{} сайта
/etc/nginx/monopanel/sites/<domain>.d/*.conf   пользовательские include внутри server{} — не перезаписываются
/etc/nginx/monopanel/apache/<user>.conf        секрет Apache аккаунта, который nginx шлёт ему (root 0600)
/etc/nginx/conf.d/                             свободная зона администратора (панель не трогает)
/etc/apache2/monopanel/accounts/<user>/httpd.conf               Apache аккаунта целиком: User/Group, Listen, модули, MPM, логи
/etc/apache2/monopanel/accounts/<user>/backend.conf             проверка секрета nginx (root 0600)
/etc/apache2/monopanel/accounts/<user>/sites/<domain>.conf      виртуальный хост сайта
/etc/apache2/monopanel/accounts/<user>/sites/<domain>.d/*.conf  свои директивы сайта внутри <VirtualHost> — не перезаписываются
/run/monopanel-apache/<user>/                                  pid-файл и блокировки Apache аккаунта (RuntimeDirectory юнита)

пул php-fpm: /etc/monopanel/php/X.Y/pool.d/<domain>.conf  (или раскладка Sury/Remi — см. 02 §5)
```

## 3. Шаблон nginx (режим A, сокращённо)

```nginx
# /etc/nginx/monopanel/sites/example.com.conf — generated by MonoPanel; правки в example.com.d/
server {
    listen 203.0.113.10:80;
    server_name example.com www.example.com;
    include monopanel/snippets/acme.conf;              # /.well-known/acme-challenge/ → общий webroot
    {{if redirect_https}}return 301 https://$host$request_uri;{{else}}# …тело, как в 443{{end}}
}
server {
    listen 203.0.113.10:443 ssl;
    http2 on;
    {{if http3}}listen 203.0.113.10:443 quic;{{end}}  # reuseport — в default-server на этот IP
    server_name example.com www.example.com;
    ssl_certificate     /var/lib/monopanel/certs/example.com/fullchain.pem;
    ssl_certificate_key /var/lib/monopanel/certs/example.com/privkey.pem;
    include monopanel/snippets/ssl.conf;               # TLS 1.2/1.3, ciphers, stapling, session cache, HSTS (флаг)
    {{if http3}}add_header Alt-Svc 'h3=":443"; ma=86400';{{end}}
    {{if redirect_www}}# if ($host = www.example.com) { return 301 … }{{end}}

    root  /var/www/alex/data/www/example.com;
    index index.php index.html;
    access_log /var/log/monopanel/sites/alex/example.com.access.log main;
    error_log  /var/log/monopanel/sites/alex/example.com.error.log;
    client_max_body_size 64m;                          # = post_max_size пула
    disable_symlinks if_not_owner from=$document_root;

    include monopanel/snippets/deny-dotfiles.conf;     # .git, .env, .user.ini, .htaccess, composer.*
    include monopanel/snippets/static-cache.conf;      # expires для картинок/шрифтов/js/css
    include monopanel/sites/example.com.d/*.conf;      # пользовательские location — раньше общих

    location ~ ^/[^/]+\.php(?:/|$) {                  # скрипт в корне docroot, в т.ч. с путём после имени
        if (!-f $document_root$fastcgi_script_name) { return 404; }
        include monopanel/snippets/fastcgi.conf;
        fastcgi_pass unix:/run/monopanel/php/example.com.sock;
    }
    location / { try_files $uri $uri/ /index.php?$args; }
    location ~ \.php$ {
        try_files $uri =404;
        include monopanel/snippets/fastcgi.conf;       # fastcgi_params, буферы, таймауты = request_terminate_timeout
        fastcgi_pass unix:/run/monopanel/php/example.com.sock;
    }
}
```

Скрипт в корне docroot принимает путь после своего имени (PATH_INFO): Roundcube 1.7 отдаёт CSS и JS как `/static.php/skins/…`, другие фронт-контроллеры принимают `/index.php/маршрут`. Этот `location` стоит раньше статики по расширению, иначе `/static.php/…/styles.css` получал бы 404. Путь после скрипта глубже — например, в каталоге загрузок — по-прежнему уходит в `try_files` и до `index.php`. Пресеты CMS этот блок не получают: их правила, закрывающие отдельные скрипты, написаны для `\.php$`. Если в своём `<domain>.d/*.conf` закрываете скрипт в корне, пишите правило с окончанием `(?:/|$)`, а не `$`, — иначе `/script.php/x` его обойдёт.

`fastcgi_pass` в конфигурации nginx — только сокет пула сайта: `unix:/run/monopanel/php/<домен>.sock` (каталог — `run_dir` из `config.yaml`); TCP-адреса, другие unix-сокеты (штатные пулы `/run/php/…`, `/var/opt/remi/…/www.sock`, `/run/php-fpm/www.sock`), имена upstream и переменные агент отклоняет. Отклоняются и `fastcgi_param PHP_VALUE`/`PHP_ADMIN_VALUE` (php-fpm применяет их к процессу пула как строки ini, `extension=` загружает любую библиотеку; PHP-параметры сайта задаёт `mp site set --ini`). Первые байты, которые nginx отправляет на сокет, не должен выбирать клиент: `proxy_method` — только слово из букв, цифр, `-` и `_`, начинающееся с буквы (не переменная и не пустая строка); `uwsgi_modifier1` — только 0; в блоке `stream` нельзя `include`, `proxy_pass unix:…` или с переменной и серверов `unix:` в `upstream`. Иначе запрос клиента заговорил бы по FastCGI с любым сокетом, куда может подключиться пользователь nginx. `include` — только файлы в `/etc/nginx`, кроме `/etc/nginx/default.d` (там локейшены PHP дистрибутива, которые передают запросы его собственным пулам); шаблон (`*`, `?`, `[`) — только в имени файла, без обратной косой черты: nginx раскрывает шаблон через glob(3), а тот читает `\.\.` и `.*` в каталоге как `..`. Записанный раньше `custom.conf` с такими строками продолжает работать, но перезаписать его можно только без них.

Режим B отличается блоками location: статика по regex расширений отдаётся nginx (переключатель `static_by_nginx`), всё остальное — `proxy_pass` на адрес Apache аккаунта (`http://127.100.3.233:81` у uid 1001) со сниппетом `proxy-apache.conf` (`proxy_http_version 1.1`, `Host`, `X-Real-IP`, `X-Forwarded-Proto`, `X-Forwarded-For`, буферы, таймауты). Оба location, которые проксируют на Apache аккаунта, подключают `monopanel/apache/<логин>.conf` сразу после сниппета: уровень, где задан хоть один `proxy_set_header`, теряет унаследованные, поэтому секрет стоит рядом с заголовками сниппета. Location, который проксирует на Apache аккаунта (`proxy_pass http://127.100.x.y:81…`) без этого include рядом с `proxy_pass`, агент не примет — Apache отвечал бы на его запросы 403; файл секрета подключается только в location, чей единственный `proxy_pass` ведёт на Apache того же аккаунта, и без вложенных блоков, которые сами проксируют. Это касается и своих директив сайта (`mp site nginx --set`), и переопределения `nginx/site.conf.tmpl` в `/etc/monopanel/templates`: в переопределение из 0.8.19 в оба location режима Apache нужно добавить `include {{ .ApacheBackendConf }};`, иначе сайт при обновлении не переедет на свой Apache (задача назовёт его).

HTTP/3: `listen … quic reuseport` допускается один раз на IP:порт, поэтому панель держит на каждый IP отдельный default-server (`http.d/ip-<ip>.conf`) с `reuseport`, а сайты объявляют `quic` без него. Он же принимает ACME-проверки и закрывает соединения к неизвестным доменам — об этом абзац ниже.

Сервер по умолчанию на каждом адресе (`http.d/ip-<адрес>.conf`) принимает ACME-проверки, имя самой панели без порта перенаправляет на её HTTPS-порт (`http://panel.example.com/` → `https://panel.example.com:8443/`), а всё остальное закрывает без ответа (444). Если у имени панели есть свой сайт, побеждает сайт: точное `server_name` в nginx старше блока по умолчанию. Файлы перегенерируются при старте API, так что смена `web.hostname` доезжает до nginx после `--restart`.

## 4. Шаблон Apache (режим B)

Виртуальный хост сайта в Apache его аккаунта (аккаунт `alex`, uid 1001; блок `<Directory>` папки `upload/exchange` появляется, когда она выдана гостю как общая папка с `--no-php`):

```apache
# /etc/apache2/monopanel/accounts/alex/sites/example.com.conf
<VirtualHost 127.100.3.233:81>
    ServerName example.com
    ServerAlias www.example.com
    RewriteEngine On                     # без него хост не унаследует проверку секрета nginx
    DocumentRoot /var/www/alex/data/www/example.com
    <Directory /var/www/alex/data/www/example.com>
        AllowOverride All
        Options -Indexes +SymLinksIfOwnerMatch
        Require all granted
    </Directory>
    <Directory /var/www/alex/data/www/example.com/upload/exchange>
        AllowOverride None
        Options +SymLinksIfOwnerMatch
        <FilesMatch "\.(?:php|phtml|phar)$">
            Require all denied
        </FilesMatch>
    </Directory>
    <FilesMatch "\.php$">
        SetHandler "proxy:unix:/run/monopanel/php/example.com.sock|fcgi://localhost"
    </FilesMatch>
    ProxyTimeout 150
    ErrorLog  /var/log/monopanel/sites/alex/example.com.apache.error.log
    CustomLog /var/log/monopanel/sites/alex/example.com.apache.access.log combined
    IncludeOptional /etc/apache2/monopanel/accounts/alex/sites/example.com.d/*.conf
</VirtualHost>
```

`httpd.conf` аккаунта (`/etc/apache2/monopanel/accounts/<логин>/httpd.conf`) — вся конфигурация, с которой юнит запускает `apache2 -f … -DFOREGROUND`, без `apache2.conf`, `envvars` и `mods-enabled` дистрибутива:

- `User` и `Group` — логин аккаунта, `Listen` — его адрес на порту 81, pid-файл, блокировки и `DefaultRuntimeDir` — `/run/monopanel-apache/<логин>/`, журнал самого Apache — `apache.error.log` в каталоге логов аккаунта;
- `RemoteIPHeader X-Real-IP` и `RemoteIPInternalProxy 127.0.0.1` — адрес посетителя от nginx;
- `<Directory />` закрыт (`AllowOverride None`, `RewriteEngine Off`, `Require all denied`), а в доме аккаунта `.htaccess` может всё (`AllowOverride All`, `Options -Indexes +SymLinksIfOwnerMatch`): то, что дочерние процессы делают по `.htaccess`, они делают от имени аккаунта, и ни `ExecCGI`, ни символьная ссылка на чужой файл не дают им больше, чем есть у аккаунта;
- два `<LocationMatch>` с `Require all denied` — те же выражения, что в `snippets/deny-dotfiles.conf` nginx (dot-файлы и каталоги, кроме `.well-known` и `.default`; `composer.json/lock`, `package*.json`, `yarn.lock`, `.user.ini`, `.htaccess`, `.htpasswd`): nginx такие запросы в Apache не шлёт, но к адресу Apache может подключиться любой локальный процесс;
- `Include /etc/apache2/monopanel/accounts/<логин>/backend.conf` на верхнем уровне — проверка секрета nginx (ниже);
- в конце — `IncludeOptional …/sites/*.conf`, виртуальные хосты сайтов аккаунта.

Модули — только эти: `mpm_event, authz_core, authz_host, authz_user, authn_core, authn_file, auth_basic, access_compat, alias, dir, mime, env, headers, expires, rewrite, setenvif, filter, deflate, remoteip, proxy, proxy_fcgi, negotiation, reqtimeout, autoindex` (листинг каталога — только там, где сайт включил его сам). Ни CGI, ни SSI (`include`), ни Lua, ни `ext_filter` — они запускают программы, — ни `status` и `info`; `mod_php` не ставится. MPM: один дочерний процесс на 16 потоков в покое, до четырёх (`StartServers 1`, `ThreadsPerChild 16`, `MaxRequestWorkers 64`) — статику и медленных клиентов держит nginx. `ProxyTimeout` = `request_terminate_timeout` пула.

Агент принимает эти файлы только в таком виде и только для аккаунта хостинга: `User`, `Group`, `Listen`, pid-файл, блокировки, `ServerRoot` и модули задаёт лишь `httpd.conf`, и только значениями этого аккаунта; `httpd.conf` подключает только свой `backend.conf` и свои `sites/*.conf`, виртуальный хост — только свой `sites/<домен>.d/*.conf`, собственные директивы не подключают ничего; `Define` не принимается; журналы — только прямо в каталоге логов этого аккаунта или в syslog. Перед reload всё проверяет `apache2 -t -f <httpd.conf аккаунта>`.

Проверка секрета nginx — `backend.conf` аккаунта (root 0600, секрет — 64 шестнадцатеричных знака), который `httpd.conf` подключает на верхнем уровне:

```apache
RewriteEngine On
RewriteOptions InheritDownBefore
RewriteCond %{HTTP:X-MonoPanel-Backend} !=<64 hex> [NV]
RewriteRule ^ - [F]
<Location "/">
    <If "req_novary('X-MonoPanel-Backend') != '<64 hex>'">
        Require all denied
        <IfModule access_compat_module>
            Satisfy All
        </IfModule>
    </If>
    ProxyFCGISetEnvIf "true" !HTTP_X_MONOPANEL_BACKEND
    ProxyFCGISetEnvIf "true" !SCRIPT_URI
    ProxyFCGISetEnvIf "true" !SCRIPT_URL
    ProxyFCGISetEnvIf "true" !REDIRECT_SCRIPT_URI
    ProxyFCGISetEnvIf "true" !REDIRECT_SCRIPT_URL
</Location>
```

Виртуальный хост наследует правила сервера, только если сам включает mod_rewrite, отсюда `RewriteEngine On` в каждом `<VirtualHost>`; `<Directory />` снова выключает его для каталогов, так что `.htaccess` без своего `RewriteEngine On` ведёт себя как раньше. Одна авторизация (`Require` в `<If>`) против `.htaccess` сайта не держится: `Satisfy Any` (mod_access_compat) пропускает запрос мимо любых `Require`, если прошёл `Allow` — по умолчанию или по подделанному `X-Real-IP`, а вложенный `<If>` из `.htaccess` сливается после любого `<If>` из `httpd.conf`. `[NV]` и `req_novary` — чтобы в ответах не появлялся `Vary: X-MonoPanel-Backend`; `RequestHeader unset` не подходит: внутренний редирект (`RewriteRule` фронт-контроллера в `.htaccess`, локальный `ErrorDocument`) проверяется заново на тех же заголовках и получал бы 403. `ProxyFCGISetEnvIf` убирает из того, что получает PHP, сам заголовок, а с ним `SCRIPT_URI` и `SCRIPT_URL` (и их копии `REDIRECT_…`): их добавляет mod_rewrite, включённый на уровне виртуального хоста, до 0.8.19 включительно PHP их не получал, а `SCRIPT_URI` называл бы схему `http` и у сайта на HTTPS — nginx говорит с Apache аккаунта по http.

В файлах Apache аккаунта, которые пишет панель, агент не принимает директивы mod_rewrite на уровне сервера (они только в `backend.conf`), `<VirtualHost>` без `RewriteEngine On`, `RewriteEngine Off` вне `<Directory>`, `<Files>` и `<Proxy>`, `RewriteOptions Inherit` или `IgnoreInherit` на уровне виртуального хоста (вместо `Inherit` — `InheritBefore`) и `<If>` внутри `<Location>` или другого `<If>`: такой `<If>` слился бы после `<If>` проверки. `backend.conf` и файл секрета nginx он принимает только в виде панели. Переопределение `apache/site.conf.tmpl` должно оставить `RewriteEngine On` прямо в `<VirtualHost>`. Свои директивы, которые администратор пишет в `sites/<домен>.d/` руками, агент не проверяет: `RewriteEngine Off` на уровне виртуального хоста там снимает основную проверку для этого сайта.

Сайт получает Apache аккаунта раньше, чем nginx начинает слать его туда, а уходит из него после того, как ушёл из nginx, — без перерыва (подробно — [01 §6](01-architecture.md#6-конвейер-применения-конфигурации)). Новый Apache требует секрет nginx с первого запуска. Apache без сайтов останавливается и выключается, а файл секрета nginx и сам секрет удаляются. Собственные директивы сайта (`sites/<домен>.d/`) живут, пока жив сайт: смена режима и приостановка их не трогают. Шаблон `monopanel-apache@.service` приходит с пакетом и останавливается мягко (`KillSignal=SIGWINCH`, `KillMode=mixed`, `TimeoutStopSec=60`): запросы, которые старые воркеры nginx ещё ведут после ухода сайта, дорабатывают до минуты. При понижении пакета до версии ниже 0.9.0 preremove копирует шаблон в `/etc/systemd/system`, и Apache аккаунтов переживают перезагрузку ([README, «Откат на старую версию»](../README.md#откат-на-старую-версию)), а следующее обновление удаляет копию.

Обновление с 0.8.19. До 0.9.0 все сайты в режиме B обслуживал один `apache2.service` дистрибутива от `www-data` на `127.0.0.1:8080`: `.htaccess` сайта мог направить PHP в пул другого аккаунта и задать ему `php_admin_value`, через символьную ссылку читать чужие файлы, показать server-status всех сайтов, а пока Apache перезапускался, порт мог занять любой локальный аккаунт. Обновлённая панель переносит сайты на Apache их аккаунтов сама, по одному и без простоя (задача `apache.migrate`), вместе с собственными директивами из прежних `sites/<домен>.d/` — и у сайтов, ушедших из режима apache. Директивы читает агент (`apache/legacy`, [01 §3.2](01-architecture.md#32-monopaneld-agent--привилегированный-процесс)), и переносятся они не доступнее, чем были: файл, доступный только root, остаётся таким и у Apache аккаунта. Сайт, переехавший на Apache своего аккаунта, сразу уходит и из общего Apache: после переключения nginx его виртуальный хост в `/etc/apache2/monopanel/sites/` удаляется (общий Apache ради этого не перезагружается). Пока какой-то сайт переехать не может и общий Apache работает, он открывает логи в `data/logs` только у сайтов, которые на нём остались. Когда переехали все, `apache2.service` останавливается и выключается, файлы прежней общей конфигурации удаляются, а `www-data` выходит из группы `monopanel-web`. Сайт, который не переехал, держит общий Apache включённым, и задача называет его. Помешать могут собственные директивы, которые агент не принимает для Apache аккаунта: `Include`, журнал в другом каталоге, переменные `APACHE_*`, которых там никто не задаёт, а на уровне виртуального хоста (не в `<Directory>`/`<Files>`) — `RewriteEngine Off` и `RewriteOptions Inherit`/`IgnoreInherit`, которые сняли бы проверку секрета nginx; файл директив, который агент не читает (символьная ссылка, жёсткая ссылка, файл больше 1 МиБ); переопределения шаблонов без проверки секрета — `nginx/site.conf.tmpl` без `include {{ .ApacheBackendConf }};` и `apache/site.conf.tmpl` без `RewriteEngine On` в `<VirtualHost>`. Исправив их (`RewriteEngine Off` — в `<Directory>`, `Inherit` → `InheritBefore`), повторите `mp stack install apache`: неудавшийся переезд не повторяется при каждом старте, следующая попытка ждёт так же, как у логов ([§2](#2-файловая-структура-на-сервере)), а состояние видно в строке `service apache2.service` вывода `mp doctor`. Apache аккаунтов, настроенные без секрета (так их поднимали сборки разработки 0.9.0 до него), панель после переезда один раз переводит на секрет (задача `apache.backend`): сначала nginx начинает слать секрет для всех сайтов аккаунта, потом Apache начинает его требовать, без 403; пока этого не случилось, `mp doctor` называет такой Apache строкой `backend monopanel-apache@<логин>.service`. Пока переезд не закончен, `mp doctor` не проверяет Apache аккаунта, ни один сайт которого ещё не переехал (у него ещё нет `httpd.conf`), а строка `apache move` называет сайты на общем `apache2.service`; если общий Apache при этом остановлен, строка `service apache2.service` — провал: эти сайты отвечают 502, и его нужно запустить (`systemctl start apache2.service`), а не выключать. `mp stack upgrade apache` (и ежедневное обновление) в это время сначала проверяет общую конфигурацию (`apache2ctl -t`) и при ошибке Apache не обновляет — его пакет перезапустил бы общий Apache на этой конфигурации; после обновления общий Apache после той же проверки мягко перезапускается, а работающий на заменённом бинарнике — перезапускается полностью. Строка Apache на странице «Расширения», в `mp stack list` и в TUI в это время считает только Apache аккаунтов, у которых уже есть переехавший сайт, называет число сайтов, ждущих переезда, и состояние общего `apache2.service`: работающий — предупреждение (жёлтая метка), остановленный при ждущих сайтах — ошибка (красная метка; `mp stack list` добавляет «systemctl start apache2.service»), на заменённом бинарнике — кнопка «обновить» (`mp stack upgrade apache`, `u` в TUI).

## 5. Шаблон пула php-fpm

```ini
[example.com]
user  = alex
group = alex
listen = /run/monopanel/php/example.com.sock
listen.owner = alex
listen.group = monopanel-web
listen.mode  = 0660
pm = ondemand                    ; ondemand (по умолчанию) | dynamic | static
pm.max_children = 8
pm.process_idle_timeout = 10s
pm.max_requests = 500
request_terminate_timeout = 150s ; max_execution_time + 30
slowlog = /var/log/monopanel/sites/alex/example.com.php.slow.log
request_slowlog_timeout = 10s
catch_workers_output = no        ; вывод воркеров не попадает в журнал мастера (root); ошибки PHP — в php_admin_value[error_log]
clear_env = no                   ; PATH/HOME нужны composer/wp-cli
env[PATH]   = /var/www/alex/data/bin:/usr/local/bin:/usr/bin:/bin
env[TMPDIR] = /var/www/alex/data/tmp
php_admin_value[open_basedir]      = /var/www/alex/data
php_admin_value[upload_tmp_dir]    = /var/www/alex/data/tmp
php_admin_value[session.save_path] = /var/www/alex/data/tmp/sess
php_admin_value[sys_temp_dir]      = /var/www/alex/data/tmp
php_admin_value[error_log]         = /var/log/monopanel/sites/alex/example.com.php.error.log
php_admin_value[sendmail_path]     = /usr/sbin/sendmail -t -i -f noreply@example.com
php_admin_value[disable_functions] = passthru,shell_exec,system,proc_open,popen,pcntl_exec,pcntl_fork
; редактируются из UI/CLI; php_value допускает переопределение в .user.ini
php_value[memory_limit]         = 256M
php_value[upload_max_filesize]  = 64M
php_value[post_max_size]        = 64M
php_value[max_execution_time]   = 120
php_value[date.timezone]        = Europe/Moscow
php_value[display_errors]       = Off
php_value[short_open_tag]       = Off   ; пресет bitrix — On
php_value[opcache.enable]       = 1
```

`disable_functions` по умолчанию перечислены выше; переключатель `allow_exec` на сайт снимает их (WP-CLI-хуки, очереди Laravel, ImageMagick через CLI).

`mp doctor` предупреждает о любом пуле установленной ветки с включённым `catch_workers_output` (строка `php-fpm pools`): через него воркер может получить root через мастер (CVE-2026-129291; исправлено в PHP 8.2.36, 8.3.37, 8.4.28 и 8.5.13). В шаблоне 0.8.19 он был включён. Пулы сайтов 0.9.0 переписывает, когда переводит сайты на дерево логов ([§2](#2-файловая-структура-на-сервере)): сайт, чей переезд не удался, держит старый пул, пока его не применят (`mp site apply <домен>`), — такие сайты называет строка «site logs» в `mp doctor`; из своего пула строку убирают руками. Каталог пулов или файл, который диагностика не смогла прочитать, называется в предупреждении, а не считается чистым.

PHP-сессии по умолчанию — файлы в `tmp/sess` аккаунта. Сайт можно перевести в экземпляр Valkey `sessions` своего аккаунта (`mp site set <домен> --sessions valkey`, поле «PHP-сессии» в настройках сайта), и тогда вместо `session.save_path` на файлы пул получает:

```ini
php_admin_value[session.save_handler] = redis
php_admin_value[session.save_path]    = "unix:///run/monopanel-valkey/alex-sessions/valkey.sock"
```

Нужно расширение redis у ветки PHP сайта. Сокет с правами 600 принадлежит аккаунту: пул сайта работает от его имени и подключается, пулы других аккаунтов — нет ([02](02-platform-matrix.md#9-valkey-на-аккаунт)).

`pm.max_children` по умолчанию 8; пресету bitrix — по воркеру на 256 МБ RAM, от 8 до 48 (число выбирается, когда сайт получает пресет: при создании, смене пресета или переносе). Причина в том, что шаблоны Битрикса ходят по HTTP на свой же сайт прямо из запроса, и восемь занятых воркеров ждут друг друга. Задать `max_children` вручную можно для любого сайта. При `pm=ondemand` неактивные сайты не держат воркеров — это основной режим для общего сервера с сотнями сайтов; нагруженным сайтам вручную ставят `dynamic` или `static`.

Глобальный `99-monopanel.ini` ветки держит `short_open_tag = Off`, но копия, которую читает CLI, разрешает короткие теги, пока на ветке есть сайт с пресетом bitrix: cron-скрипты и пролог Битрикса начинаются с `<?`, и иначе PHP печатал бы их как текст с кодом 0. Пулы задают `short_open_tag` сами, поэтому на Remi, где CLI и FPM читают один каталог, сайтам это не мешает.

Свой пул `[www]` пакета php-fpm каждой ветки (Debian/Ubuntu — `/etc/php/X.Y/fpm/pool.d/www.conf`, Remi — `/etc/opt/remi/phpXY/php-fpm.d/www.conf`) панель не удаляет — без пула мастер php-fpm не стартует, — а держит в покое: пул работает от `nobody`, слушает `/run/monopanel/php/.idle-X.Y.sock` (владелец root, права 0600 — ни nginx, ни аккаунты подключиться не могут) и не запускает ни одного процесса, пока к нему не подключились (`pm = ondemand`, `pm.max_children = 1`). Штатный пул работал от `www-data` (Debian) или `apache` (EL, с доступом nginx через `listen.acl_users`), а этим учёткам принадлежат каталог pid-файла и сокета мастера (`/run/php` на Debian и Ubuntu) и каталог его журнала (`/var/opt/remi/phpXY/log/php-fpm` на EL): PHP от их имени добирался через эти файлы до root. Файл пишется один раз на ветку — при её установке или при первом старте 0.9.0 для ветки, поставленной раньше (отметка `php.distro_idle.<X.Y>`); дальше он принадлежит администратору. Комментарии не в счёт: пул с директивами пустого пула — панельный, ваша заметка в нём остаётся; комментарий сборок разработки 0.9.0 (будто файл переписывается при каждом старте) при следующем старте один раз заменяется, без перезагрузки php-fpm. Вернувший штатный `www.conf` (phpMyAdmin из apt, версия сопровождающего на вопросе dpkg) его сохранит, а `mp doctor` предупредит строкой `php X.Y pool www`: PHP в этом пуле снова работает от `www-data` (`apache` на EL). Панели это дороги к пулу не открывает: `fastcgi_pass` в конфигурациях nginx, которые она пишет, ведёт только к сокетам её пулов, а дочерние процессы Apache аккаунтов работают от имени аккаунта. Чтобы вернуть файл панели, его удаляют и перезапускают `monopanel-api`: отсутствующий пул панель пишет снова. Другие файлы каталога пулов она не трогает. На EL мастер php-fpm пишет журнал в syslog (`error_log = syslog` в `php-fpm.conf`, остальное в файле без изменений): смотреть его — `journalctl -u phpXY-php-fpm`. Эту строку панель тоже ставит один раз; если её потом убрать или изменить, `mp doctor` предупреждает в той же строке `php X.Y pool www`, а вернуть её нужно руками: `php-fpm.conf` панель заново не пишет.

Секцию `[www]` — в любом регистре, в кавычках, с `${…}` — панель принимает только в самом `www.conf`: php-fpm сливает одноимённые секции из всех файлов, и `[www]` в другом файле изменила бы пустой пул. Пулы называются простыми словами (именами сайтов). В `include` файла `php-fpm.conf` шаблон (`*`) допускается только в имени файла (`pool.d/*.conf`): php-fpm раскрывает путь через glob(3), а шаблон в каталоге совпадает и с `..`.

## 6. Изоляция и лимиты

| Уровень | Механизм | Состояние |
|---|---|---|
| Файлы | unix-пользователь на клиента, 0710, ACL для веб-группы, `open_basedir`, отдельный tmp/session | есть, включено |
| Процессы PHP | пул FPM от пользователя, `disable_functions`, `pm.max_children` | есть, включено |
| Symlink | `disable_symlinks if_not_owner` (nginx); `SymLinksIfOwnerMatch` в Apache аккаунта, чьи дочерние процессы и так работают от имени аккаунта — ссылка на чужой файл им ничего не даёт | есть, включено |
| Запросы в обход nginx | любой локальный процесс может подключиться к `127.100.x.y:81`, но Apache аккаунта отвечает только на запросы с секретом аккаунта, который шлёт nginx (`X-MonoPanel-Backend`), остальным — 403: `allow`/`deny` сайта, лимит запросов и `X-Real-IP` в обход nginx не обойти. Dot-файлы и файлы менеджеров пакетов Apache аккаунта не отдаёт и с секретом, PHP общих папок с `--no-php` не выполняет. Свой секрет аккаунт узнать может, но он открывает только его же Apache | есть, включено |
| Диск | квоты файловой системы на uid (`quota` / `xfs_quota`); без квот — учёт по `du` в метриках | планируется |
| CPU/RAM на сайт | изолированный пул: отдельный мастер `monopanel-php-fpm@<X.Y>-<domain>.service` в `Slice=monopanel-<user>.slice` с `MemoryMax`, `CPUQuota`, `TasksMax`, `PrivateTmp`, `ProtectSystem=strict`, `ReadWritePaths=/var/www/<user>` | планируется |
| SSH | shell только по флагу; SFTP-only chroot (`Match Group monopanel-sftp`) | есть, по умолчанию только SFTP |
| Сеть | исходящие соединения PHP не ограничиваются (API, платежи); запрет исходящего SMTP через nft-правило на uid (`meta skuid`) | планируется |

## 7. TLS / ACME

- Клиент — `lego` как библиотека внутри `monopaneld api`: аккаунт, ключи, challenge-и, продление.
- HTTP-01: общий webroot `/var/lib/monopanel/acme/webroot`, подключён сниппетом `acme.conf` во всех `:80` server{} и в default-server каждого IP → выпуск работает и до создания сайта, и при редиректе на HTTPS.
- DNS-01: провайдеры lego — Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap и RFC2136 (`mp dns-provider types`) — для wildcard `*.example.com`; учётные данные хранятся зашифрованными, у каждого пользователя свои.
- Ключи: ECDSA P-256 по умолчанию, RSA-2048 по флагу; двойной сертификат (EC + RSA) — планируется.
- Хранение: `/var/lib/monopanel/certs/<domain>/{fullchain.pem,privkey.pem,chain.pem}` 0600 root (мастер nginx работает от root; TLS завершает nginx, Apache аккаунтов сертификатов не читают). Импорт своих сертификатов (PEM) — через UI/CLI/API; один сертификат может обслуживать несколько сайтов (SAN).
- Wildcard (0.9.2): `*.example.com` покрывает имя ровно на одну метку ниже — `www.example.com`, но не `example.com` и не `a.b.example.com` (RFC 6125). Сайт берёт сертификат со своей записи, иначе лучший из подходящих: запись с его именем, затем сертификат, где его имя есть среди имён, затем wildcard. Импорт или выпуск wildcard привязывает его к сайтам, которые он покрывает и у которых нет своего действующего сертификата; сайт со своим сертификатом на точное имя остаётся на нём. Wildcard, привезённый аккаунтом при переносе, обслуживает только сайты этого аккаунта. При переносе один файл, подключённый на нескольких сайтах источника, приезжает одной записью. Имена сайта, которые сертификат не покрывает (`www.kz.example.com` под `*.example.com`), задача `site.apply` называет в журнале. Панель и почта по-прежнему берут сертификат только с записи под своим именем.
- Продление: планировщик раз в сутки, за 30 дней до истечения; при неудаче — повторы с растущим интервалом, ошибка видна в `mp doctor` и уходит webhook-событием `cert.issue.failed`; после успеха сайты на этом сертификате применяются заново (`site.apply`) — reload nginx.
- Нативный `ngx_http_acme_module` (nginx 1.29+) вместо lego для HTTP-01 — возможный вариант на будущее.
- Хост панели: сертификат для `web.hostname` тем же механизмом (`mp ssl issue <hostname>`); панель подхватывает его на лету через `tls.Config.GetCertificate`, до его получения — self-signed с отпечатком в выводе `mp setup`. Текущее состояние — `mp web tls` / `GET /web/tls`.
- Реализовано (этап 0): `internal/acme` (аккаунты в `<data>/acme/accounts/<directory>/<email>/`, сертификаты в `<data>/certs/<name>/`), задача `cert.issue` с проверкой публичного DNS и probe webroot через nginx, планировщик продления в API-процессе.
- Директория ACME настраивается (Let's Encrypt, ZeroSSL, Buypass, внутренний Step-CA).
- Сертификат панели и сертификаты сайтов разведены (2026-09-10). Сертификат панели — запись с именем `web.hostname`: `GET /ssl/panel` показывает, что панель отдаёт сейчас и что за этим стоит (заказ в работе, ошибка), `POST /ssl/panel/issue` заказывает ровно для этого имени (HTTP-01 или `dns` для DNS-01), `POST /ssl/panel/import` ставит готовый, `DELETE /ssl/panel` возвращает самоподписанный.
- Сертификат сайта заказывается из сайта: `POST /sites/{domain}/tls/issue` берёт домен и алиасы, переключает сайт на `ssl: auto`, и задача выпуска сама переприменяет сайт с HTTPS. В списке `/certificates` у каждого сертификата есть `used_by_panel` и `used_by_sites`; занятый сертификат удалить нельзя (409). Сертификат, который панель делит с одноимённым сайтом (панель на `example.com` и сайт `example.com`), — законный случай: он числится за обоими. Страница SSL в Web UI показывает три блока: панель, сайты, неиспользуемые.

## 8. Логи и ротация

- Логи сайтов — в `/var/log/monopanel/sites/<логин>/`, вне домашнего каталога: их открывают от root nginx, Apache и php-fpm.
- Ротация — `/etc/logrotate.d/monopanel-sites`, по два блока на аккаунт, от root (логи серверов — `create 0640 root <логин>`, журналы ошибок PHP — `create 0660 <логин> <логин>`): раз в неделю или раньше, если лог перевалил за 100 МБ, восемь копий, сжатие со второй; после ротации nginx и Apache аккаунта получают USR1 через свои pid-файлы. Подробно — в [разделе 2](#2-файловая-структура-на-сервере).
- Формат `main` пишет `$request_time`, `$upstream_response_time`, `$host` и `$server_protocol`. Разбор access-логов для метрик по сайтам (инкрементальный, с запоминанием позиции в файле) — планируется.
- В UI — хвост логов access/error/php-error/php-slow (вкладка «Логи» сайта, `mp site logs`); слежение за логом вживую и фильтры по коду ответа и пути — планируются.

## 9. Дополнительные веб-функции (флаги сайта, шаблоны)

- Редиректы: http→https, www↔non-www; произвольные 301/302 по пути — через свои директивы в `<domain>.d/` или `mp site nginx`.
- Алиасы сайта (общий docroot) и отдельные сайты на поддоменах.
- Свой docroot внутри `data/www/<domain>/` (например `/public` для Laravel и Symfony).
- Доступ только с перечисленных адресов (`--allow`); ограничение частоты запросов с одного адреса (`--rate-limit N` — `limit_req` на динамические страницы, статика не считается, лишним — 429); заголовки безопасности (`--security-headers`). Basic-auth на путь и блокировка по странам (geoip2 как динамический модуль) — планируются.
- IPv6: сайт слушает и на IPv6-адресе сервера (`--ipv6`; по умолчанию первый глобальный, `none` — только IPv4), у каждого адреса свой default-server. Адрес, который приходит от маршрутизатора (SLAAC) или по DHCPv6, появляется на интерфейсе позже, чем стартует nginx, и без подготовки nginx при загрузке падал с «Cannot assign requested address». Поэтому панель ставит `net.ipv6.ip_nonlocal_bind = 1` (`/etc/sysctl.d/90-monopanel.conf`) и добавляет юниту nginx drop-in с `Restart=on-failure`; default-server IPv6-адреса в первые минуты после загрузки не считается устаревшим.
- Кеш статики в браузере, `gzip`; `brotli` как динамический модуль собственной сборки — планируется.
- Заглушка «сайт приостановлен» при `status=suspended`: конфиг заменяется на минимальный с 503-страницей, пул FPM удаляется из конфига мастера.
- Режим `proxy` — сайт отдаётся своему приложению (Node, Python, Docker): `--mode proxy --backend http://127.0.0.1:3000`; фоновое приложение можно держать под systemd как app-сервис (`mp app`).

## 10. Установка CMS

`mp cms install <домен> <cms>` (и вкладка «CMS» на странице сайта) ставит WordPress, Joomla, OpenCart или 1С-Битрикс в существующий сайт так, как это документирует производитель, только без кликов:

1. Сайт получает пресет CMS, если он другой, и конфигурация применяется.
2. Docroot должен быть пуст; с `--force` его содержимое удаляется (от имени клиента).
3. Создаётся база `<login>_<cms>` (при повторной установке — `<login>_<cms>2`) с отдельным пользователем и сгенерированным паролем.
4. Дистрибутив скачивается у производителя (WordPress — wordpress.org, Joomla и OpenCart — последний релиз на GitHub без API, Битрикс — пробная редакция с 1c-bitrix.ru) и потоком, без буфера в памяти, распаковывается агентом через `tar` прямо в docroot (zip OpenCart перекодируется в tar на лету); файлы отдаются клиенту, метки SELinux восстанавливаются. Оборвавшуюся или застывшую загрузку панель продолжает с того байта, где она остановилась, если сервер отдаёт файл по частям (`Accept-Ranges`; `ETag` или `Last-Modified` уходят обратно в `If-Range`, так что изменившийся файл не склеится со старым), — до восьми раз; `tar` обрыва не замечает. Если распаковка всё же сорвалась, то, что успело распаковаться, сразу отдаётся клиенту: иначе эти файлы остались бы root-овыми, и их не смог бы удалить ни клиент, ни повторная установка с `--force`.
5. Установщик CMS запускается от имени клиента через `fsop run` — он исполняет только интерпретатор PHP (ветка сайта) внутри домашнего каталога: wp-cli (`/usr/local/lib/monopanel/wp-cli.phar`, проверяется по опубликованной sha512), `installation/joomla.php install`, `install/cli_install.php`. Пароли базы и администратора в командную строку не попадают — её видит любой локальный пользователь (`ps`): они приходят установщику на stdin, а короткий загрузчик (`php -r`) дописывает их в `$argv` и запускает сам скрипт. У Битрикса CLI-установщика нет: панель проходит его веб-мастер по HTTP через сам сайт (имя сайта резолвится в его адрес), включая AJAX-шаги установки модулей, обновлений и решения, и пробную регистрацию на 1c-bitrix.ru от имени администратора сайта. Редакция (`--edition`): пробные `start`, `standard`, `small_business`, `business` с 1c-bitrix.ru; лицензионный ключ вводится потом в настройках Битрикса, и редакция должна совпадать с ключом. Решение (`--solution`): по умолчанию `clean` — «Чистая установка «1С-Битрикс»» из Маркетплейса (пустой сайт со стандартными модулями, без демо-контента), `demo` — демо-сайт, встроенный в дистрибутив, или id любого решения из Маркетплейса (`vendor.solution`), которое мастер скачает и поставит; мастер решения панель проходит так же, как основной.
6. В записи сайта остаются CMS, версия и время установки; пароль администратора возвращается один раз в ответе API и не попадает в журнал задачи (в payload он зашифрован ключом панели).

Пароль администратора — 12–20 символов, чтобы устроить все четыре CMS сразу; без него генерируется 16-символьный. E-mail — владельца сайта, иначе `admin@<домен>`.

