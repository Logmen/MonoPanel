package parity

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The web UI is read here far enough to find where it calls the API: the
// api(), apiText() and apiPutRaw() helpers of $lib/api (paths relative to
// /api/v1), fetch() and EventSource on /api/v1, and plain /api/v1 URLs
// (downloads, links). It is not a JavaScript parser: it knows strings,
// template literals, comments, regular expressions and brackets, which is
// what reading a call's path and method takes, and in Svelte files it reads
// the <script> blocks, the {…} expressions and the attribute values of the
// markup.

type tokKind uint8

const (
	tIdent tokKind = iota + 1
	tStr           // '…' or "…": text is the content as written
	tTmpl          // `…`: text is the static text, a hole mark per ${…}
	tNum
	tPunct
	tRegex
)

// hole marks a ${…} (or any other dynamic part) inside a path template.
const hole = "\x00"

type token struct {
	kind tokKind
	text string
	pos  int       // byte offset in the file
	subs [][]token // the expressions of a template's ${…}
}

func (t token) is(p string) bool { return t.kind == tPunct && t.text == p }

func (t token) literal() bool { return t.kind == tStr || t.kind == tTmpl }

type lexer struct {
	src      string
	i        int
	comments [][2]int // byte ranges of comments, blanked in webFile.code
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// puncts are the operators made of several characters, longest first, so
// that "?." and "??" are not taken for a ternary and "+=" not for a "+".
var puncts = []string{">>>=", "===", "!==", "...", "**=", "<<=", ">>=", ">>>", "??=", "||=", "&&=",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "**", "<<", ">>"}

// regexKeywords may stand right before a regular expression literal.
var regexKeywords = map[string]bool{"return": true, "typeof": true, "instanceof": true, "in": true, "of": true, "new": true,
	"delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true, "yield": true, "await": true}

// regexAllowed says whether a "/" after these tokens starts a regular
// expression rather than a division.
func regexAllowed(prev []token) bool {
	if len(prev) == 0 {
		return true
	}
	t := prev[len(prev)-1]
	switch t.kind {
	case tPunct:
		return t.text != ")" && t.text != "]" && t.text != "}"
	case tIdent:
		return regexKeywords[t.text]
	}
	return false
}

// tokens lexes up to end. With inBrace it stops after the "}" that closes
// the enclosing "{" (a template hole or a Svelte expression).
func (lx *lexer) tokens(inBrace bool, end int) []token {
	var out []token
	depth := 0
	for lx.i < end {
		c := lx.src[lx.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			lx.i++
		case c == '/' && lx.at(1) == '/':
			start := lx.i
			for lx.i < end && lx.src[lx.i] != '\n' {
				lx.i++
			}
			lx.comments = append(lx.comments, [2]int{start, lx.i})
		case c == '/' && lx.at(1) == '*':
			start := lx.i
			if k := strings.Index(lx.src[min(lx.i+2, end):end], "*/"); k >= 0 {
				lx.i += 2 + k + 2
			} else {
				lx.i = end
			}
			lx.comments = append(lx.comments, [2]int{start, lx.i})
		case c == '\'' || c == '"':
			out = append(out, lx.str(c, end))
		case c == '`':
			out = append(out, lx.tmpl(end))
		case c == '/' && regexAllowed(out):
			out = append(out, lx.regex(end))
		case isDigit(c):
			start := lx.i
			for lx.i < end && (isIdentByte(lx.src[lx.i]) || lx.src[lx.i] == '.') {
				lx.i++
			}
			out = append(out, token{kind: tNum, text: lx.src[start:lx.i], pos: start})
		case isIdentByte(c):
			start := lx.i
			for lx.i < end && isIdentByte(lx.src[lx.i]) {
				lx.i++
			}
			out = append(out, token{kind: tIdent, text: lx.src[start:lx.i], pos: start})
		case c == '{':
			depth++
			out = append(out, token{kind: tPunct, text: "{", pos: lx.i})
			lx.i++
		case c == '}':
			if depth == 0 && inBrace {
				lx.i++
				return out
			}
			depth--
			out = append(out, token{kind: tPunct, text: "}", pos: lx.i})
			lx.i++
		default:
			p := string(c)
			for _, m := range puncts {
				if strings.HasPrefix(lx.src[lx.i:end], m) {
					p = m
					break
				}
			}
			out = append(out, token{kind: tPunct, text: p, pos: lx.i})
			lx.i += len(p)
		}
	}
	return out
}

func (lx *lexer) at(k int) byte {
	if lx.i+k < len(lx.src) {
		return lx.src[lx.i+k]
	}
	return 0
}

// str reads a quoted string. A string cannot span lines, so an unterminated
// one ends at the line end instead of swallowing the rest of the file.
func (lx *lexer) str(q byte, end int) token {
	start := lx.i
	lx.i++
	var b strings.Builder
	for lx.i < end {
		c := lx.src[lx.i]
		if c == '\\' && lx.i+1 < end {
			b.WriteByte(lx.src[lx.i+1])
			lx.i += 2
			continue
		}
		lx.i++
		if c == q || c == '\n' {
			break
		}
		b.WriteByte(c)
	}
	return token{kind: tStr, text: b.String(), pos: start}
}

func (lx *lexer) tmpl(end int) token {
	t := token{kind: tTmpl, pos: lx.i}
	lx.i++
	var b strings.Builder
	for lx.i < end {
		c := lx.src[lx.i]
		switch {
		case c == '\\' && lx.i+1 < end:
			b.WriteByte(lx.src[lx.i+1])
			lx.i += 2
		case c == '`':
			lx.i++
			t.text = b.String()
			return t
		case c == '$' && lx.at(1) == '{':
			lx.i += 2
			t.subs = append(t.subs, lx.tokens(true, end))
			b.WriteString(hole)
		default:
			b.WriteByte(c)
			lx.i++
		}
	}
	t.text = b.String()
	return t
}

func (lx *lexer) regex(end int) token {
	start := lx.i
	lx.i++
	class := false
	for lx.i < end {
		c := lx.src[lx.i]
		if c == '\n' {
			break
		}
		lx.i++
		switch {
		case c == '\\' && lx.i < end:
			lx.i++ // the escaped character
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			for lx.i < end && isIdentByte(lx.src[lx.i]) {
				lx.i++
			}
			return token{kind: tRegex, text: lx.src[start:lx.i], pos: start}
		}
	}
	return token{kind: tRegex, text: lx.src[start:lx.i], pos: start}
}

// A stream is one run of code in a file: a TypeScript module, a <script>
// block, a {…} expression of the markup or a quoted attribute value.
type stream struct {
	toks     []token
	kind     streamKind
	from, to int // the bytes it covers; an expression's with its braces
}

type streamKind uint8

const (
	sScript streamKind = iota + 1 // a module or a <script> block
	sExpr                         // a {…} of the markup: {x}, {#if x}, onclick={…}
	sAttr                         // a quoted attribute value: href="/api/v1/{x}"
)

// A closer is the {/if}, {/each}… that ends a block of the markup.
type closer struct {
	pos  int    // its "{"
	word string // if, each, await, key, snippet
}

// streams lexes a whole file into token streams: a TypeScript module is one
// stream; a Svelte component gives one per <script> block, per {…}
// expression and per attribute value of the markup (as a template, so that
// href="/api/v1/…{x}" reads like `/api/v1/…${x}`), and the closers of its
// blocks. blank lists the byte ranges that are not code (comments, styles).
func streams(name, src string) (out []stream, closers []closer, blank [][2]int) {
	if !strings.HasSuffix(name, ".svelte") {
		lx := &lexer{src: src}
		out = append(out, stream{toks: lx.tokens(false, len(src)), kind: sScript, to: len(src)})
		return out, nil, lx.comments
	}
	lower := strings.ToLower(src)
	i := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "<!--"):
			k := strings.Index(src[i:], "-->")
			if k < 0 {
				k = len(src) - i - 3
			}
			blank = append(blank, [2]int{i, i + k + 3})
			i += k + 3
		case tagAt(lower, i, "script"), tagAt(lower, i, "style"):
			name := "script"
			if tagAt(lower, i, "style") {
				name = "style"
			}
			open := tagEnd(src, i)
			closeAt := strings.Index(lower[open:], "</"+name)
			if closeAt < 0 {
				closeAt = len(src) - open
			}
			if name == "script" {
				lx := &lexer{src: src, i: open}
				out = append(out, stream{toks: lx.tokens(false, open+closeAt), kind: sScript, from: open, to: open + closeAt})
				blank = append(blank, lx.comments...)
			} else {
				blank = append(blank, [2]int{open, open + closeAt})
			}
			i = open + closeAt
		case src[i] == '{':
			lx := &lexer{src: src, i: i + 1}
			if lx.at(0) == '/' && lx.at(1) != '*' && lx.at(1) != '/' {
				// {/if}, {/each}: the end of a block, not an expression.
				if k := strings.IndexByte(src[i:], '}'); k >= 0 {
					word, _, _ := strings.Cut(strings.TrimSpace(src[i+2:i+k]), " ")
					closers = append(closers, closer{pos: i, word: word})
					i += k + 1
					continue
				}
			}
			toks := lx.tokens(true, len(src))
			out = append(out, stream{toks: toks, kind: sExpr, from: i, to: lx.i})
			blank = append(blank, lx.comments...)
			i = lx.i
		case (src[i] == '"' || src[i] == '\'') && i > 0 && src[i-1] == '=':
			t, end := attrValue(src, i)
			out = append(out, stream{toks: []token{t}, kind: sAttr, from: i, to: end})
			i = end
		default:
			i++
		}
	}
	return out, closers, blank
}

