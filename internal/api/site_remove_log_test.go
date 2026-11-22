package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// The removal of a site says what it found: the configuration it removed,
// and where the files stay or that it purged them. A site with nothing left
// of either — another removal came first, or nothing was ever written, as
// for the site a webmail job made and then gave up on — gets neither an
// empty list of what was removed nor a claim of files kept in, or purged
// from, a tree that does not exist; nor does such a site of an account the
// account's removal takes.
func TestSiteRemovalLogSaysWhatWasThere(t *testing.T) {
	f := newSiteFixture(t)
	removal := func(path string) string {
		t.Helper()
		var ref apitypes.JobRef
		f.call(http.MethodDelete, path, nil, http.StatusAccepted, &ref)
		job := f.waitJob(ref.JobID)
		if job.Status != store.JobDone {
			t.Fatalf("DELETE %s: %s %s", path, job.Status, job.Error)
		}
		return job.Log
	}
	has := func(log string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(log, w+"\n") {
				t.Errorf("no line %q in the log:\n%s", w, log)
			}
		}
	}
	hasNot := func(log string, unwanted ...string) {
		t.Helper()
		for _, u := range unwanted {
			if strings.Contains(log, u) {
				t.Errorf("%q in the log:\n%s", u, log)
			}
		}
	}

	// A site with its configuration and its files, as before.
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	log := removal("/sites/a.example.com")
	has(log, "a.example.com: files kept in /var/www/alex/data/www/a.example.com")
	if !strings.Contains(log, "a.example.com: removed /etc/nginx/monopanel/sites/a.example.com.conf") {
		t.Errorf("the removed configuration is not named:\n%s", log)
	}
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	has(removal("/sites/b.example.com?purge=true"), "b.example.com: purged /var/www/alex/data/www/b.example.com")

	// Sites of which nothing was ever written.
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	bare := func(domain string) {
		t.Helper()
		if err := f.db.CreateSite(f.ctx, &store.Site{UserID: alex.ID, Domain: domain, Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: "8.4", Docroot: "public_html", SSL: "none"}); err != nil {
			t.Fatal(err)
		}
	}
	bare("c.example.com")
	log = removal("/sites/c.example.com")
	has(log, "c.example.com: no configuration left to remove", "c.example.com: no files to keep, /var/www/alex/data/www/c.example.com does not exist")
	hasNot(log, "c.example.com: removed ", "files kept in")
	bare("d.example.com")
	log = removal("/sites/d.example.com?purge=true")
	has(log, "d.example.com: no configuration left to remove", "d.example.com: nothing to purge, /var/www/alex/data/www/d.example.com does not exist")
	hasNot(log, "d.example.com: removed ", "purged")
	bare("e.example.com")
	log = removal("/users/alex?purge=true")
	has(log, "e.example.com: no configuration left to remove", "e.example.com: no files to keep, /var/www/alex/data/www/e.example.com does not exist")
	hasNot(log, "e.example.com: removed ", "e.example.com: files kept in")
}

// The purge of an account removes the configuration of each of its sites
// first and then the home, with the sites' trees in it: the log says that a
// site's files go with the home, not that they are kept in a tree that is
// about to go, and the home's line says they went. Without the purge the
// site's files stay, and the log says where. (The fake agent reports every
// home removed.)
func TestAccountRemovalSaysWhereTheSitesFilesGo(t *testing.T) {
	for _, c := range []struct {
		query        string
		want, unwant []string
	}{
		{"?purge=true",
			[]string{"a.example.com: files go with the home /var/www/alex\n", "unix user alex removed with /var/www/alex "},
			[]string{"files kept in", "no files to keep"}},
		{"",
			[]string{"a.example.com: files kept in /var/www/alex/data/www/a.example.com\n"},
			[]string{"files go with the home"}},
	} {
		f := newSiteFixture(t)
		f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
		var ref apitypes.JobRef
		f.call(http.MethodDelete, "/users/alex"+c.query, nil, http.StatusAccepted, &ref)
		job := f.waitJob(ref.JobID)
		if job.Status != store.JobDone {
			t.Fatalf("DELETE /users/alex%s: %s %s", c.query, job.Status, job.Error)
		}
		for _, w := range c.want {
			if !strings.Contains(job.Log, w) {
				t.Errorf("DELETE /users/alex%s: no %q in the log:\n%s", c.query, w, job.Log)
			}
		}
		for _, u := range c.unwant {
			if strings.Contains(job.Log, u) {
				t.Errorf("DELETE /users/alex%s: %q in the log:\n%s", c.query, u, job.Log)
			}
		}
	}
}

// When the agent does not answer the Stat, the removal of a site without the
// purge keeps the line it always had: not knowing is not a gone tree.
func TestSiteRemovalKeepsTheOldLineWhenStatFails(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.agent.Fail["/v1/stat"] = "stat: input/output error"
	var ref apitypes.JobRef
	f.call(http.MethodDelete, "/sites/a.example.com", nil, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobDone || !strings.Contains(job.Log, "a.example.com: files kept in /var/www/alex/data/www/a.example.com\n") || strings.Contains(job.Log, "does not exist") {
		t.Fatalf("a Stat that failed: %s %s\n%s", job.Status, job.Error, job.Log)
	}
}
