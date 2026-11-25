package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/store"
)

// A wildcard certificate goes to the sites it covers — a name one label
// under it, aliases included — and not to a site a label deeper, nor to a
// site with a valid certificate for its very name.
func TestWildcardCertificateGoesOnTheSitesItCovers(t *testing.T) {
	f := newSiteFixture(t)
	site := func(domain string, aliases ...string) *store.Site {
		return f.createSite(map[string]any{"domain": domain, "aliases": aliases, "user": "alex", "php_version": "8.4", "ssl": "auto"})
	}
	apex := site("wild.example.com")
	kz := site("kz.wild.example.com", "k10.wild.example.com")
	own := site("own.wild.example.com")
	deep := site("a.b.wild.example.com")

	ocert, okey := testCert(t, "own.wild.example.com")
	var oc store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": ocert, "private_key": okey}, http.StatusCreated, &oc)

	cert, key := testCert(t, "*.wild.example.com", "wild.example.com")
	var c store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "wild.example.com", "certificate": cert, "private_key": key}, http.StatusCreated, &c)
	slices.Sort(c.UsedBySites)
	if !slices.Equal(c.UsedBySites, []string{"kz.wild.example.com", "wild.example.com"}) {
		t.Errorf("used by %v", c.UsedBySites)
	}
	for _, tc := range []struct {
		site *store.Site
		want int64
	}{{apex, c.ID}, {kz, c.ID}, {own, oc.ID}, {deep, 0}} {
		got, _ := f.db.GetSite(f.ctx, tc.site.ID)
		var id int64
		if got.CertificateID != nil {
			id = *got.CertificateID
		}
		if id != tc.want {
			t.Errorf("%s: certificate %d, want %d", tc.site.Domain, id, tc.want)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conf, _ := f.agent.File("/etc/nginx/monopanel/sites/kz.wild.example.com.conf")
		if strings.Contains(conf, "443") && strings.Contains(conf, "/certs/wild.example.com/") && strings.Contains(conf, "k10.wild.example.com") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kz.wild.example.com was not applied with the wildcard:\n%s", conf)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.s.certForSite(f.ctx, deep); got != nil {
		t.Errorf("a.b.wild.example.com is served %s", got.Name)
	}
}

// A certificate is imported under a name its wildcard covers (mp ssl import
// kz.example.com with *.example.com); a name it does not cover is refused.
func TestCertificateImportUnderAWildcardName(t *testing.T) {
	f := newSiteFixture(t)
	cert, key := testCert(t, "*.wild.example.com", "wild.example.com")
	var c store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "kz.wild.example.com", "certificate": cert, "private_key": key}, http.StatusCreated, &c)
	if c.Name != "kz.wild.example.com" {
		t.Errorf("name %s", c.Name)
	}
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "a.b.wild.example.com", "certificate": cert, "private_key": key}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "*.wild.example.com", "certificate": cert, "private_key": key}, http.StatusCreated, nil)
}