// tagAt says whether an opening tag of that name starts at i.
func tagAt(lower string, i int, name string) bool {
	if !strings.HasPrefix(lower[i:], "<"+name) {
		return false
	}
	j := i + 1 + len(name)
	return j < len(lower) && (lower[j] == '>' || lower[j] == ' ' || lower[j] == '\t' || lower[j] == '\n' || lower[j] == '\r')
}

// tagEnd is the offset after the ">" of the tag starting at i; quoted
// attribute values may hold a ">".
func tagEnd(src string, i int) int {
	var q byte
	for ; i < len(src); i++ {
		c := src[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '>':
			return i + 1
		}
	}
	return len(src)
}

// attrValue reads a quoted attribute value of the markup as a template: the
// {…} expressions inside are holes with their own tokens.
func attrValue(src string, i int) (token, int) {
	q := src[i]
	t := token{kind: tTmpl, pos: i}
	var b strings.Builder
	j := i + 1
	for j < len(src) && src[j] != q {
		if src[j] == '{' {
			lx := &lexer{src: src, i: j + 1}
			t.subs = append(t.subs, lx.tokens(true, len(src)))
			b.WriteString(hole)
			j = lx.i
			continue
		}
		b.WriteByte(src[j])
		j++
	}
	t.text = b.String()
	return t, j + 1
}

