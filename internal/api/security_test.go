package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// bearerGet sends GET with a Bearer token and returns the status.
func (f *siteFixture) bearerGet(token, path string) int {
	f.t.Helper()
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, f.ts.URL+"/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func (f *siteFixture) alexWithPassword() *store.User {
	f.t.Helper()
	h, _ := auth.HashPassword("alex-password")
	u, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.SetUserPassword(f.ctx, u.ID, h); err != nil {
		f.t.Fatal(err)
	}
	return u
}

// An ordinary account must not be able to give itself a migration scope: such a
// token reads another account's password hashes, keys, files and databases.
func TestMigrationScopeIsForAdministrators(t *testing.T) {
	f := newSiteFixture(t)
	alex := f.alexWithPassword()
	victim := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, Home: "/var/www/bob"}
	if err := f.db.CreateUser(f.ctx, victim); err != nil {
		t.Fatal(err)
	}

	f.loginAs("alex", "alex-password")
	for _, scope := range []string{"migrate:user:bob", "migrate:user:admin", " migrate:user:bob"} {
		f.call(http.MethodPost, "/tokens", map[string]any{"name": "esc", "scopes": []string{scope}}, http.StatusForbidden, nil)
	}
	// Labels stay free-form.
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "label", "scopes": []string{"billing"}}, http.StatusCreated, nil)

	// A token forged before the fix (written straight into the table) opens
	// nothing: the handlers want the administrator role, whatever the scope.
	forged := "forged-token-value-for-the-test"
	if err := f.db.CreateAPIToken(f.ctx, &store.APIToken{UserID: alex.ID, Name: "forged", Hash: auth.HashToken(forged), Scopes: []string{"migrate:user:bob", "migrate:user:admin"}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/migrate/plan?scope=user:bob",
		"/migrate/state?scope=user:bob",
		"/migrate/state?scope=user:admin",
		"/migrate/files?scope=user:bob&part=home",
		"/migrate/dump?scope=user:bob&db=bob_db",
	} {
		if got := f.bearerGet(forged, path); got != http.StatusForbidden {
			t.Errorf("forged migration token: GET %s → %d, want 403", path, got)
		}
	}

	// On start the panel revokes such tokens and says so in the audit log;
	// the label token of the same account stays.
	f.s.revokeForgedMigrationTokens(f.ctx)
	list, err := f.db.ListAPITokens(f.ctx, alex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "label" {
		t.Fatalf("tokens of alex after the start-up check: %+v", list)
	}
	if got := f.bearerGet(forged, "/auth/me"); got != http.StatusUnauthorized {
		t.Fatalf("revoked token still authenticates: %d", got)
	}
	entries, err := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "token.revoke", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Actor != "system" || entries[0].Target != "forged" {
		t.Fatalf("audit of the revocation: %+v", entries)
	}

	// An administrator may still put a migration scope on its own token, and
	// the grant keeps working: both belong to an administrator.
	f.login()
	var created struct {
		Token string `json:"token"`
	}
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "own-move", "scopes": []string{"migrate:user:bob"}}, http.StatusCreated, &created)
	if got := f.bearerGet(created.Token, "/migrate/plan?scope=user:bob"); got != http.StatusOK {
		t.Fatalf("administrator's scoped token: %d", got)
	}
	// …but not onto a token it issues for an ordinary account.
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "for-alex", "user": "alex", "scopes": []string{"migrate:user:bob"}}, http.StatusForbidden, nil)

	var grant struct {
		Token string `json:"token"`
	}
	f.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:bob", "hours": 1}, http.StatusCreated, &grant)
	if got := f.bearerGet(grant.Token, "/migrate/state?scope=user:bob"); got != http.StatusOK {
		t.Fatalf("grant token: %d", got)
	}
	if got := f.bearerGet(grant.Token, "/migrate/state?scope=user:alex"); got != http.StatusForbidden {
		t.Fatalf("grant token on another account: %d", got)
	}
	entries, err = f.db.ListAudit(f.ctx, store.AuditFilter{Action: "migrate.", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	if !seen["migrate.state"] || !seen["migrate.grant"] {
		t.Fatalf("migration audit entries: %+v", entries)
	}
}

// The file and database streams of a migration leave a trace in the audit log
// like the state does.
func TestMigrationStreamsAreAudited(t *testing.T) {
	f := newSiteFixture(t)
	var grant struct {
		Token string `json:"token"`
	}
	f.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:alex", "hours": 1}, http.StatusCreated, &grant)
	if got := f.bearerGet(grant.Token, "/migrate/files?scope=user:alex&part=home"); got != http.StatusOK {
		t.Fatalf("files stream: %d", got)
	}
	entries, err := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "migrate.files", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Target != "alex" {
		t.Fatalf("audit of the files stream: %+v", entries)
	}
}

// Setting 2FA up again must not switch it off: that is what disable is for,
// and disable asks for the password.
func TestTOTPSetupDoesNotDisable(t *testing.T) {
	f := newSiteFixture(t)
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")

	var setup struct {
		Secret string `json:"secret"`
	}
	f.call(http.MethodPost, "/auth/totp/setup", nil, http.StatusOK, &setup)
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/auth/totp/enable", map[string]any{"code": code}, http.StatusOK, nil)

	f.call(http.MethodPost, "/auth/totp/setup", nil, http.StatusConflict, nil)
	var st struct {
		Enabled bool `json:"enabled"`
	}
	f.call(http.MethodGet, "/auth/totp", nil, http.StatusOK, &st)
	if !st.Enabled {
		t.Fatal("a second setup switched 2FA off")
	}

	// The documented way still works: disable with the password, then set up.
	f.call(http.MethodPost, "/auth/totp/disable", map[string]any{"password": "wrong"}, http.StatusUnauthorized, nil)
	f.call(http.MethodPost, "/auth/totp/disable", map[string]any{"password": "alex-password"}, http.StatusOK, nil)
	f.call(http.MethodPost, "/auth/totp/setup", nil, http.StatusOK, nil)
}

// A new password ends the account's other browser sessions; the session that
// changed it stays. A suspension ends all of them.
func TestPasswordChangeEndsSessions(t *testing.T) {
	f := newSiteFixture(t)
	alex := f.alexWithPassword()

	f.loginAs("alex", "alex-password")
	first := f.cookie
	f.loginAs("alex", "alex-password")
	second := f.cookie

	f.cookie = second
	f.call(http.MethodPatch, "/users/alex", map[string]any{"password": "new-alex-password", "current_password": "alex-password"}, http.StatusOK, nil)
	f.call(http.MethodGet, "/auth/me", nil, http.StatusOK, nil)
	f.cookie = first
	f.call(http.MethodGet, "/auth/me", nil, http.StatusUnauthorized, nil)

	// An administrator's reset ends every session of the account.
	f.loginAs("alex", "new-alex-password")
	userSession := f.cookie
	f.login()
	f.call(http.MethodPatch, "/users/alex", map[string]any{"password": "reset-by-admin"}, http.StatusOK, nil)
	f.cookie = userSession
	f.call(http.MethodGet, "/auth/me", nil, http.StatusUnauthorized, nil)

	// So does a suspension.
	f.loginAs("alex", "reset-by-admin")
	userSession = f.cookie
	f.login()
	f.call(http.MethodPatch, "/users/alex", map[string]any{"status": "suspended"}, http.StatusOK, nil)
	var n int
	if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM sessions WHERE user_id=?`, alex.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d sessions of a suspended account left", n)
	}
	f.cookie = userSession
	f.call(http.MethodGet, "/auth/me", nil, http.StatusUnauthorized, nil)
}

// Job payloads keep the encrypted secrets the runner needs; the job list and
// the job page do not show them.
func TestJobPayloadSecretsAreHidden(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol", "password": "carol-password-1"}, http.StatusAccepted, nil)

	var list []*store.Job
	f.call(http.MethodGet, "/jobs?limit=10", nil, http.StatusOK, &list)
	var id int64
	for _, j := range list {
		if j.Type == "user.provision" {
			id = j.ID
			if strings.Contains(string(j.Payload), "password_enc\":\"") && !strings.Contains(string(j.Payload), "[redacted]") {
				t.Fatalf("job list shows the encrypted password: %s", j.Payload)
			}
		}
	}
	if id == 0 {
		t.Fatalf("no user.provision job in %+v", list)
	}
	var one store.Job
	f.call(http.MethodGet, "/jobs/"+itoa(id), nil, http.StatusOK, &one)
	var payload map[string]any
	if err := json.Unmarshal(one.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if v, ok := payload["password_enc"]; ok && v != "[redacted]" {
		t.Fatalf("job page shows the encrypted password: %v", v)
	}
	if payload["login"] != "carol" {
		t.Fatalf("redaction ate the rest of the payload: %v", payload)
	}

	// The stored job is untouched: the runner still has what it needs.
	stored, err := f.db.GetJob(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Payload), "password_enc") || strings.Contains(string(stored.Payload), "[redacted]") {
		t.Fatalf("stored payload changed: %s", stored.Payload)
	}
}

func TestRedactJobNested(t *testing.T) {
	j := &store.Job{Payload: json.RawMessage(`{"login":"x","source":"https://a","token":"plain","key_enc":"","inner":{"admin_password_enc":"c2VjcmV0"},"list":[{"secret":"s"}]}`)}
	got := string(redactJob(j).Payload)
	for _, leak := range []string{"plain", "c2VjcmV0", `"secret":"s"`} {
		if strings.Contains(got, leak) {
			t.Fatalf("redacted payload still has %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"login":"x"`) || !strings.Contains(got, `"key_enc":""`) {
		t.Fatalf("redaction changed plain fields: %s", got)
	}
	if string(j.Payload) == got {
		t.Fatal("redactJob changed nothing")
	}
}

