package api

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/buildinfo"
	"monopanel/internal/mailpolicy"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// readPolicy is the mail policy the fake agent holds, after the checks the
// agent makes of it (content, owner, group, mode).
func readPolicy(f *siteFixture) *mailpolicy.Policy {
	f.t.Helper()
	spec, ok := f.agent.FileSpec(mailpolicy.PolicyFile)
	if !ok {
		f.t.Fatalf("no mail policy; files: %v", f.agent.Files())
	}
	if err := confcheck.Check(spec.Path, []byte(spec.Content)); err != nil {
		f.t.Fatalf("the agent refuses the policy: %v\n%s", err, spec.Content)
	}
	if err := confcheck.CheckOwnership(spec.Path, spec.Owner, spec.Group, spec.Mode); err != nil {
		f.t.Fatalf("the agent refuses the policy's owner: %v", err)
	}
	p, err := mailpolicy.Parse([]byte(spec.Content))
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func wantDomains(f *siteFixture, login string, want ...string) {
	f.t.Helper()
	p := readPolicy(f)
	a, ok := p.Accounts[login]
	if !ok {
		f.t.Fatalf("no account %s in the policy: %+v", login, p.Accounts)
	}
	slices.Sort(want)
	if !slices.Equal(a.Domains, want) {
		f.t.Fatalf("%s owns %v, want %v", login, a.Domains, want)
	}
}

// The policy follows every change of accounts, sites, aliases, mail domains,
// mailboxes and the host name.
func TestMailPolicyFollowsTheDatabase(t *testing.T) {
	f := newMailFixture(t)
	p := readPolicy(f)
	if p.Hostname != "mail.example.com" || p.Delivery != vmailUser || len(p.Mailboxes) != 0 || p.AccountHomes != f.s.cfg.WWWRoot {
		t.Fatalf("after the install: %+v", p)
	}
	wantDomains(f, "alex")
	// A panel user without a unix user sends no mail of this host's: a unix
	// login of its name (Debian's cloud images have an "admin") is no
	// account (review r3 F10).
	if _, ok := p.Accounts["admin"]; ok {
		t.Fatalf("a panel user without a unix user is an account of the policy: %+v", p.Accounts)
	}

	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "Example.com", "user": "alex"}, http.StatusCreated, nil)
	wantDomains(f, "alex", "example.com")
	f.call(http.MethodPost, "/mail/mailboxes", map[string]any{"address": "ivan@example.com"}, http.StatusCreated, nil)
	if got := readPolicy(f).Mailboxes; got["ivan@example.com"] != "alex" || len(got) != 1 {
		t.Fatalf("mailboxes %v", got)
	}

	f.createSite(map[string]any{"domain": "shop.test", "aliases": []string{"www.shop.test"}, "user": "alex", "php_version": "8.4", "ssl": "none"})
	wantDomains(f, "alex", "example.com", "shop.test", "www.shop.test")
	var upd struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/shop.test", map[string]any{"aliases": []string{"www.shop.test", "shop.example.org"}}, http.StatusAccepted, &upd)
	f.waitJob(upd.JobID)
	wantDomains(f, "alex", "example.com", "shop.example.org", "shop.test", "www.shop.test")
	f.call(http.MethodDelete, "/sites/shop.test", nil, http.StatusAccepted, &upd)
	f.waitJob(upd.JobID)
	wantDomains(f, "alex", "example.com")

	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "bob", "password": "bob-password-1"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	wantDomains(f, "bob")
	f.call(http.MethodDelete, "/users/bob", nil, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	if _, ok := readPolicy(f).Accounts["bob"]; ok {
		t.Fatal("a removed account stays in the policy")
	}

	f.call(http.MethodPut, "/mail/settings", map[string]any{"hostname": "mx.example.net"}, http.StatusOK, nil)
	if h := readPolicy(f).Hostname; h != "mx.example.net" {
		t.Fatalf("host name %q", h)
	}
	f.call(http.MethodDelete, "/mail/mailboxes/ivan@example.com", nil, http.StatusNoContent, nil)
	if got := readPolicy(f).Mailboxes; len(got) != 0 {
		t.Fatalf("mailboxes after the removal: %v", got)
	}
	f.call(http.MethodDelete, "/mail/domains/example.com", nil, http.StatusNoContent, nil)
	wantDomains(f, "alex")
}

