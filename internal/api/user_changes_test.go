package api

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
	"monopanel/internal/store"
)

// panelHost is what two starts of the panel share — the database, the fake
// agent and the configuration — with an administrator, alex (active, its
// unix user made) and PHP 8.4.
type panelHost struct {
	cfg   config.Config
	db    *store.DB
	agent *agenttest.Agent
}

func newPanelHost(t *testing.T) *panelHost {
	t.Helper()
	ctx := context.Background()
	fake := agenttest.Start(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.RunDir = t.TempDir()
	cfg.SecretKeyFile = filepath.Join(cfg.DataDir, "secret.key")
	if err := os.WriteFile(cfg.SecretKeyFile, []byte(strings.Repeat("k", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h, _ := auth.HashPassword("secret-password")
	if err := db.CreateUser(ctx, &store.User{Login: "admin", Role: store.RoleAdmin, PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	uid, gid := 1500, 1500
	if err := db.CreateUser(ctx, &store.User{Login: "alex", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/alex", Shell: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPHPVersion(ctx, &store.PHPVersion{Version: "8.4", Source: "sury", Status: store.PHPInstalled}); err != nil {
		t.Fatal(err)
	}
	return &panelHost{cfg: cfg, db: db, agent: fake}
}

// withWordPress gives the host a database server and has a CMS install
// download WordPress and wp-cli from the test, as cmsFixture does.
func (h *panelHost) withWordPress(t *testing.T) {
	t.Helper()
	if err := h.db.UpsertDBInstance(context.Background(), &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	wordpressSources(t)
	h.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		out := func(s string) *agent.RunAsUserResponse {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(s))}
		}
		switch a := strings.Join(req.Args, " "); {
		case req.Args[0] == "list":
			return out("[]")
		case strings.Contains(a, "core version"):
			return out("7.1\n")
		}
		return nil
	}
}

// wordpressSources has a CMS install download WordPress and wp-cli from
// the test.
func wordpressSources(t *testing.T) {
	t.Helper()
	wp := tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})
	phar := []byte("<?php // wp-cli " + strings.Repeat("x", 2<<20))
	sum := sha512.Sum512(phar)
	prevF, prevB, prevT := cmsFetch, cmsFetchBytes, cmsLatestTag
	cmsFetch = func(_ context.Context, url string) (io.ReadCloser, error) {
		if url != "https://wordpress.org/latest.tar.gz" {
			return nil, fmt.Errorf("no test source for %s", url)
		}
		return io.NopCloser(bytes.NewReader(wp)), nil
	}
	cmsFetchBytes = func(_ context.Context, url string, _ int64) ([]byte, error) {
		switch url {
		case wpCLIURL:
			return phar, nil
		case wpCLIURL + ".sha512":
			return []byte(hex.EncodeToString(sum[:]) + "  wp-cli.phar\n"), nil
		}
		return nil, fmt.Errorf("no test source for %s", url)
	}
	cmsLatestTag = func(context.Context, string) (string, error) { return "", nil }
	t.Cleanup(func() { cmsFetch, cmsFetchBytes, cmsLatestTag = prevF, prevB, prevT })
}

// start boots a panel on the host as `monopanel api` does (cli/daemon.go):
// two job workers on a context of their own; stop cancels it and waits for
// the workers, as systemctl stop or restart of monopanel-api does. Another
// start on the same host is the next process, which queues again the jobs
// the stopped one left running.
func (h *panelHost) start(t *testing.T) (f *siteFixture, stop func()) {
	t.Helper()
	rel, _ := osprofile.ParseOSRelease(strings.NewReader(fixtureOSRelease))
	profile, _ := osprofile.FromRelease(rel)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := jobs.NewRunner(h.db, 2, quiet)
	s := New(h.cfg, h.db, h.agent.Client(), runner, profile, quiet)
	s.SetReadinessWaits(0, 0)
	s.SetHostIPv6s(func() []string { return nil })
	s.SetUnprivilegedPortStart(func() int { return 1024 })
	rctx, cancel := context.WithCancel(context.Background())
	runner.Start(rctx)
	ts := httptest.NewServer(s.Handler())
	f = &siteFixture{t: t, s: s, db: h.db, agent: h.agent, ts: ts, ctx: context.Background()}
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); runner.Wait() }) }
	t.Cleanup(func() { stop(); ts.Close() })
	f.login()
	return f, stop
}

