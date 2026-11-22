package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// logBuffer collects a server's log lines; the refresh writes them from its
// own goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitLog waits until the log holds the text.
func waitLog(t *testing.T, logs *logBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never says %q:\n%s", text, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// absentAgent is a socket path of the fake agent's where nothing listens
// yet: listen makes the agent answer there (a symlink to its socket). The
// path stays short, as unix sockets want.
func absentAgent(t *testing.T, fake *agenttest.Agent) (sock string, listen func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mpwait")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "agent.sock")
	return sock, func() {
		if err := os.Symlink(fake.Socket, sock); err != nil {
			t.Error(err)
		}
	}
}

// The start-up refresh waits for the agent as long as it takes — it may
// listen a minute and more after the API started — and says so in the log;
// once the agent answers, the refresh runs. One that did not run because the
// API stopped first says it runs at the next start. On the generic profile
// it changes nothing of the host and says that too.
func TestStartupRefreshWaitsForTheAgent(t *testing.T) {
	f := newSiteFixture(t)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	f.s.agentWaitStep = 10 * time.Millisecond
	sock, listen := absentAgent(t, f.agent)
	f.s.agent = agent.NewClient(sock)
	f.agent.Reset()
	done := make(chan struct{})
	go func() { defer close(done); f.s.startupRefresh(f.ctx) }()
	waitLog(t, logs, "start-up refresh: waiting for the agent")
	time.Sleep(150 * time.Millisecond)
	if f.agent.Called("/v1/pkg") || f.s.refreshed.Load() {
		t.Fatal("the refresh ran without the agent")
	}
	listen()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the refresh never ran")
	}
	if !strings.Contains(logs.String(), "start-up refresh: the agent answers") || !f.agent.Called("/v1/pkg") || !f.s.refreshed.Load() {
		t.Fatalf("after the agent came:\n%s", logs.String())
	}
	if m := f.s.siteMovesState(f.ctx); m.Logs != f.s.siteLogsRoot() {
		t.Fatalf("the moves did not run: %+v", m)
	}

	// The API stops before the agent answers.
	g := newSiteFixture(t)
	glogs := &logBuffer{}
	g.s.log = slog.New(slog.NewTextHandler(glogs, nil))
	g.s.agentWaitStep = 10 * time.Millisecond
	gsock, _ := absentAgent(t, g.agent)
	g.s.agent = agent.NewClient(gsock)
	ctx, cancel := context.WithCancel(g.ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); g.s.startupRefresh(ctx) }()
	waitLog(t, glogs, "start-up refresh: waiting for the agent")
	cancel()
	<-stopped
	if !strings.Contains(glogs.String(), "start-up refresh did not run: the API stopped before the agent answered; it runs at the next start") || g.s.refreshed.Load() {
		t.Fatalf("after the stop:\n%s", glogs.String())
	}

	// An unsupported distribution: nothing of the host is touched.
	h := newSiteFixture(t)
	hlogs := &logBuffer{}
	h.s.log = slog.New(slog.NewTextHandler(hlogs, nil))
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=arch\n"))
	h.s.profile = osprofile.Generic(rel)
	h.agent.Reset()
	h.s.startupRefresh(h.ctx)
	if len(h.agent.Calls()) != 0 || !strings.Contains(hlogs.String(), "start-up refresh skipped") {
		t.Fatalf("generic profile: %v\n%s", h.agent.Calls(), hlogs.String())
	}
}

// heldPanel is a panel whose agent does not listen yet, with a job that
// needs it.
func heldPanel(t *testing.T, wait time.Duration) (db *store.DB, runner *jobs.Runner, logs *logBuffer, listen func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.RunDir = t.TempDir()
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rel, _ := osprofile.ParseOSRelease(strings.NewReader(fixtureOSRelease))
	profile, _ := osprofile.FromRelease(rel)
	logs = &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	fake := agenttest.Start(t)
	sock, listen := absentAgent(t, fake)
	runner = jobs.NewRunner(db, 2, log)
	s := New(cfg, db, agent.NewClient(sock), runner, profile, log)
	s.agentWaitStep, s.jobsAgentWait = 10*time.Millisecond, wait
	runner.Register("test.agent", func(ctx context.Context, _ *jobs.Context) error {
		_, err := s.agent.Ping(ctx)
		return err
	})
	runner.Start(ctx)
	t.Cleanup(runner.Wait)
	t.Cleanup(cancel)
	return db, runner, logs, listen
}

