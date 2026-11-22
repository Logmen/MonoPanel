package api

import (
	"encoding/json"
	"fmt"
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
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 5, NoSymfollow: true, PrivateEtc: true}
	if c := find(); c == nil || c.Status != "ok" || c.Detail != "Landlock ABI 5" {
		t.Fatalf("Landlock 5, the view and a private /etc: %+v", c)
	}
	// no private /etc: a host that replaces resolv.conf during a backup
	// takes name resolution away from restic
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 5, NoSymfollow: true, PrivateEtcError: "--etc: a tmpfs on /etc: permission denied"}
	if c := find(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "resolv.conf") || !strings.Contains(c.Detail, "a tmpfs on /etc: permission denied") {
		t.Fatalf("no private /etc: %+v", c)
	}
	// Landlock built in but not enabled (Oracle Linux's UEK): how to turn it
	// on, and that a backup still reads in its view without symlinks
	hint := "Landlock is built into this kernel but not enabled: add it to the kernel's lsm= list and reboot — grubby --update-kernel=ALL --args=lsm=lockdown,capability,yama,selinux,bpf,landlock"
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported", Hint: hint, NoSymfollow: true}
	if c := find(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, hint) || !strings.Contains(c.Detail, "view that follows no symlink") {
		t.Fatalf("no Landlock, the view: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported", Hint: hint, NoSymfollowError: "the agent does not run as root"}
	if c := find(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, hint) || !strings.Contains(c.Detail, "stored as its own") || !strings.Contains(c.Detail, "does not run as root") {
		t.Fatalf("no Landlock, no view: %+v", c)
	}
}