// An account must not take a system account's name (provisioning would adopt
// root or www-data and change its shell and groups) nor a name the panel
// signs its own jobs and audit entries with (it would see them as its own).
func TestUsersCreateRefusesReservedLogins(t *testing.T) {
	f := newSiteFixture(t)
	before, _ := f.db.ListUsers(f.ctx)
	for _, login := range []string{"root", "daemon", "scheduler", "system"} {
		status, body := f.do(http.MethodPost, "/users", map[string]any{"login": login, "password": "Made-Up-Pass-1"})
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", login, status, body)
		}
	}
	if users, _ := f.db.ListUsers(f.ctx); len(users) != len(before) {
		t.Fatalf("a refused account was made: %d users, %d before", len(users), len(before))
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "carol", "password": "Made-Up-Pass-1"}, http.StatusAccepted, nil)
}

// An ordinary account keeps to its own names: the panel's hostname and its
// mail server's (a site there would receive the administrators' panel cookie,
// which does not keep to a port), and nothing that is, or lies under, a site
// or mail domain of another account (this server delivers mail for its
// domains itself). An administrator may still place names anywhere.
func TestOrdinaryAccountsKeepToTheirOwnNames(t *testing.T) {
	f := newMailFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: alex.ID, Domain: "victim.example.com", Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: "8.4", Docroot: "public_html", PHPIni: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	f.userWithPassword("carol", "carol-password", 2001)
	f.loginAs("carol", "carol-password")
	why := func(body []byte) bool {
		return strings.Contains(string(body), "belongs to another account") || strings.Contains(string(body), "hostname of this panel itself") || strings.Contains(string(body), "already used by site")
	}
	for _, name := range []string{"mail.example.com", "shop.victim.example.com"} {
		if status, body := f.do(http.MethodPost, "/sites", map[string]any{"domain": name}); status != http.StatusUnprocessableEntity || !why(body) {
			t.Errorf("site %s for carol: %d %s", name, status, body)
		}
	}
	for _, name := range []string{"victim.example.com", "lists.victim.example.com", "mail.example.com"} {
		if status, body := f.do(http.MethodPost, "/mail/domains", map[string]any{"name": name}); (status != http.StatusForbidden && status != http.StatusUnprocessableEntity) || !why(body) {
			t.Errorf("mail domain %s for carol: %d %s", name, status, body)
		}
	}
	if status, body := f.do(http.MethodPost, "/mail/domains", map[string]any{"name": "carol.example.net"}); status != http.StatusCreated {
		t.Errorf("carol's own mail domain: %d %s", status, body)
	}
	// The owner itself may add mail for its own domain.
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")
	if status, body := f.do(http.MethodPost, "/mail/domains", map[string]any{"name": "victim.example.com"}); status != http.StatusCreated {
		t.Errorf("alex's mail for its own site: %d %s", status, body)
	}
}

