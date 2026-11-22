package api

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

const (
	alexBackend      = alexApacheDir + "/backend.conf"
	alexNginxBackend = "/etc/nginx/monopanel/apache/alex.conf"
	alexInclude      = "include monopanel/apache/alex.conf;"
)

// alexSecret is the backend secret of alex's Apache in the panel's database
// ("" without one).
func (f *siteFixture) alexSecret() string {
	f.t.Helper()
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		f.t.Fatal(err)
	}
	secret, err := f.s.apacheSecret(f.ctx, alex, false)
	if err != nil {
		f.t.Fatal(err)
	}
	return secret
}

// rootSecretFile wants the file written as the account's secret: the one
// form, 0600, root's.
func (f *siteFixture) rootSecretFile(p, want string) {
	f.t.Helper()
	spec, ok := f.agent.FileSpec(p)
	if !ok {
		f.t.Fatalf("no %s; files: %v", p, f.agent.Files())
	}
	if spec.Content != want || spec.Mode != 0o600 || spec.Owner != "root" || spec.Group != "root" {
		f.t.Fatalf("%s: %04o %s:%s %q, want 0600 root:root %q", p, spec.Mode, spec.Owner, spec.Group, spec.Content, want)
	}
}

// The first site in Apache mode brings up an Apache that demands the
// account's secret from its first start — the check is in the set that
// starts it — and nginx, sent there afterwards, carries the secret in both
// locations, from a root-only file in the set of the site's server block.
func TestApacheDemandsTheBackendSecretFromItsFirstStart(t *testing.T) {
	f := newApacheFixture(t)
	site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache", "static_by_nginx": true})
	if site.Status != store.SiteActive {
		t.Fatalf("site: %s %s", site.Status, site.LastError)
	}
	secret := f.alexSecret()
	if !confcheck.ValidApacheBackendSecret(secret) {
		t.Fatalf("secret %q", secret)
	}
	// Kept encrypted, like the panel's other secrets.
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if enc, err := f.db.GetSetting(f.ctx, apacheBackendSetting(alex)); err != nil || strings.Contains(enc, secret) || !strings.HasPrefix(enc, "v1:") {
		t.Fatalf("stored as %q (%v)", enc, err)
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(secret))
	f.rootSecretFile(alexNginxBackend, confcheck.NginxApacheBackendConf(secret))
	if main, _ := f.agent.File(alexApacheConf); !strings.Contains(main, "\nInclude "+alexBackend+"\n") {
		t.Fatalf("httpd.conf does not include its check:\n%s", main)
	}
	// The site's virtual host inherits the check's rewrite rule.
	if vhost, _ := f.agent.File(alexApacheDir + "/sites/a.example.com.conf"); !strings.Contains(vhost, "\n    RewriteEngine On\n") {
		t.Fatalf("the virtual host does not switch mod_rewrite on:\n%s", vhost)
	}
	nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf")
	if strings.Count(nginx, alexInclude) != 2 || strings.Contains(nginx, secret) {
		t.Fatalf("nginx does not carry the secret from its file in both locations:\n%s", nginx)
	}
	start := f.callAt(0, reloads(alexApache))
	if start < 0 || !writes(alexBackend)(f.agent.Calls()[start]) || !writes(alexApacheConf)(f.agent.Calls()[start]) {
		t.Fatalf("the check is not in the set that starts the Apache (at %d)", start)
	}
	if f.callAt(0, writes(alexBackend)) != start {
		t.Fatal("the check was written before the set that starts the Apache")
	}
	nginxAt := f.callAt(0, func(c agenttest.Call) bool {
		return writes("/etc/nginx/monopanel/sites/a.example.com.conf")(c) && writes(alexNginxBackend)(c) && reloads("nginx.service")(c)
	})
	if nginxAt < 0 || nginxAt < start {
		t.Fatalf("nginx's secret at %d, in the set of the server block? The Apache started at %d", nginxAt, start)
	}
	// Nothing the panel logs or answers carries it.
	job, err := f.db.ListJobs(f.ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range job {
		if strings.Contains(j.Log, secret) || strings.Contains(j.Message, secret) {
			t.Fatalf("job %d logs the secret", j.ID)
		}
	}
	var out apitypes.SiteNginx
	f.call(http.MethodGet, "/sites/a.example.com/nginx", nil, http.StatusOK, &out)
	if strings.Contains(out.Generated, secret) {
		t.Fatal("the site's nginx configuration shows the secret")
	}
}

// The secret stays what it is across applies and the account's sites, and
// goes with the Apache: a new Apache gets a new one.
func TestApacheBackendSecretGoesWithTheApache(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	first := f.alexSecret()
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.siteAction("a.example.com", "apply")
	if f.alexSecret() != first {
		t.Fatal("the secret changed with an apply")
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(first))
	f.rootSecretFile(alexNginxBackend, confcheck.NginxApacheBackendConf(first))
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/" + d + ".conf"); strings.Count(nginx, alexInclude) != 2 {
			t.Fatalf("%s:\n%s", d, nginx)
		}
	}

	// b leaves Apache mode: the Apache keeps a, and its secret.
	f.updateSite("b.example.com", map[string]any{"mode": "fpm"})
	if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/b.example.com.conf"); strings.Contains(nginx, "monopanel/apache/") {
		t.Fatalf("an fpm site includes the secret:\n%s", nginx)
	}
	if f.alexSecret() != first {
		t.Fatal("the secret changed while the Apache runs")
	}
	// a, the last site there, suspended: the Apache retires, both files and
	// the secret with it — nginx's file after nginx stopped sending there,
	// checked with nginx -t.
	f.siteAction("a.example.com", "suspend")
	for _, p := range []string{alexBackend, alexNginxBackend} {
		if _, ok := f.agent.File(p); ok {
			t.Fatalf("%s stays with the Apache retired", p)
		}
	}
	if f.alexSecret() != "" {
		t.Fatal("the secret stays with the Apache retired")
	}
	gone := f.callAt(0, func(c agenttest.Call) bool {
		req, ok := removeOf(c)
		return ok && slices.Equal(req.Paths, []string{alexNginxBackend}) && slices.ContainsFunc(req.Validate, func(v []string) bool { return slices.Contains(v, "-t") })
	})
	if gone < 0 {
		t.Fatal("nginx's file is not removed with nginx -t")
	}
	// Back: a new Apache, a new secret.
	f.siteAction("a.example.com", "unsuspend")
	second := f.alexSecret()
	if second == "" || second == first {
		t.Fatalf("the new Apache's secret %q (the old one %q)", second, first)
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(second))
	f.rootSecretFile(alexNginxBackend, confcheck.NginxApacheBackendConf(second))
	// Removed with its last site, and with the account.
	f.deleteSite("a.example.com")
	if _, ok := f.agent.File(alexNginxBackend); ok || f.alexSecret() != "" {
		t.Fatal("the secret stays after the account's last site in Apache mode went")
	}
}