// A webCall is one place where the web UI reaches the API.
type webCall struct {
	file   string   // relative to web/src
	line   int      //
	method string   // GET, POST…; "" when the code does not say which
	segs   []string // the path below /api/v1 split at "/"; "" is a dynamic segment
	via    string   // api, apiText, apiPutRaw, fetch, EventSource, url
	text   string   // the path as written, for messages
	pos    int      // byte offset of the call (of the URL for a link): webbody.go reads its arguments there
}

func (c webCall) where() string { return c.file + ":" + strconv.Itoa(c.line) }

func (c webCall) String() string {
	m := c.method
	if m == "" {
		m = "?"
	}
	return c.where() + " " + m + " " + segsString(c.segs) + " (" + c.via + " " + shorten(c.text) + ")"
}

// segsString writes a path with "{…}" for its dynamic segments.
func segsString(segs []string) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		if s == "" {
			b.WriteString("{…}")
		} else {
			b.WriteString(s)
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 90 {
		return s[:87] + "…"
	}
	return s
}

// A webFile is what the checks need to know about one source file.
type webFile struct {
	path  string       // relative to web/src
	calls []webCall    // calls whose path could be read
	paths []webCall    // every path-like expression, whatever uses it (method "")
	vague []webCall    // calls whose path the scanner could not read
	strs  []string     // the content of every string literal
	words []string     // identifiers, plain-word strings and query keys: where body fields show up
	code  string       // the source with comments and styles blanked
	lines []int        // offsets of line starts
	seen  map[int]bool // literals a fetch() or EventSource took

	streams []stream // the code, for the checks that follow values (webflow.go)
	closers []closer // the ends of the markup's blocks
	flow    *flow    // built on first use
}

