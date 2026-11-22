package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// A site that has not moved yet logs into its account's data/logs, while
// its file in the tree stays empty: its log tail is the old file's, read as
// the account (root reads nothing in a home), and the tree's again once it
// moved — or for a log that is not in data/logs.
func TestSiteLogsEndpointReadsAnUnmovedSiteAsTheAccount(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	moves, _ := json.Marshal(siteMoves{Logs: f.s.siteLogsRoot(), Sites: map[int64]*siteMove{site.ID: {Domain: site.Domain}}})
	if err := f.db.SetSetting(f.ctx, settingSiteMoves, string(moves)); err != nil {
		t.Fatal(err)
	}
	const tree = "/var/log/monopanel/sites/alex/example.com.php.error.log"
	f.agent.ReadFile[tree] = ""
	old := map[string]string{"data/logs/example.com.php.error.log": "PHP Warning: old one\nPHP Warning: old two\n"}
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		if len(req.Args) != 3 || req.Args[0] != "tail" || req.Login != "alex" {
			return nil
		}
		out := map[string]any{"missing": true}
		if c, ok := old[req.Args[1]]; ok {
			out = map[string]any{"size": len(c), "content": c}
		}
		b, _ := json.Marshal(out)
		return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString(b)}
	}
	f.agent.Reset()
	var tail apitypes.LogTail
	f.call(http.MethodGet, "/sites/example.com/logs/php?lines=1", nil, http.StatusOK, &tail)
	if tail.Path != "/var/www/alex/data/logs/example.com.php.error.log" || tail.Size != 42 || !slices.Equal(tail.Lines, []string{"PHP Warning: old two"}) {
		t.Fatalf("an unmoved site: %+v", tail)
	}
	if f.agent.Called("/v1/file/read") {
		t.Fatal("root read the log")
	}
	// Not in data/logs: the tree's.
	tail = apitypes.LogTail{}
	f.call(http.MethodGet, "/sites/example.com/logs/access", nil, http.StatusOK, &tail)
	if tail.Path != "/var/log/monopanel/sites/alex/example.com.access.log" {
		t.Fatalf("a log not in data/logs: %+v", tail)
	}
	// Moved: the tree's.
	f.s.siteMoved(f.ctx, site.ID, f.s.siteLogsRoot())
	f.agent.Reset()
	tail = apitypes.LogTail{}
	f.call(http.MethodGet, "/sites/example.com/logs/php", nil, http.StatusOK, &tail)
	if tail.Path != tree || f.agent.Called("/v1/runas") {
		t.Fatalf("a moved site: %+v", tail)
	}
}

