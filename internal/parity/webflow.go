package parity

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// The checks that follow values through the web UI's code — which fields a
// call's body can carry (webbody.go) and whether a call only administrators
// may make is behind a check of the role (webroles.go) — need more than the
// calls: where a name is declared, what a function returns, which statements
// an if guards, which components a page uses. This file gives them that
// structure. Like websrc.go it is no JavaScript parser: it reads the shapes
// the pages are written in (const, let, function, arrows, if/else,
// conditionals, object literals, Svelte's blocks, props and component tags),
// and its users say so where it cannot follow.

// A list is a token list with its brackets matched: a stream of the file or
// the expression in a template's ${…}.
type list struct {
	toks  []token
	match []int // the matching bracket of ( [ { and ) ] }; -1 when there is none
	up    []int // the innermost opening bracket around each token; -1 at the top
	file  *webFile
	top   bool       // a whole stream, not a template's ${…}
	kind  streamKind // of the stream
	from  int        // the bytes it covers
	to    int
}

func newList(f *webFile, toks []token) *list {
	l := &list{toks: toks, match: make([]int, len(toks)), up: make([]int, len(toks)), file: f}
	var stack []int
	for i, t := range toks {
		l.match[i], l.up[i] = -1, -1
		if n := len(stack); n > 0 {
			l.up[i] = stack[n-1]
		}
		switch {
		case t.is("(") || t.is("[") || t.is("{"):
			stack = append(stack, i)
		case t.is(")") || t.is("]") || t.is("}"):
			if n := len(stack); n > 0 && pairOf(toks[stack[n-1]].text) == t.text {
				o := stack[n-1]
				stack = stack[:n-1]
				l.match[o], l.match[i] = i, o
				l.up[i] = l.up[o]
			}
		}
	}
	if len(toks) > 0 {
		l.from, l.to = toks[0].pos, l.end(len(toks)-1)
	}
	return l
}

func pairOf(open string) string {
	switch open {
	case "(":
		return ")"
	case "[":
		return "]"
	}
	return "}"
}

func (l *list) tok(i int) token {
	if i >= 0 && i < len(l.toks) {
		return l.toks[i]
	}
	return token{}
}

func (l *list) word(i int, w string) bool {
	t := l.tok(i)
	return t.kind == tIdent && t.text == w
}

func (l *list) opens(i int) bool {
	t := l.tok(i)
	return t.is("(") || t.is("[") || t.is("{")
}

func (l *list) closesAt(i int) bool {
	t := l.tok(i)
	return t.is(")") || t.is("]") || t.is("}")
}

// member says whether the name at i is a property: x.name, x?.name.
func (l *list) member(i int) bool { return l.tok(i-1).is(".") || l.tok(i-1).is("?.") }

// closing is the index of the bracket that closes the one at i (the end of
// the list for one never closed).
func (l *list) closing(i int) int {
	if m := l.match[i]; m > i {
		return m
	}
	return len(l.toks)
}

// after is the index after the group that opens at i, or i+1.
func (l *list) after(i int) int {
	if l.opens(i) {
		return min(l.closing(i)+1, len(l.toks))
	}
	return i + 1
}

// end is the byte offset after token i.
func (l *list) end(i int) int {
	t := l.tok(i)
	switch {
	case t.kind == tStr:
		return t.pos + len(t.text) + 2
	case t.kind == tTmpl && i+1 < len(l.toks):
		return l.toks[i+1].pos
	case t.kind == tTmpl:
		return max(l.to, t.pos+len(t.text)+2)
	}
	return t.pos + len(t.text)
}

// newLine says whether token i starts a line after token i-1: where a
// statement may end without a semicolon.
func (l *list) newLine(i int) bool {
	if i <= 0 || i >= len(l.toks) {
		return false
	}
	return l.file.line(l.toks[i].pos) > l.file.line(l.toks[i-1].pos)
}

// stmtWords begin a statement: after a line break they end the previous one
// even without a semicolon.
var stmtWords = map[string]bool{"const": true, "let": true, "var": true, "if": true, "for": true, "while": true, "do": true,
	"return": true, "export": true, "import": true, "try": true, "switch": true, "throw": true, "class": true, "interface": true, "function": true}

// statementBreak says whether token k > s ends a statement or an
// expression that started at s without a semicolon.
func (l *list) statementBreak(s, k int, words ...string) bool {
	t := l.tok(k)
	if k <= s || t.kind != tIdent || l.member(k) || !l.newLine(k) {
		return false
	}
	if stmtWords[t.text] {
		return true
	}
	for _, w := range words {
		if t.text == w {
			return true
		}
	}
	return false
}