// A wildcard serves a site it was not made for only when it is the panel's,
// an administrator's or the site owner's own: one another account brought
// along has its key in that account's hands.
func TestWildcardServesOnlyItsOwnersSites(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "kz.wild.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	uid, gid := 1501, 1501
	bob := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/bob"}
	if err := f.db.CreateUser(f.ctx, bob); err != nil {
		t.Fatal(err)
	}
	admin, err := f.db.GetUserByLogin(f.ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(30 * 24 * time.Hour)
	c := &store.Certificate{Name: "wild.example.com", Names: []string{"*.wild.example.com", "wild.example.com"}, Kind: store.CertKindCustom, Status: store.CertValid,
		CertPath: "/x/fullchain.pem", KeyPath: "/x/privkey.pem", NotAfter: &until, UserID: &bob.ID}
	if err := f.db.UpsertCertificate(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := f.s.certForSite(f.ctx, site); got != nil {
		t.Errorf("bob's wildcard serves alex's site")
	}
	for _, owner := range []*int64{&site.UserID, &admin.ID, nil} {
		c.UserID = owner
		if err := f.db.UpsertCertificate(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		if got := f.s.certForSite(f.ctx, site); got == nil || got.ID != c.ID {
			t.Errorf("owner %v: served %v", owner, got)
		}
	}
	// A record for the site's very name wins over the wildcard.
	exact := &store.Certificate{Name: "kz.wild.example.com", Names: []string{"kz.wild.example.com"}, Kind: store.CertKindCustom, Status: store.CertValid,
		CertPath: "/y/fullchain.pem", KeyPath: "/y/privkey.pem", NotAfter: &until}
	if err := f.db.UpsertCertificate(f.ctx, exact); err != nil {
		t.Fatal(err)
	}
	if got := f.s.certForSite(f.ctx, site); got == nil || got.ID != exact.ID {
		t.Errorf("served %v, want the site's own", got)
	}
}

// A source that serves one wildcard file on four sites: the certificate is
// let in once, under the name it carries, and the other sites are told it
// serves them.
func TestMigrationTakesASharedWildcardOnce(t *testing.T) {
	f := newSiteFixture(t)
	cert, key := testCert(t, "*.qaj.example.org", "qaj.example.org")
	names := []string{"kz.qaj.example.org", "qaj.example.org", "ru.qaj.example.org", "en.qaj.example.org"}
	b := &apitypes.MigrationBundle{Secrets: &apitypes.MigrationSecrets{}}
	siteNames := map[string]bool{}
	for _, n := range names {
		b.Certificates = append(b.Certificates, apitypes.MigrationCert{Name: n, Cert: cert, Key: key})
		siteNames[n] = true
	}
	v := &migrateVet{refused: map[any]string{}, skipped: map[any]string{}, auth: map[string]dbAuth{}}
	f.s.vetCerts(f.ctx, v, b, siteNames)
	for i := range b.Certificates {
		mc := &b.Certificates[i]
		why := v.why(mc)
		if mc.Name == "qaj.example.org" {
			if why != "" {
				t.Errorf("qaj.example.org stays behind: %s", why)
			}
			continue
		}
		if !strings.Contains(why, "same certificate arrives as qaj.example.org") {
			t.Errorf("%s: %q", mc.Name, why)
		}
	}
	// Each its own (different files): all four arrive.
	b.Certificates = nil
	for _, n := range names {
		c, k := testCert(t, "*.qaj.example.org", "qaj.example.org")
		b.Certificates = append(b.Certificates, apitypes.MigrationCert{Name: n, Cert: c, Key: k})
	}
	v = &migrateVet{refused: map[any]string{}, skipped: map[any]string{}, auth: map[string]dbAuth{}}
	f.s.vetCerts(f.ctx, v, b, siteNames)
	for i := range b.Certificates {
		if why := v.why(&b.Certificates[i]); why != "" {
			t.Errorf("%s: %s", b.Certificates[i].Name, why)
		}
	}
}

// The removal of an account takes the certificates named after its sites
// only once nothing serves them: a wildcard named after alex's site still
// serves bob's site under it, and stays; one only alex's site served goes.
func TestAccountRemovalKeepsACertificateOthersServe(t *testing.T) {
	f := newSiteFixture(t)
	uid, gid := 1501, 1501
	if err := f.db.CreateUser(f.ctx, &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/bob", Shell: true}); err != nil {
		t.Fatal(err)
	}
	f.createSite(map[string]any{"domain": "wild.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto"})
	f.createSite(map[string]any{"domain": "solo.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto"})
	kz := f.createSite(map[string]any{"domain": "kz.wild.example.com", "user": "bob", "php_version": "8.4", "ssl": "auto"})
	cert, key := testCert(t, "*.wild.example.com", "wild.example.com")
	var c store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "wild.example.com", "certificate": cert, "private_key": key}, http.StatusCreated, &c)
	scert, skey := testCert(t, "solo.example.com")
	var sc store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": scert, "private_key": skey}, http.StatusCreated, &sc)
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("alex's removal: %s %s", job.Status, job.Error)
	}
	if got, err := f.db.GetCertificate(f.ctx, c.ID); err != nil {
		t.Fatalf("the wildcard bob's site serves went: %v", err)
	} else if _, err := os.Stat(got.CertPath); err != nil {
		t.Errorf("its files went: %v", err)
	}
	if got, _ := f.db.GetSite(f.ctx, kz.ID); got.CertificateID == nil || *got.CertificateID != c.ID {
		t.Errorf("bob's site lost its certificate: %v", got.CertificateID)
	}
	if _, err := f.db.GetCertificate(f.ctx, sc.ID); err == nil {
		t.Error("the certificate only alex's site served stayed")
	}
}

