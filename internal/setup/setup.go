// Package setup implements `mp setup`: it runs as root before the services
// exist and prepares accounts, directories, config, secrets, TLS, the
// database with an admin user and the systemd units.
package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent"
	"monopanel/internal/api"
	"monopanel/internal/auth"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/i18n"
	"monopanel/internal/mailpolicy"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// T is the text in the language of the terminal, detected by the CLI.
func T(ru, en string) string { return i18n.T(ru, en) }

// Options controls setup.
type Options struct {
	ConfigPath    string
	Hostname      string
	Listen        string
	AdminLogin    string
	AdminPassword string
	InstallUnits  bool
	BinaryPath    string
	Start         bool
}

// Result summarises what was done.
type Result struct {
	ConfigPath        string   `json:"config_path"`
	DBPath            string   `json:"db_path"`
	AdminLogin        string   `json:"admin_login"`
	AdminPassword     string   `json:"admin_password,omitempty"`
	GeneratedPassword bool     `json:"generated_password"`
	AdminCreated      bool     `json:"admin_created"`
	Fingerprint       string   `json:"tls_fingerprint_sha256"`
	URLs              []string `json:"urls"`
	Healthy           bool     `json:"healthy"`
}

const agentUnit = `[Unit]
Description=MonoPanel privileged agent
After=network.target

[Service]
Type=simple
ExecStart=%s agent
# Its /tmp is emptied before systemd removes it: on EL, systemd may not
# remove what package scripts leave there (rpm_script_tmp_t), and up to
# 0.8.19 they ran as the agent's children. Only this unit's own is touched.
ExecStopPost=-%s clean-private-tmp monopanel-agent.service
Restart=on-failure
RestartSec=2
# The agent runs as root on purpose: it installs packages, makes unix users,
# writes /etc and reloads services. What it never needs is fenced off here.
# Homes under /home and /root stay read-only (clients live in /var/www),
# kernel modules, tunables, cgroups, the clock and the host name are out of
# reach, and /tmp is its own. ProtectSystem= and a capability set are not
# possible: apt/dnf write everywhere and package scripts need every capability.
ProtectHome=read-only
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
PrivateTmp=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
`

const apiUnit = `[Unit]
Description=MonoPanel API and web interface
After=network-online.target monopanel-agent.service
Wants=network-online.target
Requires=monopanel-agent.service
# The HTTPS port comes from monopanel-api.socket: PID 1 keeps it bound while
# the API restarts. Wants=, not Requires=: without the socket the API listens
# on the port itself.
Wants=monopanel-api.socket
After=monopanel-api.socket
# A unit that runs into its start limit takes its socket down with it, and
# the port would be free.
StartLimitIntervalSec=0

[Service]
Type=simple
User=monopanel
Group=monopanel
ExecStart=%s api
Restart=on-failure
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/monopanel /run/monopanel /var/log/monopanel
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
`

// apiSocketUnit holds the panel's HTTPS port for monopanel-api. Its address
// follows web.listen through a drop-in MonoPanel writes
// (agent.SyncAPISocket); 8443 is that of the default web.listen, :8443.
const apiSocketUnit = `[Unit]
Description=MonoPanel HTTPS port of the API

[Socket]
# PID 1 binds the panel's HTTPS port and hands it to monopanel-api.service,
# also while the API restarts: no local account can take the port then. The
# address comes from web.listen in config.yaml (monopanel-api.socket.d/listen.conf).
# BindIPv6Only=both takes IPv4 as well, as the API's own listener on :8443
# does; FreeBind=yes binds an address the network does not have yet at boot.
ListenStream=8443
Accept=no
BindIPv6Only=both
FreeBind=yes

[Install]
WantedBy=sockets.target
`