// postfix asks the policy before opendkim, waits while it is down, and hands
// it local submissions through a cleanup of their own; opendkim's
// TrustedHosts names no mail domain.
func TestMailPolicyPostfixConfiguration(t *testing.T) {
	f := newMailFixtureOn(t, nil, func(f *siteFixture) {
		f.s.SetHostIPs(func() []string { return []string{"192.0.2.10", "198.51.100.7"} })
		f.s.SetHostIPv6s(func() []string { return []string{"2001:db8::10"} })
	})
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "example.com", "user": "alex"}, http.StatusCreated, nil)
	main, _ := f.agent.File("/etc/postfix/main.cf")
	milters := "{ unix:/var/spool/postfix/private/monopanel-mailpolicy, default_action=tempfail }, local:/run/opendkim/opendkim.sock"
	for _, want := range []string{
		"\nsmtpd_milters = " + milters + "\n",
		"\nnon_smtpd_milters = " + milters + "\n",
		"\nmilter_default_action = accept\n",
		"\nmilter_protocol = 6\n",
		"\nmilter_connect_macros = j {daemon_name} {daemon_addr} {daemon_port} v _\n",
		// A client that pipelines its dialog before the greeting is refused
		// its DATA here, whatever the policy said of its MAIL FROM.
		"\nsmtpd_data_restrictions = reject_unauth_pipelining\n",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.cf lacks %q", want)
		}
	}
	master, _ := f.agent.File("/etc/postfix/master.cf")
	for _, want := range []string{
		"\npickup       unix  n       -       n      60     1       pickup\n  -o cleanup_service_name=pickup-cleanup\n",
		// pickup's own cleanup: its header table acts on the message's own
		// header, not on an attached message's or a MIME part's (review r2
		// F6: the PREPEND doubled a Received line inside an attachment).
		"\npickup-cleanup unix n       -       n      -      0       cleanup\n  -o milter_macro_daemon_name=MONOPANEL-PICKUP\n  -o header_checks=regexp:/etc/postfix/monopanel/pickup_header_checks\n  -o mime_header_checks=\n  -o nested_header_checks=\n",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master.cf lacks %q:\n%s", want, master)
		}
	}
	if checks, _ := f.agent.File(mailpolicy.HeaderChecksFile); checks != mailpolicy.HeaderChecksContent {
		t.Errorf("pickup's header table:\n%s", checks)
	}
	if err := confcheck.Check(mailpolicy.HeaderChecksFile, []byte(mailpolicy.HeaderChecksContent)); err != nil {
		t.Error(err)
	}
	// opendkim trusts the loopback and the host's own addresses — the
	// clients the policy checks as local ones —, and no name: a name is
	// matched against the client's reverse DNS, which made a host that the
	// policy took for inbound one opendkim signed for (review r3 F4, r1
	// I1); the host's second address is trusted as well (r2 F8).
	trusted, _ := f.agent.File(dkimDir + "/TrustedHosts")
	if !strings.HasSuffix(trusted, "\n127.0.0.1\n::1\n192.0.2.10\n198.51.100.7\n2001:db8::10\n") || strings.Contains(trusted, "example.com") || strings.Contains(trusted, "localhost") {
		t.Errorf("TrustedHosts is not the loopback and the host's addresses:\n%s", trusted)
	}
	dove, _ := f.agent.File(dovecotConf)
	if !strings.Contains(dove, " sieve_vacation_send_from_recipient = yes\n") {
		t.Errorf("dovecot.conf: vacation replies would leave with the null sender:\n%s", dove)
	}
	// A redirect leaves with the forwarding mailbox as its envelope sender:
	// a refusal bounces to it, with the message (review r2 F3, the lead's
	// decision).
	if !strings.Contains(dove, " sieve_redirect_envelope_from = recipient\n") {
		t.Errorf("dovecot.conf: redirects would leave with the first sender's envelope:\n%s", dove)
	}

	// DKIM off: the policy alone.
	f.call(http.MethodPut, "/mail/settings", map[string]any{"dkim": false}, http.StatusOK, nil)
	main, _ = f.agent.File("/etc/postfix/main.cf")
	if !strings.Contains(main, "\nsmtpd_milters = { unix:/var/spool/postfix/private/monopanel-mailpolicy, default_action=tempfail }\n") {
		t.Errorf("without DKIM:\n%s", main)
	}
}

