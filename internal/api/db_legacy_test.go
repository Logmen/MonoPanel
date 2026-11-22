package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// configApplies counts the zz-monopanel.cnf writes (each one restarts MySQL).
func (f *siteFixture) dbConfigApplies() int {
	n := 0
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "zz-monopanel.cnf") {
			n++
		}
	}
	return n
}

// POST /db/engine/tune recalculates zz-monopanel.cnf, and with it whether
// mysql_native_password is loaded. It must never switch the plugin off while
// an account signs in with it — one the panel made with legacy_auth, or one
// only MySQL knows about — and it records what it wrote, so the startup
// refresh and databases-create decide from the truth.
func TestDBTuneKeepsNativePasswordWhileAccountsUseIt(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	// What MySQL answers to "how many accounts sign in with the plugin".
	var mu sync.Mutex
	count, countFails := "0", false
	answer := func(c string, fails bool) {
		mu.Lock()
		defer mu.Unlock()
		count, countFails = c, fails
	}
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name != "mysql" || req.Stdin != nativePasswordUsers {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if countFails {
			return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 2002 (HY000): Can't connect to local MySQL server"}
		}
		return &agent.ToolResponse{Output: count + "\n"}
	}
	native := func() (bool, bool) {
		t.Helper()
		conf, _ := lastDBConfig(t, f)
		inst, err := f.db.GetDBInstance(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(conf, "mysql_native_password = ON"), inst.NativePassword
	}

	// No legacy PHP on the host: the account made with legacy_auth is what
	// switched the plugin on, and tune keeps it.
	var created apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "old", "user": "alex", "legacy_auth": true}, http.StatusCreated, &created)
	if conf, rec := native(); !conf || !rec {
		t.Fatalf("legacy_auth did not switch mysql_native_password on: file %v, record %v", conf, rec)
	}
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	if conf, rec := native(); !conf || !rec {
		t.Fatalf("tune switched mysql_native_password off under alex_old: file %v, record %v", conf, rec)
	}

	// The panel forgot the database, MySQL still has such an account.
	if err := f.db.DeleteDatabase(f.ctx, created.Database.ID); err != nil {
		t.Fatal(err)
	}
	answer("1", false)
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	if conf, rec := native(); !conf || !rec {
		t.Fatalf("tune switched mysql_native_password off under an account MySQL reports: file %v, record %v", conf, rec)
	}

	// A server that cannot be asked keeps the plugin as it was.
	answer("", true)
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	if conf, rec := native(); !conf || !rec {
		t.Fatalf("without an answer the plugin must stay on: file %v, record %v", conf, rec)
	}

	// Nobody uses it any more: off, in the file and in the record.
	answer("0", false)
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	if conf, rec := native(); conf || rec {
		t.Fatalf("an unused mysql_native_password stays on: file %v, record %v", conf, rec)
	}
}

// An ordinary account must not restart MySQL for the whole server: a
// database with legacy authentication while mysql_native_password is off is
// refused with 403 (the administrator decides about the restart); once the
// plugin is on, the account may use it, and nothing restarts.
func TestLegacyAuthRestartIsTheAdministratorsCall(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")

	before := f.dbConfigApplies()
	status, body := f.do(http.MethodPost, "/databases", map[string]any{"name": "old", "legacy_auth": true})
	if status != http.StatusForbidden || !strings.Contains(string(body), "ask the administrator to enable legacy authentication") {
		t.Fatalf("legacy_auth by an ordinary account with the plugin off: %d %s", status, body)
	}
	if f.dbConfigApplies() != before || f.agent.UnitAction("mysql.service") != "" {
		t.Fatal("MySQL was reconfigured or restarted for an ordinary account")
	}
	if dbs, _ := f.db.ListDatabases(f.ctx, 0); len(dbs) != 0 {
		t.Fatalf("a refused database was made anyway: %+v", dbs)
	}
	// A regular account is no restart.
	var plain apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "new"}, http.StatusCreated, &plain)
	if u := plain.Database.Users; len(u) != 1 || u[0].AuthPlugin != "caching_sha2_password" {
		t.Fatalf("regular account: %+v", u)
	}

	// The administrator may switch it on: the restart is theirs to decide.
	f.login()
	f.call(http.MethodPost, "/databases", map[string]any{"name": "old", "user": "alex", "legacy_auth": true}, http.StatusCreated, nil)
	if f.agent.UnitAction("mysql.service") != "restart" {
		t.Fatal("enabling mysql_native_password must restart MySQL")
	}

	// With the plugin on, the account uses it and nothing restarts.
	f.agent.Reset()
	f.loginAs("alex", "alex-password")
	var legacy apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "old2", "legacy_auth": true}, http.StatusCreated, &legacy)
	if u := legacy.Database.Users; len(u) != 1 || u[0].AuthPlugin != "mysql_native_password" {
		t.Fatalf("legacy account with the plugin on: %+v", u)
	}
	if f.dbConfigApplies() != 0 || f.agent.UnitAction("mysql.service") != "" {
		t.Fatal("MySQL was restarted although mysql_native_password was already on")
	}
}

