package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"monopanel/internal/osprofile"
	"monopanel/internal/phpmyadmin"
)

// planCrontab: a client's crontab is replaced from stdin (internal/api/
// ops_cron.go) or removed with the account (ops_users_delete.go). Only the
// panel's clients: crontab -u root is a root shell on the next minute.
func (s *Server) planCrontab(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	var login string
	switch {
	case len(c.args) == 3 && c.args[0] == "-u" && c.args[2] == "-":
		login = c.args[1]
	case len(c.args) == 3 && c.args[0] == "-r" && c.args[1] == "-u":
		login = c.args[2]
	default:
		return nil, refuseArgs("crontab", c.args, "the panel only installs (-u <login> -) or removes (-r -u <login>) a client's crontab")
	}
	if _, err := s.panelAccount(login); err != nil {
		if err == errNoAccount {
			return nil, notAllowed("the crontab of "+login, "there is no such account")
		}
		return nil, err
	}
	return &toolPlan{args: c.args}, nil
}

// planDu: the size of a home or of a mail domain before a migration
// (internal/api/ops_migrate.go duBytes, which reads the number only). du
// resolves the directories on the way to its argument itself, and a symlink
// an account left in its home would have it measure /root: the agent opens
// the directory without following one and hands it over as fd 3
// (--dereference-args follows that argument alone, nothing below it).
func (s *Server) planDu(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	if len(c.args) != 2 || c.args[0] != "-sb" {
		return nil, refuseArgs("du", c.args, "the panel only asks du -sb <directory>")
	}
	dir := c.args[1]
	if !cleanAbs(dir) || !(below(dir, s.cfg.WWWRoot) || below(dir, mailStore)) || s.inRestoreArea(dir) {
		return nil, notAllowed("du of "+dir, "only the accounts' homes and the mail store are measured")
	}
	root := s.cfg.WWWRoot
	if below(dir, mailStore) {
		root = mailStore
	}
	fd, err := openDirBelow(root, dir, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &Error{Status: http.StatusNotFound, Message: "du: " + err.Error()}
	}
	if err != nil {
		return nil, notAllowed("du of "+dir, err.Error())
	}
	return &toolPlan{args: []string{"-sb", "--dereference-args", "/proc/self/fd/3"}, files: []*os.File{fdFile(fd, dir)}}, nil
}

var (
	phpModuleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
)

// planPHPMod: phpenmod/phpdismod -v <branch> <module> switch an extension on
// Debian (internal/api/ops_php_ext.go).
func (s *Server) planPHPMod(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	if len(c.args) != 3 || c.args[0] != "-v" || !osprofile.ValidPHPVersion(c.args[1]) || !phpModuleRe.MatchString(c.args[2]) {
		return nil, refuseArgs(c.name, c.args, "the panel only runs "+c.name+" -v <branch> <module>")
	}
	return &toolPlan{args: c.args}, nil
}

// planSSHD: sshd -T prints the effective configuration, the firewall reads
// the ports from it (internal/api/ops_firewall.go). -f, -o, -D, -d are a
// root sshd of the caller's configuration.
func (s *Server) planSSHD(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{"-T"})
}

var journalUnitRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\\-]*\.(service|socket|device|mount|automount|swap|target|path|timer|slice|scope)$`)

// planJournalctl: the journal of a unit (internal/api/metrics.go system
// logs, ops_apps.go app logs). Reading any unit's journal is an
// administrator's feature; --file, -D, --vacuum-*, --flush, --rotate and
// --setup-keys are not reading a unit's journal.
func (s *Server) planJournalctl(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	if len(a) == 7 && a[0] == "-u" && journalUnitRe.MatchString(a[1]) && a[2] == "-n" && a[4] == "--no-pager" && a[5] == "-o" && a[6] == "short-iso" {
		if n, err := strconv.Atoi(a[3]); err == nil && n >= 1 && n <= 5000 && strconv.Itoa(n) == a[3] {
			return &toolPlan{args: a}, nil
		}
	}
	return nil, refuseArgs("journalctl", a, "the panel only reads a unit's journal: -u <unit> -n <1..5000> --no-pager -o short-iso")
}

var (
	dnfRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	// pkgNameRe is a package name: no path, no URL, no option, no version
	// pin. rpm and deb names alike start with a letter or digit.
	pkgNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+._:~-]*$`)
)

// validPkgName accepts a package name, not a file: dnf and apt take a name
// ending in .rpm or .deb for a local package.
func validPkgName(p string) bool {
	return pkgNameRe.MatchString(p) && !strings.HasSuffix(p, ".rpm") && !strings.HasSuffix(p, ".deb")
}

