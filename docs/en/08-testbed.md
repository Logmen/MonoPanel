# Testbed

The testbed is a Proxmox host with one virtual machine for each distribution in the [platform matrix](02-platform-matrix.md): Debian 12 and 13, Ubuntu 22.04, 24.04 and 26.04, AlmaLinux 9 and 10, Rocky Linux 9 and 10, Oracle Linux 9 and 10. The machines are built from the official cloud images with cloud-init, and each gets a `clean` snapshot right after its first boot, so a run always starts from a clean OS. Everything specific to a particular host — address, network, key — lives in the git-ignored `.dev/testbed.env`; the repository holds only the scripts.

## How it works

| File | Where it runs | What it does |
|---|---|---|
| `scripts/testbed/pve.sh` | on the Proxmox host, as root | downloads the images, creates the machines (`q35`, OVMF, `cpu: host`, virtio-scsi, cloud-init on `scsi1`), waits for cloud-init and the guest agent, takes and rolls back the `clean` snapshot |
| `scripts/testbed/testbed.sh` | locally | runs `pve.sh` over ssh with the settings from `.dev/testbed.env`, writes the `mp-<name>` ssh aliases to `~/.ssh/config.d/monopanel-testbed`, builds the package, deploys the panel and runs e2e |
| `scripts/testbed/bootstrap.sh` | on the machine, as root | installs the `.deb`/`.rpm` (or a bare binary), runs `mp setup`, then uses the panel to install nginx, a PHP branch, a database server and optionally apache/fail2ban/firewall/mail; finishes with `mp doctor` |

Machine `n` gets vmid `900+n`, the address `TB_PREFIX.(TB_IP_BASE+n)` and the hostname `mp-<distro>`. The cloud-init disk is attached to virtio-scsi, not to `ide2`: Debian's `cloud` kernel has no AHCI driver, and on q35 the guest does not see a disk on `ide2`. The host can tell that a machine is ready only by the guest agent answering, and the wrapper asks over ssh whether cloud-init has finished — on EL, `guest-exec` is blocked by the agent's filter and by SELinux, and the Oracle images ship with the agent already running. Oracle images are named by update and build (`OL9U8_x86_64-kvm-b293`); new ones appear at yum.oracle.com/oracle-linux-templates.html. The firmware is OVMF without pre-enrolled Secure Boot keys, because the EL10 images require x86-64-v3 and it is simpler for all machines to boot the same way.

## Configuration

```
# .dev/testbed.env
PVE_HOST=root@pve.example.com
TB_PREFIX=192.0.2          # first three octets of the machines' network
TB_GATEWAY=192.0.2.1
TB_IP_BASE=200             # machine n → 192.0.2.(200+n)
TB_SSH_KEY_FILE=~/.ssh/id_ed25519
```

## Commands

```
make testbed-up                    # create the missing machines, boot them, take the clean snapshot
make testbed-status                # table: vmid, name, address, state, snapshot
make testbed-deploy VM=debian13    # build the package, install the panel and the stack
make testbed-e2e VM=debian13       # e2e tests against this machine
make testbed-reset VM=debian13     # roll back to clean
make testbed-matrix                # reset + deploy + e2e on all machines in parallel, summary table
make testbed-migrate SRC=ubuntu2404 DST=debian13   # move an account between two machines and check it
make testbed-sources ARGS="migrate bitrixvm debian13"   # move from BitrixVM / FASTPANEL (source machines)
make testbed-full                  # everything for a release: matrix, migrations, CMS, foreign sources, doctor — with a final summary
make testbed-down                  # delete the machines (the images stay in the cache)
```

`VM=` can be left out where the command makes sense for all machines. The logs of a parallel run are in `.dev/testbed/logs/<name>.log`. The default stack is nginx, PHP 8.4, Percona; it is overridden with the variables `TB_PHP`, `TB_DB` (`percona`, `mysql`, `none`) and `TB_EXTRA` (`apache fail2ban firewall mail`).