func (f *webFile) line(pos int) int {
	return sort.SearchInts(f.lines, pos+1)
}

func (f *webFile) hasString(s string) bool { return slices.Contains(f.strs, s) }

func (f *webFile) hasWord(w string) bool {
	_, ok := slices.BinarySearch(f.words, w)
	return ok
}

var (
	plainWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	queryKey  = regexp.MustCompile(`(?:^|[?&])([A-Za-z_][A-Za-z0-9_]*)=`)
)

// parseWebFile reads one file of the web UI.
func parseWebFile(rel, src string) *webFile {
	f := &webFile{path: rel, seen: map[int]bool{}}
	f.lines = append(f.lines, 0)
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			f.lines = append(f.lines, i+1)
		}
	}
	ss, closers, blank := streams(rel, src)
	f.streams, f.closers = ss, closers
	code := []byte(src)
	for _, r := range blank {
		for i := r[0]; i < r[1] && i < len(code); i++ {
			if code[i] != '\n' {
				code[i] = ' '
			}
		}
	}
	f.code = string(code)
	words := map[string]bool{}
	for _, s := range ss {
		f.walk(s.toks, words)
	}
	for w := range words {
		f.words = append(f.words, w)
	}
	sort.Strings(f.words)
	return f
}

// walk finds the calls and literals of one token stream (and of the
// expressions nested in its templates).
func (f *webFile) walk(toks []token, words map[string]bool) {
	skip := -1 // the end of an import or a type declaration: its names are no fields
	for i, t := range toks {
		prev := token{}
		if i > 0 {
			prev = toks[i-1]
		}
		if i > skip && t.kind == tIdent && !prev.is(".") && !prev.is("?.") {
			skip = max(skip, declEnd(toks, i))
		}
		if i <= skip {
			continue
		}
		switch t.kind {
		case tTmpl, tStr:
			if !strings.Contains(t.text, hole) {
				f.strs = append(f.strs, t.text)
				if plainWord.MatchString(t.text) {
					words[t.text] = true
				}
			}
			for _, m := range queryKey.FindAllStringSubmatch(t.text, -1) {
				words[m[1]] = true
			}
			for _, s := range t.subs {
				f.walk(s, words)
			}
			if !strings.HasPrefix(t.text, "/") || prev.is("+") {
				continue // not a path, or the tail of one that started earlier
			}
			expr := chain(toks, i)
			text := tokensText(expr)
			for _, segs := range templates(expr) {
				if len(segs) > 0 {
					f.paths = append(f.paths, webCall{file: f.path, line: f.line(t.pos), segs: segs, via: "literal", text: text})
				}
				// A /api/v1 URL the browser opens itself (a download, a
				// link) is a GET; fetch() and EventSource took theirs.
				if len(segs) > 2 && segs[0] == "api" && segs[1] == "v1" && !f.seen[t.pos] {
					f.calls = append(f.calls, webCall{file: f.path, line: f.line(t.pos), method: "GET", segs: segs[2:], via: "url", text: text, pos: t.pos})
				}
			}
			continue
		case tIdent:
			// A name that is called (api(…), api<T>(…)) is a function, not
			// a field.
			j := i + 1
			if j < len(toks) && toks[j].is("<") {
				j = skipAngles(toks, j)
			}
			if j >= len(toks) || !toks[j].is("(") {
				words[t.text] = true
			}
		default:
			continue
		}
		if prev.is(".") || prev.is("?.") || prev.kind == tIdent && prev.text == "function" {
			continue
		}
		var via string
		switch t.text {
		case "api", "apiText", "apiPutRaw", "fetch":
			via = t.text
		case "EventSource":
			if prev.kind == tIdent && prev.text == "new" {
				via = t.text
			}
		}
		if via == "" {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].is("<") {
			j = skipAngles(toks, j)
		}
		if j >= len(toks) || !toks[j].is("(") {
			continue
		}
		args := callArgs(toks, j)
		if len(args) == 0 {
			continue
		}
		methods := []string{"GET"}
		switch via {
		case "apiPutRaw":
			methods = []string{"PUT"}
		case "api", "fetch":
			if len(args) > 1 {
				methods = methodOf(args[1])
			}
		}
		var paths [][]string
		for _, p := range templates(args[0]) {
			if via == "fetch" || via == "EventSource" {
				// Only /api/v1 URLs are the panel's API; the rest is not ours.
				if len(p) < 2 || p[0] != "api" || p[1] != "v1" {
					continue
				}
				p = p[2:]
			}
			if len(p) > 0 {
				paths = append(paths, p)
			}
		}
		if via == "fetch" || via == "EventSource" {
			for _, a := range args[0] {
				if a.literal() {
					f.seen[a.pos] = true
				}
			}
			if len(paths) == 0 {
				continue // the wrapper in $lib/api, or someone else's URL
			}
		}
		text := tokensText(args[0])
		for _, method := range methods {
			if len(paths) == 0 {
				f.vague = append(f.vague, webCall{file: f.path, line: f.line(t.pos), method: method, via: via, text: text, pos: t.pos})
			}
			for _, p := range paths {
				f.calls = append(f.calls, webCall{file: f.path, line: f.line(t.pos), method: method, segs: p, via: via, text: text, pos: t.pos})
			}
		}
	}
}

