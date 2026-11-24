package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// mailHostFixture is newMailFixtureOn without the install: a valid
// certificate for mail.example.com, no DNS, every port answering as the
// panel's own postfix unless banner25 says otherwise.
func mailHostFixture(t *testing.T, banner25 string) *siteFixture {
	t.Helper()
	f := newSiteFixture(t)
	f.s.SetLookup(func(context.Context, string) ([]string, error) { return nil, nil })
	f.s.SetPortProbe(func(port int, _ bool) (bool, string) {
		if port == 25 && banner25 != "" {
			return true, banner25
		}
		return true, "220 mail.example.com ESMTP"
	})
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	if _, err := EnsureSelfSigned(cert, key, []string{"mail.example.com"}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(60 * 24 * time.Hour)
	if err := f.db.UpsertCertificate(f.ctx, &store.Certificate{
		Name: "mail.example.com", Names: []string{"mail.example.com"}, Kind: store.CertKindACME,
		Status: store.CertValid, NotAfter: &until, CertPath: cert, KeyPath: key,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *siteFixture) installMail(payload map[string]any) *store.Job {
	f.t.Helper()
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/install", payload, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

// mailInstallMeanwhile has a change of the mail settings land while
// mail.install runs, between its read of the settings and its save (at its
// vmail step).
func (f *siteFixture) mailInstallMeanwhile(put map[string]any) *atomic.Bool {
	var fired atomic.Bool
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/user/ensure" && strings.Contains(string(body), `"login":"vmail"`) && fired.CompareAndSwap(false, true) {
			if status, raw, err := f.send(http.MethodPut, "/mail/settings", put); err != nil || status != http.StatusOK {
				f.t.Errorf("PUT /mail/settings: %d %s %v", status, raw, err)
			}
		}
	})
	f.t.Cleanup(func() { f.agent.SetBefore(nil) })
	return &fired
}

// With nothing changing meanwhile, mail.install saves what it found and
// decided — the name, POP3 when asked, port 25 given up when another daemon
// holds it, the versions, the map type, the vmail ids — and every other
// setting as it was.
func TestMailInstallSavesWhatItDecides(t *testing.T) {
	stored := mailConfig{
		Installed: true, Hostname: "mail.example.com", MaxSizeMB: 77, POP3: false, DKIM: false, Port25: false,
		RBL: []string{"zen.spamhaus.org"}, VmailUID: 1, VmailGID: 2, WebmailPort: 2096,
		PostfixVersion: "0.9", DovecotVersion: "0.9", MapType: "lmdb",
	}
	withWebmail := mailConfig{
		Installed: true, Hostname: "mail.example.com", MaxSizeMB: 50, POP3: true, DKIM: true, Port25: true,
		VmailUID: 1500, VmailGID: 1500, Webmail: "webmail.example.com", WebmailVersion: "1.7.3",
		PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash",
	}
	for _, tc := range []struct {
		name        string
		stored      *mailConfig
		payload     map[string]any
		banner25    string
		postconf    string
		webmailSite bool
		want        mailConfig
	}{
		{
			name: "fresh, defaults", payload: map[string]any{"hostname": "mail.example.com"},
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 50, POP3: true, DKIM: true, Port25: true,
				VmailUID: 1500, VmailGID: 1500, PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
		{
			name: "fresh, port 25 taken, no POP3, lmdb", payload: map[string]any{"hostname": "mail.example.com", "pop3": false},
			banner25: "220 other.example.org MailTester", postconf: "lmdb\nregexp\n",
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 50, POP3: false, DKIM: true, Port25: false,
				VmailUID: 1500, VmailGID: 1500, PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "lmdb"},
		},
		{
			name: "fresh, the name kept from the settings", stored: &mailConfig{Hostname: "mail.example.com", MaxSizeMB: 20, POP3: true, DKIM: true, Port25: true},
			payload: map[string]any{},
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 20, POP3: true, DKIM: true, Port25: true,
				VmailUID: 1500, VmailGID: 1500, PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
		{
			name: "a repair over settings of its own", stored: &stored, payload: map[string]any{},
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 77, POP3: false, DKIM: false, Port25: false,
				RBL: []string{"zen.spamhaus.org"}, VmailUID: 1500, VmailGID: 1500, WebmailPort: 2096,
				PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
		{
			name: "a repair asked for with POP3 on", stored: &stored, payload: map[string]any{"hostname": "mail.example.com", "pop3": true},
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 77, POP3: true, DKIM: false, Port25: false,
				RBL: []string{"zen.spamhaus.org"}, VmailUID: 1500, VmailGID: 1500, WebmailPort: 2096,
				PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
		{
			name: "a repair with a webmail", stored: &withWebmail, webmailSite: true, payload: map[string]any{"hostname": "mail.example.com"},
			// The webmail of an older panel gets the record of the server
			// its configuration names (webmailFollowsCertificate).
			want: func() mailConfig { c := withWebmail; c.WebmailServer = "mail.example.com"; return c }(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mailHostFixture(t, tc.banner25)
			if tc.postconf != "" {
				f.agent.ToolOutput["postconf"] = tc.postconf
			}
			if tc.webmailSite {
				f.createSite(map[string]any{"domain": "webmail.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
				f.settle()
			}
			if tc.stored != nil {
				if err := f.s.saveMailConfig(f.ctx, *tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			if job := f.installMail(tc.payload); job.Status != store.JobDone {
				t.Fatalf("mail.install: %s %s\n%s", job.Status, job.Error, job.Log)
			}
			f.settle()
			if got := f.s.loadMailConfig(f.ctx); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("saved settings:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// What mp mail settings changes while mail.install runs stays — every
// setting the install does not decide — and what the install decides is
// its own: a name or POP3 the request names, else the ones set meanwhile.
func TestMailInstallKeepsEveryChangeMadeMeanwhile(t *testing.T) {
	put := map[string]any{"hostname": "other.example.com", "max_size_mb": 33, "pop3": false, "dkim": false, "port25": false, "webmail_port": 2097, "rbl": []string{"b.example.org"}}
	for _, tc := range []struct {
		name    string
		stored  *mailConfig
		payload map[string]any
		want    mailConfig
	}{
		{
			name: "the request names the host and POP3", payload: map[string]any{"hostname": "mail.example.com", "pop3": true},
			want: mailConfig{Installed: true, Hostname: "mail.example.com", MaxSizeMB: 33, POP3: true, DKIM: false, Port25: false,
				RBL: []string{"b.example.org"}, VmailUID: 1500, VmailGID: 1500, WebmailPort: 2097,
				PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
		{
			name: "the request names neither", stored: &mailConfig{Hostname: "mail.example.com", MaxSizeMB: 50, POP3: true, DKIM: true, Port25: true},
			payload: map[string]any{},
			want: mailConfig{Installed: true, Hostname: "other.example.com", MaxSizeMB: 33, POP3: false, DKIM: false, Port25: false,
				RBL: []string{"b.example.org"}, VmailUID: 1500, VmailGID: 1500, WebmailPort: 2097,
				PostfixVersion: "1.0-test", DovecotVersion: "1.0-test", MapType: "hash"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mailHostFixture(t, "")
			if tc.stored != nil {
				if err := f.s.saveMailConfig(f.ctx, *tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			fired := f.mailInstallMeanwhile(put)
			if job := f.installMail(tc.payload); job.Status != store.JobDone {
				t.Fatalf("mail.install: %s %s\n%s", job.Status, job.Error, job.Log)
			}
			if !fired.Load() {
				t.Fatal("the change did not land during the install")
			}
			f.settle()
			if got := f.s.loadMailConfig(f.ctx); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("saved settings:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// A webmail site that cannot be read — an error other than not found — is
// not forgotten: mail.apply fails on it and the settings keep the webmail.
// The mail policy reads every site before the render (writeMailPolicy), so
// its read may be the one that fails: the render stops before it writes
// anything, and the webmail stays all the same.
func TestWebmailSiteLookupErrorKeepsTheWebmail(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home)
	if _, err := f.db.SQL().ExecContext(f.ctx, `UPDATE sites SET http2='not a number' WHERE domain='webmail.example.com'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.db.SQL().ExecContext(f.ctx, `UPDATE sites SET http2=1 WHERE domain='webmail.example.com'`) //nolint:errcheck // the row back for the fixture's cleanups
	})
	if _, err := f.db.GetSiteByDomain(f.ctx, "webmail.example.com"); err == nil || err == store.ErrNotFound {
		t.Fatalf("the spoilt row reads as %v", err)
	}
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobFailed || !(strings.Contains(job.Error, "webmail site webmail.example.com") || strings.Contains(job.Error, "mail policy")) {
		t.Fatalf("mail.apply: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if c := f.s.loadMailConfig(f.ctx); c.Webmail != "webmail.example.com" {
		t.Fatalf("the webmail was forgotten on a read error: %q", c.Webmail)
	}
}

// renderMeanwhile has mp mail settings asked for while another mail render
// is under way — inside its first agent call, the query of the installed
// dovecot — and waits until the change is saved and, for half a second, for
// its answer: a render of its own that ran beside the one under way would
// answer by then. It hands over the answer once there is one.
func (f *siteFixture) renderMeanwhile(put map[string]any, saved func(mailConfig) bool) (fired *atomic.Bool, answered chan sent) {
	fired, answered = &atomic.Bool{}, make(chan sent, 1)
	f.agent.SetBefore(func(p string, body []byte) {
		if p != "/v1/pkg" || !strings.Contains(string(body), `"action":"query","packages":["dovecot-core"]`) || !fired.CompareAndSwap(false, true) {
			return
		}
		go f.sendBeside(answered, http.MethodPut, "/mail/settings", put)
		for deadline := time.Now().Add(20 * time.Second); !saved(f.s.loadMailConfig(f.ctx)); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				f.t.Error("the change was not saved")
				return
			}
		}
		select {
		case a := <-answered:
			answered <- a
		case <-time.After(500 * time.Millisecond):
		}
	})
	f.t.Cleanup(func() { f.agent.SetBefore(nil) })
	return fired, answered
}

// messageSizeLine is main.cf's message_size_limit line.
func (f *siteFixture) messageSizeLine() string {
	main, _ := f.agent.File("/etc/postfix/main.cf")
	for _, l := range strings.Split(main, "\n") {
		if strings.HasPrefix(l, "message_size_limit") {
			return l
		}
	}
	return ""
}

// mp mail settings that lands while a mail.apply renders — after the run
// read the settings, before it wrote — ends rendered: main.cf has the new
// message size, and the webmail's block is off the port switched off. The
// renders take turns, each from the settings as they are when its turn
// comes.
func TestMailChangeDuringAMailRenderIsRendered(t *testing.T) {
	f, _, home := newWebmailFixture(t)
	installedWebmail(t, f, home, map[string]any{"port": 2096})
	block := f.s.webmailConfPath()
	if conf, ok := f.agent.File(block); !ok || !strings.Contains(conf, ":2096 ssl") {
		t.Fatalf("the webmail is not on port 2096 before the test:\n%s", conf)
	}
	fired, answered := f.renderMeanwhile(map[string]any{"max_size_mb": 64, "webmail_port": 0}, func(c mailConfig) bool { return c.MaxSizeMB == 64 && c.WebmailPort == 0 })
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("mail.apply: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if !fired.Load() {
		t.Fatal("the change did not land during the mail.apply")
	}
	if a := answerOf(t, answered); a.err != nil || a.status != http.StatusOK {
		t.Fatalf("PUT /mail/settings: %d %s %v", a.status, a.body, a.err)
	}
	if line, want := f.messageSizeLine(), fmt.Sprintf("message_size_limit = %d", 64*1024*1024); line != want {
		t.Errorf("main.cf has %q, the settings say 64 MB (%s)", line, want)
	}
	if conf, ok := f.agent.File(block); ok {
		t.Errorf("the webmail's block is on its port after the port was switched off:\n%s", conf)
	}
}

// mp mail settings --max-size that lands while mp mail install (a repair
// run) renders, after its save, ends in main.cf as well.
func TestMailSettingsDuringAnInstallRenderAreRendered(t *testing.T) {
	f := newMailFixture(t)
	f.settle()
	fired, answered := f.renderMeanwhile(map[string]any{"max_size_mb": 77}, func(c mailConfig) bool { return c.MaxSizeMB == 77 })
	if job := f.installMail(map[string]any{"hostname": "mail.example.com"}); job.Status != store.JobDone {
		t.Fatalf("mail.install: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if !fired.Load() {
		t.Fatal("the change did not land during the install's render")
	}
	if a := answerOf(t, answered); a.err != nil || a.status != http.StatusOK {
		t.Fatalf("PUT /mail/settings: %d %s %v", a.status, a.body, a.err)
	}
	if line := f.messageSizeLine(); line != "message_size_limit = 80740352" {
		t.Errorf("main.cf has %q after the install, the settings say 77 MB (message_size_limit = 80740352)", line)
	}
}
