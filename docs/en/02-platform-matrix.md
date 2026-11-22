# 02. Platforms, package sources, PHP, database servers

## 1. Supported operating systems

| Family | Distribution | Versions | Notes |
|---|---|---|---|
| Debian | Debian | 12 (bookworm), 13 (trixie) | Debian 11 is not supported (its LTS ended 08.2026) |
| Debian | Ubuntu | 22.04, 24.04, 26.04 LTS | LTS releases only |
| RHEL | RHEL, AlmaLinux, Rocky Linux, Oracle Linux | 9.x, 10.x | EL10 requires x86-64-v3 (AlmaLinux 10 has a v2 build); CentOS Stream is not supported (rolling); EL8 is not supported |

Architectures: amd64 and arm64 — packages are built for both, and every package source below has arm64 builds; the testbed matrix runs on amd64.

Kernel: restic's sandbox needs Landlock (Linux 5.13+, `landlock` in the list in `/sys/kernel/security/lsm`); the "restic sandbox" line of `mp doctor` tells whether the host has it. Without it restic runs without the sandbox, apart from a backup's view without symlinks, and `mp doctor` warns. On Oracle Linux with the UEK kernel Landlock is built in but not enabled (`mp doctor`: "Landlock is built into this kernel but not enabled"): turn it on with `grubby --update-kernel=ALL --args=lsm=$(cat /sys/kernel/security/lsm),landlock` and a reboot, or boot the RHCK kernel (`dnf install kernel`, `grubby --set-default`). Until then a backup is guarded by its view without symlinks alone. For that view of the trees a backup saves the agent needs to mount (a mount namespace of its own and `mount_setattr` with `nosymfollow`, Linux 5.14), and so does restic's private `/etc` (a tmpfs); in a container whose profile forbids mounting, `mp doctor` warns, also that a host replacing `resolv.conf` during a backup leaves restic without DNS. In detail: [01 §7](01-architecture.md#7-security). On AlmaLinux, Rocky Linux and Oracle Linux EPEL's restic is built with Red Hat's Go and reads `/proc/sys/crypto/fips_enabled` as it starts; the sandbox grants it that file, and nothing else of `/proc`. The "restic sandbox" line names the restic that started in the sandbox, restic's error when it did not (a failure), or that the test did not finish within 30 s (a warning). The agent binds the shared folders ([10](10-shared-folders.md)) with `open_tree`, `move_mount` and `mount_setattr`, and tells the mount of a directory on a folder's way with `statx` (`STATX_MNT_ID`) (Linux 5.12+; every OS in the table has 5.14 or later). In a container (LXC, OpenVZ, Docker; the agent learns it from `systemd-detect-virt --container`, `/run/systemd/container` or `/proc/vz`) the host's kernel loads the modules with the host's modprobe configuration, and the panel's file in the container changes nothing: the "kernel modules" line of `mp doctor` warns of it, and `install sctp /bin/false` (the same for dccp, rds, tipc) belongs in the host's `/etc/modprobe.d`. `GET /system/status` gives the container's kind in `host.container`.

Minimum requirements: 1 vCPU / 1 GB RAM / 10 GB disk (panel + nginx + one PHP version + a database server with a 128 MB buffer pool). Recommended: 2 vCPU / 2 GB.

## 2. Package sources

