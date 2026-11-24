package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/phpmyadmin"
	"monopanel/internal/store"
)

// pmaMySQL plays MySQL behind phpMyAdmin's jobs: the root batches in order,
// and the temporary accounts mysql.user lists.
type pmaMySQL struct {
	mu    sync.Mutex
	root  []string
	users []string
}

var (
	pmaCreateRe = regexp.MustCompile(`CREATE USER '(mp\.pma\.[0-9a-f]{16})'@'localhost'`)
	pmaDropRe   = regexp.MustCompile(`DROP USER IF EXISTS '(mp\.pma\.[0-9a-f]{16})'@'localhost'`)
)

func (m *pmaMySQL) hook(req agent.ToolRequest) *agent.ToolResponse {
	if req.Name != "mysql" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.root = append(m.root, req.Stdin)
	if strings.Contains(req.Stdin, "FROM mysql.user WHERE user LIKE 'mp.pma.%'") {
		return &agent.ToolResponse{Output: strings.Join(m.users, "\n")}
	}
	for _, c := range pmaCreateRe.FindAllStringSubmatch(req.Stdin, -1) {
		m.users = append(m.users, c[1])
	}
	for _, d := range pmaDropRe.FindAllStringSubmatch(req.Stdin, -1) {
		m.users = slices.DeleteFunc(m.users, func(u string) bool { return u == d[1] })
	}
	return &agent.ToolResponse{}
}

func (m *pmaMySQL) batches() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.root)
}

func (m *pmaMySQL) accounts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.users)
}