The panel on a machine serves a self-signed certificate for the machine's address, so e2e runs with `MONOPANEL_INSECURE=1` — the wrapper sets it itself. The token for the tests is minted over the root socket (`mp token create`) and revoked after the run; the administrator password is not stored anywhere: it is printed once, on the first `bootstrap.sh`.

## Full run

`make testbed-full` (also `scripts/testbed/full-run.sh`; `STAGES="matrix cms"` runs only some of the stages) does everything that is checked before a release and prints a summary: the matrix on all machines, three migrations between panels, four CMSs on each machine (a CMS that failed on a network error is reinstalled once more), three foreign sources, `mp doctor` everywhere with the number of failures and warnings. The logs are in `.dev/testbed/logs/`, the passwords of the panels and of the CMS admin areas in `.dev/panels.txt` and `.dev/cms.txt`. The result is `exit 1` if anything at all failed. The CMS installers now also write `note` lines to the log — retries and skipped steps of the Bitrix wizard, which used to get lost when an install succeeded.

Each machine's panel certificate is issued once: Let's Encrypt has a limit of five certificates per name per week, and a rollback to the snapshot used to issue a new one. `testbed.sh deploy` keeps the issued certificate in `.dev/testbed/certs/<machine>/` and installs it back (`mp ssl panel import`) as long as more than a month remains until it expires.

## DNS names

The machines sit in a private network, so HTTP-01 cannot reach them, but names and DNS-01 do work. If `TB_ZONE` (a zone in Cloudflare) and `TB_CF_TOKEN` (an API token with the Zone:DNS:Edit permission on it) are set in `.dev/testbed.env`, `make testbed-dns` (also `testbed.sh dns up`) creates `A` records `<name>.tb.<zone>` and `*.<name>.tb.<zone>` for each machine (the `tb` label is changed with `TB_DNS_LABEL`), without proxying; `dns down` removes them, `dns list` shows them. With a zone set, `deploy` refers to the panel by name rather than by address, registers Cloudflare in the panel as the DNS provider `cf` — the token travels to the machine over stdin and is never printed — and right away issues the panel a production certificate (`mp ssl panel issue --dns cf`; a failure does not break the deploy, and the panel stays on the self-signed one). A site certificate is issued with `mp ssl issue <name> --dns cf`; on the testbed it makes sense to use `--staging`, so as not to run into Let's Encrypt's limit on duplicate certificates for the same names with frequent rollbacks to `clean`. Tested on 2026-09-09: the alma9 panel's production certificate obtained via DNS-01 is trusted from the workstation, and the site `site1.rocky10.tb.<zone>` with a staging certificate is served by nginx over HTTPS (the site is created with `--ssl none`, then `mp ssl issue … --dns cf` and `mp site set … --ssl auto` — the panel picks up the already issued certificate instead of using HTTP-01). Mind the resolver's negative cache: a name under the wildcard that was looked up before the record appeared may be remembered by a public resolver as non-existent for up to half an hour.

## Migration between panels

Any two testbed machines make a ready pair for a [migration](07-migration.md). `scripts/testbed/migrate.sh` (also `make testbed-migrate`) creates an account on the source with a site, a PHP file, a database with data and a cron job, issues a token (`mp migrate grant`), runs `mp migrate plan` and `mp migrate run` on the target with `--insecure` (the certificates are self-signed) and checks that everything arrived: the unix account with the same password hash, signing in to the panel with the old password, the site answering with the same PHP file, the rows in the database and its user's password, the job in the crontab, `mp doctor` without errors. The account stays on both machines for inspection; `make testbed-reset` removes it along with everything else.

## Foreign panels as sources

