package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// sent is what a request sent from inside a seam of the test answered.
type sent struct {
	status int
	body   string
	err    error
}

// sendBeside sends a request from inside a seam — the request in progress
// waits there — and hands its answer to ch.
func (f *siteFixture) sendBeside(ch chan sent, method, path string, body any) {
	st, b, err := f.send(method, path, body)
	ch <- sent{st, b, err}
}

// answerOf is the answer of the request a seam sent, or a failure when the
// seam never ran.
func answerOf(t *testing.T, ch chan sent) sent {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(20 * time.Second):
		t.Fatal("the request inside the seam never ran")
		return sent{}
	}
}

// holdAgent makes every agent call wait until release: a job queued by a
// request stops at its first agent call, and the test reads what the
// request itself wrote before the job changes anything.
func (f *siteFixture) holdAgent() (release func()) {
	ch := make(chan struct{})
	f.agent.SetBefore(func(string, []byte) { <-ch })
	var once sync.Once
	release = func() {
		once.Do(func() {
			f.agent.SetBefore(nil)
			close(ch)
		})
	}
	f.t.Cleanup(release)
	return release
}

// A CMS install records itself minutes after it read the site. An alias
// mp site set added to the site meanwhile — one that had none, as every
// site has none until one is added — stays, and the apply queued with it
// renders it.
func TestCMSInstallKeepsAnAliasAddedMeanwhile(t *testing.T) {
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})})
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	beside := make(chan sent, 1)
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/runas" && strings.Contains(string(body), `"core","version"`) {
			once.Do(func() {
				f.sendBeside(beside, http.MethodPatch, "/sites/wp.example.com", map[string]any{"aliases": []string{"www.wp.example.com"}})
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog"}); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	r := answerOf(t, beside)
	if r.err != nil || r.status != http.StatusAccepted {
		t.Fatalf("PATCH during the install: %d %v %s", r.status, r.err, r.body)
	}
	var out apitypes.SiteWithJob
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	// The apply the PATCH queued runs after the install (same lock key).
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("the PATCH's apply: %s %s", job.Status, job.Error)
	}
	got, _ := f.db.GetSite(f.ctx, site.ID)
	if got.CMS != "wordpress" {
		t.Fatalf("the install's record: cms %q", got.CMS)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "www.wp.example.com" {
		t.Errorf("the alias added during the install is lost: aliases %v", got.Aliases)
	}
	if conf, _ := f.agent.File("/etc/nginx/monopanel/sites/wp.example.com.conf"); !strings.Contains(conf, "www.wp.example.com") {
		t.Errorf("the apply after the PATCH renders no alias:\n%s", conf)
	}
}

// Two mp site set at once on a site without aliases: one adds an alias
// while the other, which changes client_max_body, is between its read and
// its write (PATCH's check of the IPv6 address it sets). Both stay.
func TestSiteChangeKeepsAnAliasAddedMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "one.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	beside := make(chan sent, 1)
	var once sync.Once
	f.s.SetHostIPv6s(func() []string {
		once.Do(func() {
			f.sendBeside(beside, http.MethodPatch, "/sites/one.example.com", map[string]any{"aliases": []string{"www.one.example.com"}})
		})
		return []string{"2001:db8::1"}
	})
	var out apitypes.SiteWithJob
	f.call(http.MethodPatch, "/sites/one.example.com", map[string]any{"ipv6": "2001:db8::1", "client_max_body": "128m"}, http.StatusAccepted, &out)
	r := answerOf(t, beside)
	if r.err != nil || r.status != http.StatusAccepted {
		t.Fatalf("the PATCH beside: %d %v %s", r.status, r.err, r.body)
	}
	var other apitypes.SiteWithJob
	_ = json.Unmarshal([]byte(r.body), &other)
	f.waitJob(other.JobID)
	f.waitJob(out.JobID)
	got, _ := f.db.GetSite(f.ctx, site.ID)
	if got.ClientMaxBody != "128m" || got.IPv6 != "2001:db8::1" || len(got.Aliases) != 1 {
		t.Errorf("after both changes: client_max_body %s, ipv6 %s, aliases %v", got.ClientMaxBody, got.IPv6, got.Aliases)
	}
	// The answer is the site as stored, the alias the other request added
	// included.
	if len(out.Site.Aliases) != 1 || out.Site.ClientMaxBody != "128m" {
		t.Errorf("the answer: client_max_body %s, aliases %v", out.Site.ClientMaxBody, out.Site.Aliases)
	}
}

