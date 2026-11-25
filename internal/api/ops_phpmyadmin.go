package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/phpmyadmin"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// phpMyAdmin is the panel's (internal/phpmyadmin): a pinned release in a
// root-owned tree, its own php-fpm pool as its own system user, and an nginx
// server on a port of its own with the panel's name and certificate. The
// login form takes a database user's name and password; the panel's button
// (POST /databases/{name}/phpmyadmin) makes a temporary MySQL account
// granted on that one database and hands it over a one-time ticket only the
// pool user reads. The panel never learns the database users' passwords.

const (
	settingPMAVersion = "pma.version"     // the installed release; "" — not installed
	settingPMAPort    = "pma.port"        // the port it is served on
	settingPMAPHP     = "pma.php"         // the PHP branch of its pool
	settingPMASecret  = "pma.secret"      // blowfish_secret, encrypted with the secret key
	settingPMAReady   = "pma.port_ready"  // the port SELinux and firewalld were prepared for
	settingPMATemp    = "pma.temp_users"  // the temporary accounts of the button (pmaTemp, JSON)
	settingPMACert    = "pma.certificate" // the panel certificate's fingerprint the server block serves
)

// pmaMinPHP and pmaMaxPHP bound the branches the pinned release runs on
// (composer.json: ^7.2.5 || ^8.0, with 8.4's and 8.5's deprecations fixed
// in 5.2.2 and 5.2.3); 7.4 is the first whose mysqli speaks
// caching_sha2_password, the accounts' plugin.
const (
	pmaMinPHP = "7.4"
	pmaMaxPHP = "8.5"
)

// pmaDBRe is a database the button grants on: the panel's names.
var pmaDBRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// pmaPerDatabase bounds the live temporary accounts of one actor on one
// database: a press past it drops the oldest of them (its session ends), so
// that a scripted button makes no more MySQL accounts than this per
// database it may open.
const pmaPerDatabase = 3

// pmaTemp is a temporary MySQL account of the button.
type pmaTemp struct {
	User     string    `json:"user"`
	Database string    `json:"database"`
	Actor    string    `json:"actor"`
	Expires  time.Time `json:"expires"`
	// Ticket is the file name of its ticket: the sweep drops the account
	// once the agent took the ticket away unused (nobody signed in).
	Ticket string `json:"ticket,omitempty"`
}

type pmaPayload struct {
	Port       int    `json:"port,omitempty"`
	PHPVersion string `json:"php_version,omitempty"`
	// Upgrade: the start of a panel that pins a newer release; it keeps
	// the port and the branch.
	Upgrade bool `json:"upgrade,omitempty"`
}

type pmaInstallInput struct {
	Body apitypes.PhpMyAdminInstallRequest
}

type pmaStatusOutput struct {
	Body apitypes.PhpMyAdminStatus
}

type pmaSignonOutput struct {
	Body apitypes.PhpMyAdminSignon
}

// pmaNginxConf is the server block on the port; http.d is included at http
// level, like the webmail's.
func (s *Server) pmaNginxConf() string {
	return path.Join(s.profile.Web().NginxConfDir, "monopanel", "http.d", "monopanel-phpmyadmin.conf")
}

// pmaSocket is the pool's socket, in the panel's socket directory.
func (s *Server) pmaSocket() string { return path.Join(s.cfg.RunDir, "php", phpmyadmin.Socket) }

// pmaState is what the settings say of the install.
type pmaState struct {
	Version, PHP string
	Port         int
}

func (s *Server) pmaState(ctx context.Context) pmaState {
	var st pmaState
	st.Version, _ = s.db.GetSetting(ctx, settingPMAVersion)
	st.PHP, _ = s.db.GetSetting(ctx, settingPMAPHP)
	if v, _ := s.db.GetSetting(ctx, settingPMAPort); v != "" {
		st.Port, _ = strconv.Atoi(v)
	}
	return st
}

func (st pmaState) installed() bool { return st.Version != "" && st.Port > 0 }

// pmaHost is the name phpMyAdmin is opened by: the panel's own, or — a
// panel without one — the name the request reached the panel by.
func (s *Server) pmaHost(ctx context.Context) string {
	if h := acme.NormalizeName(s.cfg.Web.Hostname); h != "" {
		return h
	}
	if host := requestInfo(ctx).Host; host != "" {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		// The CLI on the server itself reaches the API over its socket or
		// loopback: that name would not open phpMyAdmin anywhere else.
		ip := net.ParseIP(host)
		if pmaHostOK(host) && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			if strings.Contains(host, ":") {
				return "[" + host + "]"
			}
			return host
		}
	}
	if ips := localIPv4s(); len(ips) > 0 {
		return ips[0]
	}
	return "localhost"
}