// Without legacy_auth in the request the default follows the account's
// sites; for an ordinary account on PHP below 7.4 that default would restart
// MySQL as well, so it is refused the same way, and legacy_auth=false still
// makes a regular account.
func TestLegacyAuthDefaultIsRefusedToOrdinaryAccounts(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	alex := f.alexWithPassword()
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: alex.ID, Domain: "old.example.com", Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: "7.2", Docroot: "public_html", PHPIni: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password")
	status, body := f.do(http.MethodPost, "/databases", map[string]any{"name": "shop"})
	if status != http.StatusForbidden || !strings.Contains(string(body), "legacy_auth=false makes a regular account") {
		t.Fatalf("the legacy default for an ordinary account with the plugin off: %d %s", status, body)
	}
	var plain apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "shop", "legacy_auth": false}, http.StatusCreated, &plain)
	if u := plain.Database.Users; len(u) != 1 || u[0].AuthPlugin != "caching_sha2_password" {
		t.Fatalf("legacy_auth=false: %+v", u)
	}
	if f.agent.UnitAction("mysql.service") != "" {
		t.Fatal("MySQL was restarted for an ordinary account")
	}
}

// A CMS installer makes its database the way POST /databases does, legacy
// authentication following the owner's sites. For an ordinary account on PHP
// below 7.4 that would switch mysql_native_password on and restart MySQL for
// every site, so the request is refused up front, like the database itself;
// with the plugin already on, the install goes ahead.
func TestCMSInstallDoesNotRestartMySQLForAnOrdinaryAccount(t *testing.T) {
	f := cmsFixture(t, nil)
	alex := f.alexWithPassword()
	if err := f.db.UpsertPHPVersion(f.ctx, &store.PHPVersion{Version: "7.2", Source: "sury", Status: store.PHPInstalled}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: alex.ID, Domain: "old.example.com", Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: "7.2", Docroot: "public_html", PHPIni: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password")
	before := f.dbConfigApplies()
	status, body := f.do(http.MethodPost, "/sites/old.example.com/cms", map[string]any{"cms": "wordpress"})
	if status != http.StatusForbidden || !strings.Contains(string(body), "ask the administrator to enable legacy authentication") {
		t.Fatalf("a CMS on PHP 7.2 for an ordinary account with the plugin off: %d %s", status, body)
	}
	if jobs, _ := f.db.ListJobs(f.ctx, 10, ""); len(jobs) != 0 {
		t.Fatalf("a refused install queued a job: %+v", jobs)
	}
	if f.dbConfigApplies() != before || f.agent.UnitAction("mysql.service") != "" {
		t.Fatal("MySQL was reconfigured or restarted for an ordinary account")
	}

	inst, err := f.db.GetDBInstance(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.NativePassword = true
	if err := f.db.UpsertDBInstance(f.ctx, inst); err != nil {
		t.Fatal(err)
	}
	if status, body := f.do(http.MethodPost, "/sites/old.example.com/cms", map[string]any{"cms": "wordpress"}); status != http.StatusAccepted {
		t.Fatalf("with mysql_native_password on the install goes ahead: %d %s", status, body)
	}
}
