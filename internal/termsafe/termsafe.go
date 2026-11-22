// Package termsafe makes text from the server safe to print on a terminal.
// Names, comments, job messages, log lines and audit entries come from
// hosting customers, from strangers at the sign-in form and from migration
// sources; printed as they are, an escape sequence in them would move the
// cursor, hide or fake output, retitle the window or write the clipboard
// (OSC 52) of the administrator who reads them.
package termsafe

import (
	"strings"
	"unicode/utf8"
)

// Text drops every escape sequence (CSI, OSC up to BEL or ST, ESC and the
// character after it, their C1 forms) and every control character but tab
// and newline; tabs become four spaces, bytes that are not UTF-8 U+FFFD.
func Text(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(utf8.RuneError))
	}
	control := func(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }
	if !strings.ContainsFunc(s, control) {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch {
		case c == 0x9b || (c == 0x1b && next == '['):
			// CSI: ESC [ … up to its final byte, which goes too.
			if c == 0x1b {
				i++
			}
			i++
			for i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e) {
				i++
			}
		case c == 0x9d || (c == 0x1b && next == ']'):
			// OSC: ESC ] … up to BEL, ST or ESC \.
			if c == 0x1b {
				i++
			}
			i++
			for i < len(rs) && rs[i] != 0x07 && rs[i] != 0x9c && !(rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\') {
				i++
			}
			if i < len(rs) && rs[i] == 0x1b {
				i++
			}
		case c == 0x1b:
			// ESC and the character after it.
			i++
		case c == '\t':
			b.WriteString("    ")
		case c == '\n':
			b.WriteRune(c)
		case control(c):
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Line is Text for a value printed on one line: a line break becomes a space.
func Line(s string) string {
	return strings.ReplaceAll(Text(s), "\n", " ")
}