// pmaHostOK is a name or an address that may stand in a URL as it is.
func pmaHostOK(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func (s *Server) pmaURL(ctx context.Context, port int) string {
	return fmt.Sprintf("https://%s:%d/", s.pmaHost(ctx), port)
}

// pmaCert is the panel's certificate as it serves it now: the issued one
// of its name or one that covers it, or the self-signed fallback.
func (s *Server) pmaCert() (certPath, keyPath, fingerprint string) {
	certPath, keyPath = s.tls.Files()
	return certPath, keyPath, s.tls.Info().Certificate.Fingerprint
}

// pmaPortError says why port cannot be phpMyAdmin's: below 1024, or one the
// panel, the sites or the webmail use.
func (s *Server) pmaPortError(ctx context.Context, port int) error {
	switch {
	case port < 1024 || port > 65535:
		return fmt.Errorf("choose a port from 1024 to 65535 for phpMyAdmin, e.g. %d", phpmyadmin.DefaultPort)
	case slices.Contains(s.panelPorts(), port):
		return fmt.Errorf("port %d is the panel's own", port)
	}
	c := s.loadMailConfig(ctx)
	if c.WebmailPort == port {
		return fmt.Errorf("port %d is the webmail's", port)
	}
	if slices.Contains(mailPortsFor(c), port) {
		return fmt.Errorf("port %d is the mail server's", port)
	}
	return nil
}

// pmaPHP picks the branch of the pool: the one asked for, else the one it
// runs on, else the newest installed the release supports.
func (s *Server) pmaPHP(ctx context.Context, want, current string) (string, error) {
	supported := func(v string) bool { return !versionLess(v, pmaMinPHP) && !versionLess(pmaMaxPHP, v) }
	if want != "" {
		if !supported(want) {
			return "", fmt.Errorf("phpMyAdmin %s runs on PHP %s to %s, not %s", phpmyadmin.Version, pmaMinPHP, pmaMaxPHP, want)
		}
		return want, s.checkPHPInstalled(ctx, want)
	}
	if current != "" && supported(current) && s.checkPHPInstalled(ctx, current) == nil {
		return current, nil
	}
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return "", err
	}
	best := ""
	for _, v := range versions {
		if v.Status == store.PHPInstalled && supported(v.Version) && phpLayout(s, v.Version) != nil && (best == "" || versionLess(best, v.Version)) {
			best = v.Version
		}
	}
	if best == "" {
		return "", fmt.Errorf("no PHP branch phpMyAdmin %s runs on is installed (%s to %s): mp php install 8.4", phpmyadmin.Version, pmaMinPHP, pmaMaxPHP)
	}
	return best, nil
}

