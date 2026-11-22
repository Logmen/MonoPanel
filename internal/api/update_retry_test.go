package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// An install that rolled back leaves the package database with the new
// version while the previous binary runs, and installing that version again
// reinstalls it (update-run). The status and the doctor say so, and the
// automatic update leaves that version alone — every round would restart
// the panel twice to roll back again — while an administrator's update
// installs it, and a later release goes in automatically as before. An
// attempt at the version that runs now is history, which the doctor no
// longer names.
func TestARolledBackUpdateWaitsForAnAdministrator(t *testing.T) {
	// Set before the fixture's job runner starts, put back after it stopped.
	running := buildinfo.Version
	buildinfo.Version = "9.9.7"
	t.Cleanup(func() { buildinfo.Version = running })
	rel := newRelease(t, "9.9.9")
	f := newFixture(t, func(c *config.Config) { c.Update.PublicKey = rel.pub })
	f.configureUpdates(rel)
	f.call(http.MethodPut, "/system/update", map[string]any{"auto_apply": true}, http.StatusOK, nil)
	attempt := func(st updater.State) {
		t.Helper()
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.s.cfg.UpdatesDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(updater.StateFile(f.s.cfg.UpdatesDir()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The panel.update jobs the scheduler queued so far.
	queued := func() int {
		t.Helper()
		jobs, err := f.db.ListJobs(f.ctx, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, j := range jobs {
			if j.Type == "panel.update" && j.RequestedBy == "scheduler" {
				n++
			}
		}
		return n
	}
	// One round of the scheduler, a check due: what it queued.
	tick := func() int {
		t.Helper()
		c := f.s.loadUpdateConfig(f.ctx)
		c.CheckedAt = nil
		if err := f.s.saveUpdateConfig(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		before := queued()
		f.s.updateTick(f.ctx)
		f.settle()
		return queued() - before
	}
	doctor := func() string {
		t.Helper()
		for _, c := range f.s.doctor(f.ctx).Checks {
			if c.Name == "update attempt" {
				return c.Status + ": " + c.Detail
			}
		}
		return ""
	}
	now := time.Now()
	attempt(updater.State{Status: updater.StatusRolledBack, From: "9.9.7", To: "9.9.9", Started: now, Finished: now, Error: "panel did not come up after the update: panel answers 9.9.7, expected 9.9.9"})

	var st apitypes.UpdateStatus
	f.call(http.MethodGet, "/system/update", nil, http.StatusOK, &st)
	if a := st.LastAttempt; a == nil || a.Status != updater.StatusRolledBack || !a.Reinstall {
		t.Fatalf("the last attempt: %+v", st.LastAttempt)
	}
	if got := doctor(); !strings.HasPrefix(got, "warn: install of 9.9.9: rolled-back, panel did not come up") || !strings.Contains(got, "the package database lists 9.9.9 already") || !strings.Contains(got, "mp update apply installs it again, which the automatic update does not do") {
		t.Fatalf("doctor: %q", got)
	}
	if n := tick(); n != 0 {
		t.Fatalf("the automatic update queued the version that rolled back: %d jobs", n)
	}

	// The administrator asks for it: it goes to the agent.
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("update job: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.LastCall("/v1/panel/install"); !ok {
		t.Fatal("the administrator's update did not reach the agent")
	}

	// A failed install (the package manager did not get through) and the
	// rollback of an earlier release leave the automatic update as it was.
	for _, other := range []updater.State{
		{Status: updater.StatusFailed, From: "9.9.7", To: "9.9.9", Error: "package install failed"},
		{Status: updater.StatusRolledBack, From: "9.9.7", To: "9.9.8"},
	} {
		attempt(other)
		st = apitypes.UpdateStatus{} // omitted fields keep nothing of the last answer
		f.call(http.MethodGet, "/system/update", nil, http.StatusOK, &st)
		if st.LastAttempt == nil || st.LastAttempt.Reinstall != (other.Status == updater.StatusRolledBack) {
			t.Fatalf("%s of %s: %+v", other.Status, other.To, st.LastAttempt)
		}
		if n := tick(); n != 1 {
			t.Fatalf("%s of %s: the automatic update queued %d jobs, want one", other.Status, other.To, n)
		}
	}

	// An attempt at the version that runs, or an older one: history.
	for _, v := range []string{"9.9.7", "9.9.6"} {
		attempt(updater.State{Status: updater.StatusRolledBack, From: "9.9.5", To: v})
		if got := doctor(); got != "" {
			t.Fatalf("an attempt at %s: %q", v, got)
		}
		st = apitypes.UpdateStatus{} // omitted fields keep nothing of the last answer
		f.call(http.MethodGet, "/system/update", nil, http.StatusOK, &st)
		if st.LastAttempt == nil || st.LastAttempt.Reinstall {
			t.Fatalf("an attempt at %s: %+v", v, st.LastAttempt)
		}
	}
}
