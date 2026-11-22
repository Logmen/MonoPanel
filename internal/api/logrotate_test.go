package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// logrotateApplies returns the positions, among all agent calls, of the
// configuration sets that wrote the site log rotation, and that rotation's
// validators.
func (f *siteFixture) logrotateApplies() (at []int, validate [][]string) {
	for i, c := range f.agent.Calls() {
		if c.Path != "/v1/config/apply" {
			continue
		}
		var req agent.ApplyConfigSetRequest
		if json.Unmarshal(c.Body, &req) != nil {
			continue
		}
		for _, file := range req.Files {
			if file.Path == siteLogrotatePath {
				at, validate = append(at, i), req.Validate
			}
		}
	}
	return at, validate
}

// siteLogsCall is the position of the agent call that prepared these logs of
// an account, or -1.
func (f *siteFixture) siteLogsCall(login string, names ...string) int {
	return slices.IndexFunc(f.agent.Calls(), func(c agenttest.Call) bool {
		var req agent.SiteLogsRequest
		if c.Path != "/v1/sitelogs/ensure" || json.Unmarshal(c.Body, &req) != nil || req.Login != login {
			return false
		}
		for _, n := range names {
			if !slices.Contains(req.Files, n) {
				return false
			}
		}
		return true
	})
}

// Site logs live in a root-owned tree outside the accounts' homes, where
// nothing else rotates them: every account gets its stanzas, rotating as root
// — no su, the directory is root's — with the servers' logs made root:<login>
// 0640 and the PHP error log <login>:<login> 0660, checked by logrotate before
// they go live, and the account leaves before its unix user does.
func TestSiteLogsRotateInTheRootTree(t *testing.T) {
	f := newSiteFixture(t)
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "bob"}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("provision: %s %s", job.Status, job.Error)
	}
	conf, ok := f.agent.File(siteLogrotatePath)
	if !ok {
		t.Fatal("site logs got no rotation")
	}
	dir := "/var/log/monopanel/sites/bob"
	for _, want := range []string{
		dir + "/*.access.log " + dir + "/*[!p].error.log " + dir + "/*[!h]p.error.log " + dir + "/*[!p]hp.error.log " + dir + "/*[!.]php.error.log " + dir + "/*.php.slow.log {\n",
		"\tcreate 0640 root bob\n", "\n" + dir + "/*.php.error.log {\n", "\tcreate 0660 bob bob\n",
		"\tcreate 0640 root alex\n", "\tcreate 0660 alex alex\n",
		`kill -USR1 "$(cat /run/nginx.pid)"`,
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("rotation lacks %q:\n%s", want, conf)
		}
	}
	// No Apache here: nothing of it to reopen.
	if strings.Contains(conf, "apache") {
		t.Fatalf("a host without Apache reopens one:\n%s", conf)
	}
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "su ") {
			t.Fatalf("the tree is root's, nothing rotates under an account: %q", line)
		}
		if strings.Contains(line, "data/logs") && !strings.HasPrefix(line, "#") {
			t.Fatalf("the rotation still names the accounts' homes: %q", line)
		}
	}
	// Every *.error.log of the first stanza but the PHP one: logrotate
	// refuses a file that two stanzas name.
	if strings.Contains(conf, dir+"/*.error.log") {
		t.Fatalf("*.error.log would catch the PHP error logs of the second stanza:\n%s", conf)
	}
	if _, validate := f.logrotateApplies(); !slices.ContainsFunc(validate, func(argv []string) bool {
		return slices.Equal(argv, []string{"/usr/sbin/logrotate", "--debug", siteLogrotatePath})
	}) {
		t.Fatalf("the rotation must be checked by logrotate before it goes live: %v", validate)
	}

	// The site's logs are made in the tree before nginx and php-fpm open
	// them, and the home gets no data/logs any more.
	f.createSite(map[string]any{"domain": "bob.example.com", "user": "bob", "php_version": "8.4", "ssl": "none"})
	names := []string{"bob.example.com.access.log", "bob.example.com.error.log", "bob.example.com.php.error.log", "bob.example.com.php.slow.log"}
	if got := f.agent.SiteLogs("bob"); !slices.Equal(got, names) {
		t.Fatalf("logs prepared for bob: %v", got)
	}
	ensured := f.siteLogsCall("bob", names...)
	applied := slices.IndexFunc(f.agent.Calls(), func(c agenttest.Call) bool {
		return c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "/etc/nginx/monopanel/sites/bob.example.com.conf")
	})
	if ensured < 0 || applied < 0 || ensured > applied {
		t.Fatalf("the logs must exist before the configuration is applied: logs at %d, configuration at %d", ensured, applied)
	}
	for _, c := range f.agent.Calls() {
		if body := string(c.Body); strings.Contains(body, "data/logs") {
			t.Fatalf("%s still touches data/logs: %s", c.Path, body)
		}
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/users/bob", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("delete: %s %s", job.Status, job.Error)
	}
	if conf, _ := f.agent.File(siteLogrotatePath); strings.Contains(conf, "bob") || !strings.Contains(conf, "create 0660 alex alex") {
		t.Fatalf("bob must leave the rotation, alex stay:\n%s", conf)
	}
	at, _ := f.logrotateApplies()
	removed := slices.IndexFunc(f.agent.Calls(), func(c agenttest.Call) bool { return c.Path == "/v1/user/remove" })
	if removed < 0 || at[len(at)-1] > removed {
		t.Fatalf("the rotation must drop bob before his unix user goes: rotation at %v, user removed at %d", at, removed)
	}
	// Without purge the logs stay, like the home.
	if gone := f.agent.SiteLogsRemoved(); len(gone) != 0 {
		t.Fatalf("logs removed without purge: %v", gone)
	}
}