// planDnf: the mail installation looks for CodeReady Builder and, when the
// packages need it, installs them with that repository on for one
// transaction (internal/api/ops_mail_install.go crbRepo,
// installMailPackagesEL). --setopt, --nogpgcheck, -c, --repofrompath and
// local or remote packages are not among them.
func (s *Server) planDnf(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	if s.profile.Family() != osprofile.FamilyRHEL {
		return nil, notAllowed("dnf", "this is not an EL host")
	}
	a := c.args
	if equalArgs(a, []string{"-q", "repolist", "--all"}) || equalArgs(a, []string{"-q", "-C", "repolist", "--all"}) {
		return &toolPlan{args: a}, nil
	}
	if len(a) >= 4 && a[0] == "-y" && a[1] == "install" && strings.HasPrefix(a[2], "--enablerepo=") {
		repo := strings.TrimPrefix(a[2], "--enablerepo=")
		lower := strings.ToLower(repo)
		if !dnfRepoRe.MatchString(repo) || !(lower == "crb" || strings.Contains(lower, "codeready")) {
			return nil, notAllowed("dnf --enablerepo="+repo, "only CodeReady Builder is switched on for a transaction")
		}
		for _, p := range a[3:] {
			if !validPkgName(p) {
				return nil, notAllowed("the package "+p, "only package names are installed, no files, URLs or options")
			}
		}
		if c.run != "" && !runIDRe.MatchString(c.run) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid run id " + strconv.Quote(c.run)}
		}
		run := c.run
		// As /v1/pkg install: the scriptlets of what it upgrades on the
		// way restart php-fpm and nginx, and the transaction runs on to its
		// end in a unit of its own, whatever becomes of the request or the
		// agent (pkg_unit.go).
		return &toolPlan{args: a, wrap: func(ctx context.Context, _ toolRun, out io.Writer) (int, error) {
			if err := s.vetPackageScripts(ctx, "install"); err != nil {
				return 0, err
			}
			return s.runPkgTool(ctx, run, append([]string{"dnf"}, a...), out)
		}}, nil
	}
	return nil, refuseArgs("dnf", a, "not a form the panel uses")
}

// planPerconaRelease: the Percona repository for Percona Server 8.4
// (internal/api/ops_db.go).
func (s *Server) planPerconaRelease(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{"setup", "ps-84-lts"})
}

var jailRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// planFail2banClient: the jails' state and unbanning an address
// (internal/api/ops_firewall.go). set <jail> action…, addaction, -c and
// the like make fail2ban (root) run a command of the caller's choosing.
func (s *Server) planFail2banClient(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	switch {
	case equalArgs(a, []string{"status"}):
	case len(a) == 2 && a[0] == "status" && jailRe.MatchString(a[1]):
	case len(a) == 2 && a[0] == "unban" && validAddress(a[1]):
	default:
		return nil, refuseArgs("fail2ban-client", a, "the panel only asks status, status <jail> and unban <address>")
	}
	return &toolPlan{args: a}, nil
}

// validAddress accepts an IP address or a network in CIDR notation.
func validAddress(v string) bool {
	if net.ParseIP(v) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(v)
	return err == nil
}

// nftRulesPath is the panel's nftables table (internal/api/ops_firewall.go).
const nftRulesPath = "/etc/nftables.d/monopanel.nft"

// planNft: check and load the panel's table, list it, delete it
// (internal/api/ops_firewall.go).
func (s *Server) planNft(c *toolCall) (*toolPlan, error) {
	return exactShapes(c,
		[]string{"-c", "-f", nftRulesPath},
		[]string{"-f", nftRulesPath},
		[]string{"list", "table", "inet", "monopanel"},
		[]string{"delete", "table", "inet", "monopanel"},
	)
}

var (
	firewalldPortRe  = regexp.MustCompile(`^--add-port=([0-9]{1,5})/(tcp|udp)$`)
	firewalldQueryRe = regexp.MustCompile(`^--query-port=([0-9]{1,5})/(tcp|udp)$`)
)