// A site's server block that still includes nginx's file — one whose apply
// failed half-way — keeps it, and the secret: nginx -t fails without it.
func TestApacheBackendStaysWhileNginxNeedsIt(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	secret := f.alexSecret()
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	f.agent.Fail["/v1/paths/remove"] = "nginx: [emerg] open() \"/etc/nginx/monopanel/apache/alex.conf\" failed"
	f.agent.FailStatus = map[string]int{"/v1/paths/remove": http.StatusUnprocessableEntity}
	var logs []string
	f.s.dropApacheBackend(f.ctx, func(format string, a ...any) { logs = append(logs, format) }, alex)
	delete(f.agent.Fail, "/v1/paths/remove")
	if f.alexSecret() != secret {
		t.Fatal("the secret went while nginx needs its file")
	}
	f.rootSecretFile(alexNginxBackend, confcheck.NginxApacheBackendConf(secret))
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "stays") {
		t.Fatalf("logs: %v", logs)
	}
}

// After the upgrade a running Apache without its backend.conf lost it (by
// hand): its httpd.conf includes the file, so a set with the virtual host
// alone would fail apache2 -t, and nginx sends the secret for every site
// already. The next apply writes the check again, with httpd.conf.
func TestApacheBackendWrittenAgainWhenItWentMissing(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	secret := f.alexSecret()
	if err := f.db.SetSetting(f.ctx, settingApacheBackends, "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.agent.RemovePaths(f.ctx, &agent.RemovePathsRequest{Paths: []string{alexBackend}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.agent.File(alexBackend); ok {
		t.Fatal("backend.conf still there")
	}
	from := len(f.agent.Calls())
	f.siteAction("a.example.com", "apply")
	at := f.callAt(from, func(c agenttest.Call) bool {
		return writes(alexBackend)(c) && writes(alexApacheConf)(c) && reloads(alexApache)(c)
	})
	if at < 0 {
		t.Fatal("the check is not written again with httpd.conf")
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(secret))
	if f.alexSecret() != secret {
		t.Fatal("the secret changed")
	}
}

