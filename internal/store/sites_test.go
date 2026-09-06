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
