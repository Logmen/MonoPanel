package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"monopanel/internal/store"
)

// lineJob is a job of the doctor's jobs line: queued an hour before it
// started, run for a minute up to end before now.
func lineJob(now time.Time, id int64, typ, lock, payload, status string, end time.Duration) *store.Job {
	start, finish := now.Add(-end-time.Minute), now.Add(-end)
	return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), Status: status, CreatedAt: store.Time(start.Add(-time.Hour)), StartedAt: &start, FinishedAt: &finish}
}

// A removal of a failed job's own lock key clears the failure when it was
// queued after the job: the jobs of one key run in the order they were
// queued, whatever the clock said. A host that booted with its clock ahead
// and was set right later records a failure from the future and, after it,
// a removal from the past — the failure is history all the same; and a
// removal from the clock's future, queued before a failure on the site made
// again under the name, says nothing of that failure. A removal under
// another lock key — the account's, which takes its sites along — that ran
// beside the job clears it as well when it ended after the job started:
// the site's apply failed as the site was being removed with its account.
func TestDoctorJobsLineGoesByTheQueueForAJobsOwnKey(t *testing.T) {
	now := time.Now()
	ahead := -30 * 24 * time.Hour // ended 30 days from now
	const site = `{"site_id":7}`
	for _, c := range []struct {
		name   string
		failed *store.Job
		done   *store.Job
		want   int
	}{
		{"a failure while the clock was ahead, the site removed after it was set right",
			lineJob(now, 10, "site.apply", "site:a.example.com", site, store.JobFailed, ahead),
			lineJob(now, 11, "site.delete", "site:a.example.com", site, store.JobDone, time.Hour), 0},
		{"a removal while the clock was ahead, a failure of the site made again after it was set right",
			lineJob(now, 11, "site.apply", "site:a.example.com", `{"site_id":8}`, store.JobFailed, time.Hour),
			lineJob(now, 10, "site.delete", "site:a.example.com", site, store.JobDone, ahead), 1},
		{"the account removed with the site, over after the apply started",
			lineJob(now, 11, "site.apply", "site:a.example.com", site, store.JobFailed, 50*time.Minute),
			lineJob(now, 10, "user.delete", "user:alex", `{"user_id":2,"login":"alex","sites":["a.example.com"]}`, store.JobDone, 40*time.Minute), 0},
		{"the account removed with the site, over before the apply started",
			lineJob(now, 11, "site.apply", "site:a.example.com", site, store.JobFailed, 50*time.Minute),
			lineJob(now, 10, "user.delete", "user:alex", `{"user_id":2,"login":"alex","sites":["a.example.com"]}`, store.JobDone, 55*time.Minute), 1},
		{"the account removed, queued after the provision that failed, the clock stepped back",
			lineJob(now, 10, "user.provision", "user:alex", `{"user_id":2,"login":"alex"}`, store.JobFailed, ahead),
			lineJob(now, 11, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, store.JobDone, time.Hour), 0},
	} {
		if got := openFailures([]*store.Job{c.failed}, []*store.Job{c.done}, now); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
}

// Two removals of one site, as the store lists them, newest first, and a
// failure between them, on the site made again under the name: history once
// the second removal went through, by its place in the queue and by the
// clock alike. The same goes for two runs of the same work.
func TestDoctorJobsLineTheLatestOfTwoRemovalsCounts(t *testing.T) {
	now := time.Now()
	first := lineJob(now, 10, "site.delete", "site:a.example.com", `{"site_id":7}`, store.JobDone, 109*time.Minute)
	second := lineJob(now, 30, "site.delete", "site:a.example.com", `{"site_id":9}`, store.JobDone, 19*time.Minute)
	apply := lineJob(now, 20, "site.apply", "site:a.example.com", `{"site_id":9}`, store.JobFailed, 39*time.Minute)
	webmail := lineJob(now, 21, "mail.webmail", "mail", `{"domain":"a.example.com","user":"alex"}`, store.JobFailed, 39*time.Minute)
	for _, done := range [][]*store.Job{{second, first}, {first, second}} {
		if got := openFailures([]*store.Job{apply, webmail}, done, now); got != 0 {
			t.Errorf("failures between two removals of their site, the second over (listed #%d, #%d): %d open", done[0].ID, done[1].ID, got)
		}
	}
	if got := openFailures([]*store.Job{apply, webmail}, []*store.Job{first}, now); got != 2 {
		t.Errorf("failures after the first removal of their site: %d open", got)
	}
	later := lineJob(now, 25, "site.apply", "site:a.example.com", `{"site_id":9}`, store.JobDone, 29*time.Minute)
	older := lineJob(now, 15, "site.apply", "site:a.example.com", `{"site_id":9}`, store.JobDone, 89*time.Minute)
	if got := openFailures([]*store.Job{apply}, []*store.Job{later, older}, now); got != 0 {
		t.Errorf("a failure fixed by the newer of two applies: %d open", got)
	}
	if got := openFailures([]*store.Job{apply}, []*store.Job{older}, now); got != 1 {
		t.Errorf("a failure after an apply that went through: %d open", got)
	}
}

// A failed job without its start time — the runner writes none such, but
// the line must not fall over one — goes by its place in the queue beside a
// removal with its times.
func TestDoctorJobsLineAFailedJobWithoutStartTime(t *testing.T) {
	now := time.Now()
	failed := &store.Job{ID: 5, Type: "mail.webmail", LockKey: "mail", Payload: json.RawMessage(`{"domain":"webmail.example.com","user":"alex"}`), Status: store.JobFailed, CreatedAt: store.Time(now.Add(-time.Hour))}
	for _, c := range []struct {
		id   int64
		want int
	}{{6, 0}, {4, 1}} {
		removal := lineJob(now, c.id, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, store.JobDone, 19*time.Minute)
		if got := openFailures([]*store.Job{failed}, []*store.Job{removal}, now); got != c.want {
			t.Errorf("an untimed failure, a removal #%d with its times: %d open, want %d", c.id, got, c.want)
		}
	}
}

// The doctor's jobs line knows the sites that are there: a webmail asked
// into a site of kate's, naming the account alex, failed; alex was removed
// later. The site is still kate's and the webmail still not there, so the
// failure counts — until the site goes too.
func TestDoctorJobsLineWebmailIntoAnotherAccountsSite(t *testing.T) {
	f := newSiteFixture(t)
	kate := &store.User{Login: "kate", Role: store.RoleUser, Status: store.UserActive}
	if err := f.db.CreateUser(f.ctx, kate); err != nil {
		t.Fatal(err)
	}
	site := &store.Site{UserID: kate.ID, Domain: "webmail.example.com", Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: "8.4", Docroot: "public_html", SSL: "none"}
	if err := f.db.CreateSite(f.ctx, site); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	insert := func(j *store.Job) {
		t.Helper()
		if _, err := f.db.SQL().ExecContext(f.ctx, `INSERT INTO jobs(type, payload, status, lock_key, created_at, started_at, finished_at) VALUES(?,?,?,?,?,?,?)`,
			j.Type, string(j.Payload), j.Status, j.LockKey, j.CreatedAt.Std().UTC().Format(time.RFC3339Nano), j.StartedAt.UTC().Format(time.RFC3339Nano), j.FinishedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	insert(lineJob(now, 0, "mail.webmail", "mail", `{"domain":"webmail.example.com","user":"alex"}`, store.JobFailed, 50*time.Minute))
	insert(lineJob(now, 0, "user.delete", "user:alex", `{"user_id":2,"login":"alex","purge":true,"sites":["alex.example.com"]}`, store.JobDone, 10*time.Minute))
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "warn: 1 failed") {
		t.Fatalf("a webmail into kate's site after the account it named went: %q", got)
	}
	if err := f.db.DeleteSite(f.ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "ok:") {
		t.Fatalf("the same once no site of the name is there: %q", got)
	}
}