// pmaSecret is blowfish_secret: made once per host and kept, encrypted with
// the panel's secret key, so that a reinstall or an upgrade signs nobody out.
func (s *Server) pmaSecret(ctx context.Context) (string, error) {
	if s.secrets == nil {
		return "", fmt.Errorf("the secret key %s is unavailable: phpMyAdmin's blowfish_secret is kept encrypted with it", s.cfg.SecretKeyFile)
	}
	if enc, err := s.db.GetSetting(ctx, settingPMASecret); err == nil && enc != "" {
		if v, err := s.secrets.Decrypt(enc); err == nil && len(v) == 64 {
			if _, err := hex.DecodeString(v); err == nil {
				return v, nil
			}
		}
		s.log.Warn("phpmyadmin: the kept blowfish_secret cannot be read; a new one signs everybody out")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	enc, err := s.secrets.Encrypt(secret)
	if err != nil {
		return "", err
	}
	return secret, s.db.SetSetting(context.WithoutCancel(ctx), settingPMASecret, enc)
}

// fetchPMA downloads the pinned release into the panel's download directory
// and checks its digest; an already downloaded file is reused.
func (s *Server) fetchPMA(ctx context.Context, jc *jobs.Context) (string, error) {
	dir := filepath.Join(s.cfg.DataDir, "downloads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	file := filepath.Join(dir, phpmyadmin.Archive())
	if sum, err := fileSHA256(file); err == nil && sum == phpmyadmin.SHA256 {
		jc.Logf("archive already downloaded: %s", file)
		return file, nil
	}
	body, err := fetchLarge(ctx, phpmyadmin.URL())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != phpmyadmin.SHA256 {
		return "", fmt.Errorf("phpMyAdmin archive checksum mismatch: %x", sum)
	}
	if err := os.WriteFile(file, body, 0o640); err != nil {
		return "", err
	}
	jc.Logf("phpMyAdmin %s downloaded (%d KB), sha256 matches", phpmyadmin.Version, len(body)/1024)
	return file, nil
}

// pmaDirs are phpMyAdmin's directories: the tree root's, the state and the
// tickets the pool user's alone.
func pmaDirs(version string) []agent.DirSpec {
	u := phpmyadmin.User
	return []agent.DirSpec{
		{Path: "/opt/monopanel", Mode: 0o755, Owner: "root", Group: "root"},
		{Path: phpmyadmin.Root, Mode: 0o755, Owner: "root", Group: "root"},
		{Path: phpmyadmin.Tree(version), Mode: 0o755, Owner: "root", Group: "root"},
		{Path: phpmyadmin.StateDir, Mode: 0o700, Owner: u, Group: u},
		{Path: path.Join(phpmyadmin.StateDir, "sessions"), Mode: 0o700, Owner: u, Group: u},
		{Path: path.Join(phpmyadmin.StateDir, "tmp"), Mode: 0o700, Owner: u, Group: u},
		{Path: phpmyadmin.RunDir, Mode: 0o755, Owner: "root", Group: "root"},
		{Path: phpmyadmin.TicketDir(), Mode: 0o700, Owner: u, Group: u},
		{Path: phpmyadmin.LogoutDir(), Mode: 0o700, Owner: u, Group: u},
	}
}

// pmaRunDirs are those of RunDir alone: /run is empty after a boot.
func pmaRunDirs() []agent.DirSpec { return pmaDirs("0.0.0")[6:] }

// jobPMAInstall installs phpMyAdmin, moves it to another port or branch, or
// brings it to the pinned release. A run over an install keeps the secret,
// the port and the branch unless asked otherwise; the tree of the release
// it replaces goes once the new one serves.
func (s *Server) jobPMAInstall(ctx context.Context, jc *jobs.Context) error {
	var p pmaPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	inst, err := s.dbInstance(ctx)
	if err != nil {
		return err
	}
	if strings.ContainsAny(inst.Socket, "'\\\n") || !path.IsAbs(inst.Socket) {
		return fmt.Errorf("the MySQL socket %q is no plain path", inst.Socket)
	}
	cur := s.pmaState(ctx)
	port := cur.Port
	if p.Port != 0 {
		port = p.Port
	}
	if port == 0 {
		port = phpmyadmin.DefaultPort
	}
	if err := s.pmaPortError(ctx, port); err != nil {
		return err
	}
	php, err := s.pmaPHP(ctx, p.PHPVersion, cur.PHP)
	if err != nil {
		return err
	}
	layout := phpLayout(s, php)
	if layout == nil {
		return fmt.Errorf("no PHP layout for PHP %s on this OS", php)
	}
	version := phpmyadmin.Version
	tree := phpmyadmin.Tree(version)

	jc.Progress(5, "system user "+phpmyadmin.User)
	if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: phpmyadmin.User, System: true}); err != nil {
		return fmt.Errorf("group %s: %w", phpmyadmin.User, err)
	}
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: phpmyadmin.User, System: true, PrimaryGroup: phpmyadmin.User, Comment: "MonoPanel phpMyAdmin"}); err != nil {
		return fmt.Errorf("user %s: %w", phpmyadmin.User, err)
	}

	jc.Progress(10, "downloading phpMyAdmin "+version)
	archive, err := s.fetchPMA(ctx, jc)
	if err != nil {
		return err
	}
	jc.Progress(35, "unpacking into "+tree)
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: pmaDirs(version)}); err != nil {
		return err
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "tar", Args: []string{
		"-xzf", archive, "-C", tree, "--strip-components=1", "--no-same-owner",
	}, TimeoutSeconds: 300})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar: %s", strings.TrimSpace(res.Output))
	}

	jc.Progress(55, "configuration")
	secret, err := s.pmaSecret(ctx)
	if err != nil {
		return err
	}
	data := s.pmaRender(version, port, secret, inst.Socket)
	conf, err := s.render.Render("phpmyadmin/config.inc.php.tmpl", data)
	if err != nil {
		return err
	}
	signon, err := s.render.Render("phpmyadmin/signon.php.tmpl", data)
	if err != nil {
		return err
	}
	if _, err := s.agent.PhpMyAdmin(ctx, &agent.PhpMyAdminRequest{Action: "files", Version: version, Config: conf, Signon: signon}); err != nil {
		return err
	}
	if err := s.pmaSELinux(ctx, jc, port); err != nil {
		return err
	}

	jc.Progress(70, "php-fpm pool (PHP "+php+")")
	pool, err := s.render.Render("phpmyadmin/pool.conf.tmpl", data)
	if err != nil {
		return err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:    []agent.FileSpec{{Path: path.Join(layout.PoolDir, phpmyadmin.Pool+".conf"), Content: pool, Mode: 0o644}},
		Validate: [][]string{layout.FPMCheckArgv}, Reload: []string{layout.FPMService}, Origin: "phpmyadmin",
	}); err != nil {
		return err
	}
	if cur.PHP != "" && cur.PHP != php {
		if old := phpLayout(s, cur.PHP); old != nil {
			rm := &agent.RemovePathsRequest{Paths: []string{path.Join(old.PoolDir, phpmyadmin.Pool+".conf")}}
			if s.checkPHPInstalled(ctx, cur.PHP) == nil {
				rm.Reload = []string{old.FPMService}
			}
			if _, err := s.agent.RemovePaths(ctx, rm); err != nil {
				jc.Logf("warning: the pool of PHP %s stays: %v", cur.PHP, err)
			}
		}
		jc.Logf("PHP %s → %s", cur.PHP, php)
	}

	jc.Progress(85, "nginx on port "+strconv.Itoa(port))
	// The settings first: the firewall and the status read them.
	for k, v := range map[string]string{settingPMAVersion: version, settingPMAPort: strconv.Itoa(port), settingPMAPHP: php} {
		if err := s.db.SetSetting(context.WithoutCancel(ctx), k, v); err != nil {
			return err
		}
	}
	if err := s.pmaApplyServer(ctx, jc.Logf, false); err != nil {
		return err
	}
	if enabled, err := s.db.GetSetting(ctx, settingFirewall); err == nil && enabled == "yes" {
		if _, err := s.applyFirewall(ctx); err != nil {
			jc.Logf("warning: firewall reload failed: %v", err)
		}
	}
	if pr, err := s.agent.PhpMyAdmin(ctx, &agent.PhpMyAdminRequest{Action: "prune", Version: version}); err != nil {
		jc.Logf("warning: earlier releases stay: %v", err)
	} else if len(pr.Removed) > 0 {
		jc.Logf("removed: %s", strings.Join(pr.Removed, ", "))
	}
	if cur.Version != "" && cur.Version != version {
		jc.Logf("phpMyAdmin %s → %s", cur.Version, version)
	}
	if cur.Port != 0 && cur.Port != port {
		jc.Logf("port %d → %d", cur.Port, port)
	}
	jc.Progress(100, "phpMyAdmin "+version+" at "+s.pmaURL(ctx, port))
	return nil
}

