package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// userWithPassword adds an ordinary account that can sign in.
func (f *siteFixture) userWithPassword(login, password string, uid int) *store.User {
	f.t.Helper()
	h, err := auth.HashPassword(password)
	if err != nil {
		f.t.Fatal(err)
	}
	gid := uid
	u := &store.User{Login: login, Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/" + login, PasswordHash: h}
	if err := f.db.CreateUser(f.ctx, u); err != nil {
		f.t.Fatal(err)
	}
	return u
}

// Logins may hold "_": alex asking for x_shop spells alex_x_shop, the
// database alex_x made as shop. Creating it must not hand alex_x's database
// to alex with a new password (CREATE … IF NOT EXISTS, then ALTER USER) —
// not for alex, not for an administrator acting for alex, and not through an
// account of that name either; the owner itself still re-keys its own.
func TestDatabaseOfAnotherAccountIsNotTakenOver(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	victim := f.userWithPassword("alex_x", "victim-password", 1501)
	f.loginAs("alex_x", "victim-password")
	var made apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "shop"}, http.StatusCreated, &made)
	if made.Database.Name != "alex_x_shop" {
		t.Fatalf("database %q", made.Database.Name)
	}

	refused := func(who string, body map[string]any) {
		t.Helper()
		f.agent.Reset()
		status, out := f.do(http.MethodPost, "/databases", body)
		if status != http.StatusConflict || !strings.Contains(string(out), "belongs to a database of another account") {
			t.Fatalf("%s spelling another account's database: %d %s", who, status, out)
		}
		for _, tool := range f.agent.Tools() {
			if tool.Name == "mysql" {
				t.Fatalf("%s: MySQL was asked to make or re-key it: %q", who, tool.Stdin)
			}
		}
		db, err := f.db.GetDatabaseByName(f.ctx, "alex_x_shop")
		if err != nil || db.UserID != victim.ID {
			t.Fatalf("%s: the database changed hands: %+v %v", who, db, err)
		}
	}
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")
	refused("alex", map[string]any{"name": "x_shop", "legacy_auth": false})
	f.login()
	refused("an administrator for alex", map[string]any{"name": "x_shop", "user": "alex"})

	// An account of that name made for another database (a migration keeps
	// the source's account names) is just as much alex_x's.
	other := &store.Database{UserID: victim.ID, Name: "alex_x_blog"}
	if err := f.db.CreateDatabase(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateDBUser(f.ctx, &store.DBUser{UserID: victim.ID, DatabaseID: &other.ID, Name: "alex_x_wp", Host: "localhost"}); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password")
	status, out := f.do(http.MethodPost, "/databases", map[string]any{"name": "x_wp"})
	if status != http.StatusConflict {
		t.Fatalf("alex spelling another account's database account: %d %s", status, out)
	}

	// Recorded for another account while MySQL was making it: the record is
	// not handed over either (nor the generated password with it).
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" && strings.Contains(req.Stdin, "CREATE DATABASE IF NOT EXISTS `alex_x_new`") {
			if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: victim.ID, Name: "alex_x_new"}); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	status, out = f.do(http.MethodPost, "/databases", map[string]any{"name": "x_new"})
	if status != http.StatusConflict || strings.Contains(string(out), `"password"`) {
		t.Fatalf("a database recorded for another account meanwhile: %d %s", status, out)
	}
	if db, err := f.db.GetDatabaseByName(f.ctx, "alex_x_new"); err != nil || db.UserID != victim.ID {
		t.Fatalf("the database recorded meanwhile changed hands: %+v %v", db, err)
	}
	f.agent.ToolHook = nil

	// The owner asking again re-keys its own database, as before.
	f.loginAs("alex_x", "victim-password")
	var again apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "shop"}, http.StatusCreated, &again)
	if again.Database.ID != made.Database.ID || again.Password == "" || again.Password == made.Password {
		t.Fatalf("the owner re-keying its database: %+v", again)
	}
}

// A database's own account is granted on that database literally: in a
// database-level GRANT _ is a wildcard, and the login "my" with the
// database "ql" (my_ql) would open `mysql` itself, the account becoming
// MySQL's root after its next restart (review R1, P1).
func TestDatabaseGrantIsLiteral(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	f.userWithPassword("my", "my-password-1", 1502)
	f.loginAs("my", "my-password-1")
	f.agent.Reset()
	f.call(http.MethodPost, "/databases", map[string]any{"name": "ql"}, http.StatusCreated, nil)
	var sql string
	for _, tool := range f.agent.Tools() {
		if tool.Name == "mysql" {
			sql += tool.Stdin
		}
	}
	if !strings.Contains(sql, "GRANT ALL PRIVILEGES ON `my\\_ql`.* TO 'my_ql'@'localhost';") || strings.Contains(sql, "ON `my_ql`") {
		t.Fatalf("the grant is a pattern (it matches `mysql`):\n%s", sql)
	}
}