// The account goes, its secret with it, even when its Apache kept it.
func TestAccountRemovalTakesTheBackendSecret(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.jobs.Enqueue(f.ctx, "user.delete", userDeletePayload{UserID: alex.ID, Login: "alex"})
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(job.ID); j.Status != store.JobDone {
		t.Fatalf("delete: %s %s", j.Status, j.Error)
	}
	if _, err := f.db.GetSetting(f.ctx, apacheBackendSetting(alex)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the secret of a removed account: %v", err)
	}
	if _, ok := f.agent.File(alexNginxBackend); ok {
		t.Fatal("nginx's file of a removed account stays")
	}
}

// preSecretLayout leaves alex's Apache as it ran before the backend secret:
// running, its httpd.conf without the check, no backend.conf, the sites'
// server blocks without nginx's file, no secret in the database.
func (f *siteFixture) preSecretLayout(domains ...string) {
	f.t.Helper()
	strip := map[string]string{}
	main, _ := f.agent.File(alexApacheConf)
	include := "\nInclude " + alexBackend + "\n"
	if !strings.Contains(main, include) {
		f.t.Fatalf("httpd.conf:\n%s", main)
	}
	strip[alexApacheConf] = strings.Replace(main, include, "\n", 1)
	for _, d := range domains {
		vhost := alexApacheDir + "/sites/" + d + ".conf"
		v, _ := f.agent.File(vhost)
		strip[vhost] = strings.Replace(v, "\n    RewriteEngine On\n", "\n", 1)
		conf := "/etc/nginx/monopanel/sites/" + d + ".conf"
		nginx, _ := f.agent.File(conf)
		strip[conf] = strings.ReplaceAll(nginx, "        "+alexInclude+"\n", "")
	}
	f.agent.Reset()
	var files []agent.FileSpec
	for p, c := range strip {
		files = append(files, agent.FileSpec{Path: p, Content: c, Mode: 0o644})
	}
	if _, err := f.s.agent.ApplyConfigSet(f.ctx, &agent.ApplyConfigSetRequest{Files: files}); err != nil {
		f.t.Fatal(err)
	}
	f.agent.SetInstance(alexApache, true, true)
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	for k, v := range map[string]string{apacheBackendSetting(alex): "", settingApacheBackends: ""} {
		if err := f.db.DeleteSetting(f.ctx, k); err != nil {
			f.t.Fatal(err)
		}
		_ = v
	}
}