// dovecot 2.4 has the same settings at the top level.
func TestMailPolicyDovecot24(t *testing.T) {
	f := newMailFixtureWith(t, map[string]string{"dovecot-core": "1:2.4.1+dfsg1-6", "dovecot": "1:2.4.1+dfsg1-6"})
	dove, _ := f.agent.File(dovecotConf)
	for _, want := range []string{"\nsieve_vacation_send_from_recipient = yes\n", "\nsieve_redirect_envelope_from = recipient\n"} {
		if !strings.Contains(dove, want) {
			t.Errorf("dovecot.conf of 2.4 lacks %q", want)
		}
	}
}

// On EL the policy listens on a loopback port of milter_port_t, held by its
// socket unit, and the guard — nftables, which the install puts in — keeps
// every account from that port and opendkim's before postfix is told to
// wait for the policy (review r1 F1: an account had opendkim sign any
// domain over 127.0.0.1:8891).
func TestMailPolicyOnEL(t *testing.T) {
	old := fixtureOSRelease
	fixtureOSRelease = "ID=rocky\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"
	defer func() { fixtureOSRelease = old }()
	f := newMailFixture(t)
	main, _ := f.agent.File("/etc/postfix/main.cf")
	if !strings.Contains(main, "\nsmtpd_milters = { inet:127.0.0.1:8890, default_action=tempfail }, inet:127.0.0.1:8891\n") {
		t.Errorf("main.cf on EL:\n%s", main)
	}
	for _, unit := range []string{mailpolicy.GuardUnit, mailpolicy.SocketUnitInet, mailpolicy.Unit} {
		if a := f.agent.UnitAction(unit); a != "start" || !slices.Contains(f.agent.ServiceActions(), unit+" enable") {
			t.Errorf("%s: %q, enabled %v", unit, a, slices.Contains(f.agent.ServiceActions(), unit+" enable"))
		}
	}
	if a := f.agent.UnitAction(mailpolicy.SocketUnitUnix); a != "" {
		t.Errorf("Debian's socket on EL: %q", a)
	}
	calls := f.agent.Calls()
	at := func(match func(path, body string) bool) int {
		for i, c := range calls {
			if match(c.Path, string(c.Body)) {
				return i
			}
		}
		return -1
	}
	nft := at(func(p, b string) bool {
		return p == "/v1/pkg" && strings.Contains(b, "nftables")
	})
	guard := at(func(p, b string) bool {
		return p == "/v1/service" && strings.Contains(b, mailpolicy.GuardUnit) && strings.Contains(b, `"start"`)
	})
	mainCf := at(func(p, b string) bool {
		return p == "/v1/config/apply" && strings.Contains(b, "default_action=tempfail")
	})
	if nft < 0 || guard < 0 || mainCf < 0 || !(nft < guard && guard < mainCf) {
		t.Errorf("order: nftables %d, guard %d, main.cf %d", nft, guard, mainCf)
	}
	if got := readPolicy(f); got.AccountHomes == "" {
		t.Errorf("the policy names no accounts' homes: %+v", got)
	}
}