// A site that has not moved may have its log in both places: after a
// partial apply in Apache mode Apache writes into the tree while its old
// file in data/logs, of 0.8.19, stays as it was. The tail is the file's
// written last — the tree's once it holds something written after the old
// one, else the old one, read as the account; an fsop of an older binary
// says no time, and the old one stays the one, as before.
func TestSiteLogsEndpointReadsTheLiveFileOfAnUnmovedSite(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	moves, _ := json.Marshal(siteMoves{Logs: f.s.siteLogsRoot(), Sites: map[int64]*siteMove{site.ID: {Domain: site.Domain}}})
	if err := f.db.SetSetting(f.ctx, settingSiteMoves, string(moves)); err != nil {
		t.Fatal(err)
	}
	const (
		tree = "/var/log/monopanel/sites/alex/example.com.apache.access.log"
		old  = "/var/www/alex/data/logs/example.com.apache.access.log"
	)
	upgraded := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	oldMod := upgraded.Add(-time.Hour).Format(time.RFC3339)
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		if len(req.Args) != 3 || req.Args[0] != "tail" || req.Args[1] != "data/logs/example.com.apache.access.log" {
			return nil
		}
		out := map[string]any{"size": 18, "content": "GET /old HTTP/1.1\n"}
		if oldMod != "" {
			out["mtime"] = oldMod
		}
		b, _ := json.Marshal(out)
		return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString(b)}
	}
	f.agent.ReadFile[tree] = "GET /live HTTP/1.1\n"
	f.agent.ModTime = map[string]time.Time{tree: upgraded.Add(time.Minute)}
	get := func() apitypes.LogTail {
		t.Helper()
		var tail apitypes.LogTail
		f.call(http.MethodGet, "/sites/example.com/logs/apache-access", nil, http.StatusOK, &tail)
		return tail
	}
	if tail := get(); tail.Path != tree || !slices.Equal(tail.Lines, []string{"GET /live HTTP/1.1"}) {
		t.Fatalf("the tree's file written after the old one: %+v", tail)
	}
	// The old one written last: it is the live one.
	f.agent.ModTime[tree] = upgraded.Add(-2 * time.Hour)
	if tail := get(); tail.Path != old || !slices.Equal(tail.Lines, []string{"GET /old HTTP/1.1"}) {
		t.Fatalf("the old file written after the tree's: %+v", tail)
	}
	// Written in the same second: the old one, as before.
	f.agent.ModTime[tree] = upgraded.Add(-time.Hour + 500*time.Millisecond)
	if tail := get(); tail.Path != old {
		t.Fatalf("both written in one second: %+v", tail)
	}
	// The tree's file as the move made it, empty, however new.
	f.agent.ReadFile[tree] = ""
	f.agent.ModTime[tree] = upgraded.Add(time.Minute)
	if tail := get(); tail.Path != old {
		t.Fatalf("the tree's empty file: %+v", tail)
	}
	// An fsop that says no time: the old one, and root looks at nothing.
	f.agent.ReadFile[tree] = "GET /live HTTP/1.1\n"
	oldMod = ""
	f.agent.Reset()
	if tail := get(); tail.Path != old || f.agent.Called("/v1/stat") || f.agent.Called("/v1/file/read") {
		t.Fatalf("no time from fsop: %+v", tail)
	}
}

// pendingMoves are the sites that have not moved their logs yet, by domain.
func (f *siteFixture) pendingMoves() map[string]*siteMove {
	f.t.Helper()
	out := map[string]*siteMove{}
	for _, mv := range f.s.siteMovesState(f.ctx).Sites {
		out[mv.Domain] = mv
	}
	return out
}

// logMoveJobs are the site.apply jobs queued to move a site's logs, by site
// ID, and the mail.apply jobs the system queued.
func (f *siteFixture) logMoveJobs() (sites map[int64][]int64, mail int) {
	f.t.Helper()
	jobs, err := f.db.ListJobs(f.ctx, 500, "")
	if err != nil {
		f.t.Fatal(err)
	}
	sites = map[int64][]int64{}
	for _, j := range jobs {
		var p sitePayload
		switch {
		case j.Type == "site.apply" && json.Unmarshal(j.Payload, &p) == nil && p.LogsMoved:
			sites[p.SiteID] = append(sites[p.SiteID], j.ID)
		case j.Type == "mail.apply" && j.RequestedBy == "system":
			mail++
		}
	}
	return sites, mail
}

