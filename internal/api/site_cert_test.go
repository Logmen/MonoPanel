package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"monopanel/internal/store"
)

// pointAt makes public DNS answer addr for the listed names and NXDOMAIN for
// everything else.
func pointAt(f *siteFixture, addr string, names ...string) {
	f.s.SetLookup(func(_ context.Context, name string) ([]string, error) {
		for _, n := range names {
			if n == name {
				return []string{addr}, nil
			}
		}
		return nil, errors.New("NXDOMAIN")
	})
}

func certJobs(f *siteFixture) int {
	jobs, _ := f.db.ListJobs(f.ctx, 100, "")
	n := 0
	for _, j := range jobs {
		if j.Type == "cert.issue" {
			n++
		}
	}
	return n
}

// An alias added to an HTTPS site: the site keeps serving its certificate
// (it names the domain) and a new one with the alias is ordered — once the
// alias points here, and only then.
func TestAliasOnHTTPSSiteReordersCertificate(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1"} })
	pointAt(f, "10.0.0.1", "shop.example.com", "www.shop.example.com")
	later := time.Now().Add(60 * 24 * time.Hour)
	if err := f.db.UpsertCertificate(f.ctx, &store.Certificate{Name: "shop.example.com", Names: []string{"shop.example.com"}, Kind: store.CertKindACME, Status: store.CertValid,
		CertPath: "/var/lib/monopanel/certs/shop.example.com/fullchain.pem", KeyPath: "/var/lib/monopanel/certs/shop.example.com/privkey.pem", NotAfter: &later}); err != nil {
		t.Fatal(err)
	}
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto"})
	if n := certJobs(f); n != 0 {
		t.Fatalf("сертификат покрывает сайт, а заказано %d", n)
	}

	apply := func(aliases ...string) {
		var out struct {
			JobID int64 `json:"job_id"`
		}
		f.call(http.MethodPatch, "/sites/shop.example.com", map[string]any{"aliases": aliases}, http.StatusAccepted, &out)
		if job := f.waitJob(out.JobID); job.Status != store.JobDone {
			t.Fatalf("apply: %s %s", job.Status, job.Error)
		}
	}
	apply("old.shop.example.com")
	if n := certJobs(f); n != 0 {
		t.Fatalf("алиас не смотрит сюда, а заказано %d", n)
	}
	apply("old.shop.example.com", "www.shop.example.com")
	if n := certJobs(f); n != 1 {
		t.Fatalf("заказов после www: %d", n)
	}
	c, _ := f.db.GetCertificateByName(f.ctx, "shop.example.com")
	if !sameNames(c.Names, []string{"shop.example.com", "www.shop.example.com"}) || c.CertPath == "" {
		t.Errorf("сертификат: %+v", c)
	}
}

// mp ssl issue <домен сайта> берёт и алиасы сайта, которые смотрят сюда:
// иначе новый сертификат заменил бы тот, где был www.
func TestSSLIssueForSiteDomainTakesAliases(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1"} })
	pointAt(f, "10.0.0.1", "shop.example.com", "www.shop.example.com")
	f.createSite(map[string]any{"domain": "shop.example.com", "aliases": []string{"old.shop.example.com"}, "www": true, "user": "alex", "php_version": "8.4", "ssl": "none"})
	var issued struct {
		Certificate struct{ Names []string }
	}
	f.call(http.MethodPost, "/certificates", map[string]any{"names": []string{"shop.example.com"}, "staging": true}, http.StatusAccepted, &issued)
	if !sameNames(issued.Certificate.Names, []string{"shop.example.com", "www.shop.example.com"}) {
		t.Errorf("имена: %v", issued.Certificate.Names)
	}
	f.call(http.MethodPost, "/certificates", map[string]any{"names": []string{"other.example.com", "shop.example.com"}, "staging": true}, http.StatusAccepted, &issued)
	if len(issued.Certificate.Names) != 2 {
		t.Errorf("явный список не трогается: %v", issued.Certificate.Names)
	}
}
