package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// roundcubeMySQL plays the MySQL server behind the webmail job: the tables and
// the schema version of the webmail's database, and the accounts with the
// passwords the statements of the panel leave them with. Like the real
// server, it refuses mysql.initial.sql (plain CREATE TABLE) on a loaded
// database and lets an account in with its own password only.
type roundcubeMySQL struct {
	mu       sync.Mutex
	tables   []string
	version  string
	accounts map[string]string
	root     []string // every batch run as root, in order
}

var (
	rcAccountStmtRe   = regexp.MustCompile(`(?:CREATE USER IF NOT EXISTS|ALTER USER) '([^']+)'@'localhost' IDENTIFIED WITH \w+ BY '((?:[^'\\]|\\.)*)'`)
	rcAccountQueryRe  = regexp.MustCompile(`FROM mysql\.user WHERE user = '([^']+)'`)
	rcSchemaVersionRe = regexp.MustCompile(`'roundcube-version', '(\d+)'`)
)

func (m *roundcubeMySQL) hook(req agent.ToolRequest) *agent.ToolResponse {
	if req.Name != "mysql" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range req.Args {
		if user, ok := strings.CutPrefix(a, "--user="); ok {
			password := ""
			for _, e := range req.Env {
				password = strings.TrimPrefix(e, "MYSQL_PWD=")
			}
			if pw, ok := m.accounts[user]; ok && pw == password {
				return &agent.ToolResponse{Output: "1\n"}
			}
			return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 1045 (28000): Access denied for user '" + user + "'@'localhost' (using password: YES)"}
		}
	}
	sql := req.Stdin
	m.root = append(m.root, sql)
	switch {
	case strings.Contains(sql, "FROM information_schema.tables WHERE table_schema"):
		return &agent.ToolResponse{Output: strings.Join(m.tables, "\n")}
	case strings.HasPrefix(sql, "SELECT `value` FROM"):
		return &agent.ToolResponse{Output: m.version}
	case rcAccountQueryRe.MatchString(sql):
		if _, ok := m.accounts[rcAccountQueryRe.FindStringSubmatch(sql)[1]]; ok {
			return &agent.ToolResponse{Output: "1\n"}
		}
		return &agent.ToolResponse{Output: "0\n"}
	}
	if strings.HasPrefix(sql, "USE `") && strings.Contains(sql, "CREATE TABLE") {
		if len(m.tables) > 0 {
			return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 1050 (42S01) at line 2: Table 'session' already exists"}
		}
		m.tables = []string{"session", "system", "users"}
		if v := rcSchemaVersionRe.FindStringSubmatch(sql); v != nil {
			m.version = v[1]
		}
	}
	for _, st := range rcAccountStmtRe.FindAllStringSubmatch(sql, -1) {
		m.accounts[st[1]] = strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(st[2])
	}
	return &agent.ToolResponse{}
}

func (m *roundcubeMySQL) with(fn func(m *roundcubeMySQL)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

func (m *roundcubeMySQL) password(account string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[account]
}

// batches returns how many root batches ran, since(n) the ones after the first n.
func (m *roundcubeMySQL) batches() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.root)
}

func (m *roundcubeMySQL) since(n int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.root[n:])
}

// ownerSide plays what the job does as the site's owner (fsop): read answers
// with the files a test put into the home, list with a directory's entries,
// run records Roundcube's update script.
type ownerSide struct {
	mu       sync.Mutex
	files    map[string]string               // path in the home → content
	dirs     map[string][]apitypes.FileEntry // path in the home → entries
	runs     [][]string                      // login, then the fsop arguments
	runFails string
}

func (o *ownerSide) hook(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := func(s string) *agent.RunAsUserResponse {
		return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(s))}
	}
	switch req.Args[0] {
	case "read":
		if c, ok := o.files[req.Args[1]]; ok {
			return out(c)
		}
		return &agent.RunAsUserResponse{ExitCode: 1, Stderr: "Error: open " + req.Args[1] + ": no such file or directory"}
	case "list":
		raw, _ := json.Marshal(o.dirs[req.Args[1]])
		return out(string(raw))
	case "run":
		o.runs = append(o.runs, append([]string{req.Login}, req.Args...))
		if o.runFails != "" {
			return &agent.RunAsUserResponse{ExitCode: 1, Stderr: o.runFails}
		}
		return out("Updating database schema (2025010100)... [OK]\n")
	}
	return nil
}

func (o *ownerSide) with(fn func(o *ownerSide)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fn(o)
}

func (o *ownerSide) ran() [][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.runs)
}

