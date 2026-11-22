package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// GET /mail tells an ordinary account about its own mail only: the counts are
// of its domains, mailboxes and aliases, and the server-wide warnings stay
// with the administrator. mp mail status prints the same answer.
func TestMailStatusIsPerCaller(t *testing.T) {
	f := newMailFixture(t)
	f.alexWithPassword()
	h, _ := auth.HashPassword("other-password")
	for _, login := range []string{"bob", "carol"} {
		if err := f.db.CreateUser(f.ctx, &store.User{Login: login, Role: store.RoleUser, Status: store.UserActive, PasswordHash: h, Home: "/var/www/" + login}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []struct{ name, user string }{{"alex.example.com", "alex"}, {"shop.example.com", "alex"}, {"bob.example.com", "bob"}} {
		f.call(http.MethodPost, "/mail/domains", map[string]any{"name": d.name, "user": d.user}, http.StatusCreated, nil)
	}
	for _, box := range []string{"ivan@alex.example.com", "anna@bob.example.com", "petr@bob.example.com"} {
		f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": box}, http.StatusCreated, nil)
	}
	f.call(http.MethodPost, "/mail/aliases", map[string]any{"address": "info@alex.example.com", "destinations": []string{"ivan@alex.example.com"}}, http.StatusCreated, nil)
	// A server-wide warning for the administrator.
	f.call(http.MethodPut, "/mail/settings", map[string]any{"port25": false}, http.StatusOK, nil)

	status := func() apitypes.MailStatus {
		t.Helper()
		var st apitypes.MailStatus
		f.call(http.MethodGet, "/mail", nil, http.StatusOK, &st)
		return st
	}
	st := status()
	if st.Domains != 3 || st.Mailboxes != 3 || st.Aliases != 1 || !strings.Contains(strings.Join(st.Warnings, "\n"), "port 25 is off") {
		t.Fatalf("administrator: %d domains, %d mailboxes, %d aliases, warnings %v", st.Domains, st.Mailboxes, st.Aliases, st.Warnings)
	}

	f.loginAs("alex", "alex-password")
	st = status()
	if !st.Installed || st.Hostname != "mail.example.com" || st.Domains != 2 || st.Mailboxes != 1 || st.Aliases != 1 || len(st.Warnings) != 0 {
		t.Fatalf("alex: installed %v, host %q, %d domains, %d mailboxes, %d aliases, warnings %v", st.Installed, st.Hostname, st.Domains, st.Mailboxes, st.Aliases, st.Warnings)
	}

	f.loginAs("bob", "other-password")
	if st = status(); st.Domains != 1 || st.Mailboxes != 2 || st.Aliases != 0 || len(st.Warnings) != 0 {
		t.Fatalf("bob: %d domains, %d mailboxes, %d aliases, warnings %v", st.Domains, st.Mailboxes, st.Aliases, st.Warnings)
	}

	f.loginAs("carol", "other-password")
	if st = status(); st.Domains != 0 || st.Mailboxes != 0 || st.Aliases != 0 || strings.Join(st.Warnings, "|") != "no domains added yet" {
		t.Fatalf("carol: %d domains, %d mailboxes, %d aliases, warnings %v", st.Domains, st.Mailboxes, st.Aliases, st.Warnings)
	}
}
