# 03. Web stack: nginx + php-fpm and nginx + Apache

## 1. Site modes

### Mode A — `nginx → php-fpm` (default)

```
client ──HTTPS (h2/h3)──▶ nginx ──static files directly
                              └─ *.php ──FastCGI unix:/run/monopanel/php/<domain>.sock──▶ php-fpm pool (user: <user>)
```

### Mode B — `nginx → Apache → php-fpm`

```
client ──HTTPS──▶ nginx ──static files directly (by extension, can be switched off)
                      └─ the rest ──proxy_pass http://127.0.0.1:8080──▶ apache (mpm_event, .htaccess)
                                                                       └─ *.php ──mod_proxy_fcgi──▶ the same php-fpm pool
```

Switching the mode is a single field, `sites.mode`; the FPM pool, socket, user and php.ini do not change. Apache listens only on loopback (`127.0.0.1:8080`; on multi-IP servers loopback too — nginx passes `Host`). The client's real IP in Apache comes from `mod_remoteip` (`RemoteIPHeader X-Real-IP`, `RemoteIPInternalProxy 127.0.0.1`).

A limitation of mode B: `php_value` / `php_flag` directives in `.htaccess` work only with `mod_php`, which is not shipped. The replacement is the PHP settings in the panel (`php_admin_value` in the pool) and `.user.ini` in the docroot (supported by FPM through `user_ini.filename`). This is documented in the UI when the mode is chosen; the panel detects the usual `php_*` directives in `.htaccess` and offers to move them.

## 2. File layout on the server

```
/var/www/                                0711 root:root
/var/www/<user>/                         0710 <user>:<user>  + ACL g:monopanel-web:x   (traverse only)
└── data/                                0750
    ├── www/<domain>/                    docroot; default ACL g:monopanel-web:rX
    ├── logs/                            access/error/php-error/php-slow per site; ACL g:monopanel-web:x
    ├── tmp/                             0700; upload_tmp_dir / session.save_path / sys_temp_dir
    ├── bin/php -> /opt/monopanel/php/8.4/bin/php
    └── .ssh/ .composer/ .wp-cli/ …
```

The `monopanel-web` group = { `nginx`, `www-data` | `apache` }. The web servers read static files through the ACL, FPM sockets are created with `listen.group = monopanel-web`, `listen.mode = 0660`. Clients cannot see each other. Files that PHP creates (uploads, cache) belong to the client and inherit the default ACL — no manual `chown`/`chmod 777` is needed.

Site logs are rotated by `/etc/logrotate.d/monopanel-sites` — a block per account with `su <login> <login>`: the directory belongs to the client, and whatever the client puts there, logrotate acts with the client's rights, not root's. Weekly, or on the daily run if a log has grown past 100 MB; eight copies (`example.com.access.log.1`, `.2.gz` …), compressed from the second one on. All of a site's logs belong to the account, with mode 0660: the panel creates them when it applies the site, before nginx, Apache and php-fpm open them, hands the ones that already exist over to the account through the agent (by the open file, not by name), and after rotation `create 0660` makes the new ones the same. This is what logrotate before 3.19 (EL9) needs: when rotating it opens the log, and when compressing it opens it for writing as well, and the account could not open a file that nginx created as root or a slow log that php-fpm creates with mode 0600; nginx, when it reopens a log, changes only the owner, and php-fpm leaves a slow log it finds as it is. After rotation nginx gets USR1 and Apache a graceful restart, both through pid files (SELinux does not let logrotate ask systemd). After USR1 the new files are opened by the nginx worker processes, not the master, so the `monopanel-web` group can traverse `data/logs` (ACL `x`): without that the workers would keep writing to the renamed file — and would after the nightly rotation of nginx's own logs as well. The PHP error log and the php-fpm slow log reopen the file on every write, so they need no signal. php-fpm takes the trace for the slow log through `ptrace` on the worker process; on EL this is allowed by the panel's policy module ([02](02-platform-matrix.md#6-selinux-el9--el10)), and without it the slow logs had nothing in them.

Configs:

```
/etc/nginx/nginx.conf                          panel template (worker_processes auto, http{} with include)
/etc/nginx/monopanel/http.d/*.conf             global: ssl defaults, gzip/brotli, maps, limit zones, log format,
                                               ip-<ip>.conf — default-server per IP (acme, quic reuseport, 444 to unknown names)
/etc/nginx/monopanel/snippets/*.conf           snippets: fastcgi, static-cache, deny-dotfiles, acme, ssl, proxy-apache
/etc/nginx/monopanel/sites/<domain>.conf       the site's generated server{}
/etc/nginx/monopanel/sites/<domain>.d/*.conf   user includes inside server{} — never overwritten
/etc/nginx/conf.d/                             the administrator's free zone (the panel does not touch it)
/etc/apache2|httpd/monopanel/httpd.conf        Listen 127.0.0.1:8080, mpm_event, remoteip, global settings
/etc/apache2|httpd/monopanel/sites/<domain>.conf
/etc/apache2|httpd/monopanel/sites/<domain>.d/*.conf
(included with a single Include line from conf-enabled/ or conf.d/)

php-fpm pool: /etc/monopanel/php/X.Y/pool.d/<domain>.conf  (or the Sury/Remi layout — see 02 §5)
```

## 3. nginx template (mode A, abridged)

```nginx
# /etc/nginx/monopanel/sites/example.com.conf — generated by MonoPanel; edits go in example.com.d/
server {
    listen 203.0.113.10:80;
    server_name example.com www.example.com;
    include monopanel/snippets/acme.conf;              # /.well-known/acme-challenge/ → shared webroot
    {{if redirect_https}}return 301 https://$host$request_uri;{{else}}# …body, as in 443{{end}}
}
server {
    listen 203.0.113.10:443 ssl;
    http2 on;
    {{if http3}}listen 203.0.113.10:443 quic;{{end}}  # reuseport — in the default-server for this IP
    server_name example.com www.example.com;
    ssl_certificate     /var/lib/monopanel/certs/example.com/fullchain.pem;
    ssl_certificate_key /var/lib/monopanel/certs/example.com/privkey.pem;
    include monopanel/snippets/ssl.conf;               # TLS 1.2/1.3, ciphers, stapling, session cache, HSTS (flag)
    {{if http3}}add_header Alt-Svc 'h3=":443"; ma=86400';{{end}}
    {{if redirect_www}}# if ($host = www.example.com) { return 301 … }{{end}}

    root  /var/www/alex/data/www/example.com;
    index index.php index.html;
    access_log /var/www/alex/data/logs/example.com.access.log main;
    error_log  /var/www/alex/data/logs/example.com.error.log;
    client_max_body_size 64m;                          # = the pool's post_max_size
    disable_symlinks if_not_owner from=$document_root;

    include monopanel/snippets/deny-dotfiles.conf;     # .git, .env, .user.ini, .htaccess, composer.*
    include monopanel/snippets/static-cache.conf;      # expires for images/fonts/js/css
    include monopanel/sites/example.com.d/*.conf;      # user locations — before the common ones

    location / { try_files $uri $uri/ /index.php?$args; }
    location ~ \.php$ {
        try_files $uri =404;
        include monopanel/snippets/fastcgi.conf;       # fastcgi_params, buffers, timeouts = request_terminate_timeout
        fastcgi_pass unix:/run/monopanel/php/example.com.sock;
    }
}
```

Mode B differs in its location blocks: static files matched by an extension regex are served by nginx (the `static_by_nginx` switch), everything else goes to `proxy_pass http://127.0.0.1:8080` with the `proxy-apache.conf` snippet (`proxy_http_version 1.1`, `Host`, `X-Real-IP`, `X-Forwarded-Proto`, `X-Forwarded-For`, buffers, timeouts).

HTTP/3: `listen … quic reuseport` is allowed once per IP:port, so the panel keeps a separate default-server for each IP (`http.d/ip-<ip>.conf`) with `reuseport`, and sites declare `quic` without it. It also answers ACME challenges and closes connections for unknown domains — see the next paragraph.

The default server on each address (`http.d/ip-<address>.conf`) accepts ACME validations, redirects the panel's own name without a port to the panel's HTTPS port (`http://panel.example.com/` → `https://panel.example.com:8443/`), and closes everything else without a response (444). If the panel's name has a site of its own, the site wins: an exact `server_name` in nginx takes precedence over the default block. The files are regenerated when the API starts, so a change of `web.hostname` reaches nginx after `--restart`.