// An upgraded panel switches every site to the tree: the directories and
// logs of each account with sites, a site.apply for every site (whose log
// says the old files stay), the webmail on its port re-rendered. What it
// remembers is what finished — each site as its apply succeeds — and the
// next start does nothing.
func TestMoveSiteLogsRecordsWhatFinished(t *testing.T) {
	f := newMailFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "app.example.com", "user": "alex", "mode": "proxy", "backend": "http://127.0.0.1:3000", "ssl": "none"})
	c := f.s.loadMailConfig(f.ctx)
	c.Webmail, c.WebmailPort = "example.com", 2096
	if err := f.s.saveMailConfig(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	f.settle()
	// What a panel up to 0.8.19 left: no list of the moves.
	if m := f.s.siteMovesState(f.ctx); m.Logs != "" {
		t.Fatalf("the move ran before the start: %+v", m)
	}
	f.agent.Reset()
	// The first site.apply is held while it applies: the start after it
	// queues nothing beside the jobs still queued or running.
	held, release := heldApply(f, "/etc/nginx/monopanel/sites/example.com.conf")

	f.s.moveSiteLogs(f.ctx)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the site.apply never applied nginx")
	}
	f.s.moveSiteLogs(f.ctx)
	if sites, mail := f.logMoveJobs(); len(sites) != 2 || mail != 1 {
		t.Fatalf("queued while the first jobs run: sites %v, mail.apply %d", sites, mail)
	} else {
		for id, ids := range sites {
			if len(ids) != 1 {
				t.Fatalf("site %d queued again while its job ran: %v", id, ids)
			}
		}
	}
	release()
	if got := f.agent.SiteLogs("alex"); !slices.Equal(got, []string{
		"app.example.com.access.log", "app.example.com.error.log",
		"example.com.access.log", "example.com.error.log", "example.com.php.error.log", "example.com.php.slow.log",
		"webmail.access.log", "webmail.error.log",
	}) {
		t.Fatalf("logs prepared for alex: %v", got)
	}
	sites, mail := f.logMoveJobs()
	all, _ := f.db.ListSites(f.ctx, 0)
	if len(sites) != len(all) || len(all) != 2 || mail != 1 {
		t.Fatalf("queued: sites %v of %d, mail.apply %d", sites, len(all), mail)
	}
	for id, ids := range sites {
		if len(ids) != 1 {
			t.Fatalf("site %d queued %d times", id, len(ids))
		}
	}
	f.settle()
	if m := f.s.siteMovesState(f.ctx); m.Logs != "/var/log/monopanel/sites" || len(m.Sites) != 0 || m.Webmail != nil {
		t.Fatalf("after the jobs: %+v", m)
	}
	site, _ := f.db.GetSiteByDomain(f.ctx, "example.com")
	j := f.waitJob(sites[site.ID][0]) // the list leaves the logs out
	if j.Status != store.JobDone || !strings.Contains(j.Log, "/var/www/alex/data/logs") || !strings.Contains(j.Log, "the old files stay there") {
		t.Fatalf("site.apply after the move: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	if conf, _ := f.agent.File("/etc/nginx/monopanel/sites/example.com.conf"); !strings.Contains(conf, "access_log /var/log/monopanel/sites/alex/example.com.access.log main;") {
		t.Fatalf("the site was not rendered anew:\n%s", conf)
	}
	if c := f.s.siteLogsCheck(f.ctx); c == nil || c.Status != "ok" {
		t.Fatalf("doctor: %+v", c)
	}

	// The next start: nothing.
	f.agent.Reset()
	f.s.moveSiteLogs(f.ctx)
	if again, mail2 := f.logMoveJobs(); len(again) != 2 || mail2 != 1 || f.agent.Called("/v1/sitelogs/ensure") {
		t.Fatalf("the move ran twice: sites %v, mail.apply %d, agent %v", again, mail2, f.agent.Calls())
	}
}