// generic skips the type arguments of a call (api<T>(…), $state<T>(…)) at
// the "<" at i: it returns the index of the "(" after them, or i when the
// "<" compares.
func (l *list) generic(i int) int {
	if !l.tok(i).is("<") {
		return i
	}
	if j := skipAngles(l.toks, i); l.tok(j).is("(") {
		return j
	}
	return i
}

// exprEnd is the last token of the expression that starts at s: it runs to
// a "," or ";", to the bracket that closes the one around it, or to a line
// that starts a statement.
func (l *list) exprEnd(s int) int {
	last := s - 1
	for k := s; k < len(l.toks); {
		t := l.toks[k]
		switch {
		case l.opens(k):
			last, k = l.after(k)-1, l.after(k)
			continue
		case l.closesAt(k) || t.is(";") || t.is(","):
			return last
		case l.statementBreak(s, k):
			return last
		case t.kind == tIdent && l.tok(k+1).is("<"):
			if j := l.generic(k + 1); j != k+1 {
				last, k = j-1, j
				continue
			}
		}
		last, k = k, k+1
	}
	return last
}

// stmtEnd is the last token of the statement that starts at s (at least s).
func (l *list) stmtEnd(s int) int {
	if s >= len(l.toks) {
		return len(l.toks) - 1
	}
	t := l.toks[s]
	switch {
	case t.is("{"):
		return l.after(s) - 1
	case t.is(";"):
		return s
	case t.kind != tIdent:
	case t.text == "if" && l.tok(s+1).is("("):
		e := l.stmtEnd(l.after(s + 1))
		if l.word(e+1, "else") {
			return l.stmtEnd(e + 2)
		}
		return e
	case (t.text == "for" || t.text == "while") && l.tok(s+1).is("("):
		return l.stmtEnd(l.after(s + 1))
	case t.text == "do":
		e := l.stmtEnd(s + 1)
		if l.word(e+1, "while") && l.tok(e+2).is("(") {
			e = l.after(e+2) - 1
			if l.tok(e + 1).is(";") {
				e++
			}
		}
		return e
	case t.text == "try" && l.tok(s+1).is("{"):
		e := l.after(s+1) - 1
		for {
			switch {
			case l.word(e+1, "catch") && l.tok(e+2).is("("):
				e = l.after(l.after(e+2)) - 1
			case (l.word(e+1, "catch") || l.word(e+1, "finally")) && l.tok(e+2).is("{"):
				e = l.after(e+2) - 1
			default:
				return e
			}
		}
	case t.text == "function" || t.text == "async" && l.word(s+1, "function"):
		if f := l.file.flow.fnAt(l, s); f != nil && f.block {
			return f.body[1]
		}
	}
	// An expression statement, return, throw, const …: up to its ";".
	last := s
	for k := s; k < len(l.toks); {
		switch {
		case l.opens(k):
			last, k = l.after(k)-1, l.after(k)
			continue
		case l.tok(k).is(";"):
			return k
		case l.closesAt(k):
			return max(last, s)
		case l.statementBreak(s, k, "else"):
			return last
		}
		last, k = k, k+1
	}
	return last
}

// nextStmt is the start of the statement after the one at k.
func (l *list) nextStmt(k int) int { return max(l.stmtEnd(k)+1, k+1) }

// exits says whether the statement s…e always leaves the function: return,
// throw, or a block that ends with one.
func (l *list) exits(s, e int) bool {
	switch {
	case l.word(s, "return") || l.word(s, "throw"):
		return true
	case l.tok(s).is("{") && e > s:
		last := -1
		for k := s + 1; k < e; k = l.nextStmt(k) {
			last = k
		}
		return last >= 0 && (l.word(last, "return") || l.word(last, "throw"))
	}
	return false
}

// operandBefore is the first token of the operand before the operator at
// i; loose are the operators that bind more loosely than this one and so
// end the operand.
func (l *list) operandBefore(i int, loose ...string) int {
	k := i - 1
	for k >= 0 {
		t := l.toks[k]
		if l.closesAt(k) && l.match[k] >= 0 && l.match[k] < k {
			k = l.match[k] - 1
			continue
		}
		stop := false
		switch {
		case t.kind == tPunct:
			switch t.text {
			case ",", ";", "=", "=>", ":", "?", "...", "(", "[", "{", "+=", "-=", "??=", "||=", "&&=":
				stop = true
			}
			for _, x := range loose {
				stop = stop || t.text == x
			}
		case t.kind == tIdent && !l.member(k):
			switch t.text {
			case "return", "throw", "case", "yield":
				stop = true
			}
		}
		if stop {
			break
		}
		k--
	}
	return k + 1
}