// newPMAFixture is a panel with a database server, PHP 8.4, a stand-in
// phpMyAdmin release in the download directory and pmaMySQL behind the fake
// agent.
func newPMAFixture(t *testing.T) (*siteFixture, *pmaMySQL) {
	t.Helper()
	f := newSiteFixture(t)
	f.installDB()
	my := &pmaMySQL{}
	f.agent.ToolHook = my.hook
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "phpMyAdmin-" + phpmyadmin.Version + "-all-languages/"
	for _, file := range [][2]string{{"index.php", "<?php // phpMyAdmin\n"}, {"libraries/vendor_config.php", "<?php\n"}} {
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
	if err := os.WriteFile(filepath.Join(dir, phpmyadmin.Archive()), buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	prev := phpmyadmin.SHA256
	phpmyadmin.SHA256 = hex.EncodeToString(sum[:])
	t.Cleanup(func() { phpmyadmin.SHA256 = prev })
	return f, my
}

func (f *siteFixture) pmaJob(method string, body any) *store.Job {
	f.t.Helper()
	var ref apitypes.JobRef
	f.call(method, "/db/phpmyadmin", body, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

func (f *siteFixture) pmaInstalled(body map[string]any) {
	f.t.Helper()
	if job := f.pmaJob(http.MethodPost, body); job.Status != store.JobDone {
		f.t.Fatalf("phpmyadmin install: %s %s\n%s", job.Status, job.Error, job.Log)
	}
}

// pmaFiles is the last files call of the job.
func (f *siteFixture) pmaFiles() agent.PhpMyAdminRequest {
	f.t.Helper()
	var last agent.PhpMyAdminRequest
	for _, r := range f.agent.PhpMyAdmin() {
		if r.Action == "files" {
			last = r
		}
	}
	if last.Action == "" {
		f.t.Fatal("no files were written into the tree")
	}
	return last
}

var pmaSecretRe = regexp.MustCompile(`\$cfg\['blowfish_secret'\] = hex2bin\('([0-9a-f]{64})'\);`)

// The install puts the release into its own tree, writes its configuration
// (the login form over MySQL's socket with root refused, the panel's sign-on
// server), its pool as its own user and its server block on the port; a
// second run moves it to another port and keeps the secret.
func TestPMAInstallAndMove(t *testing.T) {
	f, _ := newPMAFixture(t)
	f.pmaInstalled(map[string]any{})
	tree := phpmyadmin.Tree(phpmyadmin.Version)
	untarred := false
	for _, tr := range f.agent.Tools() {
		if tr.Name == "tar" && slices.Contains(tr.Args, tree) && slices.Contains(tr.Args, "--no-same-owner") {
			untarred = true
		}
	}
	if !untarred {
		t.Fatalf("the release was not unpacked into %s: %v", tree, f.agent.Tools())
	}
	pool, ok := f.agent.File("/etc/php/8.4/fpm/pool.d/" + phpmyadmin.Pool + ".conf")
	if !ok || !strings.Contains(pool, "user  = "+phpmyadmin.User+"\n") || !strings.Contains(pool, "listen = "+path.Join(f.s.cfg.RunDir, "php", phpmyadmin.Socket)+"\n") ||
		!strings.Contains(pool, "open_basedir]       = "+tree+"/:") || !strings.Contains(pool, ",exec\n") {
		t.Fatalf("pool:\n%s", pool)
	}
	block, ok := f.agent.File(f.s.pmaNginxConf())
	if !ok || !strings.Contains(block, "listen 2083 ssl;") || !strings.Contains(block, "root  "+tree+";") || !strings.Contains(block, "fastcgi_pass unix:"+f.s.pmaSocket()+";") ||
		!strings.Contains(block, "ssl_certificate     "+f.s.cfg.TLSCertPath()+";") {
		t.Fatalf("server block:\n%s", block)
	}
	conf := f.pmaFiles().Config
	for _, want := range []string{
		"$cfg['Servers'][$i]['socket'] = '/var/run/mysqld/mysqld.sock';",
		"$cfg['Servers'][$i]['AllowRoot'] = false;",
		"$cfg['Servers'][$i]['auth_type'] = 'cookie';",
		"$cfg['Servers'][$i]['auth_type'] = 'signon';",
		"$cfg['TempDir'] = '" + phpmyadmin.StateDir + "/tmp';",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config.inc.php lacks %s", want)
		}
	}
	secret := pmaSecretRe.FindStringSubmatch(conf)
	if secret == nil {
		t.Fatalf("no blowfish_secret:\n%s", conf)
	}
	if signon := f.pmaFiles().Signon; !strings.Contains(signon, "const MP_TICKETS = '"+phpmyadmin.TicketDir()+"';") {
		t.Fatalf("sign-in script:\n%s", signon)
	}

	for _, port := range []int{80, 8443, 1000} {
		f.call(http.MethodPost, "/db/phpmyadmin", map[string]any{"port": port}, http.StatusUnprocessableEntity, nil)
	}
	f.pmaInstalled(map[string]any{"port": 2090})
	if block, _ := f.agent.File(f.s.pmaNginxConf()); !strings.Contains(block, "listen 2090 ssl;") {
		t.Fatalf("the port did not move:\n%s", block)
	}
	if again := pmaSecretRe.FindStringSubmatch(f.pmaFiles().Config); again == nil || again[1] != secret[1] {
		t.Fatal("blowfish_secret changed on a second run: everybody would be signed out")
	}
	var st apitypes.PhpMyAdminStatus
	f.call(http.MethodGet, "/db/phpmyadmin", nil, http.StatusOK, &st)
	if !st.Installed || st.Port != 2090 || st.Version != phpmyadmin.Version || st.PHPVersion != "8.4" || !strings.HasSuffix(st.URL, ":2090/") {
		t.Fatalf("status: %+v", st)
	}
}

// The button makes a temporary account granted on the one database asked
// for, of the caller's own, and hands it over a ticket named after the
// token's digest that lives a minute; another account's database is not
// found, and nothing is made for it.
func TestPMASignonGrantsOnlyThatDatabase(t *testing.T) {
	f, my := newPMAFixture(t)
	bob := f.userWithPassword("bob", "bob-password-1", 1600)
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	for _, d := range []*store.Database{{UserID: alex.ID, Name: "alex_shop"}, {UserID: bob.ID, Name: "bob_app"}} {
		if err := f.db.CreateDatabase(f.ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	f.call(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil, http.StatusConflict, nil)
	f.pmaInstalled(map[string]any{})
	f.loginAs("bob", "bob-password-1")
	n := len(my.batches())
	f.call(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil, http.StatusNotFound, nil)
	if len(my.batches()) != n {
		t.Fatalf("SQL ran for another account's database: %v", my.batches()[n:])
	}

	start := time.Now()
	var out apitypes.PhpMyAdminSignon
	f.call(http.MethodPost, "/databases/bob_app/phpmyadmin", nil, http.StatusOK, &out)
	u, err := url.Parse(out.URL)
	if err != nil || u.Scheme != "https" || u.Port() != "2083" || u.Path != "/"+phpmyadmin.SignonScript {
		t.Fatalf("url %q: %v", out.URL, err)
	}
	token := u.Query().Get("t")
	if len(token) != 43 || !phpmyadmin.TempUserRe.MatchString(out.User) || out.Database != "bob_app" {
		t.Fatalf("answer: %+v", out)
	}
	if out.Expires.Sub(start) > phpmyadmin.SessionTTL+time.Minute || out.Expires.Sub(start) < phpmyadmin.SessionTTL-time.Minute {
		t.Errorf("the account lives until %v", out.Expires)
	}
	sql := strings.Join(my.batches()[n:], "")
	if !strings.Contains(sql, "CREATE USER '"+out.User+"'@'localhost' IDENTIFIED WITH caching_sha2_password BY '") ||
		strings.Count(sql, "GRANT ") != 1 || !strings.Contains(sql, "GRANT ALL PRIVILEGES ON `bob\\_app`.* TO '"+out.User+"'@'localhost';") {
		t.Fatalf("SQL:\n%s", sql)
	}
	sum := sha256.Sum256([]byte(token))
	name := hex.EncodeToString(sum[:])
	spec, ok := f.agent.FileSpec(path.Join(phpmyadmin.TicketDir(), name))
	if !ok || spec.Mode != 0o400 || spec.Owner != phpmyadmin.User {
		t.Fatalf("ticket %s: %+v %v (files %v)", name, spec, ok, f.agent.Files())
	}
	var ticket struct {
		Hash, User, Password, DB string
		Expires                  int64
	}
	if err := json.Unmarshal([]byte(spec.Content), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Hash != name || ticket.User != out.User || ticket.DB != "bob_app" || len(ticket.Password) < 20 || !strings.Contains(sql, ticket.Password) {
		t.Fatalf("ticket: %+v", ticket)
	}
	if left := time.Until(time.Unix(ticket.Expires, 0)); left > phpmyadmin.TicketTTL || left < phpmyadmin.TicketTTL-10*time.Second {
		t.Errorf("the ticket lives %v", left)
	}
	if strings.Contains(out.URL, ticket.Password) {
		t.Error("the password travels in the address")
	}
	if list := f.s.loadPMATemp(f.ctx); len(list) != 1 || list[0].User != out.User || list[0].Actor != "bob" {
		t.Fatalf("recorded: %+v", list)
	}
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Limit: 20})
	audited := false
	for _, e := range entries {
		audited = audited || (e.Action == "db.phpmyadmin" && e.Target == "bob_app" && e.Actor == "bob")
	}
	if !audited {
		t.Error("no audit entry")
	}
}

// The sweep drops the accounts that expired, those whose session signed out
// and those MySQL has that the panel does not know; a live one stays.
func TestPMASweep(t *testing.T) {
	f, my := newPMAFixture(t)
	f.pmaInstalled(map[string]any{})
	const expired, live, out, stray = "mp.pma.00000000000000e1", "mp.pma.00000000000000a1", "mp.pma.00000000000000b1", "mp.pma.00000000000000c1"
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_shop"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := f.s.savePMATemp(f.ctx, []pmaTemp{
		{User: expired, Database: "alex_shop", Expires: now.Add(-time.Minute)},
		{User: live, Database: "alex_shop", Expires: now.Add(time.Hour)},
		{User: out, Database: "alex_shop", Expires: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	my.mu.Lock()
	my.users = []string{expired, live, out, stray}
	my.mu.Unlock()
	f.agent.PMALogouts = []string{out}
	f.s.pmaSweep(f.ctx)
	if got := my.accounts(); !slices.Equal(got, []string{live}) {
		t.Fatalf("MySQL keeps %v, want only %s", got, live)
	}
	if list := f.s.loadPMATemp(f.ctx); len(list) != 1 || list[0].User != live {
		t.Fatalf("recorded: %+v", list)
	}
}

// The removal takes everything away: the temporary accounts, the server
// block, the pool, the tree, the state and the tickets; the button answers
// that phpMyAdmin is not there.
func TestPMARemove(t *testing.T) {
	f, my := newPMAFixture(t)
	f.pmaInstalled(map[string]any{})
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_shop"}); err != nil {
		t.Fatal(err)
	}
	var signon apitypes.PhpMyAdminSignon
	f.call(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil, http.StatusOK, &signon)
	if job := f.pmaJob(http.MethodDelete, nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if left := my.accounts(); len(left) != 0 {
		t.Fatalf("temporary accounts left: %v", left)
	}
	removed := f.agent.Removed()
	for _, p := range []string{f.s.pmaNginxConf(), "/etc/php/8.4/fpm/pool.d/" + phpmyadmin.Pool + ".conf", phpmyadmin.Root, phpmyadmin.StateDir, phpmyadmin.RunDir} {
		if !slices.Contains(removed, p) {
			t.Errorf("%s was not removed (%v)", p, removed)
		}
	}
	var st apitypes.PhpMyAdminStatus
	f.call(http.MethodGet, "/db/phpmyadmin", nil, http.StatusOK, &st)
	if st.Installed || st.Port != 0 {
		t.Fatalf("status after the removal: %+v", st)
	}
	f.call(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil, http.StatusConflict, nil)
	if v, err := f.db.GetSetting(f.ctx, settingPMASecret); err == nil && v != "" {
		t.Error("the secret stayed")
	}
}

// A panel that pins a newer release moves an older install to it at its
// start, keeping the port, and the tree of the older one goes.
func TestPMAUpgradeAtStart(t *testing.T) {
	f, _ := newPMAFixture(t)
	f.pmaInstalled(map[string]any{"port": 2091})
	if err := f.db.SetSetting(f.ctx, settingPMAVersion, "5.2.2"); err != nil {
		t.Fatal(err)
	}
	before := len(f.agent.PhpMyAdmin())
	f.s.refreshPHPMyAdmin(f.ctx)
	job := f.waitJobOfType("phpmyadmin.install")
	if job.Payload == nil || !strings.Contains(string(job.Payload), "upgrade") {
		t.Fatalf("no upgrade job: %+v", job)
	}
	if job.Status != store.JobDone {
		t.Fatalf("upgrade: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if st := f.s.pmaState(f.ctx); st.Version != phpmyadmin.Version || st.Port != 2091 {
		t.Fatalf("after the upgrade: %+v", st)
	}
	pruned := false
	for _, r := range f.agent.PhpMyAdmin()[before:] {
		pruned = pruned || (r.Action == "prune" && r.Version == phpmyadmin.Version)
	}
	if !pruned || !strings.Contains(job.Log, "phpMyAdmin 5.2.2 → "+phpmyadmin.Version) {
		t.Fatalf("pruned %v, log:\n%s", pruned, job.Log)
	}
}

// A ticket the agent could not write leaves no account behind: the button
// answers the failure, and the temporary account it made goes at once.
func TestPMASignonWithoutTicketDropsTheAccount(t *testing.T) {
	f, my := newPMAFixture(t)
	f.pmaInstalled(map[string]any{})
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_shop"}); err != nil {
		t.Fatal(err)
	}
	f.agent.Fail = map[string]string{"/v1/phpmyadmin": "tickets: read-only file system"}
	code, body, err := f.send(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil)
	if err != nil || code < 500 {
		t.Fatalf("the button answered %d %s %v", code, body, err)
	}
	if left := my.accounts(); len(left) != 0 {
		t.Fatalf("accounts left without a ticket: %v", left)
	}
	if list := f.s.loadPMATemp(f.ctx); len(list) != 0 {
		t.Fatalf("recorded: %+v", list)
	}
}

// pmaBob is bob, signed in, with the database bob_app (or the one named),
// and phpMyAdmin installed.
func pmaBob(t *testing.T, name string) (*siteFixture, *pmaMySQL, *store.User) {
	t.Helper()
	f, my := newPMAFixture(t)
	bob := f.userWithPassword("bob", "bob-password-1", 1600)
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: bob.ID, Name: name}); err != nil {
		t.Fatal(err)
	}
	f.pmaInstalled(map[string]any{})
	f.loginAs("bob", "bob-password-1")
	return f, my, bob
}

// The button's grant names its database literally: in a database-level
// GRANT _ and % are wildcards, and a database "_____" (a migration may
// bring one) would otherwise open `mysql` itself (review R1-1).
func TestPMASignonGrantIsLiteral(t *testing.T) {
	f, my, _ := pmaBob(t, "_____")
	n := len(my.batches())
	var out apitypes.PhpMyAdminSignon
	f.call(http.MethodPost, "/databases/_____/phpmyadmin", nil, http.StatusOK, &out)
	sql := strings.Join(my.batches()[n:], "")
	if !strings.Contains(sql, "GRANT ALL PRIVILEGES ON `\\_\\_\\_\\_\\_`.* TO '"+out.User+"'@'localhost';") {
		t.Fatalf("the grant is a pattern (it matches `mysql`):\n%s", sql)
	}
}

// Deleting a database, or the account it belongs to, drops the button's
// temporary accounts on it: DROP DATABASE keeps their grants, and a
// database of that name made later would be open to them (review R1-2).
func TestPMADatabaseDeleteDropsTemporaryAccounts(t *testing.T) {
	f, my, bob := pmaBob(t, "bob_app")
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: bob.ID, Name: "bob_blog"}); err != nil {
		t.Fatal(err)
	}
	var app, blog, other apitypes.PhpMyAdminSignon
	f.call(http.MethodPost, "/databases/bob_app/phpmyadmin", nil, http.StatusOK, &app)
	f.call(http.MethodPost, "/databases/bob_blog/phpmyadmin", nil, http.StatusOK, &blog)
	f.login()
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_shop"}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/databases/alex_shop/phpmyadmin", nil, http.StatusOK, &other)

	f.call(http.MethodDelete, "/databases/bob_app", nil, http.StatusNoContent, nil)
	if got := my.accounts(); slices.Contains(got, app.User) || !slices.Contains(got, blog.User) {
		t.Fatalf("bob_app deleted: MySQL keeps %v (bob_app's %s, bob_blog's %s)", got, app.User, blog.User)
	}
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/bob", nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("bob's removal: %s %s", job.Status, job.Error)
	}
	if got := my.accounts(); !slices.Equal(got, []string{other.User}) {
		t.Fatalf("bob deleted: MySQL keeps %v, want only alex_shop's %s", got, other.User)
	}
	if list := f.s.loadPMATemp(f.ctx); len(list) != 1 || list[0].User != other.User {
		t.Fatalf("recorded: %+v", list)
	}
}

