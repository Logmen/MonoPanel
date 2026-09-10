package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/config"
	"monopanel/internal/store"
)

type (
	agentReq = agent.ToolRequest
	agentRes = agent.ToolResponse
)

// withOSRelease makes the fixture believe it runs on another OS for one test.
func withOSRelease(t *testing.T, osRelease string) {
	t.Helper()
	prev := fixtureOSRelease
	fixtureOSRelease = osRelease
	t.Cleanup(func() { fixtureOSRelease = prev })
}

// A fresh Ubuntu has no ppa:ondrej/php builds for months. The panel must not
// write a source apt cannot read; it stays with Ubuntu's own packages, says
// which branches exist there, and explains a branch that does not.
func TestPHPUbuntuWithoutPPAStaysWithDistroPackages(t *testing.T) {
	withOSRelease(t, "ID=ubuntu\nVERSION_ID=26.04\nVERSION_CODENAME=resolute\n")
	prev := ppaHasRelease
	ppaHasRelease = func(context.Context, string) (bool, error) { return false, nil }
	t.Cleanup(func() { ppaHasRelease = prev })
	f := newSiteFixture(t)
	f.agent.MissingPackages = map[string]bool{"php8.3-fpm": true}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "ppa:ondrej/php has no packages for Ubuntu 26.04 (resolute)") {
		t.Fatalf("install of a branch Ubuntu lacks: status=%s error=%q", job.Status, job.Error)
	}
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "ondrej-php.list") {
			t.Fatal("the PPA source was written although the PPA has no release")
		}
	}

	// The matrix now reflects apt: 8.3 is out with the reason, 8.5 is in.
	var list struct {
		Available []struct {
			Version   string
			Available bool
			Note      string
		}
	}
	f.call(http.MethodGet, "/php/versions", nil, http.StatusOK, &list)
	seen := map[string]bool{}
	for _, v := range list.Available {
		seen[v.Version] = true
		switch v.Version {
		case "8.3":
			if v.Available || !strings.Contains(v.Note, "resolute") {
				t.Errorf("8.3 must be unavailable with the reason: %+v", v)
			}
		case "8.5":
			if !v.Available {
				t.Errorf("8.5 exists in Ubuntu and must stay available: %+v", v)
			}
		}
	}
	if !seen["8.3"] || !seen["8.5"] {
		t.Fatalf("matrix incomplete: %+v", list.Available)
	}
	// And a second request for the missing branch is refused before any job.
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusUnprocessableEntity, nil)

	// When the PPA appears the next installation picks it up.
	ppaHasRelease = func(context.Context, string) (bool, error) { return true, nil }
	f.agent.MissingPackages = nil
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("install with the PPA present: %s %s", job.Status, job.Error)
	}
	written := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "ondrej-php.list") {
			written = true
		}
	}
	if !written {
		t.Fatal("the PPA source was not written once the PPA had the release")
	}
}

// On an EL host nginx and php-fpm run confined: installing nginx prepares the
// file contexts and booleans of a hosting server, and the pid file created by
// `nginx -t` gets relabelled before nginx is started.
func TestNginxInstallOnELPreparesSELinux(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "nginx"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("nginx install on EL: %s %s", job.Status, job.Error)
	}
	var fcontext, booleans, relabel bool
	for _, tool := range f.agent.Tools() {
		args := strings.Join(tool.Args, " ")
		switch tool.Name {
		case "semanage":
			if strings.Contains(args, "httpd_var_run_t") && strings.Contains(args, "/run/monopanel") {
				fcontext = true
			}
		case "setsebool":
			if strings.Contains(args, "-P") && strings.Contains(args, "httpd_unified=1") {
				booleans = true
			}
		case "restorecon":
			if strings.Contains(args, "/var/www") {
				relabel = true
			}
		}
	}
	if !fcontext || !booleans || !relabel {
		t.Fatalf("selinux policy incomplete: fcontext=%v booleans=%v relabel=%v (%+v)", fcontext, booleans, relabel, f.agent.Tools())
	}
	restore := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), `"restore":["/run/nginx.pid"]`) {
			restore = true
		}
	}
	if !restore {
		t.Fatal("nginx configuration was applied without relabelling the pid file")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingSELinux); v != "ready" {
		t.Fatalf("policy not remembered: %q", v)
	}
	// Once is enough: a PHP installation later does not repeat it.
	before := len(f.agent.Tools())
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("php install on EL: %s %s", job.Status, job.Error)
	}
	for _, tool := range f.agent.Tools()[before:] {
		if tool.Name == "semanage" || tool.Name == "setsebool" {
			t.Fatalf("selinux policy applied twice: %+v", tool)
		}
	}
}

