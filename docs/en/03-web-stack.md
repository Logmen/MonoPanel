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
                      └─ the rest ──proxy_pass http://127.100.x.y:81──▶ apache of the account (mpm_event, .htaccess, as the account)
                                                                       └─ *.php ──mod_proxy_fcgi──▶ the same php-fpm pool
```

Switching the mode is a single field, `sites.mode`; the FPM pool, socket, user and php.ini do not change. Every account with sites in mode B has an Apache of its own, `monopanel-apache@<login>.service`: its root parent listens on the account's address, `127.<100+uid>>16>.<(uid>>8)&255>.<uid&255>:81` (`127.100.3.233:81` for uid 1001), and its children run as the account and read only what it reads. Only root may bind a port below 1024, and only this Apache binds that address, so no other process takes its sites' requests, not even while it restarts; nginx passes `Host`. The client's real IP in Apache comes from `mod_remoteip` (`RemoteIPHeader X-Real-IP`, `RemoteIPInternalProxy 127.0.0.1`). Mode B exists on Debian and Ubuntu only; the panel refuses to install Apache on a host where `net.ipv4.ip_unprivileged_port_start` is 81 or lower — there any account could take the Apaches' port.

Only nginx talks to an account's Apache. Every request nginx sends to the account's Apache (`127.100.x.y:81`) carries that account's secret, the `X-MonoPanel-Backend` header from `/etc/nginx/monopanel/apache/<login>.conf`; the Apache refuses anything without it with 403 (`/etc/apache2/monopanel/accounts/<login>/backend.conf`, which its `httpd.conf` includes at the top level). Without this, any local process connecting to the address directly would bypass the site's `--allow` list and rate limit and choose the client address itself (`X-Real-IP` from `127.0.0.1`) that `.htaccess` (`Require ip`, `Allow from`) and PHP see. Both files are root 0600. The secret is 32 random bytes; the panel creates it when it first sets up the account's Apache, keeps it in its database encrypted with `/etc/monopanel/secret.key`, and removes it with that Apache. The refusal is a server-level mod_rewrite rule: it runs when the URL is mapped, before any `.htaccess` is read, and every virtual host inherits it ahead of its own rules, so a site's `.htaccess` cannot undo it — neither `Satisfy Any` nor a nested `<If>`. A second line is an `<If>` in `<Location "/">` (`Require all denied`, `Satisfy All`). PHP never sees the header (`ProxyFCGISetEnvIf`). An account can learn its own secret (its `.htaccess` can copy the header into a response), but that opens only its own Apache. In detail: [§4](#4-apache-template-mode-b).

A limitation of mode B: `php_value` / `php_flag` directives in `.htaccess` work only with `mod_php`, which is not shipped. The replacement is the PHP settings in the panel (`php_admin_value` in the pool) and `.user.ini` in the docroot (supported by FPM through `user_ini.filename`). A hint about this when the mode is chosen, and detection of `php_*` directives in `.htaccess` with an offer to move them into the panel, are planned.

## 2. File layout on the server

```
/var/www/                                0711 root:root
/var/www/<user>/                         0710 <user>:<user>  + ACL g:monopanel-web:x   (traverse only)
└── data/                                0750
    ├── www/<domain>/                    docroot; default ACL g:monopanel-web:rX
    ├── tmp/                             0700; upload_tmp_dir / session.save_path / sys_temp_dir
    ├── bin/php -> /opt/monopanel/php/8.4/bin/php
    └── .ssh/ .composer/ .wp-cli/ …
/var/www/.shares/                        0711 root:root          shared folders of guests with a shell ([10](10-shared-folders.md))
└── <guest>/                             0750 root:<guest>
    └── <name>/                          0755 root:root          the mount point of a site folder; the guest's home has a symlink to it
/var/www/.restore/                       0700 root:root          staging of in-place restores (the agent alone; from the first such restore)

