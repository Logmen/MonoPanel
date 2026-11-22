package mailpolicy

import (
	"strings"
	"testing"
)

func addr(s string) Address {
	a := EnvelopeAddress(s, "mail.host.test")
	return a
}

func TestCheckTable(t *testing.T) {
	ix := testIndex()
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "test"}
	aMailbox := Identity{Kind: KindAccount, Login: "tenanta", Via: "test"}
	nobody := Identity{Kind: KindNobody, Local: true, Via: "test"}
	free := Identity{Kind: KindFree, Login: "root", Local: true, Via: "test"}
	delivery := Identity{Kind: KindNobody, Login: "vmail", Local: true, Delivery: true, Via: "test"}
	for _, tc := range []struct {
		name     string
		id       Identity
		envelope string
		from     []string
		refused  string // the address the refusal names, "" when the message goes
	}{
		{"own mail domain", a, "info@a-corp.test", []string{"info@a-corp.test"}, ""},
		{"own site and alias", a, "noreply@site-a.test", []string{"x@www-a.test"}, ""},
		{"own subdomain", a, "", []string{"x@shop.a-corp.test"}, ""},
		{"another account's domain in From", a, "", []string{"ceo@b-corp.test"}, "b-corp.test"},
		{"another account's domain as envelope", a, "ceo@b-corp.test", []string{"info@a-corp.test"}, "b-corp.test"},
		{"another account's subdomain", a, "", []string{"x@a.example.com"}, "a.example.com"},
		{"under the longest suffix", a, "", []string{"x@y.a.example.com"}, "y.a.example.com"},
		{"under a shorter one", a, "", []string{"x@x.example.com"}, "x.example.com"},
		{"foreign domain", a, "someone@gmail.com", []string{"someone@gmail.com"}, ""},
		{"several From headers, one foreign one not", a, "", []string{"a@gmail.com", "ceo@b-corp.test"}, "b-corp.test"},
		{"own login at the host name", a, "tenanta@mail.host.test", []string{"tenanta@mail.host.test"}, ""},
		{"root at the host name (Debian's cron)", a, "", []string{"root@mail.host.test (Cron Daemon)"}, ""},
		{"postmaster at the host name", a, "", []string{"postmaster@mail.host.test"}, "postmaster@mail.host.test"},
		{"another login at the host name", a, "tenantb@mail.host.test", nil, "tenantb@mail.host.test"},
		{"a mailbox at the host name", aMailbox, "", []string{"tenanta@mail.host.test"}, "tenanta@mail.host.test"},
		{"a mailbox as root at the host name", aMailbox, "", []string{"root@mail.host.test"}, "root@mail.host.test"},
		{"root sends as anything", free, "ceo@b-corp.test", []string{"ceo@b-corp.test", "x@mail.host.test"}, ""},
		{"nobody: no panel domain", nobody, "", []string{"info@a-corp.test"}, "a-corp.test"},
		{"nobody: foreign domains", nobody, "x@gmail.com", []string{"x@gmail.com"}, ""},
		{"nobody: not the host name", nobody, "", []string{"tenanta@mail.host.test"}, "tenanta@mail.host.test"},
		{"delivery: postmaster", delivery, "", []string{"postmaster@mail.host.test", "MAILER-DAEMON@mail.host.test"}, ""},
		{"delivery: no account's domain", delivery, "", []string{"info@a-corp.test"}, "a-corp.test"},
	} {
		var env *Address
		if tc.envelope != "" {
			e := addr(tc.envelope)
			env = &e
		}
		var from []Address
		for _, f := range tc.from {
			got, _ := FromAddresses(f)
			from = append(from, got...)
		}
		r := Check(ix, tc.id, env, from, true)
		switch {
		case tc.refused == "" && r != nil:
			t.Errorf("%s: refused: %v", tc.name, r)
		case tc.refused != "" && r == nil:
			t.Errorf("%s: went, want a refusal of %s", tc.name, tc.refused)
		case r != nil && r.Address.String() != tc.refused && r.Address.Domain != tc.refused:
			t.Errorf("%s: refused %s, want %s", tc.name, r.Address, tc.refused)
		}
	}
}

