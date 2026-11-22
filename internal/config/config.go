// Package config loads /etc/monopanel/config.yaml and provides derived paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

const (
	// DefaultPath is the configuration file read when nothing else is given.
	DefaultPath = "/etc/monopanel/config.yaml"
	// EnvPath overrides DefaultPath (used by tests and dev setups).
	EnvPath = "MONOPANEL_CONFIG"
)

// Config is the panel configuration. Empty fields are filled from Default().
type Config struct {
	ConfigDir     string `yaml:"config_dir"`
	DataDir       string `yaml:"data_dir"`
	RunDir        string `yaml:"run_dir"`
	LogDir        string `yaml:"log_dir"`
	TemplatesDir  string `yaml:"templates_dir"`
	SecretKeyFile string `yaml:"secret_key_file"`
	ServiceUser   string `yaml:"service_user"`
	ServiceGroup  string `yaml:"service_group"`
	WebGroup      string `yaml:"web_group"`
	// CLIGroup owns the local API socket: root and its members (the service
	// user and accounts with shell access) may talk to the panel with mp.
	CLIGroup string `yaml:"cli_group"`
	WWWRoot  string `yaml:"www_root"`

	Web    Web    `yaml:"web"`
	Agent  Agent  `yaml:"agent"`
	Jobs   Jobs   `yaml:"jobs"`
	Log    Log    `yaml:"log"`
	Update Update `yaml:"update"`

	path string
}

// Web configures the panel's own HTTPS listener.
type Web struct {
	Listen   string `yaml:"listen"`
	Hostname string `yaml:"hostname"`
	TLSCert  string `yaml:"tls_cert"`
	TLSKey   string `yaml:"tls_key"`
}

// Agent configures the privileged agent. The built-in write allow-list lives
// in code (DefaultAllowedWritePrefixes) so upgrades extend it; the file only
// adds site-specific extras.
type Agent struct {
	Socket             string   `yaml:"socket"`
	ExtraWritePrefixes []string `yaml:"extra_write_prefixes,omitempty"`
	// StateDir is the agent's own directory (root 0700), where it keeps the
	// configuration history: none of it is the API's to read or rearrange.
	// Empty means DefaultAgentStateDir.
	StateDir string `yaml:"state_dir,omitempty"`
}

// DefaultAgentStateDir is the agent's own directory when the file names none.
const DefaultAgentStateDir = "/var/lib/monopanel-agent"

// Update configures panel self-updates. Only the trust anchor lives here: a
// package that replaces the panel binary runs as root, so the key that vouches
// for it must not be changeable from the panel itself. Where to look for
// releases is an ordinary setting in the database.
type Update struct {
	// PublicKey is the base64 ed25519 key the release checksums are signed
	// with. Empty means unsigned releases are accepted after a digest check.
	PublicKey string `yaml:"public_key,omitempty"`
	// Unit names the panel restarts after installing a new version.
	AgentUnit string `yaml:"agent_unit,omitempty"`
	APIUnit   string `yaml:"api_unit,omitempty"`
}