// The install makes the policy's account, writes its file and starts its
// socket and its unit before main.cf tells postfix to wait for it; postfix
// joins no group for it (the socket is postfix's own), and nothing of EL's
// guard runs on Debian.
func TestMailPolicyUnitStartsBeforePostfixWaitsForIt(t *testing.T) {
	f := newMailFixture(t)
	for _, unit := range []string{mailpolicy.SocketUnitUnix, mailpolicy.Unit} {
		if a := f.agent.UnitAction(unit); a != "start" || !slices.Contains(f.agent.ServiceActions(), unit+" enable") {
			t.Fatalf("%s: %q, actions %v", unit, a, f.agent.ServiceActions())
		}
	}
	for _, unit := range []string{mailpolicy.SocketUnitInet, mailpolicy.GuardUnit} {
		if a := f.agent.UnitAction(unit); a != "" {
			t.Errorf("EL's %s on Debian: %q", unit, a)
		}
	}
	calls := f.agent.Calls()
	first := func(match func(c string) bool) int {
		for i, c := range calls {
			if match(c.Path + " " + string(c.Body)) {
				return i
			}
		}
		return -1
	}
	group := first(func(c string) bool {
		return strings.HasPrefix(c, "/v1/group/ensure") && strings.Contains(c, mailpolicy.User)
	})
	postfix := first(func(c string) bool {
		return strings.HasPrefix(c, "/v1/user/ensure") && strings.Contains(c, `"login":"postfix"`) && strings.Contains(c, mailpolicy.User)
	})
	socket := first(func(c string) bool {
		return strings.HasPrefix(c, "/v1/service") && strings.Contains(c, mailpolicy.SocketUnitUnix) && strings.Contains(c, `"start"`)
	})
	policy := first(func(c string) bool {
		return strings.HasPrefix(c, "/v1/config/apply") && strings.Contains(c, mailpolicy.PolicyFile)
	})
	start := first(func(c string) bool {
		return strings.HasPrefix(c, "/v1/service") && strings.Contains(c, mailpolicy.Unit) && strings.Contains(c, `"start"`)
	})
	mainCf := -1
	for i, c := range calls {
		if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), "default_action=tempfail") {
			mainCf = i
			break
		}
	}
	if group < 0 || postfix >= 0 || policy < 0 || socket < 0 || start < 0 || mainCf < 0 || !(group < policy && policy < socket && socket < start && start < mainCf) {
		t.Fatalf("order: group %d, postfix in the group %d (none), policy %d, socket %d, start %d, main.cf %d", group, postfix, policy, socket, start, mainCf)
	}

	// A unit that runs a binary an upgrade replaced restarts on the new one.
	f.agent.BinaryReplaced = map[string]bool{mailpolicy.Unit: true}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/mail/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("mail.apply: %s %s", job.Status, job.Error)
	}
	if a := f.agent.UnitAction(mailpolicy.Unit); a != "restart" {
		t.Fatalf("a replaced binary: %q", a)
	}
}

// A unit that does not start fails the change, and postfix keeps the
// configuration it had: no main.cf that waits for a policy that is not
// there.
func TestMailPolicyUnitThatDoesNotStart(t *testing.T) {
	f := newMailFixture(t)
	before, _ := f.agent.File("/etc/postfix/main.cf")
	f.agent.Fail = map[string]string{"/v1/service": "Job for monopanel-mailpolicy.service failed"}
	f.call(http.MethodPut, "/mail/settings", map[string]any{"max_size_mb": 30}, http.StatusBadGateway, nil)
	if after, _ := f.agent.File("/etc/postfix/main.cf"); after != before {
		t.Fatal("main.cf changed though the policy did not start")
	}
}