// mp site set on a failed site takes it back to pending. An administrator
// suspends the site between that request's read and its write: the
// suspension stays — the way back from a failure goes in only while the
// site is still failed — the rest of the change goes in, the answer says
// what is stored, and the suspend's apply serves the 503 page.
func TestSiteChangeKeepsASuspensionMadeMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "sus.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	beside := make(chan sent, 1)
	var once sync.Once
	f.s.SetHostIPv6s(func() []string {
		once.Do(func() { f.sendBeside(beside, http.MethodPost, "/sites/sus.example.com/suspend", nil) })
		return []string{"2001:db8::1"}
	})
	var out apitypes.SiteWithJob
	f.call(http.MethodPatch, "/sites/sus.example.com", map[string]any{"ipv6": "2001:db8::1", "client_max_body": "128m"}, http.StatusAccepted, &out)
	r := answerOf(t, beside)
	if r.err != nil || r.status != http.StatusAccepted {
		t.Fatalf("the suspend beside: %d %v %s", r.status, r.err, r.body)
	}
	if out.Site.Status != store.SiteSuspended || out.Site.ClientMaxBody != "128m" {
		t.Errorf("the change's answer: status %s, client_max_body %s (want the stored suspended, 128m)", out.Site.Status, out.Site.ClientMaxBody)
	}
	var sus apitypes.SiteWithJob
	_ = json.Unmarshal([]byte(r.body), &sus)
	f.waitJob(sus.JobID)
	f.waitJob(out.JobID)
	got, _ := f.db.GetSite(f.ctx, site.ID)
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/sus.example.com.conf")
	if got.Status != store.SiteSuspended || got.ClientMaxBody != "128m" || got.IPv6 != "2001:db8::1" || !strings.Contains(conf, "return 503") {
		t.Errorf("after the change and the suspension: status %s, client_max_body %s, ipv6 %s, 503 page %v", got.Status, got.ClientMaxBody, got.IPv6, strings.Contains(conf, "return 503"))
	}
}

