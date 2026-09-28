package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/config"
	"monopanel/internal/store"
)

// The audit log is visible: the administrator sees everything, an account
// only its own lines, and the file mirror carries the same entries.
func TestAuditLogIsVisibleAndMirrored(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "audit.log")
	f := newFixture(t, func(c *config.Config) { c.Log.AuditFile = file })
	f.login()
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "irina", "password": "irina-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)

	var all []store.AuditEntry
	f.call(http.MethodGet, "/system/audit", nil, http.StatusOK, &all)
	if len(all) < 2 || all[0].Action != "user.create" || all[0].Actor != "admin" {
		t.Fatalf("admin view: %+v", all)
	}
	var logins []store.AuditEntry
	f.call(http.MethodGet, "/system/audit?action=auth.", nil, http.StatusOK, &logins)
	for _, e := range logins {
		if !strings.HasPrefix(e.Action, "auth.") {
			t.Fatalf("action filter let through %s", e.Action)
		}
	}
	if len(logins) == 0 || logins[len(logins)-1].Action != "auth.login" {
		t.Fatalf("the admin's own sign-in is missing: %+v", logins)
	}

	// The account sees its own entries only, whatever it asks for.
	f.loginAs("irina", "irina-password-1")
	var own []store.AuditEntry
	f.call(http.MethodGet, "/system/audit?actor=admin", nil, http.StatusOK, &own)
	for _, e := range own {
		if e.Actor != "irina" {
			t.Fatalf("an account saw %s's entry %s", e.Actor, e.Action)
		}
	}
	if len(own) == 0 || own[0].Action != "auth.login" {
		t.Fatalf("the account's sign-in is missing: %+v", own)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(all)+1 || !strings.Contains(lines[len(lines)-1], `"actor":"irina"`) || !strings.Contains(lines[0], `"action":"auth.login"`) {
		t.Fatalf("file mirror: %d lines for %d entries: %s", len(lines), len(all)+1, lines[len(lines)-1])
	}
}