// planFirewallCmd: firewalld's state, whether a port is open (now and for
// good), opening the panel's ports (for good and now — no --reload, which
// would drop everybody else's runtime rules) and closing UDP 443 the panel
// opened for HTTP/3, on either side (internal/api/firewalld.go,
// firewall_http3.go). Closing goes no further: SSH never runs on UDP, so the
// one port the API may close cannot lock anybody out.
func (s *Server) planFirewallCmd(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	if equalArgs(a, []string{"--state"}) ||
		equalArgs(a, []string{"-q", "--remove-port=443/udp"}) ||
		equalArgs(a, []string{"-q", "--permanent", "--remove-port=443/udp"}) {
		return &toolPlan{args: a}, nil
	}
	if len(a) < 2 || a[0] != "-q" {
		return nil, refuseArgs("firewall-cmd", a, "not a form the panel uses")
	}
	rest := a[1:]
	if rest[0] == "--permanent" {
		rest = rest[1:]
	}
	if len(rest) == 1 && firewalldQueryRe.MatchString(rest[0]) {
		return &toolPlan{args: a}, nil
	}
	if len(rest) == 0 {
		return nil, refuseArgs("firewall-cmd", a, "not a form the panel uses")
	}
	for _, p := range rest {
		m := firewalldPortRe.FindStringSubmatch(p)
		if m == nil {
			return nil, refuseArgs("firewall-cmd", a, "the panel only adds ports (--add-port=<port>/tcp)")
		}
		if n, _ := strconv.Atoi(m[1]); n < 1 || n > 65535 {
			return nil, notAllowed("the port "+m[1], "out of range")
		}
	}
	return &toolPlan{args: a}, nil
}

// selinuxContexts are the file contexts the panel adds, rule → type
// (internal/api/selinux.go selinuxFileContexts and selinuxSiteLogsContext,
// ops_mail_install.go selinuxMailPolicy, ops_valkey.go). /run and /var/run
// are one directory to the policy, and semanage asks for the spelling it
// wants ("Try adding …"): both are accepted.
func (s *Server) selinuxContexts() map[string]string {
	m := map[string]string{
		"/var/www/[^/]+/data/www(/.*)?":                         "httpd_sys_content_t",
		"/run/monopanel(/.*)?":                                  "httpd_var_run_t",
		"/var/www/[^/]+/data/logs(/.*)?":                        "httpd_log_t",
		`/var/www/[^/]+/\.ssh(/.*)?`:                            "ssh_home_t",
		filepath.Join(s.cfg.LogDir, "sites") + "(/.*)?":         "httpd_log_t",
		filepath.Join(s.cfg.DataDir, "certs") + "(/.*)?":        "cert_t",
		"/run/monopanel-valkey(/.*)?":                           "redis_var_run_t",
		"/var/lib/monopanel-valkey/[^/]+(/.*)?":                 "redis_var_lib_t",
		filepath.Clean(s.cfg.RunDir) + "(/.*)?":                 "httpd_var_run_t",
		filepath.Clean(s.cfg.WWWRoot) + `/[^/]+/\.ssh(/.*)?`:    "ssh_home_t",
		filepath.Clean(s.cfg.WWWRoot) + "/[^/]+/data/www(/.*)?": "httpd_sys_content_t",
		// phpMyAdmin (ops_phpmyadmin.go): its code nginx and its pool
		// read, its sessions and temporary files, its tickets.
		phpmyadmin.Root + "(/.*)?":     "httpd_sys_content_t",
		phpmyadmin.StateDir + "(/.*)?": "httpd_sys_rw_content_t",
		phpmyadmin.RunDir + "(/.*)?":   "httpd_var_run_t",
		// The mail server's self-signed pair (ops_mail.go mailTLS): the two
		// files alone, the rest of <data>/mail stays var_lib_t.
		filepath.Join(s.cfg.DataDir, "mail") + `/tls\.(crt|key)`: "cert_t",
	}
	out := make(map[string]string, 2*len(m))
	for spec, label := range m {
		out[spec] = label
		switch {
		case strings.HasPrefix(spec, "/run/"):
			out["/var"+spec] = label
		case strings.HasPrefix(spec, "/var/run/"):
			out[strings.TrimPrefix(spec, "/var")] = label
		}
	}
	return out
}

// planSemanage: the panel's file contexts and an http_port_t label for a
// port nginx listens on (internal/api/selinux.go).
func (s *Server) planSemanage(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	if len(a) == 5 && a[0] == "fcontext" && (a[1] == "-a" || a[1] == "-m") && a[2] == "-t" {
		if label, ok := s.selinuxContexts()[a[4]]; ok && label == a[3] {
			return &toolPlan{args: a}, nil
		}
		return nil, notAllowed("the file context "+a[4]+" → "+a[3], "the panel labels only its own directories")
	}
	if len(a) == 7 && a[0] == "port" && (a[1] == "-a" || a[1] == "-m") && a[2] == "-t" && a[3] == "http_port_t" && a[4] == "-p" && a[5] == "tcp" {
		if n, err := strconv.Atoi(a[6]); err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == a[6] {
			return &toolPlan{args: a}, nil
		}
	}
	return nil, refuseArgs("semanage", a, "the panel only adds its file contexts and http ports")
}