// A site whose removal is under way — its own, or its account's — takes no
// more changes: each is refused with a 409, nothing is written and no job
// is queued. A removal asked for between a change's read and its write is
// caught by the write.
func TestSiteChangesOfASiteBeingRemovedAreRefused(t *testing.T) {
	f := cmsFixture(t, nil)
	site := f.createSite(map[string]any{"domain": "gone.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	jobCount := func() int {
		t.Helper()
		list, err := f.db.ListJobs(f.ctx, 1000, "")
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	changes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/sites/gone.example.com", map[string]any{"client_max_body": "128m"}},
		{http.MethodPost, "/sites/gone.example.com/apply", nil},
		{http.MethodPost, "/sites/gone.example.com/suspend", nil},
		{http.MethodPost, "/sites/gone.example.com/unsuspend", nil},
		{http.MethodPost, "/sites/gone.example.com/fix", nil},
		{http.MethodPost, "/sites/gone.example.com/tls/issue", map[string]any{}},
		{http.MethodPost, "/sites/gone.example.com/cms", map[string]any{"cms": "wordpress"}},
	}
	refused := func(why, want string, status string) {
		t.Helper()
		before := jobCount()
		for _, c := range changes {
			code, raw := f.do(c.method, c.path, c.body)
			if code != http.StatusConflict || !strings.Contains(string(raw), want) {
				t.Errorf("%s: %s %s: %d %s (want 409 %q)", why, c.method, c.path, code, raw, want)
			}
		}
		if n := jobCount(); n != before {
			t.Errorf("%s: %d jobs queued", why, n-before)
		}
		if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != status || got.ClientMaxBody != "64m" || got.SSL != "none" || got.Preset != "" {
			t.Errorf("%s: the site changed: status %s, client_max_body %s, ssl %s, preset %q", why, got.Status, got.ClientMaxBody, got.SSL, got.Preset)
		}
	}
	if err := f.db.MarkSiteDeleting(f.ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	refused("the site being removed", "site gone.example.com is being removed", store.SiteDeleting)
	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	refused("its account being removed", "the account alex is being removed, and its site gone.example.com with it", store.SiteActive)
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserActive); err != nil {
		t.Fatal(err)
	}

	// The site's removal asked for while mp site set checks the IPv6
	// address it sets: the change is refused at its write.
	var once sync.Once
	f.s.SetHostIPv6s(func() []string {
		once.Do(func() {
			if err := f.db.MarkSiteDeleting(f.ctx, site.ID); err != nil {
				t.Error(err)
			}
		})
		return []string{"2001:db8::1"}
	})
	before := jobCount()
	if code, raw := f.do(http.MethodPatch, "/sites/gone.example.com", map[string]any{"ipv6": "2001:db8::1", "client_max_body": "128m"}); code != http.StatusConflict || !strings.Contains(string(raw), "site gone.example.com is being removed") {
		t.Errorf("a removal asked for during the change: %d %s", code, raw)
	}
	if n := jobCount(); n != before {
		t.Errorf("a removal asked for during the change: %d jobs queued", n-before)
	}
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != store.SiteDeleting || got.ClientMaxBody != "64m" || got.IPv6 != "" {
		t.Errorf("a removal asked for during the change: status %s, client_max_body %s, ipv6 %q", got.Status, got.ClientMaxBody, got.IPv6)
	}
}

// Each action writes its status: apply takes a failed site back to
// pending, suspend suspends, unsuspend takes it back to pending — read while
// the job each one queues waits at its first agent call.
func TestSiteActionsWriteTheirStatus(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "act.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	step := func(action, want string) {
		t.Helper()
		release := f.holdAgent()
		var out apitypes.SiteWithJob
		f.call(http.MethodPost, "/sites/act.example.com/"+action, nil, http.StatusAccepted, &out)
		if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != want || out.Site.Status != want {
			t.Errorf("%s: status %s, answered %s (want %s)", action, got.Status, out.Site.Status, want)
		}
		release()
		if job := f.waitJob(out.JobID); job.Status != store.JobDone {
			t.Fatalf("%s: %s %s", action, job.Status, job.Error)
		}
	}
	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	step("apply", store.SitePending)
	step("suspend", store.SiteSuspended)
	step("unsuspend", store.SitePending)
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != store.SiteActive {
		t.Fatalf("after the last apply: %s %s", got.Status, got.LastError)
	}
}