// The sweep drops an account recorded on a database that is no longer
// there (a drop that failed at the deletion).
func TestPMASweepDropsAccountsOfDeletedDatabases(t *testing.T) {
	f, my, _ := pmaBob(t, "bob_app")
	const gone = "mp.pma.00000000000000d1"
	if err := f.s.savePMATemp(f.ctx, []pmaTemp{{User: gone, Database: "bob_old", Actor: "bob", Expires: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	my.mu.Lock()
	my.users = []string{gone}
	my.mu.Unlock()
	f.s.pmaSweep(f.ctx)
	if got := my.accounts(); len(got) != 0 {
		t.Fatalf("MySQL keeps %v", got)
	}
}

// No database user of the panel has the shape of a temporary account: the
// account "mp" with the database pma_0123456789abcdef has the user
// mp_pma_0123456789abcdef, which the sweep leaves alone (review R1-3).
func TestPMASweepKeepsAccountsDatabaseUsers(t *testing.T) {
	f, my := newPMAFixture(t)
	mp := f.userWithPassword("mp", "mp-password-1", 1601)
	f.pmaInstalled(map[string]any{})
	f.loginAs("mp", "mp-password-1")
	var made apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "pma_0123456789abcdef"}, http.StatusCreated, &made)
	name := made.Database.Name
	if name != "mp_pma_0123456789abcdef" || phpmyadmin.TempUserRe.MatchString(name) {
		t.Fatalf("database %q of %s", name, mp.Login)
	}
	my.mu.Lock()
	my.users = append(my.users, name) // as if mysql.user listed it too
	my.mu.Unlock()
	f.s.pmaSweep(f.ctx)
	for _, b := range my.batches() {
		if strings.Contains(b, "DROP USER IF EXISTS '"+name+"'") {
			t.Fatalf("the sweep dropped the database user %s:\n%s", name, b)
		}
	}
}

// A temporary account MySQL still has once phpMyAdmin is gone (a press
// that raced the removal) is dropped by the next sweep (review R1-4).
func TestPMASweepAfterRemoveDropsStrays(t *testing.T) {
	f, my := newPMAFixture(t)
	f.pmaInstalled(map[string]any{})
	if job := f.pmaJob(http.MethodDelete, nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s", job.Status, job.Error)
	}
	const stray = "mp.pma.00000000000000f1"
	my.mu.Lock()
	my.users = append(my.users, stray)
	my.mu.Unlock()
	f.s.pmaSweep(f.ctx)
	if slices.Contains(my.accounts(), stray) {
		t.Fatalf("the stray %s stays after the removal", stray)
	}
}

// A press while the removal runs (review R1-4) is refused: its first step
// marks phpMyAdmin not installed under the gate a press holds, so no
// account is made after the removal dropped them.
func TestPMARemoveRefusesTheButtonMeanwhile(t *testing.T) {
	f, my, _ := pmaBob(t, "bob_app")
	f.login()
	var status atomic.Int32
	f.agent.SetBefore(func(p string, _ []byte) {
		// The server block goes after the accounts were dropped.
		if p == "/v1/paths/remove" && status.Load() == 0 {
			code, _ := f.do(http.MethodPost, "/databases/bob_app/phpmyadmin", nil)
			status.Store(int32(code))
		}
	})
	if job := f.pmaJob(http.MethodDelete, nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s", job.Status, job.Error)
	}
	f.agent.SetBefore(nil)
	if status.Load() != http.StatusConflict {
		t.Fatalf("a press during the removal: %d", status.Load())
	}
	if got := my.accounts(); len(got) != 0 {
		t.Fatalf("MySQL keeps %v after the removal", got)
	}
}

// The button is bounded: a press past pmaPerDatabase live accounts of the
// same actor on the same database drops the oldest (review R1-5), and an
// account whose ticket nobody took goes with the sweep that finds the
// ticket past its time.
func TestPMAButtonIsBounded(t *testing.T) {
	f, my, _ := pmaBob(t, "bob_app")
	var last apitypes.PhpMyAdminSignon
	for range 40 {
		f.call(http.MethodPost, "/databases/bob_app/phpmyadmin", nil, http.StatusOK, &last)
	}
	got := my.accounts()
	if len(got) != pmaPerDatabase || !slices.Contains(got, last.User) {
		t.Fatalf("40 presses leave %d live MySQL accounts %v (the last %s)", len(got), got, last.User)
	}
	if list := f.s.loadPMATemp(f.ctx); len(list) != pmaPerDatabase {
		t.Fatalf("recorded: %d", len(list))
	}

	// Of two fresh presses, the one whose ticket the sweep finds unused
	// goes; the other (signed in: its ticket was taken) stays.
	list := f.s.loadPMATemp(f.ctx)
	unused, taken := list[len(list)-2], list[len(list)-1]
	f.agent.PMAOldTickets = []string{unused.Ticket}
	f.s.pmaSweep(f.ctx)
	got = my.accounts()
	if slices.Contains(got, unused.User) || !slices.Contains(got, taken.User) {
		t.Fatalf("after the sweep MySQL keeps %v (unused %s, taken %s)", got, unused.User, taken.User)
	}
}