// settle waits until no job is queued or running — the site apply a webmail
// job leaves behind included — so that a test changes the fake host only
// while nothing uses it.
func (f *siteFixture) settle() {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, err := f.db.ListJobs(f.ctx, 500, "")
		if err != nil {
			f.t.Fatal(err)
		}
		busy := false
		for _, j := range list {
			busy = busy || j.Status == store.JobQueued || j.Status == store.JobRunning
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatal("jobs are still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newWebmailFixture is a panel with the mail server and a database server, a
// stand-in Roundcube release in the download directory, roundcubeMySQL behind
// the fake agent and ownerSide for what runs as the owner.
func newWebmailFixture(t *testing.T) (*siteFixture, *roundcubeMySQL, *ownerSide) {
	t.Helper()
	f := newMailFixture(t)
	f.installDB()
	f.settle()
	my := &roundcubeMySQL{accounts: map[string]string{}}
	home := &ownerSide{files: map[string]string{}, dirs: map[string][]apitypes.FileEntry{}}
	f.agent.ToolHook, f.agent.RunAsHook = my.hook, home.hook
	f.roundcubeRelease(t)
	return f, my, home
}

// roundcubeRelease puts a small stand-in for the Roundcube release where
// fetchRoundcube finds an earlier download, and has the job trust its digest:
// a schema that records version 2025010100 and two updates up to it.
func (f *siteFixture) roundcubeRelease(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "roundcubemail-" + roundcubeVersion + "/"
	for _, file := range [][2]string{
		{"index.php", "<?php // Roundcube Webmail\n"},
		{"bin/updatedb.sh", "#!/usr/bin/env php\n<?php\n"},
		{"SQL/mysql.initial.sql", "SET FOREIGN_KEY_CHECKS=0;\nCREATE TABLE `session` (`sess_id` varchar(128) NOT NULL);\nCREATE TABLE `users` (`user_id` int NOT NULL);\n" +
			"CREATE TABLE `system` (`name` varchar(64) NOT NULL, `value` mediumtext);\nSET FOREIGN_KEY_CHECKS=1;\nINSERT INTO `system` (`name`, `value`) VALUES ('roundcube-version', '2025010100');\n"},
		{"SQL/mysql/2024010100.sql", "ALTER TABLE `users` ADD `a` int;\n"},
		{"SQL/mysql/2025010100.sql", "ALTER TABLE `users` ADD `b` int;\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: top + file[0], Mode: 0o644, Size: int64(len(file[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.s.cfg.DataDir, "downloads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roundcubemail-"+roundcubeVersion+"-complete.tar.gz"), buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	prev := roundcubeSHA256
	roundcubeSHA256 = hex.EncodeToString(sum[:])
	t.Cleanup(func() { roundcubeSHA256 = prev })
}

// webmailJob posts mp mail webmail and waits for the job.
func (f *siteFixture) webmailJob(body map[string]any) *store.Job {
	f.t.Helper()
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/webmail", body, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

// installedWebmail runs the first install (with extra request fields, if any)
// and puts its config.inc.php where the next run looks for it, as on a real
// host.
func installedWebmail(t *testing.T, f *siteFixture, home *ownerSide, extra ...map[string]any) string {
	t.Helper()
	body := map[string]any{"domain": "webmail.example.com", "user": "alex"}
	for _, e := range extra {
		for k, v := range e {
			body[k] = v
		}
	}
	if job := f.webmailJob(body); job.Status != store.JobDone {
		t.Fatalf("webmail install: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	const file = "/var/www/alex/data/www/webmail.example.com/config/config.inc.php"
	conf, ok := f.agent.File(file)
	if !ok {
		t.Fatal("config.inc.php was not written")
	}
	f.settle()
	f.agent.Stat[file] = false
	home.with(func(o *ownerSide) { o.files["data/www/webmail.example.com/config/config.inc.php"] = conf })
	return conf
}

// A second run of the webmail job (mp mail webmail <domain> again, the Web's
// "Reinstall / update") over a working install must leave it working: no
// schema load into the database that has one, the account keeps the password
// config.inc.php names, the DES key stays, and the configuration comes out
// the same — only the recorded version follows the release.
func TestWebmailRerunKeepsTheInstall(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	conf := installedWebmail(t, f, home)
	pw := my.password("alex_roundcube")
	if pw == "" || !strings.Contains(conf, "'mysql://alex_roundcube:"+rawURLEncode(pw)+"@unix(/var/run/mysqld/mysqld.sock)/alex_roundcube'") {
		t.Fatalf("the DSN does not carry the account's password:\n%s", conf)
	}
	my.with(func(m *roundcubeMySQL) {
		if m.version != "2025010100" {
			t.Fatalf("the schema was not loaded on the first run: %v %q", m.tables, m.version)
		}
	})
	// An older panel installed it.
	c := f.s.loadMailConfig(f.ctx)
	c.WebmailVersion = "1.7.3"
	if err := f.s.saveMailConfig(f.ctx, c); err != nil {
		t.Fatal(err)
	}

	n := my.batches()
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobDone {
		t.Fatalf("second run: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, sql := range my.since(n) {
		if strings.Contains(sql, "CREATE TABLE") {
			t.Errorf("the schema was loaded into a database that has it:\n%s", sql)
		}
		if strings.Contains(sql, "IDENTIFIED") {
			t.Errorf("the account's password was touched:\n%s", sql)
		}
	}
	if my.password("alex_roundcube") != pw {
		t.Error("the account's password changed")
	}
	if again, _ := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); again != conf {
		t.Fatalf("the configuration changed on a second run:\n--- first\n%s\n--- second\n%s", conf, again)
	}
	if got := f.s.loadMailConfig(f.ctx); got.Webmail != "webmail.example.com" || got.WebmailVersion != roundcubeVersion || got.WebmailPort != 0 {
		t.Fatalf("mail settings after the second run: %+v", got)
	}
	if runs := home.ran(); len(runs) != 0 {
		t.Fatalf("bin/updatedb.sh ran over a current schema: %v", runs)
	}
	if !strings.Contains(job.Log, "keeps its password") || !strings.Contains(job.Log, "the DES key stays") || !strings.Contains(job.Log, "Roundcube 1.7.3 → "+roundcubeVersion) {
		t.Errorf("the log does not say what was kept:\n%s", job.Log)
	}
}

// A webmail published on a port calls itself by the mail server's name; a
// run without --port keeps the port, and with it the name.
func TestWebmailRerunWithoutPortKeepsThePort(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	conf := installedWebmail(t, f, home, map[string]any{"port": 2096})
	if !strings.Contains(conf, "$config['product_name'] = 'mail.example.com Webmail';") {
		t.Fatalf("a webmail on a port is named after the mail server:\n%s", conf)
	}
	if job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"}); job.Status != store.JobDone {
		t.Fatalf("second run: %s %s", job.Status, job.Error)
	}
	if again, _ := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); again != conf {
		t.Fatalf("the configuration changed on a run without --port:\n%s", again)
	}
	if c := f.s.loadMailConfig(f.ctx); c.WebmailPort != 2096 {
		t.Fatalf("the webmail port is gone: %d", c.WebmailPort)
	}
}

// The run before this fix changed the account's password and then failed on
// the schema, leaving config.inc.php with the old one. The next run sets the
// account back to the password the configuration names and leaves the file
// as it is; plugins and settings an administrator added are kept as well.
func TestWebmailRerunRepairsTheAccountPassword(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	conf := installedWebmail(t, f, home)
	pw := my.password("alex_roundcube")
	my.with(func(m *roundcubeMySQL) { m.accounts["alex_roundcube"] = "Lost-in-memory-7" })

	n := my.batches()
	if job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"}); job.Status != store.JobDone {
		t.Fatalf("repair run: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if my.password("alex_roundcube") != pw {
		t.Fatal("the account was not set back to the password config.inc.php names")
	}
	altered := 0
	for _, sql := range my.since(n) {
		if strings.Contains(sql, "ALTER USER 'alex_roundcube'@'localhost'") {
			altered++
		}
	}
	if altered != 1 {
		t.Fatalf("want one ALTER USER, got %d", altered)
	}
	if again, _ := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); again != conf {
		t.Fatalf("the configuration changed:\n%s", again)
	}
	// The password went to MySQL on stdin and to the probe in its
	// environment, never on a command line every local user can read.
	for _, tool := range f.agent.Tools() {
		if strings.Contains(strings.Join(tool.Args, " "), pw) {
			t.Fatalf("the account's password is on the argv of %s: %v", tool.Name, tool.Args)
		}
	}
	for _, run := range f.agent.RunAs() {
		if strings.Contains(strings.Join(run.Args, " "), pw) {
			t.Fatalf("the account's password is on an fsop argv: %v", run.Args)
		}
	}

	// A plugin switched on by hand stays on.
	edited := strings.Replace(conf, "    'newmail_notifier',\n", "    'newmail_notifier',\n    // 'password',\n    'markasjunk',\n", 1)
	home.with(func(o *ownerSide) { o.files["data/www/webmail.example.com/config/config.inc.php"] = edited })
	if job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"}); job.Status != store.JobDone {
		t.Fatalf("run with an added plugin: %s %s", job.Status, job.Error)
	}
	again, _ := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php")
	if !strings.Contains(again, "    'newmail_notifier',\n    'markasjunk',\n];") || strings.Contains(again, "'password'") {
		t.Fatalf("the added plugin is not kept (or the commented one came back):\n%s", again)
	}
}

// A database of an older Roundcube is brought up to the release by
// Roundcube's own bin/updatedb.sh, as the owner with the site's PHP, once the
// configuration it reads is in place; its failure fails the job.
func TestWebmailRerunUpdatesAnOlderSchema(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	conf := installedWebmail(t, f, home)
	my.with(func(m *roundcubeMySQL) { m.version = "2024010100" })

	n := my.batches()
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobDone {
		t.Fatalf("update run: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, sql := range my.since(n) {
		if strings.Contains(sql, "CREATE TABLE") {
			t.Fatalf("the initial schema was loaded over an older one:\n%s", sql)
		}
	}
	want := []string{"alex", "run", "--cwd", "data/www/webmail.example.com", "--", "/usr/bin/php8.4", "bin/updatedb.sh", "--package=roundcube", "--dir=/var/www/alex/data/www/webmail.example.com/SQL"}
	if runs := home.ran(); len(runs) != 1 || !slices.Equal(runs[0], want) {
		t.Fatalf("bin/updatedb.sh:\n got %v\nwant %v", runs, want)
	}
	if !strings.Contains(job.Log, "Updating database schema (2025010100)... [OK]") {
		t.Errorf("the update's output is not in the log:\n%s", job.Log)
	}
	if again, _ := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); again != conf {
		t.Fatalf("the configuration changed:\n%s", again)
	}

	home.with(func(o *ownerSide) { o.runFails = "ERROR: Error in DDL upgrade 2025010100: Duplicate column name 'b'" })
	job = f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "Duplicate column name") {
		t.Fatalf("a failed update must fail the job with its message: %s %s", job.Status, job.Error)
	}
}

// Tables without Roundcube's schema version — an interrupted load, or a
// database of that name made for something else — are neither loaded over
// nor updated: the job stops and says how to go on.
func TestWebmailRefusesAForeignDatabase(t *testing.T) {
	f, my, _ := newWebmailFixture(t)
	my.with(func(m *roundcubeMySQL) { m.tables = []string{"session", "users"} })
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "no Roundcube schema version") || !strings.Contains(job.Error, "mp db rm alex_roundcube") {
		t.Fatalf("a database with foreign tables: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); ok {
		t.Fatal("a configuration was written for a database the job refused")
	}
	if my.password("alex_roundcube") != "" {
		t.Fatal("an account was made for a database the job refused")
	}
}

// The webmail applies its site under the site's lock key, site:<domain>, as
// every other job of a site does (ops_sites.go, logrotate.go, cms.go, the
// shared folders, an import): it never runs beside another apply or the
// delete of the same site.
func TestWebmailAppliesTheSiteUnderItsLock(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home)
	site, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.db.ListJobs(f.ctx, 500, "")
	if err != nil {
		t.Fatal(err)
	}
	applies := 0
	for _, j := range list {
		var p sitePayload
		if j.Type != "site.apply" || json.Unmarshal(j.Payload, &p) != nil || p.SiteID != site.ID {
			continue
		}
		applies++
		if j.LockKey != "site:webmail.example.com" {
			t.Errorf("job %d applies the webmail's site under %q", j.ID, j.LockKey)
		}
	}
	if applies == 0 {
		t.Fatal("the webmail job queued no apply of its site")
	}
}

// An existing site keeps its owner: the webmail is installed into that
// account's tree and database, whoever the request named.
func TestWebmailKeepsTheSiteOwner(t *testing.T) {
	f, my, _ := newWebmailFixture(t)
	uid, gid := 1501, 1501
	bob := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/bob"}
	if err := f.db.CreateUser(f.ctx, bob); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: bob.ID, Domain: "webmail.example.com", PHPVersion: "8.4", Docroot: "public_html"}); err != nil {
		t.Fatal(err)
	}
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobDone {
		t.Fatalf("install into bob's site: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	conf, ok := f.agent.File("/var/www/bob/data/www/webmail.example.com/config/config.inc.php")
	if !ok || !strings.Contains(conf, "mysql://bob_roundcube:") {
		t.Fatalf("the webmail is not in bob's tree with bob's database:\n%s", conf)
	}
	if _, ok := f.agent.FileContaining("/var/www/alex/data/www/webmail.example.com"); ok {
		t.Fatal("files went into alex's home for bob's site")
	}
	if my.password("bob_roundcube") == "" || my.password("alex_roundcube") != "" {
		t.Fatal("the database account is not bob's")
	}
	if db, err := f.db.GetDatabaseByName(f.ctx, "bob_roundcube"); err != nil || db.UserID != bob.ID {
		t.Fatalf("the database is not recorded for bob: %+v %v", db, err)
	}
	if !strings.Contains(job.Log, "belongs to bob") {
		t.Errorf("the log does not say whose site it is:\n%s", job.Log)
	}
}

// An existing site that is not the webmail yet may hold the placeholder page
// or an earlier Roundcube, and then gets Roundcube's docroot; a site with
// files of its own is refused before anything is unpacked over it.
func TestWebmailDoesNotUnpackOverAnotherSite(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.settle()
	f.agent.Stat["/var/www/alex/data/www/shop.example.com/"] = true
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{{Name: "index.php", Type: "file", Size: 120}, {Name: "wp-config.php", Type: "file", Size: 3000}}
	})
	f.agent.Reset()
	job := f.webmailJob(map[string]any{"domain": "shop.example.com", "user": "alex"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "already holds files of its own (index.php, wp-config.php in /var/www/alex/data/www/shop.example.com)") {
		t.Fatalf("webmail over a site with files: %s %s", job.Status, job.Error)
	}
	for _, tool := range f.agent.Tools() {
		if tool.Name == "tar" {
			t.Fatal("Roundcube was unpacked over the site's files")
		}
	}

	// The same site with nothing but the panel's placeholder page is free,
	// and it serves Roundcube's public_html from now on.
	page := f.s.placeholderPages("shop.example.com")[0]
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{{Name: placeholderName, Type: "file", Size: int64(len(page))}}
		o.files["data/www/shop.example.com/"+placeholderName] = page
	})
	job = f.webmailJob(map[string]any{"domain": "shop.example.com", "user": "alex"})
	if job.Status != store.JobDone {
		t.Fatalf("webmail into a site with the placeholder: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if site, _ := f.db.GetSiteByDomain(f.ctx, "shop.example.com"); site.Docroot != "public_html" {
		t.Fatalf("the webmail site must serve public_html, not %q", site.Docroot)
	}

	// An earlier Roundcube (an install that did not finish) is free as well.
	f.settle()
	c := f.s.loadMailConfig(f.ctx)
	c.Webmail = ""
	if err := f.s.saveMailConfig(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{{Name: "index.php", Type: "file"}, {Name: "program", Type: "dir"}, {Name: "SQL", Type: "dir"}, {Name: "public_html", Type: "dir"}}
	})
	if job := f.webmailJob(map[string]any{"domain": "shop.example.com", "user": "alex"}); job.Status != store.JobDone {
		t.Fatalf("webmail over an earlier Roundcube: %s %s", job.Status, job.Error)
	}
}