// selinuxBooleans are the booleans the panel sets (internal/api/selinux.go).
var selinuxBooleans = map[string]bool{
	"httpd_unified": true, "httpd_can_network_connect": true, "httpd_can_network_connect_db": true,
	"httpd_can_sendmail": true, "httpd_execmem": true, "httpd_setrlimit": true,
}

func (s *Server) planSetsebool(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	if len(a) < 2 || a[0] != "-P" {
		return nil, refuseArgs("setsebool", a, "the panel only runs setsebool -P <boolean>=<value>…")
	}
	for _, kv := range a[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !selinuxBooleans[k] || (v != "0" && v != "1") {
			return nil, notAllowed("the boolean "+kv, "the panel sets only its hosting booleans")
		}
	}
	return &toolPlan{args: a}, nil
}

// selinuxModulePath is the panel's policy module (internal/api/selinux.go).
const selinuxModulePath = "/etc/monopanel/selinux/monopanel.cil"

func (s *Server) planSemodule(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{"-i", selinuxModulePath})
}

// planRestorecon: labels back to what the policy says, for the trees the
// panel manages (internal/api/selinux.go, ops_mail_install.go).
func (s *Server) planRestorecon(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	i := 0
	for ; i < len(a) && strings.HasPrefix(a[i], "-"); i++ {
		switch a[i] {
		case "-R", "-F", "-i", "-RF", "-FR":
		default:
			return nil, notAllowed("restorecon "+a[i], "the panel uses only -R, -F and -i")
		}
	}
	if i == len(a) {
		return nil, refuseArgs("restorecon", a, "no path")
	}
	// the hosting tree, the site logs, the mail certificates and the mail
	// server's self-signed pair, the mail store after a migration
	// (ops_migrate_import.go relabel)
	roots := []string{s.cfg.WWWRoot, s.cfg.RunDir, s.cfg.LogDir, filepath.Join(s.cfg.DataDir, "certs"), "/etc/nginx", "/var/log/nginx", mailStore,
		filepath.Join(s.cfg.DataDir, "mail", "tls.crt"), filepath.Join(s.cfg.DataDir, "mail", "tls.key"),
		phpmyadmin.Root, phpmyadmin.StateDir, phpmyadmin.RunDir}
	for _, p := range a[i:] {
		ok := false
		for _, r := range roots {
			if cleanAbs(p) && atOrBelow(p, r) {
				ok = true
			}
		}
		if !ok {
			return nil, notAllowed("restorecon of "+p, "the panel relabels only its own trees")
		}
	}
	return &toolPlan{args: a}, nil
}

var (
	ausearchDateRe = regexp.MustCompile(`^[0-9]{2}/[0-9]{2}/[0-9]{2}$`)
	ausearchTimeRe = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}:[0-9]{2}$`)
)

// planAusearch: today's AVC denials, or those since a time, for the doctor
// (internal/api/selinux.go selinuxCheck).
func (s *Server) planAusearch(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	a := c.args
	if equalArgs(a, []string{"-m", "AVC", "-ts", "today", "--raw"}) {
		return &toolPlan{args: a}, nil
	}
	if len(a) == 6 && a[0] == "-m" && a[1] == "AVC" && a[2] == "-ts" && ausearchDateRe.MatchString(a[3]) && ausearchTimeRe.MatchString(a[4]) && a[5] == "--raw" {
		return &toolPlan{args: a}, nil
	}
	return nil, refuseArgs("ausearch", a, "the panel only reads today's AVC records")
}

// planSetenforce: mp selinux enforcing|permissive.
func (s *Server) planSetenforce(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{"0"}, []string{"1"})
}

var postmapArgRe = regexp.MustCompile(`^(hash|btree|lmdb|cdb):(/etc/postfix/[A-Za-z0-9_./-]+)$`)

// planPostmap: the panel's lookup tables (internal/api/ops_mail.go).
func (s *Server) planPostmap(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	if len(c.args) == 1 {
		if m := postmapArgRe.FindStringSubmatch(c.args[0]); m != nil && cleanAbs(m[2]) {
			return &toolPlan{args: c.args}, nil
		}
	}
	return nil, refuseArgs("postmap", c.args, "the panel only builds tables below /etc/postfix (postmap <type>:<file>)")
}

// planPostconf: which lookup table types postfix has (ops_mail_install.go).
func (s *Server) planPostconf(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{"-m"})
}

func (s *Server) planNewaliases(c *toolCall) (*toolPlan, error) {
	return exactShapes(c, []string{})
}