// DELETE /sites marks the site, and an address sites.move-ip gave it between
// the handler's read and its mark stays.
func TestSiteDeleteMarkLands(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1", "10.0.0.2"} })
	site := f.createSite(map[string]any{"domain": "del.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "ip": "10.0.0.1"})
	var once sync.Once
	f.s.restoresRead = func() {
		once.Do(func() {
			if moved, err := f.db.MoveSiteIP(f.ctx, site.ID, "10.0.0.1", "10.0.0.2"); err != nil || !moved {
				t.Errorf("move-ip: %v %v", moved, err)
			}
		})
	}
	release := f.holdAgent()
	var out apitypes.JobRef
	f.call(http.MethodDelete, "/sites/del.example.com", nil, http.StatusAccepted, &out)
	got, err := f.db.GetSite(f.ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.SiteDeleting || got.IP != "10.0.0.2" {
		t.Errorf("after the mark: status %s ip %s", got.Status, got.IP)
	}
	release()
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("site.delete: %s %s", job.Status, job.Error)
	}
}

// An imported certificate that covers an auto-SSL site goes on the site's
// record (reapplySitesForCert), and the site is applied with it.
func TestCertificateImportGoesOnTheSite(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "tls.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto"})
	if site.CertificateID != nil {
		t.Fatalf("a certificate before the import: %v", *site.CertificateID)
	}
	cert, key := testCert(t, "tls.example.com")
	var c store.Certificate
	// The apply waits at its first agent call: what is read below is
	// reapplySitesForCert's own write, not the apply's (SaveAppliedSite
	// records the certificate it serves too).
	release := f.holdAgent()
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": cert, "private_key": key}, http.StatusCreated, &c)
	got, _ := f.db.GetSite(f.ctx, site.ID)
	if got.CertificateID == nil || *got.CertificateID != c.ID {
		t.Fatalf("certificate_id after the import: %v (want %d)", got.CertificateID, c.ID)
	}
	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conf, _ := f.agent.File("/etc/nginx/monopanel/sites/tls.example.com.conf")
		if strings.Contains(conf, "443") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the site was not applied with HTTPS:\n%s", conf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holds numbers the job types holdKey registers.
var holds atomic.Int64

// holdKey runs a job under key until the returned release: jobs queued
// under the same key wait behind it.
func (f *siteFixture) holdKey(key string) (release func()) {
	f.t.Helper()
	gate := make(chan struct{})
	typ := fmt.Sprintf("test.hold.%s.%d", key, holds.Add(1))
	f.s.jobs.Register(typ, func(context.Context, *jobs.Context) error {
		<-gate
		return nil
	})
	j, err := f.s.jobs.Enqueue(f.ctx, typ, struct{}{}, jobs.WithLockKey(key))
	if err != nil {
		f.t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if got, _ := f.db.GetJob(f.ctx, j.ID); got != nil && got.Status == store.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the hold of %s did not start", key)
		}
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate); f.waitJob(j.ID) }) }
	f.t.Cleanup(release)
	return release
}

// wordpressFixture is cmsFixture with WordPress to download and alex's
// site wp.example.com.
func wordpressFixture(t *testing.T) (*siteFixture, *store.Site) {
	t.Helper()
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})})
	return f, f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
}

// cmsMadeSince lists what the agent calls from the n-th on made for a CMS
// install: a look into or a change of the docroot, a database, an unpack.
func (f *siteFixture) cmsMadeSince(n int) []string {
	var made []string
	for _, c := range f.agent.Calls()[n:] {
		b := string(c.Body)
		switch {
		case c.Path == "/v1/runas" && strings.Contains(b, "data/www/wp.example.com"),
			c.Path == "/v1/stream/in",
			c.Path == "/v1/tool" && strings.Contains(b, "CREATE DATABASE"):
			made = append(made, c.Path+" "+b)
		}
	}
	return made
}

// An install whose site, or whose site's account, is marked being removed
// while it waits for the site's job lock stops before it changes anything:
// no look into the docroot, no database, no unpack.
func TestCMSInstallStopsForWhatIsBeingRemoved(t *testing.T) {
	f, site := wordpressFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	stops := func(why, want string, mark func() error) {
		t.Helper()
		release := f.holdKey("site:wp.example.com")
		var res apitypes.CMSInstallResult
		f.call(http.MethodPost, "/sites/wp.example.com/cms", map[string]any{"cms": "wordpress"}, http.StatusAccepted, &res)
		if err := mark(); err != nil {
			t.Fatal(err)
		}
		n := len(f.agent.Calls())
		release()
		if job := f.waitJob(res.JobID); job.Status != store.JobFailed || job.Error != want {
			t.Errorf("%s: %s %q (want failed %q)", why, job.Status, job.Error, want)
		}
		if made := f.cmsMadeSince(n); len(made) > 0 {
			t.Errorf("%s: the install went on: %q", why, made)
		}
		if _, err := f.db.GetDatabaseByName(f.ctx, "alex_wordpress"); err == nil {
			t.Errorf("%s: a database was recorded", why)
		}
	}
	stops("the account being removed", "the account alex is being removed: nothing was installed into its site wp.example.com", func() error {
		return f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting)
	})
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserActive); err != nil {
		t.Fatal(err)
	}
	stops("the site being removed", "site wp.example.com is being removed: nothing was installed", func() error {
		return f.db.MarkSiteDeleting(f.ctx, site.ID)
	})
}

