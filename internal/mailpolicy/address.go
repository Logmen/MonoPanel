package mailpolicy

import "strings"

// Address is an address the policy checks: its domain (NormalizeDomain's
// form) and, as well as it can be told, its local part — which only the
// rule for the mail server's own host name looks at.
type Address struct {
	Local  string
	Domain string
}

func (a Address) String() string {
	if a.Local == "" {
		return a.Domain
	}
	return a.Local + "@" + a.Domain
}

// MaxHeaderAddresses is the most distinct addresses one From header may
// name: a From header names one mailbox, rarely a few, and a header that
// names more cannot be read (FromAddresses).
const MaxHeaderAddresses = 64

// maxLocalScan bounds how far back from an "@" its local part is looked for:
// a local part is at most 64 octets (RFC 5321), a quoted one with its
// escapes and quotes a little more.
const maxLocalScan = 256

// FromAddresses reads a From header the way no address parser could read
// more out of it: the header unfolded, every "@" anywhere in it — in the
// address, a display name, a comment, a quoted local part — and the domain
// after it. The header is read several times and the readings joined:
//
//   - as it is, with blanks and comments allowed around the dots of a
//     domain as RFC 5322's obsolete syntax allows them ("ceo@b . example",
//     "ceo@b(x).example" read b.example);
//   - with the comments and blanks outside quoted strings taken out, as
//     opendkim's parser takes them out of a domain anywhere, also inside a
//     label: it signs "ceo@b-co(x)rp.example", "ceo@b-co rp.example" and
//     "ceo@b-corp\v.example" (a vertical tab is a blank to C's isspace) as
//     b-corp.example;
//   - with every comment, blank, backslash and quote taken out wherever it
//     stands, so that a quote or a parenthesis a parser reads otherwise than
//     the second reading hides no domain either;
//   - the last two once with a "(" that nothing closes taken for a comment
//     to the end of the header, once for itself alone;
//   - what follows the first "<" and what follows the last one, so read,
//     as a parser takes the address in angle brackets that knows nothing
//     of a comment or a quote before them.
//
// Encoded words are not decoded: no parser takes an address out of one, and
// a literal "@" in one is read like any other. ok is false when no "@" in
// the header is followed by a domain, or when it names more than
// MaxHeaderAddresses addresses: such a header names no domain a signer could
// pick, or more than a From header does, and the policy cannot read it.
// Every reading takes time linear in the header's length.
func FromAddresses(value string) (addrs []Address, ok bool) {
	s := strings.NewReplacer("\r", "", "\n", "").Replace(value)
	if strings.IndexByte(s, '@') < 0 {
		return nil, false
	}
	c := collector{seen: map[Address]bool{}}
	readings := []string{s}
	if strings.ContainsAny(s, "( \t\v\f\"\\)") {
		bases := []string{s}
		if first, last := strings.IndexByte(s, '<'), strings.LastIndexByte(s, '<'); first >= 0 {
			bases = append(bases, s[first+1:])
			if last != first {
				bases = append(bases, s[last+1:])
			}
		}
		for _, b := range bases {
			readings = append(readings, stripCFWS(b, true, true), stripCFWS(b, false, true))
			if strings.IndexByte(b, '(') >= 0 {
				readings = append(readings, stripCFWS(b, true, false), stripCFWS(b, false, false))
			}
		}
	}
	for _, reading := range readings {
		scanAddresses(reading, &c)
		if c.full {
			return nil, false
		}
	}
	return c.addrs, len(c.addrs) > 0
}

// collector gathers the distinct addresses of a header, MaxHeaderAddresses
// at most.
type collector struct {
	seen  map[Address]bool
	addrs []Address
	full  bool
}

func (c *collector) add(a Address) {
	if c.seen[a] {
		return
	}
	if len(c.addrs) >= MaxHeaderAddresses {
		c.full = true
		return
	}
	// The strings point into the header; a copy keeps the header itself
	// from staying in memory with them.
	a = Address{Local: strings.Clone(a.Local), Domain: strings.Clone(a.Domain)}
	c.seen[a] = true
	c.addrs = append(c.addrs, a)
}

// scanAddresses adds the address after every "@" of s.
func scanAddresses(s string, c *collector) {
	if strings.IndexByte(s, '@') < 0 {
		return
	}
	ends := commentEnds(s)
	for i := 0; i < len(s) && !c.full; i++ {
		if s[i] != '@' {
			continue
		}
		raw := scanDomain(s, i+1, ends)
		if raw == "" {
			continue
		}
		d, _ := NormalizeDomain(raw)
		if d == "" {
			continue
		}
		c.add(Address{Local: scanLocal(s, i), Domain: d})
	}
}

// isBlank is a blank as C's isspace knows it — opendkim's parser skips each
// of them —, a line break included: space, \t, \n, \v, \f, \r.
func isBlank(c byte) bool { return c == ' ' || c >= '\t' && c <= '\r' }

