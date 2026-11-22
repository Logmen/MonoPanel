package mailpolicy

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// domainsOf lists the distinct domains of addrs, in their order.
func domainsOf(addrs []Address) string {
	var d []string
	for _, a := range addrs {
		if !slices.Contains(d, a.Domain) {
			d = append(d, a.Domain)
		}
	}
	return strings.Join(d, ",")
}

func TestFromAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"plain", "ceo@b-corp.test", "b-corp.test"},
		{"angle", "CEO <ceo@B-Corp.Test>", "b-corp.test"},
		{"trailing dot", "<ceo@b-corp.test.>", "b-corp.test"},
		{"folded", "CEO\n\t<ceo@b-corp.test>", "b-corp.test"},
		{"folded crlf", "CEO\r\n <ceo@b-corp.test>", "b-corp.test"},
		{"display name with @", `"ceo@b-corp.test" <me@a-corp.test>`, "b-corp.test,a-corp.test"},
		{"bare display name with @", `ceo@b-corp.test <me@a-corp.test>`, "b-corp.test,a-corp.test"},
		{"comment", "me@a-corp.test (on behalf of ceo@b-corp.test)", "a-corp.test,b-corp.test"},
		{"quoted local part with @", `"ceo@b-corp.test"@a-corp.test`, "b-corp.test,a-corp.test"},
		{"two addresses", "a@a-corp.test, b@b-corp.test", "a-corp.test,b-corp.test"},
		{"group", "Team: a@a-corp.test, b@b-corp.test;", "a-corp.test,b-corp.test"},
		{"blanks around the @", "ceo @ b-corp.test", "b-corp.test"},
		{"obsolete domain with blanks", "ceo@b-corp . test", "b-corp.test"},
		{"folded inside the domain", "ceo@b-corp.\n\ttest", "b-corp.test"},
		{"comment inside the domain", "ceo@b-corp(x).test", "b-corp.test"},
		{"comment after the @", "ceo@(x)b-corp.test", "b-corp.test"},
		{"unclosed comment", "ceo@(b-corp.test", "b-corp.test"},
		{"route", "<@b-corp.test:me@a-corp.test>", "b-corp.test,a-corp.test"},
		{"idn", "ceo@сайт.рф", "xn--80aswg.xn--p1ai"},
		{"encoded word, encoded @", "=?utf-8?q?ceo=40b-corp.test?= <me@a-corp.test>", "a-corp.test"},
		{"encoded word with a literal @", "=?utf-8?q?ceo@b-corp.test?= <me@a-corp.test>", "b-corp.test,a-corp.test"},
		{"unclosed encoded word", "=?utf-8?q? ceo@b-corp.test", "b-corp.test"},
		{"domain literal", "ceo@[192.0.2.1]", ""},
	} {
		addrs, ok := FromAddresses(tc.value)
		if got := domainsOf(addrs); got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: FromAddresses(%q) = %q, %v; want %q", tc.name, tc.value, got, ok, tc.want)
		}
	}
	for _, unreadable := range []string{"", "undisclosed-recipients:;", "Root", "ceo@", "ceo@ >", "=?utf-8?b?Y2VvQGItY29ycC50ZXN0?="} {
		if addrs, ok := FromAddresses(unreadable); ok {
			t.Errorf("FromAddresses(%q) = %v: no domain in it, it cannot be read", unreadable, addrs)
		}
	}
}

// The local part, as the header reads as it is, comes first; the other
// readings may add others of the same domain (a quoted one without its
// quotes and blanks), which the host name's rule checks as well.
func TestFromLocalParts(t *testing.T) {
	for value, want := range map[string]string{
		"root@mail.host.test (Cron Daemon)":        "root",
		"Cron <tenanta@mail.host.test>":            "tenanta",
		`"(Cron Daemon)" <tenanta@mail.host.test>`: "tenanta",
		`"a b"@mail.host.test`:                     "a b",
		`"a\"b"@mail.host.test`:                    `a"b`,
		"@mail.host.test":                          "",
	} {
		addrs, _ := FromAddresses(value)
		if len(addrs) == 0 || addrs[0].Local != want || domainsOf(addrs) != "mail.host.test" {
			t.Errorf("FromAddresses(%q) = %+v, want local part %q first", value, addrs, want)
		}
	}
	// cron's own forms read one address each: nothing else for the host
	// name's rule to refuse.
	for _, value := range []string{"root@mail.host.test (Cron Daemon)", "Cron <tenanta@mail.host.test>", `"(Cron Daemon)" <tenanta@mail.host.test>`} {
		if addrs, _ := FromAddresses(value); len(addrs) != 1 {
			t.Errorf("FromAddresses(%q) = %+v, want one address", value, addrs)
		}
	}
}