// operandAfter is the last token of the operand after the operator at i:
// up to a "," or ";", the bracket around it or one of stops.
func (l *list) operandAfter(i int, stops ...string) int {
	last := i
	for k := i + 1; k < len(l.toks); {
		t := l.toks[k]
		switch {
		case l.opens(k):
			last, k = l.after(k)-1, l.after(k)
			continue
		case l.closesAt(k) || t.is(",") || t.is(";"):
			return last
		case t.kind == tPunct:
			for _, s := range stops {
				if t.text == s {
					return last
				}
			}
		}
		last, k = k, k+1
	}
	return last
}

// question says whether the "?" at i starts the branches of a conditional:
// not the "?:" of an optional parameter or property, nor a "?)" of a type.
func (l *list) question(i int) bool {
	return l.tok(i).is("?") && !l.tok(i+1).is(":") && !l.tok(i+1).is(")") && !l.tok(i+1).is(",") && !l.tok(i+1).is("=")
}

// ternaryColon finds the ":" of the conditional whose "?" is at i, or -1.
func (l *list) ternaryColon(i int) int {
	nested := 0
	for k := i + 1; k < len(l.toks); {
		switch t := l.toks[k]; {
		case l.opens(k):
			k = l.after(k)
			continue
		case l.closesAt(k) || t.is(",") || t.is(";"):
			return -1
		case l.question(k):
			nested++
		case t.is(":"):
			if nested == 0 {
				return k
			}
			nested--
		}
		k++
	}
	return -1
}

// ternaryEnd is the last token of the branch after the ":" at c.
func (l *list) ternaryEnd(c int) int {
	nested, last := 0, c
	for k := c + 1; k < len(l.toks); {
		switch t := l.toks[k]; {
		case l.opens(k):
			last, k = l.after(k)-1, l.after(k)
			continue
		case l.closesAt(k) || t.is(",") || t.is(";"):
			return last
		case l.question(k):
			nested++
		case t.is(":"):
			if nested == 0 {
				return last
			}
			nested--
		}
		last, k = k, k+1
	}
	return last
}

// trim strips what does not change a value: brackets around it, a type
// assertion (x as T, x!), await.
func (l *list) trim(r [2]int) [2]int {
	for r[0] <= r[1] {
		switch {
		case l.tok(r[0]).is("(") && l.match[r[0]] == r[1]:
			r = [2]int{r[0] + 1, r[1] - 1}
		case l.word(r[0], "await"):
			r[0]++
		case l.tok(r[1]).is("!") && r[1] > r[0]:
			r[1]--
		default:
			for k := r[0] + 1; k <= r[1]; k = l.after(k) {
				if (l.word(k, "as") || l.word(k, "satisfies")) && !l.member(k) {
					return [2]int{r[0], k - 1}
				}
			}
			return r
		}
	}
	return r
}

// split splits a token range at an operator outside brackets.
func (l *list) split(r [2]int, op string) [][2]int {
	var out [][2]int
	start := r[0]
	for k := r[0]; k <= r[1]; {
		if l.tok(k).is(op) {
			out = append(out, [2]int{start, k - 1})
			start = k + 1
			k++
			continue
		}
		k = l.after(k)
	}
	return append(out, [2]int{start, r[1]})
}

// topQuestion finds the "?" of a conditional at the top of a range, or -1.
func (l *list) topQuestion(r [2]int) int {
	for k := r[0]; k <= r[1]; k = l.after(k) {
		if l.question(k) {
			return k
		}
	}
	return -1
}

func (l *list) slice(r [2]int) []token {
	if r[0] > r[1] || r[0] < 0 || r[1] >= len(l.toks) {
		return nil
	}
	return l.toks[r[0] : r[1]+1]
}

// A fn is a function in the code: declared, assigned to a name or
// anonymous.
type fn struct {
	name   string  // "" for an anonymous one
	decl   *decl   // the declaration that names it
	params []*decl // its parameters in order (nil for a destructured one)
	list   *list
	head   int    // the token it starts with
	body   [2]int // token range: a block with its braces, or the expression
	block  bool
	lo, hi int // the bytes of the body
}

type declKind uint8

