package api

import (
	"strings"
	"testing"
)

// Every pool points mail() at /usr/sbin/sendmail: without an MTA mail()
// returns false and a contact form loses its letters with nothing in a log.
// The doctor follows the link chain the alternatives make, a relative hop
// counted from the link's directory, and a dangling link is no sendmail.
func TestDoctorSendmail(t *testing.T) {
	f := newSiteFixture(t)
	if got := f.doctorCheck("sendmail"); got != "" {
		t.Errorf("no PHP sites, no line: %q", got)
	}
	f.createSite(map[string]any{"domain": "proxy.example.com", "user": "alex", "mode": "proxy", "backend": "http://127.0.0.1:3000", "ssl": "none"})
	if got := f.doctorCheck("sendmail"); got != "" {
		t.Errorf("a proxy site does not call mail(): %q", got)
	}
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	want := "warn: PHP sites send mail through /usr/sbin/sendmail, which is not there: mail() fails quietly; mp mail install sets up the panel's mail (or install an MTA such as postfix or msmtp-mta)"
	if got := f.doctorCheck("sendmail"); got != want {
		t.Errorf("no sendmail: %q", got)
	}

	// EL: sendmail → /etc/alternatives/mta → /usr/sbin/sendmail.postfix.
	f.agent.StatLinks["/usr/sbin/sendmail"] = "/etc/alternatives/mta"
	f.agent.StatLinks["/etc/alternatives/mta"] = "/usr/sbin/sendmail.postfix"
	if got := f.doctorCheck("sendmail"); got != want {
		t.Errorf("a dangling chain: %q", got)
	}
	f.agent.Stat["/usr/sbin/sendmail.postfix"] = false
	if got := f.doctorCheck("sendmail"); got != "ok: /usr/sbin/sendmail → /etc/alternatives/mta → /usr/sbin/sendmail.postfix takes mail() of the PHP sites" {
		t.Errorf("the alternatives: %q", got)
	}

	// Debian's postfix: a relative link, from /usr/sbin.
	delete(f.agent.StatLinks, "/etc/alternatives/mta")
	f.agent.StatLinks["/usr/sbin/sendmail"] = "../lib/sendmail"
	if got := f.doctorCheck("sendmail"); got != want {
		t.Errorf("../lib/sendmail missing: %q", got)
	}
	f.agent.Stat["/usr/lib/sendmail"] = false
	if got := f.doctorCheck("sendmail"); !strings.HasPrefix(got, "ok: /usr/sbin/sendmail → /usr/lib/sendmail ") {
		t.Errorf("a relative link: %q", got)
	}

	// A loop ends, and is no sendmail; nor is a directory.
	f.agent.StatLinks["/usr/sbin/sendmail"] = "/usr/sbin/sendmail"
	if got := f.doctorCheck("sendmail"); got != want {
		t.Errorf("a loop: %q", got)
	}
	delete(f.agent.StatLinks, "/usr/sbin/sendmail")
	f.agent.Stat["/usr/sbin/sendmail/"] = true
	if got := f.doctorCheck("sendmail"); got != want {
		t.Errorf("a directory: %q", got)
	}
	delete(f.agent.Stat, "/usr/sbin/sendmail/")
	f.agent.Stat["/usr/sbin/sendmail"] = false
	if got := f.doctorCheck("sendmail"); got != "ok: /usr/sbin/sendmail takes mail() of the PHP sites" {
		t.Errorf("a plain file: %q", got)
	}
}
