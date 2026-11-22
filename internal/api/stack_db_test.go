package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

// elPercona is a ready Percona Server on EL as the panel records it.
func elPercona(version string, native bool) *store.DBInstance {
	return &store.DBInstance{Engine: "percona", Version: version, Socket: "/var/lib/mysql/mysql.sock", Service: "mysqld.service", Status: store.DBReady, NativePassword: native}
}

// Reinstalling a ready engine (mp stack install percona, Web Stack →
// Reinstall) must not take the database server away from the panel: the
// record stays ready with its version while the job runs, so databases, CMS
// installs, webmail, migrations and backups go on. A failed reinstall leaves
// a server that still answers in service and marks one that does not as
// failed; only a server that was not ready is "installing" during the run.
func TestDBReinstallKeepsTheServerInService(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	if err := f.db.UpsertDBInstance(f.ctx, elPercona("8.4.11-11", false)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var during []string // what the panel made of the server in the middle of each run
	answers, version := true, "8.4.12-12"
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		mu.Lock()
		defer mu.Unlock()
		switch req.Name {
		case "percona-release": // the repository step: the job is under way
			if inst, err := f.s.dbInstance(f.ctx); err != nil {
				during = append(during, "unusable: "+err.Error())
			} else {
				during = append(during, inst.Status+" "+inst.Version)
			}
			return &agent.ToolResponse{}
		case "mysql":
			if !answers {
				return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/lib/mysql/mysql.sock' (2)"}
			}
			if strings.Contains(req.Stdin, "VERSION()") {
				return &agent.ToolResponse{Output: version + "\n"}
			}
			return &agent.ToolResponse{Output: "0\n"}
		}
		return nil
	}
	reinstall := func() *store.Job {
		t.Helper()
		mu.Lock()
		during = nil
		mu.Unlock()
		return f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "percona"})
	}
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(during, "; ")
	}
	record := func() *store.DBInstance {
		t.Helper()
		inst, err := f.db.GetDBInstance(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}

	// A reinstall that succeeds: in service all along, the new version after.
	if job := reinstall(); job.Status != store.JobDone {
		t.Fatalf("reinstall: %s %s", job.Status, job.Error)
	}
	if got := seen(); got != "ready 8.4.11-11" {
		t.Fatalf("while reinstalling, the panel saw %q, want the ready server", got)
	}
	if inst := record(); inst.Status != store.DBReady || inst.Version != "8.4.12-12" || inst.LastError != "" {
		t.Fatalf("after the reinstall: %+v", inst)
	}

	// A package error: the old server still answers, so it stays in service.
	f.agent.InstallFails = map[string]string{"percona-server-server": "E: Failed to fetch percona-server-server"}
	job := reinstall()
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "Failed to fetch") {
		t.Fatalf("a failing reinstall: %s %s", job.Status, job.Error)
	}
	if got := seen(); got != "ready 8.4.12-12" {
		t.Fatalf("while reinstalling, the panel saw %q", got)
	}
	if inst := record(); inst.Status != store.DBReady || inst.Version != "8.4.12-12" {
		t.Fatalf("a server that still answers must stay in service: %+v", inst)
	}
	if _, err := f.s.dbInstance(f.ctx); err != nil {
		t.Fatalf("databases must keep working after a failed reinstall: %v", err)
	}

	// The same error with the server gone: the record says so.
	mu.Lock()
	answers = false
	mu.Unlock()
	if job := reinstall(); job.Status != store.JobFailed {
		t.Fatalf("a failing reinstall: %s", job.Status)
	}
	if inst := record(); inst.Status != store.DBError || !strings.Contains(inst.LastError, "Failed to fetch") {
		t.Fatalf("a server that no longer answers must be marked as failed: %+v", inst)
	}

	// From there it is an install again: "installing" during the run, ready after.
	f.agent.InstallFails = nil
	mu.Lock()
	answers = true
	mu.Unlock()
	if job := reinstall(); job.Status != store.JobDone {
		t.Fatalf("install over a failed server: %s %s", job.Status, job.Error)
	}
	if got := seen(); !strings.HasPrefix(got, "unusable: ") {
		t.Fatalf("a server that was not ready is not in service during its install: %q", got)
	}
	if inst := record(); inst.Status != store.DBReady || inst.LastError != "" {
		t.Fatalf("after the install: %+v", inst)
	}
}

// A reinstall that wrote zz-monopanel.cnf and then could not restart the
// server leaves the record matching the file. Here nothing uses
// mysql_native_password any more and the new file goes without it: a record
// still claiming the plugin would let databases-create skip the restart a
// legacy account then needs. A file mysqld refused never replaced the old
// one, so then the record keeps the plugin.
func TestDBReinstallRecordFollowsTheConfigFile(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name != "mysql" {
			return nil
		}
		if req.Stdin == nativePasswordUsers {
			return &agent.ToolResponse{Output: "0\n"}
		}
		return &agent.ToolResponse{Output: "8.4.11-11\n"}
	}
	for _, c := range []struct {
		name   string
		status int // of the failing config apply; 0 — the default 502
		native bool
	}{
		{"restart failed after the write", 0, false},
		{"file refused by mysqld", http.StatusUnprocessableEntity, true},
	} {
		if err := f.db.UpsertDBInstance(f.ctx, elPercona("8.4.11-11", true)); err != nil {
			t.Fatal(err)
		}
		f.agent.Fail["/v1/config/apply"] = "files applied but restart failed"
		f.agent.FailStatus = map[string]int{}
		if c.status != 0 {
			f.agent.FailStatus["/v1/config/apply"] = c.status
		}
		if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "percona"}); job.Status != store.JobFailed {
			t.Fatalf("%s: the reinstall must fail: %s", c.name, job.Status)
		}
		inst, err := f.db.GetDBInstance(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Status != store.DBReady || inst.NativePassword != c.native {
			t.Errorf("%s: record %+v, want ready with native_password %v", c.name, inst, c.native)
		}
	}
}

// A reinstall recomputes zz-monopanel.cnf: an account that signs in with
// mysql_native_password (made with legacy_auth on a host without legacy PHP)
// keeps the plugin on — turning it off would lock that account out.
func TestDBReinstallKeepsNativePasswordForItsAccounts(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	if err := f.db.UpsertDBInstance(f.ctx, elPercona("8.4.11-11", true)); err != nil {
		t.Fatal(err)
	}
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	legacy := &store.Database{UserID: alex.ID, Name: "alex_old"}
	if err := f.db.CreateDatabase(f.ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateDBUser(f.ctx, &store.DBUser{UserID: alex.ID, DatabaseID: &legacy.ID, Name: "alex_old", AuthPlugin: "mysql_native_password"}); err != nil {
		t.Fatal(err)
	}
	// MySQL itself counts no such account: the panel's own record of alex_old
	// is what must keep the plugin on.
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name != "mysql" {
			return nil
		}
		if req.Stdin == nativePasswordUsers {
			return &agent.ToolResponse{Output: "0\n"}
		}
		return &agent.ToolResponse{Output: "8.4.11-11\n"}
	}
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "percona"}); job.Status != store.JobDone {
		t.Fatalf("reinstall: %s %s", job.Status, job.Error)
	}
	conf, _ := lastDBConfig(t, f)
	if !strings.Contains(conf, "mysql_native_password = ON") {
		t.Fatalf("the reinstall switched mysql_native_password off under alex_old:\n%s", conf)
	}
	if inst, _ := f.db.GetDBInstance(f.ctx); !inst.NativePassword {
		t.Fatalf("the record lost native_password: %+v", inst)
	}
}