// config.inc.php is the owner's file: only values that can go back into the
// file and into MySQL as they are are taken over, the last assignment wins
// as in PHP, and commented plugins stay off.
func TestParseRoundcubeConfig(t *testing.T) {
	k := parseRoundcubeConfig(`<?php
$config['db_dsnw'] = 'mysql://old:x@unix(/a)/old';
$config['db_dsnw'] = 'mysql://alex_roundcube:Ab3%25x%2B9_!z=@unix(/var/run/mysqld/mysqld.sock)/alex_roundcube';
$config['des_key'] = 'abcdefghijklmnopqrstuvwx';
$config['plugins'] = [
    'archive', 'zipdownload',
    /* 'password', */
    # 'markasjunk',
    'contextmenu', // third-party
    'bad-name',
];
`)
	if k.dbUser != "alex_roundcube" || k.dbName != "alex_roundcube" || k.dbPassword != "Ab3%x+9_!z=" {
		t.Errorf("DSN: %q %q %q", k.dbUser, k.dbPassword, k.dbName)
	}
	if k.desKey != "abcdefghijklmnopqrstuvwx" {
		t.Errorf("des_key: %q", k.desKey)
	}
	if !slices.Equal(k.plugins, []string{"archive", "zipdownload", "contextmenu"}) {
		t.Errorf("plugins: %v", k.plugins)
	}
	// What cannot go back as it is, is made anew.
	k = parseRoundcubeConfig(`$config['db_dsnw'] = 'mysql://alex_roundcube:short@unix(/s)/alex_roundcube';
$config['des_key'] = 'too-short';`)
	if k.dbPassword != "" || k.desKey != "" {
		t.Errorf("unusable values were kept: %+v", k)
	}
	k = parseRoundcubeConfig("$config['db_dsnw'] = 'mysql://alex_roundcube:new%0Aline-pass@unix(/s)/alex_roundcube';")
	if k.dbPassword != "" {
		t.Errorf("a password with a newline was kept: %q", k.dbPassword)
	}
}