// A user.delete broken off by a shutdown — systemctl restart monopanel-api,
// mp update, a reboot — is not a failure: the runner leaves it running and
// the next start queues it again. The account keeps its mark meanwhile and
// through the run after the start: an in-place restore into its home and a
// change of its sites are refused, and the log rotation that run renders
// drops the account before its unix user goes.
func TestUserDeleteCutByAShutdownKeepsTheMark(t *testing.T) {
	h := newPanelHost(t)
	f, stop := h.start(t)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})

	rm := f.removalHeld(nil)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	rm.wait(t, "the user.delete")
	stop()

	j, err := h.db.GetJob(f.ctx, del.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != store.JobRunning || !strings.Contains(j.Log, "interrupted by shutdown") {
		t.Fatalf("the user.delete after the shutdown: %s %q\n%s", j.Status, j.Error, j.Log)
	}
	f.keepsOut(t, "after the shutdown, the removal to run again at the start")
	if code, raw := f.do(http.MethodPatch, "/sites/a.example.com", map[string]any{"client_max_body": "128m"}); code != http.StatusConflict {
		t.Errorf("mp site set on a site of alex, whose removal runs again at the start: %d %s (want 409)", code, raw)
	}

	// The next start: the removal runs again, held at the same step.
	rm2 := newCallGate()
	h.agent.SetBefore(func(p string, b []byte) {
		if p == "/v1/paths/remove" && strings.Contains(string(b), "a.example.com.conf") {
			rm2.hit()
		}
	})
	t.Cleanup(rm2.open)
	rm.open()
	f2, _ := h.start(t)
	rm2.wait(t, "the user.delete run again")
	f2.keepsOut(t, "during the removal's run after the start")
	rm2.open()
	if dj := f2.waitJob(del.JobID); dj.Status != store.JobDone {
		t.Fatalf("the user.delete run again: %s %s", dj.Status, dj.Error)
	}
	// user.delete renders the rotation before it removes the unix user, for
	// an account being removed to drop out: logrotate must never meet a user
	// that no longer exists.
	if conf, ok := h.agent.File(siteLogrotatePath); ok && strings.Contains(conf, "alex") {
		t.Errorf("after the removal ran again, %s still names alex:\n%s", siteLogrotatePath, conf)
	}
}

// The panel stopped while a user.delete waits at a site's lock for a CMS
// install in that site — it waits there for the install's whole run. The
// stop cancels both; the removal keeps the account's mark for its run after
// the start: an in-place restore into the home is refused meanwhile, the
// install queued again makes nothing in the account being removed, and the
// rotation drops the account.
func TestShutdownWhileUserDeleteWaitsForACMSInstall(t *testing.T) {
	h := newPanelHost(t)
	h.withWordPress(t)
	f, stop := h.start(t)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})

	// The install is held at its first look at the docroot: under the
	// site's lock, before the database and the unpack.
	held, release := make(chan struct{}), make(chan struct{})
	var once, opened sync.Once
	open := func() { opened.Do(func() { close(release) }) }
	h.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/runas" && strings.Contains(string(body), `"list"`) {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() { open(); h.agent.SetBefore(nil) })
	var res apitypes.CMSInstallResult
	f.call(http.MethodPost, "/sites/wp.example.com/cms", map[string]any{"cms": "wordpress"}, http.StatusAccepted, &res)
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("the install did not reach the docroot")
	}
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if j, _ := h.db.GetJob(f.ctx, del.JobID); j != nil && j.Status == store.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user.delete did not start")
		}
	}
	time.Sleep(300 * time.Millisecond) // to the site's lock: database reads only before it

	stop()
	open()
	h.agent.SetBefore(nil)
	if dj, _ := h.db.GetJob(f.ctx, del.JobID); dj.Status != store.JobRunning {
		t.Fatalf("user.delete: %s %q (want running: broken off, queued again at the start)\n%s", dj.Status, dj.Error, dj.Log)
	}
	f.keepsOut(t, "after the shutdown, the removal to run again at the start")

	n := len(h.agent.Calls())
	f2, _ := h.start(t)
	cj, dj := f2.waitJob(res.JobID), f2.waitJob(del.JobID)
	if dj.Status != store.JobDone {
		t.Fatalf("user.delete after the start: %s %q", dj.Status, dj.Error)
	}
	if made := f2.cmsMadeSince(n); len(made) > 0 || cj.Status != store.JobFailed {
		t.Errorf("the CMS install queued again: %s %q, made in the account being removed: %v", cj.Status, cj.Error, made)
	}
	if conf, ok := h.agent.File(siteLogrotatePath); ok && strings.Contains(conf, "alex") {
		t.Errorf("after alex's removal the site log rotation still names alex:\n%s", conf)
	}
}