// apacheUnit is the Apache of one account's sites in Apache mode,
// monopanel-apache@<login>.service, which the API enables and starts through
// the agent. The parent runs as root, as Debian's apache2.service does — it
// binds the account's address on port 81 and opens the logs, which only
// root writes — and the children as the account (User and Group of
// httpd.conf, which the agent writes only for a hosting account). Debian's
// apache2 has no mod_systemd: Type=simple with -DFOREGROUND keeps the parent
// the main process, and USR1 is its graceful restart, which keeps the
// listening socket. The bounding set is what the parent needs: bind port 81,
// switch the children to the account (and set their groups), signal them,
// hand them the lock files (chown), and pass the account's 0750 home to
// check each DocumentRoot as it reads the configuration (CAP_SETUID gives it
// that much already). The runtime directory, the pid file in it, goes when
// the unit stops: logrotate signals no stale pid. A stop is graceful
// (SIGWINCH): when the account's last site leaves the Apache, the requests
// nginx's old workers still have in flight there finish — within a minute,
// so that neither the job nor the host's shutdown waits longer.
const apacheUnit = `[Unit]
Description=Apache for the sites of %i in Apache mode (MonoPanel)
After=network.target remote-fs.target nss-lookup.target
# MonoPanel writes the configuration of an account that has sites in Apache
# mode; without it there is nothing to start.
ConditionPathExists=/etc/apache2/monopanel/accounts/%i/httpd.conf

[Service]
Type=simple
ExecStart=/usr/sbin/apache2 -f /etc/apache2/monopanel/accounts/%i/httpd.conf -DFOREGROUND
ExecReload=/bin/kill -USR1 $MAINPID
# A stop is Apache's graceful-stop: the parent stops listening, lets every
# child finish the request in hand (those of nginx's old workers, after a
# reload sent the site elsewhere) and exits; mixed: the signal goes to the
# parent alone, and what is left after it exited, or after the timeout, is
# killed.
KillMode=mixed
KillSignal=SIGWINCH
TimeoutStopSec=60
Restart=on-failure
RestartSec=2
RuntimeDirectory=monopanel-apache/%i
UMask=0027
LimitNOFILE=8192
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_KILL CAP_CHOWN CAP_DAC_READ_SEARCH

[Install]
WantedBy=multi-user.target
`

// mailPolicyUnit is the panel's mail policy (internal/mailpolicy), a milter
// postfix asks before opendkim signs, as its own system user, on the
// listener of a socket unit: mailPolicyUnixSocketUnit (Debian, Ubuntu) or
// mailPolicyInetSocketUnit (EL); mailGuardUnit fences the loopback ports of
// EL. The API enables and starts them with the mail server, mp setup none.
const mailPolicyUnit = `[Unit]
Description=MonoPanel mail policy (postfix milter: no account sends as another account's domain)
After=network.target
# Postfix asks it about every message an account sends, before opendkim
# signs; while it is down, mail waits (tempfail): it starts before postfix
# (postfix@-.service is the postfix master of Debian 12 and Ubuntu up to
# 24.04), and systemd never gives up restarting it. Its listener is a socket
# unit's, which PID 1 holds while the service restarts or is down:
# monopanel-mailpolicy-unix.socket on Debian and Ubuntu,
# monopanel-mailpolicy-inet.socket on EL. The panel enables and starts them
# with the mail server.
Before=postfix.service postfix@-.service
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%s mail-policy
Restart=always
RestartSec=1
# Its own system user: it parses mail from untrusted senders, so it is not
# root, not the API's user and not opendkim's, which holds the DKIM keys.
User=monopanel-mail
Group=monopanel-mail
UMask=0077
# It reads the policy (/etc/postfix/monopanel, readable) and /proc/net/tcp
# of the host's network namespace — no PrivateNetwork — and writes nothing.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=256M
Environment=GOMEMLIMIT=192MiB
TasksMax=256
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
`

const mailPolicyUnixSocketUnit = `[Unit]
Description=MonoPanel mail policy's socket (Debian and Ubuntu)
# PID 1 holds the socket from boot on, also while the policy restarts, is
# down or is killed: postfix's connections wait in its backlog rather than
# fail. It is in postfix's own private directory (postfix:root 0700), and
# postfix's own: only postfix and root may open it.
Before=postfix.service postfix@-.service

[Socket]
ListenStream=/var/spool/postfix/private/monopanel-mailpolicy
SocketUser=postfix
SocketGroup=postfix
SocketMode=0600
RemoveOnStop=yes
Accept=no
Service=monopanel-mailpolicy.service
# No rate limit on the activations: the listener stays whatever the
# service does.
TriggerLimitIntervalSec=0

[Install]
WantedBy=sockets.target
`

const mailPolicyInetSocketUnit = `[Unit]
Description=MonoPanel mail policy's port (EL)
# PID 1 holds 127.0.0.1:8890 (milter_port_t) from boot on, also while the
# policy restarts, is down or is killed: no local account can bind the port
# in between and get postfix's connections, and those wait in its backlog
# rather than fail. Only root and postfix may connect to it
# (monopanel-mailguard.service).
Before=postfix.service

[Socket]
ListenStream=127.0.0.1:8890
Accept=no
Service=monopanel-mailpolicy.service
# No rate limit on the activations: a socket past its limit would close
# and leave the port to whoever binds it first.
TriggerLimitIntervalSec=0

[Install]
WantedBy=sockets.target
`