// The panel's HTTPS port, phpMyAdmin and the mail server take a wildcard that
// covers their name when none of that name is there — an administrator's,
// never one an account brought along.
func TestPanelAndMailTakeACoveringWildcard(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "panel.wild.example.com" })
	if err := f.s.tls.Reload(); err != nil {
		t.Fatal(err)
	}
	if f.s.tls.Info().Source != "selfsigned" {
		t.Fatalf("source %s before the import", f.s.tls.Info().Source)
	}
	// An account's wildcard: neither the panel nor the mail server takes it.
	uid, gid := 1501, 1501
	bob := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/bob"}
	if err := f.db.CreateUser(f.ctx, bob); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bcert, bkey := testCert(t, "*.wild.example.com")
	bc, bk := filepath.Join(dir, "b.pem"), filepath.Join(dir, "bk.pem")
	_ = os.WriteFile(bc, []byte(bcert), 0o600)
	_ = os.WriteFile(bk, []byte(bkey), 0o600)
	until := time.Now().Add(30 * 24 * time.Hour)
	if err := f.db.UpsertCertificate(f.ctx, &store.Certificate{Name: "bob.wild.example.com", Names: []string{"*.wild.example.com"}, Kind: store.CertKindCustom, Status: store.CertValid,
		CertPath: bc, KeyPath: bk, NotAfter: &until, UserID: &bob.ID}); err != nil {
		t.Fatal(err)
	}
	_ = f.s.tls.Reload()
	if f.s.tls.Info().Source != "selfsigned" {
		t.Error("the panel serves an account's wildcard")
	}
	if cp, _, _, _ := f.s.mailTLS(f.ctx, mailConfig{Hostname: "mail.wild.example.com"}); cp == bc {
		t.Error("the mail server serves an account's wildcard")
	}

	cert, key := testCert(t, "*.wild.example.com", "wild.example.com")
	var c store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "wild.example.com", "certificate": cert, "private_key": key}, http.StatusCreated, &c)
	if cp, _ := f.s.tls.Files(); cp != c.CertPath || f.s.tls.Info().Source != "acme" {
		t.Errorf("the panel serves %s (%s), want %s", cp, f.s.tls.Info().Source, c.CertPath)
	}
	if cp, _, fp := f.s.pmaCert(); cp != c.CertPath || fp == "" {
		t.Errorf("phpMyAdmin is given %s", cp)
	}
	if cp, _, kind, _ := f.s.mailTLS(f.ctx, mailConfig{Hostname: "mail.wild.example.com"}); cp != c.CertPath || kind != store.CertKindCustom {
		t.Errorf("the mail server is given %s (%s)", cp, kind)
	}
	var list []store.Certificate
	f.call(http.MethodGet, "/certificates", nil, http.StatusOK, &list)
	for _, l := range list {
		if l.UsedByPanel != (l.ID == c.ID) {
			t.Errorf("%s: used by the panel %v", l.Name, l.UsedByPanel)
		}
	}
	// The panel cannot go back to self-signed while the wildcard is there:
	// it is not the panel's own record.
	f.call(http.MethodDelete, "/ssl/panel", nil, http.StatusConflict, nil)
}