// A user.delete asked for while a CMS install works in one of the account's
// sites waits for it, at the site's lock, and then takes away all it made:
// no database the install makes is left after the account's were dropped,
// and nothing is unpacked into the home after it was purged.
func TestUserDeleteWaitsForACMSInstall(t *testing.T) {
	f, _ := wordpressFixture(t)
	// The install is held at its first look at the docroot, before the
	// database and the unpack.
	held, release := make(chan struct{}), make(chan struct{})
	var once, opened sync.Once
	open := func() { opened.Do(func() { close(release) }) }
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/runas" && strings.Contains(string(body), `"list"`) {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() {
		open()
		f.agent.SetBefore(nil)
	})
	var res apitypes.CMSInstallResult
	f.call(http.MethodPost, "/sites/wp.example.com/cms", map[string]any{"cms": "wordpress"}, http.StatusAccepted, &res)
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("the install did not reach the docroot")
	}
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	// The removal gets as far as it can while the install is held.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if j, _ := f.db.GetJob(f.ctx, del.JobID); j != nil && (j.Status == store.JobDone || j.Status == store.JobFailed) {
			break
		}
	}
	open()
	cms, dj := f.waitJob(res.JobID), f.waitJob(del.JobID)
	if cms.Status != store.JobDone || dj.Status != store.JobDone {
		t.Errorf("the install: %s %s; user.delete: %s %s", cms.Status, cms.Error, dj.Status, dj.Error)
	}
	calls := f.agent.Calls()
	purged := -1
	for i, c := range calls {
		if c.Path == "/v1/user/remove" {
			purged = i
		}
	}
	if purged < 0 {
		t.Fatal("the unix user was not removed")
	}
	if made := f.cmsMadeSince(purged + 1); len(made) > 0 {
		t.Errorf("the install went on after the account was removed: %q", made)
	}
	if _, err := f.db.GetDatabaseByName(f.ctx, "alex_wordpress"); err == nil {
		t.Error("the install's database outlived the account")
	}
}

// A webmail install into an existing site whose removal is asked for while
// the run unpacks Roundcube — after its look under the site's lock — stops
// at its write of the site's docroot: it says the site is being removed,
// and the mail settings do not name a site on its way out.
func TestWebmailStopsForARemovalAskedForDuringTheRun(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.settle()
	shop, err := f.db.GetSiteByDomain(f.ctx, "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	page := f.s.placeholderPages("shop.example.com")[0]
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{{Name: placeholderName, Type: "file", Size: int64(len(page))}}
		o.files["data/www/shop.example.com/"+placeholderName] = page
	})
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/tool" && strings.Contains(string(body), `"name":"tar"`) {
			once.Do(func() {
				if err := f.db.MarkSiteDeleting(f.ctx, shop.ID); err != nil {
					t.Error(err)
				}
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	job := f.webmailJob(map[string]any{"domain": "shop.example.com", "user": "alex"})
	if job.Status != store.JobFailed || job.Error != "site shop.example.com is being removed: the webmail is not installed into it" {
		t.Fatalf("a removal asked for during the run: %s %q", job.Status, job.Error)
	}
	if got, _ := f.db.GetSite(f.ctx, shop.ID); got.Status != store.SiteDeleting || got.Docroot == "public_html" {
		t.Errorf("the site: status %s, docroot %q", got.Status, got.Docroot)
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "" {
		t.Errorf("the mail settings name the webmail %q", c.Webmail)
	}
}
