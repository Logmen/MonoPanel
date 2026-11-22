package mailpolicy

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// opendkimDomain models how opendkim's dkim_mail_parse picks the domain of a
// From value (review r3's model, checked against what opendkim 2.11.0~beta2
// on mp-debian13 and 2.11.0-0.36.el9 on mp-rocky9 signed): the address in
// <...> when there is one, and after its first "@" outside quotes and
// comments every atom up to the next special character, the atoms run
// together across blanks (C isspace: also \v and \f) and comments. A "." is
// an atom character for it, a backslash, a quote or a bracket a special one.
func opendkimDomain(v string) string {
	const specials = "<>@,;:\\\"/[]?="
	s := v
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[i+1:]
		if j := strings.IndexByte(s, '>'); j >= 0 {
			s = s[:j]
		}
	}
	at, depth, inq := -1, 0, false
	for i := 0; i < len(s) && at < 0; i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case inq:
			if c == '"' {
				inq = false
			}
		case c == '"' && depth == 0:
			inq = true
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case c == '@' && depth == 0:
			at = i
		}
	}
	if at < 0 {
		return ""
	}
	var b strings.Builder
	for i := at + 1; i < len(s); {
		c := s[i]
		switch {
		case isBlank(c):
			i++
		case c == '(':
			d := 0
			for ; i < len(s); i++ {
				if s[i] == '\\' {
					i++
					continue
				}
				if s[i] == '(' {
					d++
				}
				if s[i] == ')' {
					d--
					if d == 0 {
						i++
						break
					}
				}
			}
		case strings.IndexByte(specials, c) >= 0:
			return b.String()
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// The model reads what opendkim read on the VMs: signed d=b-corp.test for
// the first four, not for the backslash, quote and bracket forms.
func TestOpendkimModelMatchesTheVMs(t *testing.T) {
	for v, want := range map[string]string{
		"<ceo@b-co rp.test>":                     "b-corp.test",
		"<ceo@b-co(x)rp.test>":                   "b-corp.test",
		"<ceo@b-corp\v.test>":                    "b-corp.test",
		"<ceo@b-corp.test>":                      "b-corp.test",
		"ceo@b-corp.te(x)st":                     "b-corp.test",
		"ceo@b(a)-(b)c(c)orp.test":               "b-corp.test",
		"<ceo@b-corp\\.test>":                    "b-corp",
		"<ceo@b\\-corp.test>":                    "b",
		"<ceo@\"b-corp.test\"> (me@a-corp.test)": "",
		"<ceo@[b-corp.test]> (me@a-corp.test)":   "",
		"<info@a-corp.test>":                     "a-corp.test",
	} {
		if got := opendkimDomain(v); got != want {
			t.Errorf("opendkimDomain(%q) = %q, want %q", v, got, want)
		}
	}
}

// fromSeeds are From values the policy has to read right: the VMs' forms
// and the ones a quote or a parenthesis could hide.
var fromSeeds = []string{
	"ceo@b-corp.test", "CEO <ceo@b-corp.test>", "<ceo@b-corp . test>", "x (c) <a@a-corp.test>",
	"ceo@b-corp(x).test", "\"a@b\" <x@y.z>", "@B-Corp.t est", "<ceo@b-co rp.test>", "<ceo@b-co(x)rp.test>",
	"<ceo@b-corp\v.test>", "\"(\" <ceo@b-co rp.test>", "(\") <ceo@b-co rp.test> (\"", "\"abc <ceo@b-co rp.test>",
	"ceo@b-co)rp.test", "a@a-corp.test, b@b-co rp.test", "(<@B-Co rp.test", "x@b-co rp.test(garbage", "(<@B-Co rp.test\")", "\"<@B-Co rp.test\"00", "ceo@.b-corp.test",
}

// TestFromAgainstOpendkim: whatever domain opendkim would sign a From value
// with is never another account's when the policy lets tenanta's message
// go.
func TestFromAgainstOpendkim(t *testing.T) {
	for _, v := range fromSeeds {
		checkFromAgainstOpendkim(t, v)
	}
}

func checkFromAgainstOpendkim(t *testing.T, v string) {
	t.Helper()
	ix := testIndex()
	a := Identity{Kind: KindAccount, Login: "tenanta", Local: true, Via: "test"}
	addrs, ok := FromAddresses(v)
	if Check(ix, a, nil, addrs, ok) != nil {
		return
	}
	// opendkim signs only a domain of its SigningTable, a valid name; the
	// owners are looked up as the policy does (subdomains included), which
	// is stricter than the SigningTable's exact names.
	d, _ := NormalizeDomain(opendkimDomain(v))
	if !ValidDomain(d) {
		return
	}
	if owners := ix.Owners(d); len(owners) > 0 && !slices.Contains(owners, "tenanta") {
		t.Errorf("the policy lets tenanta send From %q (it reads %v); opendkim signs it as %q (%v)", v, addrs, d, owners)
	}
}

// FuzzFromAgainstOpendkim is TestFromAgainstOpendkim over any From value.
func FuzzFromAgainstOpendkim(f *testing.F) {
	for _, s := range fromSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if strings.ContainsAny(v, "\r\n\x00") || !utf8.ValidString(v) {
			return
		}
		checkFromAgainstOpendkim(t, v)
	})
}

// FuzzFromAddresses: no panic, every address it returns has a domain, and
// never more than MaxHeaderAddresses of them.
func FuzzFromAddresses(f *testing.F) {
	for _, s := range append([]string{"\"a\\\"@b\"@c.d", "a@(b(c)d", "@", "\"@", "(\\", "x@[1.2.3.4]", "=?utf-8?q?a@b?= <c@d.e>"}, fromSeeds...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		addrs, ok := FromAddresses(v)
		if ok != (len(addrs) > 0) || len(addrs) > MaxHeaderAddresses {
			t.Fatalf("ok=%v with %d addresses", ok, len(addrs))
		}
		for _, a := range addrs {
			if a.Domain == "" {
				t.Fatalf("empty domain in %+v", a)
			}
		}
	})
}