// declEnd is the index of the last token of the import statement or the
// TypeScript type declaration (type X = …, interface X {…}) that starts at i,
// or -1. Their names are no request fields: a type that mirrors a response
// (Account with quota_mb) says nothing about what a form sends.
func declEnd(toks []token, i int) int {
	next := func(k int) token {
		if k < len(toks) {
			return toks[k]
		}
		return token{}
	}
	switch t := toks[i]; {
	case t.text == "import" && !next(i+1).is("(") && !next(i+1).is("."):
		k := i + 1
		for k < len(toks) && toks[k].kind != tStr && !toks[k].is(";") {
			k++
		}
		return k
	case t.text == "type" && next(i+1).kind == tIdent && (next(i+2).is("=") || next(i+2).is("<")):
		depth := 0
		for k := i + 2; k < len(toks); k++ {
			x := toks[k]
			switch {
			case x.is("{") || x.is("(") || x.is("[") || x.is("<"):
				depth++
			case x.is("}") || x.is(")") || x.is("]") || x.is(">"):
				depth--
			case x.is(">>"): // Map<string, Array<number>>
				depth -= 2
			case x.is(">>>"):
				depth -= 3
			case depth == 0 && x.is(";"):
				return k
			case depth == 0 && k > i+3 && x.kind == tIdent && statementStart[x.text] && !toks[k-1].is("|") && !toks[k-1].is("&") && !toks[k-1].is("="):
				return k - 1
			}
		}
		return len(toks) - 1
	case t.text == "interface" && next(i+1).kind == tIdent:
		for k := i + 2; k < len(toks); k++ {
			if toks[k].is("{") {
				return group(toks, k) - 1
			}
		}
	}
	return -1
}

// statementStart are the words that begin the statement after a type alias
// written without a semicolon.
var statementStart = map[string]bool{"export": true, "const": true, "let": true, "var": true, "function": true, "type": true,
	"interface": true, "class": true, "import": true, "async": true, "await": true, "if": true, "for": true, "return": true}

// skipAngles skips TypeScript type arguments (api<Site[]>(…)) starting at
// the "<" at j and returns the index after the matching ">".
func skipAngles(toks []token, j int) int {
	depth := 0
	for ; j < len(toks); j++ {
		t := toks[j]
		switch {
		case t.is("<"):
			depth++
		case t.is(">"):
			depth--
		case t.is(">>"):
			depth -= 2
		case t.is(">>>"):
			depth -= 3
		case t.is(";") || t.is(")") || t.is("]") || t.is("}"):
			return j
		case t.is("(") || t.is("[") || t.is("{"):
			if depth <= 0 {
				return j
			}
			// An object type ({ a: string; b?: number }), a tuple, the
			// parameters of a function type: brackets inside the angles.
			j = groupEnd(toks, j)
			continue
		}
		if depth <= 0 {
			return j + 1
		}
	}
	return j
}