const mailGuardUnit = `[Unit]
Description=MonoPanel mail guard (EL: only root and postfix connect to the local milter ports)
# opendkim (127.0.0.1:8891) and the mail policy (127.0.0.1:8890) listen on
# loopback ports, which every local process can reach: an account that
# spoke the milter protocol to opendkim itself would have any domain of the
# SigningTable signed. An nftables table of its own resets the connection of
# anyone but root and postfix to either port, from before the network and
# the accounts' processes start. The panel enables and starts it with the
# mail server on EL.
DefaultDependencies=no
After=local-fs.target
Before=network-pre.target opendkim.service postfix.service monopanel-mailpolicy-inet.socket shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%s mail-guard
# The table replaces itself in one transaction: a reload leaves no moment
# without it, as a stop and a start would.
ExecReload=%s mail-guard
ExecStop=%s mail-guard --remove
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// The panel's units (panelUnits).
const (
	agentService       = "monopanel-agent.service"
	apiService         = "monopanel-api.service"
	apiSocket          = "monopanel-api.socket"
	apacheUnitName     = "monopanel-apache@.service"
	mailPolicyUnitName = mailpolicy.Unit
	mailPolicyUnixName = mailpolicy.SocketUnitUnix
	mailPolicyInetName = mailpolicy.SocketUnitInet
	mailGuardUnitName  = mailpolicy.GuardUnit
)

// Run performs the setup.
func Run(ctx context.Context, opts Options, out io.Writer) (*Result, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("mp setup must run as root")
	}
	step := func(format string, a ...any) { fmt.Fprintf(out, "• "+format+"\n", a...) }
	profile, err := osprofile.Detect()
	if err != nil {
		return nil, err
	}
	step(T("ОС: %s (%s)", "OS: %s (%s)"), profile.Release().PrettyName, profile.Family())

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	// --hostname and --listen are checked by the rules PUT /system/config and
	// mp config set apply.
	if opts.Hostname != "" {
		if cfg.Web.Hostname, err = config.NormalizePanelValue(config.KeyHostname, opts.Hostname); err != nil {
			return nil, err
		}
	} else if cfg.Web.Hostname == "" {
		cfg.Web.Hostname, _ = os.Hostname()
	}
	if opts.Listen != "" {
		if cfg.Web.Listen, err = config.NormalizePanelValue(config.KeyListen, opts.Listen); err != nil {
			return nil, err
		}
	}
	res := &Result{ConfigPath: cfg.Path(), DBPath: cfg.DBPath()}

	step(T("группы и служебный пользователь: %s, %s, %s", "groups and the service user: %s, %s, %s"), cfg.ServiceGroup, cfg.WebGroup, cfg.CLIGroup)
	for _, g := range []string{cfg.ServiceGroup, cfg.WebGroup, cfg.CLIGroup} {
		if _, err := agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: g, System: true}); err != nil {
			return nil, err
		}
	}
	// The service user hands the API socket to the CLI group, so it has to be a member.
	if _, err := agent.EnsureUnixUser(ctx, profile, &agent.EnsureUnixUserRequest{Login: cfg.ServiceUser, System: true, PrimaryGroup: cfg.ServiceGroup, Home: cfg.DataDir, Groups: []string{cfg.CLIGroup}, Comment: "MonoPanel service"}); err != nil {
		return nil, err
	}
	// The mail policy's own user and group (sysusers.d does it where the
	// package is installed).
	if _, err := agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: mailpolicy.User, System: true}); err != nil {
		return nil, err
	}
	if _, err := agent.EnsureUnixUser(ctx, profile, &agent.EnsureUnixUserRequest{Login: mailpolicy.User, System: true, PrimaryGroup: mailpolicy.User, Home: "/", Comment: "MonoPanel mail policy"}); err != nil {
		return nil, err
	}

	step("%s", T("каталоги", "directories"))
	dirs := []agent.DirSpec{
		{Path: cfg.ConfigDir, Mode: 0o750, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.TemplatesDir, Mode: 0o750, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.DataDir, Mode: 0o751, Owner: cfg.ServiceUser, Group: cfg.ServiceGroup}, // o+x: nginx must traverse to acme/webroot
		{Path: filepath.Join(cfg.DataDir, "tls"), Mode: 0o700, Owner: cfg.ServiceUser, Group: cfg.ServiceGroup},
		// The agent's own, the configuration history in it: root's alone,
		// out of the data directory the API owns.
		{Path: cfg.AgentStateDir(), Mode: 0o700, Owner: "root", Group: "root"},
		{Path: cfg.ConfHistoryDir(), Mode: 0o700, Owner: "root", Group: "root"},
		// The API makes its socket here; the sticky bit keeps it from
		// renaming root's entries (the agent's socket, php/ with the pools'
		// sockets the php-fpm masters make as root).
		{Path: cfg.RunDir, Mode: 0o1771, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.WWWRoot, Mode: 0o711, Owner: "root", Group: "root"},
	}
	for _, d := range dirs {
		if _, err := agent.EnsureDir(d); err != nil {
			return nil, err
		}
	}
	// root:<service group> 1771 and sites/ root's: the API writes its audit
	// log there, but cannot rename sites/, whose logs nginx opens as root.
	if err := agent.PrepareLogDir(cfg.LogDir, cfg.ServiceGroup); err != nil {
		return nil, err
	}
	if err := os.WriteFile("/etc/tmpfiles.d/monopanel.conf", []byte(tmpfilesConf(cfg)), 0o644); err != nil {
		return nil, err
	}
	if wrote, err := keepPHPTmpfiles(phpTmpfilesFile, packageTmpfilesFile, cfg); err != nil {
		step(T("предупреждение: %s: %v", "warning: %s: %v"), phpTmpfilesFile, err)
	} else if wrote {
		step(T("tmpfiles: %s — каталог сокетов php-fpm при загрузке и для версии ниже 0.9.0", "tmpfiles: %s: the php-fpm socket directory at boot, for a version older than 0.9.0 too"), phpTmpfilesFile)
	}

	if _, err := os.Stat(cfg.Path()); errors.Is(err, os.ErrNotExist) {
		step(T("конфигурация: %s", "configuration: %s"), cfg.Path())
		if err := cfg.Save(cfg.Path()); err != nil {
			return nil, err
		}
	} else {
		step(T("конфигурация уже есть: %s", "configuration already exists: %s"), cfg.Path())
		if opts.Hostname != "" || opts.Listen != "" {
			// Only these keys change: the administrator's comments, other
			// settings and the release key stay as they are.
			set := map[string]string{}
			if opts.Hostname != "" {
				set[config.KeyHostname] = cfg.Web.Hostname
			}
			if opts.Listen != "" {
				set[config.KeyListen] = cfg.Web.Listen
			}
			if _, err := config.WriteFileKeys(cfg.Path(), set, -1); err != nil {
				return nil, err
			}
			step(T("обновлено: hostname=%s listen=%s", "updated: hostname=%s listen=%s"), cfg.Web.Hostname, cfg.Web.Listen)
		}
	}
	if err := chownName(cfg.Path(), "root", cfg.ServiceGroup, 0o640); err != nil {
		return nil, err
	}

	if _, err := os.Stat(cfg.SecretKeyFile); errors.Is(err, os.ErrNotExist) {
		step(T("секретный ключ: %s", "secret key: %s"), cfg.SecretKeyFile)
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(cfg.SecretKeyFile, []byte(hex.EncodeToString(key)+"\n"), 0o640); err != nil {
			return nil, err
		}
	}
	if err := chownName(cfg.SecretKeyFile, "root", cfg.ServiceGroup, 0o640); err != nil {
		return nil, err
	}

	certPath, keyPath := cfg.TLSCertPath(), cfg.TLSKeyPath()
	created, err := ensureSelfSigned(cfg, certPath, keyPath, api.LocalNames(cfg.Web.Hostname))
	if err != nil {
		return nil, err
	}
	if created {
		step(T("самоподписанный сертификат: %s", "self-signed certificate: %s"), certPath)
	}
	for _, p := range []string{certPath, keyPath} {
		if err := chownName(p, cfg.ServiceUser, cfg.ServiceGroup, 0); err != nil {
			return nil, err
		}
	}
	res.Fingerprint, _ = api.CertFingerprint(certPath)

	step(T("база данных: %s", "database: %s"), cfg.DBPath())
	if err := checkDBFiles(cfg.DBPath()); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	login := opts.AdminLogin
	if login == "" {
		login = "admin"
	}
	res.AdminLogin = login
	password := opts.AdminPassword
	existing, err := db.GetUserByLogin(ctx, login)
	switch {
	case err == nil:
		if password != "" {
			h, err := auth.HashPassword(password)
			if err != nil {
				db.Close() //nolint:errcheck // cleanup
				return nil, err
			}
			if err := db.SetUserPassword(ctx, existing.ID, h); err != nil {
				db.Close() //nolint:errcheck // cleanup
				return nil, err
			}
			step(T("пароль администратора %s обновлён", "administrator %s: password updated"), login)
		} else {
			step(T("администратор %s уже существует", "administrator %s already exists"), login)
		}
	case errors.Is(err, store.ErrNotFound):
		if password == "" {
			password, err = auth.NewToken(15)
			if err != nil {
				db.Close() //nolint:errcheck // cleanup
				return nil, err
			}
			res.GeneratedPassword = true
		}
		h, err := auth.HashPassword(password)
		if err != nil {
			db.Close() //nolint:errcheck // cleanup
			return nil, err
		}
		if err := db.CreateUser(ctx, &store.User{Login: login, Role: store.RoleAdmin, PasswordHash: h}); err != nil {
			db.Close() //nolint:errcheck // cleanup
			return nil, err
		}
		res.AdminCreated = true
		res.AdminPassword = password
		step(T("администратор %s создан", "administrator %s created"), login)
	default:
		db.Close() //nolint:errcheck // cleanup
		return nil, err
	}
	db.Audit(ctx, store.AuditEntry{Actor: "root", Action: "setup", Target: login})
	db.Close() //nolint:errcheck // cleanup
	if err := chownTree(cfg.DataDir, cfg.ServiceUser, cfg.ServiceGroup, cfg.LegacyConfHistoryDir()); err != nil {
		return nil, err
	}

	binary := opts.BinaryPath
	if binary == "" {
		binary, _ = os.Executable()
		binary, _ = filepath.EvalSymlinks(binary)
	}
	sd, err := systemd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer sd.Close()
	units, err := installUnits(ctx, systemUnitDir, packageUnitDir, cfg, binary, opts.InstallUnits, sd.DaemonReload)
	for _, p := range units.Removed {
		step(T("systemd: удалён %s — копия юнита пакета, которую сделала панель; действует юнит пакета", "systemd: removed %s, a copy of the package's unit the panel made: the package's unit applies"), p)
	}
	if len(units.Wrote) > 0 {
		step("systemd units: %s (%s)", strings.Join(units.Wrote, ", "), binary)
	}
	for _, p := range units.Shadowing {
		step(T("предупреждение: %s заменяет юнит пакета, и его обновления с пакетом сюда не доходят; если он не нужен, удалите его и запустите mp setup ещё раз",
			"warning: %s overrides the package's unit, and the package's updates of it do not reach this host; unless you keep it on purpose, remove it and run mp setup again"), p)
	}
	if units.PruneErr != nil {
		step(T("предупреждение: копии юнитов в %s: %v", "warning: copies of units in %s: %v"), systemUnitDir, units.PruneErr)
	}
	if err != nil {
		return nil, err
	}
	if units.SocketErr != nil {
		step(T("предупреждение: %v; monopanel-api будет слушать web.listen сам, и пока он перезапускается, порт свободен: исправьте web.listen и запустите mp setup ещё раз",
			"warning: %v; monopanel-api will listen on web.listen itself, and the port is free while it restarts: fix web.listen and run mp setup again"), units.SocketErr)
	}
	if units.Moved {
		step(T("%s: адрес из web.listen (%s)", "%s: the address of web.listen (%s)"), apiSocket, cfg.Web.Listen)
	}
	if ip := agent.ForeignListenIP(cfg.Web.Listen); ip != nil {
		step(T("предупреждение: %s — не адрес этого сервера; сокет порта панели всё равно займёт его, и панель будет слушать там, где её никто не достанет", "warning: %s is not an address of this server; the socket of the panel's port takes it all the same, and the panel would listen where nobody reaches it"), ip)
	}
	masked, err := enablePanel(ctx, sd)
	if err != nil {
		return nil, err
	}
	if masked {
		step(T("%s замаскирован: monopanel-api слушает порт сам, как решил администратор (systemctl unmask %s, затем mp setup, вернёт сокет)",
			"%s is masked: monopanel-api listens on the port itself, as the administrator chose (systemctl unmask %s, then mp setup, brings the socket back)"), apiSocket, apiSocket)
	}
	// Enabling creates symlinks and flags NeedDaemonReload; clear it.
	if err := sd.DaemonReload(ctx); err != nil {
		return nil, err
	}
	if opened, err := openFirewalld(cfg.Web.Listen); err != nil {
		step(T("предупреждение: firewalld: %v", "warning: firewalld: %v"), err)
	} else if opened != "" {
		step(T("firewalld: открыт порт %s", "firewalld: port %s opened"), opened)
	}
	// The package ships the rule; an install from the binary alone gets it here.
	modprobeStep(step)
	if opts.Start {
		step("%s", T("запуск сервисов", "starting the services"))
		// A masked socket starts for nobody: the API listens itself.
		fresh := !masked && (slices.Contains(units.Wrote, apiSocket) || units.Moved)
		if !fresh && !masked {
			st, err := sd.Status(ctx, apiSocket)
			fresh = err != nil || agent.APISocketStale(st, modTime(agent.APISocketDropIn(cfg)))
		}
		warn := func(err error) {
			step(T("предупреждение: %v; monopanel-api слушает порт сам", "warning: %v; monopanel-api listens on the port itself"), err)
		}
		if err := startPanel(ctx, sd, fresh, warn); err != nil {
			return nil, err
		}
		cl := client.NewUnix(cfg.APISocket())
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := cl.Health(hctx)
			cancel()
			if err == nil {
				res.Healthy = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !res.Healthy {
			step("%s", T("предупреждение: API не ответил за 20 с; смотрите journalctl -u monopanel-api", "warning: the API did not answer within 20 s; see journalctl -u monopanel-api"))
		}
	}

	_, port, _ := net.SplitHostPort(cfg.Web.Listen)
	if port == "" {
		port = "8443"
	}
	seen := map[string]bool{}
	for _, n := range api.LocalNames(cfg.Web.Hostname) {
		if n == "localhost" || n == "127.0.0.1" || seen[n] {
			continue
		}
		seen[n] = true
		if strings.Contains(n, ":") {
			n = "[" + n + "]"
		}
		res.URLs = append(res.URLs, fmt.Sprintf("https://%s:%s/", n, port))
	}
	return res, nil
}

// unitsResult is what installUnits did.
type unitsResult struct {
	// Wrote are the units written into the unit directory (writeUnits).
	Wrote []string
	// Removed are the panel's own copies of the package's units removed
	// there, Shadowing the files there that override a unit of the package
	// all the same (pruneUnitCopies), PruneErr what kept a copy from going.
	Removed, Shadowing []string
	PruneErr           error
	// Moved says that the socket's address changed on disk, SocketErr that
	// the socket cannot follow web.listen.
	Moved     bool
	SocketErr error
}

// installUnits gives systemd the panel's units in dir: the panel's own
// copies of the units the package ships in packaged go (pruneUnitCopies),
// the units it does not ship are written (writeUnits), and the socket of the
// panel's port gets the address of web.listen (agent.APISocketSync);
// systemd is reloaded when anything changed. A copy that cannot go is
// PruneErr, and a socket that cannot follow web.listen (a name that does not
// resolve now) SocketErr, not err: the API listens on web.listen itself
// then, stopping a socket in its way, as it does when the agent or the
// package's script could not move the socket.
func installUnits(ctx context.Context, dir, packaged string, cfg config.Config, binary string, force bool, reload func(context.Context) error) (unitsResult, error) {
	var res unitsResult
	pruned, perr := pruneUnitCopies(dir, packaged, binary)
	res.Removed, res.Shadowing, res.PruneErr = pruned.Removed, pruned.Shadowing, perr
	var err error
	if res.Wrote, err = writeUnits(dir, packaged, binary, force); err != nil {
		return res, err
	}
	if len(res.Wrote)+len(res.Removed) > 0 {
		if err := reload(ctx); err != nil {
			return res, err
		}
	}
	// The API unit's drop-in probes the binary the units run.
	if res.Moved, res.SocketErr = (agent.APISocketSync{UnitDir: dir, Reload: reload, Binary: binary}).Sync(ctx, cfg); res.SocketErr != nil {
		res.SocketErr = fmt.Errorf("%s: %w", apiSocket, res.SocketErr)
	}
	return res, nil
}

// writeUnits writes into dir (/etc/systemd/system) the panel's units the
// package does not ship in packaged for binary (packagedFor): on an install
// without the package all four, those that are missing or, with force
// (--install-units), every one. A unit the package ships is never written
// there, force or not: the copy would override the package's unit for good.
// It returns the names it wrote.
func writeUnits(dir, packaged, binary string, force bool) ([]string, error) {
	var wrote []string
	for _, u := range panelUnits() {
		if packagedFor(packaged, u, binary) {
			continue
		}
		path := filepath.Join(dir, u.name)
		if _, err := os.Stat(path); !force && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.WriteFile(path, []byte(unitText(u.text, binary)), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, u.name)
	}
	return wrote, nil
}

// unitEnabler is what enabling the panel's units needs of systemd.
type unitEnabler interface {
	Enable(ctx context.Context, unit string) error
	Status(ctx context.Context, unit string) (systemd.Status, error)
}

// enablePanel enables the panel's units for boot, all but a socket of the
// panel's port the administrator masked (agent.APISocketMasked), and tells
// whether it is: systemd refuses to enable a masked unit, and the mask is
// the way to have the API listen on the port itself on purpose, which the
// doctor calls well and mp setup leaves as it is. A disabled socket is
// enabled again: the API's unit starts it all the same, and one that is not
// listening then leaves the port free while the API restarts.
func enablePanel(ctx context.Context, sd unitEnabler) (socketMasked bool, err error) {
	for _, u := range []string{agentService, apiSocket, apiService} {
		if u == apiSocket {
			if st, err := sd.Status(ctx, u); err == nil && agent.APISocketMasked(st) {
				socketMasked = true
				continue
			}
		}
		if err := sd.Enable(ctx, u); err != nil {
			return socketMasked, err
		}
	}
	return socketMasked, nil
}

// serviceManager is what starting the panel needs of systemd.
type serviceManager interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
}

// startPanel starts the panel's services, or restarts them. The socket of
// the panel's port starts afresh only when it has to take the port anew
// (fresh): its unit or its address was written just now, it is not
// listening — an API of 0.8.19 or older still listens on the port itself —
// or it was started on an older address. The API is stopped first then: the
// socket can only bind the port once no API process holds it, and systemd
// refuses to start a socket whose service runs. Otherwise the socket keeps
// the port while the API restarts. A socket that does not start is warned
// about: the API listens on the port itself then.
func startPanel(ctx context.Context, sd serviceManager, fresh bool, warn func(error)) error {
	if !fresh {
		for _, u := range []string{agentService, apiService} {
			if err := sd.Restart(ctx, u); err != nil {
				return err
			}
		}
		return nil
	}
	if err := sd.Stop(ctx, apiService); err != nil {
		return err
	}
	if err := sd.Restart(ctx, apiSocket); err != nil {
		warn(err)
	}
	if err := sd.Restart(ctx, agentService); err != nil {
		return err
	}
	return sd.Start(ctx, apiService)
}

// modTime is when path was last written, zero when it is not there.
func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func chownName(path, owner, group string, mode os.FileMode) error {
	if mode != 0 {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	_, err := agent.EnsureDirOwner(path, owner, group)
	return err
}

// tmpfilesConf is mp setup's /etc/tmpfiles.d/monopanel.conf, the package's
// file for this layout (it takes the package's place at boot): the run
// directory, and php/ in it root's from boot — the php-fpm masters make the
// pools' sockets there as root, and the API may write to the run directory.
func tmpfilesConf(cfg config.Config) string {
	return fmt.Sprintf("d %s 1771 root %s -\n", cfg.RunDir, cfg.ServiceGroup) + phpTmpfilesLine(cfg)
}

// phpTmpfilesLine is the line of php/ in the run directory, root's. Hosts
// keep it in monopanel-php.conf as well, for good (phpTmpfilesFile; the
// package's scripts write it word for word), and systemd-tmpfiles reads
// that file before monopanel.conf: the same line twice is silent, but a
// line here that differs from it loses to it, with a notice at every boot.
// A change of the line has to bring that file along.
func phpTmpfilesLine(cfg config.Config) string {
	return fmt.Sprintf("d %s 0755 root root -\n", filepath.Join(cfg.RunDir, "php"))
}

// The package's tmpfiles file, and the one the API of 0.8.19 wrote for
// php/ alone.
const (
	packageTmpfilesFile = "/usr/lib/tmpfiles.d/monopanel.conf"
	phpTmpfilesFile     = "/etc/tmpfiles.d/monopanel-php.conf"
)

// keepPHPTmpfiles writes phpFile with php/'s line (phpTmpfilesLine) where it
// is missing on an install without the package (no packageFile). Going back
// below 0.9.0 there runs no preremove, which writes the file for the
// package (packaging/scripts/preremove.sh), and the older version's mp
// setup writes monopanel.conf without the line: nothing would make php/ at
// boot, and every php-fpm master would fail to start after a reboot. One
// line for one path in two files is no warning to systemd-tmpfiles. A file
// there already stays as it is, whatever it holds — the postinstall warns of
// more than the panel's line — and a symlink is not written through.
func keepPHPTmpfiles(phpFile, packageFile string, cfg config.Config) (bool, error) {
	for _, p := range []string{packageFile, phpFile} {
		if _, err := os.Lstat(p); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	f, err := os.OpenFile(phpFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(phpTmpfilesLine(cfg)); err != nil {
		f.Close()          //nolint:errcheck // the write failed already
		os.Remove(phpFile) //nolint:errcheck // a half-written line is no line
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(phpFile) //nolint:errcheck // a half-written line is no line
		return false, err
	}
	return true, nil
}

// chownTree hands the data directory back to the service user after setup
// made files there as root. The tree is the API's, so it goes down through
// directories opened without following a symlink and changes each entry
// itself (fchownat with AT_SYMLINK_NOFOLLOW): a link planted there, or a
// directory swapped for one meanwhile, cannot turn it into a chown of
// /etc/shadow. It leaves alone the configuration history of 0.8.19 and
// older (skip), every other root-owned directory closed to group and others
// (that is what the history looks like, wherever it was moved), and a file
// with a second name (a hard link may be another file of the system).
func chownTree(root, owner, group, skip string) error {
	uid, err := lookupID(owner, false)
	if err != nil {
		return err
	}
	gid, err := lookupID(group, true)
	if err != nil {
		return err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return &os.PathError{Op: "chown", Path: root, Err: err}
	}
	return chownDir(fd, root, uid, gid, filepath.Clean(skip))
}

// chownDir gives what the directory fd (which it closes) holds to uid:gid.
func chownDir(fd int, dir string, uid, gid int, skip string) error {
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		p := filepath.Join(dir, name)
		if p == skip {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue // vanished meanwhile
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if st.Uid == 0 && st.Mode&0o077 == 0 {
				continue
			}
		case unix.S_IFREG:
			if st.Nlink > 1 && int(st.Uid) != uid {
				continue
			}
		}
		if err := unix.Fchownat(fd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			sub, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				continue // swapped for something else meanwhile: not followed
			}
			if err := chownDir(sub, p, uid, gid, skip); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureSelfSigned makes the panel's self-signed certificate when there is
// none. Its default place is the data directory, which the API owns: a
// symlink it left there under the name of the key would carry root's write
// anywhere (the API cannot change what is written, but it could choose
// where — over /etc/shadow). So there the pair is made in a directory of
// root's own and moved in with agent.InstallFile, which replaces such a
// symlink instead of writing through it, and only plain files count as
// present. A place the administrator chose elsewhere (web.tls_cert) is
// theirs, and so is a symlink there: Let's Encrypt's live/ files are links.
func ensureSelfSigned(cfg config.Config, certPath, keyPath string, names []string) (bool, error) {
	if !below(cfg.DataDir, certPath) && !below(cfg.DataDir, keyPath) {
		return api.EnsureSelfSigned(certPath, keyPath, names)
	}
	haveCert, certErr := agent.PlainFile(certPath)
	haveKey, keyErr := agent.PlainFile(keyPath)
	if haveCert && haveKey && certErr == nil && keyErr == nil {
		return false, nil
	}
	tmp, err := os.MkdirTemp("", "mp-setup-tls-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // a scratch directory of this run
	tmpCert, tmpKey := filepath.Join(tmp, "panel.crt"), filepath.Join(tmp, "panel.key")
	if _, err := api.EnsureSelfSigned(tmpCert, tmpKey, names); err != nil {
		return false, err
	}
	for _, f := range []struct {
		from, to string
		mode     os.FileMode
	}{{tmpKey, keyPath, 0o600}, {tmpCert, certPath, 0o644}} {
		b, err := os.ReadFile(f.from)
		if err != nil {
			return false, err
		}
		if err := agent.InstallFile(f.to, b, f.mode); err != nil {
			return false, fmt.Errorf("%s: %w", f.to, err)
		}
	}
	return true, nil
}

// checkDBFiles refuses to open the panel's database as root when its file or
// a journal SQLite keeps next to it is anything but a plain file: they lie in
// the API's directory, SQLite opens them by name, and a symlink the API left
// there would have root write a database anywhere.
func checkDBFiles(db string) error {
	for _, p := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		if _, err := agent.PlainFile(p); err != nil {
			return fmt.Errorf("%s: %w; mp setup opens the database as root and does not go through it — remove it (or put the plain file back) and run mp setup again", p, err)
		}
	}
	return nil
}

// below reports whether p lies in dir.
func below(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	return strings.HasPrefix(p, dir+"/")
}

// lookupID resolves a user (or, with group, a group) name; "" leaves it.
func lookupID(name string, group bool) (int, error) {
	if name == "" {
		return -1, nil
	}
	var id string
	if group {
		g, err := user.LookupGroup(name)
		if err != nil {
			return -1, err
		}
		id = g.Gid
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return -1, err
		}
		id = u.Uid
	}
	return strconv.Atoi(id)
}

// openFirewalld lets the panel port through a running firewalld — some EL
// images (Oracle Linux) ship it enabled with only ssh allowed. Returns the
// port it opened, or "" when there is no firewalld to talk to.
func openFirewalld(listen string) (string, error) {
	if _, err := os.Stat("/usr/bin/firewall-cmd"); err != nil {
		return "", nil //nolint:nilerr // no firewall-cmd means no firewalld: nothing to open
	}
	// A non-zero exit means "not running"; its text is all that matters.
	state, _ := exec.Command("/usr/bin/firewall-cmd", "--state").CombinedOutput()
	if !strings.Contains(string(state), "running") {
		return "", nil
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("listen address %q has no port", listen)
	}
	for _, args := range [][]string{{"-q", "--permanent", "--add-port=" + port + "/tcp"}, {"-q", "--reload"}} {
		if out, err := exec.Command("/usr/bin/firewall-cmd", args...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("firewall-cmd %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
	}
	return port + "/tcp", nil
}