const (
	dVar    declKind = iota + 1 // const, let, var
	dFunc                       // function name() {…}
	dParam                      // a parameter of a function
	dLoop                       // for (const x of …)
	dProp                       // a prop of a component: let { x } = $props()
	dImport                     // import x from …, import { x } from …
)

// A decl is a declared name.
type decl struct {
	name   string
	kind   declKind
	list   *list
	at     int    // the name's token
	init   [2]int // the initializer as a token range; empty (init[0] > init[1]) when there is none
	fn     *fn    // the function it names
	owner  *fn    // a parameter's function
	index  int    // a parameter's place
	of     [2]int // a loop variable's iterable
	lo, hi int    // the bytes where the name is visible
	top    bool   // declared at the top of the file (a script or module)
	export bool   // export function …, export const …
	from   string // an import's source: a path under web/src (no extension), or "" for a package
	origin string // the name an import has in its module ("default" for a default import)
}

var none = [2]int{1, 0}

func (d *decl) hasInit() bool { return d.init[0] <= d.init[1] }

func (d *decl) pos() int { return d.list.tok(d.at).pos }

// A flow is what the checks know about the code of one file.
type flow struct {
	f      *webFile
	lists  []*list        // the streams in order
	all    []*list        // with the ${…} of their templates
	at     map[int]tokRef // every token by its byte offset
	byName map[string][]*decl
	fns    []*fn
	heads  map[*list]map[int]*fn
}

// A tokRef is one token of a list.
type tokRef struct {
	l *list
	i int
}

func (r tokRef) pos() int { return r.l.tok(r.i).pos }

// flowOf builds the structure of a file once.
func flowOf(f *webFile) *flow {
	if f.flow != nil {
		return f.flow
	}
	fl := &flow{f: f, at: map[int]tokRef{}, byName: map[string][]*decl{}, heads: map[*list]map[int]*fn{}}
	f.flow = fl
	var add func(l *list)
	add = func(l *list) {
		fl.all = append(fl.all, l)
		for i, t := range l.toks {
			fl.at[t.pos] = tokRef{l, i}
			for _, sub := range t.subs {
				add(newList(f, sub))
			}
		}
	}
	for _, s := range f.streams {
		l := newList(f, s.toks)
		l.top, l.kind, l.from, l.to = true, s.kind, s.from, s.to
		fl.lists = append(fl.lists, l)
		add(l)
	}
	for _, l := range fl.all {
		fl.scanFns(l)
	}
	for _, l := range fl.all {
		fl.scanDecls(l)
	}
	return fl
}

func (fl *flow) ref(pos int) (tokRef, bool) {
	r, ok := fl.at[pos]
	return r, ok
}

func (fl *flow) fnAt(l *list, head int) *fn { return fl.heads[l][head] }

func (fl *flow) addFn(f *fn) {
	f.lo, f.hi = f.list.tok(f.body[0]).pos, f.list.end(f.body[1])
	fl.fns = append(fl.fns, f)
	if fl.heads[f.list] == nil {
		fl.heads[f.list] = map[int]*fn{}
	}
	fl.heads[f.list][f.head] = f
}

// scanFns finds the functions of a list: function declarations and
// expressions, and arrows.
func (fl *flow) scanFns(l *list) {
	for i, t := range l.toks {
		switch {
		case t.kind == tIdent && t.text == "function" && !l.member(i):
			k := i + 1
			if l.tok(k).is("*") {
				k++
			}
			name := ""
			if l.tok(k).kind == tIdent && (l.tok(k+1).is("(") || l.tok(k+1).is("<")) {
				name = l.tok(k).text
				k++
			}
			if l.tok(k).is("<") {
				k = skipAngles(l.toks, k)
			}
			if !l.tok(k).is("(") {
				continue
			}
			params := k
			k = l.after(k)
			if l.tok(k).is(":") { // the return type
				for k < len(l.toks) && !l.tok(k).is("{") && !l.tok(k).is(";") {
					if l.tok(k).is("<") {
						k = skipAngles(l.toks, k)
						continue
					}
					k = l.after(k)
				}
			}
			if !l.tok(k).is("{") {
				continue
			}
			head := i
			if l.word(i-1, "async") {
				head = i - 1
			}
			f := &fn{list: l, head: head, body: [2]int{k, l.after(k) - 1}, block: true}
			if name != "" && fl.declares(l, head) {
				f.name = name
			}
			fl.addFn(f)
			fl.params(f, params)
		case t.is("=>"):
			params, head := -1, -1
			switch j := i - 1; {
			case l.tok(j).is(")") && l.match[j] >= 0:
				params, head = l.match[j], l.match[j]
			case l.tok(j).kind == tIdent && !l.tok(j-1).is(":") && !l.member(j):
				head = j // x => …
			default: // (x): Ask => …, (): Promise<void> => …
				if c := l.returnType(j); c >= 0 {
					params, head = l.match[c-1], l.match[c-1]
				}
			}
			if head < 0 {
				continue
			}
			if l.word(head-1, "async") {
				head--
			}
			f := &fn{list: l, head: head}
			if l.tok(i + 1).is("{") {
				f.body, f.block = [2]int{i + 1, l.after(i+1) - 1}, true
			} else {
				f.body = [2]int{i + 1, l.exprEnd(i + 1)}
			}
			if f.body[1] < f.body[0] {
				continue
			}
			fl.addFn(f)
			if params >= 0 {
				fl.params(f, params)
			} else {
				fl.param(f, i-1, 0)
			}
		}
	}
}