// Asking for an account's removal again — while the first still waits
// behind another job of the account — ends done: the second finds the
// account gone, and nothing fails for the doctor to count. A removal or a
// provision whose account id another account took meanwhile is not that
// account's either: SQLite gives the newest row's id to the next row once
// that row is gone.
func TestUserRemovalAskedAgainEndsDone(t *testing.T) {
	f := newSiteFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	release := f.holdKey("user:alex")
	var first, second apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &first)
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &second)
	release()
	if j := f.waitJob(first.JobID); j.Status != store.JobDone {
		t.Fatalf("the first removal: %s %s", j.Status, j.Error)
	}
	if j := f.waitJob(second.JobID); j.Status != store.JobDone || j.Message != "the account alex is gone already: nothing to remove" {
		t.Errorf("the removal asked again: %s %q %q", j.Status, j.Error, j.Message)
	}
	failed, _ := f.db.ListJobs(f.ctx, 200, store.JobFailed)
	done, _ := f.db.ListJobs(f.ctx, 500, store.JobDone)
	if n := openFailures(failed, done, time.Now()); n > 0 {
		t.Errorf("the doctor's jobs line: %d failed in the last 24h", n)
	}

	bob := f.userWithPassword("bob", "bob-password", 1501)
	if bob.ID != alex.ID {
		t.Fatalf("bob has id %d, alex had %d: the id was not given again", bob.ID, alex.ID)
	}
	n := len(f.agent.Calls())
	stale, err := f.s.jobs.Enqueue(f.ctx, "user.delete", userDeletePayload{UserID: alex.ID, Login: "alex", Purge: true}, jobs.WithLockKey("user:alex"))
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(stale.ID); j.Status != store.JobDone || j.Message != "the account alex is gone already: nothing to remove" {
		t.Errorf("a removal of alex on alex's old id, bob's now: %s %q %q", j.Status, j.Error, j.Message)
	}
	if _, err := f.db.GetUserByLogin(f.ctx, "bob"); err != nil {
		t.Errorf("bob after a removal of alex: %v", err)
	}
	// A provision queued for alex, on that id too.
	prov, err := f.s.jobs.Enqueue(f.ctx, "user.provision", userProvisionPayload{UserID: alex.ID, Login: "alex", Shell: true}, jobs.WithLockKey("user:alex"))
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(prov.ID); j.Status != store.JobDone || j.Message != "the account is gone: nothing to provision" {
		t.Errorf("a provision of alex on alex's old id, bob's now: %s %q %q", j.Status, j.Error, j.Message)
	}
	if got, _ := f.db.GetUserByLogin(f.ctx, "bob"); got == nil || got.UnixUID == nil || *got.UnixUID != 1501 {
		t.Errorf("bob's unix identity after a provision of alex: %+v", got)
	}
	for _, c := range f.agent.Calls()[n:] {
		if c.Path == "/v1/user/remove" || c.Path == "/v1/user/ensure" {
			t.Errorf("a job of alex on bob's id changed a unix user: %s %s", c.Path, c.Body)
		}
	}
}

// A change of an account that went in before the account's removal was
// asked for — the PATCH held at its agent call for the unix password, which
// comes after its write — is answered as accepted, its password set: the
// removal comes after it and takes the account. The mark stays.
func TestAccountChangeBeforeItsRemovalIsAccepted(t *testing.T) {
	f, _, _, _ := restoreFixture(t)
	pw := newCallGate()
	t.Cleanup(pw.open)
	rm := f.removalHeld(func(p string, body []byte) {
		if p == "/v1/user/password" && strings.Contains(string(body), `"login":"alex"`) {
			pw.hit()
		}
	})
	patched := make(chan sent, 1)
	go f.sendBeside(patched, http.MethodPatch, "/users/alex", map[string]any{"password": "another-password-1"})
	pw.wait(t, "the PATCH")
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	rm.wait(t, "the user.delete")
	pw.open()
	a := answerOf(t, patched)
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	changed, _ := auth.VerifyPassword("another-password-1", alex.PasswordHash)
	if a.err != nil || a.status != http.StatusOK || !changed {
		t.Errorf("the PATCH answered %d %s %v; the panel password changed %v", a.status, strings.TrimSpace(a.body), a.err, changed)
	}
	f.keepsOut(t, "a PATCH /users/alex that went in before the removal")
	rm.open()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
}