// pmaRender is what the templates need. config.inc.php names no address:
// phpMyAdmin takes the one it was reached by, and the panel's name may
// change (web.hostname) without a run of the install.
func (s *Server) pmaRender(version string, port int, secret, mysqlSocket string) render.PhpMyAdmin {
	return render.PhpMyAdmin{
		Port: port, ServerName: s.pmaServerName(), Docroot: phpmyadmin.Tree(version), Socket: s.pmaSocket(),
		Name: phpmyadmin.Pool, User: phpmyadmin.User, ListenGroup: s.cfg.WebGroup, Tree: phpmyadmin.Tree(version),
		StateDir: phpmyadmin.StateDir, RunDir: phpmyadmin.RunDir,
		DisableFunctions: render.DefaultDisableFunctions + ",exec",
		Secret:           secret, MySQLSocket: mysqlSocket,
		TicketDir: phpmyadmin.TicketDir(), LogoutDir: phpmyadmin.LogoutDir(),
	}
}

// pmaServerName is the server block's name: the panel's, or any.
func (s *Server) pmaServerName() string {
	if h := acme.NormalizeName(s.cfg.Web.Hostname); h != "" && pmaHostOK(h) {
		return h
	}
	return "_"
}

// pmaApplyServer writes the server block on the port with the panel's
// certificate as it is now; force reloads nginx even when the block did not
// change (a renewed certificate under the same name).
func (s *Server) pmaApplyServer(ctx context.Context, logf func(string, ...any), force bool) error {
	st := s.pmaState(ctx)
	if !st.installed() {
		return nil
	}
	certPath, keyPath, fp := s.pmaCert()
	data := render.PhpMyAdmin{Port: st.Port, ServerName: s.pmaServerName(), CertPath: certPath, KeyPath: keyPath,
		Docroot: phpmyadmin.Tree(st.Version), Socket: s.pmaSocket()}
	body, err := s.render.Render("phpmyadmin/nginx.conf.tmpl", data)
	if err != nil {
		return err
	}
	web := s.profile.Web()
	res, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:    []agent.FileSpec{{Path: s.pmaNginxConf(), Content: body, Mode: 0o644}},
		Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Force: force, Origin: "phpmyadmin",
	})
	if err != nil {
		return err
	}
	s.db.SetSetting(context.WithoutCancel(ctx), settingPMACert, fp) //nolint:errcheck // compared again at the next sweep
	if len(res.Written) > 0 || force {
		logf("phpMyAdmin at https://%s:%d/ (the panel's certificate)", s.pmaHost(ctx), st.Port)
	}
	return nil
}

// pmaSELinux labels the tree, the state and the run directory and lets
// nginx bind the port, on an enforcing host; firewalld opens the port where
// it runs. Done once per port.
func (s *Server) pmaSELinux(ctx context.Context, jc *jobs.Context, port int) error {
	if s.profile.MAC() == "selinux" {
		for _, fc := range [][2]string{
			{phpmyadmin.Root + "(/.*)?", "httpd_sys_content_t"},
			{phpmyadmin.StateDir + "(/.*)?", "httpd_sys_rw_content_t"},
			{phpmyadmin.RunDir + "(/.*)?", "httpd_var_run_t"},
		} {
			if err := s.selinuxFileContext(ctx, fc[0], fc[1]); err != nil {
				return err
			}
		}
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "restorecon", Args: []string{"-R", phpmyadmin.Root, phpmyadmin.StateDir, phpmyadmin.RunDir}, TimeoutSeconds: 300})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("restorecon: %s", strings.TrimSpace(res.Output))
		}
	}
	if v, _ := s.db.GetSetting(ctx, settingPMAReady); v != strconv.Itoa(port) {
		if err := s.selinuxHTTPPort(ctx, port); err != nil {
			return err
		}
		s.firewalldOpen(ctx, jc.Logf, fmt.Sprintf("%d/tcp", port))
		if err := s.db.SetSetting(ctx, settingPMAReady, strconv.Itoa(port)); err != nil {
			return err
		}
	}
	return nil
}