// Roundcube rawurldecodes the DSN's credentials: the panel encodes them, and
// reads back what an older panel wrote unencoded the way Roundcube did.
func TestRoundcubeDSN(t *testing.T) {
	for _, pw := range []string{"Ab3%2Bx!+=._-~", "plain", "a%zz", "%"} {
		user, got, db, ok := parseRoundcubeDSN(roundcubeDSN("alex_roundcube", pw, "/var/run/mysqld/mysqld.sock", "alex_roundcube"))
		if !ok || got != pw || user != "alex_roundcube" || db != "alex_roundcube" {
			t.Errorf("%q came back as %q (%v)", pw, got, ok)
		}
	}
	if got := rawURLEncode("Ab3%x+9_!z=.~-"); got != "Ab3%25x%2B9_%21z%3D.~-" {
		t.Errorf("rawurlencode: %s", got)
	}
	// An older panel's unencoded "%zz" meant itself to Roundcube, "%2B" a "+".
	if rawURLDecode("a%zz%2B%") != "a%zz+%" {
		t.Errorf("rawurldecode: %s", rawURLDecode("a%zz%2B%"))
	}
}

// The webmail's database is <owner>_roundcube. When the panel has that name
// for a database of another account (alex + "x_roundcube" spells the one of
// alex_x), the job must not take it over: it stops before it makes a site,
// unpacks a file or asks MySQL for anything.
func TestWebmailDoesNotTakeAnotherAccountsDatabase(t *testing.T) {
	f, my, _ := newWebmailFixture(t)
	alexX := f.userWithPassword("alex_x", "alex-x-password", 1502)
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	squat := &store.Database{UserID: alex.ID, Name: "alex_x_roundcube"}
	if err := f.db.CreateDatabase(f.ctx, squat); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	n := my.batches()
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": alexX.Login})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "alex_x_roundcube, and that name belongs to a database of another account") {
		t.Fatalf("webmail over another account's database: %s %s", job.Status, job.Error)
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com"); err == nil {
		t.Fatal("a site was made for a webmail the job refused")
	}
	if got := my.since(n); len(got) != 0 {
		t.Fatalf("MySQL was asked for a refused webmail: %q", got)
	}
	for _, tool := range f.agent.Tools() {
		if tool.Name == "tar" {
			t.Fatal("Roundcube was unpacked for a refused webmail")
		}
	}
	if db, err := f.db.GetDatabaseByName(f.ctx, "alex_x_roundcube"); err != nil || db.UserID != alex.ID {
		t.Fatalf("the database changed hands: %+v %v", db, err)
	}

	// The same database made while the run downloads and unpacks: checked
	// again before MySQL is touched.
	if err := f.db.DeleteDatabase(f.ctx, squat.ID); err != nil {
		t.Fatal(err)
	}
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "tar" {
			if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_x_roundcube"}); err != nil {
				t.Error(err)
			}
		}
		return my.hook(req)
	}
	n = my.batches()
	job = f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": alexX.Login})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "another account") {
		t.Fatalf("webmail over a database another account made during the run: %s %s", job.Status, job.Error)
	}
	for _, sql := range my.since(n) {
		if !strings.Contains(sql, "information_schema.tables") {
			t.Fatalf("MySQL was changed for a database another account made during the run: %q", sql)
		}
	}
	if my.password("alex_x_roundcube") != "" {
		t.Fatal("an account was made for a database another account made during the run")
	}
	if db, err := f.db.GetDatabaseByName(f.ctx, "alex_x_roundcube"); err != nil || db.UserID != alex.ID {
		t.Fatalf("the database changed hands: %+v %v", db, err)
	}
}

