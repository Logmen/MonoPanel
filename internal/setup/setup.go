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
Restart=on-failure
RestartSec=2
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
	if err := os.WriteFile("/etc/tmpfiles.d/monopanel.conf", []byte(fmt.Sprintf("d %s 1771 root %s -\n", cfg.RunDir, cfg.ServiceGroup)), 0o644); err != nil {
		return nil, err
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
	units := map[string]string{
		"/etc/systemd/system/monopanel-agent.service": fmt.Sprintf(agentUnit, binary),
		"/etc/systemd/system/monopanel-api.service":   fmt.Sprintf(apiUnit, binary),
	}
	sd, err := systemd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer sd.Close()
	wrote := false
	for path, content := range units {
		_, statErr := os.Stat(path)
		if opts.InstallUnits || errors.Is(statErr, os.ErrNotExist) {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, err
			}
			wrote = true
		}
	}
	if wrote {
		step("systemd units: monopanel-agent, monopanel-api (%s)", binary)
		if err := sd.DaemonReload(ctx); err != nil {
			return nil, err
		}
	}
	for _, u := range []string{"monopanel-agent.service", "monopanel-api.service"} {
		if err := sd.Enable(ctx, u); err != nil {
			return nil, err
		}
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
	if opts.Start {
		step("%s", T("запуск сервисов", "starting the services"))
		for _, u := range []string{"monopanel-agent.service", "monopanel-api.service"} {
			if err := sd.Restart(ctx, u); err != nil {
				return nil, err
			}
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

func chownName(path, owner, group string, mode os.FileMode) error {
	if mode != 0 {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	_, err := agent.EnsureDirOwner(path, owner, group)
	return err
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
	defer os.RemoveAll(tmp)
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