// A change of an account writes what it sets and nothing else. A password
// change that read alex before an administrator suspended alex — the
// suspension answered 200 and ended the sessions — does not put 'active'
// back; one that read carol pending does not undo the activation her
// provision made meanwhile.
func TestAccountChangeKeepsAStatusSetMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	beside := make(chan sent, 1)
	var fired atomic.Bool
	f.s.beforeChangeWrite = func() {
		if fired.CompareAndSwap(false, true) {
			f.sendBeside(beside, http.MethodPatch, "/users/alex", map[string]any{"status": "suspended"})
		}
	}
	t.Cleanup(func() { f.s.beforeChangeWrite = nil })
	var out apitypes.UserWithJob
	f.call(http.MethodPatch, "/users/alex", map[string]any{"password": "another-password-1"}, http.StatusOK, &out)
	if r := answerOf(t, beside); r.err != nil || r.status != http.StatusOK {
		t.Fatalf("the suspension beside: %d %v %s", r.status, r.err, r.body)
	}
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if changed, _ := auth.VerifyPassword("another-password-1", alex.PasswordHash); alex.Status != store.UserSuspended || !changed {
		t.Errorf("after the password change that read alex before the suspension: %s, password changed %v", alex.Status, changed)
	}
	// The answer is the account as stored.
	if out.User == nil || out.User.Status != store.UserSuspended {
		t.Errorf("the answer: %+v", out.User)
	}

	release := f.holdKey("user:carol")
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol", "password": "carol-password-1"}, http.StatusAccepted, &created)
	reached, goOn := make(chan struct{}), make(chan struct{})
	fired.Store(false)
	f.s.beforeChangeWrite = func() {
		if fired.CompareAndSwap(false, true) {
			close(reached)
			<-goOn
		}
	}
	proceed := sync.OnceFunc(func() { close(goOn) })
	t.Cleanup(proceed)
	patched := make(chan sent, 1)
	go f.sendBeside(patched, http.MethodPatch, "/users/carol", map[string]any{"password": "carol-password-2"})
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the password change of carol did not reach its write")
	}
	release()
	if j := f.waitJob(created.JobID); j.Status != store.JobDone {
		t.Fatalf("carol's provision: %s %s", j.Status, j.Error)
	}
	proceed()
	if r := answerOf(t, patched); r.err != nil || r.status != http.StatusOK {
		t.Fatalf("the password change of carol: %d %v %s", r.status, r.err, r.body)
	}
	if carol, _ := f.db.GetUserByLogin(f.ctx, "carol"); carol.Status != store.UserActive {
		t.Errorf("carol after a password change that read her pending: %s", carol.Status)
	}
}