| Component | Debian / Ubuntu | EL9 / EL10 | Comment |
|---|---|---|---|
| nginx | nginx.org, the `nginx` repository (stable) | nginx.org `nginx-stable` | The same layout on every OS: `/etc/nginx/nginx.conf` + `conf.d/`, user `nginx`. HTTP/3 (QUIC) in the nginx.org builds. Option: Angie (a fork with built-in ACME and API) as a drop-in |
| Apache 2.4 | distribution (`apache2`) | — | Debian/Ubuntu only: one instance per account (`monopanel-apache@<login>`), `mpm_event` + `mod_proxy_fcgi`; `mod_php` is not installed. EL has no apache mode |
| PHP (stage 1) | Debian and Ubuntu: `packages.sury.org/php` (an Ubuntu release Sury does not publish yet: Ubuntu's own PHP) | Remi (`remi-release-9` / `remi-release-10`), SCL-style packages `php{56..85}-php-*` | Parallel versions out of the box. Each of the two repositories is maintained by a single person — a risk, hence stage 2 |
| PHP (stage 2, planned) | own repository | own repository | A build farm, layout `/opt/monopanel/php/<X.Y>`, the same on every OS |
| MySQL 8.4 LTS | MySQL APT repo (`mysql-apt-config`) | MySQL Yum repo (`mysql84-community-release-el9` / `-el10`) | Community Server 8.4.x |
| Percona Server 8.4 LTS | `percona-release setup ps-84-lts` | `percona-release setup ps-84-lts` | + Percona XtraBackup 8.4, Percona Toolkit |
| phpMyAdmin (planned) | own package from the upstream tarball | own package | Distribution versions lag behind |
| restic | distribution | EPEL | Backups: deduplication, encryption, local/SFTP/S3/B2/REST |
| fail2ban | distribution | EPEL | + the panel's filters |
| Valkey | distribution: `valkey-server` (Debian 13, Ubuntu 24.04/26.04; Debian 12 — backports), otherwise `redis-server` (Ubuntu 22.04 — 6.0, Debian 12 without backports — 7.0) | AppStream: `valkey` (8.0) | Instances per account, the package's shared instance is switched off, see §9 |
| Other | `acl quota cron logrotate unzip nftables openssh-server ca-certificates` | `acl quota cronie logrotate unzip nftables openssh-server policycoreutils-python-utils` + EPEL | |

Updates: what the panel installed from vendor repositories (nginx.org, Sury, Remi, Percona, MySQL), and Apache, it keeps up to date itself — `mp stack upgrade`, and daily for nginx, PHP and Apache ([01 §9](01-architecture.md#9-packaging-installation-updates)); `mp doctor` names every component with an update waiting. The distribution's own packages (restic, fail2ban, Valkey, memcached, mail) are left to the distribution's updates: unattended-upgrades on Debian/Ubuntu (it does not take the vendors' origins); on EL there is nothing by default, so dnf-automatic is recommended. The agent runs apt with `NEEDRESTART_MODE=l` (and so do the panel's self-update and `install.sh` when they install the panel's package), so needrestart, where it is installed (on Ubuntu), only lists the services on old libraries and restarts nothing itself, and the panel restarts only the services of what it upgrades (nginx, php-fpm, the accounts' Apaches, the database server), after the configuration check; `mp doctor` names the stack's services that still run a replaced binary.

Component developers add new OS releases with a delay, so support for a new OS is announced only after a [testbed](08-testbed.md) run; a weekly CI check of package availability is planned.

What the testbed showed:

- Percona 8.4 and nginx.org are available for every OS of the matrix (checked 2026-09-09; Oracle Linux was added 2026-09-10).
- PHP on Ubuntu comes from `packages.sury.org/php`, as on Debian. The maintainer marked `ppa:ondrej/php` deprecated ("PPA for PHP (deprecated)", November 2026) and its resolute suite is empty, while Sury publishes jammy, noble and resolute (checked 2026-11-22: signed by `15058500A0235D97F5D10063B188E2B695BD4743`, rsa3072 until 2028-02-04, the same key as for Debian; plucky and questing are not there). Before adding it on Ubuntu the panel asks for Sury's `dists/<codename>/Release`; without the release it installs Ubuntu's own PHP (only 8.5 there on 26.04), marks the missing branches with the reason and asks again at the next install. When apt cannot read the index after the panel wrote a PHP source — the key refused, the repository gone — the panel removes the source it just wrote: left behind, it would fail `apt-get update` for everything else, the mail and nginx installs included. The next PHP install writes it again. The same when `apt-get update` ends well but apt has no index of Sury (a network failure apt takes for passing: only a warning, the source left out): the install fails with that warning, the source is removed, and the repository is not marked ready. When apt already reads Sury from a source the server has — one an administrator added (Sury's README: `/etc/apt/sources.list.d/php.list` with the key `/usr/share/keyrings/deb.sury.org-php.gpg`) or a mirror of Sury (Sury's `+debsuryorg` builds from another address) —, the panel writes no source of its own and updates the index before the install: apt refuses two sources of one repository with different Signed-By, and every other source with them ("Conflicting values set for option Signed-By"). When apt has not read the administrator's source yet, a PHP install fails with that error of apt's and advice: after `apt-get update` the next install keeps it. A source made by Sury's old README — no signed-by, the key in `/etc/apt/trusted.gpg.d` — apt accepts beside the panel's when it reads it first (`php.list`), warning at every update ("configured multiple times"); read after it, it conflicts on apt 2.x and not on apt 3. The panel's own `sury-php.list` left from before (the panel installed again) is written again with its key.
- A server where a panel up to 0.8.19 added the PPA (`ondrej-php.list`, key `ondrej-php.asc`) moves to Sury at the start of the API, as the last step of the start-up work: Sury's key and source go in beside the PPA's, the index is read with both, and the PPA goes only if `apt-cache policy php-common` then names packages.sury.org with the release's codename and apt read Sury in that index update and said nothing against it. How `apt-get update` itself ended decides nothing: it fails for any failing source (the PPA itself included) and ends well when Sury was left out for a passing network failure (only a warning then); an index of Sury from before does not count — nor when the update did not run at all (the lists' lock held over ten minutes). Then the index is updated once more, without the PPA. When the move fails, Sury's files go again, the PPA stays, the reason is in `mp doctor` (`php repository`, the setting `php.repo.switch`) and in the API's journal, and every start tries again. When apt already reads Sury from a source of the server's (an administrator's, a mirror), the panel updates the index with the PPA still there and removes the PPA alone if apt read Sury and said nothing against it; it writes no source of its own. When such a source is there but apt has not read it yet, the panel's write runs into "Conflicting values set for option Signed-By": the panel's Sury files go again, the PPA stays, and `mp doctor` advises `apt-get update` (the next start of the API keeps the administrator's source and removes the PPA alone) or removing that source (the panel then puts its own in). A source of the PPA an administrator added (`add-apt-repository`: `ondrej-ubuntu-php-<release>.sources`/`.list`) is left alone. One of the PPA's address today (`ppa.launchpadcontent.net`) has broken apt beside the panel's `ondrej-php.list` since 0.8.19 (the administrator's names another Signed-By or none — `add-apt-repository` on Ubuntu 22.04 writes it so, its key in `/etc/apt/trusted.gpg.d` — and apt reads no source at all): the move cannot happen until one of the two goes, and `mp doctor` says so — remove yours, and the next start of the API moves PHP to Sury, or remove the panel's `ondrej-php.list` and `ondrej-php.asc`, and your PPA stays PHP's source. A source of the PPA's old address (`ppa.launchpad.net`) apt takes beside it, and the move goes as usual. The agent no longer writes the PPA (config/apply refuses its source and key) but lets its two files be removed (paths/remove). Nothing is reinstalled: Sury's builds sort above the PPA's (`8.4.28-3+debsuryorg24.04.1+1` against `8.4.28-2+ubuntu24.04.1+deb.sury.org+1`, `php-common` `2:102+…` against `2:101~+…`), and the next stack upgrade installs them. Everything the panel installs is in Sury for every branch and all three releases (but `phpX.Y-sodium` from 7.3 on: sodium is built into `phpX.Y-common`, and the PPA lacks those packages too); the PPA also had `php-pear`, `php-imap`, `php-pspell`, swoole/tideways builds and, on jammy, its own `libpcre2`/`libmemcached` — the panel installs none of them, installed ones stay, and Sury's builds work with them.
- On EL the Percona/MySQL packages start the server with a temporary root password in `/var/log/mysqld.log` rather than with `auth_socket`; the panel reads the password from the log and switches root to the socket itself.
- Oracle Linux: EPEL is enabled by the `oracle-epel-release-el9` package, and on OL10 by the official `epel-release` from dl.fedoraproject.org, because the Oracle package does not provide `epel-release = 10`, which `remi-release-10` requires. Oracle images ship with firewalld enabled.

## 3. PHP

### 3.1 Version matrix (as of 09.2026)

| Version | Upstream status | Sury (Debian 12/13, Ubuntu 22.04–26.04) | Remi EL9 | Remi EL10 | Own build (planned) |
|---|---|---|---|---|---|
| 5.6 | EOL (2018) | yes, a reduced set of extensions | yes | no, or not all | yes (OpenSSL 1.1 statically linked) |
| 7.0–7.3 | EOL | yes | yes | no, or not all | yes (OpenSSL 1.1 statically linked) |
| 7.4 | EOL (2022) | yes | yes | yes | yes |
| 8.0 | EOL (2023) | yes | yes | yes | yes |
| 8.1 | security fixes ended 12.2025 | yes | yes | yes | yes |
| 8.2 | security until 12.2026 | yes | yes | yes | yes |
| 8.3 | security until 12.2027 | yes | yes | yes | yes |
| 8.4 | active until 12.2026, security until 12.2028 | yes | yes | yes | yes |
| 8.5 | active until 12.2027, security until 12.2029 | yes | yes | yes | yes |
| 8.6 / 9.0 | expected 11.2026 | will appear after the release | will appear | will appear | add to the farm |

Whether Remi has the old branches for EL10 needs to be checked against his current table; the missing ones can only come from our own build. In the UI versions are marked "current", "security only", "EOL — insecure". A new site defaults to the latest stable (8.5).

### 3.2 Standard extension set

For every version where the extension builds:

`opcache, mysqli, pdo_mysql, mbstring, intl, gd (webp/avif), imagick, curl, zip, xml, dom, simplexml, xmlreader, xmlwriter, soap, bcmath, gmp, exif, fileinfo, iconv, sockets, calendar, ctype, tokenizer, json, redis, memcached, apcu, igbinary, msgpack, xsl, ldap, imap (≤ 8.3 in core, 8.4+ via PECL), sodium (≥ 7.2), mcrypt (≤ 7.1), ioncube loader (optional, 5.6–8.5), xdebug (enabled for dev only)`.

Composer is a single shared binary, run through the `php` of the selected version.

### 3.3 Own build layout (stage 2, planned)

```
/opt/monopanel/php/8.4/{bin/php, bin/phpize, bin/php-config, sbin/php-fpm, lib/php/extensions/…}
/etc/monopanel/php/8.4/php.ini          # the version's global ini (panel template)
/etc/monopanel/php/8.4/php-fpm.conf     # include pool.d/*.conf
/etc/monopanel/php/8.4/pool.d/<domain>.conf
/etc/monopanel/php/8.4/conf.d/*.ini     # enabling extensions
/run/monopanel/php/<domain>.sock        # pool socket
/var/log/monopanel/php/8.4-fpm.log
systemd: monopanel-php-fpm@8.4.service
  ExecStart=/opt/monopanel/php/%i/sbin/php-fpm -y /etc/monopanel/php/%i/php-fpm.conf --nodaemonize
  ExecReload=/bin/kill -USR2 $MAINPID
```

Build: Docker images for each target OS (linked against the system libc/ICU/libxml2), `nfpm` → deb/rpm, packages `monopanel-php-8.4`, `monopanel-php-8.4-imagick` and so on. For 5.6–7.3, OpenSSL 1.1.1 and, where needed, old libxml2/ICU are linked statically — explicitly marked "EOL, no security updates". For 7.4/8.0 — OpenSSL 3 compatibility patches (as Sury/Remi do).

Implemented (stage 1): `internal/osprofile/php.go` describes the Sury/Remi layout (core packages and best-effort extensions per branch, paths, unit), `mp php install <ver>` adds the repository once (the Sury key in `/etc/apt/keyrings/` on Debian and Ubuntu, remi-release on EL), installs the packages, writes `99-monopanel.ini` and enables `phpX.Y-fpm`. The `php_versions.source` field in the database says which layout to use. Both schemes coexist on one server during the transition; switching a site over means changing `source` + regenerating the pool + reloading both masters. The branch's optional extensions (imagick, redis, apcu, memcached and others — older branches lack some) install in one run of apt or dnf: every run with packages of a branch restarts its php-fpm (on Debian the fpm package's trigger at the end of the run), and with one package per run php-fpm was restarted more often than systemd's start limit allows (5 in 10 s) — systemd refused the panel's own reload and the install failed (testbed, Debian 12). The panel asks the repositories once which of the packages they carry and installs those together; if that run fails, it installs them one by one — a broken package does not keep the others out — and says so in the job log. Names the repositories carry no package of go in one more run together: another package may provide them (with Ubuntu's own PHP the package `php-imagick` provides `php8.3-imagick`, Debian's `php8.4-xml` brings `php8.4-xsl`). An extension a package of the branch already has built in does not count as missing; the rest the log names in its "not available for this branch, skipped" line.