// A host whose mail server an older panel set up gets this version's mail
// configuration at the API's first start, by a mail.apply retried until it
// goes through, and only once.
func TestMailPolicyMovesOnUpgrade(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mail.example.com ESMTP" })
	f.s.moveMailPolicy(f.ctx)
	if _, err := f.db.GetSetting(f.ctx, settingMailPolicy); err == nil {
		t.Fatal("a host without the mail server moved")
	}
	// The mail server of 0.8.19: installed, no policy.
	if err := f.s.saveMailConfig(f.ctx, mailConfig{Installed: true, Hostname: "mail.example.com", DKIM: true, Port25: true, POP3: true, MaxSizeMB: 50, VmailUID: 990, VmailGID: 990, PostfixVersion: "3.10.1", DovecotVersion: "2.4.1"}); err != nil {
		t.Fatal(err)
	}
	f.agent.Fail = map[string]string{"/v1/group/ensure": "groupadd: cannot lock /etc/group"}
	f.s.moveMailPolicy(f.ctx)
	job := f.waitJobOfType("mail.apply")
	if job.Status != store.JobFailed {
		t.Fatalf("the first try: %s %s", job.Status, job.Error)
	}
	f.s.moveMailPolicy(f.ctx)
	m := f.s.mailPolicyMoveState(f.ctx)
	if m.Done != 0 || m.Tries != 1 || m.Job != 0 || !m.Next.After(time.Now()) {
		t.Fatalf("after a failure: %+v", m)
	}
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if !slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool {
		return c.Name == "mail policy" && c.Status == "warn" && strings.Contains(c.Detail, "failed 1 time(s)")
	}) {
		t.Fatalf("doctor: %+v", doc.Checks)
	}
	// Not due yet: nothing is queued. Then the next try, which goes through.
	f.agent.Fail = nil
	f.s.moveMailPolicy(f.ctx)
	if jobs, _ := f.db.ListJobs(f.ctx, 10, store.JobQueued); len(jobs) != 0 {
		t.Fatalf("queued before its time: %+v", jobs)
	}
	m.Next = time.Now().Add(-time.Minute)
	f.s.saveMailPolicyMove(f.ctx, m)
	f.s.moveMailPolicy(f.ctx)
	m = f.s.mailPolicyMoveState(f.ctx)
	if job := f.waitJob(m.Job); job.Status != store.JobDone {
		t.Fatalf("the second try: %s %s", job.Status, job.Error)
	}
	if m = f.s.mailPolicyMoveState(f.ctx); m.Done != mailPolicyGeneration || m.Job != 0 {
		t.Fatalf("after the move: %+v", m)
	}
	readPolicy(f)
	if f.agent.UnitAction(mailpolicy.Unit) != "start" {
		t.Fatal("the unit did not start")
	}
	applies := func() int {
		n := 0
		for _, st := range []string{store.JobQueued, store.JobRunning, store.JobDone, store.JobFailed} {
			jobs, _ := f.db.ListJobs(f.ctx, 100, st)
			for _, j := range jobs {
				if j.Type == "mail.apply" {
					n++
				}
			}
		}
		return n
	}
	before := applies()
	f.s.moveMailPolicy(f.ctx)
	if applies() != before || f.s.mailPolicyMoveState(f.ctx).Job != 0 {
		t.Fatal("the move ran again")
	}
}

func TestMailPolicyDoctor(t *testing.T) {
	f := newSiteFixture(t)
	doctor := func() *apitypes.Check {
		t.Helper()
		var doc apitypes.Doctor
		f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
		for i, c := range doc.Checks {
			if c.Name == "mail policy" {
				return &doc.Checks[i]
			}
		}
		return nil
	}
	if c := doctor(); c != nil {
		t.Fatalf("a host without the mail server: %+v", c)
	}
	f = newMailFixture(t)
	if c := doctor(); c == nil || c.Status != "ok" || !strings.Contains(c.Detail, mailpolicy.SocketPath) {
		t.Fatalf("running: %+v", c)
	}
	f.agent.ActiveState = map[string]string{mailpolicy.Unit: "failed"}
	if c := doctor(); c == nil || c.Status != "fail" || !strings.Contains(c.Detail, "mail waits until it is back: systemctl status monopanel-mailpolicy-unix monopanel-mailpolicy") {
		t.Fatalf("failed unit: %+v", c)
	}
	f.agent.ActiveState = map[string]string{mailpolicy.SocketUnitUnix: "inactive"}
	if c := doctor(); c == nil || c.Status != "fail" || !strings.Contains(c.Detail, mailpolicy.SocketUnitUnix+" is inactive") {
		t.Fatalf("its socket stopped: %+v", c)
	}
	f.agent.ActiveState = nil
	if _, err := f.agent.Client().Service(f.ctx, mailpolicy.Unit, "stop"); err != nil {
		t.Fatal(err)
	}
	f.agent.ActiveState = map[string]string{mailpolicy.Unit: "active"}
	if c := doctor(); c == nil || c.Status != "fail" || !strings.Contains(c.Detail, "does not answer") || !strings.Contains(c.Detail, "systemctl status monopanel-mailpolicy") {
		t.Fatalf("a unit that does not answer: %+v", c)
	}
	f.agent.ActiveState = nil
	if _, err := f.agent.Client().Service(f.ctx, mailpolicy.Unit, "start"); err != nil {
		t.Fatal(err)
	}
	// A write of the policy's file that failed: the milter keeps the policy
	// it had, which the doctor says until a write goes through.
	f.agent.Fail = map[string]string{"/v1/config/apply": "disk full"}
	f.s.refreshMailPolicy(f.ctx)
	f.agent.Fail = nil
	if c := doctor(); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "disk full") {
		t.Fatalf("a failed write: %+v", c)
	}
	f.s.refreshed.Store(true) // the start-up refresh ran
	f.s.retryMoves(f.ctx)
	if c := doctor(); c == nil || c.Status != "ok" {
		t.Fatalf("after the hourly retry: %+v", c)
	}
}