// A purged account takes its directory of site logs along: it is not in the
// home, and a later account by the same name must not find it.
func TestPurgedAccountTakesItsSiteLogs(t *testing.T) {
	f := newSiteFixture(t)
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol"}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("provision: %s %s", job.Status, job.Error)
	}
	f.createSite(map[string]any{"domain": "carol.example.com", "user": "carol", "php_version": "8.4", "ssl": "none"})
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/users/carol?purge=true", nil, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobDone {
		t.Fatalf("delete: %s %s", job.Status, job.Error)
	}
	if gone := f.agent.SiteLogsRemoved(); !slices.Equal(gone, []string{"carol"}) {
		t.Fatalf("logs removed: %v", gone)
	}
	if !strings.Contains(job.Log, "site logs removed: /var/log/monopanel/sites/carol") {
		t.Fatalf("job log:\n%s", job.Log)
	}
}

// The nginx, Apache and php-fpm configuration of a site sends every log to
// the account's directory of the root-owned tree, never into its home.
func TestSiteConfigsLogOutsideTheHome(t *testing.T) {
	f := newSiteFixture(t)
	if err := f.db.SetSetting(f.ctx, settingApache, "installed"); err != nil {
		t.Fatal(err)
	}
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	dir := "/var/log/monopanel/sites/alex/"
	for file, wants := range map[string][]string{
		"/etc/nginx/monopanel/sites/example.com.conf":                 {"access_log " + dir + "example.com.access.log main;", "error_log  " + dir + "example.com.error.log;"},
		"/etc/php/8.4/fpm/pool.d/example.com.conf":                    {"slowlog = " + dir + "example.com.php.slow.log", "php_admin_value[error_log]         = " + dir + "example.com.php.error.log"},
		"/etc/apache2/monopanel/accounts/alex/sites/example.com.conf": {"ErrorLog  " + dir + "example.com.apache.error.log", "CustomLog " + dir + "example.com.apache.access.log combined"},
		"/etc/apache2/monopanel/accounts/alex/httpd.conf":             {"ErrorLog " + dir + "apache.error.log\n"},
	} {
		conf, ok := f.agent.File(file)
		if !ok {
			t.Fatalf("%s not written; files: %v", file, f.agent.Files())
		}
		for _, want := range wants {
			if !strings.Contains(conf, want) {
				t.Errorf("%s lacks %q:\n%s", file, want, conf)
			}
		}
		if strings.Contains(conf, "/data/logs") {
			t.Errorf("%s still logs into the home:\n%s", file, conf)
		}
	}
	if got := f.agent.SiteLogs("alex"); !slices.Equal(got, []string{
		"apache.error.log", "example.com.access.log", "example.com.apache.access.log", "example.com.apache.error.log",
		"example.com.error.log", "example.com.php.error.log", "example.com.php.slow.log",
	}) {
		t.Fatalf("logs prepared: %v", got)
	}
}

// The site's Logs tab reads the tree, through the agent.
func TestSiteLogsEndpointReadsTheTree(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.agent.ReadFile["/var/log/monopanel/sites/alex/example.com.php.error.log"] = "PHP Warning: one\nPHP Warning: two\n"
	var tail apitypes.LogTail
	f.call(http.MethodGet, "/sites/example.com/logs/php?lines=1", nil, http.StatusOK, &tail)
	if tail.Path != "/var/log/monopanel/sites/alex/example.com.php.error.log" || !slices.Equal(tail.Lines, []string{"PHP Warning: two"}) {
		t.Fatalf("tail: %+v", tail)
	}
}