// An Apache that ran before the backend secret gets it once, at the next
// start: nginx sends the secret for every site of the account first — the
// Apache, not asking for it yet, gets nothing but the sites' virtual hosts —
// and only then does the Apache demand it, so no site is refused for a
// moment longer than none. The marker keeps it from running again.
func TestUpgradeGivesRunningApachesTheirBackendSecret(t *testing.T) {
	f := newApacheFixture(t)
	for _, d := range []string{"a.example.com", "b.example.com"} {
		f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	}
	f.preSecretLayout("a.example.com", "b.example.com")
	from := len(f.agent.Calls())

	f.s.refreshApacheBackends(f.ctx)
	if job := f.waitJobOfType("apache.backend"); job.Status != store.JobDone {
		t.Fatalf("apache.backend: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	secret := f.alexSecret()
	if secret == "" {
		t.Fatal("no secret")
	}
	demand := f.callAt(from, func(c agenttest.Call) bool { return writes(alexBackend)(c) && reloads(alexApache)(c) })
	if demand < 0 {
		t.Fatal("the Apache never demands the secret")
	}
	req, _ := applyOf(f.agent.Calls()[demand])
	if !writes(alexApacheConf)(f.agent.Calls()[demand]) || !slices.ContainsFunc(req.Validate, func(v []string) bool { return slices.Equal(v, alexApacheCheck) }) {
		t.Fatalf("the check comes without its include, or unchecked: %+v", req)
	}
	// Before that, nothing of the Apache's own changes: httpd.conf and the
	// check come in that one set.
	if at := f.callAt(from, writes(alexApacheConf)); at != demand {
		t.Fatalf("httpd.conf written at %d, before the Apache demands the secret at %d", at, demand)
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		at := f.callAt(from, func(c agenttest.Call) bool {
			return writes("/etc/nginx/monopanel/sites/"+d+".conf")(c) && writes(alexNginxBackend)(c) && reloads("nginx.service")(c)
		})
		if at < 0 || at > demand {
			t.Fatalf("%s: nginx sends the secret at %d, the Apache demands it at %d", d, at, demand)
		}
		if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/" + d + ".conf"); strings.Count(nginx, alexInclude) != 2 {
			t.Fatalf("%s:\n%s", d, nginx)
		}
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(secret))
	f.rootSecretFile(alexNginxBackend, confcheck.NginxApacheBackendConf(secret))
	if active, _ := f.agent.InstanceState(alexApache); !active {
		t.Fatal("alex's Apache does not run")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheBackends); v != "yes" {
		t.Fatalf("marker %q", v)
	}
	// Once only; and run again, it finds nothing left to do.
	from = len(f.agent.Calls())
	f.s.refreshApacheBackends(f.ctx)
	if err := f.db.SetSetting(f.ctx, settingApacheBackends, ""); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.jobs.Enqueue(f.ctx, "apache.backend", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(job.ID); j.Status != store.JobDone {
		t.Fatalf("again: %s %s", j.Status, j.Error)
	}
	for _, c := range f.agent.Calls()[from:] {
		if c.Path == "/v1/config/apply" || c.Path == "/v1/paths/remove" {
			t.Fatalf("ran again: %s %s", c.Path, c.Body)
		}
	}
	if f.alexSecret() != secret {
		t.Fatal("the secret changed")
	}
}

// An Apache set up before the backend secret that does not run at the
// upgrade — failed, or stopped by hand, but enabled — would start at the
// next boot with its old httpd.conf, letting in whatever comes, and a site's
// apply would not change that while it runs. The job gives it its check
// now: the first of its sites starts it with the check, the others follow.
func TestUpgradeGivesStoppedApachesTheirBackendSecret(t *testing.T) {
	f := newApacheFixture(t)
	for _, d := range []string{"a.example.com", "b.example.com"} {
		f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	}
	f.preSecretLayout("a.example.com", "b.example.com")
	f.agent.SetInstance(alexApache, false, true)
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if !slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool { return c.Name == "backend "+alexApache && c.Status == "warn" }) {
		t.Fatalf("doctor: %+v", doc.Checks)
	}

	f.s.refreshApacheBackends(f.ctx)
	if job := f.waitJobOfType("apache.backend"); job.Status != store.JobDone {
		t.Fatalf("apache.backend: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	secret := f.alexSecret()
	if secret == "" {
		t.Fatal("no secret")
	}
	f.rootSecretFile(alexBackend, confcheck.ApacheBackendConf(secret))
	if main, _ := f.agent.File(alexApacheConf); !strings.Contains(main, "\nInclude "+alexBackend+"\n") {
		t.Fatalf("httpd.conf without the check:\n%s", main)
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/" + d + ".conf"); strings.Count(nginx, alexInclude) != 2 {
			t.Fatalf("%s:\n%s", d, nginx)
		}
		if vhost, _ := f.agent.File(alexApacheDir + "/sites/" + d + ".conf"); !strings.Contains(vhost, "\n    RewriteEngine On\n") {
			t.Fatalf("%s's virtual host:\n%s", d, vhost)
		}
	}
	if active, _ := f.agent.InstanceState(alexApache); !active {
		t.Fatal("alex's Apache does not run")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheBackends); v != "yes" {
		t.Fatalf("marker %q", v)
	}
	doc = apitypes.Doctor{}
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool { return strings.HasPrefix(c.Name, "backend ") }) {
		t.Fatalf("doctor: %+v", doc.Checks)
	}
}

// A site that does not go through keeps its account's Apache as it was —
// answering whatever comes, so that the site nginx does not send the secret
// for is not refused — and the job names it; the next start tries again.
func TestUpgradeKeepsTheApacheOpenForASiteThatFails(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.preSecretLayout("a.example.com", "b.example.com")
	// b's PHP branch went away since: it has nothing to run on.
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.s.refreshApacheBackends(f.ctx)
	job := f.waitJobOfType("apache.backend")
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "b.example.com") || !strings.Contains(job.Error, "mp stack install apache") {
		t.Fatalf("apache.backend: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.File(alexBackend); ok {
		t.Fatal("the Apache demands the secret, which b's nginx does not send")
	}
	if main, _ := f.agent.File(alexApacheConf); strings.Contains(main, alexBackend) {
		t.Fatal("httpd.conf includes the check")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheBackends); v == "yes" {
		t.Fatal("marked done")
	}
	// The doctor says so.
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if !slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool {
		return c.Name == "backend "+alexApache && c.Status == "warn" && strings.Contains(c.Detail, "any local process")
	}) {
		t.Fatalf("doctor: %+v", doc.Checks)
	}
}