// A database a newer Roundcube left (updated by hand past the release the
// panel pins) is not given an older Roundcube: the run stops before the
// files are unpacked, so the webmail keeps working as it is. The same holds
// for tables without a Roundcube schema version.
func TestWebmailStopsBeforeTheFilesOverANewerOrForeignSchema(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	installedWebmail(t, f, home)
	for _, c := range []struct {
		name, version string
		tables        []string
		want          string
	}{
		{"newer", "2026060100", []string{"session", "system", "users"}, "holds the schema of a newer Roundcube (2026060100) than " + roundcubeVersion + ", the one this panel installs (2025010100)"},
		{"foreign", "", []string{"session", "users"}, "no Roundcube schema version"},
	} {
		my.with(func(m *roundcubeMySQL) { m.version, m.tables = c.version, c.tables })
		f.agent.Reset()
		job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
		if job.Status != store.JobFailed || !strings.Contains(job.Error, c.want) {
			t.Fatalf("%s schema: %s %s", c.name, job.Status, job.Error)
		}
		for _, tool := range f.agent.Tools() {
			if tool.Name == "tar" {
				t.Fatalf("%s schema: Roundcube was unpacked over the webmail", c.name)
			}
		}
		if _, ok := f.agent.File("/var/www/alex/data/www/webmail.example.com/config/config.inc.php"); ok {
			t.Fatalf("%s schema: config.inc.php was written", c.name)
		}
		if len(home.ran()) != 0 {
			t.Fatalf("%s schema: bin/updatedb.sh ran", c.name)
		}
	}
}

// The download takes minutes: mail settings changed meanwhile — the server's
// name, the webmail port switched off — stay as they were set, and a run
// without --port does not bring the old port back.
func TestWebmailKeepsSettingsChangedDuringTheRun(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "tar" {
			c := f.s.loadMailConfig(f.ctx)
			c.WebmailPort, c.MaxSizeMB = 0, 77
			if err := f.s.saveMailConfig(f.ctx, c); err != nil {
				t.Error(err)
			}
		}
		return my.hook(req)
	}
	if job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"}); job.Status != store.JobDone {
		t.Fatalf("second run: %s %s", job.Status, job.Error)
	}
	if c := f.s.loadMailConfig(f.ctx); c.WebmailPort != 0 || c.MaxSizeMB != 77 || c.Webmail != "webmail.example.com" {
		t.Fatalf("settings changed during the run were written over: port %d, max size %d, webmail %q", c.WebmailPort, c.MaxSizeMB, c.Webmail)
	}
}