// A provision does not lift a suspension. The shell of a suspended account
// switched — its provision runs — leaves it suspended: in the store, and so
// out of the panel's sign-in and off the local socket. On the host the
// provision makes the switch asked for and leaves the rest of the account as
// the suspension left it: its unix password, its sites, its cron. A
// suspension asked for together with the switch ends suspended too, and an
// account made pending still becomes active.
func TestProvisionKeepsASuspension(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "s.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.alexWithPassword()
	f.call(http.MethodPatch, "/users/alex", map[string]any{"status": "suspended"}, http.StatusOK, nil)
	n := len(f.agent.Calls())
	var prov apitypes.UserWithJob
	f.call(http.MethodPatch, "/users/alex", map[string]any{"shell": false}, http.StatusAccepted, &prov)
	j := f.waitJob(prov.JobID)
	if j.Status != store.JobDone {
		t.Fatalf("the provision: %s %s", j.Status, j.Error)
	}
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if alex.Status != store.UserSuspended || alex.Shell {
		t.Errorf("after the provision of a suspended alex: %s, shell %v\n%s", alex.Status, alex.Shell, j.Log)
	}
	res, err := http.Post(f.ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"login":"alex","password":"alex-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the suspended alex signs in after the provision: %d", res.StatusCode)
	}
	if alex.UnixUID != nil {
		local := peercred.WithCred(f.ctx, &peercred.Cred{UID: uint32(*alex.UnixUID)})
		if p := f.s.authenticateWith(local, "", func() string { return "" }); p != nil {
			t.Errorf("the local socket lets the suspended alex in after the provision: %+v", p.Principal)
		}
	}
	switched := false
	for _, c := range f.agent.Calls()[n:] {
		b := string(c.Body)
		switch {
		case c.Path == "/v1/user/ensure" && strings.Contains(b, `"login":"alex"`):
			switched = strings.Contains(b, `"shell":"/usr/sbin/nologin"`) && strings.Contains(b, sftpGroup)
		case c.Path == "/v1/user/password", c.Path == "/v1/user/shadow":
			t.Errorf("the provision of a suspended account set its unix password: %s %s", c.Path, b)
		case strings.Contains(b, "s.example.com.conf"), strings.Contains(b, `"crontab"`):
			t.Errorf("the provision of a suspended account touched its site or its cron: %s %s", c.Path, b)
		}
	}
	if !switched {
		t.Error("the provision did not switch alex to SFTP only")
	}

	// A suspension asked for together with a switch of the shell.
	f.call(http.MethodPatch, "/users/alex", map[string]any{"status": "active"}, http.StatusOK, nil)
	f.call(http.MethodPatch, "/users/alex", map[string]any{"shell": true, "status": "suspended"}, http.StatusAccepted, &prov)
	if j := f.waitJob(prov.JobID); j.Status != store.JobDone {
		t.Fatalf("the provision: %s %s", j.Status, j.Error)
	}
	if alex, _ := f.db.GetUserByLogin(f.ctx, "alex"); alex.Status != store.UserSuspended || !alex.Shell {
		t.Errorf("after --shell with --status suspended: %s, shell %v", alex.Status, alex.Shell)
	}

	// An account made pending becomes active once provisioned.
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol", "password": "carol-password-1"}, http.StatusAccepted, &created)
	if j := f.waitJob(created.JobID); j.Status != store.JobDone {
		t.Fatalf("carol's provision: %s %s", j.Status, j.Error)
	}
	if carol, _ := f.db.GetUserByLogin(f.ctx, "carol"); carol.Status != store.UserActive {
		t.Errorf("carol after her provision: %s", carol.Status)
	}
}

// An account a move brings in becomes active with its provision, as one
// made here does, and the move's end changes nothing of what was asked for
// meanwhile: a suspension made during the move — here once its provision
// is over, at the log rotation the provision renders last — stays, and the
// job says so.
func TestMigrationKeepsASuspensionMadeDuringTheMove(t *testing.T) {
	fakeUnixAccounts(t)
	source := newSiteFixture(t)
	source.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var grant struct {
		Token string `json:"token"`
	}
	source.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:alex", "hours": 1}, http.StatusCreated, &grant)
	target := newSiteFixture(t)
	suspended := make(chan sent, 1)
	var once sync.Once
	target.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/config/apply" && strings.Contains(string(body), siteLogrotatePath) {
			once.Do(func() {
				target.sendBeside(suspended, http.MethodPatch, "/users/alex2", map[string]any{"status": "suspended"})
			})
		}
	})
	t.Cleanup(func() { target.agent.SetBefore(nil) })
	var ref apitypes.JobRef
	target.call(http.MethodPost, "/migrate/run", map[string]any{"source": source.ts.URL, "token": grant.Token, "scope": "user:alex", "as": "alex2", "insecure": true}, http.StatusAccepted, &ref)
	j := target.waitJob(ref.JobID)
	if j.Status != store.JobDone {
		t.Fatalf("the move: %s %s\n%s", j.Status, j.Error, j.Log)
	}
	if r := answerOf(t, suspended); r.err != nil || r.status != http.StatusOK {
		t.Fatalf("the suspension during the move: %d %v %s", r.status, r.err, r.body)
	}
	if u, _ := target.db.GetUserByLogin(target.ctx, "alex2"); u.Status != store.UserSuspended {
		t.Errorf("alex2, suspended during the move, after it: %s\n%s", u.Status, j.Log)
	}
	if !strings.Contains(j.Log, "the account alex2 was suspended during the move: it stays suspended") {
		t.Errorf("the move's log does not say so:\n%s", j.Log)
	}
}