## 4. Apache template (mode B)

```apache
<VirtualHost 127.0.0.1:8080>
    ServerName example.com
    ServerAlias www.example.com
    DocumentRoot /var/www/alex/data/www/example.com
    <Directory /var/www/alex/data/www/example.com>
        AllowOverride All
        Options -Indexes +SymLinksIfOwnerMatch
        Require all granted
    </Directory>
    <FilesMatch "\.php$">
        SetHandler "proxy:unix:/run/monopanel/php/example.com.sock|fcgi://localhost"
    </FilesMatch>
    ProxyTimeout 150
    RemoteIPHeader X-Real-IP
    RemoteIPInternalProxy 127.0.0.1
    ErrorLog  /var/www/alex/data/logs/example.com.apache.error.log
    CustomLog /var/www/alex/data/logs/example.com.apache.access.log combined
    IncludeOptional /etc/apache2/monopanel/sites/example.com.d/*.conf
</VirtualHost>
```

Modules: `mpm_event, proxy, proxy_fcgi, rewrite, remoteip, headers, expires, setenvif, dir, alias, deflate, env, mime, authz_core, autoindex(off)`. `mpm_prefork` and any `php*` modules are disabled. `ProxyTimeout` = the pool's `request_terminate_timeout`.

## 5. php-fpm pool template

```ini
[example.com]
user  = alex
group = alex
listen = /run/monopanel/php/example.com.sock
listen.owner = alex
listen.group = monopanel-web
listen.mode  = 0660
pm = ondemand                    ; ondemand (default) | dynamic | static
pm.max_children = 8
pm.process_idle_timeout = 10s
pm.max_requests = 500
request_terminate_timeout = 150s ; max_execution_time + 30
slowlog = /var/www/alex/data/logs/example.com.php.slow.log
request_slowlog_timeout = 10s
catch_workers_output = yes
clear_env = no                   ; composer/wp-cli need PATH/HOME
env[PATH]   = /var/www/alex/data/bin:/usr/local/bin:/usr/bin:/bin
env[TMPDIR] = /var/www/alex/data/tmp
php_admin_value[open_basedir]      = /var/www/alex/data
php_admin_value[upload_tmp_dir]    = /var/www/alex/data/tmp
php_admin_value[session.save_path] = /var/www/alex/data/tmp/sess
php_admin_value[sys_temp_dir]      = /var/www/alex/data/tmp
php_admin_value[error_log]         = /var/www/alex/data/logs/example.com.php.error.log
php_admin_value[sendmail_path]     = /usr/sbin/sendmail -t -i -f noreply@example.com
php_admin_value[disable_functions] = passthru,shell_exec,system,proc_open,popen,pcntl_exec,pcntl_fork
; edited from the UI/CLI; php_value can be overridden in .user.ini
php_value[memory_limit]         = 256M
php_value[upload_max_filesize]  = 64M
php_value[post_max_size]        = 64M
php_value[max_execution_time]   = 120
php_value[date.timezone]        = Europe/Moscow
php_value[display_errors]       = Off
php_value[short_open_tag]       = Off   ; bitrix preset — On
php_value[opcache.enable]       = 1
```

The default `disable_functions` are listed above; the per-site `allow_exec` switch lifts them (WP-CLI hooks, Laravel queues, ImageMagick through the CLI).

By default PHP sessions are files in the account's `tmp/sess`. A site can be switched to the `sessions` Valkey instance of its account (`mp site set <domain> --sessions valkey`, the "PHP sessions" field in the site settings), and then, instead of a `session.save_path` pointing to files, the pool gets:

```ini
php_admin_value[session.save_handler] = redis
php_admin_value[session.save_path]    = "unix:///run/monopanel-valkey/alex-sessions/valkey.sock"
```

