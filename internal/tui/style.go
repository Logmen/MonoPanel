package tui

import (
	"encoding/csv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"monopanel/internal/termsafe"
)

// The palette reads on dark and on light terminals alike: one teal accent,
// grey for secondary text, orange for warnings, red for failures.
var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#3FC9B8"))
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A98A4"))
	styleCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#0E7C73"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2A33A"))
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FC9B8"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5534B"))
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleHead   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#8A98A4"))
	styleCaret  = lipgloss.NewStyle().Reverse(true)
	styleBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#3FC9B8")).Padding(0, 1)
)

// tone colours a cell or a line by what it says.
type tone int

const (
	toneNone tone = iota
	toneOK
	toneWarn
	toneErr
	toneMuted
)

func (t tone) render(s string) string {
	switch t {
	case toneOK:
		return styleKey.Render(s)
	case toneWarn:
		return styleWarn.Render(s)
	case toneErr:
		return styleErr.Render(s)
	case toneMuted:
		return styleMuted.Render(s)
	}
	return s
}

// stateTone picks the tone of a state word: active/installed/valid are fine,
// failed/error are not, the rest is neutral.
func stateTone(state string) tone {
	switch state {
	case "active", "installed", "valid", "ok", "done", "running", "enabled", "on", "yes", "issued", "https", "success":
		return toneOK
	case "failed", "error", "fail", "inactive", "dead", "expired", "denied":
		return toneErr
	case "warn", "pending", "queued", "suspended", "activating", "deactivating", "reloading", "off", "disabled":
		return toneWarn
	}
	return toneNone
}

// textWidth is the number of terminal cells a plain or styled string takes.
func textWidth(s string) int { return lipgloss.Width(s) }

func runeWidth(r rune) int {
	if r < 0x300 {
		if r < 0x20 || r == 0x7f {
			return 0
		}
		return 1
	}
	return lipgloss.Width(string(r))
}

// truncate shortens plain text to w cells, ending it with an ellipsis.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if textWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

// pad fills plain or styled text with spaces up to w cells.
func pad(s string, w int) string {
	if n := w - textWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// fit truncates and pads plain text to exactly w cells.
func fit(s string, w int) string { return pad(truncate(s, w), w) }

// clip cuts a styled line at w cells without breaking escape sequences.
func clip(line string, w int) string {
	if textWidth(line) <= w {
		return line
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(line)
}

// wrap breaks plain text into lines of at most w cells, at spaces where it
// can and inside long words where it must.
func wrap(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			for textWidth(word) > w {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				head, rest := splitAtWidth(word, w)
				out = append(out, head)
				word = rest
			}
			switch {
			case line == "":
				line = word
			case textWidth(line)+1+textWidth(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}

// wrapKeep breaks preformatted text — a command's output, an example in a
// help text — without touching its spaces: a line that fits stays as it is,
// so the columns of a table stay aligned; a longer one breaks at its last
// space that fits (inside a word when there is none), and the rest keeps the
// line's indentation.
func wrapKeep(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		// Trailing spaces (a table's padding) show nothing and would only
		// wrap into blank lines.
		line = strings.TrimRight(line, " ")
		indent := ""
		for line != "" && textWidth(line) > w {
			head, rest := splitAtWidth(line, w)
			if i := strings.LastIndexByte(head, ' '); i > len(indent) && strings.TrimSpace(head[:i]) != "" {
				head, rest = head[:i], head[i+1:]+rest
			}
			out = append(out, head)
			if indent == "" {
				indent = line[:len(line)-len(strings.TrimLeft(line, " "))]
				if textWidth(indent) > w/2 {
					indent = ""
				}
			}
			line = indent + strings.TrimLeft(rest, " ")
		}
		out = append(out, line)
	}
	return out
}

func splitAtWidth(s string, w int) (string, string) {
	used := 0
	for i, r := range s {
		rw := runeWidth(r)
		if used+rw > w {
			return s[:i], s[i:]
		}
		used += rw
	}
	return s, ""
}

// sanitize drops escape sequences and control characters a child process
// may print, so they cannot repaint the screen; tabs become spaces. The C1
// controls count too (U+009B is a CSI of its own), and bytes that are not
// UTF-8 become U+FFFD.
func sanitize(s string) string { return termsafe.Text(s) }

// safeStyled is sanitize for a line the interface has already styled: the
// SGR sequences (ESC [ … m: colours, bold) stay, every other escape sequence
// goes — cursor moves, OSC titles, hyperlinks and clipboard writes (OSC 52) —
// and so do the C1 controls and the C0 controls but tab. Whatever reaches a
// screen from the server cannot drive the terminal through it.
func safeStyled(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(utf8.RuneError))
	}
	control := func(r rune) bool { return (r < 0x20 && r != '\t') || (r >= 0x7f && r <= 0x9f) }
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
		case c == 0x1b && next == '[':
			// CSI: kept only when it is SGR (parameters, then "m").
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' && sgrParams(rs[i+2:j]) {
				b.WriteString(string(rs[i : j+1]))
			}
			i = j
		case c == 0x9b:
			for i++; i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e); i++ {
			}
		case c == 0x9d || (c == 0x1b && next == ']'):
			if c == 0x1b {
				i++
			}
			for i++; i < len(rs) && rs[i] != 0x07 && rs[i] != 0x9c && !(rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\'); i++ {
			}
			if i < len(rs) && rs[i] == 0x1b {
				i++
			}
		case c == 0x1b:
			i++
		case control(c):
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// sgrParams accepts the parameter bytes of an SGR sequence: digits, ";" and
// ":" only.
func sgrParams(p []rune) bool {
	for _, r := range p {
		if (r < '0' || r > '9') && r != ';' && r != ':' {
			return false
		}
	}
	return true
}