/var/log/monopanel/                      1771 root:monopanel        (log_dir; the panel's audit.log is here too; sticky bit)
└── sites/                               0711 root:root             traverse, no listing
    └── <user>/                          0751 root:<user>           the client reads through its group
        ├── <domain>.access.log          0640 root:<user>           nginx; after USR1 owned by its user
        ├── <domain>.error.log           0640 root:<user>
        ├── <domain>.apache.{access,error}.log  0640 root:<user>    apache mode
        ├── apache.error.log             0640 root:<user>           the log of the account's Apache itself
        ├── <domain>.php.slow.log        0640 root:<user>           written by the php-fpm master
        ├── <domain>.php.error.log       0660 <user>:<user>         written by PHP itself, as the client
        └── webmail.{access,error}.log   0640 root:<user>           the webmail on its own port

/run/monopanel/                          1771 root:monopanel        (run_dir; the API's and the agent's sockets; sticky bit)
└── php/                                 0755 root:root             the FPM pools' sockets; from boot (the package's and mp setup's tmpfiles)
```

The `monopanel-web` group = { `nginx` }: nginx reads static files through the ACL, FPM sockets are created with `listen.group = monopanel-web`, `listen.mode = 0660`. The account's Apache runs as the account and reads its files as the account itself; the panel takes the distribution's Apache account (`www-data`) out of the group. Clients cannot see each other. Files that PHP creates (uploads, cache) belong to the client and inherit the default ACL — no manual `chown`/`chmod 777` is needed.

Site logs do not live in the home but in `/var/log/monopanel/sites/<login>/` (`log_dir` in `config.yaml`). They are opened as root by the nginx master (at start, reload and USR1; when it reopens a log it also changes the log's owner by name), the parent process of the account's Apache and the php-fpm master (the slow log, on every write). Up to and including 0.8.19 the logs were in `data/logs`, and that directory and `data/` above it belong to the client: by swapping a log for a symlink to `/etc/ld.so.preload` or `/etc/cron.d/…`, the client or any PHP code of its sites made the servers write there as root — and so got root. In `sites/` only root creates, renames and removes entries. The client reads its logs through its group (over SSH; an SFTP-only account is locked in its home and reads them in the panel), the panel and `mp site logs` through the agent.

The agent makes the directories and the files before nginx, Apache and php-fpm open them (nginx creates a missing log itself, but not a missing directory, and php-fpm does not start when it cannot open its slow log). It walks the tree through opened directories without following symlinks, refuses to work in a directory that anyone but root may write to, changes owner and mode through the opened file, and decides by the name whom a file belongs to. The servers' logs (nginx, Apache, the php-fpm slow log) are `root:<login>` 0640; nginx, when it reopens a log, hands it to its own user but keeps the group and the mode, so the client can still read it. The PHP error log is written by PHP itself, as the client, which opens the file on every write, so it belongs to the client: `<login>:<login>` 0660; the client still cannot replace it, the directory is not the client's. The servers write logs only as files right in `sites/<login>/`: confcheck refuses a deeper path (`sites/<login>/<dir>/…`) or one right in `sites/`, custom nginx directives included.

Site logs are rotated by `/etc/logrotate.d/monopanel-sites`: two blocks per account, as root, without `su` — the directory belongs to root, and nothing the client controls is in logrotate's way. Weekly, or on the daily run if a log has grown past 100 MB; eight copies (`example.com.access.log.1`, `.2.gz` …), compressed from the second one on. The first block is the servers' logs with `create 0640 root <login>`, the second the PHP error logs with `create 0660 <login> <login>`. logrotate refuses a file that two blocks name, so instead of `*.error.log` the first block has four masks that take every error log but `*.php.error.log`.

After rotation nginx and the account's Apache get USR1 (a graceful restart for Apache), both through their pid files, `/run/nginx.pid` and `/run/monopanel-apache/<login>/httpd.pid` (SELinux does not let logrotate ask systemd). After USR1 the new files are opened by the nginx worker processes, not the master, so `/var/log/monopanel` and the account's directory can be traversed by everyone (`x` without the right to list); without that the workers would keep writing to the renamed file. The PHP error log and the php-fpm slow log reopen the file on every write, so they need no signal.

A panel upgraded from 0.8.19 or earlier lists, at its first start, the sites whose configuration still writes the logs into `data/logs` (`sites.moves`: active ones, those in error and pending ones — a site in error applied again whose job the upgrade interrupted may be one; a suspended site has no logs there), makes the directories and logs of their accounts, and queues `site.apply` for each (and `mail.apply` when the webmail is published on its own port). A site leaves the list with the first `site.apply` that succeeds, whoever queued it. A move that failed is queued again at every start and once an hour, but not before 10 minutes after the first failure, then four times as long after each further one, at least once a day; `mp doctor` (its `site logs` line) names such sites and the reason. Until a site has moved, its logs in `data/logs` are not rotated and fail2ban does not read them: the agent accepts logrotate stanzas for the log tree only (and no `su`), and fail2ban log paths in `/var/log` and the tree only, because root is not to work on files in a client's directory. The fail2ban jails watch such a site's logs in the tree from when the move made them — empty until the site moves — so the move changes the jails once, not once a site: otherwise fail2ban would restart for every site and, on a big server, hit systemd's limit on frequent starts. A site log the jails list that appears after fail2ban started (a site suspended under 0.8.19, unsuspended) restarts a running fail2ban once, so that it reads the file. The old files in `data/logs` stay where they are: they are the client's files now, nothing rotates them, and once the site has moved nothing writes or shows them. Until a site has moved (its `site.apply` after the upgrade failed — the `site logs` line of `mp doctor`), its servers write the old files, and its log tail (the "Logs" tab, `mp site logs`) shows the one of the two files written last: `~/data/logs/<file>` (the account reads it, not root), or the tree's file when it holds lines written after the old one's — after a partial apply in apache mode Apache already writes into the tree while nginx still writes into `data/logs`. On EL the tree gets the `httpd_log_t` label (an `fcontext` rule for `/var/log/monopanel/sites(/.*)?`, [02](02-platform-matrix.md#6-selinux-el9--el10)); until the rule is in place the sites and the webmail on its port are not switched and stay on their old paths — nginx and php-fpm in `httpd_t` could not open logs labelled `var_log_t`. If `semanage` failed, the panel tries the rule again every hour and before it applies a site or the mail settings.

Up to 0.8.19 `/var/log/monopanel` belonged to the panel's user. The package script leaves such a directory to the agent, which takes it over at its start through the opened directory — the owner first, then the ACLs go, then the mode 1771 — and only then makes or trusts `sites/`. A self-update installs the same package through apt-get or dnf, so the package script runs there too. A `sites/` that the panel's user of 0.8.19 may have left there is taken over by the agent at its start too, and cleared: directories that do not belong there go to `/var/log/monopanel/untrusted-<time>-<random>/`, and symlinks, FIFOs and files with a second name are removed. `mp setup` and `sitelogs/ensure` leave such a directory alone and say to restart the agent: a directory handed to root by hand would keep what was left in it. The agent prepares the pools' socket directory `<run_dir>/php` at its start the same way ([01 §7](01-architecture.md#7-security)).

php-fpm takes the trace for the slow log through `ptrace` on the worker process; on EL this is allowed by the panel's policy module ([02](02-platform-matrix.md#6-selinux-el9--el10)), and without it the slow logs had nothing in them.

Configs:

```
/etc/nginx/nginx.conf                          panel template (worker_processes auto, http{} with include)
/etc/nginx/monopanel/http.d/*.conf             global: ssl defaults, gzip/brotli, maps, limit zones, log format,
                                               ip-<ip>.conf — default-server per IP (acme, 444 and a refused TLS handshake to unknown names, quic reuseport)
/etc/nginx/monopanel/snippets/*.conf           snippets: fastcgi, static-cache, deny-dotfiles, acme, ssl, proxy-apache
/etc/nginx/monopanel/sites/<domain>.conf       the site's generated server{}
/etc/nginx/monopanel/sites/<domain>.d/*.conf   user includes inside server{} — never overwritten
/etc/nginx/monopanel/apache/<user>.conf        the account's Apache secret nginx sends it (root 0600)
/etc/nginx/conf.d/                             the administrator's free zone (the panel does not touch it)
/etc/apache2/monopanel/accounts/<user>/httpd.conf               the account's Apache, all of it: User/Group, Listen, modules, MPM, logs
/etc/apache2/monopanel/accounts/<user>/backend.conf             the check of nginx's secret (root 0600)
/etc/apache2/monopanel/accounts/<user>/sites/<domain>.conf      the site's virtual host
/etc/apache2/monopanel/accounts/<user>/sites/<domain>.d/*.conf  the site's custom directives inside <VirtualHost> — never overwritten
/run/monopanel-apache/<user>/                                  pid file and locks of the account's Apache (the unit's RuntimeDirectory)

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
    access_log /var/log/monopanel/sites/alex/example.com.access.log main;
    error_log  /var/log/monopanel/sites/alex/example.com.error.log;
    client_max_body_size 64m;                          # = the pool's post_max_size
    disable_symlinks if_not_owner from=$document_root;

    include monopanel/snippets/deny-dotfiles.conf;     # .git, .env, .user.ini, .htaccess, composer.*
    include monopanel/snippets/static-cache.conf;      # expires for images/fonts/js/css
    include monopanel/sites/example.com.d/*.conf;      # user locations — before the common ones

    location ~ ^/[^/]+\.php(?:/|$) {                  # a script at the docroot's root, a path after its name included
        if (!-f $document_root$fastcgi_script_name) { return 404; }
        include monopanel/snippets/fastcgi.conf;
        fastcgi_pass unix:/run/monopanel/php/example.com.sock;
    }
    location / { try_files $uri $uri/ /index.php?$args; }
    location ~ \.php$ {
        try_files $uri =404;
        include monopanel/snippets/fastcgi.conf;       # fastcgi_params, buffers, timeouts = request_terminate_timeout
        fastcgi_pass unix:/run/monopanel/php/example.com.sock;
    }
}
```

A script at the docroot's root takes a path after its name (PATH_INFO): Roundcube 1.7 serves its CSS and JS as `/static.php/skins/…`, other front controllers take `/index.php/route`. This `location` comes before the static files' one, which would otherwise answer `/static.php/…/styles.css` with 404. A path after a script deeper down — in an uploads folder, say — still goes through `try_files` and ends at `index.php`. The CMS presets do not get this block: their rules that close particular scripts are written for `\.php$`. If you close a root-level script in your own `<domain>.d/*.conf`, end the rule with `(?:/|$)` rather than `$`, or `/script.php/x` gets around it.

`fastcgi_pass` in an nginx configuration may only name a site's pool socket, `unix:/run/monopanel/php/<domain>.sock` (the directory is `run_dir` from `config.yaml`); the agent refuses TCP addresses, other unix sockets (the stock pools `/run/php/…`, `/var/opt/remi/…/www.sock`, `/run/php-fpm/www.sock`), upstream names and variables. It also refuses `fastcgi_param PHP_VALUE`/`PHP_ADMIN_VALUE` (php-fpm applies them to the pool's worker as ini lines, and `extension=` loads any library; a site's PHP settings are `mp site set --ini`). The first bytes nginx sends to a socket must not be the client's choice: `proxy_method` is a single word of letters, digits, `-` and `_` that starts with a letter (no variable, never empty); `uwsgi_modifier1` stays 0; a `stream` block holds no `include`, no `proxy_pass unix:…` or one with a variable, and no `unix:` server in an `upstream`. Otherwise a client's request would speak FastCGI to any socket nginx's user can open. An `include` names files in `/etc/nginx` other than `/etc/nginx/default.d` (the distribution's PHP locations there pass requests to its own pools), with a wildcard (`*`, `?`, `[`) in the file name only and no backslash: nginx expands patterns with glob(3), which reads `\.\.` and `.*` in a directory as `..`. A `custom.conf` written earlier with such lines keeps working, but can only be written again without them.

Mode B differs in its location blocks: static files matched by an extension regex are served by nginx (the `static_by_nginx` switch), everything else goes by `proxy_pass` to the address of the account's Apache (`http://127.100.3.233:81` for uid 1001) with the `proxy-apache.conf` snippet (`proxy_http_version 1.1`, `Host`, `X-Real-IP`, `X-Forwarded-Proto`, `X-Forwarded-For`, buffers, timeouts). Both locations that proxy to the account's Apache include `monopanel/apache/<login>.conf` right after the snippet: a level that sets any `proxy_set_header` loses the inherited ones, so the secret sits next to the snippet's headers. The agent refuses a location that proxies to an account's Apache (`proxy_pass http://127.100.x.y:81…`) without that include next to `proxy_pass` — the Apache would answer it 403; the secret's file is included only in a location whose one `proxy_pass` leads to that same account's Apache, with no block inside it that proxies itself. This holds for a site's custom directives (`mp site nginx --set`) and for an override of `nginx/site.conf.tmpl` in `/etc/monopanel/templates`: an override from 0.8.19 needs `include {{ .ApacheBackendConf }};` in both Apache-mode locations, or the site does not move to its own Apache on upgrade (the job names it).

HTTP/3: `listen … quic reuseport` is allowed once per IP:port, so the panel keeps a separate default-server for each IP (`http.d/ip-<ip>.conf`) with `reuseport`, and sites declare `quic` without it. QUIC runs over UDP 443: applying a site with HTTP/3 opens it in the panel's firewall or a running firewalld (and closes what it opened once no site serves HTTP/3; in firewalld both the permanent and the runtime rule, while an admin's rule stays), and `mp doctor` warns while it is closed or open in firewalld until the next reload only. The provider's firewall and filters in front of the server are beyond the panel — UDP 443 is opened there separately, or browsers stay on HTTP/2. It also answers ACME challenges and closes connections for unknown domains — see the next paragraph.

The default server on each address (`http.d/ip-<address>.conf`) accepts ACME validations, redirects the panel's own name without a port to the panel's HTTPS port (`http://panel.example.com/` → `https://panel.example.com:8443/`), and closes everything else without a response (444). On 443 it (since 0.9.3) refuses the TLS handshake of a name no site has and of a client without SNI (`ssl_reject_handshake on`, no certificate needed): nginx would otherwise answer them with the address's first site in file order, certificate and all. A reverse proxy that connects to the server by IP (nginx with `proxy_pass https://<address>` sends no SNI by default) does not get through that way: for it an administrator picks a site of the address — `mp site set <domain> --default-tls` (the "Clients without SNI" switch on the site page). A client without SNI then gets that site's certificate, its `Host` header picks the site (the proxy has to pass the site's name, `proxy_set_header Host $host`), a `Host` that is the address itself is redirected to the chosen site, and a name no site has still gets a refused handshake when it comes as SNI. One such site per address; choosing another takes it off the previous one, and a suspended site or one without a certificate serves no clients without SNI. It holds the QUIC socket with `reuseport` (nginx takes it once per address and port) while any site serves HTTP/3 (on, with a certificate, not suspended), and lets it go as soon as none does (0.9.5; before, only at the API's next start). If the panel's name has a site of its own, the site wins: an exact `server_name` in nginx takes precedence over the default block. The files are regenerated when the API starts, so a change of `web.hostname` reaches nginx after `--restart`.

## 4. Apache template (mode B)

A site's virtual host in its account's Apache (the account `alex`, uid 1001; the `<Directory>` block of the folder `upload/exchange` appears once it is handed to a guest as a shared folder with `--no-php`):

```apache
# /etc/apache2/monopanel/accounts/alex/sites/example.com.conf
<VirtualHost 127.100.3.233:81>
    ServerName example.com
    ServerAlias www.example.com
    RewriteEngine On                     # without it the host does not inherit nginx's secret check
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

The account's `httpd.conf` (`/etc/apache2/monopanel/accounts/<login>/httpd.conf`) is the whole configuration the unit starts `apache2 -f … -DFOREGROUND` with, without the distribution's `apache2.conf`, `envvars` and `mods-enabled`:

- `User` and `Group` are the account's login, `Listen` is its address on port 81, the pid file, the locks and `DefaultRuntimeDir` are in `/run/monopanel-apache/<login>/`, and the Apache's own log is `apache.error.log` in the account's log directory;
- `RemoteIPHeader X-Real-IP` and `RemoteIPInternalProxy 127.0.0.1` take the visitor's address from nginx;
- `<Directory />` is closed (`AllowOverride None`, `RewriteEngine Off`, `Require all denied`), and in the account's home `.htaccess` may do anything (`AllowOverride All`, `Options -Indexes +SymLinksIfOwnerMatch`): what the children do for a `.htaccess` they do as the account, and neither `ExecCGI` nor a symlink to someone else's file gives them more than the account has;
- two `<LocationMatch>` blocks with `Require all denied` — the same expressions as nginx's `snippets/deny-dotfiles.conf` (dotfiles and dot directories except `.well-known` and `.default`; `composer.json/lock`, `package*.json`, `yarn.lock`, `.user.ini`, `.htaccess`, `.htpasswd`): nginx never sends such requests to the Apache, but any local process can connect to its address;
- `Include /etc/apache2/monopanel/accounts/<login>/backend.conf` at the top level, the check of nginx's secret (below);
- last, `IncludeOptional …/sites/*.conf`, the virtual hosts of the account's sites.

Modules — these and no others: `mpm_event, authz_core, authz_host, authz_user, authn_core, authn_file, auth_basic, access_compat, alias, dir, mime, env, headers, expires, rewrite, setenvif, filter, deflate, remoteip, proxy, proxy_fcgi, negotiation, reqtimeout, autoindex` (a directory listing only where a site turns it on itself). No CGI, no server-side includes (`include`), no Lua and no `ext_filter`, which run programs, and no `status` or `info`; `mod_php` is not installed. MPM: one child of 16 threads while idle, four at most (`StartServers 1`, `ThreadsPerChild 16`, `MaxRequestWorkers 64`) — nginx serves the static files and keeps the slow clients. `ProxyTimeout` = the pool's `request_terminate_timeout`.

The agent takes these files in this shape alone, and only for a hosting account: `User`, `Group`, `Listen`, the pid file, the locks, `ServerRoot` and the modules are set by `httpd.conf` alone, and only to that account's values; `httpd.conf` includes only its own `backend.conf` and `sites/*.conf`, a virtual host only its own `sites/<domain>.d/*.conf`, and custom directives include nothing; no `Define` is taken; logs go right into that account's log directory or to syslog only. `apache2 -t -f <the account's httpd.conf>` checks it all before a reload.

The check of nginx's secret is the account's `backend.conf` (root 0600, the secret 64 hex digits), which `httpd.conf` includes at the top level:

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

A virtual host inherits the server's rules only when it switches mod_rewrite on itself, hence `RewriteEngine On` in every `<VirtualHost>`; `<Directory />` switches it off again for the directories, so a `.htaccess` without its own `RewriteEngine On` behaves as before. Authorization alone (`Require` in an `<If>`) does not hold against a site's `.htaccess`: `Satisfy Any` (mod_access_compat) lets a request past every `Require` once `Allow` passes — by default, or by a forged `X-Real-IP` — and a nested `<If>` in `.htaccess` merges after any `<If>` of `httpd.conf`. `[NV]` and `req_novary` keep `Vary: X-MonoPanel-Backend` out of responses; `RequestHeader unset` does not work: an internal redirect (an `.htaccess` front-controller `RewriteRule`, a local `ErrorDocument`) is checked again on the same headers and would get 403. `ProxyFCGISetEnvIf` takes the header out of what PHP gets, and with it `SCRIPT_URI` and `SCRIPT_URL` (and their `REDIRECT_…` copies): mod_rewrite adds them once a virtual host switches it on, PHP never got them up to 0.8.19, and `SCRIPT_URI` would say `http` on an HTTPS site too — nginx speaks http to the account's Apache.

In the account's Apache files the panel writes, the agent refuses server-level mod_rewrite directives (they belong to `backend.conf` alone), a `<VirtualHost>` without `RewriteEngine On`, `RewriteEngine Off` outside `<Directory>`, `<Files>` and `<Proxy>`, `RewriteOptions Inherit` or `IgnoreInherit` at a virtual host's level (use `InheritBefore` instead of `Inherit`), and an `<If>` inside a `<Location>` or another `<If>`: such an `<If>` would merge after the check's. `backend.conf` and nginx's secret file it takes in the panel's form alone. An override of `apache/site.conf.tmpl` must keep `RewriteEngine On` right inside the `<VirtualHost>`. Custom directives an administrator writes into `sites/<domain>.d/` by hand are not checked by the agent: a `RewriteEngine Off` at the virtual host's level there removes the main check for that site.

A site comes to its account's Apache before nginx starts sending it there and leaves it after it has left nginx, without a break (in detail: [01 §6](01-architecture.md#6-configuration-apply-pipeline)). A new Apache demands nginx's secret from its first start. An Apache left without sites is stopped and disabled, and nginx's secret file and the secret itself are removed. A site's custom directives (`sites/<domain>.d/`) live as long as the site does: a mode change or a suspension leaves them alone. The `monopanel-apache@.service` template comes with the package and stops gracefully (`KillSignal=SIGWINCH`, `KillMode=mixed`, `TimeoutStopSec=60`): requests that nginx's old workers still have in flight after a site moves away get up to a minute to finish. On a package downgrade below 0.9.0 the preremove copies it to `/etc/systemd/system`, so the accounts' Apaches survive a reboot ([README, Going back to an older version](../../README.en.md#going-back-to-an-older-version)), and the next upgrade removes the copy.

Upgrading from 0.8.19. Before 0.9.0 one `apache2.service` of the distribution served every site in mode B, as `www-data` on `127.0.0.1:8080`: a site's `.htaccess` could point PHP at another account's pool and set its `php_admin_value`s, read other accounts' files through a symlink and show the server-status of every site, and while Apache restarted any local account could take the port. The upgraded panel moves the sites to their accounts' Apaches by itself, one at a time and without downtime (the `apache.migrate` job), with their custom directives from the old `sites/<domain>.d/` — also those of sites that left apache mode. The agent reads the directives (`apache/legacy`, [01 §3.2](01-architecture.md#32-monopaneld-agent--the-privileged-process)), and they go over no more open than they were: one only root could read stays so with the account's Apache. A site that moved to its account's Apache leaves the shared Apache at once: once nginx is switched, its virtual host in `/etc/apache2/monopanel/sites/` is removed (the shared Apache is not reloaded for it). While some site cannot move and the shared Apache keeps running, it opens logs in `data/logs` only for the sites still on it. Once all of them have moved, `apache2.service` is stopped and disabled, the files of the old shared configuration are removed and `www-data` leaves the `monopanel-web` group. A site that did not move keeps the shared Apache running, and the job names it. What may stand in the way: custom directives the agent does not take for an account's Apache — an `Include`, a log in another directory, `APACHE_*` variables, which nothing defines there, and at the virtual host's level (not in a `<Directory>`/`<Files>`) `RewriteEngine Off` and `RewriteOptions Inherit`/`IgnoreInherit`, which would remove nginx's secret check; a directive file the agent does not read (a symlink, a hard link, a file over 1 MiB); template overrides without the secret's check — `nginx/site.conf.tmpl` without `include {{ .ApacheBackendConf }};` and `apache/site.conf.tmpl` without `RewriteEngine On` in the `<VirtualHost>`. Fix them (move `RewriteEngine Off` into a `<Directory>`, use `InheritBefore` instead of `Inherit`) and run `mp stack install apache`: a move that failed does not repeat at every start, the next try waits as for the logs ([§2](#2-file-layout-on-the-server)), and the `service apache2.service` line of `mp doctor` says where it stands. Accounts' Apaches set up without the secret (as 0.9.0's development builds made them before it) the panel switches to it once after the move (the `apache.backend` job): nginx starts sending the secret for every site of the account first, then the Apache starts demanding it, with no 403; until then `mp doctor` names such an Apache in a `backend monopanel-apache@<login>.service` line. While the move has not finished, `mp doctor` does not check the Apache of an account none of whose sites has moved yet (it has no `httpd.conf` yet), and an `apache move` line names the sites on the shared `apache2.service`; when that shared Apache is stopped meanwhile, its `service apache2.service` line is a failure: those sites answer 502, and it wants starting (`systemctl start apache2.service`), not disabling. `mp stack upgrade apache` (and the daily upgrade) checks the shared configuration first then (`apache2ctl -t`) and leaves Apache alone when that fails — its package would restart the shared Apache on that configuration; after the upgrade the shared Apache gets a graceful restart after the same check, or a full restart when it still runs the replaced binary. Meanwhile the Apache row of the Extensions page, `mp stack list` and the TUI counts only the accounts' Apaches that have a moved site, gives the number of sites that wait for the move and the state of the shared `apache2.service`: running, it is a warning (an amber tag); stopped while sites wait, an error (red; `mp stack list` adds "systemctl start apache2.service"); on a replaced binary the row gets its Upgrade button (`mp stack upgrade apache`, `u` in the TUI).

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
slowlog = /var/log/monopanel/sites/alex/example.com.php.slow.log
request_slowlog_timeout = 10s
catch_workers_output = no        ; worker output stays out of the root master's log; PHP errors go to php_admin_value[error_log]
clear_env = no                   ; composer/wp-cli need PATH/HOME
env[PATH]   = /var/www/alex/data/bin:/usr/local/bin:/usr/bin:/bin
env[TMPDIR] = /var/www/alex/data/tmp
php_admin_value[open_basedir]      = /var/www/alex/data
php_admin_value[upload_tmp_dir]    = /var/www/alex/data/tmp
php_value[session.save_path]       = /var/www/alex/data/tmp/sess
php_admin_value[sys_temp_dir]      = /var/www/alex/data/tmp
php_admin_value[error_log]         = /var/log/monopanel/sites/alex/example.com.php.error.log
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

`mp doctor` warns about any pool of an installed branch that has `catch_workers_output` on (its `php-fpm pools` line): with it a worker can reach root through the master (CVE-2026-129291; fixed in PHP 8.2.36, 8.3.37, 8.4.28 and 8.5.13). The 0.8.19 template had it on. 0.9.0 rewrites the sites' pools as it moves the sites to the log tree ([§2](#2-file-layout-on-the-server)): a site whose move failed keeps its old pool until it is applied (`mp site apply <domain>`) — the doctor's "site logs" line names such sites; take the line out of a pool of your own by hand. A pool directory or file the doctor could not read is named in a warning, not taken as clean.

`mail()` of every PHP site hands the message to `/usr/sbin/sendmail` (`sendmail_path` above). Without it `mail()` just returns false and the message is gone with no trace in the logs, so while there are PHP sites `mp doctor` checks for it (the `sendmail` line, following symlink chains such as `/etc/alternatives/mta`) and warns when it is missing. The panel's mail (`mp mail install`) brings it, and so does any MTA — postfix, msmtp-mta.

By default PHP sessions are files in the account's `tmp/sess`. A site can be switched to the `sessions` Valkey instance of its account (`mp site set <domain> --sessions valkey`, the "PHP sessions" field in the site settings), and then, instead of a `session.save_path` pointing to files, the pool gets:

```ini
php_value[session.save_handler] = redis
php_value[session.save_path]    = "unix:///run/monopanel-valkey/alex-sessions/valkey.sock"
```

The site's PHP branch needs the redis extension. The socket, with mode 600, belongs to the account: the site's pool runs as the account and can connect, the pools of other accounts cannot ([02](02-platform-matrix.md#9-valkey-per-account)).

The pool's session lines are `php_value`, not `php_admin_value`: an application cannot change an administrator's value, and a CMS with a session handler of its own (1C-Bitrix, `session.handlers` in `bitrix/.settings.php`) would break. Such a site keeps `--sessions files`, and the CMS connects to the account's Valkey instance itself, in its own settings (`mp valkey list` shows the socket).

`pm.max_children` is 8 by default; the bitrix preset gets one worker per 256 MB of RAM, from 8 to 48 (the number is chosen when the site gets the preset: on creation, on a preset change or on a move): Bitrix templates make HTTP requests to their own site from inside a request, and eight busy workers end up waiting for each other. The branch's global `99-monopanel.ini` keeps `short_open_tag = Off`, but the copy the CLI reads turns short tags on while the branch has a site with the bitrix preset: Bitrix's cron scripts and prolog start with `<?`, and otherwise PHP prints them as text and exits with code 0. Pools set `short_open_tag` themselves, so on Remi, where the CLI and FPM read the same directory, this does not affect sites. There the panel's start and `mp php ini set` leave a change of `short_open_tag` in the shared file to `mp site apply` of a site on the branch (a pool rendered before 0.8.6 does not close the tags itself), and write it at once for a branch no site's pool runs on — otherwise the `php cli` line of `mp doctor` would never clear. `max_children` can be set by hand for any site; with `pm=ondemand` idle sites hold no workers — the main mode for a shared server with hundreds of sites. For busy sites — `dynamic`/`static`, set by hand.

The pool `[www]` that every branch's php-fpm package ships (Debian/Ubuntu: `/etc/php/X.Y/fpm/pool.d/www.conf`, Remi: `/etc/opt/remi/phpXY/php-fpm.d/www.conf`) is not removed, since a php-fpm master does not start without a pool, but kept idle. It runs as `nobody`, listens on `/run/monopanel/php/.idle-X.Y.sock` (owned by root, mode 0600: neither nginx nor the accounts can connect) and starts no process until something connects (`pm = ondemand`, `pm.max_children = 1`). The stock pool ran as `www-data` (Debian) or `apache` (EL, with nginx let in through `listen.acl_users`), and those accounts own the directory of the master's pid file and socket (`/run/php` on Debian and Ubuntu) and that of its log (`/var/opt/remi/phpXY/log/php-fpm` on EL): PHP running as them reached root through those files. The panel writes the file once per branch — when it installs the branch, or at the first start of 0.9.0 for a branch installed before (the mark `php.distro_idle.<X.Y>`) — and afterwards it is the administrator's. Comments do not count: a pool whose directives are the idle pool's is the panel's, and a note of yours in it stays; the comment of 0.9.0's development builds (which said the file is rewritten at every start) is replaced once at the next start, without a php-fpm reload. One who brings the stock `www.conf` back (phpMyAdmin from apt, the maintainer's version at dpkg's prompt) keeps it, and `mp doctor` warns in its `php X.Y pool www` line: PHP in that pool runs as `www-data` (`apache` on EL) again. That opens no way to it for the panel: `fastcgi_pass` in the nginx configurations it writes leads to its own pools' sockets only, and the children of the accounts' Apaches run as the account. To hand the file back to the panel, remove it and restart `monopanel-api`: the panel writes a missing pool again. No other file of the pool directory is touched. On EL the php-fpm master logs to syslog (`error_log = syslog` in `php-fpm.conf`, the rest of the file unchanged): read it with `journalctl -u phpXY-php-fpm`. The panel sets that line once as well; should it be removed or changed later, `mp doctor` warns in the same `php X.Y pool www` line, and it is put back by hand: the panel never writes `php-fpm.conf` anew.

The panel accepts a `[www]` section (in any case, quoted or with `${…}`) in `www.conf` alone: php-fpm merges sections of one name across all its files, and `[www]` in another file would change the idle pool. Pools are named by plain words, the names of their sites. An `include` in `php-fpm.conf` may have a wildcard (`*`) in the file name only (`pool.d/*.conf`): php-fpm expands the path with glob(3), and a wildcard in a directory matches `..` too.

## 6. Isolation and limits

| Level | Mechanism | Status |
|---|---|---|
| Files | a unix user per client, 0710, ACLs for the web group, `open_basedir`, separate tmp/session | in place, on |
| PHP processes | an FPM pool running as the user, `disable_functions`, `pm.max_children` | in place, on |
| Symlink | `disable_symlinks if_not_owner` (nginx); `SymLinksIfOwnerMatch` in the account's Apache, whose children run as the account anyway — a symlink to someone else's file gets them nothing | in place, on |
| Requests past nginx | any local process can connect to `127.100.x.y:81`, but the account's Apache answers only requests carrying the account's secret, which nginx sends (`X-MonoPanel-Backend`), and others get 403: a site's `allow`/`deny`, rate limit and `X-Real-IP` cannot be bypassed past nginx. The account's Apache refuses dotfiles and the package managers' files even with the secret and runs no PHP from shared folders with `--no-php`. An account can learn its own secret, but it opens only that account's Apache | in place, on |
| Disk | filesystem quotas per uid (`quota` / `xfs_quota`); without quotas — accounting by `du` in the metrics | planned |
| CPU/RAM per site | an isolated pool: a separate master `monopanel-php-fpm@<X.Y>-<domain>.service` in `Slice=monopanel-<user>.slice` with `MemoryMax`, `CPUQuota`, `TasksMax`, `PrivateTmp`, `ProtectSystem=strict`, `ReadWritePaths=/var/www/<user>` | planned |
| SSH | shell only by a flag; SFTP-only chroot (`Match Group monopanel-sftp`) | in place, SFTP only by default |
| Network | outgoing PHP connections are not restricted (APIs, payments); blocking outgoing SMTP through an nft rule per uid (`meta skuid`) | planned |

## 7. TLS / ACME

- Client: `lego` as a library inside `monopaneld api`: account, keys, challenges, renewal.
- HTTP-01: a shared webroot `/var/lib/monopanel/acme/webroot`, included through the `acme.conf` snippet in every `:80` server{} and in the default-server of each IP → issuance works both before the site is created and with a redirect to HTTPS.
- DNS-01: lego providers — Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap and RFC2136 (`mp dns-provider types`) — for wildcard `*.example.com`; credentials are stored encrypted, per user.
- Keys: ECDSA P-256 by default, RSA-2048 by a flag; a dual certificate (EC + RSA) is planned.
- Storage: `/var/lib/monopanel/certs/<domain>/{fullchain.pem,privkey.pem,chain.pem}` 0600 root (the nginx master runs as root; nginx terminates TLS, and the accounts' Apaches read no certificates). Importing your own certificates (PEM) — through the UI/CLI/API; a certificate can be shared by several sites (SAN).
- Wildcards (0.9.2): `*.example.com` covers a name exactly one label below — `www.example.com`, not `example.com` and not `a.b.example.com` (RFC 6125). A site takes the certificate on its record, else the best that fits: the record of its name, then a certificate that carries its name, then a wildcard. Importing or issuing a wildcard attaches it to the sites it covers that have no valid certificate of their own; a site with its own certificate for its exact name stays on it. A wildcard an account brought along in a migration serves that account's sites only. In a migration one file a source serves on several sites arrives as one record. The names of a site its certificate does not cover (`www.kz.example.com` under `*.example.com`) are named in the `site.apply` log. A certificate given to a site by hand (`mp site set --cert`, `mp ssl import --site`, the select on the site page) stays with it even when another one is issued for its names (`sites.cert_pinned`); `--cert auto` gives the choice back to the panel. The panel's port, phpMyAdmin and mail take the record of their own name, else a certificate that covers it — the panel's or an administrator's, never an account's. A Let's Encrypt wildcard arrives in a migration with its `*.` name; it renews through DNS-01 only, and without a DNS provider the renewal fails with the hint `mp ssl issue <name> '*.<name>' --dns <provider>`. Removing an account takes its sites' certificates only when nothing else serves them.
- Renewal: a scheduler once a day, 30 days before expiry; on failure — retries with a growing interval, the error is visible in `mp doctor` and goes out as the `cert.issue.failed` webhook event; on success the sites on that certificate are applied again (`site.apply`) — a reload of nginx.
- The native `ngx_http_acme_module` (nginx 1.29+) instead of lego for HTTP-01 is a possible future option.
- Panel host: a certificate for `web.hostname` through the same mechanism (`mp ssl issue <hostname>`); the panel picks it up on the fly through `tls.Config.GetCertificate`, and until it has one — a self-signed certificate with its fingerprint in the `mp setup` output. Current state: `mp web tls` / `GET /web/tls`.
- Implemented (stage 0): `internal/acme` (accounts in `<data>/acme/accounts/<directory>/<email>/`, certificates in `<data>/certs/<name>/`), the `cert.issue` job with a public DNS check and a webroot probe through nginx, the renewal scheduler in the API process.
- The ACME directory is configurable (Let's Encrypt, ZeroSSL, Buypass, an internal Step-CA).
- The panel certificate and the site certificates are kept apart (2026-09-10). The panel certificate is the record named `web.hostname`: `GET /ssl/panel` shows what the panel serves now and what is behind it (an order in progress, an error), `POST /ssl/panel/issue` orders one for exactly that name (HTTP-01, or `dns` for DNS-01), `POST /ssl/panel/import` installs a ready one, `DELETE /ssl/panel` goes back to self-signed. A site certificate is ordered from the site: `POST /sites/{domain}/tls/issue` takes the domain and its aliases, switches the site to `ssl: auto`, and the issuance job re-applies the site with HTTPS itself. In the `/certificates` list every certificate has `used_by_panel` and `used_by_sites`; a certificate in use cannot be deleted (409), and a certificate the panel shares with a site of the same name (the panel on `example.com` and the site `example.com`) is a legitimate case: it is listed under both. The SSL page in the web UI shows three blocks: "Panel", "Sites", "Not in use".

## 8. Logs and rotation

- Site logs live in `/var/log/monopanel/sites/<login>/`, outside the home: nginx, Apache and php-fpm open them as root.
- Rotation — `/etc/logrotate.d/monopanel-sites`, two blocks per account, as root (the servers' logs with `create 0640 root <login>`, the PHP error logs with `create 0660 <login> <login>`): weekly, or sooner if a log has grown past 100 MB; eight copies, compressed from the second one on; after rotation nginx and the account's Apache get USR1 through their pid files. In detail — [section 2](#2-file-layout-on-the-server).
- The `main` format records `$request_time`, `$upstream_response_time`, `$host` and `$server_protocol`. Parsing access logs for per-site metrics (incrementally, remembering the file position) is planned.
- In the UI — the tail of access/error/php-error/php-slow (the site's "Logs" tab, `mp site logs`); live follow and filtering by response code and path are planned.

## 9. Additional web features (site flags, templates)

- Redirects: http→https, www↔non-www; arbitrary 301/302 by path — through custom directives in `<domain>.d/` or `mp site nginx`.
- Site aliases (shared docroot) and separate sites on subdomains.
- A custom docroot inside `data/www/<domain>/` (for example `/public` for Laravel and Symfony).
- Access only from listed addresses (`--allow`); a rate limit per address (`--rate-limit N` — `limit_req` on dynamic pages, static files do not count, the excess gets 429); security headers (`--security-headers`). Basic auth per path and blocking by country (geoip2 as a dynamic module) are planned.
- IPv6: a site listens on the server's IPv6 address too (`--ipv6`; the first global one by default, `none` — IPv4 only), and every address has its own default server. An address that comes from the router (SLAAC) or over DHCPv6 reaches the interface later than nginx starts, and without preparation nginx failed at boot with "Cannot assign requested address". So the panel sets `net.ipv6.ip_nonlocal_bind = 1` (`/etc/sysctl.d/90-monopanel.conf`) and gives the nginx unit a drop-in with `Restart=on-failure`; the default server of an IPv6 address is not treated as stale in the first minutes after boot.
- Browser caching of static files, `gzip`; `brotli` as a dynamic module of our own build is planned.
- A "site suspended" placeholder for `status=suspended`: the config is replaced with a minimal one serving a 503 page, and the FPM pool is removed from the master's config.
- `proxy` mode — the site is served by your own application (Node, Python, Docker): `--mode proxy --backend http://127.0.0.1:3000`; a background application can run under systemd as an app service (`mp app`).

## 10. CMS installation

`mp cms install <domain> <cms>` (and the "CMS" tab on the site page) installs WordPress, Joomla, OpenCart or 1C-Bitrix into an existing site the way the vendor documents it, only without the clicking:

1. The site gets the CMS preset, if it has a different one, and the configuration is applied.
2. The docroot must be empty; with `--force` its contents are deleted (as the client).
3. A database `<login>_<cms>` is created (on a repeat install — `<login>_<cms>2`) with its own user and a generated password.
4. The distribution is downloaded from the vendor (WordPress from wordpress.org, Joomla and OpenCart as the latest release on GitHub, without the API, Bitrix as the trial edition from 1c-bitrix.ru) and, streamed without buffering in memory, unpacked by the agent with `tar` straight into the docroot (the OpenCart zip is re-encoded to tar on the fly); the files are handed over to the client and the SELinux labels are restored. A download that breaks off or stalls is picked up from the byte where it stopped when the server serves byte ranges (`Accept-Ranges`; its `ETag` or `Last-Modified` goes back as `If-Range`, so a file that changed meanwhile is not spliced onto the old one) — up to eight times; `tar` never sees the break. When the unpack fails all the same, whatever it unpacked is handed to the client right away: otherwise those files would stay root's, and neither the client nor a repeat install with `--force` could remove them.
5. The CMS installer runs as the client through `fsop run`, which executes only the PHP interpreter (the site's branch) inside the home directory: wp-cli (`/usr/local/lib/monopanel/wp-cli.phar`, verified against the published sha512), `installation/joomla.php install`, `install/cli_install.php`. The database and administrator passwords never go on the command line, which every local user can read (`ps`): they reach the installer on stdin, and a short bootstrap (`php -r`) appends them to `$argv` and runs the script itself. Bitrix has no CLI installer: the panel walks through its web wizard over HTTP via the site itself (the site's name is resolved to its address), including the AJAX steps that install the modules, updates and the solution, and the trial registration on 1c-bitrix.ru on behalf of the site administrator. Edition (`--edition`): the trial `start`, `standard`, `small_business`, `business` from 1c-bitrix.ru; the license key is entered later in the Bitrix settings, and the edition must match the key. Solution (`--solution`): by default `clean`, the Marketplace's "1C-Bitrix clean install" (an empty site with the standard modules and no demo content); `demo`, the demo site bundled with the distribution; or the id of any Marketplace solution (`vendor.solution`), which the wizard downloads and installs; the panel walks through the solution's wizard the same way as through the main one.
6. The site record keeps the CMS, its version and the time of installation; the administrator password is returned once in the API response and never gets into the job log (in the payload it is encrypted with the panel's key).

The administrator password is 12–20 characters, to satisfy all four CMSs at once; without one, a 16-character password is generated. The e-mail is the site owner's, otherwise `admin@<domain>`.