// The EL packages start MySQL with a temporary root password written to the
// error log, not with auth_socket like Debian's. The installer must read it,
// switch root to the socket and carry on.
func TestPerconaInstallOnELTakesTheTemporaryRootPassword(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	f.agent.ReadFile["/var/log/mysqld.log"] = "2026-09-09T15:53:48Z 6 [Note] [MY-010454] [Server] A temporary password is generated for root@localhost: Tmp!Pass9\n"
	// The server the hook plays: the temporary password is expired (only
	// ALTER USER passes), the plugin is not loaded, the socket works only
	// after the switch.
	var reset, switched bool
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name != "mysql" {
			return nil
		}
		password := ""
		for _, e := range req.Env {
			password = strings.TrimPrefix(e, "MYSQL_PWD=")
		}
		switch {
		case password == "Tmp!Pass9" && strings.Contains(req.Stdin, "IDENTIFIED BY"):
			reset = true
			return &agentRes{ExitCode: 0}
		case password == "Tmp!Pass9":
			return &agentRes{ExitCode: 1, Output: "ERROR 1820 (HY000): You must reset your password using ALTER USER statement before executing this statement."}
		case password != "" && !reset:
			return &agentRes{ExitCode: 1, Output: "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)"}
		case password != "" && strings.Contains(req.Stdin, "IDENTIFIED WITH auth_socket"):
			switched = true
			return &agentRes{ExitCode: 0}
		case password != "":
			return &agentRes{ExitCode: 0}
		case !switched:
			return &agentRes{ExitCode: 1, Output: "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)"}
		}
		return &agentRes{ExitCode: 0, Output: "8.4.11-11\n"}
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "percona"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("percona install on EL: %s %s", job.Status, job.Error)
	}
	if !switched {
		t.Fatal("root was never switched to auth_socket with the temporary password")
	}
	for _, tool := range f.agent.Tools() {
		if tool.Name == "mysql" && strings.Contains(strings.Join(tool.Args, " "), "Tmp!Pass9") {
			t.Fatal("the temporary password must not appear on the command line")
		}
	}
}

// On Oracle Linux the PHP repository step must ask for Oracle's EPEL package,
// not for epel-release, which no Oracle repository carries.
func TestPHPInstallOnOracleLinuxUsesOracleEPEL(t *testing.T) {
	withOSRelease(t, "ID=ol\nVERSION_ID=9.8\nID_LIKE=fedora\n")
	f := newSiteFixture(t)
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("php install on OL: %s %s", job.Status, job.Error)
	}
	epel := false
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/pkg" {
			continue
		}
		if strings.Contains(string(c.Body), `"epel-release"`) {
			t.Fatalf("epel-release requested on Oracle Linux: %s", c.Body)
		}
		if strings.Contains(string(c.Body), "oracle-epel-release-el9") {
			epel = true
		}
	}
	if !epel {
		t.Fatal("oracle-epel-release-el9 was not installed")
	}
}