// The agent runs the installed restic's restic version in the sandbox a
// backup gets: one that starts there the doctor names. One that does not —
// Red Hat's Go exits when it cannot read the kernel's FIPS switch — fails
// every backup, and the doctor's line fails, with restic's own error and what
// to do, counted in the summary like any failure. One that did not finish in
// time may only have met a host too busy at that moment and is tested again:
// a warning that says so, when, and what to look at should it last. An agent
// that does not say it timed out (no restic_timeout) reports a failure, even
// in words that say it did not finish. Either verdict comes first, so that
// no rule of the testbed's doctor-expected.sh that allows a host's missing
// Landlock (^no Landlock) takes it for that, and the sandbox's state follows.
func TestDoctorResticSelfTest(t *testing.T) {
	f := newSiteFixture(t)
	doctor := func() (apitypes.Doctor, *apitypes.Check) {
		var d apitypes.Doctor
		f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
		for i := range d.Checks {
			if d.Checks[i].Name == "restic sandbox" {
				return d, &d.Checks[i]
			}
		}
		t.Fatal("no restic sandbox line")
		return d, nil
	}
	find := func() *apitypes.Check {
		_, c := doctor()
		return c
	}
	failures := func(d apitypes.Doctor) int {
		n := 0
		for _, c := range d.Checks {
			if c.Status == "fail" {
				n++
			}
		}
		return n
	}
	const (
		bin     = "/usr/bin/restic"
		version = "restic 0.19.1 compiled with go1.26.4 (Red Hat 1.26.4-1.el9_8) on linux/amd64"
		failed  = "error reading /proc/sys/crypto/fips_enabled: open /proc/sys/crypto/fips_enabled: permission denied (exit 1)"
		hung    = "restic version did not finish within 30s"
	)
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 6, NoSymfollow: true, PrivateEtc: true, Restic: bin, ResticVersion: version}
	healthy, c := doctor()
	if c.Status != "ok" || c.Detail != "Landlock ABI 6; restic starts as a backup's restic would ("+version+")" {
		t.Fatalf("a restic that starts in its sandbox: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 6, NoSymfollow: true, PrivateEtc: true, Restic: bin, ResticError: failed}
	d, c := doctor()
	if c.Status != "fail" || !strings.HasPrefix(c.Detail, "restic does not start in its sandbox — "+bin+" version there: "+failed+". ") ||
		!strings.Contains(c.Detail, "Every backup fails") || !strings.Contains(c.Detail, "does not run restic outside it") ||
		!strings.Contains(c.Detail, "panel's developers") || !strings.Contains(c.Detail, "upgrade the panel") || !strings.HasSuffix(c.Detail, "The sandbox: Landlock ABI 6") {
		t.Fatalf("a restic that does not start in its sandbox: %+v", c)
	}
	if n := failures(d); n != failures(healthy)+1 || !strings.Contains(d.Summary, fmt.Sprintf(", %d failed, ", n)) {
		t.Fatalf("the summary of a doctor whose restic does not start: %q (%d failed, %d before)", d.Summary, n, failures(healthy))
	}
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 6, NoSymfollow: true, PrivateEtc: true, Restic: bin, ResticError: hung, ResticTimeout: true}
	d, c = doctor()
	if c.Status != "warn" || !strings.HasPrefix(c.Detail, "restic took too long to start in its sandbox — "+bin+" version there: "+hung+". ") ||
		!strings.Contains(c.Detail, "too busy") || !strings.Contains(c.Detail, "tests it again on the first ping 10 minutes after that run") ||
		!strings.Contains(c.Detail, "If it keeps timing out, backups hang the same way") || !strings.Contains(c.Detail, "(time "+bin+" version)") ||
		strings.Contains(c.Detail, "does not start") || !strings.HasSuffix(c.Detail, "The sandbox: Landlock ABI 6") {
		t.Fatalf("a restic that did not finish in time: %+v", c)
	}
	if failures(d) != failures(healthy) {
		t.Fatalf("a timeout counted as a failure: %q", d.Summary)
	}
	// the ping of an agent that does not tell a timeout from an error
	var older agent.SandboxInfo
	if err := json.Unmarshal([]byte(`{"landlock":6,"nosymfollow":true,"private_etc":true,"restic":"`+bin+`","restic_error":"`+hung+`"}`), &older); err != nil {
		t.Fatal(err)
	}
	f.agent.Sandbox = &older
	if c := find(); c.Status != "fail" || !strings.HasPrefix(c.Detail, "restic does not start in its sandbox — "+bin+" version there: "+hung+". ") {
		t.Fatalf("an agent without restic_timeout: %+v", c)
	}
	// the flag qualifies an error and says nothing without one
	f.agent.Sandbox = &agent.SandboxInfo{Landlock: 6, NoSymfollow: true, PrivateEtc: true, Restic: bin, ResticVersion: version, ResticTimeout: true}
	if c := find(); c.Status != "ok" || c.Detail != "Landlock ABI 6; restic starts as a backup's restic would ("+version+")" {
		t.Fatalf("restic_timeout without an error: %+v", c)
	}
	// without Landlock a backup's restic still goes through sandbox-exec, for
	// its view: the self-test's failure and timeout come before the missing
	// Landlock, its success after it
	hint := "Landlock is built into this kernel but not enabled: add it to the kernel's lsm= list and reboot — grubby --update-kernel=ALL --args=lsm=lockdown,capability,yama,selinux,bpf,landlock"
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported", Hint: hint, NoSymfollow: true, Restic: bin, ResticError: failed}
	if c := find(); c.Status != "fail" || !strings.HasPrefix(c.Detail, "restic does not start in its sandbox") || !strings.Contains(c.Detail, failed) || !strings.Contains(c.Detail, "The sandbox: no Landlock (landlock: operation not supported)") {
		t.Fatalf("no Landlock, a restic that does not start: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported", Hint: hint, NoSymfollow: true, Restic: bin, ResticError: hung, ResticTimeout: true}
	if c := find(); c.Status != "warn" || !strings.HasPrefix(c.Detail, "restic took too long to start in its sandbox") || !strings.Contains(c.Detail, hung) || !strings.Contains(c.Detail, "The sandbox: no Landlock (landlock: operation not supported)") {
		t.Fatalf("no Landlock, a restic that did not finish in time: %+v", c)
	}
	f.agent.Sandbox = &agent.SandboxInfo{Error: "landlock: operation not supported", Hint: hint, NoSymfollow: true, Restic: bin, ResticVersion: version}
	if c := find(); c.Status != "warn" || !strings.HasPrefix(c.Detail, "no Landlock (") || !strings.HasSuffix(c.Detail, "; restic starts as a backup's restic would ("+version+")") {
		t.Fatalf("no Landlock, a restic that starts: %+v", c)
	}
}