// A site whose move fails stays on the list, with the job's error, and is
// not queued again at every start: the next try waits ten minutes, then four
// times as long each time. Meanwhile the jails watch its files in the tree,
// which the move made and which stay empty until it moves — never its
// data/logs — and the doctor names it with the reason. mp site apply, once
// the cause is fixed, moves it at once; nothing is queued for it after that.
func TestMoveSiteLogsRetriesWhatFailed(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	// b's PHP branch went away since: it cannot be applied.
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.s.moveSiteLogs(f.ctx)
	f.settle()
	pending := f.pendingMoves()
	if len(pending) != 1 || pending["b.example.com"] == nil {
		t.Fatalf("pending after the first try: %v", pending)
	}
	// The failure counts when the panel looks next — at the next start —
	// and that start queues nothing for b.
	before, _ := f.logMoveJobs()
	f.s.moveSiteLogs(f.ctx)
	after, _ := f.logMoveJobs()
	mv := f.pendingMoves()["b.example.com"]
	if len(after[b.ID]) != len(before[b.ID]) || mv == nil || mv.Tries != 1 || mv.Job != 0 || !strings.Contains(mv.Error, "PHP 7.4 is not installed") {
		t.Fatalf("after the failure: jobs %v → %v, move %+v", before, after, mv)
	}
	if wait := time.Until(mv.Next); wait < 9*time.Minute || wait > 11*time.Minute {
		t.Fatalf("next try in %s, want 10m", wait)
	}
	access, _ := f.s.fail2banSiteLogs(f.ctx)
	if !slices.Equal(access, []string{"/var/log/monopanel/sites/alex/a.example.com.access.log", "/var/log/monopanel/sites/alex/b.example.com.access.log"}) {
		t.Fatalf("jails: %v", access)
	}
	// Not made by the move (a host whose tree the agent refused): left out,
	// the file may not be there.
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Sites[b.ID].Made = false
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if access, _ := f.s.fail2banSiteLogs(f.ctx); !slices.Equal(access, []string{"/var/log/monopanel/sites/alex/a.example.com.access.log"}) {
		t.Fatalf("jails without b's logs made: %v", access)
	}
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Sites[b.ID].Made = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	c := f.s.siteLogsCheck(f.ctx)
	if c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "b.example.com (failed 1 time(s)") || !strings.Contains(c.Detail, "PHP 7.4 is not installed") || strings.Contains(c.Detail, "a.example.com") {
		t.Fatalf("doctor: %+v", c)
	}

	// Its time has come: queued again, failing again, waiting four times as
	// long.
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Sites[b.ID].Next = time.Now().Add(-time.Second)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	f.s.moveSiteLogs(f.ctx)
	if again, _ := f.logMoveJobs(); len(again[b.ID]) != len(before[b.ID])+1 {
		t.Fatalf("not queued again when due: %v", again)
	}
	f.settle()
	f.s.moveSiteLogs(f.ctx)
	mv = f.pendingMoves()["b.example.com"]
	if wait := time.Until(mv.Next); mv.Tries != 2 || wait < 39*time.Minute || wait > 41*time.Minute {
		t.Fatalf("second failure: %+v, next in %s", mv, wait)
	}

	// Fixed and applied by hand: moved, and nothing more is queued.
	b.PHPVersion = "8.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.siteAction("b.example.com", "apply")
	if p := f.pendingMoves(); len(p) != 0 {
		t.Fatalf("still pending: %v", p)
	}
	jobs, _ := f.logMoveJobs()
	f.s.moveSiteLogs(f.ctx)
	if again, _ := f.logMoveJobs(); len(again[b.ID]) != len(jobs[b.ID]) {
		t.Fatal("queued again after it moved")
	}
	if c := f.s.siteLogsCheck(f.ctx); c == nil || c.Status != "ok" {
		t.Fatalf("doctor after the fix: %+v", c)
	}
	if access, _ := f.s.fail2banSiteLogs(f.ctx); len(access) != 2 {
		t.Fatalf("jails after the fix: %v", access)
	}
}

// A fresh panel has no sites to move and remembers so: its sites get the
// tree from the start and never enter the list, not even one whose first
// apply failed.
func TestMoveSiteLogsOnAFreshPanel(t *testing.T) {
	f := newSiteFixture(t)
	f.s.moveSiteLogs(f.ctx)
	if m := f.s.siteMovesState(f.ctx); m.Logs != "/var/log/monopanel/sites" || len(m.Sites) != 0 {
		t.Fatalf("moves: %+v", m)
	}
	if jobs, _ := f.db.ListJobs(f.ctx, 500, ""); len(jobs) != 0 {
		t.Fatalf("jobs on a panel without sites: %d", len(jobs))
	}
	if c := f.s.siteLogsCheck(f.ctx); c == nil || c.Status != "ok" {
		t.Fatalf("doctor: %+v", c)
	}
	f.agent.Fail["/v1/config/apply"] = "nginx: [emerg] something else"
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	delete(f.agent.Fail, "/v1/config/apply")
	f.s.moveSiteLogs(f.ctx)
	if sites, _ := f.logMoveJobs(); len(sites) != 0 || len(f.pendingMoves()) != 0 {
		t.Fatalf("a new site was taken for an old one: %v", sites)
	}
}