// shellQuote quotes one word for the command line shown to the user: the
// line can be copied into a shell as it is.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("@%+=:,./_-*", r)) {
			safe = false
			break
		}
	}
	if safe && !strings.HasPrefix(s, "*") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitWords splits a command line the way a shell would for simple cases:
// whitespace separates words, single and double quotes group them, a
// backslash escapes the next character.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errUnclosedQuote
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

type tuiError string

func (e tuiError) Error() string { return string(e) }

const errUnclosedQuote = tuiError("unclosed quote")

// splitList turns "a, b,,c" into [a b c]. An item in double quotes may hold
// commas — "KEY=a,b", c — the way mp's list flags read their value (CSV).
func splitList(s string) []string {
	r := csv.NewReader(strings.NewReader(s))
	r.TrimLeadingSpace, r.LazyQuotes = true, true
	parts, err := r.Read()
	if err != nil {
		// Not one CSV line (a line break inside, say): plain commas then.
		parts = strings.Split(s, ",")
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinList is the opposite of splitList: the items as one CSV line, quoted
// where an item has a comma, a quote or spaces around it.
func joinList(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		if strings.ContainsAny(it, ",\"\r\n") || strings.TrimSpace(it) != it {
			it = `"` + strings.ReplaceAll(it, `"`, `""`) + `"`
		}
		parts[i] = it
	}
	return strings.Join(parts, ",")
}

// splitPairs reads the items of a list typed into one field for a
// --cred-stdin style flag: commas or semicolons separate them, spaces around
// an item go, and an item in double quotes keeps whatever it holds ("" is a
// quote) — "PASS=a;b", USER=x. A quote inside an item is just a character.
func splitPairs(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, inQuotes, started := false, false, false
	end := func() {
		item := cur.String()
		if !quoted {
			item = strings.TrimSpace(item)
		}
		if item != "" {
			out = append(out, item)
		}
		cur.Reset()
		quoted, inQuotes, started = false, false, false
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case inQuotes && r == '"' && i+1 < len(rs) && rs[i+1] == '"':
			cur.WriteRune('"')
			i++
		case inQuotes && r == '"':
			inQuotes = false
		case inQuotes:
			cur.WriteRune(r)
		case r == ',' || r == ';':
			end()
		case !started && r == '"':
			quoted, inQuotes, started = true, true, true
		case !started && unicode.IsSpace(r), quoted && unicode.IsSpace(r):
			// Spaces before an item, or after its closing quote.
		default:
			started = true
			cur.WriteRune(r)
		}
	}
	end()
	return out
}

// joinPairs is the opposite of splitPairs: the items as one line, an item
// with a separator, a quote or spaces around it in double quotes.
func joinPairs(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if strings.ContainsAny(it, ",;\"") || strings.TrimSpace(it) != it {
			it = `"` + strings.ReplaceAll(it, `"`, `""`) + `"`
		}
		parts = append(parts, it)
	}
	return strings.Join(parts, ", ")
}

// firstLine is the first line of a or, when a is empty, of b.
func firstLine(a, b string) string {
	s := a
	if s == "" {
		s = b
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// sparkline draws values as a row of block characters.
func sparkline(vals []float64, w int) string {
	if len(vals) == 0 || w <= 0 {
		return ""
	}
	if len(vals) > w {
		// Average neighbouring points down to the available width.
		out := make([]float64, w)
		for i := range out {
			from, to := i*len(vals)/w, (i+1)*len(vals)/w
			if to <= from {
				to = from + 1
			}
			sum := 0.0
			for _, v := range vals[from:to] {
				sum += v
			}
			out[i] = sum / float64(to-from)
		}
		vals = out
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = min(lo, v), max(hi, v)
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if hi > lo {
			i = int((v - lo) / (hi - lo) * float64(len(blocks)-1))
		}
		b.WriteRune(blocks[i])
	}
	return b.String()
}