// On EL the guard must run too: without it any local account has opendkim
// sign any domain.
func TestMailPolicyDoctorOnEL(t *testing.T) {
	old := fixtureOSRelease
	fixtureOSRelease = "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=10.0\n"
	defer func() { fixtureOSRelease = old }()
	f := newMailFixture(t)
	var doc apitypes.Doctor
	line := func() apitypes.Check {
		f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
		for _, c := range doc.Checks {
			if c.Name == "mail policy" {
				return c
			}
		}
		t.Fatal("no mail policy line")
		return apitypes.Check{}
	}
	if c := line(); c.Status != "ok" || !strings.Contains(c.Detail, mailpolicy.TCPAddress) {
		t.Fatalf("EL: %+v", c)
	}
	f.agent.ActiveState = map[string]string{mailpolicy.GuardUnit: "inactive"}
	if c := line(); c.Status != "fail" || !strings.Contains(c.Detail, "opendkim sign any domain") {
		t.Fatalf("EL without the guard: %+v", c)
	}
}

// The API's start-up refresh brings a host with the mail server of an older
// panel to this version's mail configuration.
func TestStartupRefreshMovesTheMailPolicy(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mail.example.com ESMTP" })
	if err := f.s.saveMailConfig(f.ctx, mailConfig{Installed: true, Hostname: "mail.example.com", DKIM: true, Port25: true, POP3: true, MaxSizeMB: 50, VmailUID: 990, VmailGID: 990}); err != nil {
		t.Fatal(err)
	}
	f.s.startupRefresh(f.ctx)
	job := f.waitJobOfType("mail.apply")
	if job.Status != store.JobDone || job.RequestedBy != "system" {
		t.Fatalf("mail.apply: %s by %s, %s", job.Status, job.RequestedBy, job.Error)
	}
	if m := f.s.mailPolicyMoveState(f.ctx); m.Done != mailPolicyGeneration {
		t.Fatalf("after the start: %+v", m)
	}
	readPolicy(f)
}

// main.cf that lost the policy after the move was recorded — the package's
// preremove takes it out on a removal, an older version's mp mail apply
// drops it after a downgrade — and the panel comes back (installed again,
// upgraded again): the next start (and the hourly retry) puts it back with
// a mail.apply, and until then the doctor fails, saying why (review r2 F1;
// its test, turned around).
func TestMailPolicyGoneFromMainCf(t *testing.T) {
	f := newMailFixture(t)
	main, ok := f.agent.File("/etc/postfix/main.cf")
	if !ok {
		t.Fatal("no main.cf")
	}
	tok := f.s.mailPolicyListener().MilterToken()
	stripped := strings.ReplaceAll(strings.ReplaceAll(main, tok+", ", ""), tok, "")
	if stripped == main {
		t.Fatalf("main.cf names no policy:\n%s", main)
	}
	// What packaging/scripts/preremove.sh's sed leaves on a removal.
	if _, err := f.agent.Client().ApplyConfigSet(f.ctx, &agent.ApplyConfigSetRequest{
		Files: []agent.FileSpec{{Path: "/etc/postfix/main.cf", Content: stripped, Mode: 0o644}}, Origin: "test",
	}); err != nil {
		t.Fatal(err)
	}
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if !slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool {
		return c.Name == "mail policy" && c.Status == "fail" && strings.Contains(c.Detail, "postfix does not ask the mail policy") && strings.Contains(c.Detail, "an account can send as another account's domain")
	}) {
		t.Errorf("doctor while main.cf lacks the policy: %+v", doc.Checks)
	}
	f.s.moveMailPolicy(f.ctx) // the API's next start (startupRefresh)
	job := f.waitJobOfType("mail.apply")
	if job.Status != store.JobDone || job.RequestedBy != "system" {
		t.Fatalf("mail.apply: %s by %s %s", job.Status, job.RequestedBy, job.Error)
	}
	if main, _ := f.agent.File("/etc/postfix/main.cf"); !strings.Contains(main, "\nsmtpd_milters = "+tok) {
		t.Errorf("main.cf after the start:\n%s", main)
	}
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	if !slices.ContainsFunc(doc.Checks, func(c apitypes.Check) bool { return c.Name == "mail policy" && c.Status == "ok" }) {
		t.Errorf("doctor afterwards: %+v", doc.Checks)
	}
}

