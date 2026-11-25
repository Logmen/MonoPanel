package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/apitypes"
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