The site's PHP branch needs the redis extension. The socket, with mode 600, belongs to the account: the site's pool runs as the account and can connect, the pools of other accounts cannot ([02](02-platform-matrix.md#9-valkey-per-account)).

`pm.max_children` is 8 by default; the bitrix preset gets one worker per 256 MB of RAM, from 8 to 48 (the number is chosen when the site gets the preset: on creation, on a preset change or on a move): Bitrix templates make HTTP requests to their own site from inside a request, and eight busy workers end up waiting for each other. The branch's global `99-monopanel.ini` keeps `short_open_tag = Off`, but the copy the CLI reads turns short tags on while the branch has a site with the bitrix preset: Bitrix's cron scripts and prolog start with `<?`, and otherwise PHP prints them as text and exits with code 0. Pools set `short_open_tag` themselves, so on Remi, where the CLI and FPM read the same directory, this does not affect sites. `max_children` can be set by hand for any site; with `pm=ondemand` idle sites hold no workers — the main mode for a shared server with hundreds of sites. For busy sites — `dynamic`/`static`, set by hand.

## 6. Isolation and limits

| Level | Mechanism | Default |
|---|---|---|
| Files | a unix user per client, 0710, ACLs for the web group, `open_basedir`, separate tmp/session | on |
| PHP processes | an FPM pool running as the user, `disable_functions`, `pm.max_children` | on |
| Symlink | `disable_symlinks if_not_owner` (nginx), `SymLinksIfOwnerMatch` (Apache) | on |
| Disk | filesystem quotas per uid (`quota` / `xfs_quota`); without quotas — accounting by `du` in the metrics | optional |
| CPU/RAM per site | an isolated pool: a separate master `monopanel-php-fpm@<X.Y>-<domain>.service` in `Slice=monopanel-<user>.slice` with `MemoryMax`, `CPUQuota`, `TasksMax`, `PrivateTmp`, `ProtectSystem=strict`, `ReadWritePaths=/var/www/<user>` | optional (v1.1) |
| SSH | shell only by a flag; SFTP-only chroot (`Match Group monopanel-sftp`) | SFTP |
| Network | outgoing PHP connections are not restricted (APIs, payments); a "block outgoing SMTP" option through an nft rule per uid (`meta skuid`) | optional |

## 7. TLS / ACME

- Client: `lego` as a library inside `monopaneld api`: account, keys, challenges, renewal.
- HTTP-01: a shared webroot `/var/lib/monopanel/acme/webroot`, included through the `acme.conf` snippet in every `:80` server{} and in the default-server of each IP → issuance works both before the site is created and with a redirect to HTTPS.
- DNS-01: lego providers (Cloudflare, Route53, Hetzner, Yandex Cloud, DigitalOcean, RFC2136 and others) → wildcard `*.example.com`; credentials are stored encrypted, per user.
- Keys: ECDSA P-256 by default, RSA-2048 by a flag; optionally a dual certificate (EC + RSA).
- Storage: `/var/lib/monopanel/certs/<domain>/{fullchain.pem,privkey.pem,chain.pem}` 0600 root (the nginx/Apache master processes run as root). Importing your own certificates (PEM/PFX) — through the UI/CLI/API; a certificate can be shared by several sites (SAN).
- Renewal: a scheduler once a day, 30 days before expiry; on failure — a notification and retries with backoff; on success — `ApplyConfigSet` with only a reload of nginx (and Apache if needed).
- Optionally the native `ngx_http_acme_module` (nginx 1.29+) instead of lego for HTTP-01; behind a flag until it stabilises.
- Panel host: a certificate for `web.hostname` through the same mechanism (`mp ssl issue <hostname>`); the panel picks it up on the fly through `tls.Config.GetCertificate`, and until it has one — a self-signed certificate with its fingerprint in the `mp setup` output. Current state: `mp web tls` / `GET /web/tls`.
- Implemented (stage 0): `internal/acme` (accounts in `<data>/acme/accounts/<directory>/<email>/`, certificates in `<data>/certs/<name>/`), the `cert.issue` job with a public DNS check and a webroot probe through nginx, the renewal scheduler in the API process.
- The ACME directory is configurable (Let's Encrypt, ZeroSSL, Buypass, an internal Step-CA).
- The panel certificate and the site certificates are kept apart (2026-09-10). The panel certificate is the record named `web.hostname`: `GET /ssl/panel` shows what the panel serves now and what is behind it (an order in progress, an error), `POST /ssl/panel/issue` orders one for exactly that name (HTTP-01, or `dns` for DNS-01), `POST /ssl/panel/import` installs a ready one, `DELETE /ssl/panel` goes back to self-signed. A site certificate is ordered from the site: `POST /sites/{domain}/tls/issue` takes the domain and its aliases, switches the site to `ssl: auto`, and the issuance job re-applies the site with HTTPS itself. In the `/certificates` list every certificate has `used_by_panel` and `used_by_sites`; a certificate in use cannot be deleted (409), and a certificate the panel shares with a site of the same name (the panel on `example.com` and the site `example.com`) is a legitimate case: it is listed under both. The SSL page in the web UI shows three blocks: "Panel", "Sites", "Not in use".

## 8. Logs and rotation

- Rotation — `/etc/logrotate.d/monopanel-sites`, a block per account with `su <login> <login>`: weekly, or sooner if a log has grown past 100 MB; eight copies, compressed from the second one on; after rotation nginx gets USR1 and Apache a graceful restart, both through their pid files. In detail — [section 2](#2-file-layout-on-the-server).
- Access logs are analysed for metrics incrementally (the file position is remembered), in the `main` format with `$request_time`, `$upstream_response_time`, `$host`, `$server_protocol`.
- In the UI: tail view and follow (SSE) for access/error/php-error/php-slow, filtering by response code and path.

## 9. Additional web features (site flags, templates)

- Redirects: http→https, www↔non-www, arbitrary 301/302 by path, an include for complex rules.
- Site aliases/subdomains (shared docroot) and separate sites on subdomains.
- A custom docroot inside `data/www/<domain>/` (for example `/public` for Laravel/Symfony).
- Basic auth per path; blocking by IP/CIDR/country (geoip2 as a dynamic module, optional); `limit_req` per site.
- Browser caching of static files, `gzip` (+ `brotli` as a dynamic module of our own build, optional).
- A "site suspended" placeholder for `status=suspended`: the config is replaced with a minimal one serving a 503 page, and the FPM pool is removed from the master's config.
- `proxy` mode (site → an arbitrary backend: Node/Python/Docker) — roadmap.

## CMS installation

`mp cms install <domain> <cms>` (and the "CMS" tab on the site page) installs WordPress, Joomla, OpenCart or 1C-Bitrix into an existing site the way the vendor documents it, only without the clicking:

1. The site gets the CMS preset, if it has a different one, and the configuration is applied.
2. The docroot must be empty; with `--force` its contents are deleted (as the client).
3. A database `<login>_<cms>` is created (on a repeat install — `<login>_<cms>2`) with its own user and a generated password.
4. The distribution is downloaded from the vendor (WordPress from wordpress.org, Joomla and OpenCart as the latest release on GitHub, without the API, Bitrix as the trial edition from 1c-bitrix.ru) and, streamed without buffering in memory, unpacked by the agent with `tar` straight into the docroot (the OpenCart zip is re-encoded to tar on the fly); the files are handed over to the client and the SELinux labels are restored.
5. The CMS installer runs as the client through `fsop run`, which executes only the PHP interpreter (the site's branch) inside the home directory: wp-cli (`/usr/local/lib/monopanel/wp-cli.phar`, verified against the published sha512), `installation/joomla.php install`, `install/cli_install.php`. Bitrix has no CLI installer: the panel walks through its web wizard over HTTP via the site itself (the site's name is resolved to its address), including the AJAX steps that install the modules, updates and the solution, and the trial registration on 1c-bitrix.ru on behalf of the site administrator. Edition (`--edition`): the trial `start`, `standard`, `small_business`, `business` from 1c-bitrix.ru; the license key is entered later in the Bitrix settings, and the edition must match the key. Solution (`--solution`): by default `clean`, the Marketplace's "1C-Bitrix clean install" (an empty site with the standard modules and no demo content); `demo`, the demo site bundled with the distribution; or the id of any Marketplace solution (`vendor.solution`), which the wizard downloads and installs; the panel walks through the solution's wizard the same way as through the main one.
6. The site record keeps the CMS, its version and the time of installation; the administrator password is returned once in the API response and never gets into the job log (in the payload it is encrypted with the panel's key).

The administrator password is 12–20 characters, to satisfy all four CMSs at once; without one, a 16-character password is generated. The e-mail is the site owner's, otherwise `admin@<domain>`.