// A policy that never answers — its port taken, its process failing at
// once (Type=simple: systemd started it all the same) — is not put into a
// main.cf that does not name it yet: the upgrade's mail.apply fails, a mail
// server that worked keeps working (unchecked, as before the upgrade), the
// move is not recorded and tries again later (review r2 F4; its test,
// turned around).
func TestMailPolicyNeverAnswers(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mail.example.com ESMTP" })
	f.s.mailPolicyWait = time.Millisecond
	if err := f.s.saveMailConfig(f.ctx, mailConfig{Installed: true, Hostname: "mail.example.com", DKIM: true, Port25: true, POP3: true, MaxSizeMB: 50, VmailUID: 990, VmailGID: 990, PostfixVersion: "3.10.1", DovecotVersion: "2.4.1"}); err != nil {
		t.Fatal(err)
	}
	f.agent.Fail = map[string]string{"/v1/mailpolicy/probe": "milter: negotiate: optneg read: connection reset by peer"}
	f.s.moveMailPolicy(f.ctx)
	job := f.waitJobOfType("mail.apply")
	if main, _ := f.agent.File("/etc/postfix/main.cf"); strings.Contains(main, "default_action=tempfail") {
		t.Errorf("mail.apply %s: main.cf names a policy that never answered (every message waits)", job.Status)
	}
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "does not answer") {
		t.Errorf("mail.apply: %s %q", job.Status, job.Error)
	}
	f.s.moveMailPolicy(f.ctx)
	if m := f.s.mailPolicyMoveState(f.ctx); m.Done != 0 || m.Tries != 1 || !m.Next.After(time.Now()) {
		t.Errorf("the move after a policy that never answered: %+v", m)
	}
	// Once main.cf names the policy, one that does not answer yet is said,
	// and mail waits for it (the change of the settings goes through).
	f.agent.Fail = nil
	m := f.s.mailPolicyMoveState(f.ctx)
	m.Next = time.Now().Add(-time.Minute)
	f.s.saveMailPolicyMove(f.ctx, m)
	f.s.moveMailPolicy(f.ctx)
	if job := f.waitJob(f.s.mailPolicyMoveState(f.ctx).Job); job.Status != store.JobDone {
		t.Fatalf("the second try: %s %s", job.Status, job.Error)
	}
	f.agent.Fail = map[string]string{"/v1/mailpolicy/probe": "milter: negotiate: optneg read: connection reset by peer"}
	f.call(http.MethodPut, "/mail/settings", map[string]any{"max_size_mb": 30}, http.StatusOK, nil)
}

