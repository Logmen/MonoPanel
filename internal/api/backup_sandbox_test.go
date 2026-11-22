package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A backup restic could not read everything for — exit 3, a snapshot
// without some files: the agent's sandbox refuses what a symlink in a home
// leads to outside it — is neither a failure nor done: the run and the target
// say "partial" and name what the snapshot lacks; the job itself succeeds,
// and retention runs as after any other snapshot.
func TestBackupWithUnreadableFilesIsPartial(t *testing.T) {
	f := newSiteFixture(t)
	forgot := false
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		switch {
		case req.Name == "restic" && len(req.Args) > 0 && req.Args[0] == "backup":
			return &agent.ToolResponse{ExitCode: 3, Output: strings.Join([]string{
				`{"message_type":"error","error":{"message":"open /var/www/alex/a/shadow: permission denied"},"during":"scan","item":"/var/www/alex/a/shadow"}`,
				`{"message_type":"error","error":{"message":"open /var/www/alex/a/shadow: permission denied"},"during":"archival","item":"/var/www/alex/a/shadow"}`,
				`{"message_type":"summary","snapshot_id":"0123456789abcdef","total_files_processed":10,"total_bytes_processed":1000,"data_added":100}`,
				`Warning: at least one source file could not be read`,
			}, "\n")}
		case req.Name == "restic" && len(req.Args) > 0 && req.Args[0] == "forget":
			forgot = true
		}
		return nil
	}
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	var job apitypes.JobRef
	f.call(http.MethodPost, "/backups/run", map[string]any{"target": "local", "scope": "user:alex"}, http.StatusAccepted, &job)
	if j := f.waitJob(job.JobID); j.Status != store.JobDone {
		t.Fatalf("an incomplete snapshot fails the job: %s %s", j.Status, j.Error)
	}
	var runs []store.Backup
	f.call(http.MethodGet, "/backups", nil, http.StatusOK, &runs)
	if len(runs) != 1 || runs[0].Status != "partial" || runs[0].SnapshotID != "0123456789abcdef" ||
		!strings.Contains(runs[0].Error, "lacks 1 path restic could not read") || !strings.Contains(runs[0].Error, "/var/www/alex/a/shadow: permission denied") {
		t.Fatalf("runs: %+v", runs)
	}
	var targets []store.BackupTarget
	f.call(http.MethodGet, "/backups/targets", nil, http.StatusOK, &targets)
	if len(targets) != 1 || targets[0].LastStatus != "partial" || targets[0].LastError != runs[0].Error {
		t.Fatalf("targets: %+v", targets)
	}
	if !forgot {
		t.Error("retention did not run after a partial snapshot")
	}
}

// What an incomplete backup lacks, as restic's versions print it: the --json
// error lines with a message (0.15 on) or without one (older ones print the
// error as {}), the plain "error:" lines, an item reported by the scan and by
// the archival once, and at most three of them by name.
func TestIncompleteBackup(t *testing.T) {
	for _, c := range []struct {
		out  string
		want string
	}{
		{"", "restic exit 3: the snapshot lacks what restic could not read"},
		{`{"message_type":"error","error":{},"during":"archival","item":"/var/www/a/x"}`, "restic exit 3: the snapshot lacks 1 path restic could not read: /var/www/a/x"},
		{`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/var/www/a/x"}`, "restic exit 3: the snapshot lacks 1 path restic could not read: /var/www/a/x: permission denied"},
		{"error: open /var/www/a/x: permission denied\n", "restic exit 3: the snapshot lacks 1 path restic could not read: open /var/www/a/x: permission denied"},
		{strings.Join([]string{
			`{"message_type":"error","error":{"message":"open /a: permission denied"},"during":"scan","item":"/a"}`,
			`{"message_type":"error","error":{"message":"open /a: permission denied"},"during":"archival","item":"/a"}`,
			`{"message_type":"error","error":{"message":"open /b: permission denied"},"during":"archival","item":"/b"}`,
			`{"message_type":"error","error":{"message":"open /c: permission denied"},"during":"archival","item":"/c"}`,
			`{"message_type":"error","error":{"message":"open /d: permission denied"},"during":"archival","item":"/d"}`,
			`{"message_type":"status","percent_done":1}`,
		}, "\n"), "restic exit 3: the snapshot lacks 4 paths restic could not read: open /a: permission denied; open /b: permission denied; open /c: permission denied and 1 more"},
	} {
		if got := incompleteBackup(3, c.out); got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.out, got, c.want)
		}
	}
}

// The doctor says whether restic runs in its sandbox: a warning, with the
// risk, on a kernel without Landlock; nothing from an agent that does not
// tell.
func TestDoctorResticSandbox(t *testing.T) {
	f := newSiteFixture(t)
	find := func() *apitypes.Check {
		var d apitypes.Doctor
		f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
		for i := range d.Checks {
			if d.Checks[i].Name == "restic sandbox" {
				return &d.Checks[i]
			}
		}
		return nil
	}
	if c := find(); c != nil {
		t.Fatalf("an agent that does not tell: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported"}
	if c := find(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "operation not supported") || !strings.Contains(c.Detail, "symlink") {
		t.Fatalf("no Landlock: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 5}
	if c := find(); c == nil || c.Status != "ok" || c.Detail != "Landlock ABI 5" {
		t.Fatalf("Landlock 5: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 1}
	if c := find(); c == nil || c.Status != "ok" || !strings.Contains(c.Detail, "restores without it") {
		t.Fatalf("Landlock 1: %+v", c)
	}
	// Landlock, but no view of the web root without symlinks: a server
	// backup still lets an account's symlink lead into the other trees
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 5, NoSymfollowError: "move_mount: permission denied"}
	if c := find(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "move_mount: permission denied") || !strings.Contains(c.Detail, "server backup") {
		t.Fatalf("no view without symlinks: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 5, NoSymfollow: true}
	if c := find(); c == nil || c.Status != "ok" {
		t.Fatalf("Landlock 5 and the view: %+v", c)
	}
}
