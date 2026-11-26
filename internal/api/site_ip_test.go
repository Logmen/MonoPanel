package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
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

// moveIPHook runs fn once, while sites.move-ip takes the server block of the
// site away: under that site's lock, after the job read the site — what a
// change made beside the job meets.
func (f *siteFixture) moveIPHook(domain string, fn func()) {
	f.t.Helper()
	var once sync.Once
	conf := "/etc/nginx/monopanel/sites/" + domain + ".conf"
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/paths/remove" && strings.Contains(string(body), `"`+conf+`"`) {
			once.Do(fn)
		}
	})
	f.t.Cleanup(func() { f.agent.SetBefore(nil) })
}

// appliesAfter are the sites the site.apply jobs queued after job id apply.
func (f *siteFixture) appliesAfter(id int64) []int64 {
	f.t.Helper()
	list, err := f.db.ListJobs(f.ctx, 500, "")
	if err != nil {
		f.t.Fatal(err)
	}
	var out []int64
	for _, j := range list {
		var p sitePayload
		if j.ID > id && j.Type == "site.apply" && json.Unmarshal(j.Payload, &p) == nil {
			out = append(out, p.SiteID)
		}
	}
	slices.Sort(out)
	return out
}

// A change made to a site while sites.move-ip runs — mp site set between
// the job's read and its write — stays: the job writes the address alone.
func TestSitesMoveIPKeepsAChangeMadeMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	ips := []string{"10.0.0.1"}
	f.s.SetHostIPs(func() []string { return ips })
	site := f.createSite(map[string]any{"domain": "one.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteError, "nginx: bind() to 10.0.0.1:80 failed"); err != nil {
		t.Fatal(err)
	}
	ips = []string{"10.0.0.2"}
	f.moveIPHook("one.example.com", func() {
		cur, err := f.db.GetSite(f.ctx, site.ID)
		if err != nil {
			t.Error(err)
			return
		}
		cur.ClientMaxBody, cur.PHPIni["memory_limit"] = "128m", "512M"
		if err := f.db.UpdateSite(f.ctx, cur); err != nil {
			t.Error(err)
		}
	})
	var res apitypes.SiteMoveIPResponse
	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"to": "10.0.0.2"}, http.StatusAccepted, &res)
	if job := f.waitJob(res.JobID); job.Status != store.JobDone {
		t.Fatalf("move-ip: %s %s", job.Status, job.Error)
	}
	got, err := f.db.GetSite(f.ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != "10.0.0.2" || got.ClientMaxBody != "128m" || got.PHPIni["memory_limit"] != "512M" {
		t.Fatalf("after the move: ip %s, client_max_body %s, memory_limit %q", got.IP, got.ClientMaxBody, got.PHPIni["memory_limit"])
	}
	if !slices.Equal(f.appliesAfter(res.JobID), []int64{site.ID}) {
		t.Fatalf("applies queued: %v", f.appliesAfter(res.JobID))
	}
}

