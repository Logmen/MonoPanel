// Package apitypes holds the request/response bodies shared by the API
// server, the Go CLI client and (via OpenAPI) the TypeScript client.
package apitypes

import (
	"time"

	"monopanel/internal/acme"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

// Health is the public liveness answer.
type Health struct {
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// Principal is the authenticated caller.
type Principal struct {
	UserID int64    `json:"user_id,omitempty"`
	Login  string   `json:"login"`
	Role   string   `json:"role"`
	Via    string   `json:"via"` // peercred | token | session
	Scopes []string `json:"scopes,omitempty"`
}

// PanelStatus describes the panel process.
type PanelStatus struct {
	Version       string         `json:"version"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	SchemaVersion int            `json:"schema_version"`
	Jobs          map[string]int `json:"jobs"`
	Users         map[string]int `json:"users"`
	AgentOK       bool           `json:"agent_ok"`
	AgentVersion  string         `json:"agent_version,omitempty"`
	AgentError    string         `json:"agent_error,omitempty"`
}

// SystemStatus is the dashboard payload.
type SystemStatus struct {
	Panel    PanelStatus      `json:"panel"`
	Host     *sysinfo.Info    `json:"host,omitempty"`
	Services []systemd.Status `json:"services,omitempty"`
}

// LoginRequest is the login form.
type LoginRequest struct {
	Login    string `json:"login" minLength:"1" maxLength:"64"`
	Password string `json:"password" minLength:"1" maxLength:"1024"`
	Code     string `json:"totp_code,omitempty" maxLength:"8" doc:"Required when 2FA is enabled (error detail: totp_required)"`
}

// LoginResponse returns who you are.
type LoginResponse struct {
	Principal Principal   `json:"principal"`
	User      *store.User `json:"user"`
}

// CreateUserRequest creates a panel account and its unix user.
type CreateUserRequest struct {
	Login    string `json:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$" doc:"Unix-compatible login"`
	Password string `json:"password,omitempty" minLength:"8" maxLength:"1024"`
	Email    string `json:"email,omitempty" format:"email"`
	Role     string `json:"role,omitempty" enum:"admin,user" default:"user"`
	Shell    bool   `json:"shell,omitempty" doc:"Allow SSH shell (otherwise SFTP only)"`
	QuotaMB  *int   `json:"quota_mb,omitempty" minimum:"0"`
}

// UserWithJob is returned by create: the record plus the provisioning job.
type UserWithJob struct {
	User  *store.User `json:"user"`
	JobID int64       `json:"job_id,omitempty"`
}

// CreateTokenRequest mints an API token.
type CreateTokenRequest struct {
	Name          string   `json:"name" minLength:"1" maxLength:"64"`
	User          string   `json:"user,omitempty" pattern:"^[a-z_][a-z0-9_-]{0,31}$" doc:"Account the token acts as; administrators only. Root on the local socket has no account of its own: without this the single administrator is used."`
	Scopes        []string `json:"scopes,omitempty"`
	ExpiresInDays int      `json:"expires_in_days,omitempty" minimum:"0" maximum:"3650"`
}

// CreateTokenResponse returns the plaintext token exactly once.
type CreateTokenResponse struct {
	Token  string          `json:"token"`
	Record *store.APIToken `json:"record"`
}

// ServiceActionRequest acts on a unit.
type ServiceActionRequest struct {
	Action string `json:"action" enum:"start,stop,reload,restart,reload-or-restart,enable,disable"`
}

// StackComponent describes one installable component.
type StackComponent struct {
	Name      string          `json:"name"`
	Installed bool            `json:"installed"`
	Version   string          `json:"version,omitempty"`
	Service   *systemd.Status `json:"service,omitempty"`
}

// StackInstallRequest installs a component.
type StackInstallRequest struct {
	Component string `json:"component" enum:"nginx,apache,percona,mysql,fail2ban"`
}

// JobRef points at an asynchronous job.
type JobRef struct {
	JobID int64 `json:"job_id"`
}

// JobEvent is the SSE payload.
type JobEvent = jobs.Event

// IssueCertificateRequest orders an ACME certificate (HTTP-01).
type IssueCertificateRequest struct {
	Names     []string `json:"names" minItems:"1" maxItems:"100" doc:"Hostnames; the first one names the certificate"`
	Email     string   `json:"email,omitempty" format:"email" doc:"ACME account contact; remembered for later orders"`
	Staging   bool     `json:"staging,omitempty" doc:"Use the Let's Encrypt staging directory (untrusted test certificates)"`
	Directory string   `json:"directory,omitempty" format:"uri" doc:"Custom ACME directory URL"`
	KeyType   string   `json:"key_type,omitempty" enum:"ec256,rsa2048" default:"ec256"`
	AutoRenew *bool    `json:"auto_renew,omitempty"`
	DNS       string   `json:"dns,omitempty" doc:"DNS provider name for DNS-01 (required for wildcards)"`
}

// DNSProviderRequest registers DNS API credentials for DNS-01.
type DNSProviderRequest struct {
	Name        string            `json:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	Type        string            `json:"type" doc:"cloudflare, hetzner, digitalocean, gandiv5, desec, namecheap, rfc2136"`
	Credentials map[string]string `json:"credentials" doc:"environment variables of the lego provider"`
}

// CertificateWithJob is returned by issue/renew.
type CertificateWithJob struct {
	Certificate *store.Certificate `json:"certificate"`
	JobID       int64              `json:"job_id"`
}

// TLSInfo describes the certificate the panel itself serves on :8443.
type TLSInfo struct {
	Hostname    string        `json:"hostname"`
	Source      string        `json:"source"` // acme | selfsigned
	Certificate acme.CertInfo `json:"certificate"`
}

// PHPInstallRequest installs a PHP branch.
type PHPInstallRequest struct {
	Version string `json:"version" pattern:"^[578]\\.[0-9]$" doc:"Branch such as 8.4"`
}

// PHPVersions lists installed branches and the availability matrix.
type PHPVersions struct {
	Installed []*store.PHPVersion        `json:"installed"`
	Available []osprofile.PHPVersionInfo `json:"available"`
}

// SiteRequest creates a site. Unset optional fields take panel defaults.
type SiteRequest struct {
	Domain         string            `json:"domain" doc:"Primary hostname"`
	User           string            `json:"user,omitempty" doc:"Owner login (admins only; users create their own)"`
	Aliases        []string          `json:"aliases,omitempty"`
	WWW            bool              `json:"www,omitempty" doc:"Add www.<domain> alias"`
	Mode           string            `json:"mode,omitempty" enum:"fpm,apache,proxy"`
	Backend        string            `json:"backend,omitempty" doc:"proxy mode: http://127.0.0.1:PORT or http://unix:/path.sock:"`
	PHPVersion     string            `json:"php_version,omitempty"`
	Docroot        string            `json:"docroot,omitempty" doc:"Subdirectory of the site root, e.g. public"`
	IP             string            `json:"ip,omitempty"`
	SSL            string            `json:"ssl,omitempty" enum:"auto,none"`
	HTTP2          *bool             `json:"http2,omitempty"`
	HTTP3          *bool             `json:"http3,omitempty"`
	RedirectHTTPS  *bool             `json:"redirect_https,omitempty"`
	RedirectWWW    string            `json:"redirect_www,omitempty" enum:"none,to_www,to_root"`
	StaticByNginx  *bool             `json:"static_by_nginx,omitempty"`
	FPMPM          string            `json:"fpm_pm,omitempty" enum:"ondemand,dynamic,static"`
	FPMMaxChildren int               `json:"fpm_max_children,omitempty" minimum:"1" maximum:"512"`
	PHPIni         map[string]string `json:"php_ini,omitempty"`
	AllowExec      *bool             `json:"allow_exec,omitempty"`
	ClientMaxBody  string            `json:"client_max_body,omitempty" pattern:"^[0-9]+[kKmMgG]?$"`
	AllowFrom      []string          `json:"allow_from,omitempty" maxItems:"64" doc:"Only these IPs/CIDRs may open the site (empty = everyone); ACME challenges stay reachable"`
	Preset         string            `json:"preset,omitempty" doc:"CMS preset: wordpress, joomla, bitrix, opencart (empty = generic php-fpm)"`
}

// SiteUpdateRequest patches a site; every field is optional.
type SiteUpdateRequest struct {
	Aliases        *[]string         `json:"aliases,omitempty"`
	Mode           string            `json:"mode,omitempty" enum:"fpm,apache,proxy"`
	Backend        string            `json:"backend,omitempty"`
	PHPVersion     string            `json:"php_version,omitempty"`
	Docroot        *string           `json:"docroot,omitempty"`
	IP             string            `json:"ip,omitempty"`
	SSL            string            `json:"ssl,omitempty" enum:"auto,none"`
	HTTP2          *bool             `json:"http2,omitempty"`
	HTTP3          *bool             `json:"http3,omitempty"`
	RedirectHTTPS  *bool             `json:"redirect_https,omitempty"`
	RedirectWWW    string            `json:"redirect_www,omitempty" enum:"none,to_www,to_root"`
	StaticByNginx  *bool             `json:"static_by_nginx,omitempty"`
	FPMPM          string            `json:"fpm_pm,omitempty" enum:"ondemand,dynamic,static"`
	FPMMaxChildren int               `json:"fpm_max_children,omitempty" minimum:"1" maximum:"512"`
	PHPIni         map[string]string `json:"php_ini,omitempty" doc:"Merged into the site ini; empty value removes a key"`
	AllowExec      *bool             `json:"allow_exec,omitempty"`
	ClientMaxBody  string            `json:"client_max_body,omitempty" pattern:"^[0-9]+[kKmMgG]?$"`
	AllowFrom      *[]string         `json:"allow_from,omitempty" maxItems:"64" doc:"Replace the IP allow-list; [] opens the site to everyone"`
	Preset         *string           `json:"preset,omitempty" doc:"CMS preset: wordpress, joomla, bitrix, opencart or \"\" for generic"`
}

// SiteWithJob is returned by mutations.
type SiteWithJob struct {
	Site  *store.Site `json:"site"`
	JobID int64       `json:"job_id,omitempty"`
}

// DatabaseRequest creates a database with its primary account.
type DatabaseRequest struct {
	Name       string `json:"name" pattern:"^[a-z0-9_]{1,24}$" doc:"Suffix; the full name is <login>_<name>"`
	User       string `json:"user,omitempty" doc:"Owner login (admins only)"`
	Password   string `json:"password,omitempty" minLength:"8" maxLength:"128"`
	LegacyAuth *bool  `json:"legacy_auth,omitempty" doc:"Use mysql_native_password (PHP < 7.4); default: auto by the owner's sites"`
	Remote     bool   `json:"remote,omitempty" doc:"Also allow connections from other hosts (needs the firewall stage)"`
}

// DatabaseResponse returns the database and, once, the generated password.
type DatabaseResponse struct {
	Database *store.Database `json:"database"`
	Password string          `json:"password,omitempty"`
	DSN      string          `json:"dsn,omitempty"`
}

// PasswordRequest sets a new password (generated when empty).
type PasswordRequest struct {
	Password string `json:"password,omitempty" minLength:"8" maxLength:"128"`
}

// DBEngineStatus describes the database server.
type DBEngineStatus struct {
	Installed bool              `json:"installed"`
	Instance  *store.DBInstance `json:"instance,omitempty"`
	Service   *systemd.Status   `json:"service,omitempty"`
	Databases int               `json:"databases"`
}

// CronRequest adds a cron job.
type CronRequest struct {
	Schedule string `json:"schedule" minLength:"1" maxLength:"64" doc:"5-field cron expression or @daily/@hourly/@weekly/@monthly/@reboot"`
	Command  string `json:"command" minLength:"1" maxLength:"1000"`
	Comment  string `json:"comment,omitempty" maxLength:"120"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// CronUpdateRequest edits a cron job.
type CronUpdateRequest struct {
	Schedule string  `json:"schedule,omitempty" maxLength:"64"`
	Command  string  `json:"command,omitempty" maxLength:"1000"`
	Comment  *string `json:"comment,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// FirewallRuleRequest adds an allow/deny rule.
type FirewallRuleRequest struct {
	Kind    string `json:"kind" enum:"allow,deny"`
	Proto   string `json:"proto,omitempty" enum:"tcp,udp,any"`
	Port    string `json:"port,omitempty" doc:"N or N-M"`
	Source  string `json:"source,omitempty" doc:"IP or CIDR"`
	Comment string `json:"comment,omitempty" maxLength:"120"`
}

// BanRequest bans or unbans an address.
type BanRequest struct {
	IP string `json:"ip" minLength:"1"`
}

// JailStatus is one fail2ban jail.
type JailStatus struct {
	Name   string   `json:"name"`
	Banned int      `json:"banned"`
	Total  int      `json:"total"`
	IPs    []string `json:"ips,omitempty"`
}

// Fail2banStatus summarises fail2ban.
type Fail2banStatus struct {
	Installed bool         `json:"installed"`
	Running   bool         `json:"running"`
	Jails     []JailStatus `json:"jails"`
}

// FirewallStatus is the firewall page.
type FirewallStatus struct {
	Enabled   bool                  `json:"enabled"`
	Active    bool                  `json:"active" doc:"nftables table currently loaded"`
	PanelPort int                   `json:"panel_port"`
	SSHPorts  []int                 `json:"ssh_ports"`
	Rules     []*store.FirewallRule `json:"rules"`
	Fail2ban  *Fail2banStatus       `json:"fail2ban,omitempty"`
}

// Metrics is a time series of host metrics.
type Metrics struct {
	Range       string               `json:"range"`
	StepSeconds int64                `json:"step_seconds"`
	Points      []*store.MetricPoint `json:"points"`
}

// LogTail is the tail of a log.
type LogTail struct {
	Path  string   `json:"path"`
	Size  int64    `json:"size,omitempty"`
	Lines []string `json:"lines"`
}

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status" enum:"ok,warn,fail"`
	Detail string `json:"detail,omitempty"`
}

// Doctor is the health report.
type Doctor struct {
	Summary string  `json:"summary"`
	Checks  []Check `json:"checks"`
}

// BackupTargetRequest registers a restic repository.
type BackupTargetRequest struct {
	Name        string            `json:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	Type        string            `json:"type" enum:"local,sftp,s3,b2,rest"`
	Repository  string            `json:"repository" minLength:"1" doc:"restic repository: /path, sftp:user@host:/path, s3:https://…/bucket, b2:bucket, rest:https://…"`
	Password    string            `json:"password,omitempty" doc:"repository password (generated when empty; keep it!)"`
	Env         map[string]string `json:"env,omitempty" doc:"credentials such as AWS_ACCESS_KEY_ID"`
	KeepDaily   int               `json:"keep_daily,omitempty" minimum:"0" maximum:"365"`
	KeepWeekly  int               `json:"keep_weekly,omitempty" minimum:"0" maximum:"104"`
	KeepMonthly int               `json:"keep_monthly,omitempty" minimum:"0" maximum:"120"`
	Schedule    string            `json:"schedule,omitempty" enum:",daily"`
}

// BackupTargetResponse returns the target and, once, a generated password.
type BackupTargetResponse struct {
	Target   *store.BackupTarget `json:"target"`
	Password string              `json:"password,omitempty"`
}

// BackupRunRequest starts a backup.
type BackupRunRequest struct {
	Target string `json:"target" minLength:"1" doc:"target name or id"`
	Scope  string `json:"scope,omitempty" doc:"server | user:<login> | site:<domain> | db:<name>"`
}

// RestoreRequest restores a snapshot.
type RestoreRequest struct {
	Target   string   `json:"target" minLength:"1"`
	Snapshot string   `json:"snapshot" minLength:"1"`
	Include  []string `json:"include,omitempty" doc:"paths to restore (all when empty)"`
	InPlace  bool     `json:"in_place,omitempty" doc:"restore to / (requires include); otherwise to <data>/restore/<snapshot>"`
}

// Snapshot is a restic snapshot.
type Snapshot struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Paths    []string `json:"paths"`
	Tags     []string `json:"tags"`
}

// TOTPSetup returns a freshly generated secret.
type TOTPSetup struct {
	Secret string `json:"secret"`
	URL    string `json:"url" doc:"otpauth:// URL for authenticator apps (render as QR)"`
}

// TOTPCode confirms a code.
type TOTPCode struct {
	Code string `json:"code" minLength:"6" maxLength:"8"`
}

// TOTPDisable turns 2FA off.
type TOTPDisable struct {
	Password string `json:"password" minLength:"1"`
}

// TOTPStatus reports whether 2FA is on.
type TOTPStatus struct {
	Enabled bool `json:"enabled"`
}

// Webhook is a subscriber.
type Webhook struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	Secret    string   `json:"secret,omitempty"`
	Events    []string `json:"events"`
	CreatedAt string   `json:"created_at"`
}

// WebhookRequest adds a subscriber.
type WebhookRequest struct {
	URL    string   `json:"url" format:"uri"`
	Secret string   `json:"secret,omitempty" maxLength:"128"`
	Events []string `json:"events,omitempty" doc:"job.done, job.failed, site.apply.done, cert.issue.failed, backup.run.* or *"`
}

// FileEntry is a directory entry.
type FileEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mtime"`
	Target  string `json:"target,omitempty"`
}

// FileList is a directory listing.
type FileList struct {
	User    string      `json:"user"`
	Home    string      `json:"home"`
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileOpRequest is a file manager operation.
type FileOpRequest struct {
	User  string   `json:"user,omitempty"`
	Op    string   `json:"op" enum:"mkdir,rm,mv,chmod,extract,size"`
	Path  string   `json:"path" minLength:"1"`
	Paths []string `json:"paths,omitempty" doc:"extra paths for rm"`
	Dest  string   `json:"dest,omitempty"`
	Mode  string   `json:"mode,omitempty" pattern:"^[0-7]{3,4}$"`
}

// FileOpResult reports success and an optional value (size, extracted count).
type FileOpResult struct {
	OK    bool  `json:"ok"`
	Value int64 `json:"value,omitempty"`
}

// UserUpdateRequest changes account settings.
type UserUpdateRequest struct {
	Email    string `json:"email,omitempty" format:"email"`
	Shell    *bool  `json:"shell,omitempty" doc:"SSH shell (true) or SFTP-only chroot (false)"`
	Password string `json:"password,omitempty" minLength:"8" maxLength:"1024" doc:"panel and SFTP/SSH password"`
	Status   string `json:"status,omitempty" enum:"active,suspended"`
}

// AppRequest creates an app service: a systemd unit that runs a long-lived
// program (gunicorn, a Node server, a bot) under the user's unix account.
type AppRequest struct {
	Name        string   `json:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	User        string   `json:"user,omitempty" doc:"Owner login (admins only; users create their own)"`
	Description string   `json:"description,omitempty" maxLength:"120"`
	Command     string   `json:"command" minLength:"1" maxLength:"1000" doc:"Absolute path of the executable followed by its arguments"`
	WorkDir     string   `json:"workdir,omitempty" doc:"Working directory inside the home (default ~/data)"`
	EnvFile     string   `json:"env_file,omitempty" doc:"EnvironmentFile inside the home"`
	Env         []string `json:"env,omitempty" maxItems:"64" doc:"KEY=value pairs"`
	Restart     string   `json:"restart,omitempty" enum:"always,on-failure,no" default:"always"`
	Enabled     *bool    `json:"enabled,omitempty" doc:"Start now and at boot (default true)"`
}

// AppUpdateRequest edits an app; every field is optional.
type AppUpdateRequest struct {
	Description *string   `json:"description,omitempty"`
	Command     string    `json:"command,omitempty" maxLength:"1000"`
	WorkDir     *string   `json:"workdir,omitempty"`
	EnvFile     *string   `json:"env_file,omitempty"`
	Env         *[]string `json:"env,omitempty" maxItems:"64" doc:"Replaces the whole list"`
	Restart     string    `json:"restart,omitempty" enum:"always,on-failure,no"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

// AppStatus is an app with the live state of its unit.
type AppStatus struct {
	App     *store.App      `json:"app"`
	Service *systemd.Status `json:"service,omitempty"`
}

// ImportCertificateRequest installs an existing certificate. Let's Encrypt
// certificates are renewed through ACME when due unless auto_renew is false.
type ImportCertificateRequest struct {
	Name        string `json:"name,omitempty" doc:"Primary hostname (default: certificate subject)"`
	Certificate string `json:"certificate" minLength:"1" doc:"PEM: leaf certificate followed by intermediates"`
	PrivateKey  string `json:"private_key" minLength:"1" doc:"PEM private key"`
	AutoRenew   *bool  `json:"auto_renew,omitempty" doc:"Re-issue via Let's Encrypt 30 days before expiry (default: yes for Let's Encrypt certificates)"`
}

// RealIPSettings tells nginx which proxies may set the real client address.
type RealIPSettings struct {
	Cloudflare bool     `json:"cloudflare" doc:"Trust Cloudflare's published ranges (CF-Connecting-IP)"`
	From       []string `json:"from,omitempty" maxItems:"64" doc:"Additional trusted proxies (IP or CIDR); X-Forwarded-For is then honoured recursively"`
}

// SiteNginx shows the generated server block and the custom include of a site.
type SiteNginx struct {
	ConfigPath string   `json:"config_path" doc:"Generated file (read-only, rewritten on every apply)"`
	Generated  string   `json:"generated"`
	IncludeDir string   `json:"include_dir" doc:"Every *.conf here is included inside server {}"`
	CustomPath string   `json:"custom_path" doc:"The file edited through the panel"`
	Custom     string   `json:"custom"`
	Others     []string `json:"others" doc:"Other include files present in the directory"`
}

// SiteNginxRequest replaces the custom directives of a site.
type SiteNginxRequest struct {
	Custom string `json:"custom" maxLength:"65536" doc:"nginx directives inside server {}; empty removes the file"`
}

// PHPValue is one effective php.ini value of a site.
type PHPValue struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source" enum:"default,preset,site"`
}

// SitePHP lists the effective PHP settings of a site.
type SitePHP struct {
	Version  string     `json:"version"`
	PoolPath string     `json:"pool_path"`
	Socket   string     `json:"socket"`
	Allowed  []string   `json:"allowed" doc:"Keys accepted in php_ini"`
	Values   []PHPValue `json:"values"`
}

// SitePreset describes a CMS preset selectable when creating a site.
type SitePreset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateSettings says where the panel looks for new versions of itself.
type UpdateSettings struct {
	Repo       string `json:"repo" doc:"Репозиторий с релизами в виде owner/name"`
	API        string `json:"api,omitempty" doc:"Адрес API репозитория; пусто — api.github.com (для GitHub Enterprise)"`
	Channel    string `json:"channel" doc:"stable — только релизы, beta — ещё и предрелизы"`
	HasToken   bool   `json:"has_token" doc:"Токен доступа сохранён (обязателен для приватного репозитория)"`
	CheckHours int    `json:"check_hours" doc:"Как часто проверять обновления; 0 — не проверять"`
	AutoApply  bool   `json:"auto_apply" doc:"Устанавливать найденное обновление без подтверждения"`
}

// UpdateSettingsRequest changes them. Omitted fields keep their value.
type UpdateSettingsRequest struct {
	Repo       string `json:"repo,omitempty" maxLength:"140"`
	API        string `json:"api,omitempty" maxLength:"200" doc:"Адрес API репозитория; \"-\" возвращает api.github.com"`
	Channel    string `json:"channel,omitempty" enum:"stable,beta"`
	Token      string `json:"token,omitempty" maxLength:"512" doc:"Токен доступа к репозиторию; хранится зашифрованным"`
	ClearToken bool   `json:"clear_token,omitempty" doc:"Удалить сохранённый токен"`
	CheckHours *int   `json:"check_hours,omitempty" minimum:"0" maximum:"720"`
	AutoApply  *bool  `json:"auto_apply,omitempty"`
}

// UpdateAttempt is the outcome of the last installation, read back from disk
// after the panel restarted itself.
type UpdateAttempt struct {
	Status   string     `json:"status" doc:"installing, done, failed или rolled-back"`
	From     string     `json:"from,omitempty"`
	To       string     `json:"to,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Error    string     `json:"error,omitempty"`
	Log      string     `json:"log,omitempty"`
}

// UpdateStatus is what the panel knows about its own version.
type UpdateStatus struct {
	Current     string         `json:"current"`
	Latest      string         `json:"latest,omitempty"`
	Tag         string         `json:"tag,omitempty"`
	Notes       string         `json:"notes,omitempty"`
	PublishedAt *time.Time     `json:"published_at,omitempty"`
	Available   bool           `json:"available" doc:"Latest новее текущей версии"`
	KeyPinned   bool           `json:"key_pinned" doc:"Настроен ключ, которым подписаны релизы"`
	Settings    UpdateSettings `json:"settings"`
	CheckedAt   *time.Time     `json:"checked_at,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	LastAttempt *UpdateAttempt `json:"last_attempt,omitempty"`
}

// UpdateApplyRequest installs a version; empty means the latest one.
type UpdateApplyRequest struct {
	Version string `json:"version,omitempty" maxLength:"64" doc:"Версия или тег; по умолчанию последняя в канале"`
}