// groupEnd is the index of the bracket that closes the one at j (the last
// token when it is never closed).
func groupEnd(toks []token, j int) int {
	depth := 0
	for k := j; k < len(toks); k++ {
		switch {
		case toks[k].is("(") || toks[k].is("[") || toks[k].is("{"):
			depth++
		case toks[k].is(")") || toks[k].is("]") || toks[k].is("}"):
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return len(toks) - 1
}

// callArgs splits the arguments of the call whose "(" is at j.
func callArgs(toks []token, j int) [][]token {
	var args [][]token
	var cur []token
	depth := 0
	for k := j + 1; k < len(toks); k++ {
		t := toks[k]
		if t.kind == tPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					if len(cur) > 0 {
						args = append(args, cur)
					}
					return args
				}
				depth--
			case ",":
				if depth == 0 {
					args = append(args, cur)
					cur = nil
					continue
				}
			}
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		args = append(args, cur)
	}
	return args
}

// methodOf reads the HTTP methods from a fetch init or api() options object:
// GET when it names none, every branch of a choice between literals
// (method: editing ? 'PATCH' : 'POST'), and "" (any method) when the code
// computes it otherwise.
func methodOf(arg []token) []string {
	unknown := []string{""}
	if len(arg) == 0 || !arg[0].is("{") {
		return unknown // options built elsewhere
	}
	depth := 0
	for k, t := range arg {
		switch {
		case t.is("{") || t.is("(") || t.is("["):
			depth++
		case t.is("}") || t.is(")") || t.is("]"):
			depth--
		case t.is("..."):
			if depth == 1 {
				return unknown // the method may come with the spread object
			}
		case depth == 1 && t.kind == tIdent && t.text == "method" && k > 0 && (arg[k-1].is("{") || arg[k-1].is(",")):
			if k+1 >= len(arg) || !arg[k+1].is(":") {
				return unknown // { method } from a variable
			}
			return choices(propertyValue(arg, k+2))
		}
	}
	return []string{"GET"}
}

// propertyValue is the value of an object literal's property that starts at
// k: the tokens up to the "," or "}" that ends it.
func propertyValue(toks []token, k int) []token {
	depth := 0
	for end := k; end < len(toks); end++ {
		switch t := toks[end]; {
		case t.is("{") || t.is("(") || t.is("["):
			depth++
		case (t.is("}") || t.is(")") || t.is("]")) && depth > 0:
			depth--
		case depth == 0 && (t.is(",") || t.is("}") || t.is(")") || t.is("]")):
			return toks[k:end]
		}
	}
	return toks[k:]
}

