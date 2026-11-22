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