// stripCFWS is s without its comments (nested, a backslash escaping the
// next byte) and without blanks; a ")" that closes no comment goes as well.
// A "(" that nothing closes opens a comment to the end (unclosedRest), or
// goes alone. quotes: what stands between double quotes stays as it is.
// Otherwise quotes and backslashes go too.
func stripCFWS(s string, quotes, unclosedRest bool) string {
	var ends []int32
	if !unclosedRest {
		ends = commentEnds(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	depth, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case depth > 0:
			switch c {
			case '\\':
				i++
			case '(':
				depth++
			case ')':
				depth--
			}
		case c == '(' && !inQuote && ends != nil:
			// A comment that is closed goes whole; an unclosed "(" alone,
			// and so does one commentEnds took for escaped (no end).
			if e := int(ends[i]); e > i {
				i = e - 1
			}
		case inQuote:
			b.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(s) {
					i++
					b.WriteByte(s[i])
				}
			case '"':
				inQuote = false
			}
		case c == '(':
			depth = 1
		case c == ')' || isBlank(c):
		case quotes && c == '"':
			inQuote = true
			b.WriteByte(c)
		case !quotes && (c == '"' || c == '\\'):
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// commentEnds pairs the parentheses of s in one pass: for the "(" at i,
// ends[i] is the index right after the ")" that closes it, or i+1 when none
// does (the "(" is then skipped alone, and what follows it stays text to
// read a domain from). A backslash escapes the next byte. nil when s has
// no "(".
func commentEnds(s string) []int32 {
	if strings.IndexByte(s, '(') < 0 {
		return nil
	}
	ends := make([]int32, len(s))
	var open []int32
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '(':
			open = append(open, int32(j)) //nolint:gosec // a header is far below 2 GB
		case ')':
			if n := len(open); n > 0 {
				ends[open[n-1]] = int32(j + 1) //nolint:gosec // as above
				open = open[:n-1]
			}
		}
	}
	for _, k := range open {
		ends[k] = k + 1
	}
	return ends
}

// skipCFWS skips blanks and (nested) comments from i.
func skipCFWS(s string, i int, ends []int32) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t':
			i++
		case s[i] == '(' && ends != nil:
			i = int(ends[i])
		default:
			return i
		}
	}
	return i
}

// EnvelopeAddress splits an envelope sender (MAIL FROM, without the angle
// brackets): a source route goes, the domain is what follows the last "@".
// An address without one — postfix appends @$myorigin to such a sender of
// local mail — has the domain hostname.
func EnvelopeAddress(sender, hostname string) Address {
	sender = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(sender), "<"), ">"))
	if strings.HasPrefix(sender, "@") {
		if i := strings.IndexByte(sender, ':'); i >= 0 {
			sender = sender[i+1:]
		}
	}
	i := strings.LastIndexByte(sender, '@')
	if i < 0 {
		return Address{Local: unquote(sender), Domain: hostname}
	}
	d, _ := NormalizeDomain(sender[i+1:])
	if d == "" {
		d = hostname
	}
	return Address{Local: unquote(sender[:i]), Domain: d}
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\`, "")
	}
	return s
}

// domainByte is a byte a domain label can hold for a lenient parser:
// letters, digits, hyphens, underscores and UTF-8 (IDNs).
func domainByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c >= 0x80
}

// label reads a run of domain bytes from i.
func label(s string, i int) (string, int) {
	j := i
	for j < len(s) && domainByte(s[j]) {
		j++
	}
	return s[i:j], j
}

// scanDomain reads the domain that starts at i (right after an "@"):
// labels joined by dots, blanks and comments allowed around each; dots
// before the first label are skipped (a parser that drops what it cannot
// read may read ".b.example" as b.example). A domain literal ([192.0.2.1])
// is no domain.
func scanDomain(s string, i int, ends []int32) string {
	i = skipCFWS(s, i, ends)
	for i < len(s) && s[i] == '.' {
		i = skipCFWS(s, i+1, ends)
	}
	if i < len(s) && s[i] == '[' {
		return ""
	}
	first, j := label(s, i)
	if first == "" {
		return ""
	}
	labels := []string{first}
	for {
		k := skipCFWS(s, j, ends)
		if k >= len(s) || s[k] != '.' {
			break
		}
		next, m := label(s, skipCFWS(s, k+1, ends))
		if next == "" {
			break
		}
		labels = append(labels, next)
		j = m
	}
	return strings.Join(labels, ".")
}

// localByte is a byte of an unquoted local part (RFC 5322 atext and dots).
func localByte(c byte) bool {
	return domainByte(c) || strings.IndexByte("!#$%&'*+/=?^`{|}~.", c) >= 0
}

// scanLocal reads the local part that ends right before the "@" at at: a
// quoted string or a run of atext, no longer than maxLocalScan. It is ""
// when there is none that can be told.
func scanLocal(s string, at int) string {
	j := at
	stop := max(0, at-maxLocalScan)
	if j > 0 && s[j-1] == '"' {
		for k := j - 2; k >= stop; k-- {
			if s[k] == '"' && (k == 0 || s[k-1] != '\\') {
				return strings.ReplaceAll(s[k+1:j-1], `\`, "")
			}
		}
		return ""
	}
	k := j
	for k > stop && localByte(s[k-1]) {
		k--
	}
	if k == stop && stop > 0 && localByte(s[k-1]) {
		// Longer than a local part can be.
		return ""
	}
	return s[k:j]
}