// jobPMARemove takes phpMyAdmin away: the temporary accounts, the server
// block, the pool, the tree, its state and its tickets. Its system user
// stays, as the package declares it (sysusers.d); its blowfish_secret goes.
func (s *Server) jobPMARemove(ctx context.Context, jc *jobs.Context) error {
	st := s.pmaState(ctx)
	// Not installed from here on: the button refuses, and a press already
	// under way ends before (pmaGate) — its account is on the list the
	// next step drops.
	s.pmaGate.Lock()
	err := s.db.DeleteSetting(context.WithoutCancel(ctx), settingPMAVersion)
	s.pmaGate.Unlock()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	jc.Progress(10, "temporary accounts")
	if err := s.pmaDropAll(ctx, jc.Logf); err != nil {
		return err
	}
	jc.Progress(30, "nginx")
	web := s.profile.Web()
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{s.pmaNginxConf()}, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}}); err != nil {
		return err
	}
	jc.Progress(50, "php-fpm pool")
	for _, v := range osprofileVersions() {
		l := phpLayout(s, v)
		if l == nil {
			continue
		}
		rm := &agent.RemovePathsRequest{Paths: []string{path.Join(l.PoolDir, phpmyadmin.Pool+".conf")}}
		if s.checkPHPInstalled(ctx, v) == nil {
			rm.Reload = []string{l.FPMService}
		}
		res, err := s.agent.RemovePaths(ctx, rm)
		if err != nil {
			return err
		}
		if len(res.Removed) > 0 {
			jc.Logf("pool removed: %s", strings.Join(res.Removed, ", "))
		}
	}
	jc.Progress(70, "files")
	res, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{phpmyadmin.Root, phpmyadmin.StateDir, phpmyadmin.RunDir}, Recursive: true})
	if err != nil {
		return err
	}
	if len(res.Removed) > 0 {
		jc.Logf("removed: %s", strings.Join(res.Removed, ", "))
	}
	for _, k := range []string{settingPMAVersion, settingPMAPort, settingPMAPHP, settingPMASecret, settingPMACert, settingPMATemp} {
		if err := s.db.DeleteSetting(context.WithoutCancel(ctx), k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if enabled, err := s.db.GetSetting(ctx, settingFirewall); err == nil && enabled == "yes" && st.Port > 0 {
		if _, err := s.applyFirewall(ctx); err != nil {
			jc.Logf("warning: firewall reload failed: %v", err)
		} else {
			jc.Logf("firewall: port %d closed", st.Port)
		}
	}
	jc.Progress(100, "phpMyAdmin removed")
	return nil
}

// pmaPortOpen is the port the firewall opens for phpMyAdmin; 0 when it is
// not installed.
func (s *Server) pmaPortOpen(ctx context.Context) int {
	if st := s.pmaState(ctx); st.installed() {
		return st.Port
	}
	return 0
}

func (s *Server) loadPMATemp(ctx context.Context) []pmaTemp {
	var list []pmaTemp
	if v, err := s.db.GetSetting(ctx, settingPMATemp); err == nil && v != "" {
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			s.log.Warn("phpmyadmin: the list of temporary accounts cannot be read; the sweep looks at MySQL's own list", "err", err)
		}
	}
	return list
}

