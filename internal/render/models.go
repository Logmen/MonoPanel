package render

import "strings"

// NginxMain feeds nginx/nginx.conf.tmpl.
type NginxMain struct {
	User string
}

// IPDefault feeds nginx/ip-default.conf.tmpl (one per IP address).
type IPDefault struct {
	IP       string
	TLS      bool
	HTTP3    bool
	CertPath string
	KeyPath  string
}

// Site feeds nginx/site.conf.tmpl and apache/site.conf.tmpl.
type Site struct {
	Domain            string
	Aliases           []string
	Mode              string // fpm | apache
	IP                string
	TLS               bool
	HTTP2             bool
	HTTP3             bool
	RedirectHTTPS     bool
	RedirectWWW       string // none | to_www | to_root
	CertPath          string
	KeyPath           string
	Docroot           string
	LogDir            string
	IncludeDir        string
	ClientMaxBodySize string
	StaticByNginx     bool
	ApacheBackend     string
	FPMSocket         string
	ProxyTimeout      int
	Backend           string
	AllowFrom         []string // IPs/CIDRs allowed to open the site; empty = everyone
	HSTS              bool     // send Strict-Transport-Security (TLS + forced HTTPS)
	Preset            string   // wordpress | joomla | bitrix | opencart | "" (generic php-fpm locations)
}

// Plain is the site as seen by its :80 server: no TLS-only headers.
func (s Site) Plain() Site {
	s.TLS, s.HTTP3, s.HSTS = false, false, false
	return s
}

// ServerNames joins the domain and its aliases for server_name.
func (s Site) ServerNames() string {
	return strings.Join(append([]string{s.Domain}, s.Aliases...), " ")
}

// ApacheMain feeds apache/httpd.conf.tmpl. Listen is empty on Debian, where
// ports.conf owns it.
type ApacheMain struct {
	Listen   string
	Backend  string
	SitesDir string
}

// PHPIni feeds php/monopanel.ini.tmpl.
type PHPIni struct {
	Version       string
	Timezone      string
	OpcacheMemory int
}

// Welcome feeds site/index.html.tmpl (placeholder page for an empty docroot;
// deliberately shows nothing but the domain).
type Welcome struct {
	Domain string
}

// KV is an ordered php_value entry.
type KV struct {
	Key   string
	Value string
}

// Pool feeds php-fpm/pool.conf.tmpl.
type Pool struct {
	Name             string
	User             string
	Group            string
	Socket           string
	ListenGroup      string
	PM               string // ondemand | dynamic | static
	MaxChildren      int
	StartServers     int
	MinSpare         int
	MaxSpare         int
	TerminateTimeout int
	Home             string
	DataDir          string
	TmpDir           string
	LogDir           string
	BinDir           string
	SendmailFrom     string
	DisableFunctions string
	Values           []KV
}

// DefaultDisableFunctions is the per-site default; "allow exec" clears it.
const DefaultDisableFunctions = "passthru,shell_exec,system,proc_open,popen,pcntl_exec,pcntl_fork"

// MySQLConf feeds mysql/monopanel.cnf.tmpl.
type MySQLConf struct {
	RAMMB          int
	BindAddress    string
	BufferPoolMB   int
	RedoLogMB      int
	MaxConnections int
	SlowLog        string
	NativePassword bool
}

// Firewall feeds nftables/monopanel.nft.tmpl: pre-rendered rule lines.
type Firewall struct {
	Policy string
	Allow  []string
	Deny   []string
}

// Fail2ban feeds fail2ban/jail.local.tmpl.
type Fail2ban struct {
	BanTime   string
	FindTime  string
	MaxRetry  int
	PanelPort string
	Nginx     bool
	IgnoreIPs []string
}

// Crontab feeds cron/crontab.tmpl.
type Crontab struct {
	Login  string
	BinDir string
	Jobs   []CronLine
}

// CronLine is one crontab entry.
type CronLine struct {
	Schedule string
	Command  string
	Enabled  bool
	Comment  string
}

// AppUnit feeds systemd/app.service.tmpl.
type AppUnit struct {
	Login       string
	Name        string
	Description string
	Command     string
	WorkDir     string
	EnvFile     string
	Env         []string
	Restart     string
}

// RealIP feeds nginx/real-ip.conf.tmpl.
type RealIP struct {
	Cloudflare       bool
	CloudflareRanges []string
	From             []string
}

// CloudflareRanges are the edge networks published at https://www.cloudflare.com/ips/.
var CloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18", "108.162.192.0/18",
	"190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// Postfix feeds mail/main.cf.tmpl.
type Postfix struct {
	Hostname         string
	MapsDir          string
	MailBase         string
	MessageSizeBytes int64
	VmailUID         int
	VmailGID         int
	TLS              bool
	CertPath         string
	KeyPath          string
	RBL              []string
	// Milter is the opendkim socket in Postfix notation, empty when DKIM is off.
	Milter string
}

// PostfixMaster feeds mail/master.cf.tmpl. Port25 is off on hosts where
// something else already owns the SMTP port.
type PostfixMaster struct {
	MapsDir string
	TLS     bool
	Port25  bool
}

// Dovecot feeds mail/dovecot.conf.tmpl (2.3 syntax).
type Dovecot struct {
	Hostname   string
	MailBase   string
	UsersFile  string
	VmailUser  string
	VmailUID   int
	VmailGID   int
	TLS        bool
	CertPath   string
	KeyPath    string
	POP3       bool
	Postmaster string
}

// OpenDKIM feeds mail/opendkim.conf.tmpl.
type OpenDKIM struct {
	Socket       string
	KeyTable     string
	SigningTable string
	TrustedHosts string
}

// Roundcube feeds mail/roundcube.inc.php.tmpl.
type Roundcube struct {
	DSN         string
	IMAPHost    string
	SMTPHost    string
	SieveHost   string
	VerifyPeer  bool
	SupportURL  string
	ProductName string
	DESKey      string
	Domain      string
	TempDir     string
	Plugins     []string
}

// Webmail feeds mail/webmail.conf.tmpl: Roundcube on a port of the mail host,
// so a webmail needs no DNS record of its own.
type Webmail struct {
	IP                string
	Port              int
	ServerName        string
	CertPath          string
	KeyPath           string
	Docroot           string
	LogDir            string
	Socket            string
	ClientMaxBodySize string
}