// An upgraded panel switches every site to the tree once: the directories
// and logs of each account with sites, a site.apply for every site (whose
// log says the old files stay), the webmail on its port re-rendered. The
// next start does nothing.
func TestMoveSiteLogsOnce(t *testing.T) {
	f := newMailFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "app.example.com", "user": "alex", "mode": "proxy", "backend": "http://127.0.0.1:3000", "ssl": "none"})
	c := f.s.loadMailConfig(f.ctx)
	c.Webmail, c.WebmailPort = "example.com", 2096
	if err := f.s.saveMailConfig(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	f.settle()
	// What a panel up to 0.8.19 left: no record of the move.
	if v, _ := f.db.GetSetting(f.ctx, settingSiteLogsMoved); v != "" {
		t.Fatalf("the move ran before the start: %q", v)
	}
	f.agent.Reset()

	f.s.moveSiteLogs(f.ctx)
	if got := f.agent.SiteLogs("alex"); !slices.Equal(got, []string{
		"app.example.com.access.log", "app.example.com.error.log",
		"example.com.access.log", "example.com.error.log", "example.com.php.error.log", "example.com.php.slow.log",
		"webmail.access.log", "webmail.error.log",
	}) {
		t.Fatalf("logs prepared for alex: %v", got)
	}
	moved := func() (sites map[int64]bool, mail int) {
		t.Helper()
		jobs, err := f.db.ListJobs(f.ctx, 500, "")
		if err != nil {
			t.Fatal(err)
		}
		sites = map[int64]bool{}
		for _, j := range jobs {
			var p sitePayload
			switch {
			case j.Type == "site.apply" && json.Unmarshal(j.Payload, &p) == nil && p.LogsMoved:
				if sites[p.SiteID] {
					t.Fatalf("site %d queued twice", p.SiteID)
				}
				sites[p.SiteID] = true
			case j.Type == "mail.apply" && j.RequestedBy == "system":
				mail++
			}
		}
		return sites, mail
	}
	sites, mail := moved()
	all, _ := f.db.ListSites(f.ctx, 0)
	if len(sites) != len(all) || len(all) != 2 || mail != 1 {
		t.Fatalf("queued: sites %v of %d, mail.apply %d", sites, len(all), mail)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingSiteLogsMoved); v != "/var/log/monopanel/sites" {
		t.Fatalf("the move is not remembered: %q", v)
	}
	f.settle()
	site, _ := f.db.GetSiteByDomain(f.ctx, "example.com")
	jobs, _ := f.db.ListJobs(f.ctx, 500, "")
	for _, j := range jobs {
		var p sitePayload
		if j.Type != "site.apply" || json.Unmarshal(j.Payload, &p) != nil || !p.LogsMoved || p.SiteID != site.ID {
			continue
		}
		j = f.waitJob(j.ID) // the list leaves the logs out
		if j.Status != store.JobDone || !strings.Contains(j.Log, "/var/www/alex/data/logs") || !strings.Contains(j.Log, "the old files stay there") {
			t.Fatalf("site.apply after the move: %s %s\n%s", j.Status, j.Error, j.Log)
		}
	}
	if conf, _ := f.agent.File("/etc/nginx/monopanel/sites/example.com.conf"); !strings.Contains(conf, "access_log /var/log/monopanel/sites/alex/example.com.access.log main;") {
		t.Fatalf("the site was not rendered anew:\n%s", conf)
	}

	// The next start: nothing.
	f.agent.Reset()
	f.s.moveSiteLogs(f.ctx)
	if again, mail2 := moved(); len(again) != 2 || mail2 != 1 || f.agent.Called("/v1/sitelogs/ensure") {
		t.Fatalf("the move ran twice: sites %v, mail.apply %d, agent %v", again, mail2, f.agent.Calls())
	}
}

// A fresh panel has no sites to move and remembers so: its sites get the
// tree from the start.
func TestMoveSiteLogsOnAFreshPanel(t *testing.T) {
	f := newSiteFixture(t)
	f.s.moveSiteLogs(f.ctx)
	if v, _ := f.db.GetSetting(f.ctx, settingSiteLogsMoved); v != "/var/log/monopanel/sites" {
		t.Fatalf("setting: %q", v)
	}
	if jobs, _ := f.db.ListJobs(f.ctx, 500, ""); len(jobs) != 0 {
		t.Fatalf("jobs on a panel without sites: %d", len(jobs))
	}
}