func (s *Server) savePMATemp(ctx context.Context, list []pmaTemp) error {
	if len(list) == 0 {
		err := s.db.DeleteSetting(context.WithoutCancel(ctx), settingPMATemp)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.db.SetSetting(context.WithoutCancel(ctx), settingPMATemp, string(b))
}

// pmaDropSQL drops temporary accounts; only names of their shape go in.
func pmaDropSQL(users []string) string {
	var b strings.Builder
	for _, u := range users {
		if phpmyadmin.TempUserRe.MatchString(u) {
			fmt.Fprintf(&b, "DROP USER IF EXISTS '%s'@'localhost';\n", u)
		}
	}
	return b.String()
}

// pmaSignon is the panel's button: a temporary account on the database,
// recorded before MySQL makes it (a crash in between leaves a record the
// sweep drops, never an account it does not know), and a ticket for it.
// It holds pmaGate for reading: the removal, the sweep and the drop of a
// database's accounts wait for a press under way and see its account.
func (s *Server) pmaSignon(ctx context.Context, db *store.Database) (*apitypes.PhpMyAdminSignon, error) {
	s.pmaGate.RLock()
	defer s.pmaGate.RUnlock()
	st := s.pmaState(ctx)
	if !st.installed() {
		return nil, huma.Error409Conflict("phpMyAdmin is not installed (mp db phpmyadmin install)")
	}
	if !pmaDBRe.MatchString(db.Name) {
		return nil, huma.Error422UnprocessableEntity("database name " + db.Name + " cannot be granted")
	}
	// Deleted since the caller loaded it: its accounts were dropped, and
	// one made now would outlive it.
	if cur, err := s.db.GetDatabaseByName(ctx, db.Name); err != nil || cur.ID != db.ID {
		return nil, huma.Error404NotFound("database not found")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	user := phpmyadmin.TempUserPrefix + hex.EncodeToString(b)
	password, err := auth.NewPassword(24)
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tok)
	sum := sha256.Sum256([]byte(token))
	name := hex.EncodeToString(sum[:])
	now := time.Now()
	p := principalFrom(ctx)
	rec := pmaTemp{User: user, Database: db.Name, Actor: p.Login, Expires: now.Add(phpmyadmin.SessionTTL).UTC().Truncate(time.Second), Ticket: name}

	s.pmaMu.Lock()
	list := s.loadPMATemp(ctx)
	// The oldest of this actor's live accounts on the database beyond the
	// bound go with this press.
	var mine []pmaTemp
	for _, t := range list {
		if t.Actor == rec.Actor && t.Database == rec.Database && now.Before(t.Expires) {
			mine = append(mine, t)
		}
	}
	slices.SortFunc(mine, func(a, b pmaTemp) int { return a.Expires.Compare(b.Expires) })
	var old []string
	for _, t := range mine[:max(0, len(mine)-pmaPerDatabase+1)] {
		old = append(old, t.User)
	}
	err = s.savePMATemp(ctx, append(list, rec))
	s.pmaMu.Unlock()
	if err != nil {
		return nil, err
	}
	sql := pmaDropSQL(old) + fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s' PASSWORD EXPIRE NEVER;\n"+
		"GRANT ALL PRIVILEGES ON %s.* TO '%s'@'localhost';\n", user, sqlEscaper.Replace(password), grantDB(db.Name), user)
	if _, err := s.mysqlExec(ctx, sql); err != nil {
		s.pmaForget(context.WithoutCancel(ctx), user)
		return nil, huma.Error502BadGateway(err.Error())
	}
	if len(old) > 0 {
		s.pmaForget(context.WithoutCancel(ctx), old...)
	}
	ticket, err := json.Marshal(map[string]any{"hash": name, "user": user, "password": password, "db": db.Name, "expires": now.Add(phpmyadmin.TicketTTL).Unix()})
	if err != nil {
		return nil, err
	}
	// /run is empty after a boot: the directories come back first.
	_, err = s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: pmaRunDirs()})
	if err == nil {
		_, err = s.agent.PhpMyAdmin(ctx, &agent.PhpMyAdminRequest{Action: "ticket", Name: name, Content: string(ticket)})
	}
	if err != nil {
		s.mysqlExec(context.WithoutCancel(ctx), pmaDropSQL([]string{user})) //nolint:errcheck // the sweep drops it otherwise
		s.pmaForget(context.WithoutCancel(ctx), user)
		return nil, err
	}
	return &apitypes.PhpMyAdminSignon{
		URL:      fmt.Sprintf("https://%s:%d/%s?t=%s", s.pmaHost(ctx), st.Port, phpmyadmin.SignonScript, token),
		Database: db.Name, User: user, Expires: rec.Expires,
	}, nil
}

// pmaDropDatabases drops the temporary accounts on databases that are gone:
// DROP DATABASE leaves their grants in mysql.db, and a database of the same
// name made later — by another account, once a login is given again —
// would be open to them. The sweep drops what this misses.
func (s *Server) pmaDropDatabases(ctx context.Context, names ...string) {
	s.pmaGate.Lock()
	defer s.pmaGate.Unlock()
	var drop []string
	for _, t := range s.loadPMATemp(ctx) {
		if slices.Contains(names, t.Database) {
			drop = append(drop, t.User)
		}
	}
	if len(drop) == 0 {
		return
	}
	if _, err := s.mysqlExec(ctx, pmaDropSQL(drop)); err != nil {
		s.log.Warn("phpmyadmin: the temporary accounts of a deleted database stay until the sweep", "err", err, "accounts", strings.Join(drop, ", "))
		return
	}
	s.pmaForget(ctx, drop...)
	s.log.Info("phpmyadmin: temporary accounts of a deleted database dropped", "accounts", strings.Join(drop, ", "))
}

// pmaForget takes accounts off the list.
func (s *Server) pmaForget(ctx context.Context, users ...string) {
	s.pmaMu.Lock()
	defer s.pmaMu.Unlock()
	list := slices.DeleteFunc(s.loadPMATemp(ctx), func(t pmaTemp) bool { return slices.Contains(users, t.User) })
	if err := s.savePMATemp(ctx, list); err != nil {
		s.log.Warn("phpmyadmin: list of temporary accounts", "err", err)
	}
}