### 3.4 PHP version for the CLI

- Globally: `/usr/bin/php` → `update-alternatives` / `alternatives` pointing to the default version.
- Per client: `/var/www/<user>/data/bin/php` → a symlink to the version of the client's main site; the directory is added to `PATH` through `/etc/profile.d/monopanel.sh`; the panel's cron jobs run with an explicit path to the selected version.

## 4. Database servers

| | MySQL Community 8.4 LTS | Percona Server 8.4 LTS |
|---|---|---|
| Compatibility | the reference | drop-in: the same protocol, file format and clients |
| Pros | the "official" MySQL | XtraBackup (hot physical backup), extended diagnostics (slow log, PFS), Percona Toolkit, thread pool, MyRocks |
| Recommendation | on request | **default** |

Common to both (implemented in the `DBEngine` provider):

- Panel access: `root@localhost` over the unix socket with the `auth_socket` plugin — the root password is not stored; queries are run by the agent.
- Naming: database `<user>_<name>`, user `<user>_<name>@localhost`. Remote access (a user with host `%`, a firewall rule for 3306 and `bind-address` set to the external IP) is planned.
- `character_set_server=utf8mb4`, `collation_server=utf8mb4_0900_ai_ci`.
- The `zz-monopanel.cnf` template, sized to the RAM: `innodb_buffer_pool_size` (25–50 %), `innodb_redo_log_capacity`, `max_connections`, `table_open_cache`, `tmp_table_size`; `bind-address=127.0.0.1`; `local_infile=OFF`; `innodb_strict_mode=OFF` (strict mode rejects tables whose row size exceeds the InnoDB limit, and dumps from old servers — CMS installers and moves run into this); `transaction_isolation=READ-COMMITTED` and `sql_mode=NO_ENGINE_SUBSTITUTION` (required by 1C-Bitrix, harmless to other CMSs); `max_allowed_packet=64M`, `thread_cache_size=32`, `sort_buffer_size`/`join_buffer_size` 2M; `performance_schema=ON`; the binlog is off by default (`disable_log_bin`) — on a single node it only takes up disk space; a flag turns it on with `binlog_expire_logs_seconds=259200`.
- **Legacy PHP**: in 8.4 the `mysql_native_password` plugin is disabled by default (removed in 9.0). PHP < 7.4 (mysqlnd) does not support `caching_sha2_password`. When there are sites on PHP ≤ 7.3, the panel sets `mysql_native_password=ON` and `authentication_policy=mysql_native_password,,` and creates the users of those sites `IDENTIFIED WITH mysql_native_password`; all others get `caching_sha2_password`. The plugin alone is not enough: without `authentication_policy` the server offers `caching_sha2_password` in the handshake, and the mysqlnd of PHP 5.6/7.0 drops the connection before the method is switched — even for an account with `mysql_native_password`. After a panel update the configuration of such servers is re-rendered when the API starts (MySQL restarts only if the file changed). The `db_users.auth_plugin` field stores the choice.
- Backups: `mysqldump --single-transaction --routines --triggers --events` per database. `mysqlsh util.dumpSchemas` (parallel, faster on large volumes) and physical XtraBackup 8.4 copies of the whole server (Percona) are planned.
- Updates within 8.4.x: `mp stack upgrade db`, by name only and not in the daily upgrade, as the server's restart is downtime for the sites; the configuration is checked with `mysqld --validate-config` before the upgrade and before the restart. The upgrade to 9.x is not automated (a separate procedure that checks legacy authentication and users).
- phpMyAdmin (planned): SSO from the panel (signon auth), a separate FPM pool `monopanel-pma` on the newest installed PHP, served by the panel through a FastCGI client at `https://host:8443/pma/` — independent of the system nginx.