// What changed while sites.move-ip runs is read again under each site's
// lock. A site removed meanwhile is neither written nor applied; one being
// removed, or whose account is, loses its server block — it listens on the
// address the host lost, as the removal would take it — but does not move
// and gets no apply; one an administrator put on another address is left
// as it is.
func TestSitesMoveIPLeavesWhatChangedMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	ips := []string{"10.0.0.1", "10.0.0.3"}
	f.s.SetHostIPs(func() []string { return ips })
	bob := f.userWithPassword("bob", "bob-password", 1501)
	sites := map[string]*store.Site{}
	for _, c := range [][2]string{{"a.example.com", "alex"}, {"b.example.com", "alex"}, {"c.example.com", "alex"}, {"d.example.com", "bob"}, {"e.example.com", "alex"}} {
		sites[c[0]] = f.createSite(map[string]any{"domain": c[0], "user": c[1], "php_version": "8.4", "ssl": "none", "ip": "10.0.0.1"})
	}
	ips = []string{"10.0.0.2", "10.0.0.3"}
	// While the job takes a.example.com's server block away, after it
	// listed every site.
	f.moveIPHook("a.example.com", func() {
		if err := f.db.DeleteSite(f.ctx, sites["b.example.com"].ID); err != nil {
			t.Error(err)
		}
		if err := f.db.SetSiteStatus(f.ctx, sites["c.example.com"].ID, store.SiteDeleting, ""); err != nil {
			t.Error(err)
		}
		if err := f.db.SetUserStatus(f.ctx, bob.ID, store.UserDeleting); err != nil {
			t.Error(err)
		}
		e, err := f.db.GetSite(f.ctx, sites["e.example.com"].ID)
		if err != nil {
			t.Error(err)
			return
		}
		e.IP = "10.0.0.3"
		if err := f.db.UpdateSite(f.ctx, e); err != nil {
			t.Error(err)
		}
	})
	before := len(f.agent.Removed())
	var res apitypes.SiteMoveIPResponse
	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"from": "10.0.0.1", "to": "10.0.0.2"}, http.StatusAccepted, &res)
	if len(res.Sites) != 5 {
		t.Fatalf("listed: %v", res.Sites)
	}
	job := f.waitJob(res.JobID)
	if job.Status != store.JobDone {
		t.Fatalf("move-ip: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if _, err := f.db.GetSite(f.ctx, sites["b.example.com"].ID); err == nil {
		t.Fatal("the site removed meanwhile is back")
	}
	for domain, want := range map[string]string{"a.example.com": "10.0.0.2", "c.example.com": "10.0.0.1", "d.example.com": "10.0.0.1", "e.example.com": "10.0.0.3"} {
		got, err := f.db.GetSite(f.ctx, sites[domain].ID)
		if err != nil || got.IP != want {
			t.Errorf("%s: %v %v, want the address %s", domain, got, err, want)
		}
	}
	if got, _ := f.db.GetSite(f.ctx, sites["c.example.com"].ID); got.Status != store.SiteDeleting {
		t.Errorf("c.example.com is %s, not being removed", got.Status)
	}
	removed := f.agent.Removed()[before:]
	for domain, gone := range map[string]bool{"a.example.com": true, "b.example.com": false, "c.example.com": true, "d.example.com": true, "e.example.com": false} {
		if slices.Contains(removed, "/etc/nginx/monopanel/sites/"+domain+".conf") != gone {
			t.Errorf("%s: server block removed %v, want %v (removed: %v)", domain, !gone, gone, removed)
		}
	}
	if got := f.appliesAfter(res.JobID); !slices.Equal(got, []int64{sites["a.example.com"].ID}) {
		t.Fatalf("applies queued for %v, want only a.example.com (%d)", got, sites["a.example.com"].ID)
	}
	for _, want := range []string{"b.example.com: removed meanwhile", "c.example.com: being removed", "d.example.com: being removed", "e.example.com: on 10.0.0.3 now, not 10.0.0.1: left as it is", "a.example.com: 10.0.0.1 → 10.0.0.2"} {
		if !strings.Contains(job.Log, want) {
			t.Errorf("the log does not say %q:\n%s", want, job.Log)
		}
	}
}