// pmaStrays are the temporary accounts MySQL has: the list may have lost one
// (a restored panel database, a list that could not be read).
func (s *Server) pmaStrays(ctx context.Context) ([]string, error) {
	out, err := s.mysqlExec(ctx, "SELECT user FROM mysql.user WHERE user LIKE '"+phpmyadmin.TempUserPrefix+"%' AND host = 'localhost';")
	if err != nil {
		return nil, err
	}
	var users []string
	for _, u := range strings.Fields(out) {
		if phpmyadmin.TempUserRe.MatchString(u) {
			users = append(users, u)
		}
	}
	return users, nil
}

// pmaSweep drops the temporary accounts that expired, whose session signed
// out, whose ticket nobody took, or whose database is gone, and those MySQL
// has that the list does not know — all of them once phpMyAdmin is not
// installed (a press that raced a removal); the agent clears the tickets
// nobody took. It runs at the API's start, every ten minutes after it.
func (s *Server) pmaSweep(ctx context.Context) {
	s.pmaGate.Lock()
	defer s.pmaGate.Unlock()
	st := s.pmaState(ctx)
	list := s.loadPMATemp(ctx)
	var logouts, removed []string
	if st.installed() {
		res, err := s.agent.PhpMyAdmin(ctx, &agent.PhpMyAdminRequest{Action: "sweep"})
		if err != nil {
			s.log.Warn("phpmyadmin: sweep of the tickets", "err", err)
		} else {
			logouts, removed = res.Logouts, res.Removed
		}
	}
	now := time.Now()
	known := map[string]bool{}
	var drop []string
	for _, t := range list {
		known[t.User] = true
		gone := false
		if _, err := s.db.GetDatabaseByName(ctx, t.Database); errors.Is(err, store.ErrNotFound) {
			gone = true
		}
		unused := t.Ticket != "" && slices.Contains(removed, path.Join(phpmyadmin.TicketDir(), t.Ticket))
		if !st.installed() || !now.Before(t.Expires) || slices.Contains(logouts, t.User) || unused || gone {
			drop = append(drop, t.User)
		}
	}
	if _, err := s.dbInstance(ctx); err == nil {
		if strays, err := s.pmaStrays(ctx); err == nil {
			for _, u := range strays {
				if !known[u] {
					drop = append(drop, u)
				}
			}
		}
	}
	if len(drop) > 0 {
		if _, err := s.mysqlExec(ctx, pmaDropSQL(drop)); err != nil {
			s.log.Warn("phpmyadmin: dropping temporary accounts", "err", err, "accounts", len(drop))
			return
		}
		s.pmaForget(ctx, drop...)
		s.log.Info("phpmyadmin: temporary accounts dropped", "accounts", strings.Join(drop, ", "))
	}
	s.pmaCertCheck(ctx)
}

// pmaCertCheck writes the server block again, with a reload, once the
// panel's certificate changed (a renewal, an issued one in place of the
// self-signed): nginx keeps the one it loaded.
func (s *Server) pmaCertCheck(ctx context.Context) {
	if !s.pmaState(ctx).installed() {
		return
	}
	_, _, fp := s.pmaCert()
	if was, _ := s.db.GetSetting(ctx, settingPMACert); was == fp {
		return
	}
	if err := s.pmaApplyServer(ctx, func(string, ...any) {}, true); err != nil {
		s.log.Warn("phpmyadmin: the server block with the panel's new certificate", "err", err)
	}
}

// pmaDropAll drops every temporary account, known or not (the removal).
func (s *Server) pmaDropAll(ctx context.Context, logf func(string, ...any)) error {
	var users []string
	for _, t := range s.loadPMATemp(ctx) {
		users = append(users, t.User)
	}
	if _, err := s.dbInstance(ctx); err == nil {
		strays, err := s.pmaStrays(ctx)
		if err != nil {
			return err
		}
		for _, u := range strays {
			if !slices.Contains(users, u) {
				users = append(users, u)
			}
		}
		if sql := pmaDropSQL(users); sql != "" {
			if _, err := s.mysqlExec(ctx, sql); err != nil {
				return err
			}
			logf("temporary accounts dropped: %s", strings.Join(users, ", "))
		}
	}
	s.pmaForget(ctx, users...)
	return nil
}

// refreshPHPMyAdmin runs at the API's start: the sweep, and a panel that
// pins a newer release than the installed one queues the move to it once
// (the install job keeps the port, the branch and the secret).
func (s *Server) refreshPHPMyAdmin(ctx context.Context) {
	s.pmaSweep(ctx)
	st := s.pmaState(ctx)
	if !st.installed() || st.Version == phpmyadmin.Version {
		return
	}
	if _, err := s.jobs.Enqueue(ctx, "phpmyadmin.install", pmaPayload{Upgrade: true}, jobs.WithLockKey("phpmyadmin"), jobs.WithRequestedBy("system")); err != nil {
		s.log.Warn("phpmyadmin: the move to the pinned release", "err", err)
		return
	}
	s.log.Info("phpmyadmin: moving to the pinned release", "from", st.Version, "to", phpmyadmin.Version)
}