// On a host with SELinux nothing switches to the tree before its label rule
// is in place — nginx and php-fpm could not open logs labelled var_log_t —
// and the doctor says what the sites wait for; once the rule is recorded,
// they move. Without SELinux on the host the rule is not waited for.
func TestMoveSiteLogsWaitsForTheSELinuxRule(t *testing.T) {
	withOSRelease(t, elRelease)
	enforce := filepath.Join(t.TempDir(), "enforce")
	if err := os.WriteFile(enforce, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := selinuxEnforcePath
	selinuxEnforcePath = enforce
	t.Cleanup(func() { selinuxEnforcePath = prev })
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	// A host whose rule could not be added (semanage failed at this start).
	if err := f.db.SetSetting(f.ctx, settingSELinuxSiteLogs, ""); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	f.s.moveSiteLogs(f.ctx)
	if sites, _ := f.logMoveJobs(); len(sites) != 0 || f.agent.Called("/v1/sitelogs/ensure") {
		t.Fatalf("moved before the label rule: %v", sites)
	}
	if p := f.pendingMoves(); len(p) != 1 {
		t.Fatalf("pending: %v", p)
	}
	// Its logs in the tree are not made yet: the jails leave it out.
	if access, _ := f.s.fail2banSiteLogs(f.ctx); len(access) != 0 {
		t.Fatalf("jails before the move made the logs: %v", access)
	}
	if c := f.s.siteLogsCheck(f.ctx); c == nil || !strings.Contains(c.Detail, "example.com (waits for the SELinux label rule of /var/log/monopanel/sites)") {
		t.Fatalf("doctor: %+v", c)
	}
	if err := f.db.SetSetting(f.ctx, settingSELinuxSiteLogs, f.s.selinuxSiteLogsContext()[0]); err != nil {
		t.Fatal(err)
	}
	f.s.moveSiteLogs(f.ctx)
	f.settle()
	if sites, _ := f.logMoveJobs(); len(sites) != 1 || len(f.pendingMoves()) != 0 {
		t.Fatalf("after the rule: %v, pending %v", sites, f.pendingMoves())
	}

	// SELinux off: no rule to wait for.
	selinuxEnforcePath = filepath.Join(t.TempDir(), "none")
	g := newSiteFixture(t)
	g.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	g.s.moveSiteLogs(g.ctx)
	if sites, _ := g.logMoveJobs(); len(sites) != 1 {
		t.Fatalf("SELinux off: %v", sites)
	}
}

// Only sites whose configuration may log into data/logs move: a suspended
// site's stub logs nothing, and a site of an account being removed goes with
// it. A pending one moves: it may be a site in error applied again, whose
// apply the upgrade interrupted, still on the older panel's configuration.
func TestMoveSiteLogsLeavesSitesWithoutLogs(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "s.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.siteAction("s.example.com", "suspend")
	owner, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: owner.ID, Domain: "p.example.com", PHPVersion: "8.4", SSL: "none"}); err != nil {
		t.Fatal(err)
	}
	uid, gid := 1501, 1501
	gone := &store.User{Login: "gone", Role: store.RoleUser, Status: store.UserDeleting, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/gone"}
	if err := f.db.CreateUser(f.ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: gone.ID, Domain: "g.example.com", PHPVersion: "8.4", SSL: "none", Status: store.SiteActive}); err != nil {
		t.Fatal(err)
	}
	f.s.moveSiteLogs(f.ctx)
	if p := f.pendingMoves(); len(p) != 2 || p["a.example.com"] == nil || p["p.example.com"] == nil {
		t.Fatalf("listed: %v", p)
	}
	f.settle()
	if sites, _ := f.logMoveJobs(); len(sites) != 2 {
		t.Fatalf("queued: %v", sites)
	}
}