// Jobs configures the job runner.
type Jobs struct {
	Workers int `yaml:"workers"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// AuditFile mirrors the audit log (sign-ins, tokens, changes) as JSON
	// lines; the database stays the primary copy. "none" switches it off.
	AuditFile string `yaml:"audit_file,omitempty"`
}

// DefaultAllowedWritePrefixes lists where the agent may write files on behalf
// of the API process. A prefix ending in "/" matches a directory tree; any
// other entry must match the path exactly.
//
// Two trees are not here on purpose. /etc/systemd/system: a unit or a drop-in
// runs whatever it says as root, so the panel's units go through the agent's
// typed /v1/units/* calls, which accept only the units the panel writes. And
// the panel's data directory (/var/lib/monopanel): it belongs to the API's
// service user, who makes what it needs there itself — a root write below a
// directory the API owns could be redirected by it.
var DefaultAllowedWritePrefixes = []string{
	"/etc/monopanel/",
	"/etc/nginx/nginx.conf",
	"/etc/nginx/monopanel/",
	"/etc/nginx/conf.d/default.conf",
	"/etc/apache2/monopanel/",
	"/etc/apache2/conf-available/monopanel.conf",
	"/etc/apache2/ports.conf",
	"/etc/httpd/monopanel/",
	"/etc/httpd/conf.d/monopanel.conf",
	"/etc/php/",
	"/etc/opt/remi/",
	"/etc/mysql/",
	"/var/log/mysql/", // slow log directory on EL, made by the panel
	"/etc/memcached.conf",
	"/etc/sysconfig/memcached",
	"/etc/sphinxsearch/",
	"/etc/default/sphinxsearch",
	"/var/lib/sphinxsearch/data/", // index files of Debian's sphinxsearch, dropped on a schema change
	"/etc/sphinx/",
	"/opt/monopanel/",
	"/var/lib/sphinx/",
	"/var/log/sphinx/",
	"/usr/local/bin/composer",
	"/usr/local/lib/monopanel/",
	"/etc/my.cnf.d/",
	"/etc/apt/sources.list.d/",
	"/etc/selinux/config", // the mode for the next boot (mp selinux)
	"/etc/apt/preferences.d/",
	"/etc/apt/keyrings/",
	"/etc/yum.repos.d/",
	"/etc/pki/rpm-gpg/",
	"/etc/nftables.d/",
	"/etc/postfix/",
	"/etc/dovecot/",
	"/etc/opendkim/",
	"/etc/opendkim.conf",
	"/etc/default/opendkim",
	"/var/mail/monopanel/",
	"/etc/logrotate.d/",
	"/etc/fail2ban/jail.d/",
	"/etc/fail2ban/filter.d/",
	"/etc/ssh/sshd_config.d/",
	"/etc/tmpfiles.d/",
	"/etc/sysusers.d/",
	"/etc/sysctl.d/90-monopanel.conf", // nginx may bind an IPv6 address the host does not have yet
	"/var/lib/monopanel-valkey/",      // snapshots of the per-account Valkey instances, removed with them
	"/usr/share/keyrings/",
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		ConfigDir:     "/etc/monopanel",
		DataDir:       "/var/lib/monopanel",
		RunDir:        "/run/monopanel",
		LogDir:        "/var/log/monopanel",
		TemplatesDir:  "/etc/monopanel/templates",
		SecretKeyFile: "/etc/monopanel/secret.key",
		ServiceUser:   "monopanel",
		ServiceGroup:  "monopanel",
		WebGroup:      "monopanel-web",
		CLIGroup:      "monopanel-cli",
		WWWRoot:       "/var/www",
		Web:           Web{Listen: ":8443"},
		Agent:         Agent{},
		Jobs:          Jobs{Workers: 2},
		Update:        Update{AgentUnit: "monopanel-agent.service", APIUnit: "monopanel-api.service"},
		Log:           Log{Level: "info", Format: "text", AuditFile: "/var/log/monopanel/audit.log"},
	}
}

// Load reads the configuration file. A missing file yields Default() without error.
func Load(path string) (Config, error) {
	if path == "" {
		path = os.Getenv(EnvPath)
	}
	if path == "" {
		path = DefaultPath
	}
	cfg := Default()
	cfg.path = path
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	d := Default()
	def := func(v *string, dv string) {
		if *v == "" {
			*v = dv
		}
	}
	def(&c.ConfigDir, d.ConfigDir)
	def(&c.DataDir, d.DataDir)
	def(&c.RunDir, d.RunDir)
	def(&c.LogDir, d.LogDir)
	def(&c.TemplatesDir, d.TemplatesDir)
	def(&c.SecretKeyFile, d.SecretKeyFile)
	def(&c.ServiceUser, d.ServiceUser)
	def(&c.ServiceGroup, d.ServiceGroup)
	def(&c.WebGroup, d.WebGroup)
	def(&c.CLIGroup, d.CLIGroup)
	def(&c.WWWRoot, d.WWWRoot)
	def(&c.Web.Listen, d.Web.Listen)
	def(&c.Log.Level, d.Log.Level)
	def(&c.Log.Format, d.Log.Format)
	def(&c.Log.AuditFile, d.Log.AuditFile)
	if c.Log.AuditFile == "none" {
		c.Log.AuditFile = ""
	}
	def(&c.Update.AgentUnit, d.Update.AgentUnit)
	def(&c.Update.APIUnit, d.Update.APIUnit)
	if c.Jobs.Workers <= 0 {
		c.Jobs.Workers = d.Jobs.Workers
	}
}

// WritePrefixes returns the effective agent allow-list: defaults plus extras.
func (c Config) WritePrefixes() []string {
	out := append([]string{}, DefaultAllowedWritePrefixes...)
	return append(out, c.Agent.ExtraWritePrefixes...)
}

// Path returns the file this configuration was loaded from (or would be saved to).
func (c Config) Path() string { return c.path }

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "panel.db") }

// APISocket is the unix socket the API listens on for local CLI clients.
func (c Config) APISocket() string { return filepath.Join(c.RunDir, "api.sock") }

// AgentSocket is the unix socket the privileged agent listens on.
func (c Config) AgentSocket() string {
	if c.Agent.Socket != "" {
		return c.Agent.Socket
	}
	return filepath.Join(c.RunDir, "agent.sock")
}

// TLSCertPath and TLSKeyPath return the panel certificate files.
func (c Config) TLSCertPath() string {
	if c.Web.TLSCert != "" {
		return c.Web.TLSCert
	}
	return filepath.Join(c.DataDir, "tls", "panel.crt")
}

// TLSKeyPath returns the panel private key file.
func (c Config) TLSKeyPath() string {
	if c.Web.TLSKey != "" {
		return c.Web.TLSKey
	}
	return filepath.Join(c.DataDir, "tls", "panel.key")
}

// DownloadsDir is where packages are staged before they are installed.
func (c Config) DownloadsDir() string { return filepath.Join(c.DataDir, "downloads") }

// UpdatesDir holds the state of the last self-update, for the API to read.
// Up to 0.8.19 it also kept the binary an update replaced.
func (c Config) UpdatesDir() string { return filepath.Join(c.DataDir, "updates") }

// AgentUpdatesDir is root's part of a self-update: the package being
// installed, copied out of the API's downloads directory, and the binary it
// replaces, which a rollback puts back as /usr/bin/monopanel. In a directory
// the API owns, either could be swapped between the check and its use.
func (c Config) AgentUpdatesDir() string { return filepath.Join(c.AgentStateDir(), "updates") }

// AgentStateDir is the agent's own directory (agent.state_dir).
func (c Config) AgentStateDir() string {
	if c.Agent.StateDir != "" {
		return c.Agent.StateDir
	}
	return DefaultAgentStateDir
}

// ConfHistoryDir stores previous versions of generated configuration files.
// It is root's (in the agent's own directory, not below the data directory
// the API owns): the agent writes and prunes it as root, and a directory the
// API could rename or swap for a symlink would steer those writes.
func (c Config) ConfHistoryDir() string { return filepath.Join(c.AgentStateDir(), "confhistory") }

// LegacyConfHistoryDir is where the history lived up to 0.8.19, below the
// data directory; it stays there, root's, and nothing writes to it any more.
func (c Config) LegacyConfHistoryDir() string { return filepath.Join(c.DataDir, "confhistory") }

// Save writes the configuration as YAML.
func (c Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		path = DefaultPath
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# MonoPanel configuration. Generated by `mp setup`; edit and restart monopanel-api / monopanel-agent.\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(header), b...), 0o640)
}