// A job queued while the agent is not listening yet — at a start of both,
// it listens only after its own work — is not claimed to fail on "agent
// unreachable": the workers wait for the agent, then run it. They wait for a
// while only: an agent that does not come at all fails the jobs, as at any
// later time, and the log says why.
func TestJobsWaitForTheAgent(t *testing.T) {
	db, runner, logs, listen := heldPanel(t, time.Minute)
	ctx := context.Background()
	j, err := runner.Enqueue(ctx, "test.agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitLog(t, logs, "jobs: waiting for the agent")
	time.Sleep(150 * time.Millisecond)
	if got, _ := db.GetJob(ctx, j.ID); got.Status != store.JobQueued {
		t.Fatalf("claimed without the agent: %s %s", got.Status, got.Error)
	}
	listen()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, _ := db.GetJob(ctx, j.ID)
		if got.Status == store.JobDone {
			break
		}
		if got.Status == store.JobFailed || time.Now().After(deadline) {
			t.Fatalf("job: %s %s", got.Status, got.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}

	db2, runner2, logs2, _ := heldPanel(t, 200*time.Millisecond)
	j2, _ := runner2.Enqueue(ctx, "test.agent", nil)
	deadline = time.Now().Add(20 * time.Second)
	for {
		got, _ := db2.GetJob(ctx, j2.ID)
		if got.Status == store.JobFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job: %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs2.String(), "jobs: gave up waiting for the agent") || !strings.Contains(logs2.String(), "the workers start without the agent") {
		t.Fatalf("log:\n%s", logs2.String())
	}
}

// Once an hour after the start-up refresh ran, a move whose next try is due
// is queued again; before the refresh ran, the hour queues nothing.
func TestHourlyRetryOfTheMoves(t *testing.T) {
	f := newSiteFixture(t)
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.s.moveSiteLogs(f.ctx)
	f.settle()
	f.s.moveSiteLogs(f.ctx)
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Sites[b.ID].Next = time.Now().Add(-time.Second)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := f.logMoveJobs()
	f.s.retryMoves(f.ctx)
	if now, _ := f.logMoveJobs(); len(now[b.ID]) != len(before[b.ID]) {
		t.Fatal("queued before the start-up refresh ran")
	}
	f.s.refreshed.Store(true)
	f.s.retryMoves(f.ctx)
	if now, _ := f.logMoveJobs(); len(now[b.ID]) != len(before[b.ID])+1 {
		t.Fatalf("not queued again: %v", now)
	}
}

// A site that cannot move keeps the shared Apache running, and the move is
// not queued again at every start: the next start leaves it waiting, the
// journal and the doctor say where it stands, and once due it is queued
// again.
func TestApacheMoveWaitsAfterAFailure(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.sharedLayout(nil)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	f.s.refreshApacheInstances(f.ctx)
	if job := f.waitJobOfType("apache.migrate"); job.Status != store.JobFailed {
		t.Fatalf("migrate: %s", job.Status)
	}
	count := func() int {
		jobs, _ := f.db.ListJobs(f.ctx, 500, "")
		n := 0
		for _, j := range jobs {
			if j.Type == "apache.migrate" {
				n++
			}
		}
		return n
	}
	f.s.refreshApacheInstances(f.ctx)
	r := f.s.apacheMigrateState(f.ctx)
	if n := count(); n != 1 || r.Tries != 1 || r.Job != 0 || !strings.Contains(r.Error, "b.example.com") {
		t.Fatalf("after the failure: %d jobs, %+v", n, r)
	}
	if wait := time.Until(r.Next); wait < 9*time.Minute || wait > 11*time.Minute {
		t.Fatalf("next try in %s", wait)
	}
	if !strings.Contains(logs.String(), "apache: the move of the sites to their accounts' Apaches failed") {
		t.Fatalf("log:\n%s", logs.String())
	}
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	found := false
	for _, c := range doc.Checks {
		if c.Name == "service apache2.service" && c.Status == "warn" && strings.Contains(c.Detail, "failed 1 time(s)") && strings.Contains(c.Detail, "b.example.com") {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor: %+v", doc.Checks)
	}
	r.Next = time.Now().Add(-time.Second)
	f.s.saveApacheMigrateState(f.ctx, r)
	f.s.refreshApacheInstances(f.ctx)
	if n := count(); n != 2 {
		t.Fatalf("not queued again when due: %d", n)
	}
	f.settle()
}

// At the upgrade's first start the log move's own site.apply jobs put the
// sites in Apache mode on their accounts' Apaches; the move of the sites to
// the accounts' Apaches then applies none of them again, and still retires
// the shared Apache.
func TestApacheMoveSkipsSitesThatMovedAlready(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.agent.Reset()
	f.sharedLayout(nil)
	f.s.moveSiteLogs(f.ctx)
	f.settle()
	from := len(f.agent.Calls())
	f.s.refreshApacheInstances(f.ctx)
	job := f.waitJobOfType("apache.migrate")
	if job.Status != store.JobDone || !strings.Contains(job.Log, "a.example.com: on the Apache of alex already") || !strings.Contains(job.Log, "b.example.com: on the Apache of alex already") {
		t.Fatalf("migrate: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if at := f.callAt(from, writes("/etc/nginx/monopanel/sites/"+d+".conf")); at >= 0 {
			t.Fatalf("%s applied again by the move", d)
		}
	}
	if f.callAt(from, service("apache2.service", "stop")) < 0 {
		t.Fatal("the shared Apache was not retired")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheInstances); v != "yes" {
		t.Fatalf("marker %q", v)
	}
	if raw, _ := f.db.GetSetting(f.ctx, settingApacheMigrate); raw != "" {
		t.Fatalf("the bookkeeping stays: %q", raw)
	}
}

// heldApply holds the first config/apply that writes the file until release
// is closed, and reports it on held.
func heldApply(f *siteFixture, file string) (held chan struct{}, release func()) {
	held = make(chan struct{})
	gate := make(chan struct{})
	var once, freed sync.Once
	f.agent.SetBefore(func(path string, body []byte) {
		var req agent.ApplyConfigSetRequest
		if path != "/v1/config/apply" || json.Unmarshal(body, &req) != nil {
			return
		}
		for _, x := range req.Files {
			if x.Path == file {
				once.Do(func() { close(held); <-gate })
				return
			}
		}
	})
	release = func() { freed.Do(func() { close(gate) }) }
	f.t.Cleanup(release)
	return held, release
}

// The move of the sites to their accounts' Apaches applies a site under the
// site's lock, and decides under it whether the site still needs it: while
// a site.apply of the site's own runs, it waits, and then leaves the site
// alone — the site.apply moved it.
func TestApacheMoveTakesTheSiteLock(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.agent.Reset()
	f.sharedLayout(nil)
	const nginx = "/etc/nginx/monopanel/sites/a.example.com.conf"
	held, release := heldApply(f, nginx)
	f.s.moveSiteLogs(f.ctx) // the upgrade's site.apply of a
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the site.apply never applied nginx")
	}
	f.s.refreshApacheInstances(f.ctx)
	time.Sleep(300 * time.Millisecond)
	nginxWrites := func() int {
		n := 0
		for i := 0; ; i++ {
			at := f.callAt(i, writes(nginx))
			if at < 0 {
				return n
			}
			n, i = n+1, at
		}
	}
	// The held write is recorded once it goes on: any write now is the
	// move's, beside the site.apply.
	if n := nginxWrites(); n != 0 {
		t.Fatalf("the move applied a beside its own site.apply: %d nginx writes", n)
	}
	release()
	job := f.waitJobOfType("apache.migrate")
	if job.Status != store.JobDone || !strings.Contains(job.Log, "a.example.com: on the Apache of alex already") {
		t.Fatalf("migrate: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if n := nginxWrites(); n != 1 {
		t.Fatalf("a applied twice: %d", n)
	}
}

// A change made while a site.apply runs is not overwritten by what that
// apply read at its start: the apply saves only what it decided, and the
// change's own site.apply renders it.
func TestSiteApplyKeepsAChangeMadeWhileItRan(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	const nginx = "/etc/nginx/monopanel/sites/a.example.com.conf"
	held, release := heldApply(f, nginx)
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/a.example.com/apply", nil, http.StatusAccepted, &out)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the site.apply never applied nginx")
	}
	var change struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/a.example.com", map[string]any{"mode": "fpm"}, http.StatusAccepted, &change)
	release()
	for _, id := range []int64{out.JobID, change.JobID} {
		if j := f.waitJob(id); j.Status != store.JobDone {
			t.Fatalf("job %d: %s %s", id, j.Status, j.Error)
		}
	}
	site, _ := f.db.GetSiteByDomain(f.ctx, "a.example.com")
	conf, _ := f.agent.File(nginx)
	if site.Mode != store.ModeFPM || strings.Contains(conf, alexApacheAddr) {
		t.Fatalf("mode %s, nginx:\n%s", site.Mode, conf)
	}
}