// The probe of a started Apache carries the account's secret, as nginx's
// requests do, and asks OPTIONS *, which no site's configuration or code
// answers.
func TestWaitApacheSendsTheBackendSecret(t *testing.T) {
	f := newApacheFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	type probe struct{ method, uri, host, secret string }
	got := make(chan probe, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if r, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
				got <- probe{r.Method, r.RequestURI, r.Host, r.Header.Get(confcheck.ApacheBackendHeader)}
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")) //nolint:errcheck // test server
			}
			c.Close()
		}
	}()
	dialed := make(chan string, 4)
	f.s.apacheWait = 5 * time.Second
	f.s.apacheDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed <- addr
		return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
	}
	site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if site.Status != store.SiteActive {
		t.Fatalf("site: %s %s", site.Status, site.LastError)
	}
	select {
	case p := <-got:
		if p.method != http.MethodOptions || p.uri != "*" || p.secret != f.alexSecret() || p.host != "localhost" {
			t.Fatalf("probe %+v, secret %q", p, f.alexSecret())
		}
	default:
		t.Fatal("the Apache was not probed")
	}
	if addr := <-dialed; addr != alexApacheAddr {
		t.Fatalf("probed %s", addr)
	}
}

// The doctor: a shared Apache that is off but enabled would start at boot
// with the shared configuration; off and disabled it is fine.
func TestDoctorWarnsOfTheSharedApacheEnabledAtBoot(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	status := func() (string, string) {
		var doc apitypes.Doctor
		f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
		for _, c := range doc.Checks {
			if strings.HasPrefix(c.Name, "backend ") {
				t.Fatalf("an Apache that demands its secret: %+v", c)
			}
			if c.Name == "service apache2.service" {
				return c.Status, c.Detail
			}
		}
		t.Fatalf("no check of apache2.service: %+v", doc.Checks)
		return "", ""
	}
	f.agent.ActiveState = map[string]string{"apache2.service": "inactive"}
	f.agent.UnitFileState = map[string]string{"apache2.service": "enabled"}
	if st, detail := status(); st != "warn" || !strings.Contains(detail, "boot") || !strings.Contains(detail, "systemctl disable apache2.service") {
		t.Fatalf("off but enabled: %s %s", st, detail)
	}
	f.agent.UnitFileState["apache2.service"] = "disabled"
	if st, detail := status(); st != "ok" {
		t.Fatalf("off and disabled: %s %s", st, detail)
	}
}