// A certificate given to a site by hand stays: one issued or imported later
// for the site's name does not take it over, and auto lets the panel choose
// again. Only an administrator gives one.
func TestSiteKeepsTheCertificateGivenByHand(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "kz.wild.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	cert, key := testCert(t, "*.wild.example.com", "wild.example.com")
	var w store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"name": "wild.example.com", "certificate": cert, "private_key": key, "sites": []string{"kz.wild.example.com"}}, http.StatusCreated, &w)
	got, _ := f.db.GetSite(f.ctx, site.ID)
	if got.CertificateID == nil || *got.CertificateID != w.ID || !got.CertPinned || got.SSL != "auto" {
		t.Fatalf("after the import with --site: cert %v pinned %v ssl %s", got.CertificateID, got.CertPinned, got.SSL)
	}
	// Its own certificate arrives: the site keeps the wildcard.
	ocert, okey := testCert(t, "kz.wild.example.com")
	var o store.Certificate
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": ocert, "private_key": okey}, http.StatusCreated, &o)
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.CertificateID == nil || *got.CertificateID != w.ID {
		t.Fatalf("the site's own certificate took the wildcard's place: %v", got.CertificateID)
	}
	// auto: the panel chooses — the site's own wins.
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/kz.wild.example.com", map[string]any{"certificate": "auto"}, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.CertPinned || got.CertificateID == nil || *got.CertificateID != o.ID {
		t.Errorf("after auto: cert %v pinned %v (own %d, wildcard %d)", *got.CertificateID, got.CertPinned, o.ID, w.ID)
	}
	// By name, back to the wildcard; a certificate that does not cover the
	// site, or none, is refused.
	f.call(http.MethodPatch, "/sites/kz.wild.example.com", map[string]any{"certificate": "wild.example.com"}, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if got, _ := f.db.GetSite(f.ctx, site.ID); !got.CertPinned || got.CertificateID == nil || *got.CertificateID != w.ID {
		t.Errorf("by name: cert %v pinned %v", got.CertificateID, got.CertPinned)
	}
	other := f.createSite(map[string]any{"domain": "other.example.org", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.call(http.MethodPatch, "/sites/"+other.Domain, map[string]any{"certificate": "wild.example.com"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPatch, "/sites/"+other.Domain, map[string]any{"certificate": "999"}, http.StatusNotFound, nil)
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": cert, "private_key": key, "sites": []string{other.Domain}}, http.StatusUnprocessableEntity, nil)
	// An ordinary account does not give its site a certificate by hand.
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := auth.HashPassword("alex-password-1")
	if err := f.db.SetUserPassword(f.ctx, alex.ID, h); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password-1")
	f.call(http.MethodPatch, "/sites/kz.wild.example.com", map[string]any{"certificate": "auto"}, http.StatusForbidden, nil)
}

// A Let's Encrypt wildcard over an account's own site arrives with its
// wildcard; one over a name the account does not have loses it. Without a
// DNS provider its renewal fails with the command that fixes it.
func TestMigratedLetsEncryptWildcardKeepsItsName(t *testing.T) {
	own := map[string]bool{"qaj.example.org": true, "kz.qaj.example.org": true}
	got := acmeNames([]string{"*.qaj.example.org", "qaj.example.org", "*.other.example.org", "*.kz.qaj.example.org", "x.qaj.example.org"}, func(n string) bool { return own[n] })
	if !slices.Equal(got, []string{"*.qaj.example.org", "qaj.example.org", "*.kz.qaj.example.org"}) {
		t.Errorf("acmeNames: %v", got)
	}
	if h := reorderHint("qaj.example.org", []string{"*.qaj.example.org", "qaj.example.org"}); h != "mp ssl issue qaj.example.org '*.qaj.example.org' --dns <provider>" {
		t.Errorf("hint: %s", h)
	}
	f := newSiteFixture(t)
	c := &store.Certificate{Name: "qaj.example.org", Names: []string{"*.qaj.example.org", "qaj.example.org"}, Kind: store.CertKindACME, AutoRenew: true, Status: store.CertPending, DirectoryURL: "https://acme.invalid/dir"}
	if err := f.db.UpsertCertificate(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.jobs.Enqueue(f.ctx, "cert.issue", certIssuePayload{CertID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.waitJob(job.ID)
	got2, _ := f.db.GetCertificate(f.ctx, c.ID)
	if !strings.Contains(got2.LastError, "DNS-01") || !strings.Contains(got2.LastError, "--dns <provider>") {
		t.Errorf("renewal error: %q", got2.LastError)
	}
}