// An image that ships firewalld enabled (Oracle Linux) allows only ssh:
// installing nginx must open 80 and 443 there, and hosts without firewalld
// must not be touched.
func TestNginxInstallOpensFirewalld(t *testing.T) {
	withOSRelease(t, "ID=ol\nVERSION_ID=9.8\nID_LIKE=fedora\n")
	f := newSiteFixture(t)
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "firewall-cmd" && len(req.Args) == 1 && req.Args[0] == "--state" {
			return &agentRes{ExitCode: 0, Output: "running\n"}
		}
		return nil
	}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "nginx"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("nginx install: %s %s", job.Status, job.Error)
	}
	opened, reloaded := false, false
	for _, tool := range f.agent.Tools() {
		if tool.Name != "firewall-cmd" {
			continue
		}
		args := strings.Join(tool.Args, " ")
		if strings.Contains(args, "--permanent") && strings.Contains(args, "--add-port=80/tcp") && strings.Contains(args, "--add-port=443/tcp") {
			opened = true
		}
		if strings.Contains(args, "--reload") {
			reloaded = true
		}
	}
	if !opened || !reloaded {
		t.Fatalf("firewalld not opened for nginx: opened=%v reloaded=%v", opened, reloaded)
	}

	// Without firewalld (the default fake answers "" to --state) nothing is added.
	g := newSiteFixture(t)
	g.call(http.MethodPost, "/stack/install", map[string]any{"component": "nginx"}, http.StatusAccepted, &ref)
	g.waitJob(ref.JobID)
	for _, tool := range g.agent.Tools() {
		if tool.Name == "firewall-cmd" && strings.Contains(strings.Join(tool.Args, " "), "--add-port") {
			t.Fatalf("firewalld touched although it is not running: %v", tool.Args)
		}
	}
}

// http://<panel hostname>/ must lead to the panel: the default server
// redirects that one name to the panel port and keeps 444 for the rest.
func TestNginxDefaultServerRedirectsPanelHostname(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "panel.example.com"; c.Web.Listen = ":8443" })
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "nginx"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("nginx install: %s %s", job.Status, job.Error)
	}
	found := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "ip-") && strings.Contains(string(c.Body), `if ($host = \"panel.example.com\") { return 301 https://$host:8443$request_uri; }`) && strings.Contains(string(c.Body), "return 444;") {
			found = true
		}
	}
	if !found {
		t.Fatal("default server does not redirect the panel hostname to the panel port")
	}
}

// mp db engine tune re-renders the panel's MySQL configuration with the
// current defaults (innodb_strict_mode off among them) and restarts the server.
func TestDBEngineTuneRewritesConfig(t *testing.T) {
	f := newSiteFixture(t)
	if status, _ := f.do(http.MethodPost, "/db/engine/tune", nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("tune without an engine: %d", status)
	}
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	var st struct {
		Installed bool
		Instance  struct{ Engine string }
	}
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, &st)
	if !st.Installed || st.Instance.Engine != "percona" {
		t.Fatalf("tune: %+v", st)
	}
	written := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "zz-monopanel.cnf") && strings.Contains(string(c.Body), "innodb_strict_mode = OFF") {
			written = true
		}
	}
	if !written {
		t.Fatal("zz-monopanel.cnf with innodb_strict_mode = OFF was not written")
	}
	if act := f.agent.UnitAction("mysql.service"); act != "restart" {
		t.Fatalf("mysql must be restarted after tune (a reload does not reread the config): %q", act)
	}
}

// Percona's my.cnf on EL includes nothing, so the panel's settings would be
// ignored there: the config writer adds /etc/mysql/my.cnf, which mysqld reads
// by default, with an !includedir for the panel's directory.
func TestDBConfigOnELPerconaIsIncluded(t *testing.T) {
	withOSRelease(t, "ID=ol\nVERSION_ID=9.8\nID_LIKE=fedora\n")
	f := newSiteFixture(t)
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/lib/mysql/mysql.sock", Service: "mysqld.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	include := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), `"/etc/mysql/my.cnf"`) && strings.Contains(string(c.Body), "!includedir /etc/my.cnf.d/") && strings.Contains(string(c.Body), "/etc/my.cnf.d/zz-monopanel.cnf") {
			include = true
		}
	}
	if !include {
		t.Fatal("no /etc/mysql/my.cnf with !includedir /etc/my.cnf.d/ was written for Percona on EL")
	}
	logdir := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/dirs/ensure" && strings.Contains(string(c.Body), `"/var/log/mysql"`) && strings.Contains(string(c.Body), `"mysql"`) {
			logdir = true
		}
	}
	if !logdir {
		t.Fatal("/var/log/mysql for the slow log was not created for mysql on EL")
	}
}