// opendkim's parser takes comments and blanks (C's isspace: \v and \f too)
// out of a domain anywhere, inside a label as well, and signs what is left
// with that domain's key — seen on mp-rocky9 (opendkim 2.11.0-0.36.el9) and
// mp-debian13 (2.11.0~beta2): "ceo@rv(x)wb.test", "ceo@rvwb.te(x)st",
// "ceo@r(a)v(b)w(c)b.test", "ceo@rv((x))wb.test", "ceo@rvw(CRLF )b.test",
// "<ceo@b-co rp.test>", "<ceo@b-corp\v.test>" all signed as the other
// account's domain. Each of them, and the forms a quote or a parenthesis
// could hide from one reading, must be refused for tenanta (reviews r1 F4
// and r3 F1).
func TestFromAddressesReadsWhatOpendkimSigns(t *testing.T) {
	ix := testIndex()
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "test"}
	for _, from := range []string{
		// r1, as seen on the VMs (rvwb.test there is b-corp.test here)
		"ceo@b-co(x)rp.test",
		"ceo@b-co()rp.test",
		"ceo@b-corp.te(x)st",
		"ceo@b(a)-(b)c(c)orp.test",
		"ceo@b-co((x))rp.test",
		"CEO <ceo@b-co()rp.test>",
		"ceo@b-co(\r\n )rp.test",
		// r3, as seen on the VMs, and the same rule
		"<ceo@b-co rp.test>",
		"<ceo@b-co(x)rp.test>",
		"<ceo@b-corp\v.test>",
		"CEO <ceo@b-corp\f.test>",
		"ceo@b-\tcorp.test",
		// a quote or a parenthesis that one reading takes otherwise
		`"(" <ceo@b-co rp.test>`,
		`"x(" <ceo@b-co rp.test>`,
		`(") <ceo@b-co rp.test> ("`,
		`"abc <ceo@b-co rp.test>`,
		`ceo@b-corp\.test`,
		`<ceo@"b-corp".test>`,
		"ceo@b-co)rp.test",
		`x\(<ceo@b-co rp.test>`,
	} {
		addrs, ok := FromAddresses(from)
		if r := Check(ix, a, nil, addrs, ok); r == nil {
			t.Errorf("From: %q goes (the policy reads %v); opendkim signs such a From with b-corp.test's key", from, addrs)
		}
	}
	// The forms of the account's own domain still go.
	for _, from := range []string{"Info <info@a-co rp.test>", "info@a-corp(x).test", `"Info" <info@a-corp.test>`, "info@a-corp.test (Info)"} {
		addrs, ok := FromAddresses(from)
		if r := Check(ix, a, nil, addrs, ok); r != nil {
			t.Errorf("From: %q of tenanta's own domain is refused: %v (read %v)", from, r, addrs)
		}
	}
}

// A From header that names more addresses than a From header does cannot
// be read: what the milter keeps of a message stays small (review r1 F3).
func TestFromAddressesBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 100*1024; i++ {
		b.WriteString("x@d" + strings.Repeat("a", i%7) + string(rune('a'+i%26)) + ".test ")
	}
	if addrs, ok := FromAddresses(b.String()); ok || addrs != nil {
		t.Errorf("a From header of %d addresses reads: %d addresses, %v", strings.Count(b.String(), "@"), len(addrs), ok)
	}
	// The same address many times over is one (one per reading at most).
	same := strings.Repeat("@a.b ", 20000)
	if addrs, ok := FromAddresses(same); !ok || len(addrs) > 3 {
		t.Errorf("one address 20000 times: %d addresses, %v", len(addrs), ok)
	}
	// Exactly the cap goes.
	var c strings.Builder
	for i := 0; i < MaxHeaderAddresses; i++ {
		c.WriteString("x@d" + strings.Repeat("a", i) + ".test, ")
	}
	if addrs, ok := FromAddresses(c.String()); !ok || len(addrs) != MaxHeaderAddresses {
		t.Errorf("%d addresses: %d, %v", MaxHeaderAddresses, len(addrs), ok)
	}
}

// Every reading of a From header takes time linear in its length: postfix
// hands the milter headers of up to 100 kB, as many as a message holds
// (reviews r1 F8, r3 F5: "@(" repeated took 1.76 s, \"@a 418 ms).
func TestFromScanIsLinear(t *testing.T) {
	for name, unit := range map[string]string{
		"unclosed comments": "@(",
		"escaped quotes":    `\"@a`,
		"nested comments":   "((@",
		"closed comments":   "@()",
		"long atoms":        strings.Repeat("a", 200) + "@",
		// a "(" after a backslash, which the pairing of the parentheses
		// takes for escaped (it once sent a reading back to the start)
		"escaped parentheses": `@\(x`,
	} {
		value := strings.Repeat(unit, 100*1024/len(unit))
		start := time.Now()
		FromAddresses(value)
		if took := time.Since(start); took > 150*time.Millisecond {
			t.Errorf("%s: a 100 kB From header takes %v", name, took)
		}
	}
}

// What a From header leaves in memory is its addresses, not the header.
func TestFromAddressesKeepNoHeader(t *testing.T) {
	value := strings.Repeat("x", 100*1024) + " <ceo@b-corp.test>"
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	kept := make([][]Address, 200)
	for i := range kept {
		kept[i], _ = FromAddresses(value)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 4<<20 {
		t.Errorf("200 readings of a 100 kB header keep %d bytes", grown)
	}
	runtime.KeepAlive(kept)
}

func TestEnvelopeAddress(t *testing.T) {
	for in, want := range map[string]Address{
		"ceo@b-corp.test":                {Local: "ceo", Domain: "b-corp.test"},
		"<CEO@B-CORP.TEST.>":             {Local: "CEO", Domain: "b-corp.test"},
		"@relay.test,@x.test:ceo@b.test": {Local: "ceo", Domain: "b.test"},
		`"a@b"@c.test`:                   {Local: "a@b", Domain: "c.test"},
		"tenanta":                        {Local: "tenanta", Domain: "mail.host.test"},
		"root@":                          {Local: "root", Domain: "mail.host.test"},
	} {
		if got := EnvelopeAddress(in, "mail.host.test"); got != want {
			t.Errorf("EnvelopeAddress(%q) = %+v, want %+v", in, got, want)
		}
	}
}
