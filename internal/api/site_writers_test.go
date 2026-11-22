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

// The account's removal asked for while mp site set is between its read of
// the site and its write — PATCH's check of the IPv6 address it sets — is
// caught by the write as the site's own removal is: the account's removal
// marks the account, not its sites. 409, nothing written, nothing queued
// but the removal.
func TestSiteChangeRefusedForAnAccountRemovalAskedForMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "own.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.holdKey("user:alex") // the user.delete stays queued: the site stays
	jobCount := func() int {
		t.Helper()
		list, err := f.db.ListJobs(f.ctx, 1000, "")
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	beside := make(chan sent, 1)
	var once sync.Once
	f.s.SetHostIPv6s(func() []string {
		once.Do(func() { f.sendBeside(beside, http.MethodDelete, "/users/alex?purge=true", nil) })
		return []string{"2001:db8::1"}
	})
	before := jobCount()
	code, raw := f.do(http.MethodPatch, "/sites/own.example.com", map[string]any{"ipv6": "2001:db8::1", "client_max_body": "128m"})
	if r := answerOf(t, beside); r.err != nil || r.status != http.StatusAccepted {
		t.Fatalf("DELETE /users/alex beside: %d %v %s", r.status, r.err, r.body)
	}
	if code != http.StatusConflict || !strings.Contains(string(raw), "the account alex is being removed, and its site own.example.com with it") {
		t.Errorf("mp site set while the removal of its account is asked for: %d %s", code, raw)
	}
	if n := jobCount() - before; n != 1 {
		t.Errorf("%d jobs queued (want only the user.delete)", n)
	}
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.ClientMaxBody != "64m" || got.IPv6 != "" {
		t.Errorf("the change went onto the site of an account being removed: client_max_body %s, ipv6 %q", got.ClientMaxBody, got.IPv6)
	}
}

