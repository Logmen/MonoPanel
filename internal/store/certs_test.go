package store

import (
	"context"
	"testing"
	"time"
)

func TestCertificates(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	c := &Certificate{Name: "panel.example.com", Names: []string{"panel.example.com", "www.panel.example.com"}, DirectoryURL: "https://acme.example/dir", AutoRenew: true}
	if err := db.UpsertCertificate(ctx, c); err != nil || c.ID == 0 {
		t.Fatalf("upsert: %v id=%d", err, c.ID)
	}
	// pending certs are not renewal candidates
	if list, _ := db.ListCertificatesForRenewal(ctx, time.Now().Add(365*24*time.Hour)); len(list) != 0 {
		t.Fatalf("pending must not be renewed: %d", len(list))
	}
	soon := time.Now().Add(10 * 24 * time.Hour)
	c.Status = CertValid
	c.NotAfter = &soon
	c.Issuer = "R11"
	if err := db.UpsertCertificate(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetCertificateByName(ctx, "panel.example.com")
	if err != nil || got.ID != c.ID || got.Issuer != "R11" || len(got.Names) != 2 || !got.AutoRenew {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, _ := db.ListCertificatesForRenewal(ctx, time.Now().Add(30*24*time.Hour))
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("renewal list: %v", list)
	}
	if list, _ := db.ListCertificatesForRenewal(ctx, time.Now().Add(24*time.Hour)); len(list) != 0 {
		t.Fatalf("not yet due: %d", len(list))
	}
	db.SetCertificateStatus(ctx, c.ID, CertError, "boom")
	got, _ = db.GetCertificate(ctx, c.ID)
	if got.Status != CertError || got.LastError != "boom" || got.LastAttempt == nil {
		t.Fatalf("status: %+v", got)
	}
	if err := db.DeleteCertificate(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCertificate(ctx, c.ID); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}