// Moving the webmail to another site of the same account keeps what the
// webmail in use has — its database password and DES key — and not what an
// earlier install may have left in the target site.
func TestWebmailMoveKeepsTheConfigurationInUse(t *testing.T) {
	f, my, home := newWebmailFixture(t)
	conf := installedWebmail(t, f, home)
	pw := my.password("alex_roundcube")
	key := regexp.MustCompile(`\$config\['des_key'\] = '([^']+)';`).FindStringSubmatch(conf)
	if key == nil {
		t.Fatalf("no DES key:\n%s", conf)
	}
	const target = "data/www/mail2.example.com/config/config.inc.php"
	stale := strings.NewReplacer(rawURLEncode(pw), "Stale-password-9", key[1], "stalestalestalestalestal").Replace(conf)
	f.agent.Stat["/var/www/alex/"+target] = false
	home.with(func(o *ownerSide) { o.files[target] = stale })

	n := my.batches()
	job := f.webmailJob(map[string]any{"domain": "mail2.example.com", "user": "alex"})
	if job.Status != store.JobDone {
		t.Fatalf("move: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if !strings.Contains(job.Log, "existing configuration: /var/www/alex/data/www/webmail.example.com/config/config.inc.php") {
		t.Errorf("the move did not take over the configuration in use:\n%s", job.Log)
	}
	for _, sql := range my.since(n) {
		if strings.Contains(sql, "IDENTIFIED") {
			t.Fatalf("the move changed the account's password:\n%s", sql)
		}
	}
	moved, _ := f.agent.File("/var/www/alex/" + target)
	if !strings.Contains(moved, "mysql://alex_roundcube:"+rawURLEncode(pw)+"@") || !strings.Contains(moved, "'"+key[1]+"'") {
		t.Fatalf("the moved webmail lost the password or the DES key in use:\n%s", moved)
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "mail2.example.com" {
		t.Fatalf("webmail after the move: %q", c.Webmail)
	}
}

// The webmail job works in the site's tree, the owner's database and the
// mail settings under the site's lock, with the site and its owner read
// again under it. A removal asked for before it got there — the owner's
// (user.delete marks the account first, then lists its sites) or the
// site's — stops the run before it unpacks, makes a database or names the
// site in the mail settings; the site the run made already is not missed:
// the account's removal takes it.
func TestWebmailStopsForWhatIsBeingRemoved(t *testing.T) {
	f, my, _ := newWebmailFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	// stopsBefore has fn run when the job looks at the database, after it
	// found or made the site and before it takes the site's lock.
	stopsBefore := func(fn func()) {
		var once sync.Once
		f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
			if req.Name == "mysql" && strings.Contains(req.Stdin, "information_schema.tables") {
				once.Do(fn)
			}
			return my.hook(req)
		}
	}
	nothingMade := func(name, domain string, n int) {
		t.Helper()
		for _, tool := range f.agent.Tools() {
			if tool.Name == "tar" {
				t.Fatalf("%s: Roundcube was unpacked", name)
			}
		}
		for _, sql := range my.since(n) {
			if !strings.Contains(sql, "information_schema.tables") {
				t.Fatalf("%s: MySQL was changed: %q", name, sql)
			}
		}
		if _, err := f.db.GetDatabaseByName(f.ctx, "alex_roundcube"); err == nil {
			t.Fatalf("%s: the database was recorded", name)
		}
		if _, ok := f.agent.FileContaining("/var/www/alex/data/www/" + domain + "/config/config.inc.php"); ok {
			t.Fatalf("%s: config.inc.php was written", name)
		}
		if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" {
			t.Fatalf("%s: the mail settings name the webmail %q", name, c.Webmail)
		}
	}

	// The site removed while the job looked at the database.
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.settle()
	shop, _ := f.db.GetSiteByDomain(f.ctx, "shop.example.com")
	f.agent.Reset()
	n := my.batches()
	stopsBefore(func() {
		if err := f.db.SetSiteStatus(f.ctx, shop.ID, store.SiteDeleting, ""); err != nil {
			t.Error(err)
		}
	})
	job := f.webmailJob(map[string]any{"domain": "shop.example.com", "user": "alex"})
	if job.Status != store.JobFailed || job.Error != "site shop.example.com is being removed: the webmail is not installed into it" {
		t.Fatalf("a site being removed: %s %q", job.Status, job.Error)
	}
	nothingMade("a site being removed", "shop.example.com", n)

	// The owner removed while the job, having made the site, looked at the
	// database.
	f.agent.Reset()
	n = my.batches()
	stopsBefore(func() {
		if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
			t.Error(err)
		}
	})
	job = f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobFailed || job.Error != "the account alex is being removed: the webmail is not installed for it" {
		t.Fatalf("an account being removed: %s %q", job.Status, job.Error)
	}
	nothingMade("an account being removed", "webmail.example.com", n)
	made, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com")
	if err != nil {
		t.Fatalf("the site the run made: %v", err)
	}
	// Asked once more, the job stops before it makes anything.
	f.agent.Reset()
	n = my.batches()
	job = f.webmailJob(map[string]any{"domain": "mail2.example.com", "user": "alex"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "the account alex is being removed") {
		t.Fatalf("an account being removed, asked again: %s %q", job.Status, job.Error)
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "mail2.example.com"); err == nil {
		t.Fatal("a site was made for an account being removed")
	}
	if got := my.since(n); len(got) != 0 {
		t.Fatalf("MySQL was asked for an account being removed: %q", got)
	}
	// The account's removal takes the site the run made.
	del, err := f.s.jobs.Enqueue(f.ctx, "user.delete", userDeletePayload{UserID: alex.ID, Login: "alex", Purge: true}, jobs.WithLockKey("user:alex"))
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(del.ID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
	if _, err := f.db.GetSite(f.ctx, made.ID); err == nil {
		t.Fatal("the site the webmail made outlived its account")
	}
}

// A user.delete queued while the webmail job works in the account's site
// waits for it, at that site's lock, and then takes away all it made: the
// site, the database recorded as the account's (with its MySQL account),
// the webmail in the mail settings. The site.apply the webmail queued and
// that runs after ends done.
func TestUserDeleteTakesTheWebmailItWaitedFor(t *testing.T) {
	f, my, _ := newWebmailFixture(t)
	// The job is held as it unpacks, under the site's lock; the fake goes on
	// answering every other call meanwhile.
	atTar, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/tool" && strings.Contains(string(body), `"name":"tar"`) {
			once.Do(func() {
				close(atTar)
				<-release
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var install apitypes.JobRef
	f.call(http.MethodPost, "/mail/webmail", map[string]any{"domain": "webmail.example.com", "user": "alex"}, http.StatusAccepted, &install)
	select {
	case <-atTar:
	case <-time.After(20 * time.Second):
		close(release)
		t.Fatal("the webmail job did not get to the files")
	}
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	// The delete gets as far as the site and waits for its lock.
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := f.db.GetJob(f.ctx, del.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == store.JobRunning && j.Message == "sites" {
			break
		}
		if j.Status != store.JobQueued && j.Status != store.JobRunning || time.Now().After(deadline) {
			close(release)
			t.Fatalf("user.delete: %s %q %s", j.Status, j.Message, j.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com"); err != nil {
		close(release)
		t.Fatalf("the delete did not wait for the webmail: %v", err)
	}
	n := my.batches()
	close(release)
	if j := f.waitJob(install.JobID); j.Status != store.JobDone {
		t.Fatalf("the webmail job: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	f.settle()
	dropped := strings.Join(my.since(n), "\n")
	for _, want := range []string{"CREATE DATABASE IF NOT EXISTS `alex_roundcube`", "DROP DATABASE IF EXISTS `alex_roundcube`;", "DROP USER IF EXISTS 'alex_roundcube'@'localhost';"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("MySQL did not get %q:\n%s", want, dropped)
		}
	}
	if strings.Index(dropped, "DROP DATABASE") < strings.Index(dropped, "CREATE DATABASE") {
		t.Errorf("the database was dropped before the webmail made it:\n%s", dropped)
	}
	if _, err := f.db.GetDatabaseByName(f.ctx, "alex_roundcube"); err == nil {
		t.Error("the webmail's database is still recorded")
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com"); err == nil {
		t.Error("the webmail's site outlived its account")
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" || c.WebmailVersion != "" {
		t.Errorf("the mail settings still name the webmail: %q %q", c.Webmail, c.WebmailVersion)
	}
	list, _ := f.db.ListJobs(f.ctx, 500, "")
	for _, j := range list {
		if j.Type == "site.apply" && j.ID > install.JobID && j.Status != store.JobDone {
			t.Errorf("the webmail's apply %d: %s %s", j.ID, j.Status, j.Error)
		}
	}
}

// The removal of the webmail's site takes the webmail with it: the server
// block on its port goes in the same checked reload as the site's own, and
// the mail settings no longer name the site (the port stays chosen), so
// that mail.apply goes on working.
func TestWebmailGoesWithItsSite(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	block := f.s.webmailConfPath()
	if _, ok := f.agent.File(block); !ok {
		t.Fatal("the webmail's block on its port was not written")
	}
	from := len(f.agent.Calls())
	f.deleteSite("webmail.example.com")
	together := false
	for _, c := range f.agent.Calls()[from:] {
		var req agent.RemovePathsRequest
		if c.Path == "/v1/paths/remove" && json.Unmarshal(c.Body, &req) == nil {
			together = together || slices.Contains(req.Paths, block) && slices.Contains(req.Paths, "/etc/nginx/monopanel/sites/webmail.example.com.conf") && len(req.Validate) > 0
		}
	}
	if !together {
		t.Error("the webmail's block did not go with the site's, in one checked reload")
	}
	if _, ok := f.agent.File(block); ok {
		t.Fatal("the webmail's block outlived its site")
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" || c.WebmailVersion != "" || c.WebmailPort != 2096 {
		t.Fatalf("mail settings after the site went: webmail %q, version %q, port %d", c.Webmail, c.WebmailVersion, c.WebmailPort)
	}
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("mail.apply after the webmail's site went: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.File(block); ok {
		t.Fatal("mail.apply wrote the block of a webmail without a site")
	}
}

// An older panel removed the webmail's site and left the mail settings
// naming it: every mail change then failed on the webmail port. mail.apply
// now takes the site out of the settings and the block off the port. A site
// that is being removed keeps its block until its removal takes it.
func TestMailApplyForgetsAWebmailSiteThatIsGone(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	block := f.s.webmailConfPath()
	site, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	mailApply := func() *store.Job {
		t.Helper()
		var ref apitypes.JobRef
		f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
		return f.waitJob(ref.JobID)
	}

	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteDeleting, ""); err != nil {
		t.Fatal(err)
	}
	from := len(f.agent.Calls())
	if job := mailApply(); job.Status != store.JobDone || !strings.Contains(job.Log, "the webmail's site webmail.example.com is being removed") {
		t.Fatalf("mail.apply beside the removal: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, c := range f.agent.Calls()[from:] {
		if strings.Contains(string(c.Body), block) {
			t.Fatalf("mail.apply touched the block of a webmail being removed: %s %s", c.Path, c.Body)
		}
	}
	if _, ok := f.agent.File(block); !ok {
		t.Fatal("the block of a webmail being removed went before its removal")
	}

	if err := f.db.DeleteSite(f.ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if job := mailApply(); job.Status != store.JobDone {
		t.Fatalf("mail.apply with the webmail's site gone: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.File(block); ok {
		t.Fatal("the block of a webmail whose site is gone stays")
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" || c.WebmailPort != 2096 {
		t.Fatalf("mail settings: webmail %q, port %d", c.Webmail, c.WebmailPort)
	}
}

// The same for a webmail without a port: nothing failed there, but the
// settings went on naming the site an older panel removed, and the status
// linked to it. mail.apply takes it out of the settings as well.
func TestMailApplyForgetsAWebmailWithoutAPortWhoseSiteIsGone(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home)
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "webmail.example.com" || c.WebmailPort != 0 {
		t.Fatalf("mail settings after the install: webmail %q, port %d", c.Webmail, c.WebmailPort)
	}
	site, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.DeleteSite(f.ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone || !strings.Contains(job.Log, "webmail.example.com held the webmail: the mail settings no longer name it") {
		t.Fatalf("mail.apply with the webmail's site gone: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" || c.WebmailVersion != "" {
		t.Fatalf("mail settings: webmail %q, version %q", c.Webmail, c.WebmailVersion)
	}
	var status apitypes.MailStatus
	f.call(http.MethodGet, "/mail", nil, http.StatusOK, &status)
	if status.Webmail != "" || status.WebmailURL != "" {
		t.Fatalf("the status still links to the webmail: %q %q", status.Webmail, status.WebmailURL)
	}
}

// A mail change that read the mail settings before the webmail's site was
// removed does not write the webmail's block back after the removal took it
// away: the two take turns (webmailMu).
func TestWebmailBlockDoesNotOutliveItsSite(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	block := f.s.webmailConfPath()
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/config/apply" && strings.Contains(string(body), `"origin":"mail:webmail"`) {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var apply apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &apply)
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		close(release)
		t.Fatal("mail.apply did not get to the webmail's block")
	}
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/sites/webmail.example.com", nil, http.StatusAccepted, &del)
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := f.db.GetJob(f.ctx, del.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == store.JobRunning && j.Message == "removing configuration" || j.Status == store.JobDone || j.Status == store.JobFailed {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("site.delete: %s %q", j.Status, j.Message)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	if j := f.waitJob(apply.JobID); j.Status != store.JobDone {
		t.Fatalf("mail.apply: %s %s", j.Status, j.Error)
	}
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("site.delete: %s %s", j.Status, j.Error)
	}
	if conf, ok := f.agent.File(block); ok {
		t.Fatalf("the webmail's block outlived its site:\n%s", conf)
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" {
		t.Fatalf("the mail settings name the webmail %q", c.Webmail)
	}
}

// Every change of the mail settings goes in over the settings as they are
// by then. mail.apply that notices another dovecot (it asks the agent
// first) and an install run again (minutes of packages) read them before
// the webmail's site went and an administrator changed the message size:
// the webmail the site's removal took out stays out, the size stays. And
// mp mail settings asked for while the removal holds them goes in after it.
func TestMailSettingsKeepWhatChangedMeanwhile(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home)
	site, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// beside is the removal of the webmail's site under way — its server
	// block went and the settings no longer name it, the row is not gone
	// yet — and mp mail settings --max-size.
	beside := func(size int) {
		if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteDeleting, ""); err != nil {
			t.Error(err)
		}
		f.s.webmailMu.Lock()
		if err := f.s.forgetWebmail(f.ctx, func(string, ...any) {}, site.Domain); err != nil {
			t.Error(err)
		}
		f.s.webmailMu.Unlock()
		c := f.s.loadMailConfig(f.ctx)
		c.MaxSizeMB = size
		if err := f.s.saveMailConfig(f.ctx, c); err != nil {
			t.Error(err)
		}
	}
	reset := func() {
		t.Helper()
		f.settle()
		if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteActive, ""); err != nil {
			t.Fatal(err)
		}
		c := f.s.loadMailConfig(f.ctx)
		c.Webmail = site.Domain
		if err := f.s.saveMailConfig(f.ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what string, size int) mailConfig {
		t.Helper()
		c := f.s.loadMailConfig(f.ctx)
		if c.Webmail != "" || c.MaxSizeMB != size {
			t.Fatalf("%s put back what it read: webmail %q, max size %d MB (want none, %d)", what, c.Webmail, c.MaxSizeMB, size)
		}
		return c
	}

	// mail.apply after a release upgrade brought another dovecot.
	f.agent.PackageVersions = map[string]string{"dovecot-core": "1:2.3.21+dfsg1-3"}
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/pkg" && strings.Contains(string(body), `"action":"query","packages":["dovecot-core"]`) {
			once.Do(func() { beside(77) })
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone || !strings.Contains(job.Log, "dovecot 1.0-test → 1:2.3.21+dfsg1-3") {
		t.Fatalf("mail.apply: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if c := check("mail.apply", 77); c.DovecotVersion != "1:2.3.21+dfsg1-3" {
		t.Fatalf("the dovecot version: %q", c.DovecotVersion)
	}

	// mp mail install run again, as a repair.
	reset()
	var again sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/user/ensure" && strings.Contains(string(body), `"login":"vmail"`) {
			again.Do(func() { beside(99) })
		}
	})
	f.call(http.MethodPost, "/mail/install", map[string]any{"hostname": "mail.example.com"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("mail.install: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if c := check("mail.install", 99); !c.Installed || c.Hostname != "mail.example.com" || c.VmailUID == 0 {
		t.Fatalf("the install's own findings: installed %v, hostname %q, vmail uid %d", c.Installed, c.Hostname, c.VmailUID)
	}

	// mp mail settings asked for while the site's removal takes the webmail
	// out of the settings (it holds them, forgetWebmail): the request's
	// change goes in after it, over what it saved.
	reset()
	f.agent.SetBefore(nil)
	f.s.webmailMu.Lock()
	removal := f.s.loadMailConfig(f.ctx)
	type answer struct {
		status int
		body   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		status, body, err := f.send(http.MethodPut, "/mail/settings", map[string]any{"max_size_mb": 64})
		answered <- answer{status, body, err}
	}()
	time.Sleep(200 * time.Millisecond)
	removal.Webmail, removal.WebmailVersion = "", ""
	if err := f.s.saveMailConfig(f.ctx, removal); err != nil {
		t.Error(err)
	}
	f.s.webmailMu.Unlock()
	select {
	case a := <-answered:
		if a.err != nil || a.status != http.StatusOK {
			t.Fatalf("PUT /mail/settings: %d %s %v", a.status, a.body, a.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("PUT /mail/settings did not answer")
	}
	check("mp mail settings", 64)
}
