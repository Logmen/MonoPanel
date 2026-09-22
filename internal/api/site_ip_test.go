package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
)

// The host's address changed (cloud-init gave the VM another one): the
// default server of the old address goes, the doctor names the sites that
// still listen there, and move-ip brings them all to the new address.
func TestSitesMoveToNewHostAddress(t *testing.T) {
	f := newSiteFixture(t)
	ips := []string{"10.0.0.1"}
	f.s.SetHostIPs(func() []string { return ips })
	for _, d := range []string{"one.example.com", "two.example.com"} {
		if site := f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "none"}); site.IP != "10.0.0.1" {
			t.Fatalf("%s: ip %q", d, site.IP)
		}
	}
	f.call(http.MethodPatch, "/sites/one.example.com", map[string]any{"ip": "192.0.2.7"}, http.StatusUnprocessableEntity, nil)

	ips = []string{"10.0.0.2"}
	dir := f.s.defaultServersDir()
	f.agent.DirEntries = map[string][]string{dir: {"ip-10.0.0.1.conf", "ip-10.0.0.2.conf", "README"}}
	doc := f.s.doctor(f.ctx)
	found := false
	for _, c := range doc.Checks {
		if c.Name == "site addresses" {
			found = c.Status == "fail" && strings.Contains(c.Detail, "10.0.0.1 (one.example.com, two.example.com)") && strings.Contains(c.Detail, "mp site move-ip 10.0.0.2")
		}
	}
	if !found {
		t.Errorf("doctor не назвал сайты на пропавшем адресе: %+v", doc.Checks)
	}

	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"to": "192.0.2.7"}, http.StatusUnprocessableEntity, nil)
	var res apitypes.SiteMoveIPResponse
	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"to": "10.0.0.2"}, http.StatusAccepted, &res)
	if len(res.Sites) != 2 {
		t.Fatalf("сайты: %+v", res)
	}
	if job := f.waitJob(res.JobID); job.Status != "done" {
		t.Fatalf("move-ip: %s %s", job.Status, job.Error)
	}
	removed := strings.Join(f.agent.Removed(), " ")
	if !strings.Contains(removed, dir+"/ip-10.0.0.1.conf") || strings.Contains(removed, "ip-10.0.0.2.conf") {
		t.Errorf("default-серверы: удалено %s", removed)
	}
	for _, d := range []string{"one.example.com", "two.example.com"} {
		site, _ := f.db.GetSiteByDomain(f.ctx, d)
		if site.IP != "10.0.0.2" {
			t.Errorf("%s: ip %s", d, site.IP)
		}
	}
	// The sites come back through their own apply jobs.
	jobs, _ := f.db.ListJobs(f.ctx, 50, "")
	for _, j := range jobs {
		if j.Type == "site.apply" {
			f.waitJob(j.ID)
		}
	}
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/two.example.com.conf")
	if !strings.Contains(conf, "listen 10.0.0.2:80;") {
		t.Errorf("сайт слушает не новый адрес:\n%s", conf)
	}
	if _, ok := f.agent.File(dir + "/ip-10.0.0.2.conf"); !ok {
		t.Error("default-сервер нового адреса не записан")
	}
}