// pmaCheck is the doctor's line.
func (s *Server) pmaCheck(ctx context.Context) apitypes.Check {
	st := s.pmaState(ctx)
	if !st.installed() {
		return check("phpmyadmin", "ok", "not installed (mp db phpmyadmin install)")
	}
	url := s.pmaURL(ctx, st.Port)
	var problems []string
	if l := phpLayout(s, st.PHP); l == nil {
		problems = append(problems, "no PHP "+st.PHP+" layout")
	} else if svc, err := s.agent.Service(ctx, l.FPMService, "status"); err != nil || svc.Status.ActiveState != "active" {
		state := "unknown"
		if err == nil {
			state = svc.Status.ActiveState
		}
		problems = append(problems, fmt.Sprintf("%s is %s", l.FPMService, state))
	}
	if res, err := s.agent.Stat(ctx, s.pmaSocket(), s.pmaNginxConf()); err == nil && len(res.Entries) == 2 {
		if !res.Entries[0].Exists {
			problems = append(problems, "its pool's socket "+s.pmaSocket()+" is missing")
		}
		if !res.Entries[1].Exists {
			problems = append(problems, "its nginx server block is missing (mp db phpmyadmin install)")
		}
	}
	if probe := s.probePorts([]int{st.Port}); !probe[st.Port].Open {
		problems = append(problems, fmt.Sprintf("nothing answers on port %d", st.Port))
	}
	if len(problems) > 0 {
		return check("phpmyadmin", "fail", strings.Join(problems, "; ")+" — "+url)
	}
	detail := fmt.Sprintf("%s %s on PHP %s", url, st.Version, st.PHP)
	if st.Version != phpmyadmin.Version {
		return check("phpmyadmin", "warn", detail+fmt.Sprintf("; this panel pins %s (mp db phpmyadmin install)", phpmyadmin.Version))
	}
	return check("phpmyadmin", "ok", detail)
}

func (s *Server) pmaStatus(ctx context.Context) apitypes.PhpMyAdminStatus {
	st := s.pmaState(ctx)
	out := apitypes.PhpMyAdminStatus{Installed: st.installed(), Pinned: phpmyadmin.Version}
	if !out.Installed {
		return out
	}
	out.Version, out.Port, out.PHPVersion, out.URL = st.Version, st.Port, st.PHP, s.pmaURL(ctx, st.Port)
	if p := principalFrom(ctx); p != nil && p.Role == store.RoleAdmin {
		now := time.Now()
		for _, t := range s.loadPMATemp(ctx) {
			if now.Before(t.Expires) {
				out.Sessions++
			}
		}
	}
	return out
}

func (s *Server) registerPHPMyAdmin() {
	huma.Register(s.api, huma.Operation{
		OperationID: "phpmyadmin-status", Method: http.MethodGet, Path: "/db/phpmyadmin", Summary: "phpMyAdmin: installed or not, its release, port and address", Tags: []string{"db"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*pmaStatusOutput, error) {
		return &pmaStatusOutput{Body: s.pmaStatus(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "phpmyadmin-install", Method: http.MethodPost, Path: "/db/phpmyadmin", Summary: "Install phpMyAdmin on its own port, or move it to another port or PHP branch (async)", Tags: []string{"db"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *pmaInstallInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		if _, err := s.dbInstance(ctx); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error() + ": phpMyAdmin needs the database server (mp stack install percona)")
		}
		if in.Body.Port != 0 {
			if err := s.pmaPortError(ctx, in.Body.Port); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if in.Body.PHPVersion != "" {
			if _, err := s.pmaPHP(ctx, in.Body.PHPVersion, ""); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		job, err := s.jobs.Enqueue(ctx, "phpmyadmin.install", pmaPayload{Port: in.Body.Port, PHPVersion: in.Body.PHPVersion}, jobs.WithLockKey("phpmyadmin"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "phpmyadmin.install", Target: phpmyadmin.Version, IP: requestInfo(ctx).IP, Details: map[string]any{"port": in.Body.Port, "php": in.Body.PHPVersion}})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "phpmyadmin-remove", Method: http.MethodDelete, Path: "/db/phpmyadmin", Summary: "Remove phpMyAdmin with its temporary accounts, pool and port (async)", Tags: []string{"db"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, _ *struct{}) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		job, err := s.jobs.Enqueue(ctx, "phpmyadmin.remove", struct{}{}, jobs.WithLockKey("phpmyadmin"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "phpmyadmin.remove", Target: "phpmyadmin", IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "databases-phpmyadmin", Method: http.MethodPost, Path: "/databases/{name}/phpmyadmin", Summary: "Open the database in phpMyAdmin: a one-time address that signs in at once, as a temporary account granted on this database only", Tags: []string{"db"},
		Security: secured,
	}, func(ctx context.Context, in *dbNameInput) (*pmaSignonOutput, error) {
		p := principalFrom(ctx)
		db, err := s.loadDatabaseFor(ctx, in.Name)
		if err != nil {
			return nil, err
		}
		out, err := s.pmaSignon(ctx, db)
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "db.phpmyadmin", Target: db.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"account": out.User, "until": out.Expires}})
		return &pmaSignonOutput{Body: *out}, nil
	})
}