func TestCheckHostnameIsAnAccountsDomain(t *testing.T) {
	p := testPolicy()
	p.Hostname = "a-corp.test"
	ix := NewIndex(p)
	b := Identity{Kind: KindAccount, Login: "tenantb", Local: true, Via: "test"}
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "test"}
	from := func(s string) []Address { got, _ := FromAddresses(s); return got }
	if r := Check(ix, b, nil, from("tenantb@a-corp.test"), true); r != nil {
		t.Errorf("tenantb's own login at the host name, which is tenanta's mail domain: %v", r)
	}
	if r := Check(ix, b, nil, from("info@a-corp.test"), true); r == nil {
		t.Error("tenantb sent as another login at tenanta's domain, which is the host name")
	}
	if r := Check(ix, a, nil, from("info@a-corp.test"), true); r != nil {
		t.Errorf("tenanta at its own domain, which is the host name: %v", r)
	}
}

func TestCheckUnreadable(t *testing.T) {
	ix := testIndex()
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "test"}
	r := Check(ix, a, nil, nil, false)
	if r == nil || r.Text != ReasonUnreadable {
		t.Fatalf("an unreadable From: %v", r)
	}
	if r := Check(ix, Identity{Kind: KindFree, Via: "test"}, nil, nil, false); r != nil {
		t.Errorf("root's unreadable From: %v", r)
	}
}

// The refusal names the domain, and never the account that owns it.
func TestRefusalNamesNoAccount(t *testing.T) {
	ix := testIndex()
	from, _ := FromAddresses("CEO <ceo@b-corp.test>")
	r := Check(ix, Identity{Kind: KindAccount, Login: "tenanta", Via: "test"}, nil, from, true)
	if r == nil {
		t.Fatal("not refused")
	}
	want := "5.7.1 <b-corp.test>: this server does not send mail as a domain of another account"
	if r.Error() != want {
		t.Errorf("refusal %q, want %q", r.Error(), want)
	}
	for _, login := range []string{"tenantb", "tenanta", "tenantc"} {
		if strings.Contains(r.Error(), login) {
			t.Errorf("the refusal names the account %s: %q", login, r.Error())
		}
	}
}

// root@<host name> goes for a local account (Debian's cron) only while no
// account owns the name: when the host name is an account's domain,
// opendkim signs root@ it with that account's key (review r1 F9). The
// account's own login there still goes, and the owner sends as anything of
// it.
func TestCheckRootAtAnAccountsHostName(t *testing.T) {
	p := testPolicy()
	p.Hostname = "b-corp.test"
	ix := NewIndex(p)
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "sendmail, uid 1000 (tenanta)"}
	b := Identity{Kind: KindAccount, Login: "tenantb", Local: true, Via: "sendmail, uid 1001 (tenantb)"}
	for _, tc := range []struct {
		id      Identity
		from    string
		refused bool
	}{
		{a, "root@b-corp.test", true},
		{a, "tenanta@b-corp.test", false},
		{a, "ceo@b-corp.test", true},
		{b, "root@b-corp.test", false},
		{b, "ceo@b-corp.test", false},
	} {
		addrs, ok := FromAddresses(tc.from)
		if r := Check(ix, tc.id, nil, addrs, ok); (r != nil) != tc.refused {
			t.Errorf("%s sends From: %s: %v, want refused=%v", tc.id.Login, tc.from, r, tc.refused)
		}
	}
	// Where nobody owns the host name, root@ it goes (the default).
	addrs, ok := FromAddresses("root@mail.host.test")
	if r := Check(testIndex(), a, nil, addrs, ok); r != nil {
		t.Errorf("root@ a host name nobody owns: %v", r)
	}
}