// sites.move-ip moves each site under the site's lock, as its applies run:
// a site.apply under way beside it — rendering the old address it read —
// writes the site's server block before the move takes the block away, and
// the move reads the site after it. Without the lock the move would take
// the block away first and the apply would write it back on the address
// the host lost, where nginx's check fails on it.
func TestSitesMoveIPWaitsForAnApplyOfTheSite(t *testing.T) {
	f := newSiteFixture(t)
	ips := []string{"10.0.0.1"}
	f.s.SetHostIPs(func() []string { return ips })
	site := f.createSite(map[string]any{"domain": "one.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	ips = []string{"10.0.0.2"}
	conf := "/etc/nginx/monopanel/sites/one.example.com.conf"
	held, release := make(chan struct{}), make(chan struct{})
	var once, freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/config/apply" && strings.Contains(string(body), `"`+conf+`"`) {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() {
		free()
		f.agent.SetBefore(nil)
	})
	from := len(f.agent.Calls())
	var apply apitypes.SiteWithJob
	f.call(http.MethodPost, "/sites/one.example.com/apply", nil, http.StatusAccepted, &apply)
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("the site's apply did not get to its server block")
	}
	var res apitypes.SiteMoveIPResponse
	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"to": "10.0.0.2"}, http.StatusAccepted, &res)
	// The move runs beside the apply — a lock key and a worker of its own —
	// and waits at the site's lock, the server block as the apply left it.
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := f.db.GetJob(f.ctx, res.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == store.JobRunning {
			break
		}
		if j.Status != store.JobQueued {
			t.Fatalf("the move did not wait for the apply of the site under way: %s %s\n%s", j.Status, j.Error, j.Log)
		}
		if time.Now().After(deadline) {
			t.Fatal("move-ip did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if slices.Contains(f.agent.Removed(), conf) {
			t.Fatal("the move took the server block away while an apply of the site was writing it")
		}
	}
	free()
	if job := f.waitJob(apply.JobID); job.Status != store.JobDone {
		t.Fatalf("the apply: %s %s", job.Status, job.Error)
	}
	if job := f.waitJob(res.JobID); job.Status != store.JobDone {
		t.Fatalf("move-ip: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	written, removed := -1, -1
	for i, c := range f.agent.Calls()[from:] {
		if !strings.Contains(string(c.Body), `"`+conf+`"`) {
			continue
		}
		switch {
		case c.Path == "/v1/config/apply" && written < 0:
			written = i
		case c.Path == "/v1/paths/remove" && removed < 0:
			removed = i
		}
	}
	if written < 0 || removed < written {
		t.Fatalf("the apply wrote the server block at call %d, the move took it away at %d", written, removed)
	}
	if got, err := f.db.GetSite(f.ctx, site.ID); err != nil || got.IP != "10.0.0.2" {
		t.Fatalf("after the move: %v %v", got, err)
	}
	if !slices.Equal(f.appliesAfter(res.JobID), []int64{site.ID}) {
		t.Fatalf("applies queued: %v", f.appliesAfter(res.JobID))
	}
}

// The webmail on its port listens on its site's address, as the site's own
// server block does, and nginx's check fails on either once the host lost
// the address: sites.move-ip takes the webmail's block away with the site's
// and queues a mail.apply, which writes it again on the new address.
func TestSitesMoveIPTakesTheWebmailPortAlong(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	ips := []string{"10.0.0.1"}
	f.s.SetHostIPs(func() []string { return ips })
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	block := f.s.webmailConfPath()
	mailApply := func() {
		t.Helper()
		var ref apitypes.JobRef
		f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
		if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
			t.Fatalf("mail.apply: %s %s", job.Status, job.Error)
		}
	}
	// Written once more now that the site has its address: the install's
	// site.apply and mail.apply run side by side.
	mailApply()
	if conf, _ := f.agent.File(block); !strings.Contains(conf, "listen 10.0.0.1:2096 ssl;") {
		t.Fatalf("the webmail's block before the move:\n%s", conf)
	}
	ips = []string{"10.0.0.2"}
	from := len(f.agent.Calls())
	var res apitypes.SiteMoveIPResponse
	f.call(http.MethodPost, "/sites/move-ip", map[string]any{"to": "10.0.0.2"}, http.StatusAccepted, &res)
	if job := f.waitJob(res.JobID); job.Status != store.JobDone {
		t.Fatalf("move-ip: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	f.settle()
	together := false
	for _, c := range f.agent.Calls()[from:] {
		var req agent.RemovePathsRequest
		if c.Path == "/v1/paths/remove" && json.Unmarshal(c.Body, &req) == nil {
			together = together || slices.Contains(req.Paths, block) && slices.Contains(req.Paths, "/etc/nginx/monopanel/sites/webmail.example.com.conf")
		}
	}
	if !together {
		t.Error("the webmail's block did not go with its site's before nginx's check of the move")
	}
	conf, ok := f.agent.File(block)
	if !ok || !strings.Contains(conf, "listen 10.0.0.2:2096 ssl;") || strings.Contains(conf, "listen 10.0.0.1:") {
		t.Fatalf("the webmail's block after the move:\n%s", conf)
	}
}

// mp site set writes what it changes and nothing else of what it read: the
// address sites.move-ip gave the site between the request's read and its
// write stays — the apply after it renders the site there — and so does a
// removal asked for meanwhile, which refuses the change.
func TestSiteChangeKeepsWhatChangedMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1", "10.0.0.2"} })
	site := f.createSite(map[string]any{"domain": "one.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "ip": "10.0.0.1"})
	// The request checks the IPv6 address it sets against the host's: what
	// happens beside it lands then, after the request read the site.
	var mu sync.Mutex
	var meanwhile func()
	f.s.SetHostIPv6s(func() []string {
		mu.Lock()
		fn := meanwhile
		meanwhile = nil
		mu.Unlock()
		if fn != nil {
			fn()
		}
		return []string{"2001:db8::1"}
	})
	beside := func(fn func()) {
		mu.Lock()
		meanwhile = fn
		mu.Unlock()
	}
	change := func(body map[string]any) {
		t.Helper()
		var out apitypes.SiteWithJob
		f.call(http.MethodPatch, "/sites/one.example.com", body, http.StatusAccepted, &out)
		if job := f.waitJob(out.JobID); job.Status != store.JobDone {
			t.Fatalf("the apply after the change: %s %s", job.Status, job.Error)
		}
	}

	beside(func() {
		if moved, err := f.db.MoveSiteIP(f.ctx, site.ID, "10.0.0.1", "10.0.0.2"); err != nil || !moved {
			t.Errorf("move-ip: %v %v", moved, err)
		}
	})
	change(map[string]any{"ipv6": "2001:db8::1", "client_max_body": "128m"})
	got, err := f.db.GetSite(f.ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != "10.0.0.2" || got.IPv6 != "2001:db8::1" || got.ClientMaxBody != "128m" {
		t.Fatalf("after the change: ip %s, ipv6 %s, client_max_body %s", got.IP, got.IPv6, got.ClientMaxBody)
	}
	if conf, _ := f.agent.File("/etc/nginx/monopanel/sites/one.example.com.conf"); !strings.Contains(conf, "10.0.0.2:80") || strings.Contains(conf, "10.0.0.1:") {
		t.Fatalf("the apply after the change renders another address:\n%s", conf)
	}

	// A change that also takes an error back to pending, while the site's
	// removal is asked for: refused at its write, the site left as its mark
	// left it.
	if err := f.db.SetSiteStatus(f.ctx, site.ID, store.SiteError, "boom"); err != nil {
		t.Fatal(err)
	}
	beside(func() {
		if err := f.db.MarkSiteDeleting(f.ctx, site.ID); err != nil {
			t.Error(err)
		}
	})
	f.call(http.MethodPatch, "/sites/one.example.com", map[string]any{"ipv6": "2001:db8::1", "client_max_body": "256m"}, http.StatusConflict, nil)
	if got, _ := f.db.GetSite(f.ctx, site.ID); got.Status != store.SiteDeleting || got.ClientMaxBody != "128m" {
		t.Fatalf("a change beside the removal: status %s, client_max_body %s", got.Status, got.ClientMaxBody)
	}
}

// A move is done once its sites are up on the new address: it waits for
// their applies and fails naming those whose apply failed — they have no
// server block until one succeeds (new.qaj.kz, 26.11.2026).
func TestSitesMoveIPWaitsForTheAppliesAndNamesTheFailed(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "one.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	ok, err := f.s.enqueueSiteApply(f.ctx, site, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// An apply refused by nginx -t, as the one of a site whose address went.
	f.s.jobs.Register("test.refused", func(context.Context, *jobs.Context) error {
		return errors.New("nginx: [emerg] bind() to 10.0.0.1:80 failed (99)")
	})
	bad, err := f.s.jobs.Enqueue(f.ctx, "test.refused", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	err = f.s.waitSiteApplies(f.ctx, logf, map[int64]string{ok: "one.example.com", bad.ID: "two.example.com"}, "10.0.0.2")
	if err == nil || !strings.Contains(err.Error(), "1 site(s) are not up on 10.0.0.2") || !strings.Contains(err.Error(), fmt.Sprintf("two.example.com (job #%d", bad.ID)) || strings.Contains(err.Error(), "one.example.com") {
		t.Fatalf("err %v", err)
	}
	if len(lines) != 1 || lines[0] != "one.example.com: up on 10.0.0.2" {
		t.Errorf("log %q", lines)
	}
	// The doctor counts a failed site without its server block as missing.
	if err := f.db.SetSiteStatusFrom(f.ctx, site.ID, store.SiteActive, store.SiteError, "nginx -t failed"); err != nil {
		t.Fatal(err)
	}
	// (The fake's Stat finds no file the apply wrote: the block is missing.)
	if got := f.doctorCheck("drift"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "/etc/nginx/monopanel/sites/one.example.com.conf") {
		t.Errorf("doctor: %q", got)
	}
}