## 5. OS Profile — OS differences in one place

| Parameter | Debian / Ubuntu | EL9 / EL10 |
|---|---|---|
| Package manager | apt/dpkg, `DEBIAN_FRONTEND=noninteractive` | dnf/rpm |
| nginx user | `nginx` (nginx.org package) | `nginx` |
| Apache user | `www-data` (the panel takes it out of `monopanel-web`; the children of an account's Apache run as the account) | `apache` (no apache mode) |
| Apache: package / service / directory | `apache2` / `monopanel-apache@<login>.service` (`apache2.service` disabled) / `/etc/apache2/monopanel/accounts/<login>/` | — |
| Apache: config check | `apache2 -t -f /etc/apache2/monopanel/accounts/<login>/httpd.conf` | — |
| PHP (stage 1) | `/etc/php/X.Y/fpm/{php.ini,pool.d/}`, binary `php-fpmX.Y`, service `phpX.Y-fpm.service`, CLI `phpX.Y` | `/etc/opt/remi/phpXY/{php.ini,php-fpm.d/}`, binary `/opt/remi/phpXY/root/usr/sbin/php-fpm`, service `phpXY-php-fpm.service`, CLI `phpXY` |
| PHP (stage 2) | `/opt/monopanel/php/X.Y` | `/opt/monopanel/php/X.Y` |
| The panel's database config | `/etc/mysql/mysql.conf.d/zz-monopanel.cnf` (MySQL) / `/etc/mysql/conf.d/zz-monopanel.cnf` (Percona) | `/etc/my.cnf.d/zz-monopanel.cnf`; with Percona on EL, `/etc/my.cnf` includes nothing, and `mysqld` reads only `/etc/my.cnf`, `/etc/mysql/my.cnf`, `/usr/etc/my.cnf` — the panel writes `/etc/mysql/my.cnf` with `!includedir /etc/my.cnf.d/` (found by the testbed on 2026-09-10: until then the panel's settings were not applied on EL) |
| Database service | `mysql.service` | `mysqld.service` |
| MAC | AppArmor (no profiles ship for nginx/php, no intervention needed) | SELinux enforcing — see §6 |
| Firewall | nftables directly (ufw, if present — disable it or coexist, by choice) | firewalld (nftables backend) — see §7 |
| cron | `cron` | `cronie` |
| Quotas | `quota` (ext4: `usrquota`; xfs: `uquota`) | `quota` / `xfs_quota` |
| fail2ban | distribution | EPEL |
| Client shells | `/bin/bash`, `/usr/sbin/nologin` | `/bin/bash`, `/sbin/nologin` |
| Certificates bundle | `/etc/ssl/certs/ca-certificates.crt` | `/etc/pki/tls/certs/ca-bundle.crt` |

