package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// logrotateApplies returns the positions, among all agent calls, of the
// configuration sets that wrote the site log rotation, and that rotation's
// validators.
func (f *siteFixture) logrotateApplies() (at []int, validate [][]string) {
	for i, c := range f.agent.Calls() {
		if c.Path != "/v1/config/apply" {
			continue
		}
		var req agent.ApplyConfigSetRequest
		if json.Unmarshal(c.Body, &req) != nil {
			continue
		}
		for _, file := range req.Files {
			if file.Path == siteLogrotatePath {
				at, validate = append(at, i), req.Validate
			}
		}
	}
	return at, validate
}

// Site logs live in the accounts' homes, where nothing else rotates them:
// every account gets a stanza that rotates under its own rights, checked by
// logrotate before it goes live, and the stanza leaves before the unix user.
func TestSiteLogsRotateUnderTheirAccount(t *testing.T) {
	f := newSiteFixture(t)
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "bob"}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("provision: %s %s", job.Status, job.Error)
	}
	conf, ok := f.agent.File(siteLogrotatePath)
	if !ok {
		t.Fatal("site logs got no rotation")
	}
	for _, want := range []string{
		"/var/www/bob/data/logs/*.access.log /var/www/bob/data/logs/*.error.log /var/www/bob/data/logs/*.php.slow.log {", "su bob bob",
		"create 0660 alex alex", "su alex alex",
		`kill -USR1 "$(cat /run/nginx.pid)"`, `kill -USR1 "$(cat /run/apache2/apache2.pid)"`,
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("rotation lacks %q:\n%s", want, conf)
		}
	}
	if _, validate := f.logrotateApplies(); !slices.ContainsFunc(validate, func(argv []string) bool {
		return slices.Equal(argv, []string{"/usr/sbin/logrotate", "--debug", siteLogrotatePath})
	}) {
		t.Fatalf("the rotation must be checked by logrotate before it goes live: %v", validate)
	}

	// After USR1 the nginx workers reopen the logs themselves: the web group
	// must pass through the account's data/logs.
	f.createSite(map[string]any{"domain": "bob.example.com", "user": "bob", "php_version": "8.4", "ssl": "none"})
	if !slices.ContainsFunc(f.agent.Calls(), func(c agenttest.Call) bool {
		var req agent.SetACLRequest
		return c.Path == "/v1/acl/set" && json.Unmarshal(c.Body, &req) == nil &&
			req.Path == "/var/www/bob/data/logs" && slices.Equal(req.Entries, []string{"g:monopanel-web:x"})
	}) {
		t.Fatal("the web group cannot pass through data/logs: nginx workers would keep writing into rotated logs")
	}
	for _, name := range []string{"bob.example.com.access.log", "bob.example.com.error.log", "bob.example.com.php.error.log", "bob.example.com.php.slow.log"} {
		p := "/var/www/bob/data/logs/" + name
		created := slices.ContainsFunc(f.agent.Calls(), func(c agenttest.Call) bool {
			var req agent.EnsureFileRequest
			return c.Path == "/v1/file/ensure" && json.Unmarshal(c.Body, &req) == nil && req.Path == p && req.OnlyIfMissing && req.Owner == "bob" && req.Mode == 0o660
		})
		handed := slices.ContainsFunc(f.agent.Calls(), func(c agenttest.Call) bool {
			var req agent.ChownRequest
			return c.Path == "/v1/chown" && json.Unmarshal(c.Body, &req) == nil && req.Path == p && req.Owner == "bob" && req.Group == "bob" && req.Mode == 0o660
		})
		if !created || !handed {
			t.Fatalf("%s must be the account's with mode 0660 before nginx opens it: created=%v handed=%v", name, created, handed)
		}
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/users/bob", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("delete: %s %s", job.Status, job.Error)
	}
	if conf, _ := f.agent.File(siteLogrotatePath); strings.Contains(conf, "bob") || !strings.Contains(conf, "su alex alex") {
		t.Fatalf("bob must leave the rotation, alex stay:\n%s", conf)
	}
	at, _ := f.logrotateApplies()
	removed := slices.IndexFunc(f.agent.Calls(), func(c agenttest.Call) bool { return c.Path == "/v1/user/remove" })
	if removed < 0 || at[len(at)-1] > removed {
		t.Fatalf("the rotation must drop bob before his unix user goes: rotation at %v, user removed at %d", at, removed)
	}
}