// returnType reads back over an arrow's return type that ends at j to its
// ":"; it returns the index of the ":" when the parameters' ")" is before
// it, else -1.
func (l *list) returnType(j int) int {
	for k := j; k >= 0 && j-k < 32; k-- {
		t := l.toks[k]
		switch {
		case l.closesAt(k):
			if m := l.match[k]; m >= 0 && m < k {
				k = m
			}
		case t.is(":"):
			if l.tok(k-1).is(")") && l.match[k-1] >= 0 {
				return k
			}
			return -1
		case t.kind == tIdent, t.kind == tStr, t.kind == tNum, t.is("."), t.is("<"), t.is(">"), t.is(">>"), t.is("|"), t.is("&"), t.is(","), t.is("["):
		default:
			return -1
		}
	}
	return -1
}

// declares says whether the function keyword at head starts a declaration
// (a statement of its own) rather than an expression.
func (fl *flow) declares(l *list, head int) bool {
	p := l.tok(head - 1)
	switch {
	case head == 0, p.is(";"), p.is("{"), p.is("}"):
		return true
	case p.kind == tIdent && (p.text == "export" || p.text == "default"):
		return true
	case l.newLine(head):
		// after a line break, unless the line before wants an operand
		return !(p.kind == tPunct && !p.is(")") && !p.is("]")) && !(p.kind == tIdent && (p.text == "return" || p.text == "await" || p.text == "new"))
	}
	return false
}

// params declares the parameters in the "(" at i.
func (fl *flow) params(f *fn, i int) {
	l := f.list
	end := l.closing(i)
	for k, n := i+1, 0; k < end; n++ {
		fl.param(f, k, n)
		for k < end && !l.tok(k).is(",") {
			k = l.after(k)
		}
		k++
	}
}

// param declares the parameter that starts at k: a name (with its type or
// default), …rest, or a destructuring pattern (whose names have no place).
func (fl *flow) param(f *fn, k, n int) {
	l := f.list
	if l.tok(k).is("...") {
		k++
	}
	switch t := l.tok(k); {
	case t.kind == tIdent && t.text != "async":
		d := &decl{name: t.text, kind: dParam, list: l, at: k, init: none, owner: f, index: n, lo: f.lo, hi: f.hi}
		fl.addDecl(d)
		for len(f.params) < n {
			f.params = append(f.params, nil)
		}
		f.params = append(f.params, d)
	case t.is("{") || t.is("["):
		for _, at := range patternNames(l, k) {
			fl.addDecl(&decl{name: l.tok(at).text, kind: dParam, list: l, at: at, init: none, owner: f, index: -1, lo: f.lo, hi: f.hi})
		}
	}
}

// patternNames are the names a destructuring pattern at k binds:
// { a, b = 1, c: d, ...e } binds a, b, d and e; [x, y] binds x and y.
func patternNames(l *list, k int) []int {
	end := l.closing(k)
	object := l.tok(k).is("{")
	var out []int
	for i := k + 1; i < end; {
		t := l.toks[i]
		switch {
		case l.opens(i):
			if p := l.tok(i - 1); p.is(":") || p.is(",") || p.is("[") || p.is("{") || p.is("...") {
				out = append(out, patternNames(l, i)...)
			}
			i = l.after(i)
			continue
		case t.kind == tIdent:
			p, n := l.tok(i-1), l.tok(i+1)
			key := object && n.is(":") && (p.is("{") || p.is(","))
			if !key && (p.is("{") || p.is("[") || p.is(",") || p.is("...") || p.is(":")) && (n.is(",") || n.is("}") || n.is("]") || n.is("=") || i+1 == end) {
				out = append(out, i)
			}
			if n.is("=") { // a default value
				i += 2
				for i < end && !l.tok(i).is(",") {
					i = l.after(i)
				}
				continue
			}
		}
		i++
	}
	return out
}