On both layouts the package's stock `www` pool is the panel's idle pool (made so once per branch, see [03 §5](03-web-stack.md#5-php-fpm-pool-template)). On EL the php-fpm master logs to syslog, not into `/var/opt/remi/phpXY/log/php-fpm/` (that directory is apache's). The panel neither installs nor touches AppStream PHP.

The interface in code:

```go
type Profile interface {
    Family() Family                   // Debian | RHEL
    Release() Release                 // id, version, codename, arch
    Packages() PackageManager         // AddRepo / Install / Remove / Upgrade / Query
    Web() WebLayout                   // nginx/apache users, directories, services, validation commands
    PHP(source string) PHPLayout      // sury | remi | monopanel
    DB(engine string) DBLayout        // mysql | percona
    MAC() MACHandler                  // selinux | apparmor | none
    Firewall() FirewallHandler        // nft | firewalld
    Cron() CronHandler
    Quota() QuotaHandler
}
```

## 6. SELinux (EL9 / EL10)

The panel runs in **enforcing** mode; disabling SELinux is outside the project's policy.

Done (2026-09-09, checked on the testbed on AlmaLinux 9/10 and Rocky 9/10): on the first install of nginx or PHP the panel prepares the host for hosting, once — it installs `policycoreutils-python-utils`, adds `fcontext` rules for the FPM sockets (`/var/run/monopanel(/.*)?` → `httpd_var_run_t`; semanage itself suggests this spelling because of the `/run` ↔ `/var/run` equivalence rule, and the panel follows the hint) and for site logs (`/var/www/[^/]+/data/logs(/.*)?` → `httpd_log_t`), turns on the booleans `httpd_unified`, `httpd_can_network_connect`, `httpd_can_network_connect_db`, `httpd_can_sendmail`, `httpd_execmem`, `httpd_setrlimit` and runs `restorecon` over `/var/www`, `/run/monopanel`, `/etc/nginx`, `/var/log/nginx`. The agent restores labels itself after every config write and every directory it creates, and a config set has a `restore` field: `nginx -t`, which the agent uses to check the configuration, creates `/run/nginx.pid` with the agent's label (`var_run_t`), and nginx in the `httpd_t` domain could not open it — the file is relabelled before the start. The panel itself is still unconfined: the agent (root, no NoNewPrivileges) runs in `unconfined_service_t`. systemd runs the package manager's changes in `monopanel-pkg-<run>.service` with the agent's fences, none of which turns NoNewPrivileges on for root, so dnf goes into `rpm_t` there as it does from dnf-automatic's units, and the packages' scriptlets into `rpm_script_t`.

Since the site logs moved from `data/logs` to root's tree (after 0.8.19, [03](03-web-stack.md#2-file-layout-on-the-server)), they have a rule of their own: `/var/log/monopanel/sites(/.*)?` → `httpd_log_t` (the path follows `log_dir`). It is part of the host preparation, and a host prepared by an earlier version — or a host with sites on which SELinux was turned on later — gets it at panel start-up together with a `restorecon` of that tree — before the sites switch to the new paths: a directory made under `/var/log` inherits `var_log_t`, which nginx and php-fpm in `httpd_t` cannot write. If `semanage` failed at start-up, the panel tries the rule again every hour (before it retries the move of the logs) and before it applies any site: a `semanage` fixed meanwhile is picked up within the hour, without a restart of the panel. While the rule is missing, a site on an enforcing host is not applied — the job fails with `semanage`'s reason, and the site keeps running on its current configuration (a new site does not appear at all; a site of 0.8.19 keeps its logs in `data/logs` — the agent accepts no new configuration with logs there, so that root opens no log in an account's directory); on a permissive host the site is applied with a warning in the job log. Suspending a site is not held back: the stub writes no logs. The webmail on its own port waits for the rule too: on an enforcing host without it, its server block stays as it is (for a webmail 0.8.19 published, logging into `data/logs`; the move of the logs rewrites it once the rule is there), while the rest of the mail change is applied, with the warning `the webmail on port N stays as it is` in the job's and the panel's log. A webmail newly put on a port is published by the next mail change (`mp mail apply`) once the rule is in place. The rule for `data/logs` stays; it does no harm to the old directories.

Together with this preparation (and at startup on hosts prepared by earlier versions) the panel installs its own policy module `monopanel` — `/etc/monopanel/selinux/monopanel.cil`, `semodule -i`. It currently holds two rules: `allow httpd_t self:capability sys_ptrace` and `allow httpd_t self:process ptrace`. The php-fpm slow log records a PHP trace by attaching to the worker process with `ptrace`; the master runs as root in `httpd_t`, the worker as the site's user, and without `CAP_SYS_PTRACE` the stock policy silently denies this: on EL all the slow logging produced was "executing too slow" warnings in the php-fpm log, with no traces. The rule gives the worker process nothing: without the `CAP_SYS_PTRACE` capability itself it can trace only processes of its own account, and only in `httpd_t`.

The package manager's runs (`monopanel-pkg-<run>.service`) and the agent itself empty their own `/tmp` and `/var/tmp` (`PrivateTmp=`) before systemd removes them: an `ExecStopPost`, `monopanel clean-private-tmp <unit>`, and only where the mount is that unit's own `/tmp`. The reason is Percona's `%post`: it leaves a directory labelled `rpm_script_tmp_t` in `/tmp`, and systemd removes a unit's directories in `init_t`, which the policy does not let remove such a directory — an AVC after every install and upgrade of Percona, and a directory kept until the next boot. The cleanup runs in `unconfined_service_t`, which may remove it, so the panel's policy module needs no rule for it; for the same reason neither unit has NoNewPrivileges: under it dnf would not reach `rpm_t`, and the cleanup would stay in `init_t`. The cleanup gives up after 30 seconds, and a run's `TimeoutStopSec` is always 90 seconds (an administrator's lower `DefaultTimeoutStopSec` does not apply), so it never turns a run into a failure. Checked on EL9 and EL10.

The planned `monopanel-selinux` package with the rest of the policy rules and `fcontext` entries:

- Docroot `/var/www(/.*)?` → `httpd_sys_content_t` (already in the base policy). Writable directories (`uploads`, `cache`, `tmp`, `logs`) → `httpd_sys_rw_content_t` via `semanage fcontext` + `restorecon` when the site is created; a user can mark any directory as "writable" from the UI.
- FPM sockets `/run/monopanel/php(/.*)?` → `httpd_var_run_t`.
- Own PHP builds: `/opt/monopanel/php/[^/]+/sbin/php-fpm` → `httpd_exec_t`, libraries → `lib_t`, configs → `httpd_config_t`, logs → `httpd_log_t`.
- EL has no apache mode: the panel installs and runs the accounts' Apaches (`monopanel-apache@<login>`) on Debian and Ubuntu only, so they need no SELinux rules.
- Booleans: `httpd_can_network_connect_db=1`, `httpd_can_sendmail=1`; `httpd_can_network_connect=1` — only if at least one site makes outgoing HTTP requests (a site flag, on by default, since almost any CMS does); `httpd_execmem=1` — only when ionCube/JIT is enabled.
- The panel (`monopaneld`) — unconfined in the first release, in v1.0 its own `monopanel_t` domain in the shipped module.
- `mp doctor` shows AVC denials from `ausearch` for the last 24 hours with a hint on how to fix them.

### 6.1 Living with enforcing

The panel keeps SELinux enforcing and is built for it: the policy above has been checked on the full matrix, including installing Bitrix, Sphinx 3 and memcached. The only source of tickets is the labels of files that got into a docroot bypassing the panel: `cp -a` and `rsync -X` from `/root` keep `admin_home_t`, and nginx answers with a 403 that cannot be told apart from a permissions error. What the panel has for this:

- `mp doctor` shows the SELinux mode and today's number of AVC denials for the web domain (nginx and php-fpm both run as `httpd_t`), with the latest denial and a `mp site fix <domain>` hint if the path lies inside a site's directory; in the web UI the dashboard has a "Fix <domain>" button next to that line (or "Fix all sites" when the record holds only a file name, not a path), and in permissive mode a "Back to enforcing" button. The agent reads the audit log through `ausearch`.
- `mp site fix <domain>` restores the directories and ACLs as at creation, makes the client the owner of the whole site tree and runs `restorecon -R` over `data/`. The panel restores the same labels itself after a move from another panel and after an archive is extracted in the file manager.
- Files the agent itself creates are labelled at once (`restorecon` after the write); files uploaded over SFTP or through the file manager inherit the directory's label and need no fixing.
- Everything under a site's docroot is web content: the panel's own rule `/var/www/[^/]+/data/www(/.*)?` → `httpd_sys_content_t` overrides the base policy rules for `logs` and `cgi-bin` directories inside the site (OpenCart writes `system/storage/logs/error.log`, the base rule made it `httpd_log_t`, php-fpm got a denial, and `restorecon` put the same label back). The rule set is versioned: `mp site fix` applies the current set if the host installed nginx with an older one. After a fix, doctor counts denials from the moment of the fix, not from midnight.

What SELinux gives with our booleans (`httpd_unified`, `httpd_can_network_connect`, `httpd_can_sendmail`, `httpd_execmem`): a compromised site cannot read `/etc/shadow`, `/root`, other users' home directories, mail data or databases, and postfix, dovecot, mysqld and fail2ban stay in their own domains. SELinux does not isolate sites from each other — separate users, php-fpm pools, ACLs and open_basedir do that.

If permissive is needed after all (for example, third-party software without a policy): `mp selinux permissive` or the button in "Settings". The panel first shows what this costs and asks for confirmation (`--yes` in scripts), then runs `setenforce 0` and writes `SELINUX=permissive` to `/etc/selinux/config` so the mode survives a reboot. From then on denials are only recorded in the audit log; `mp doctor` and the settings page keep warning until the mode is switched back: `mp selinux enforcing`. The panel does not offer `SELINUX=disabled`: on EL9 and EL10 it has no effect without the `selinux=0` kernel parameter and a reboot, and the panel does not need it.

## 7. Firewall and brute-force protection

- The panel's own `inet monopanel` table in nftables: an `input` chain with the panel's rules (ssh, 80/443 and the panel port are always open; allow, deny and bans from the UI and CLI). The rules live in `/etc/nftables.d/monopanel.nft` and are applied by the `monopanel-firewall` unit, so they survive a reboot. Managing the table through the `google/nftables` library instead of the rules file and `nft`, rate limits on ssh and the panel port, and geo-blocking are planned.
- EL: firewalld is not removed. The panel does not manage it yet: if it is running (Oracle Linux images ship with it, Alma and Rocky do not), `mp setup` opens the panel's port in it, installing nginx opens 80 and 443, installing mail opens the mail ports (`firewall-cmd --permanent` + `--reload`); `mp firewall enable` stops and disables firewalld, and from then on the panel runs the table. Managing the zone over D-Bus and a choice during `mp setup` come later. Ubuntu: the same for ufw.
- fail2ban: jails `sshd`, `monopanel` (the panel's log), `nginx-http-auth`, `nginx-botsearch`; the action is `nftables-multiport`.
  - The nginx jails read the shared log and every site's own logs (`<domain>.access.log`, `<domain>.error.log` in `/var/log/monopanel/sites/<login>/`). The list is explicit rather than a mask: `*.error.log` would also catch `<domain>.php.error.log`. The panel rewrites it when a site is created or deleted.
  - After the list changes the panel restarts fail2ban instead of reloading it: a reload goes through `fail2ban-client`, which SELinux keeps in its own domain (`fail2ban_client_t`) — it could not see the site logs in the accounts' directories and silently dropped them from the jails. The server itself (`fail2ban_t`) reads them. Bans survive the restart: fail2ban keeps them in its database.
  - A fail2ban that is enabled but not running — systemd refused a restart for its start limit (every new site restarts fail2ban, and an import creates them back to back), or it crashed — is restarted by the next refresh of the jails (a site created or deleted, a start of the panel) even when the list did not change: once a refresh, logged in the panel's journal. One the administrator disabled (`systemctl disable --now fail2ban`) stays off. A site log the jails list that appears after fail2ban started (a site suspended under 0.8.19, unsuspended) restarts a running fail2ban once, so that it reads the file.
  - Trusted addresses (`mp firewall trust`, the Firewall page) go to `ignoreip`.
  - The panel rewrites `/etc/fail2ban/jail.d/monopanel.local` in full. Your own settings go to `/etc/fail2ban/jail.d/zz-local.local`: fail2ban reads it later and the panel never touches it.
  - On EL only `fail2ban-server` is installed: the `fail2ban` meta package pulls in `fail2ban-sendmail` (and with it some mail server — exim on a host without postfix) and `fail2ban-firewalld` with firewalld itself. The panel bans through nftables and sends no mail. Where the meta package is already installed, `mp doctor` names the extras and the commands: `dnf mark install fail2ban-server fail2ban-selinux`, then `dnf remove fail2ban fail2ban-sendmail fail2ban-firewalld` (without the first command the removal would take the server along — it counts as a dependency).

## 8. Sphinx for 1C-Bitrix

The `sphinx` extension installs the search server that Bitrix expects in "Settings → Search → Sphinx" (connection `127.0.0.1:9306`, index `bitrix`). What the testbed turned up:

- **Debian/Ubuntu**: the `sphinxsearch` package (2.2.11) from the distribution, a config following Bitrix's reference "for 2.X" (`rt_field`/`rt_attr_*`), `/etc/default/sphinxsearch` with `START=yes`. The unit is generated from a SysV script: `systemctl enable` does not accept it ("generated"), and the panel ignores that — the package's rc links start the daemon anyway.
- **EL**: there is no package. Manticore does not fit: its `SHOW TABLES` returns a `Table` column, while `search/tools/sphinx.php` reads `$res['Index']` — Bitrix answers "Указанный индекс не найден" (the specified index was not found). What gets installed is the Sphinx 3.9.1 build from sphinxsearch.com (tar.gz, the checksum is built into the panel), user `sphinx`, `/opt/monopanel/sphinx`, unit `monopanel-sphinx.service`, config `/etc/sphinx/sphinx.conf`.
- **Sphinx 3 silently ignores the 2.x spelling** (`rt_attr_timestamp` and the other `rt_attr_*`): the index came up without `date_change`/`date_to`/`date_from`. The config for 3.x follows Bitrix's reference "for 3.X": `field`, `attr_uint`, `attr_string`, `attr_uint_set`, dates as `attr_uint` (Bitrix accepts `uint` and `timestamp`).
- **An existing index on disk wins over the config**: searchd logs `attribute count mismatch … EXISTING INDEX TAKES PRECEDENCE` and keeps the old schema. So after installing, the panel runs `DESCRIBE bitrix`, and if columns from Bitrix's list are missing, it stops the daemon, deletes `bitrix.*` and the binlog from the data directory and starts it again (Bitrix fills the index by reindexing). Removing the extension also removes the index files.
- `systemctl restart` returns before searchd starts listening on its ports: the `SHOW TABLES` check is retried for up to 15 s while the client answers "Can't connect".
- The `_` character cannot be in both `charset_table` and `blend_chars` — the index does not come up (NOT SERVING).
- sphinxsearch.com serves the archive (40 MB) slowly, up to a minute and a half: the panel's downloads are limited not by an overall timeout but by idle time (a minute without data).

## 9. Valkey per account

`mp stack install valkey` installs the server, `mp valkey add cache|sessions --user <login>` creates an account's instance. What the testbed showed (Debian 13 — Valkey 8.1, Ubuntu 22.04 — Redis 6.0, AlmaLinux 10 and Rocky 9 — Valkey 8.0, SELinux enforcing):

- **The package's shared instance** listens on 127.0.0.1:6379 with no password, for every account on the host: Debian and Ubuntu start it on install, EL does not. The panel stops and disables it (`valkey-server`, `valkey`, `redis-server`).
- **The configuration is in the arguments** of `ExecStart`: the process runs as the account, and `/etc/monopanel` is not readable to it (0750 root:monopanel). `--port 0`, `--unixsocket … --unixsocketperm 600`, the socket directory is `RuntimeDirectory=` with mode 0700, snapshots go to `StateDirectory=`. Redis 6.0, Valkey 7.2, 8.0 and 8.1 all understand the same arguments.
- **`Type=notify` and `--supervised systemd`**, as in the packages' own units. With `Type=simple` a restart returned before the server had loaded the snapshot and opened the socket, and the first PHP request lost its session.
- **Sessions** are held by the `sessions` instance: `volatile-lru` (only keys with a TTL are evicted, and for phpredis sessions the TTL equals `session.gc_maxlifetime`) and `--save 60 1`; on stop the server writes a final snapshot, so sessions survive a restart. `cache` has `allkeys-lru` and `--save ""`.
- **SELinux: the snapshot directory label.** Sockets are `redis_var_run_t` and snapshots `redis_var_lib_t`, as with the package: the policy already lets php-fpm (`httpd_t`) reach a `redis_t` socket. But init_t has no `add_name` in `redis_var_lib_t` directories (in `redis_var_run_t` it does — through the `pidfile` attribute), so the rule is on `/var/lib/monopanel-valkey/[^/]+(/.*)?`, while `/var/lib/monopanel-valkey` itself stays `var_lib_t`. With the label on the parent, every `StateDirectory=` failed with `238/STATE_DIRECTORY` and EACCES from `mkdirat` — and with no AVC, even with `semodule -DB`.
- **SELinux: NoNewPrivileges.** `ProtectKernelTunables=`, `ProtectKernelModules=` and `RestrictAddressFamilies=` with a non-root `User=` turn on NoNewPrivileges. Under it the init_t → redis_t transition is allowed only by an `nnp_transition` rule: the EL10 policy has one, EL9 (selinux-policy 38.1) does not — the kernel silently left the server in init_t, where it could not write its snapshot ("Failed opening the temp RDB file … Permission denied") and on stop refused to exit until SIGKILL. So the unit has only "mount" isolation: `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `ProtectControlGroups`.
- **Isolation is verified**: another account, `www-data`, `nginx` and `apache` get "Permission denied" on the socket, and there are no TCP ports; a PHP session written through the site's php-fpm goes into the instance on all four OSes and survives `mp valkey restart sessions`.
