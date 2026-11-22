package store

import (
	"context"
	"errors"
	"testing"
)

func TestPHPAndSites(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	if err := db.UpsertPHPVersion(ctx, &PHPVersion{Version: "8.4", Source: "sury", Status: PHPInstalled, Extensions: []string{"mysql"}}); err != nil {
		t.Fatal(err)
	}
	db.UpsertPHPVersion(ctx, &PHPVersion{Version: "7.4", Source: "sury", Status: PHPInstalling})
	db.UpsertPHPVersion(ctx, &PHPVersion{Version: "8.4", Source: "sury", Status: PHPInstalled, PackageVersion: "8.4.12", Extensions: []string{"mysql", "gd"}})
	list, _ := db.ListPHPVersions(ctx)
	if len(list) != 2 || list[0].Version != "7.4" || list[1].PackageVersion != "8.4.12" || len(list[1].Extensions) != 2 {
		t.Fatalf("php list: %+v %+v", list[0], list[1])
	}
	s := &Site{UserID: u.ID, Domain: "example.com", Aliases: []string{"www.example.com"}, PHPVersion: "8.4", HTTP2: true, PHPIni: map[string]string{"memory_limit": "256M"}}
	if err := db.CreateSite(ctx, s); err != nil || s.ID == 0 {
		t.Fatalf("create site: %v", err)
	}
	if err := db.CreateSite(ctx, &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetSiteByDomain(ctx, "example.com")
	if err != nil || got.Login != "alex" || got.Mode != ModeFPM || got.SSL != "auto" || got.FPMMaxChildren != 8 || got.PHPIni["memory_limit"] != "256M" || !got.HTTP2 {
		t.Fatalf("get site: %+v %v", got, err)
	}
	got.Mode, got.HTTP3, got.Status = ModeApache, true, SiteActive
	if err := db.UpdateSite(ctx, got); err != nil {
		t.Fatal(err)
	}
	found, _ := db.FindSitesByName(ctx, "www.example.com")
	if len(found) != 1 || !found[0].HTTP3 || found[0].Mode != ModeApache {
		t.Fatalf("find by alias: %+v", found)
	}
	if n, _ := db.CountSitesByPHP(ctx, "8.4"); n != 1 {
		t.Fatalf("count by php: %d", n)
	}
	mine, _ := db.ListSites(ctx, u.ID)
	if len(mine) != 1 {
		t.Fatalf("list for user: %d", len(mine))
	}
	if err := db.DeleteSite(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSite(ctx, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("site must be gone")
	}
	db.DeletePHPVersion(ctx, "7.4")
	if _, err := db.GetPHPVersion(ctx, "7.4"); !errors.Is(err, ErrNotFound) {
		t.Fatal("php version must be gone")
	}
}

// What a site.apply saves is what it decided — an address for a site without
// one, the certificate it serves, the status — and nothing it merely read: a
// change made while it ran (the mode, the PHP branch, an address) stays.
func TestSaveAppliedSite(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4", Mode: ModeApache}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	read, _ := db.GetSite(ctx, s.ID)
	changed, _ := db.GetSite(ctx, s.ID)
	changed.Mode, changed.PHPVersion = ModeFPM, "8.3"
	if err := db.UpdateSite(ctx, changed); err != nil {
		t.Fatal(err)
	}
	c := &Certificate{Name: "example.com", Names: []string{"example.com"}}
	if err := db.UpsertCertificate(ctx, c); err != nil {
		t.Fatal(err)
	}
	cert := c.ID
	from := read.Status
	read.IP, read.CertificateID, read.Status, read.LastError = "192.0.2.10", &cert, SiteActive, ""
	if err := db.SaveAppliedSite(ctx, read, from, false); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSite(ctx, s.ID)
	if got.Mode != ModeFPM || got.PHPVersion != "8.3" || got.Status != SiteActive || got.IP != "192.0.2.10" || got.CertificateID != nil {
		t.Fatalf("after the apply: mode %s php %s status %s ip %s cert %v", got.Mode, got.PHPVersion, got.Status, got.IP, got.CertificateID)
	}
	read.IP, read.Status, read.LastError = "198.51.100.1", SiteError, "boom"
	if err := db.SaveAppliedSite(ctx, read, SiteActive, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.IP != "192.0.2.10" || got.CertificateID == nil || *got.CertificateID != cert || got.Status != SiteError || got.LastError != "boom" {
		t.Fatalf("second apply: ip %s cert %v status %s %q", got.IP, got.CertificateID, got.Status, got.LastError)
	}
	if err := db.SaveAppliedSite(ctx, &Site{ID: 999}, SiteActive, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a site that is gone: %v", err)
	}
}

// A status asked for while a job ran — a suspension, a removal — stays: the
// job's own goes in only over the status it read at its start, its error
// with it.
func TestSiteStatusOnlyOverWhatTheJobRead(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4", Mode: ModeFPM}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSiteStatus(ctx, s.ID, SiteActive, ""); err != nil {
		t.Fatal(err)
	}
	read, _ := db.GetSite(ctx, s.ID)
	from := read.Status
	if err := db.SetSiteStatus(ctx, s.ID, SiteSuspended, ""); err != nil {
		t.Fatal(err)
	}
	read.Status, read.LastError = SiteActive, ""
	if err := db.SaveAppliedSite(ctx, read, from, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteSuspended {
		t.Fatalf("the apply overwrote the suspension: %s", got.Status)
	}
	if err := db.SetSiteStatusFrom(ctx, s.ID, from, SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteSuspended || got.LastError != "" {
		t.Fatalf("the failure overwrote the suspension: %s %q", got.Status, got.LastError)
	}
	if err := db.SetSiteStatusFrom(ctx, s.ID, SiteSuspended, SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteError || got.LastError != "boom" {
		t.Fatalf("a failure over what the job read: %s %q", got.Status, got.LastError)
	}
}

// sites.move-ip moves the address and nothing else: a change made since the
// job read the site stays, an error goes back to pending (and nothing else
// does), and a site that left the address or is being removed stays where
// it is.
func TestMoveSiteIP(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	site := func(domain, status string) *Site {
		t.Helper()
		s := &Site{UserID: u.ID, Domain: domain, PHPVersion: "8.4", IP: "192.0.2.1", Status: status, LastError: "nginx: bind() failed"}
		if err := db.CreateSite(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	failed, active, suspended, deleting := site("a.example.com", SiteError), site("b.example.com", SiteActive), site("c.example.com", SiteSuspended), site("d.example.com", SiteDeleting)
	changed, _ := db.GetSite(ctx, active.ID)
	changed.PHPVersion, changed.ClientMaxBody = "8.3", "128m"
	if err := db.UpdateSite(ctx, changed); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		site   *Site
		moved  bool
		ip     string
		status string
	}{
		{failed, true, "198.51.100.7", SitePending},
		{active, true, "198.51.100.7", SiteActive},
		{suspended, true, "198.51.100.7", SiteSuspended},
		{deleting, false, "192.0.2.1", SiteDeleting},
	} {
		moved, err := db.MoveSiteIP(ctx, c.site.ID, "192.0.2.1", "198.51.100.7")
		if err != nil || moved != c.moved {
			t.Fatalf("%s: moved %v, %v", c.site.Domain, moved, err)
		}
		got, _ := db.GetSite(ctx, c.site.ID)
		if got.IP != c.ip || got.Status != c.status || got.LastError != "nginx: bind() failed" {
			t.Errorf("%s: ip %s status %s error %q", c.site.Domain, got.IP, got.Status, got.LastError)
		}
	}
	if got, _ := db.GetSite(ctx, active.ID); got.PHPVersion != "8.3" || got.ClientMaxBody != "128m" {
		t.Errorf("the change made meanwhile is lost: php %s, client_max_body %s", got.PHPVersion, got.ClientMaxBody)
	}
	// Moved once already, or put elsewhere by hand: no longer on the address.
	if moved, err := db.MoveSiteIP(ctx, active.ID, "192.0.2.1", "203.0.113.9"); err != nil || moved {
		t.Fatalf("a site no longer on the address: moved %v, %v", moved, err)
	}
	if got, _ := db.GetSite(ctx, active.ID); got.IP != "198.51.100.7" {
		t.Fatalf("a site no longer on the address moved to %s", got.IP)
	}
	if moved, err := db.MoveSiteIP(ctx, 999, "192.0.2.1", "198.51.100.7"); err != nil || moved {
		t.Fatalf("a site that is gone: moved %v, %v", moved, err)
	}
}

// A change written as what differs from the site as it was read goes in
// alone: the address sites.move-ip gave the site meanwhile and a field
// another request changed stay. The way back from a failure goes in only
// while the site is still failed, a status asked for over any other, and
// nothing over a removal asked for meanwhile.
func TestUpdateSiteChanges(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4", IP: "192.0.2.1", Status: SiteError, LastError: "nginx: bind() failed", PHPIni: map[string]string{"memory_limit": "256M"}}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	read, _ := db.GetSite(ctx, s.ID)
	was := read.Clone()
	read.ClientMaxBody, read.PHPIni["upload_max_filesize"], read.Aliases, read.Status = "128m", "64M", []string{"www.example.com"}, SitePending
	if len(was.PHPIni) != 1 || len(was.Aliases) != 0 || was.Status != SiteError {
		t.Fatalf("the copy shares the site's fields: %+v", was)
	}
	// Meanwhile: sites.move-ip, and another request changes the mode.
	if moved, err := db.MoveSiteIP(ctx, s.ID, "192.0.2.1", "198.51.100.7"); err != nil || !moved {
		t.Fatalf("move: %v %v", moved, err)
	}
	other, _ := db.GetSite(ctx, s.ID)
	other.Mode = ModeApache
	if err := db.UpdateSite(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteChanges(ctx, was, read); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSite(ctx, s.ID)
	if got.IP != "198.51.100.7" || got.Mode != ModeApache {
		t.Errorf("what changed meanwhile is lost: ip %s, mode %s", got.IP, got.Mode)
	}
	if got.ClientMaxBody != "128m" || got.PHPIni["upload_max_filesize"] != "64M" || got.PHPIni["memory_limit"] != "256M" || len(got.Aliases) != 1 || got.Status != SitePending || got.LastError != "nginx: bind() failed" {
		t.Errorf("the change: client_max_body %s, php_ini %v, aliases %v, status %s %q", got.ClientMaxBody, got.PHPIni, got.Aliases, got.Status, got.LastError)
	}

	// The way back from a failure, read as failed, over a suspension given
	// after the read: the suspension stays, the rest of the change goes in.
	if err := db.SetSiteStatus(ctx, s.ID, SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	read, _ = db.GetSite(ctx, s.ID)
	was = read.Clone()
	read.Status, read.HTTP2 = SitePending, true
	if err := db.SetSiteStatus(ctx, s.ID, SiteSuspended, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteChanges(ctx, was, read); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteSuspended || !got.HTTP2 {
		t.Errorf("the way back from a failure over a suspension: status %s, http2 %v", got.Status, got.HTTP2)
	}
	// A status asked for — the end of the suspension — goes in over one
	// given meanwhile (an apply of the suspended site that failed).
	read, _ = db.GetSite(ctx, s.ID)
	was = read.Clone()
	read.Status = SitePending
	if err := db.SetSiteStatus(ctx, s.ID, SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteChanges(ctx, was, read); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SitePending {
		t.Errorf("a status asked for over another: %s", got.Status)
	}
	// Nothing changed is no error.
	if err := db.UpdateSiteChanges(ctx, read, read); err != nil {
		t.Fatalf("nothing to change: %v", err)
	}

	// A removal asked for after the read: nothing goes in, the site stays
	// being removed, and the caller hears it — nothing to change included.
	read, _ = db.GetSite(ctx, s.ID)
	was = read.Clone()
	read.Status, read.HTTP3 = SiteSuspended, true
	if err := db.MarkSiteDeleting(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteChanges(ctx, was, read); !errors.Is(err, ErrBeingRemoved) {
		t.Fatalf("over a removal: %v", err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteDeleting || got.HTTP3 {
		t.Errorf("over a removal: status %s, http3 %v", got.Status, got.HTTP3)
	}
	if err := db.UpdateSiteChanges(ctx, was, was); !errors.Is(err, ErrBeingRemoved) {
		t.Fatalf("nothing to change over a removal: %v", err)
	}
	// A site that is gone is ErrNotFound.
	gone := read.Clone()
	gone.ID = 999
	if err := db.UpdateSiteChanges(ctx, gone, gone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a site that is gone: %v", err)
	}
}

// The removal's mark changes the status alone: what another request changed
// after the removal read the site stays. A site marked already is marked
// again; a site that is gone is ErrNotFound.
func TestMarkSiteDeleting(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4", IP: "192.0.2.1", Status: SiteError, LastError: "boom"}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	if moved, err := db.MoveSiteIP(ctx, s.ID, "192.0.2.1", "198.51.100.7"); err != nil || !moved {
		t.Fatalf("move: %v %v", moved, err)
	}
	for range 2 {
		if err := db.MarkSiteDeleting(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := db.GetSite(ctx, s.ID); got.Status != SiteDeleting || got.IP != "198.51.100.7" || got.LastError != "boom" {
			t.Fatalf("after the mark: status %s, ip %s, last error %q", got.Status, got.IP, got.LastError)
		}
	}
	if err := db.MarkSiteDeleting(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a site that is gone: %v", err)
	}
}

// A site and its untouched copy are the same in every column: a column in
// which they differed would be written by every UpdateSiteChanges, over
// what another writer put there meanwhile. So for a site read from the
// store, with its empty lists and map, and for one made in code without
// them: to the store, none is the empty one.
func TestSiteCloneComparesEqualToTheSite(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4"}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	read, err := db.GetSite(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	same := func(what string, a, b *Site) {
		t.Helper()
		x, y := siteColumns(a), siteColumns(b)
		for i := range x {
			if x[i].value != y[i].value {
				t.Errorf("%s: column %s: %v against %v", what, x[i].name, x[i].value, y[i].value)
			}
		}
	}
	same("read from the store", read, read.Clone())
	none := &Site{Domain: "example.com"}
	same("made in code", none, none.Clone())
	same("none and empty", none, &Site{Domain: "example.com", Aliases: []string{}, AllowFrom: []string{}, PHPIni: map[string]string{}})
}

// A site without aliases — every site as created, until one is added: a
// change read before another request added an alias leaves the alias alone.
func TestUpdateSiteChangesKeepsAnAliasAddedMeanwhile(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4"}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	read, _ := db.GetSite(ctx, s.ID)
	was := read.Clone()
	read.ClientMaxBody = "128m"
	other, _ := db.GetSite(ctx, s.ID)
	otherWas := other.Clone()
	other.Aliases = []string{"www.example.com"}
	if err := db.UpdateSiteChanges(ctx, otherWas, other); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteChanges(ctx, was, read); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSite(ctx, s.ID); got.ClientMaxBody != "128m" || len(got.Aliases) != 1 || got.Aliases[0] != "www.example.com" {
		t.Errorf("client_max_body %s, aliases %v (want 128m, [www.example.com])", got.ClientMaxBody, got.Aliases)
	}
}

// Setting a field back to nothing — "", 0, false, no certificate, an empty
// list or map — is a change, and it goes in.
func TestUpdateSiteChangesWritesZeroValues(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	c := &Certificate{Name: "example.com", Names: []string{"example.com"}, Kind: CertKindACME, KeyType: "ec256", Status: CertPending}
	if err := db.UpsertCertificate(ctx, c); err != nil {
		t.Fatal(err)
	}
	cid := c.ID
	s := &Site{UserID: u.ID, Domain: "example.com", PHPVersion: "8.4", Aliases: []string{"www.example.com"}, AllowFrom: []string{"192.0.2.0/24"},
		PHPIni: map[string]string{"memory_limit": "256M"}, IPv6: "2001:db8::1", HTTP2: true, HTTP3: true, RedirectHTTPS: true, StaticByNginx: true,
		AllowExec: true, SecurityHeaders: true, RateLimit: 10, CertificateID: &cid, Backend: "http://127.0.0.1:3000", Preset: "wordpress",
		CMS: "wordpress", CMSVersion: "6.9", CMSAt: "2026-01-01T00:00:00Z", CMSDatabase: "alex_wordpress", SessionStore: SessionStoreValkey,
		Docroot: "public_html", IP: "192.0.2.1", LastError: "boom", Status: SiteError}
	if err := db.CreateSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	read, _ := db.GetSite(ctx, s.ID)
	was := read.Clone()
	read.Aliases, read.AllowFrom, read.PHPIni = nil, nil, nil
	read.IPv6, read.HTTP2, read.HTTP3, read.RedirectHTTPS, read.StaticByNginx, read.AllowExec, read.SecurityHeaders, read.RateLimit = "", false, false, false, false, false, false, 0
	read.CertificateID, read.Backend, read.Preset, read.CMS, read.CMSVersion, read.CMSAt, read.CMSDatabase, read.SessionStore = nil, "", "", "", "", "", "", ""
	read.Docroot, read.IP, read.LastError, read.Status = "", "", "", SitePending
	if err := db.UpdateSiteChanges(ctx, was, read); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSite(ctx, s.ID)
	if len(got.Aliases) != 0 || len(got.AllowFrom) != 0 || len(got.PHPIni) != 0 || got.IPv6 != "" || got.HTTP2 || got.HTTP3 || got.RedirectHTTPS ||
		got.StaticByNginx || got.AllowExec || got.SecurityHeaders || got.RateLimit != 0 || got.CertificateID != nil || got.Backend != "" ||
		got.Preset != "" || got.CMS != "" || got.CMSVersion != "" || got.CMSAt != "" || got.CMSDatabase != "" || got.SessionStore != "" ||
		got.Docroot != "" || got.IP != "" || got.LastError != "" || got.Status != SitePending {
		t.Errorf("not set back: %+v", got)
	}
}
