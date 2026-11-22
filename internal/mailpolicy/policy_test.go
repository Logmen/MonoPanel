package mailpolicy

import (
	"slices"
	"strings"
	"testing"
)

// testPolicy is two accounts and the mail server's host name: tenanta has
// a-corp.test (mail), shop.a-corp.test is nobody's but under it, site-a.test
// with the alias www-a.test; tenantb has b-corp.test and the subdomain
// a.example.com of example.com, which tenantc owns.
func testPolicy() *Policy {
	return &Policy{
		Version:      Version,
		Hostname:     "mail.host.test",
		Delivery:     "vmail",
		AccountHomes: "/var/www",
		Accounts: map[string]Account{
			"tenanta": {Domains: []string{"a-corp.test", "site-a.test", "www-a.test"}},
			"tenantb": {Domains: []string{"b-corp.test", "a.example.com", "xn--80aswg.xn--p1ai"}},
			"tenantc": {Domains: []string{"example.com"}},
			"empty":   {Domains: []string{}},
		},
		Mailboxes: map[string]string{"info@a-corp.test": "tenanta", "ceo@b-corp.test": "tenantb"},
	}
}

func testIndex() *Index { return NewIndex(testPolicy()) }

func TestParseStrict(t *testing.T) {
	good, err := testPolicy().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(good); err != nil {
		t.Fatalf("the API's own policy: %v", err)
	}
	bad := map[string]string{
		"unknown field":         `{"version":1,"accounts":{},"mailboxes":{},"extra":1}`,
		"second value":          `{"version":1,"accounts":{},"mailboxes":{}} {}`,
		"version":               `{"version":2,"accounts":{},"mailboxes":{}}`,
		"no accounts":           `{"version":1,"mailboxes":{}}`,
		"no domains list":       `{"version":1,"accounts":{"a":{}},"mailboxes":{}}`,
		"login":                 `{"version":1,"accounts":{"Root!":{"domains":[]}},"mailboxes":{}}`,
		"upper case domain":     `{"version":1,"accounts":{"a":{"domains":["A.test"]}},"mailboxes":{}}`,
		"single label":          `{"version":1,"accounts":{"a":{"domains":["com"]}},"mailboxes":{}}`,
		"trailing dot":          `{"version":1,"accounts":{"a":{"domains":["a.test."]}},"mailboxes":{}}`,
		"wildcard":              `{"version":1,"accounts":{"a":{"domains":["*.a.test"]}},"mailboxes":{}}`,
		"unicode":               `{"version":1,"accounts":{"a":{"domains":["пример.рф"]}},"mailboxes":{}}`,
		"mailbox of no account": `{"version":1,"accounts":{"a":{"domains":[]}},"mailboxes":{"x@a.test":"b"}}`,
		"mailbox not address":   `{"version":1,"accounts":{"a":{"domains":[]}},"mailboxes":{"x":"a"}}`,
		"host name":             `{"version":1,"hostname":"bad host","accounts":{},"mailboxes":{}}`,
		"delivery user":         `{"version":1,"delivery_user":"Vmail!","accounts":{},"mailboxes":{}}`,
		"not json":              `version=1`,
	}
	for name, content := range bad {
		if _, err := Parse([]byte(content)); err == nil {
			t.Errorf("%s: accepted %s", name, content)
		}
	}
	if _, err := Parse(make([]byte, MaxPolicySize+1)); err == nil {
		t.Error("a policy over MaxPolicySize was accepted")
	}
}

func TestMarshalSortsAndDedups(t *testing.T) {
	p := testPolicy()
	p.Accounts["tenanta"] = Account{Domains: []string{"www-a.test", "a-corp.test", "www-a.test"}}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Accounts["tenanta"].Domains; !slices.Equal(got, []string{"a-corp.test", "www-a.test"}) {
		t.Errorf("domains %v", got)
	}
	again, _ := back.Marshal()
	if string(again) != string(b) {
		t.Error("marshalling the same policy twice differs")
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"B-Corp.TEST":         "b-corp.test",
		"b-corp.test.":        "b-corp.test",
		" b-corp.test..":      "b-corp.test",
		"пример.рф":           "xn--e1afmkfd.xn--p1ai",
		"ПРИМЕР.РФ.":          "xn--e1afmkfd.xn--p1ai",
		"xn--80aswg.xn--P1AI": "xn--80aswg.xn--p1ai",
	} {
		if got, _ := NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
	if _, ok := NormalizeDomain(" . "); ok {
		t.Error("an empty name normalized")
	}
}

func TestOwnersLongestSuffix(t *testing.T) {
	ix := testIndex()
	for domain, want := range map[string]string{
		"a-corp.test":         "tenanta",
		"shop.a-corp.test":    "tenanta",
		"A-CORP.TEST.":        "tenanta",
		"b-corp.test":         "tenantb",
		"a.example.com":       "tenantb",
		"x.a.example.com":     "tenantb",
		"example.com":         "tenantc",
		"x.example.com":       "tenantc",
		"badexample.com":      "",
		"corp.test":           "",
		"gmail.com":           "",
		"сайт.рф":             "tenantb", // xn--80aswg.xn--p1ai
		"www.сайт.рф":         "tenantb",
		"xn--80aswg.xn--p1ai": "tenantb",
	} {
		if got := strings.Join(ix.Owners(domain), ","); got != want {
			t.Errorf("Owners(%q) = %q, want %q", domain, got, want)
		}
	}
}

func TestOwnersShared(t *testing.T) {
	p := testPolicy()
	p.Accounts["tenantc"] = Account{Domains: []string{"example.com", "b-corp.test"}}
	ix := NewIndex(p)
	if got := ix.Owners("b-corp.test"); !slices.Equal(got, []string{"tenantb", "tenantc"}) {
		t.Errorf("a name of two accounts: %v", got)
	}
	if got := ix.AddressAccount("x@b-corp.test"); got != "" {
		t.Errorf("an address of a shared domain names %q", got)
	}
}

func TestMailboxAccount(t *testing.T) {
	ix := testIndex()
	for addr, want := range map[string]string{
		"info@a-corp.test":   "tenanta",
		"INFO@A-Corp.Test.":  "tenanta",
		"ceo@b-corp.test":    "tenantb",
		"nobody@a-corp.test": "",
		"info":               "",
	} {
		if got := ix.MailboxAccount(addr); got != want {
			t.Errorf("MailboxAccount(%q) = %q, want %q", addr, got, want)
		}
	}
	if got := ix.AddressAccount("info+tag@a-corp.test"); got != "tenanta" {
		t.Errorf("an address under a domain: %q", got)
	}
}
