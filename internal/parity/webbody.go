package parity

import (
	"sort"
	"strings"
)

// What a call of the web UI sends: the fields of its JSON body and the keys
// of its query string, read from the code that builds them — an object
// literal, a variable and the properties assigned to it, the function that
// returns it, the arguments of the function whose parameter it is. A field
// counts as sent only where it is a key of what goes into the call, not
// where it is merely named (a table column showing site.quota_mb sends
// nothing). Where the value comes from somewhere the reading cannot follow
// (an import, a prop, a callback's argument), the keys are open and the
// check falls back to the field being named in the file.

// keys are the fields a request can carry, with where each is set.
type keys struct {
	at   map[string][]int // a field → the byte offsets that set it
	open []string         // what could not be followed; then any field may be there
}

func (k *keys) add(name string, pos int) {
	if k.at == nil {
		k.at = map[string][]int{}
	}
	k.at[name] = append(k.at[name], pos)
}

func (k *keys) merge(o keys) {
	for name, list := range o.at {
		for _, p := range list {
			k.add(name, p)
		}
	}
	k.open = append(k.open, o.open...)
}

func (k *keys) opened(why string) { k.open = append(k.open, why) }

func (k keys) names() []string {
	out := make([]string, 0, len(k.at))
	for n := range k.at {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// A reader follows the value of an expression to the keys it can have.
type reader struct {
	fl    *flow
	query bool         // a query string (?a=1&b=2) rather than a JSON body
	busy  map[any]bool // declarations and functions being read: a cycle adds nothing
	depth int
}

func newReader(f *webFile, query bool) *reader {
	return &reader{fl: flowOf(f), query: query, busy: map[any]bool{}}
}

// callArgs finds the arguments of a call of the scanner by its offset.
func (r *reader) callArgs(c webCall) (*list, [][2]int) {
	ref, ok := r.fl.ref(c.pos)
	if !ok {
		return nil, nil
	}
	if c.via == "url" {
		// A link: the URL expression itself.
		return ref.l, [][2]int{{ref.i, ref.l.exprEnd(ref.i)}}
	}
	j := ref.l.generic(ref.i + 1)
	return ref.l, argRanges(ref.l, j)
}

// bodyOf reads the JSON body of a call: api(path, { json: … }) or
// fetch(url, { body: JSON.stringify(…) }). has says it sends one at all.
func bodyOf(f *webFile, c webCall) (k keys, has bool) {
	if c.via != "api" && c.via != "fetch" {
		return keys{}, false // a GET, a link, or a raw body (apiPutRaw)
	}
	r := newReader(f, false)
	l, args := r.callArgs(c)
	if l == nil || len(args) < 2 {
		return keys{}, false
	}
	opts := l.trim(args[1])
	if !l.tok(opts[0]).is("{") || l.match[opts[0]] != opts[1] {
		k.opened("the options of the call are built elsewhere")
		return k, true
	}
	name := "json"
	if c.via == "fetch" {
		name = "body"
	}
	v, ok := property(l, opts, name)
	if !ok {
		return keys{}, false
	}
	if c.via == "fetch" {
		// JSON.stringify(x)
		if l.word(v[0], "JSON") && l.tok(v[0]+1).is(".") && l.word(v[0]+2, "stringify") && l.tok(v[0]+3).is("(") {
			if a := argRanges(l, v[0]+3); len(a) > 0 {
				return r.expr(l, a[0]), true
			}
		}
		k.opened("the body is not JSON.stringify(…)")
		return k, true
	}
	return r.expr(l, v), true
}

// queryOf reads the keys of the query string of a call's path.
func queryOf(f *webFile, c webCall) keys {
	r := newReader(f, true)
	l, args := r.callArgs(c)
	if l == nil || len(args) == 0 {
		return keys{}
	}
	return r.expr(l, args[0])
}

// property finds the value of a property of the object literal at r:
// { name: value } or the shorthand { name }.
func property(l *list, r [2]int, name string) ([2]int, bool) {
	for _, e := range l.split([2]int{r[0] + 1, r[1] - 1}, ",") {
		switch {
		case e[0] > e[1]:
		case l.word(e[0], name) && l.tok(e[0]+1).is(":"):
			return [2]int{e[0] + 2, e[1]}, true
		case l.word(e[0], name) && e[0] == e[1]:
			return e, true
		}
	}
	return none, false
}

// expr reads the keys of the value of a token range.
func (r *reader) expr(l *list, rg [2]int) (k keys) {
	if r.depth > 24 {
		k.opened("a value built through too many steps")
		return k
	}
	r.depth++
	defer func() { r.depth-- }()
	rg = l.trim(rg)
	if rg[0] > rg[1] {
		return k
	}
	// A conditional or a fallback: whatever any branch gives.
	if q := l.topQuestion(rg); q >= 0 {
		if c := l.ternaryColon(q); c > q && c <= rg[1] {
			k.merge(r.expr(l, [2]int{q + 1, c - 1}))
			k.merge(r.expr(l, [2]int{c + 1, rg[1]}))
			return k
		}
	}
	for _, op := range []string{"||", "??"} {
		if parts := l.split(rg, op); len(parts) > 1 {
			for _, p := range parts {
				k.merge(r.expr(l, p))
			}
			return k
		}
	}
	if parts := l.split(rg, "&&"); len(parts) > 1 {
		return r.expr(l, parts[len(parts)-1])
	}
	if r.query {
		if parts := l.split(rg, "+"); len(parts) > 1 {
			// '/sites/' + d + '/apply': d is a segment; '?range=' + r a
			// value; '/mail/mailboxes' + byDomain(f) may be the query.
			prev := ""
			for i, p := range parts {
				lit := p[0] == p[1] && l.tok(p[0]).literal()
				if lit || i > 0 && queryPart(prev) {
					k.merge(r.expr(l, p))
				}
				if lit {
					prev = l.tok(p[0]).text
				} else {
					prev = hole
				}
			}
			return k
		}
	}
	t := l.tok(rg[0])
	switch {
	case t.is("{") && l.match[rg[0]] == rg[1]:
		if !r.query {
			return r.object(l, rg)
		}
	case t.literal() && rg[0] == rg[1]:
		if r.query {
			r.literal(l, rg[0], &k)
		}
	case t.kind == tIdent && rg[0] == rg[1]:
		return r.name(l, rg[0])
	case t.kind == tIdent && t.text == "new" && l.word(rg[0]+1, "URLSearchParams"):
		if a := argRanges(l, rg[0]+2); len(a) > 0 && r.query {
			return r.params(l, a[0])
		}
	case t.kind == tIdent:
		return r.callOrMember(l, rg)
	}
	if !r.query && !(t.kind == tIdent && (t.text == "null" || t.text == "undefined")) && !t.literal() && t.kind != tNum {
		k.opened(shorten(tokensText(l.slice(rg))))
	}
	return k
}

// literal reads a string or template: in a query, its ?key= and &key=, and
// what the ${…} that may hold a query add (not a segment's, not a value's).
func (r *reader) literal(l *list, i int, k *keys) {
	t := l.toks[i]
	for _, m := range queryKey.FindAllStringSubmatch(t.text, -1) {
		k.add(m[1], t.pos)
	}
	parts := strings.Split(t.text, hole)
	for n, sub := range t.subs {
		if len(sub) == 0 || n >= len(parts) || n == 0 && parts[0] == "" || !queryPart(strings.Join(parts[:n+1], hole)) {
			continue
		}
		if ref, ok := r.fl.ref(sub[0].pos); ok {
			k.merge(r.expr(ref.l, [2]int{0, len(ref.l.toks) - 1}))
		}
	}
}

// queryPart says whether what follows the text before it can be (part of)
// a query string: not a path segment (after "/"), not a value (after "=").
func queryPart(before string) bool {
	return before != "" && !strings.HasSuffix(before, "/") && !strings.HasSuffix(before, "=")
}

// params reads new URLSearchParams(x): the keys of an object, or of a
// query string.
func (r *reader) params(l *list, a [2]int) keys {
	a = l.trim(a)
	if l.tok(a[0]).is("{") && l.match[a[0]] == a[1] {
		saved := r.query
		r.query = false
		k := r.object(l, a)
		r.query = saved
		return k
	}
	return r.expr(l, a)
}

// object reads the keys of an object literal: { a: 1, b, 'c': 2, [k]: 3,
// ...rest, m() {} }.
func (r *reader) object(l *list, rg [2]int) (k keys) {
	for _, e := range l.split([2]int{rg[0] + 1, rg[1] - 1}, ",") {
		if e[0] > e[1] {
			continue
		}
		t := l.tok(e[0])
		switch {
		case t.is("..."):
			k.merge(r.expr(l, [2]int{e[0] + 1, e[1]}))
		case t.is("["):
			k.merge(r.computed(l, [2]int{e[0] + 1, l.closing(e[0]) - 1}))
		case (t.kind == tIdent || t.kind == tStr || t.kind == tNum) && (e[0] == e[1] || l.tok(e[0]+1).is(":") || l.tok(e[0]+1).is("(")):
			k.add(t.text, t.pos)
		case t.kind == tIdent && (t.text == "get" || t.text == "set" || t.text == "async") && l.tok(e[0]+1).kind == tIdent:
			k.add(l.tok(e[0]+1).text, l.tok(e[0]+1).pos)
		default:
			k.opened("an object entry " + shorten(tokensText(l.slice(e))))
		}
	}
	return k
}

// computed reads a computed key ([k]): a string, or the strings of the
// array a loop variable runs over (for (const k of ['a', 'b'])).
func (r *reader) computed(l *list, e [2]int) (k keys) {
	e = l.trim(e)
	switch t := l.tok(e[0]); {
	case e[0] == e[1] && t.kind == tStr:
		k.add(t.text, t.pos)
		return k
	case e[0] == e[1] && t.kind == tIdent:
		if d := r.fl.resolve(t.text, t.pos); d != nil && d.kind == dLoop {
			found := false
			for i := d.of[0]; i <= d.of[1] && i < len(d.list.toks); i++ {
				if s := d.list.toks[i]; s.kind == tStr && plainWord.MatchString(s.text) {
					k.add(s.text, s.pos)
					found = true
				}
			}
			if found {
				return k
			}
		}
	}
	k.opened("a computed key [" + shorten(tokensText(l.slice(e))) + "]")
	return k
}

// name reads what a name holds.
func (r *reader) name(l *list, i int) (k keys) {
	t := l.toks[i]
	d := r.fl.resolve(t.text, t.pos)
	if d == nil {
		if !r.query {
			k.opened(t.text + " (not declared in the file)")
		}
		return k
	}
	if r.busy[d] {
		return k
	}
	r.busy[d] = true
	defer delete(r.busy, d)
	switch d.kind {
	case dVar:
		if d.fn != nil {
			if !r.query {
				k.opened(t.text + " (a function)")
			}
			return k
		}
		if d.hasInit() {
			k.merge(r.expr(d.list, d.init))
		}
		k.merge(r.assigned(d))
	case dParam:
		f := d.owner
		if f.decl == nil || d.index < 0 {
			k.opened(t.text + " (a parameter of a function the check cannot follow)")
			return k
		}
		calls, passed := r.fl.callsOf(f.decl)
		if passed && !r.query {
			k.opened(t.text + " (a parameter of " + f.name + ", which is also handed over as a callback)")
		}
		for _, c := range calls {
			if d.index < len(c.args) {
				k.merge(r.expr(c.l, c.args[d.index]))
			}
		}
	case dImport:
		k.opened(t.text + " (imported)")
	case dProp:
		k.opened(t.text + " (a prop of the component)")
	default:
		if !r.query {
			k.opened(t.text)
		}
	}
	return k
}

// assigned reads what is added to a variable after its declaration: in a
// body x.key = …, x[k] = … and Object.assign(x, …); in a query
// x.set('key', …) and x.append('key', …).
func (r *reader) assigned(d *decl) (k keys) {
	for _, ref := range r.fl.refs(d) {
		l, i := ref.l, ref.i
		switch {
		case r.query && l.tok(i+1).is(".") && (l.word(i+2, "set") || l.word(i+2, "append")) && l.tok(i+3).is("("):
			if a := argRanges(l, i+3); len(a) > 0 {
				if s := l.tok(a[0][0]); a[0][0] == a[0][1] && s.kind == tStr {
					k.add(s.text, s.pos)
				} else {
					k.merge(r.computed(l, a[0]))
				}
			}
		case r.query:
		case l.tok(i+1).is(".") && l.tok(i+2).kind == tIdent && assignTo(l, i+3):
			k.add(l.tok(i+2).text, l.tok(i+2).pos)
		case l.tok(i+1).is("[") && assignTo(l, l.after(i+1)):
			k.merge(r.computed(l, [2]int{i + 2, l.closing(i+1) - 1}))
		case l.tok(i-1).is("(") && l.word(i-2, "assign") && l.tok(i-3).is(".") && l.word(i-4, "Object"):
			for _, a := range argRanges(l, i-1)[1:] {
				k.merge(r.expr(l, a))
			}
		}
	}
	return k
}

// assignTo says whether the token at i assigns to what is before it.
func assignTo(l *list, i int) bool {
	t := l.tok(i)
	return t.kind == tPunct && (t.text == "=" || t.text == "??=" || t.text == "||=" || t.text == "&&=")
}

// callOrMember reads a call (whose result is the value) or a property.
func (r *reader) callOrMember(l *list, rg [2]int) (k keys) {
	// The callee: a name or a dotted name, maybe with type arguments.
	j := rg[0]
	for j+2 <= rg[1] && (l.tok(j+1).is(".") || l.tok(j+1).is("?.")) && l.tok(j+2).kind == tIdent {
		j += 2
	}
	callee := tokensText(l.slice([2]int{rg[0], j}))
	open := l.generic(j + 1)
	if !l.tok(open).is("(") || l.closing(open) != rg[1] {
		// x.y, x[k], f(a)(b), f(a).g(): a property or more than one call.
		if strings.HasSuffix(callee, ".toString") || l.tok(open).is("(") && l.word(l.closing(open)+2, "toString") {
			return r.expr(l, [2]int{rg[0], j - 2})
		}
		if !r.query {
			k.opened(shorten(tokensText(l.slice(rg))))
		}
		return k
	}
	args := argRanges(l, open)
	switch callee {
	case "$state", "$state.raw", "$derived", "structuredClone", "String", "JSON.parse":
		if len(args) > 0 {
			return r.expr(l, args[0])
		}
		return k
	case "JSON.stringify":
		if len(args) > 0 && !r.query {
			return r.expr(l, args[0])
		}
		return k
	case "$derived.by":
		if len(args) > 0 {
			return r.returnsOf(l, args[0])
		}
		return k
	case "encodeURIComponent", "encodeURI", "Number", "Boolean", "Math.min", "Math.max", "Math.trunc":
		return k // a value
	}
	if j == rg[0] {
		if d := r.fl.resolve(callee, l.tok(rg[0]).pos); d != nil && d.fn != nil {
			return r.fnReturns(d.fn)
		}
	}
	if !r.query {
		k.opened(callee + "(…)")
	}
	return k
}

// returnsOf reads the values a function expression (or a function's name)
// returns.
func (r *reader) returnsOf(l *list, a [2]int) (k keys) {
	a = l.trim(a)
	if f := r.fl.fnAt(l, a[0]); f != nil {
		return r.fnReturns(f)
	}
	if a[0] == a[1] && l.tok(a[0]).kind == tIdent {
		if d := r.fl.resolve(l.tok(a[0]).text, l.tok(a[0]).pos); d != nil && d.fn != nil {
			return r.fnReturns(d.fn)
		}
	}
	if !r.query {
		k.opened("a function the check cannot follow")
	}
	return k
}

func (r *reader) fnReturns(f *fn) (k keys) {
	if r.busy[f] {
		return k
	}
	r.busy[f] = true
	defer delete(r.busy, f)
	for _, ret := range f.returns() {
		k.merge(r.expr(f.list, ret))
	}
	return k
}
