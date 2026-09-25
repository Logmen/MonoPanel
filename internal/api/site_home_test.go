package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// lastHomeSpec is the last directory spec the panel sent for a path.
func lastHomeSpec(t *testing.T, f *siteFixture, path string) agent.DirSpec {
	t.Helper()
	var last agent.DirSpec
	found := false
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/dirs/ensure" {
			continue
		}
		var req agent.EnsureDirsRequest
		if err := json.Unmarshal(c.Body, &req); err != nil {
			t.Fatal(err)
		}
		for _, d := range req.Dirs {
			if d.Path == path {
				last, found = d, true
			}
		}
	}
	if !found {
		t.Fatalf("no directory spec for %s", path)
	}
	return last
}

// An SFTP-only account is chrooted into its home, and sshd refuses a chroot
// directory the client owns or can write to. Creating a site and mp site fix
// used to hand the home over to the client, after which the account could
// not log in; the home stays root:<login> 0750 now. A shell account's home
// is its own, as before.
func TestSiteKeepsSFTPOnlyHomeRootOwned(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "sftpbob"}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("provision: %s %s", job.Status, job.Error)
	}
	want := agent.DirSpec{Path: "/var/www/sftpbob", Mode: 0o750, Owner: "root", Group: "sftpbob"}
	if got := lastHomeSpec(t, f, want.Path); got != want {
		t.Fatalf("provisioned home: %+v", got)
	}

	f.createSite(map[string]any{"domain": "sftp.example.com", "user": "sftpbob", "php_version": "8.4", "ssl": "none"})
	if got := lastHomeSpec(t, f, want.Path); got != want {
		t.Fatalf("home after creating a site: %+v, want %+v", got, want)
	}
	var fix apitypes.SiteWithJob
	f.call(http.MethodPost, "/sites/sftp.example.com/fix", nil, http.StatusAccepted, &fix)
	if job := f.waitJob(fix.JobID); job.Status != store.JobDone {
		t.Fatalf("fix: %s %s", job.Status, job.Error)
	}
	if got := lastHomeSpec(t, f, want.Path); got != want {
		t.Fatalf("home after mp site fix: %+v, want %+v", got, want)
	}

	f.createSite(map[string]any{"domain": "shell.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if got := lastHomeSpec(t, f, "/var/www/alex"); got.Owner != "alex" || got.Mode != 0o710 {
		t.Fatalf("a shell account's home: %+v", got)
	}
	// The web group's read access comes from the site ACL operation now.
	acl := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/acl/site" {
			acl = true
		}
	}
	if !acl {
		t.Fatal("the site tree got no ACL")
	}
}

// Homes that an older panel handed over to their SFTP-only clients are put
// back at startup; shell accounts are left alone.
func TestRefreshSFTPHomes(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	var created apitypes.UserWithJob
	f.call(http.MethodPost, "/users", map[string]any{"login": "sftpbob"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.agent.Reset()
	f.s.refreshSFTPHomes(f.ctx)
	if got := lastHomeSpec(t, f, "/var/www/sftpbob"); got != (agent.DirSpec{Path: "/var/www/sftpbob", Mode: 0o750, Owner: "root", Group: "sftpbob"}) {
		t.Fatalf("sftp home: %+v", got)
	}
	groupEntry := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/dirs/ensure" && strings.Contains(string(c.Body), "/var/www/alex\"") {
			t.Fatal("a shell account's home was touched")
		}
		if c.Path == "/v1/acl/set" && strings.Contains(string(c.Body), "/var/www/sftpbob\"") && strings.Contains(string(c.Body), "g::r-x") {
			groupEntry = true
		}
	}
	if !groupEntry {
		t.Fatal("the chroot root is not listable for its client")
	}
}
