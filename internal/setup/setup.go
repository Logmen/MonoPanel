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
	"path/filepath"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/api"
	"monopanel/internal/auth"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

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
	step("ОС: %s (%s)", profile.Release().PrettyName, profile.Family())

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opts.Hostname != "" {
		cfg.Web.Hostname = opts.Hostname
	} else if cfg.Web.Hostname == "" {
		cfg.Web.Hostname, _ = os.Hostname()
	}
	if opts.Listen != "" {
		cfg.Web.Listen = opts.Listen
	}
	res := &Result{ConfigPath: cfg.Path(), DBPath: cfg.DBPath()}

	step("группы и служебный пользователь: %s, %s", cfg.ServiceGroup, cfg.WebGroup)
	for _, g := range []string{cfg.ServiceGroup, cfg.WebGroup} {
		if _, err := agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: g, System: true}); err != nil {
			return nil, err
		}
	}
	if _, err := agent.EnsureUnixUser(ctx, profile, &agent.EnsureUnixUserRequest{Login: cfg.ServiceUser, System: true, PrimaryGroup: cfg.ServiceGroup, Home: cfg.DataDir, Comment: "MonoPanel service"}); err != nil {
		return nil, err
	}

	step("каталоги")
	dirs := []agent.DirSpec{
		{Path: cfg.ConfigDir, Mode: 0o750, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.TemplatesDir, Mode: 0o750, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.DataDir, Mode: 0o751, Owner: cfg.ServiceUser, Group: cfg.ServiceGroup}, // o+x: nginx must traverse to acme/webroot
		{Path: filepath.Join(cfg.DataDir, "tls"), Mode: 0o700, Owner: cfg.ServiceUser, Group: cfg.ServiceGroup},
		{Path: cfg.ConfHistoryDir(), Mode: 0o700, Owner: "root", Group: "root"},
		{Path: cfg.LogDir, Mode: 0o750, Owner: cfg.ServiceUser, Group: cfg.ServiceGroup},
		{Path: cfg.RunDir, Mode: 0o771, Owner: "root", Group: cfg.ServiceGroup},
		{Path: cfg.WWWRoot, Mode: 0o711, Owner: "root", Group: "root"},
	}
	for _, d := range dirs {
		if _, err := agent.EnsureDir(d); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile("/etc/tmpfiles.d/monopanel.conf", []byte(fmt.Sprintf("d %s 0771 root %s -\n", cfg.RunDir, cfg.ServiceGroup)), 0o644); err != nil {
		return nil, err
	}

	if _, err := os.Stat(cfg.Path()); errors.Is(err, os.ErrNotExist) {
		step("конфигурация: %s", cfg.Path())
		if err := cfg.Save(cfg.Path()); err != nil {
			return nil, err
		}
	} else {
		step("конфигурация уже есть: %s", cfg.Path())
		if opts.Hostname != "" || opts.Listen != "" {
			if err := cfg.Save(cfg.Path()); err != nil {
				return nil, err
			}
			step("обновлено: hostname=%s listen=%s", cfg.Web.Hostname, cfg.Web.Listen)
		}
	}
	if err := chownName(cfg.Path(), "root", cfg.ServiceGroup, 0o640); err != nil {
		return nil, err
	}

	if _, err := os.Stat(cfg.SecretKeyFile); errors.Is(err, os.ErrNotExist) {
		step("секретный ключ: %s", cfg.SecretKeyFile)
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
	created, err := api.EnsureSelfSigned(certPath, keyPath, api.LocalNames(cfg.Web.Hostname))
	if err != nil {
		return nil, err
	}
	if created {
		step("самоподписанный сертификат: %s", certPath)
	}
	for _, p := range []string{certPath, keyPath} {
		if err := chownName(p, cfg.ServiceUser, cfg.ServiceGroup, 0); err != nil {
			return nil, err
		}
	}
	res.Fingerprint, _ = api.CertFingerprint(certPath)

	step("база данных: %s", cfg.DBPath())
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
				db.Close()
				return nil, err
			}
			if err := db.SetUserPassword(ctx, existing.ID, h); err != nil {
				db.Close()
				return nil, err
			}
			step("пароль администратора %s обновлён", login)
		} else {
			step("администратор %s уже существует", login)
		}
	case errors.Is(err, store.ErrNotFound):
		if password == "" {
			password, err = auth.NewToken(15)
			if err != nil {
				db.Close()
				return nil, err
			}
			res.GeneratedPassword = true
		}
		h, err := auth.HashPassword(password)
		if err != nil {
			db.Close()
			return nil, err
		}
		if err := db.CreateUser(ctx, &store.User{Login: login, Role: store.RoleAdmin, PasswordHash: h}); err != nil {
			db.Close()
			return nil, err
		}
		res.AdminCreated = true
		res.AdminPassword = password
		step("администратор %s создан", login)
	default:
		db.Close()
		return nil, err
	}
	db.Audit(ctx, store.AuditEntry{Actor: "root", Action: "setup", Target: login})
	db.Close()
	if err := chownTree(cfg.DataDir, cfg.ServiceUser, cfg.ServiceGroup, cfg.ConfHistoryDir()); err != nil {
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
	if opts.Start {
		step("запуск сервисов")
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
			step("предупреждение: API не ответил за 20 с; смотрите journalctl -u monopanel-api")
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

func chownTree(root, owner, group, skip string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip != "" && (p == skip || strings.HasPrefix(p, skip+"/")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		_, err = agent.EnsureDirOwner(p, owner, group)
		return err
	})
}