// choices are the upper-cased string literals an expression evaluates to:
// one literal, or the branches of a (nested) ternary whose branches are all
// literals; otherwise "" for a method the code computes.
func choices(expr []token) []string {
	parts := splitTop(expr, "?", ":")
	// A part followed by "?" is a condition; the others are the branches.
	var out []string
	seps := 0
	for i, part := range parts {
		cond := false
		if i < len(parts)-1 {
			// The separator after part i is the i-th "?" or ":" at the top.
			cond = topSep(expr, seps) == "?"
			seps++
		}
		if cond {
			continue
		}
		if len(part) != 1 || !part[0].literal() || strings.Contains(part[0].text, hole) || part[0].text == "" {
			return []string{""}
		}
		if m := strings.ToUpper(part[0].text); !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// topSep is the n-th "?" or ":" outside brackets in toks.
func topSep(toks []token, n int) string {
	depth := 0
	for _, t := range toks {
		switch {
		case t.is("(") || t.is("[") || t.is("{"):
			depth++
		case t.is(")") || t.is("]") || t.is("}"):
			depth--
		case depth == 0 && (t.is("?") || t.is(":")):
			if n == 0 {
				return t.text
			}
			n--
		}
	}
	return ""
}

// chain is the expression starting at the literal toks[i]: the literal and
// the terms joined to it with "+".
func chain(toks []token, i int) []token {
	end := i + 1
	for end+1 < len(toks) && toks[end].is("+") {
		k := term(toks, end+1)
		if k == end+1 {
			break
		}
		end = k
	}
	return toks[i:end]
}

// term is the end of the operand starting at k: a literal, a name or a
// bracketed group, with what follows it (.x, ?.x, [i], (args)).
func term(toks []token, k int) int {
	if k >= len(toks) {
		return k
	}
	switch t := toks[k]; {
	case t.is("(") || t.is("["):
		k = group(toks, k)
	case t.kind == tIdent || t.kind == tNum || t.literal():
		k++
	default:
		return k
	}
	for k < len(toks) {
		switch t := toks[k]; {
		case t.is(".") || t.is("?."):
			k += 2
		case t.is("(") || t.is("["):
			k = group(toks, k)
		default:
			return k
		}
	}
	return k
}

// group is the index after the bracket that closes the one at k.
func group(toks []token, k int) int {
	depth := 0
	for ; k < len(toks); k++ {
		switch t := toks[k]; {
		case t.is("(") || t.is("[") || t.is("{"):
			depth++
		case t.is(")") || t.is("]") || t.is("}"):
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return k
}

// templates turns a path expression into the paths it can produce: every
// branch of a ternary or of a || that starts with a "/" literal, its parts
// joined with "+", the query string dropped, dynamic parts as "" segments.
func templates(expr []token) [][]string {
	var out [][]string
	for _, branch := range splitTop(expr, "?", ":", "||", "??", "&&") {
		if len(branch) >= 2 && branch[0].is("(") && closes(branch) {
			out = append(out, templates(branch[1:len(branch)-1])...)
			continue
		}
		terms := splitTop(branch, "+")
		if len(terms) == 0 || len(terms[0]) != 1 || !terms[0][0].literal() || !strings.HasPrefix(terms[0][0].text, "/") {
			continue
		}
		var b strings.Builder
		for _, term := range terms {
			if len(term) == 1 && term[0].literal() {
				b.WriteString(term[0].text)
			} else {
				b.WriteString(hole)
			}
		}
		out = append(out, pathSegs(b.String())...)
	}
	return out
}

// closes says whether the "(" at the start of toks closes at its end.
func closes(toks []token) bool {
	depth := 0
	for k, t := range toks {
		switch {
		case t.is("(") || t.is("[") || t.is("{"):
			depth++
		case t.is(")") || t.is("]") || t.is("}"):
			depth--
			if depth == 0 && k != len(toks)-1 {
				return false
			}
		}
	}
	return depth == 0
}

// splitTop splits at the given punctuation outside brackets.
func splitTop(toks []token, seps ...string) [][]token {
	var out [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		if t.kind == tPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			if depth == 0 && slices.Contains(seps, t.text) {
				out = append(out, cur)
				cur = nil
				continue
			}
		}
		cur = append(cur, t)
	}
	return append(out, cur)
}

// pathSegs splits a path template into segments. A segment with a hole is
// dynamic, except that a hole right after the last segment's own text may be
// the query string (`/jobs${query}`), so that reading is offered as well.
func pathSegs(s string) [][]string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	var segs []string
	for _, p := range strings.Split(s, "/") {
		switch {
		case p == "":
		case strings.Contains(p, hole):
			segs = append(segs, "")
		default:
			segs = append(segs, p)
		}
	}
	out := [][]string{segs}
	parts := strings.Split(s, "/")
	if last := parts[len(parts)-1]; len(segs) > 0 && segs[len(segs)-1] == "" && !strings.HasPrefix(last, hole) && strings.Contains(last, hole) {
		alt := append(slices.Clone(segs[:len(segs)-1]), last[:strings.Index(last, hole)])
		out = append(out, alt)
	}
	return out
}

func tokensText(toks []token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && (t.kind == tIdent || t.kind == tNum) && (toks[i-1].kind == tIdent || toks[i-1].kind == tNum) {
			b.WriteByte(' ')
		}
		switch t.kind {
		case tStr:
			b.WriteString("'" + t.text + "'")
		case tTmpl:
			b.WriteString("`" + strings.ReplaceAll(t.text, hole, "${…}") + "`")
		default:
			b.WriteString(t.text)
		}
	}
	return b.String()
}

// webSources reads every Svelte component and TypeScript module under dir
// (web/src), keyed by the path relative to it.
func webSources(dir string) (map[string]*webFile, error) {
	files := map[string]*webFile{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch path.Ext(p) {
		case ".svelte", ".ts", ".js":
		default:
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files[rel] = parseWebFile(rel, string(b))
		return nil
	})
	return files, err
}
