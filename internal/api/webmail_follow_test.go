package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/store"
)

// webmailRuns counts the mail.webmail jobs so far.
func webmailRuns(f *siteFixture) int {
	f.t.Helper()
	list, err := f.db.ListJobs(f.ctx, 500, "")
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, j := range list {
		if j.Type == "mail.webmail" {
			n++
		}
	}
	return n
}

// The webmail follows the certificate of the mail server's name: an ACME
// certificate that expires (or goes) leaves the name with the self-signed
// one, and config.inc.php, which checked the name's certificate, refused
// every login; one issued for a name that had the self-signed one leaves
// the webmail on localhost unchecked. A real change of kind re-runs the
// webmail job, once; nothing else does.
func TestWebmailFollowsTheMailCertificate(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	const file = "/var/www/alex/data/www/webmail.example.com/config/config.inc.php"
	conf := installedWebmail(t, f, home, map[string]any{"port": 2096})
	if !strings.Contains(conf, "'tls://mail.example.com:143'") {
		t.Fatalf("the webmail does not sign in to mail.example.com:\n%s", conf)
	}
	if c := f.s.loadMailConfig(f.ctx); c.WebmailServer != "mail.example.com" {
		t.Fatalf("recorded server %q", c.WebmailServer)
	}
	cert, err := f.db.GetCertificateByName(f.ctx, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	setUntil := func(until time.Time) {
		t.Helper()
		cert.NotAfter = &until
		if err := f.db.UpsertCertificate(f.ctx, cert); err != nil {
			t.Fatal(err)
		}
	}
	runs := webmailRuns(f)

	// Nothing changed: neither mp mail apply nor the hourly look runs it.
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, nil)
	f.settle()
	f.s.webmailFollowsCertificate(f.ctx)
	f.settle()
	if n := webmailRuns(f); n != runs {
		t.Fatalf("mail.webmail ran %d more times without a change of the certificate", n-runs)
	}

	// The certificate expires (renewal failed): the hourly look, twice in a
	// row, queues one run.
	setUntil(time.Now().Add(-time.Hour))
	f.s.webmailFollowsCertificate(f.ctx)
	f.s.webmailFollowsCertificate(f.ctx)
	f.settle()
	if n := webmailRuns(f); n != runs+1 {
		t.Fatalf("after the expiry mail.webmail ran %d times, want once", n-runs)
	}
	again, _ := f.agent.File(file)
	if !strings.Contains(again, "'tls://localhost:143'") || strings.Contains(again, "mail.example.com:143") {
		t.Fatalf("config.inc.php after the expiry:\n%s", again)
	}
	if c := f.s.loadMailConfig(f.ctx); c.WebmailServer != "localhost" {
		t.Fatalf("recorded server %q after the expiry", c.WebmailServer)
	}
	f.s.webmailFollowsCertificate(f.ctx)
	f.settle()
	if n := webmailRuns(f); n != runs+1 {
		t.Fatalf("the next hourly look ran mail.webmail again (%d)", n-runs)
	}

	// A certificate issued again for the name: cert.issue applies the mail
	// configuration, and the webmail checks the name again.
	setUntil(time.Now().Add(60 * 24 * time.Hour))
	if err := f.s.applyMailConfig(f.ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if n := webmailRuns(f); n != runs+2 {
		t.Fatalf("after the new certificate mail.webmail ran %d times, want twice", n-runs)
	}
	back, _ := f.agent.File(file)
	if !strings.Contains(back, "'tls://mail.example.com:143'") {
		t.Fatalf("config.inc.php after the new certificate:\n%s", back)
	}

	// A webmail an older panel set up has no record: it is taken as it is,
	// and no job runs for it.
	if _, err := f.s.changeMailConfig(f.ctx, func(c *mailConfig) { c.WebmailServer = "" }); err != nil {
		t.Fatal(err)
	}
	f.s.webmailFollowsCertificate(f.ctx)
	f.settle()
	if c := f.s.loadMailConfig(f.ctx); c.WebmailServer != "mail.example.com" || webmailRuns(f) != runs+2 {
		t.Fatalf("an older webmail: recorded %q, %d runs", c.WebmailServer, webmailRuns(f)-runs)
	}
}

// The webmail's port answers only once its site's pool is there: the block
// on the port passes PHP to the pool's socket, which the site's apply makes.
// mail.apply used to be queued beside the site's apply and wrote the block
// first (404/502 on the port for minutes on ubuntu2604); now the site's
// apply queues it when it is through.
func TestWebmailPortWaitsForItsSite(t *testing.T) {
	f, _, _ := newWebmailFixture(t)
	release := make(chan struct{})
	var once sync.Once
	held := make(chan struct{})
	f.agent.SetBefore(func(path string, body []byte) {
		// The site's apply writing the webmail site's pool.
		if path == "/v1/config/apply" && strings.Contains(string(body), "webmail.example.com.conf") && strings.Contains(string(body), "listen.group") {
			once.Do(func() { close(held) })
			<-release
		}
	})
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/mail/webmail", map[string]any{"domain": "webmail.example.com", "user": "alex", "port": 2096}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		close(release)
		t.Fatalf("webmail install: %s %s", job.Status, job.Error)
	}
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		close(release)
		t.Fatal("the site's apply did not write its pool")
	}
	list, err := f.db.ListJobs(f.ctx, 500, "")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	var apply *store.Job
	for _, j := range list {
		var p sitePayload
		if j.Type == "site.apply" && j.ID > ref.JobID && json.Unmarshal(j.Payload, &p) == nil {
			apply = j
		}
		if j.Type == "mail.apply" && j.ID > ref.JobID {
			close(release)
			t.Fatalf("mail.apply #%d is queued while the site's apply is still at its pool: the port block would come before the socket", j.ID)
		}
	}
	close(release)
	f.agent.SetBefore(nil)
	if apply == nil {
		t.Fatal("no apply of the webmail's site")
	}
	f.settle()
	if _, ok := f.agent.File(f.s.webmailConfPath()); !ok {
		t.Fatal("no server block on the webmail's port after the site's apply")
	}
	list, _ = f.db.ListJobs(f.ctx, 500, "")
	after := false
	for _, j := range list {
		after = after || (j.Type == "mail.apply" && j.ID > apply.ID && j.Status == store.JobDone)
	}
	if !after {
		t.Fatal("no mail.apply after the site's apply")
	}
}