// A migration scope written with blanks or in capitals restricts the token
// like the plain one: it must not leave an unrestricted administrator token.
func TestMigrationScopeSpellings(t *testing.T) {
	f := newSiteFixture(t)
	for _, sc := range []string{" migrate:user:alex", "MIGRATE:user:alex", "Migrate:User:alex "} {
		var res apitypes.CreateTokenResponse
		f.call(http.MethodPost, "/tokens", map[string]any{"name": "m", "scopes": []string{sc}}, http.StatusCreated, &res)
		if got := res.Record.Scopes; len(got) != 1 || !strings.HasPrefix(got[0], "migrate:") {
			t.Errorf("%q stored as %q", sc, got)
		}
		if status := f.bearerGet(res.Token, "/users"); status != http.StatusForbidden {
			t.Errorf("%q: a migration token reached GET /users: %d", sc, status)
		}
	}
	if !hasMigrationScope([]string{" MIGRATE:user:x"}) {
		t.Error("an old token spelled in capitals is not restricted")
	}
}

// Your own password needs the current one over a session or a token: a stolen
// session must not set a new password and then turn 2FA off with it. An
// administrator resetting another account's password needs none.
func TestOwnPasswordNeedsTheCurrentOne(t *testing.T) {
	f := newSiteFixture(t)
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")
	for _, body := range []map[string]any{
		{"password": "stolen-session-pass"},
		{"password": "stolen-session-pass", "current_password": "a-wrong-guess"},
	} {
		if status, out := f.do(http.MethodPatch, "/users/alex", body); status != http.StatusForbidden {
			t.Fatalf("%v: %d %s", body, status, out)
		}
	}
	// The old password still signs in.
	f.loginAs("alex", "alex-password")
	f.call(http.MethodPatch, "/users/alex", map[string]any{"password": "alex-password-2", "current_password": "alex-password"}, http.StatusOK, nil)
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "user.password", Limit: 10})
	if len(entries) != 2 {
		t.Errorf("refused attempts in the audit log: %d, want 2", len(entries))
	}
	// An administrator's reset of another account needs no current password.
	f.login()
	f.call(http.MethodPatch, "/users/alex", map[string]any{"password": "reset-by-admin-1"}, http.StatusOK, nil)
}