// A removal of a site or of its account asked for between a request's read
// of the site and its write is caught by the write, for every request that
// changes the site — mp site set, apply, suspend, unsuspend, ordering its
// certificate, a CMS install's preset: a 409 that names the removal, the
// row as it was, nothing queued. A site gone meanwhile is a 404.
func TestSiteChangeRefusedAtItsWrite(t *testing.T) {
	f := cmsFixture(t, nil)
	site := f.createSite(map[string]any{"domain": "late.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
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
		{http.MethodPatch, "/sites/late.example.com", map[string]any{"client_max_body": "128m"}},
		{http.MethodPost, "/sites/late.example.com/apply", nil},
		{http.MethodPost, "/sites/late.example.com/suspend", nil},
		{http.MethodPost, "/sites/late.example.com/unsuspend", nil},
		{http.MethodPost, "/sites/late.example.com/tls/issue", map[string]any{}},
		{http.MethodPost, "/sites/late.example.com/cms", map[string]any{"cms": "wordpress"}},
	}
	for _, removal := range []struct {
		name, want, status string
		mark, unmark       func() error
	}{
		{"the site's removal", "site late.example.com is being removed", store.SiteDeleting,
			func() error { return f.db.MarkSiteDeleting(f.ctx, site.ID) },
			func() error { return f.db.SetSiteStatus(f.ctx, site.ID, store.SiteActive, "") }},
		{"its account's removal", "the account alex is being removed, and its site late.example.com with it", store.SiteActive,
			func() error { return f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting) },
			func() error { return f.db.SetUserStatus(f.ctx, alex.ID, store.UserActive) }},
	} {
		for _, c := range changes {
			var marked atomic.Bool
			f.s.beforeChangeWrite = func() {
				if marked.CompareAndSwap(false, true) {
					if err := removal.mark(); err != nil {
						t.Error(err)
					}
				}
			}
			before := jobCount()
			code, raw := f.do(c.method, c.path, c.body)
			f.s.beforeChangeWrite = nil
			if !marked.Load() {
				t.Errorf("%s: %s %s did not reach its write: %d %s", removal.name, c.method, c.path, code, raw)
			}
			if code != http.StatusConflict || !strings.Contains(string(raw), removal.want) {
				t.Errorf("%s asked for during %s %s: %d %s (want 409 %q)", removal.name, c.method, c.path, code, raw, removal.want)
			}
			if n := jobCount(); n != before {
				t.Errorf("%s asked for during %s %s: %d jobs queued", removal.name, c.method, c.path, n-before)
			}
			if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != removal.status || got.ClientMaxBody != "64m" || got.SSL != "none" || got.Preset != "" {
				t.Errorf("%s asked for during %s %s: the site changed: status %s, client_max_body %s, ssl %s, preset %q", removal.name, c.method, c.path, got.Status, got.ClientMaxBody, got.SSL, got.Preset)
			}
			if err := removal.unmark(); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Gone meanwhile: the account's removal takes its sites' rows without
	// marking them.
	f.s.beforeChangeWrite = func() {
		if err := f.db.DeleteSite(f.ctx, site.ID); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { f.s.beforeChangeWrite = nil })
	if code, raw := f.do(http.MethodPatch, "/sites/late.example.com", map[string]any{"client_max_body": "128m"}); code != http.StatusNotFound || !strings.Contains(string(raw), "site not found") {
		t.Errorf("a site gone during mp site set: %d %s (want 404)", code, raw)
	}
}

// Custom nginx directives are a change of the site like any other: refused
// (409) for a site being removed — here while site.delete --purge empties
// its tree — and for one whose removal is asked for between the request's
// read and its write, which the write under the site's lock sees. Written
// after the removal took <domain>.d away, the file would outlive the site
// and be included by the next site of that domain.
func TestNginxCustomOfASiteBeingRemovedIsRefused(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "n.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	custom := "/etc/nginx/monopanel/sites/n.example.com.d/custom.conf"
	purge := newCallGate()
	t.Cleanup(purge.open)
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/paths/remove" && strings.Contains(string(body), `"/var/www/alex/data/www/n.example.com"`) {
			purge.hit()
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/sites/n.example.com?purge=true", nil, http.StatusAccepted, &del)
	purge.wait(t, "the purge of the site's tree")
	// Refused at once: the removal holds the site's lock through its purge,
	// and a request that waited for it would answer only once the site is
	// gone.
	bodies := []map[string]any{{"custom": "add_header X-Old-Site yes;\n"}, {"custom": ""}}
	answers := make([]chan sent, len(bodies))
	for i, body := range bodies {
		answers[i] = make(chan sent, 1)
		go f.sendBeside(answers[i], http.MethodPut, "/sites/n.example.com/nginx", body)
	}
	got := make([]sent, len(bodies))
	for i, body := range bodies {
		select {
		case got[i] = <-answers[i]:
		case <-time.After(5 * time.Second):
			t.Errorf("PUT /sites/n.example.com/nginx %v while the site is being removed: no answer while the removal runs", body)
		}
	}
	purge.open()
	for i, body := range bodies {
		if got[i].status == 0 {
			got[i] = answerOf(t, answers[i])
		}
		if got[i].err != nil || got[i].status != http.StatusConflict || !strings.Contains(got[i].body, "site n.example.com is being removed") {
			t.Errorf("PUT /sites/n.example.com/nginx %v while the site is being removed: %d %v %s (want 409)", body, got[i].status, got[i].err, got[i].body)
		}
	}
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("site.delete: %s %s", j.Status, j.Error)
	}
	if _, ok := f.agent.File(custom); ok {
		t.Errorf("after the removal: %s is left behind", custom)
	}
	f.agent.SetBefore(nil)
	f.createSite(map[string]any{"domain": "n.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if c, ok := f.agent.File(custom); ok && strings.Contains(c, "X-Old-Site") {
		t.Errorf("the new site n.example.com has the removed site's custom directives: %q", strings.TrimSpace(c))
	}

	// The removal asked for after the read: DELETE /sites inside the
	// request, its site.delete queued behind the site's job key.
	release := f.holdKey("site:n.example.com")
	deleted := make(chan sent, 1)
	var once sync.Once
	f.s.beforeChangeWrite = func() {
		once.Do(func() { f.sendBeside(deleted, http.MethodDelete, "/sites/n.example.com?purge=true", nil) })
	}
	t.Cleanup(func() { f.s.beforeChangeWrite = nil })
	code, raw := f.do(http.MethodPut, "/sites/n.example.com/nginx", map[string]any{"custom": "add_header X-Late yes;\n"})
	d := answerOf(t, deleted)
	if d.err != nil || d.status != http.StatusAccepted {
		t.Fatalf("DELETE /sites/n.example.com inside the request: %d %v %s", d.status, d.err, d.body)
	}
	if code != http.StatusConflict || !strings.Contains(string(raw), "site n.example.com is being removed") {
		t.Errorf("PUT /sites/n.example.com/nginx whose site's removal was asked for after its read: %d %s (want 409)", code, raw)
	}
	release()
	var del2 apitypes.JobRef
	_ = json.Unmarshal([]byte(d.body), &del2)
	if j := f.waitJob(del2.JobID); j.Status != store.JobDone {
		t.Fatalf("site.delete: %s %s", j.Status, j.Error)
	}
	if c, ok := f.agent.File(custom); ok && strings.Contains(c, "X-Late") {
		t.Errorf("the directives sent during the removal outlived the site: %q", strings.TrimSpace(c))
	}
}

// The site's removal asked for during a CMS install — after its look under
// the site's lock, at wp-cli's "core version" — waits for the install under
// the site's job key; the install's record does not go onto the site being
// removed, and the job says so.
func TestCMSRecordOfASiteRemovedDuringTheInstall(t *testing.T) {
	f, site := wordpressFixture(t)
	deleted := make(chan sent, 1)
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/runas" && strings.Contains(string(body), `"core","version"`) {
			once.Do(func() { f.sendBeside(deleted, http.MethodDelete, "/sites/wp.example.com", nil) })
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	_, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress"})
	if job.Status != store.JobFailed || job.Error != "site wp.example.com is being removed: WordPress is not recorded on it" {
		t.Errorf("the install: %s %q", job.Status, job.Error)
	}
	d := answerOf(t, deleted)
	var del apitypes.JobRef
	if err := json.Unmarshal([]byte(d.body), &del); d.err != nil || d.status != http.StatusAccepted || err != nil {
		t.Fatalf("DELETE /sites/wp.example.com during the install: %d %v %s", d.status, d.err, d.body)
	}
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("site.delete: %s %s", j.Status, j.Error)
	}
	if _, err := f.db.GetSite(f.ctx, site.ID); err == nil {
		t.Error("the site outlived its removal")
	}
}

// A CMS install queued while the site's job key is busy, whose site went
// with its account before the install's turn came, stops before anything:
// it says the site was removed meanwhile and makes nothing — not even the
// preset's apply.
func TestCMSInstallOfASiteRemovedWhileQueued(t *testing.T) {
	f, site := wordpressFixture(t)
	release := f.holdKey("site:wp.example.com")
	var res apitypes.CMSInstallResult
	f.call(http.MethodPost, "/sites/wp.example.com/cms", map[string]any{"cms": "wordpress"}, http.StatusAccepted, &res)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
	if _, err := f.db.GetSite(f.ctx, site.ID); err == nil {
		t.Fatal("the site outlived its account")
	}
	n := len(f.agent.Calls())
	release()
	if j := f.waitJob(res.JobID); j.Status != store.JobFailed || j.Error != "the site was removed meanwhile: nothing was installed" {
		t.Errorf("the install: %s %q", j.Status, j.Error)
	}
	for _, c := range f.agent.Calls()[n:] {
		t.Errorf("the install of a site gone called the agent: %s %s", c.Path, c.Body)
	}
}

// The same with a site of another account made meanwhile under the gone
// site's id, which SQLite gives the next row: the install does not take it
// for its own — no preset applied to it, nothing installed into it.
func TestCMSInstallOfASiteWhoseIDWasGivenAgain(t *testing.T) {
	f, site := wordpressFixture(t)
	release := f.holdKey("site:wp.example.com")
	var res apitypes.CMSInstallResult
	f.call(http.MethodPost, "/sites/wp.example.com/cms", map[string]any{"cms": "wordpress"}, http.StatusAccepted, &res)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
	f.userWithPassword("bob", "bob-password", 1501)
	fresh := f.createSite(map[string]any{"domain": "fresh.example.com", "user": "bob", "php_version": "8.4", "ssl": "none"})
	if fresh.ID != site.ID {
		t.Fatalf("fresh.example.com has id %d, wp.example.com had %d: the id was not given again", fresh.ID, site.ID)
	}
	n := len(f.agent.Calls())
	release()
	if j := f.waitJob(res.JobID); j.Status != store.JobFailed || j.Error != "the site was removed meanwhile: nothing was installed" {
		t.Errorf("the install: %s %q", j.Status, j.Error)
	}
	for _, c := range f.agent.Calls()[n:] {
		t.Errorf("the install of wp.example.com called the agent for bob's fresh.example.com: %s %s", c.Path, c.Body)
	}
	if got, _ := f.db.GetSite(f.ctx, fresh.ID); got.CMS != "" || got.Preset != "" {
		t.Errorf("bob's fresh.example.com got the install: cms %q, preset %q", got.CMS, got.Preset)
	}
}

// A site.delete with purge waiting under its site's job key behind a long
// job of the site, whose site went with its account meanwhile, does not
// take the site another account made since under the gone site's id: it
// ends done, the site gone already.
func TestQueuedSiteDeleteKeepsANewSiteWithTheSameID(t *testing.T) {
	f := newSiteFixture(t)
	old := f.createSite(map[string]any{"domain": "old.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	release := f.holdKey("site:old.example.com")
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/sites/old.example.com?purge=true", nil, http.StatusAccepted, &del)
	var udel apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &udel)
	if j := f.waitJob(udel.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %q", j.Status, j.Error)
	}
	f.userWithPassword("bob", "bob-password", 1501)
	fresh := f.createSite(map[string]any{"domain": "new.example.com", "user": "bob", "php_version": "8.4", "ssl": "none"})
	if fresh.ID != old.ID {
		t.Fatalf("new.example.com has id %d, old.example.com had %d: the id was not given again", fresh.ID, old.ID)
	}
	n := len(f.agent.Calls())
	release()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone || j.Message != "the site is gone already: its files are as its removal left them" {
		t.Errorf("the queued site.delete of old.example.com: %s %q %q", j.Status, j.Error, j.Message)
	}
	for _, c := range f.agent.Calls()[n:] {
		if strings.Contains(string(c.Body), "new.example.com") {
			t.Errorf("the site.delete asked for old.example.com touched new.example.com: %s %s", c.Path, c.Body)
		}
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "new.example.com"); err != nil {
		t.Errorf("bob's new.example.com after the site.delete of old.example.com: %v", err)
	}
}

// A webmail install and a CMS install asked for the same site at about the
// same time: the webmail looks whether the site holds files of its own
// before its download, and the CMS install, under the site's job key and
// not the webmail's, may run in that window and put WordPress there. The
// webmail looks again under the site's lock, which the CMS install holds
// from its docroot to its record, and does not unpack Roundcube over it.
func TestWebmailDoesNotUnpackOverACMSInstalledMeanwhile(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	wordpressSources(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.settle()
	page := f.s.placeholderPages("shop.example.com")[0]
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{{Name: placeholderName, Type: "file", Size: int64(len(page))}}
		o.files["data/www/shop.example.com/"+placeholderName] = page
	})
	// The webmail run is held at its look into its database: past its look
	// at the site, before the site's lock.
	held, release := make(chan struct{}), make(chan struct{})
	var once, opened sync.Once
	open := func() { opened.Do(func() { close(release) }) }
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/tool" && strings.Contains(string(body), "information_schema.tables WHERE table_schema") {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() { open(); f.agent.SetBefore(nil) })
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/webmail", map[string]any{"domain": "shop.example.com", "user": "alex"}, http.StatusAccepted, &ref)
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("the webmail run did not reach its database look")
	}
	_, cj := f.installCMS(t, "shop.example.com", map[string]any{"cms": "wordpress"})
	if cj.Status != store.JobDone {
		t.Fatalf("the CMS install: %s %q\n%s", cj.Status, cj.Error, cj.Log)
	}
	// What the install left in the site's root, as the owner lists it.
	home.with(func(o *ownerSide) {
		o.dirs["data/www/shop.example.com"] = []apitypes.FileEntry{
			{Name: "index.php", Type: "file", Size: 405}, {Name: "wp-admin", Type: "dir"}, {Name: "wp-config.php", Type: "file", Size: 3200},
		}
	})
	n := len(f.agent.Calls())
	open()
	wj := f.waitJob(ref.JobID)
	unpacked := false
	for _, c := range f.agent.Calls()[n:] {
		if c.Path == "/v1/tool" && strings.Contains(string(c.Body), `"name":"tar"`) && strings.Contains(string(c.Body), "/var/www/alex/data/www/shop.example.com") {
			unpacked = true
		}
	}
	got, err := f.db.GetSiteByDomain(f.ctx, "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if unpacked || wj.Status != store.JobFailed || !strings.Contains(wj.Error, "site shop.example.com already holds files of its own") || got.Docroot == "public_html" || got.CMS != "wordpress" {
		t.Errorf("the webmail over the WordPress just installed: unpacked %v, job %s %q, the site: cms %q, docroot %q", unpacked, wj.Status, wj.Error, got.CMS, got.Docroot)
	}
}