// While the updater has not confirmed this version, the move waits for its
// verdict: an update that rolls back puts the binary it replaced back,
// which knows no policy, and a main.cf that waited for the policy would
// keep every message waiting then (review r3 F8). Once the update is
// through, the move goes.
func TestMailPolicyMoveWaitsForTheUpdate(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mail.example.com ESMTP" })
	f.s.mailPolicyUpdateStep = 20 * time.Millisecond
	if err := f.s.saveMailConfig(f.ctx, mailConfig{Installed: true, Hostname: "mail.example.com", DKIM: true, Port25: true, POP3: true, MaxSizeMB: 50, VmailUID: 990, VmailGID: 990, PostfixVersion: "3.10.1", DovecotVersion: "2.4.1"}); err != nil {
		t.Fatal(err)
	}
	dir := f.s.cfg.UpdatesDir()
	write := func(status string) {
		t.Helper()
		b, _ := json.Marshal(updater.State{Status: status, From: "0.8.19", To: buildinfo.Version, Started: time.Now()})
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(updater.StateFile(dir), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(updater.StatusInstalling)
	if !f.s.updateUnconfirmed() {
		t.Fatalf("the state file is not where the updater writes it: %s", dir)
	}
	f.s.moveMailPolicy(f.ctx)
	time.Sleep(100 * time.Millisecond)
	if jobs, _ := f.db.ListJobs(f.ctx, 10, ""); len(jobs) != 0 {
		t.Fatalf("queued before the update was confirmed: %+v", jobs)
	}
	write(updater.StatusDone)
	job := f.waitJobOfType("mail.apply")
	if job.Status != store.JobDone {
		t.Fatalf("mail.apply after the update: %s %s", job.Status, job.Error)
	}
}

// An ordinary account cannot take another account's mail domain by its very
// name as a site or an alias — the check of names taken knew sites only —,
// and a site of that name that an administrator gives another account
// leaves the name to the mail domain's owner, whose key opendkim signs it
// with (review r1 F5; its test, turned around).
func TestMailPolicyMailDomainIsItsOwnersAlone(t *testing.T) {
	f := newMailFixture(t)
	uid, gid := 1501, 1501
	other := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/bob", Shell: true}
	if err := f.db.CreateUser(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/mail/domains", map[string]any{"name": "bob-corp.test", "user": "bob"}, http.StatusCreated, nil)

	u, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := auth.HashPassword("alex-password-1")
	if err := f.db.SetUserPassword(f.ctx, u.ID, h); err != nil {
		t.Fatal(err)
	}
	admin := f.cookie
	f.loginAs("alex", "alex-password-1")
	f.call(http.MethodPost, "/sites", map[string]any{"domain": "alex-site.test", "aliases": []string{"www.bob-corp.test"}, "php_version": "8.4", "ssl": "none"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites", map[string]any{"domain": "alex-site.test", "aliases": []string{"bob-corp.test"}, "php_version": "8.4", "ssl": "none"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites", map[string]any{"domain": "bob-corp.test", "php_version": "8.4", "ssl": "none"}, http.StatusUnprocessableEntity, nil)
	if p := readPolicy(f); slices.Contains(p.Accounts["alex"].Domains, "bob-corp.test") {
		t.Fatalf("alex took bob's mail domain: %v", p.Accounts["alex"].Domains)
	}
	// An administrator may give alex a site of that name: the name stays
	// bob's to send as.
	f.cookie = admin
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites", map[string]any{"domain": "alex-site.test", "aliases": []string{"bob-corp.test"}, "user": "alex", "php_version": "8.4", "ssl": "none"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	p := readPolicy(f)
	if ix := mailpolicy.NewIndex(p); !slices.Equal(ix.Owners("bob-corp.test"), []string{"bob"}) || !slices.Contains(p.Accounts["alex"].Domains, "alex-site.test") {
		t.Errorf("an administrator's alias of bob's mail domain: owners %v, alex %v", ix.Owners("bob-corp.test"), p.Accounts["alex"].Domains)
	}
}

// Every job that changes accounts, sites or their aliases writes the policy
// after it, whether it went through or not: migrate.run among them, which
// makes an account and its sites (review r3 F11: no test had it).
func TestMailPolicyFollowsTheJobs(t *testing.T) {
	f := newMailFixture(t)
	writes := func() int {
		n := 0
		for _, c := range f.agent.Calls() {
			if c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), mailpolicy.PolicyFile) {
				n++
			}
		}
		return n
	}
	for _, typ := range []string{"user.provision", "user.delete", "site.apply", "site.delete", "migrate.run"} {
		before := writes()
		j, err := f.s.jobs.Enqueue(f.ctx, typ, map[string]any{"user_id": 999999, "site_id": 999999})
		if err != nil {
			t.Fatal(err)
		}
		f.waitJob(j.ID)
		if writes() == before {
			t.Errorf("%s: no write of the policy after the job", typ)
		}
	}
}