Besides the matrix, the testbed has three source machines for [moving in from foreign panels](07-migration.md#6-foreign-panels-and-servers-without-a-panel): `bitrixvm` (AlmaLinux 9, vmid 913, .213), `bitrixvm7` (CentOS 7 with bitrix-env 7 — both long past EOL, which is still what old Bitrix servers run on; vmid 914, .214, SeaBIOS, because the CentOS 7 image has no EFI partition; cloud-init switches the repositories to vault.centos.org) and `fastpanel` (Debian 12, vmid 912, .212). They are not part of the matrix (`pve.sh` keeps them in a separate `SOURCES` table), but `up`, `reset`, `status` and `dns` know them. `scripts/testbed/sources.sh` (also `make testbed-sources ARGS=…`):

- `install bitrixvm` installs bitrix-env 9 with the official `bitrix-env-9.sh` (it requires SELinux to be switched off and a reboot — the script does this itself, ~15 minutes) and creates the management pool, without which `bx-sites` does not work; `install bitrixvm7` runs the old `bitrix-env.sh` on CentOS 7, with EPEL taken from the archive beforehand and Remi and Percona from their still-live EL7 trees (the installer's link to `epel-release-latest-7` is dead, and it skips the repositories it finds already in place); `install fastpanel` runs `install_fastpanel.sh` (the `fastuser` password goes into `.dev/sources.txt`).
- `seed bitrixvm|bitrixvm7 [FROM]` puts into `/home/bitrix/www` the Bitrix site that `testbed.sh cms FROM bitrix` installed on a matrix machine (`alma9` by default), with its database in `sitemanager` and bitrix-env credentials in `.settings.php`; the main site stays under `server_name _`, as on real servers. `seed fastpanel [FROM]` creates the account `shop` with WordPress from the same machine, a site with docroot `public/`, an alias and an allow-list, a database, a crontab with `data/bin/php`, a real Let's Encrypt certificate (DNS-01 through the FROM panel) and a mailbox. Without a license from its billing, FASTPANEL lets you into neither the API nor the interface, so the rows for the account, sites, backends, databases, certificates and cron are written straight into `fastpanel2.db` — the way a live panel writes them (checked against a working FASTPANEL 1.11); the files, nginx, php-fpm and MySQL are real.
- The CentOS 7 image leaves libvirt's dead nameserver (192.168.122.1) first in `resolv.conf`: every lookup waits 5 seconds, an ssh login 40, and a migration dry run takes minutes. `pve.sh` removes it on every boot, and `prepare_centos7` once more before the install.
- `migrate bitrixvm|bitrixvm7 DST` and `migrate fastpanel DST` give root on the DST machine a key to the source, install the source's PHP branch there, run `mp migrate plan` and `run` and check: the site answers at the new address (Bitrix with `X-Powered-CMS`, WordPress over HTTPS with the certificate that came along), the `/home/bitrix` paths are rewritten, the database login works with the old password or hash, the unix hash matches the source, cron and the allow-list are in place.

The passwords that `seed` makes up are in `.dev/sources.txt` (git-ignored). The run of 2026-09-12: BitrixVM → debian13 and FASTPANEL → rocky9 with no issues.

## CMS on top of presets

`make testbed-cms NAME=<machine> [CMS="wordpress joomla opencart bitrix"]` (also `testbed.sh cms`) installs CMSs on a machine with the panel's own tools — `mp cms install` — into sites with the matching presets, and checks that the site works and the preset's rules take effect. DNS names are required (`dns up`): the sites are called `<cms>.<machine>.tb.<zone>` and are served over HTTP — HTTP-01 cannot reach into the private network, and TLS is not needed to check presets. The panel user is `cms`; a site that already has files is reinstalled with `--force`.

- **WordPress**: the home page and the login, `/wp-admin/` redirects to the login, `xmlrpc.php` is closed (403), PHP in `wp-content/uploads/` does not execute, a pretty URL returns 200.
- **Joomla**: the home page, `/administrator/`, closed `configuration.php` and `cache/`, `/api/` answers 401, PHP in `images/` does not execute, a SEF route works, `installation/` is removed.
- **OpenCart**: the storefront, `/admin/`, closed `system/` and `storage/`, `.twig` is not served, an SEO URL goes to `_route_`, `install/` is removed.
- **1C-Bitrix** (the trial "Start" edition): `X-Powered-CMS: Bitrix Site Manager` on the home page, the `/bitrix/admin/` login form, closed `php_interface/dbconn.php`, `modules/`, `.settings.php`, `cache/`, PHP in `upload/` does not execute, a non-existent address is handled by `urlrewrite.php`, `mp site php` shows `short_open_tag On`, `max_input_vars 20000`, `memory_limit 512M`.

Admin credentials are printed once, as `CREDS` lines. What had to be dealt with while the scenario was still manual: on EL, Percona comes with `validate_password` (a database password must contain all character classes, and that is how the panel generates them); files copied with `cp -a` from `/root` keep the SELinux label `admin_home_t` on EL — `mp site fix` now repairs that, and the panel labels the files itself after unpacking; `mp db create` on an existing database does not fail but resets the password.

## What the first run found (2026-09-09)

Before the testbed, the panel had been tested only on Ubuntu 24.04. The very first round over the whole matrix turned up six bugs and the first move a seventh, and each of them reproduced only on a real OS — the test with a fake agent could not see them:

| Where | What happened | What was done |
|---|---|---|
| Ubuntu 26.04 | `ppa:ondrej/php` has no builds for resolute, and `apt update` failed with a 404 | the panel checks `dists/<codename>/Release` before adding the source, installs PHP from Ubuntu itself (8.5), marks unavailable branches in the branch list with the reason, checks the PPA again on the next install and cleans up a broken source after itself |
| EL9, EL10 | nginx did not start: `nginx -t`, which the agent uses to check the configuration, created `/run/nginx.pid` with the `var_run_t` label, and the `httpd_t` domain could not open it | the config set gained a `restore` field; after writing files and creating directories the agent runs `restorecon`; on the first install of nginx/PHP the panel sets up `fcontext` and the SELinux booleans for hosting ([02](02-platform-matrix.md#6-selinux-el9--el10)) |
| EL9 | `semanage fcontext` rejects `/run/...` because of the `/run` ↔ `/var/run` equivalence rule (on EL10 it is the other way round) | the panel follows semanage's hint |
| EL9, EL10 | Percona installs with a temporary, expired root password in `/var/log/mysqld.log`, and the `auth_socket` plugin is not loaded | the panel reads the password from the log, sets a fresh one (an expired one allows only `ALTER USER`), loads the plugin and moves root to the socket; for the three queries this takes, the password lives only in memory and is passed through the environment |
| EL (Percona) | `validate_password` MEDIUM rejected about half of the generated database passwords — a random string with no guaranteed special character; Rocky passed by chance | the `auth.NewPassword` generator guarantees every character class; it is used for database, mailbox and Roundcube passwords |
| Ubuntu 24.04 | `repo.percona.com` answered with a TLS timeout, and the whole install failed | downloads of repository keys and packages are retried on network errors |
| migration | a `caching_sha2_password` hash with a binary salt got corrupted in the text output of `SHOW CREATE USER` (`ERROR 1827`) | the source asks the server to print the hash as a hex literal |

After the fixes, a full `make testbed-matrix` run from clean snapshots is green on all machines (Ubuntu 26.04 with PHP 8.5, the rest with 8.4; nginx 1.30, Percona 8.4), and the migrations Ubuntu 24.04 → Debian 13 and Debian 12 → AlmaLinux 10 pass all checks.

Oracle Linux 9 and 10 were added on 2026-09-10 and turned up two more quirks: the Oracle images come with firewalld enabled (ssh only) — now `mp setup` opens the panel port in it, installing nginx opens 80 and 443, installing mail opens its ports, and `mp firewall enable` switches firewalld off; and `remi-release-10` requires `epel-release = 10`, which the `oracle-epel-release-el10` package does not provide — on OL10 the panel installs the official `epel-release`. After that, e2e is green on both, and the move Ubuntu 24.04 → Oracle Linux 9 passed all checks.

The testbed itself taught a few lessons as well: a cloud-init disk on `ide2` is invisible to Debian's `cloud` kernel; on EL the guest agent forbids `guest-exec` (lifted in `/etc/sysconfig/qemu-ga`), and SELinux still does not let it run commands — so on EL a machine's readiness is judged by the agent answering; `pkill -f` inside a single ssh command also kills that command itself; parallel `deploy` runs should not each build the package on their own — `matrix` builds it once.
