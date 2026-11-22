package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// The jobs that may run past the runner's default hour have limits of their
// own: a backup restic's backup and the retention's prune, each for as long
// as the agent lets restic run, and an hour before them; a restore restic's
// restore and an hour; a migration, which takes a whole account over, four
// of the agent's streams. Every other job type keeps the hour.
func TestJobLimitsFitTheirWork(t *testing.T) {
	f := newSiteFixture(t)
	want := map[string]time.Duration{"backup.run": 8 * time.Hour, "backup.restore": 7 * time.Hour, "migrate.run": 24 * time.Hour}
	for _, typ := range []string{
		"user.provision", "user.delete", "stack.install", "apache.migrate", "apache.backend", "stack.remove", "stack.upgrade",
		"cert.issue", "php.install", "php.remove", "site.apply", "site.fix", "site.cms", "site.delete", "sites.move-ip",
		"backup.run", "backup.restore", "panel.update", "mail.install", "mail.apply", "mail.webmail", "migrate.run",
	} {
		w, ok := want[typ]
		if !ok {
			w = time.Hour
		}
		if got := f.s.jobs.Limit(typ); got != w {
			t.Errorf("%s: a limit of %s, want %s", typ, got, w)
		}
	}
	if jobs.DefaultLimit != time.Hour || agent.StreamLimit != 6*time.Hour {
		t.Errorf("the default limit %s, the agent's streams %s", jobs.DefaultLimit, agent.StreamLimit)
	}

	// What a backup and a restore have the agent give restic fits in them.
	f.agent.ToolOutput["restic"] = `{"message_type":"summary","snapshot_id":"0123456789abcdef","total_files_processed":1,"total_bytes_processed":1,"data_added":1}`
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	var run, restore apitypes.JobRef
	f.call(http.MethodPost, "/backups/run", map[string]any{"target": "local", "scope": "user:alex"}, http.StatusAccepted, &run)
	if j := f.waitJob(run.JobID); j.Status != store.JobDone {
		t.Fatalf("backup: %s %s", j.Status, j.Error)
	}
	f.call(http.MethodPost, "/backups/restore", map[string]any{"target": "local", "snapshot": "0123456789abcdef"}, http.StatusAccepted, &restore)
	if j := f.waitJob(restore.JobID); j.Status != store.JobDone {
		t.Fatalf("restore: %s %s", j.Status, j.Error)
	}
	given := map[string]time.Duration{}
	for _, tool := range f.agent.Tools() {
		if tool.Name == "restic" && len(tool.Args) > 0 {
			given[tool.Args[0]] = time.Duration(tool.TimeoutSeconds) * time.Second
		}
	}
	if given["backup"] != 6*time.Hour || given["forget"] != time.Hour || given["restore"] != 6*time.Hour {
		t.Fatalf("restic was given %v", given)
	}
	if got := f.s.jobs.Limit("backup.run"); got < given["backup"]+given["forget"]+time.Hour {
		t.Errorf("a backup's limit %s leaves restic's backup %s and the prune %s no hour before them", got, given["backup"], given["forget"])
	}
	if got := f.s.jobs.Limit("backup.restore"); got < given["restore"]+time.Hour {
		t.Errorf("a restore's limit %s leaves restic's restore %s no hour more", got, given["restore"])
	}
}

// The jobs line counts a failure for a day from when the job failed, not
// from when it was queued: a backup or a migration may run for hours, and
// one queued a day ago that failed at its limit just now is a failure of
// today.
func TestDoctorJobsLineCountsTheDayFromTheFailure(t *testing.T) {
	now := time.Now()
	job := func(id int64, queued time.Duration, ended *time.Duration) *store.Job {
		j := &store.Job{ID: id, Type: "migrate.run", LockKey: "user:alex", Payload: json.RawMessage(`{}`), Status: store.JobFailed, CreatedAt: store.Time(now.Add(-queued))}
		if ended != nil {
			start, end := now.Add(-queued), now.Add(-*ended)
			j.StartedAt, j.FinishedAt = &start, &end
		}
		return j
	}
	hour, day := time.Hour, 25*time.Hour
	if got := openFailures([]*store.Job{job(1, 30*time.Hour, &hour)}, nil, now); got != 1 {
		t.Errorf("queued 30 hours ago, failed an hour ago: %d open", got)
	}
	if got := openFailures([]*store.Job{job(1, 30*time.Hour, &day)}, nil, now); got != 0 {
		t.Errorf("failed 25 hours ago: %d open", got)
	}
	if got := openFailures([]*store.Job{job(1, 25*time.Hour, nil)}, nil, now); got != 0 {
		t.Errorf("queued 25 hours ago, no end recorded: %d open", got)
	}
	if got := openFailures([]*store.Job{job(1, time.Hour, nil)}, nil, now); got != 1 {
		t.Errorf("queued an hour ago, no end recorded: %d open", got)
	}
}