func (fl *flow) addDecl(d *decl) {
	fl.byName[d.name] = append(fl.byName[d.name], d)
}

// scope is the byte range where a declaration at token i is visible: the
// block around it, or the whole file at the top of a script or module.
func (fl *flow) scope(l *list, i int) (lo, hi int, top bool) {
	for u := l.up[i]; u >= 0; u = l.up[u] {
		if l.toks[u].is("{") {
			return l.toks[u].pos, l.end(l.closing(u)), false
		}
	}
	if l.top && l.kind == sScript {
		return 0, len(fl.f.code), true
	}
	return l.from, l.to, false
}

// scanDecls finds the declarations of a list: function declarations, const,
// let and var (with the props of a component and loop variables), imports.
func (fl *flow) scanDecls(l *list) {
	for _, f := range fl.fns {
		if f.list != l || f.name == "" || f.decl != nil {
			continue
		}
		at := f.head + 1
		if l.word(f.head, "async") {
			at++
		}
		lo, hi, top := fl.scope(l, f.head)
		d := &decl{name: f.name, kind: dFunc, list: l, at: at, init: none, fn: f, lo: lo, hi: hi, top: top, export: l.word(f.head-1, "export")}
		f.decl = d
		fl.addDecl(d)
	}
	for i, t := range l.toks {
		if t.kind != tIdent || l.member(i) {
			continue
		}
		switch t.text {
		case "const", "let", "var":
			fl.declarators(l, i)
		case "import":
			fl.imports(l, i)
		}
	}
}

// declarators reads `const a = 1, { b } = c, d: T = …` from the keyword at
// kw on, and `for (const x of xs)`.
func (fl *flow) declarators(l *list, kw int) {
	lo, hi, top := fl.scope(l, kw)
	loop := false
	if u := l.up[kw]; u >= 0 && l.toks[u].is("(") && l.word(u-1, "for") {
		lo, hi, top, loop = l.toks[u-1].pos, l.end(l.stmtEnd(u-1)), false, true
	}
	for k := kw + 1; k < len(l.toks); {
		var names []int
		pattern := l.tok(k).is("{") || l.tok(k).is("[")
		switch {
		case l.tok(k).kind == tIdent:
			names = []int{k}
			k++
		case pattern:
			names = patternNames(l, k)
			k = l.after(k)
		default:
			return
		}
		if l.tok(k).is(":") { // the type
			for k++; k < len(l.toks); {
				t := l.tok(k)
				if t.is("=") || t.is(",") || t.is(";") || l.closesAt(k) || l.word(k, "of") || l.word(k, "in") {
					break
				}
				if t.is("<") {
					k = max(skipAngles(l.toks, k), k+1)
				} else {
					k = l.after(k)
				}
			}
		}
		init, of := none, none
		switch {
		case l.tok(k).is("="):
			init = [2]int{k + 1, l.exprEnd(k + 1)}
			k = max(init[1]+1, k+1)
		case loop && (l.word(k, "of") || l.word(k, "in")):
			of = [2]int{k + 1, l.closing(l.up[kw]) - 1}
			k = of[1] + 1
		}
		props := init[0] <= init[1] && l.word(init[0], "$props")
		for _, at := range names {
			d := &decl{name: l.tok(at).text, kind: dVar, list: l, at: at, init: init, of: none, lo: lo, hi: hi, top: top, export: l.word(kw-1, "export")}
			switch {
			case loop:
				d.kind, d.of, d.init = dLoop, of, none
			case props:
				d.kind, d.init = dProp, none
			case pattern:
				d.init = none // it holds a part of the value, not all of it
			}
			if d.hasInit() {
				s := d.init[0]
				if l.word(s, "async") {
					s++
				}
				for _, h := range []int{d.init[0], s} {
					if f := fl.fnAt(l, h); f != nil && f.decl == nil {
						d.fn, f.decl, f.name = f, d, d.name
						break
					}
				}
			}
			fl.addDecl(d)
		}
		if !l.tok(k).is(",") || loop {
			return
		}
		k++
	}
}

// imports reads `import x from '…'`, `import { a, b as c } from '…'` and
// `import * as x from '…'`.
func (fl *flow) imports(l *list, i int) {
	if l.word(i+1, "type") || l.tok(i+1).is("(") || l.tok(i+1).is(".") {
		return
	}
	type binding struct {
		at     int // the local name
		origin string
	}
	var names []binding
	k := i + 1
	for ; k < len(l.toks) && !l.word(k, "from") && l.tok(k).kind != tStr && !l.tok(k).is(";"); k++ {
		switch t := l.tok(k); {
		case t.is("{"):
			end := l.closing(k)
			for j := k + 1; j < end; j++ {
				switch {
				case l.toks[j].kind != tIdent, l.word(j, "type") && l.tok(j+1).kind == tIdent, l.word(j, "as"):
				case l.word(j+1, "as"): // { a as b }: b is a's name here
					names = append(names, binding{j + 2, l.toks[j].text})
					j += 2
				default:
					names = append(names, binding{j, l.toks[j].text})
				}
			}
			k = end
		case t.kind == tIdent && t.text != "as" && !l.tok(k-1).is("*") && !l.word(k-1, "as"):
			names = append(names, binding{k, "default"})
		case t.kind == tIdent && l.word(k-1, "as"): // * as x
			names = append(names, binding{k, "*"})
		}
	}
	if !l.word(k, "from") || l.tok(k+1).kind != tStr {
		return
	}
	src := resolveImport(fl.f.path, l.tok(k+1).text)
	for _, b := range names {
		fl.addDecl(&decl{name: l.tok(b.at).text, kind: dImport, list: l, at: b.at, init: none, lo: 0, hi: len(fl.f.code), top: true, from: src, origin: b.origin})
	}
}

// resolveImport turns an import's source into a path under web/src, or ""
// for a package.
func resolveImport(from, src string) string {
	switch {
	case strings.HasPrefix(src, "$lib/"):
		return "lib/" + strings.TrimPrefix(src, "$lib/")
	case strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../"):
		return path.Clean(path.Join(path.Dir(from), src))
	}
	return ""
}

// resolve finds what a name at a byte offset refers to: the declaration of
// that name with the innermost scope around the offset.
func (fl *flow) resolve(name string, pos int) *decl {
	var best *decl
	for _, d := range fl.byName[name] {
		if pos < d.lo || pos >= d.hi {
			continue
		}
		if best == nil || d.hi-d.lo < best.hi-best.lo {
			best = d
		}
	}
	return best
}

// refs are the places that name a declaration, its own name aside.
func (fl *flow) refs(d *decl) []tokRef {
	var out []tokRef
	own := d.pos()
	for _, l := range fl.all {
		for i, t := range l.toks {
			if t.kind != tIdent || t.text != d.name || l.member(i) || t.pos == own {
				continue
			}
			// An object key of that name is no reference: { load: … }.
			if l.tok(i+1).is(":") && (l.tok(i-1).is("{") || l.tok(i-1).is(",")) && l.up[i] >= 0 && l.toks[l.up[i]].is("{") {
				continue
			}
			if fl.resolve(t.text, t.pos) == d {
				out = append(out, tokRef{l, i})
			}
		}
	}
	return out
}

// innermost is the named function whose body holds a byte offset, if any.
func (fl *flow) innermost(pos int) *fn {
	var best *fn
	for _, f := range fl.fns {
		if f.name == "" || pos < f.lo || pos >= f.hi {
			continue
		}
		if best == nil || f.hi-f.lo < best.hi-best.lo {
			best = f
		}
	}
	return best
}

// returns are the expressions a function returns: those of its return
// statements (not of the functions inside it), or its expression body.
func (f *fn) returns() [][2]int {
	l := f.list
	if !f.block {
		return [][2]int{f.body}
	}
	var out [][2]int
	for k := f.body[0] + 1; k < f.body[1]; k++ {
		if inner := l.file.flow.fnAt(l, k); inner != nil && inner != f {
			k = inner.body[1]
			continue
		}
		if l.word(k, "return") && !l.member(k) {
			e := l.stmtEnd(k)
			if l.tok(e).is(";") {
				e--
			}
			if e > k {
				out = append(out, [2]int{k + 1, e})
			}
		}
	}
	return out
}

// A call is a call in the code with the token ranges of its arguments.
type call struct {
	l    *list
	args [][2]int
}

// callsOf are the places that call a declared function, with their
// arguments; passed says it is also handed over as a value (a callback,
// whose arguments are not in the code).
func (fl *flow) callsOf(d *decl) (out []call, passed bool) {
	for _, r := range fl.refs(d) {
		j := r.l.generic(r.i + 1)
		if !r.l.tok(j).is("(") {
			passed = true
			continue
		}
		out = append(out, call{r.l, argRanges(r.l, j)})
	}
	return out, passed
}

// argRanges splits the arguments of the call whose "(" is at j.
func argRanges(l *list, j int) [][2]int {
	if !l.tok(j).is("(") {
		return nil
	}
	end := l.closing(j)
	var out [][2]int
	start := j + 1
	for k := j + 1; k < end; {
		if l.tok(k).is(",") {
			if k > start {
				out = append(out, [2]int{start, k - 1})
			}
			start = k + 1
			k++
			continue
		}
		k = l.after(k)
	}
	if end > start {
		out = append(out, [2]int{start, end - 1})
	}
	return out
}

// tagName finds the tags of components: <Name …>.
var tagName = regexp.MustCompile(`<([A-Z][A-Za-z0-9_]*)[\s/>]`)

// A usage is one <Component …> in a file.
type usage struct {
	f        *webFile
	pos, end int    // its "<" and the offset after its ">"
	src      string // the component's file
}

// usages lists, per component file, where the other files use it: the tags
// of a default import of a .svelte file, outside their scripts.
func usages(web map[string]*webFile) map[string][]usage {
	out := map[string][]usage{}
	names := make([]string, 0, len(web))
	for n := range web {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := web[n]
		if !strings.HasSuffix(n, ".svelte") {
			continue
		}
		fl := flowOf(f)
		for _, m := range tagName.FindAllStringSubmatchIndex(f.code, -1) {
			pos := m[0]
			if fl.within(pos, sScript) || fl.within(pos, sExpr) || fl.within(pos, sAttr) {
				continue
			}
			d := fl.resolve(f.code[m[2]:m[3]], pos)
			if d == nil || d.kind != dImport || d.origin != "default" || !strings.HasSuffix(d.from, ".svelte") {
				continue
			}
			out[d.from] = append(out[d.from], usage{f: f, pos: pos, end: fl.tagEnd(pos), src: d.from})
		}
	}
	return out
}

// within says whether a byte offset is inside a stream of that kind.
func (fl *flow) within(pos int, kind streamKind) bool {
	for _, l := range fl.lists {
		if l.kind == kind && pos >= l.from && pos < l.to {
			return true
		}
	}
	return false
}

// tagEnd is the offset after the ">" of the tag at pos: the {…} values of
// its attributes (onclick={() => …}) hold ">" too.
func (fl *flow) tagEnd(pos int) int {
	code := fl.f.code
	for i := pos + 1; i < len(code); i++ {
		if code[i] == '>' && !fl.within(i, sExpr) && !fl.within(i, sAttr) {
			return i + 1
		}
	}
	return len(code)
}

var attrBefore = regexp.MustCompile(`(^|\s)([A-Za-z_][\w:.-]*)=\s*$`)

// An attr is how a component tag gives one attribute.
type attr struct {
	given  bool
	bare   bool  // the name alone: true
	bound  bool  // bind:name, the page's variable of that name: changes over time
	value  *list // the {…} of name={…}, bind:name={…} or the {name} shorthand
	spread bool  // a {...props} may set it as well
}

// attr reads how a component tag gives an attribute.
func (u usage) attr(name string) (a attr) {
	fl := flowOf(u.f)
	for _, l := range fl.lists {
		if l.kind != sExpr || l.from < u.pos || l.from >= u.end {
			continue
		}
		m := attrBefore.FindStringSubmatch(u.f.code[u.pos:l.from])
		switch {
		case len(l.toks) > 0 && l.toks[0].is("...") && m == nil:
			a.spread = true
		case m != nil && strings.TrimPrefix(m[2], "bind:") == name:
			a.given, a.value = true, l
		case m == nil && len(l.toks) == 1 && l.toks[0].kind == tIdent && l.toks[0].text == name:
			a.given, a.value = true, l
		}
	}
	if a.given {
		return a
	}
	re := regexp.MustCompile(`\s(bind:)?` + regexp.QuoteMeta(name) + `(\s|/|>)`)
	inside := u.f.code[u.pos:u.end]
	for _, loc := range re.FindAllStringSubmatchIndex(inside, -1) {
		if fl.within(u.pos+loc[0]+1, sExpr) || fl.within(u.pos+loc[0]+1, sAttr) {
			continue
		}
		a.given = true
		if loc[2] >= 0 {
			a.bound = true
		} else {
			a.bare = true
		}
	}
	return a
}
