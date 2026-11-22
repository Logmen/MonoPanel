package parity

import (
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Which code of the web UI ordinary accounts can reach. Pages the navigation
// shows them (and the components and modules under lib/) are shared with
// administrators; there, a call only administrators may make must sit
// behind a check of the role, so that an ordinary account never meets a
// control that can only fail. A place is guarded when something on the way
// to it implies the administrator role:
//
//   - a condition around it: {#if admin}…{/if}, the {:else} of {#if !admin},
//     if (admin) …, the rest of a function after if (!admin) return,
//     admin ? … : …, admin && …;
//   - every place that uses the function it is in (a button's onclick, the
//     run of a confirmation built by a function that is itself guarded …);
//   - every place that uses the component it is in, at its top level —
//     except a place whose condition on a prop the place refutes (CronPanel
//     calls GET /cron only without login, and the account page gives it
//     login={me.login}; an attribute given a value other than a falsy
//     literal counts as set, unless it is a name that may hold nothing);
//   - the content of a component that renders it only under a prop (Modal
//     shows its children and footer only {#if open}): there the value the
//     page gives that prop holds (open={tlsOpen});
//
// where the role is the caller's (auth.me.role === 'admin', or a name the
// file derives from that; the role of a table row is not), a prop called
// admin that every user of the component sets from the role (or only where
// it is guarded), or a variable that starts empty and is only ever filled in
// guarded places (the account a delete dialog was opened for). Functions
// that call each other count as one: a way into any of them is a way into
// all. The reading is the same as for the calls: shapes, not semantics, and
// a guard it cannot see is reported, so that it gets written plainly.

// A fact is a condition that holds, or does not, in a region of the code.
type fact struct {
	l     *list
	cond  [2]int
	holds bool
}

// A region is a byte range of a file with what is known there.
type region struct {
	lo, hi int
	facts  []fact
}

// roles answers whether places of the web UI are guarded by the role.
type roles struct {
	web     map[string]*webFile
	pages   []string // what the navigation shows ordinary accounts: "/", "/sites" …
	uses    map[string][]usage
	regions map[*webFile][]region
	gates   map[string]string    // component → the prop its content renders under ("" none)
	busy    map[string]bool      // regions or gates being worked out
	cands   map[*webFile][]*decl // variables that start empty: may hold an administrator's choice
	sets    map[*decl][]int      // where such a variable is filled
	state   map[*decl]bool       // the ones filled in guarded places only
	memo    map[string]bool      // places decided
	honest  map[string]int8      // component → 1 computing, 2 yes, 3 no

	// Working a place out can lead back to a place still being worked out:
	// functions that call each other, a cycle that is no way in by itself.
	// A yes found meanwhile leans on that and stays provisional until the
	// first place of the cycle is decided (Tarjan's strongly connected
	// components). open has the places in progress by when they were
	// reached and the provisional ones by the earliest place in progress
	// they lean on; pending lists the provisional ones; low is that earliest
	// place for the working-out in progress.
	open    map[string]int
	pending []string
	seq     int
	low     int
}

// newRoles reads the navigation and prepares the files.
func newRoles(web map[string]*webFile) (*roles, string) {
	g := &roles{web: web, uses: usages(web), regions: map[*webFile][]region{}, gates: map[string]string{}, busy: map[string]bool{},
		cands: map[*webFile][]*decl{}, sets: map[*decl][]int{}, open: map[string]int{}, low: math.MaxInt}
	nav := web["lib/components/Nav.svelte"]
	if nav == nil {
		return nil, "web/src/lib/components/Nav.svelte does not exist: the check reads from it which pages ordinary accounts see"
	}
	pages, ok := navPages(nav)
	if !ok {
		return nil, "web/src/lib/components/Nav.svelte has no list of the pages ordinary accounts see (['/', '/sites', …].includes(href)): the check reads it to know which pages are shared"
	}
	g.pages = pages
	names := make([]string, 0, len(web))
	for n := range web {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g.regionsOf(web[n])
		g.findCandidates(web[n])
	}
	g.settle()
	return g, ""
}

// navPages reads the pages the navigation shows ordinary accounts: the
// list of paths whose .includes(href) filters the entries.
func navPages(nav *webFile) ([]string, bool) {
	fl := flowOf(nav)
	for _, l := range fl.all {
		for i := range l.toks {
			if !l.tok(i).is("[") {
				continue
			}
			e := l.closing(i)
			if !l.tok(e+1).is(".") || !l.word(e+2, "includes") {
				continue
			}
			var out []string
			for _, p := range l.split([2]int{i + 1, e - 1}, ",") {
				if t := l.tok(p[0]); p[0] == p[1] && t.kind == tStr && strings.HasPrefix(t.text, "/") {
					out = append(out, t.text)
				} else if p[0] <= p[1] {
					out = nil
					break
				}
			}
			if len(out) > 0 {
				return out, true
			}
		}
	}
	return nil, false
}

// shared says whether ordinary accounts reach a file: a page the navigation
// shows them, the layout, anything under lib/.
func (g *roles) shared(file string) bool {
	rest, ok := strings.CutPrefix(file, "routes/")
	if !ok {
		return true
	}
	dir, _, nested := strings.Cut(rest, "/")
	if !nested {
		// routes/+page.svelte is "/"; the layout and the error page are around every page.
		return !strings.HasPrefix(rest, "+page.") || slices.Contains(g.pages, "/")
	}
	return slices.Contains(g.pages, "/"+dir)
}

// component says whether a file is a component used by others (not a page).
func component(file string) bool {
	return strings.HasSuffix(file, ".svelte") && !strings.HasPrefix(path.Base(file), "+")
}

// scanRegions finds the conditional regions of a file: those of its
// statements and expressions and those of the markup's {#if} blocks.
func scanRegions(f *webFile) []region {
	fl := flowOf(f)
	var out []region
	add := func(l *list, a, b int, facts ...fact) {
		if a <= b && a >= 0 && b < len(l.toks) {
			out = append(out, region{lo: l.toks[a].pos, hi: l.end(b), facts: facts})
		}
	}
	for _, l := range fl.all {
		for i, t := range l.toks {
			switch {
			case t.kind == tIdent && t.text == "if" && l.tok(i+1).is("(") && !l.member(i) && !l.tok(i-1).is("#") && !(l.word(i-1, "else") && l.tok(i-2).is(":")):
				cond := [2]int{i + 2, l.closing(i+1) - 1}
				s := l.after(i + 1)
				e := l.stmtEnd(s)
				add(l, s, e, fact{l, cond, true})
				if l.word(e+1, "else") {
					add(l, e+2, l.stmtEnd(e+2), fact{l, cond, false})
				} else if l.exits(s, e) {
					// if (!admin) return; — the rest of the block is the administrator's.
					stop := len(l.toks) - 1
					for u := l.up[i]; u >= 0; u = l.up[u] {
						if l.toks[u].is("{") {
							stop = l.closing(u) - 1
							break
						}
					}
					add(l, e+1, stop, fact{l, cond, false})
				}
			case l.question(i):
				c := l.ternaryColon(i)
				if c < 0 {
					continue
				}
				cond := [2]int{l.operandBefore(i), i - 1}
				add(l, i+1, c-1, fact{l, cond, true})
				add(l, c+1, l.ternaryEnd(c), fact{l, cond, false})
			case t.is("&&"):
				cond := [2]int{l.operandBefore(i, "||", "??"), i - 1}
				add(l, i+1, l.operandAfter(i, "||", "??", "?", ":"), fact{l, cond, true})
			case t.is("||"):
				cond := [2]int{l.operandBefore(i, "??"), i - 1}
				add(l, i+1, l.operandAfter(i, "??", "?", ":"), fact{l, cond, false})
			}
		}
	}
	return append(out, markupRegions(f)...)
}

// markupRegions are the branches of the markup's {#if}…{:else if}…{:else}
// …{/if} blocks: in each, its own condition holds and the earlier ones do
// not.
func markupRegions(f *webFile) []region {
	fl := flowOf(f)
	type event struct {
		pos, end int
		l        *list // the {#…} or {:…} expression; nil for a closer
		word     string
	}
	var evs []event
	for _, l := range fl.lists {
		if l.kind != sExpr || len(l.toks) < 2 || !(l.toks[0].is("#") || l.toks[0].is(":")) || l.toks[1].kind != tIdent {
			continue
		}
		w := l.toks[0].text + l.toks[1].text
		if w == ":else" && l.word(2, "if") {
			w = ":elseif"
		}
		evs = append(evs, event{pos: l.from, end: l.to, l: l, word: w})
	}
	for _, c := range f.closers {
		evs = append(evs, event{pos: c.pos, end: c.pos, word: "/" + c.word})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].pos < evs[j].pos })
	type frame struct {
		word  string
		conds []fact // the conditions so far, as they hold in the current branch
		lo    int
	}
	var stack []*frame
	var out []region
	closeBranch := func(fr *frame, at int) {
		if fr.word == "if" && at > fr.lo {
			out = append(out, region{lo: fr.lo, hi: at, facts: append([]fact(nil), fr.conds...)})
		}
	}
	for _, ev := range evs {
		switch {
		case ev.word == "#if":
			stack = append(stack, &frame{word: "if", conds: []fact{{ev.l, [2]int{2, len(ev.l.toks) - 1}, true}}, lo: ev.end})
		case strings.HasPrefix(ev.word, "#"):
			stack = append(stack, &frame{word: ev.word[1:], lo: ev.end})
		case (ev.word == ":elseif" || ev.word == ":else") && len(stack) > 0 && stack[len(stack)-1].word == "if":
			fr := stack[len(stack)-1]
			closeBranch(fr, ev.pos)
			// What held in the branch before is false from here on.
			if n := len(fr.conds); n > 0 && fr.conds[n-1].holds {
				fr.conds[n-1].holds = false
			}
			if ev.word == ":elseif" {
				fr.conds = append(fr.conds, fact{ev.l, [2]int{3, len(ev.l.toks) - 1}, true})
			}
			fr.lo = ev.end
		case strings.HasPrefix(ev.word, "/"):
			for len(stack) > 0 {
				fr := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				closeBranch(fr, ev.pos)
				if fr.word == ev.word[1:] {
					break
				}
			}
		}
	}
	for len(stack) > 0 {
		fr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		closeBranch(fr, len(f.code))
	}
	return out
}

// findCandidates lists a file's variables that start empty ($state(null),
// an empty string, false) and where they are filled with something.
func (g *roles) findCandidates(f *webFile) {
	fl := flowOf(f)
	var cands []*decl
	for _, list := range fl.byName {
		for _, d := range list {
			if d.kind != dVar || !d.top || d.fn != nil || !startsEmpty(d) {
				continue
			}
			cands = append(cands, d)
			for _, r := range fl.refs(d) {
				if fills(r.l, r.i) {
					g.sets[d] = append(g.sets[d], r.pos())
				}
			}
		}
	}
	for _, m := range bindRe.FindAllStringSubmatchIndex(f.code, -1) {
		name, prop := "", f.code[m[2]:m[3]]
		switch {
		case m[4] >= 0:
			name = f.code[m[4]:m[5]]
		case !strings.HasPrefix(strings.TrimLeft(f.code[m[3]:], " \t"), "="):
			name = prop
		}
		d := fl.resolve(name, m[0])
		if d == nil || name == "" {
			continue
		}
		// bind: on a component fills the variable with what the component
		// puts into that prop: Modal only ever closes (open = false).
		if comp := g.boundComponent(f, m[0]); comp != nil && !fillsProp(comp, prop) {
			continue
		}
		g.sets[d] = append(g.sets[d], m[0])
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].pos() < cands[j].pos() })
	g.cands[f] = cands
}

// boundComponent is the component whose tag holds the bind: at pos, or nil
// for an element of the page (an input fills what it is bound to).
func (g *roles) boundComponent(f *webFile, pos int) *webFile {
	for _, u := range g.uses {
		for _, x := range u {
			if x.f == f && pos > x.pos && pos < x.end {
				return g.web[x.src]
			}
		}
	}
	return nil
}

// fillsProp says whether a component puts something into one of its props
// (a bind: of it to another component counts).
func fillsProp(comp *webFile, prop string) bool {
	fl := flowOf(comp)
	for _, d := range fl.byName[prop] {
		if d.kind != dProp {
			continue
		}
		for _, r := range fl.refs(d) {
			if fills(r.l, r.i) {
				return true
			}
		}
	}
	return regexp.MustCompile(`bind:\w+=\{\s*` + regexp.QuoteMeta(prop) + `\s*\}|bind:` + regexp.QuoteMeta(prop) + `\b`).MatchString(comp.code)
}

// fills says whether the name at i gets a value there that may be
// something: x = y (not x = null, an empty string or false), x += …, x++,
// [a, x] = pair, ({ x } = obj), for (x of list).
func fills(l *list, i int) bool {
	if destructured(l, i) {
		return true
	}
	switch op := l.tok(i + 1); {
	case op.is("++") || op.is("--") || l.tok(i-1).is("++") || l.tok(i-1).is("--"):
		return true
	case op.kind != tPunct || !strings.HasSuffix(op.text, "=") || op.text == "==" || op.text == "===" || op.text == "!=" || op.text == "!==" || op.text == "<=" || op.text == ">=":
		return false
	case op.text == "=" || op.text == "??=" || op.text == "||=" || op.text == "&&=":
		rhs := [2]int{i + 2, l.exprEnd(i + 2)}
		return !(rhs[0] == rhs[1] && falsy(l.tok(rhs[0])))
	}
	return true // +=, -=, |= …
}

// destructured says whether the name at i is a target of a destructuring
// assignment ([a, x] = pair, ({ x } = obj), nested ones too) or the variable
// of a for…of or for…in it is not declared in: it gets a value there.
func destructured(l *list, i int) bool {
	if u := l.up[i]; u == i-1 && l.tok(u).is("(") && l.word(u-1, "for") && (l.word(i+1, "of") || l.word(i+1, "in")) {
		return true
	}
	for u := l.up[i]; u >= 0; u = l.up[u] {
		o, c := l.toks[u], l.match[u]
		// x[k] = … sets a property of x, not k: an index is no pattern.
		if c < 0 || !o.is("[") && !o.is("{") || o.is("[") && indexes(l.tok(u-1)) {
			return false
		}
		switch next := l.tok(c + 1); {
		case next.is("="):
			return true
		case !next.is(",") && !next.is("]") && !next.is("}"):
			return false // a literal, not the left side of an assignment
		}
	}
	return false
}

// indexes says whether a "[" after this token reads a property (x[k],
// f()[0]) rather than starting an array.
func indexes(prev token) bool {
	switch prev.kind {
	case tIdent:
		return !regexKeywords[prev.text] // return [a, b] starts an array
	case tStr, tTmpl, tNum:
		return true
	}
	return prev.is(")") || prev.is("]")
}

var bindRe = regexp.MustCompile(`bind:([A-Za-z_$][\w$]*)(?:\s*=\s*\{\s*([A-Za-z_$][\w$]*)\s*\})?`)

func falsy(t token) bool {
	switch t.kind {
	case tIdent:
		return t.text == "null" || t.text == "undefined" || t.text == "false"
	case tStr:
		return t.text == ""
	case tNum:
		return t.text == "0"
	}
	return false
}

// startsEmpty says whether a variable starts as nothing: no initializer,
// null, an empty string, false, or $state (or $state.raw) of one.
func startsEmpty(d *decl) bool {
	if !d.hasInit() {
		return true
	}
	l, r := d.list, d.init
	if !l.word(r[0], "$state") {
		return r[0] == r[1] && falsy(l.tok(r[0]))
	}
	callee := r[0]
	if l.tok(callee + 1).is(".") {
		callee += 2 // $state.raw
	}
	open := l.generic(callee + 1)
	if !l.tok(open).is("(") || l.closing(open) != r[1] {
		return false
	}
	a := argRanges(l, open)
	return len(a) == 0 || a[0][0] == a[0][1] && falsy(l.tok(a[0][0]))
}

// settle finds the variables filled in guarded places only: start from all
// of them, drop those filled anywhere else, until nothing changes.
func (g *roles) settle() {
	g.state = map[*decl]bool{}
	for _, list := range g.cands {
		for _, d := range list {
			g.state[d] = true
		}
	}
	for changed := true; changed; {
		changed = false
		g.memo, g.honest = map[string]bool{}, map[string]int8{}
		files := make([]*webFile, 0, len(g.cands))
		for f := range g.cands {
			files = append(files, f)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
		for _, f := range files {
			for _, d := range g.cands[f] {
				if !g.state[d] {
					continue
				}
				for _, p := range g.sets[d] {
					if !g.guarded(f, p) {
						delete(g.state, d)
						changed = true
						break
					}
				}
			}
		}
	}
	g.memo, g.honest = map[string]bool{}, map[string]int8{}
}

// guarded says whether a place of a file runs for administrators only.
func (g *roles) guarded(f *webFile, p int) bool { return g.guardedWith(f, p, nil) }

// A propFact is what a place of a component assumes about one of its props.
type propFact struct {
	name   string
	truthy bool
}

func factsKey(facts []propFact) string {
	var b strings.Builder
	for _, x := range facts {
		b.WriteString("|" + x.name + strconv.FormatBool(x.truthy))
	}
	return b.String()
}

// guardedWith is guarded for a place reached under conditions on the
// component's props (from the places on the way to it).
func (g *roles) guardedWith(f *webFile, p int, assumed []propFact) bool {
	if !g.shared(f.path) {
		return true
	}
	key := f.path + "@" + strconv.Itoa(p) + factsKey(assumed)
	if v, ok := g.memo[key]; ok {
		return v
	}
	if at, ok := g.open[key]; ok {
		// Back in a cycle, which is no way in by itself: yes, for now.
		g.low = min(g.low, at)
		return true
	}
	g.seq++
	at := g.seq
	g.open[key] = at
	mark, outer := len(g.pending), g.low
	g.low = at
	v := g.guardedNow(f, p, assumed)
	low := g.low
	switch {
	case !v:
		// No is final: taking a place in progress for yes only ever adds
		// yeses. The provisional yeses found meanwhile may have taken this
		// place for yes, so they are worked out again when asked.
		g.forget(mark)
		g.memo[key] = false
		delete(g.open, key)
		g.low = outer
	case low >= at:
		// The first place of its cycle (or of none) is decided, and so are
		// the provisional yeses that lean on it.
		for _, k := range g.pending[mark:] {
			g.memo[k] = true
			delete(g.open, k)
		}
		g.pending = g.pending[:mark]
		g.memo[key] = true
		delete(g.open, key)
		g.low = outer
	default:
		// Yes, leaning on a place still in progress: provisional.
		g.open[key] = low
		g.pending = append(g.pending, key)
		g.low = min(outer, low)
	}
	return v
}

// forget drops the provisional answers found since mark.
func (g *roles) forget(mark int) {
	for _, k := range g.pending[mark:] {
		delete(g.open, k)
	}
	g.pending = g.pending[:mark]
}

func (g *roles) guardedNow(f *webFile, p int, assumed []propFact) bool {
	known := assumed
	for _, r := range g.regionsOf(f) {
		if p < r.lo || p >= r.hi {
			continue
		}
		if g.holds(f, r) {
			return true
		}
		for _, x := range r.facts {
			if pf, ok := g.propFact(f, x); ok && !slices.Contains(known, pf) {
				known = append(append([]propFact(nil), known...), pf)
			}
		}
	}
	sort.Slice(known, func(i, j int) bool { return factsKey(known[i:i+1]) < factsKey(known[j:j+1]) })
	fl := flowOf(f)
	if fn := fl.innermost(p); fn != nil {
		if fn.decl == nil {
			return false
		}
		n := 0
		for _, r := range fl.refs(fn.decl) {
			if q := r.pos(); q < fn.lo || q >= fn.hi {
				if !g.guardedWith(f, q, known) {
					return false
				}
				n++
			}
		}
		// An exported function of a module: where the files that import it
		// use it.
		if fn.decl.export && fn.decl.top && !strings.HasSuffix(f.path, ".svelte") {
			for _, imp := range g.importers(f.path, fn.decl.name) {
				for _, r := range flowOf(imp.list.file).refs(imp) {
					if !g.guarded(imp.list.file, r.pos()) {
						return false
					}
					n++
				}
			}
		}
		return n > 0
	}
	if component(f.path) {
		us := g.uses[f.path]
		for _, u := range us {
			if !g.refuted(f, known, u) && !g.guarded(u.f, u.pos) {
				return false
			}
		}
		return len(us) > 0
	}
	return false
}

// importers are the imports of a module's export of that name, in the
// files that import it ('./lib' is lib.ts, lib.js or lib/index.ts).
func (g *roles) importers(module, name string) []*decl {
	var out []*decl
	for _, n := range sortedKeys(g.web) {
		for _, list := range flowOf(g.web[n]).byName {
			for _, d := range list {
				if d.kind != dImport || d.origin != name || d.from == "" {
					continue
				}
				for _, ext := range []string{"", ".ts", ".js", "/index.ts", "/index.js"} {
					if d.from+ext == module {
						out = append(out, d)
					}
				}
			}
		}
	}
	return out
}

// propFact reads a fact about a prop of the component: login (truthy),
// !login, !!open.
func (g *roles) propFact(f *webFile, x fact) (propFact, bool) {
	l, r, truthy := x.l, x.l.trim(x.cond), x.holds
	for r[0] < r[1] && l.tok(r[0]).is("!") {
		truthy = !truthy
		r[0]++
	}
	t := l.tok(r[0])
	if r[0] != r[1] || t.kind != tIdent || l.member(r[0]) {
		return propFact{}, false
	}
	if d := flowOf(f).resolve(t.text, t.pos); d == nil || d.kind != dProp {
		return propFact{}, false
	}
	return propFact{t.text, truthy}, true
}

// refuted says whether a usage of a component rules out the conditions on
// its props that a place assumes: login={me.login} for a place reached only
// without login. A value other than a falsy literal counts as set, except a
// name that may well hold nothing (see mayBeEmpty); a prop left out has its
// default.
func (g *roles) refuted(comp *webFile, facts []propFact, u usage) bool {
	for _, x := range facts {
		a := u.attr(x.name)
		var set bool
		switch {
		case a.spread || a.bound:
			continue // unknown, or changing
		case a.bare:
			set = true
		case a.given && a.value != nil && len(a.value.toks) == 1 && falsy(a.value.toks[0]):
			set = false
		case a.given && !x.truthy && !mayBeEmpty(a.value):
			// login={me.login}: an expression counts as set, so a place
			// reached only without the prop is out of reach here.
			set = true
		case a.given:
			continue // whether an expression is empty is not in the code
		default:
			set = propDefaultSet(comp, x.name)
		}
		if set != x.truthy {
			return true
		}
	}
	return false
}

// mayBeEmpty says whether an attribute's value is a name that can hold
// nothing while the page shows the component: a variable that starts empty
// (let picked = $state(null)), a prop or a parameter, whose value comes from
// elsewhere. A property (me.login) or any other expression counts as set.
func mayBeEmpty(v *list) bool {
	if v == nil || len(v.toks) != 1 || v.toks[0].kind != tIdent {
		return false
	}
	d := flowOf(v.file).resolve(v.toks[0].text, v.toks[0].pos)
	return d == nil || d.kind != dVar && d.kind != dFunc || d.kind == dVar && startsEmpty(d)
}

// propDefaultSet says whether a prop's default is something (not an empty
// string, 0, false, null or none).
func propDefaultSet(comp *webFile, name string) bool {
	for _, d := range flowOf(comp).byName[name] {
		if d.kind != dProp {
			continue
		}
		l := d.list
		if !l.tok(d.at + 1).is("=") {
			return false
		}
		e := d.at + 2
		for e < len(l.toks) && !l.tok(e).is(",") && !l.closesAt(e) {
			e = l.after(e)
		}
		if e == d.at+3 && falsy(l.tok(d.at+2)) {
			return false
		}
		// $bindable(false), $bindable(null), $bindable()
		if l.word(d.at+2, "$bindable") {
			a := argRanges(l, d.at+3)
			return len(a) > 0 && !(a[0][0] == a[0][1] && falsy(l.tok(a[0][0])))
		}
		return true
	}
	return false
}

// regionsOf are the conditional regions of a file, with the content of the
// components it uses that render their content only under a prop.
func (g *roles) regionsOf(f *webFile) []region {
	if r, ok := g.regions[f]; ok {
		return r
	}
	if g.busy["regions "+f.path] {
		return scanRegions(f)
	}
	g.busy["regions "+f.path] = true
	defer delete(g.busy, "regions "+f.path)
	out := scanRegions(f)
	for _, c := range sortedKeys(g.uses) {
		for _, u := range g.uses[c] {
			if u.f != f {
				continue
			}
			prop := g.gate(c)
			if prop == "" {
				continue
			}
			a := u.attr(prop)
			if a.value == nil || a.spread {
				continue
			}
			if end := closeTag(f, u); end > u.end {
				out = append(out, region{lo: u.end, hi: end, facts: []fact{{a.value, [2]int{0, len(a.value.toks) - 1}, true}}})
			}
		}
	}
	g.regions[f] = out
	return out
}

// gate is the prop a component renders all its content under, if any:
// every {@render …} of it sits where that prop holds (Modal: {#if open}).
func (g *roles) gate(c string) string {
	if p, ok := g.gates[c]; ok {
		return p
	}
	f := g.web[c]
	if f == nil || g.busy["gate "+c] {
		return ""
	}
	g.busy["gate "+c] = true
	defer delete(g.busy, "gate "+c)
	var common []string
	renders := 0
	for _, l := range flowOf(f).lists {
		if l.kind != sExpr || !l.tok(0).is("@") || !l.word(1, "render") {
			continue
		}
		var here []string
		for _, r := range g.regionsOf(f) {
			if l.from < r.lo || l.from >= r.hi {
				continue
			}
			for _, x := range r.facts {
				if pf, ok := g.propFact(f, x); ok && pf.truthy {
					here = append(here, pf.name)
				}
			}
		}
		if renders == 0 {
			common = here
		} else {
			common = slices.DeleteFunc(common, func(p string) bool { return !slices.Contains(here, p) })
		}
		renders++
	}
	p := ""
	if renders > 0 && len(common) > 0 {
		sort.Strings(common)
		p = common[0]
	}
	g.gates[c] = p
	return p
}

// closeTag is the offset of the </Name> that closes a component's tag, or
// -1 for a tag that closes itself or never does.
func closeTag(f *webFile, u usage) int {
	if u.end < 2 || f.code[u.end-2] == '/' {
		return -1
	}
	m := tagName.FindStringSubmatch(f.code[u.pos:])
	if m == nil {
		return -1
	}
	name := m[1]
	re := regexp.MustCompile(`<(/?)` + regexp.QuoteMeta(name) + `[\s/>]`)
	fl := flowOf(f)
	depth := 1
	for _, loc := range re.FindAllStringSubmatchIndex(f.code[u.end:], -1) {
		at := u.end + loc[0]
		if fl.within(at, sScript) || fl.within(at, sExpr) || fl.within(at, sAttr) {
			continue
		}
		if loc[3] > loc[2] { // </Name
			depth--
			if depth == 0 {
				return at
			}
			continue
		}
		if end := fl.tagEnd(at); end >= 2 && f.code[end-2] != '/' {
			depth++
		}
	}
	return -1
}

// holds says whether a region's facts imply the administrator role.
func (g *roles) holds(f *webFile, r region) bool {
	for _, x := range r.facts {
		if x.holds && g.pos(f, x.l, x.cond) || !x.holds && g.neg(f, x.l, x.cond) {
			return true
		}
	}
	return false
}

// pos says whether a condition, when true, implies the administrator role.
func (g *roles) pos(f *webFile, l *list, r [2]int) bool {
	r = l.trim(r)
	if r[0] > r[1] {
		return false
	}
	if parts := l.split(r, "||"); len(parts) > 1 {
		for _, p := range parts {
			if !g.pos(f, l, p) {
				return false
			}
		}
		return true
	}
	if parts := l.split(r, "&&"); len(parts) > 1 {
		for _, p := range parts {
			if g.pos(f, l, p) {
				return true
			}
		}
		return false
	}
	if l.tok(r[0]).is("!") {
		return g.neg(f, l, [2]int{r[0] + 1, r[1]})
	}
	if op, a, b := comparison(l, r); op == "===" || op == "==" {
		return roleCompare(l, a, b) || g.stateChain(f, l, a) && literalValue(l, b) || g.stateChain(f, l, b) && literalValue(l, a)
	} else if op != "" {
		return false
	}
	return g.stateChain(f, l, r) || r[0] == r[1] && g.roleName(f, l, r[0])
}

// neg says whether a condition, when false, implies the administrator role.
func (g *roles) neg(f *webFile, l *list, r [2]int) bool {
	r = l.trim(r)
	if r[0] > r[1] {
		return false
	}
	if parts := l.split(r, "||"); len(parts) > 1 {
		for _, p := range parts {
			if g.neg(f, l, p) {
				return true
			}
		}
		return false
	}
	if parts := l.split(r, "&&"); len(parts) > 1 {
		for _, p := range parts {
			if !g.neg(f, l, p) {
				return false
			}
		}
		return true
	}
	if l.tok(r[0]).is("!") {
		return g.pos(f, l, [2]int{r[0] + 1, r[1]})
	}
	if op, a, b := comparison(l, r); op == "!==" || op == "!=" {
		return roleCompare(l, a, b)
	}
	return false
}

// comparison splits a === b (or ==, !==, !=) at the top of a range.
func comparison(l *list, r [2]int) (op string, a, b [2]int) {
	for k := r[0]; k <= r[1]; k = l.after(k) {
		if t := l.tok(k); t.kind == tPunct && (t.text == "===" || t.text == "==" || t.text == "!==" || t.text == "!=") {
			return t.text, l.trim([2]int{r[0], k - 1}), l.trim([2]int{k + 1, r[1]})
		}
	}
	return "", none, none
}

// roleCompare: one side is the string 'admin', the other the caller's role.
func roleCompare(l *list, a, b [2]int) bool {
	is := func(x [2]int) bool { return x[0] == x[1] && l.tok(x[0]).kind == tStr && l.tok(x[0]).text == "admin" }
	return is(a) && callerRole(l, b) || is(b) && callerRole(l, a)
}

// callerRole says whether a range is auth.me.role (?. anywhere), auth being
// the signed-in principal of $lib/state.svelte. The role of anything else —
// a row of the accounts table, the account a dialog is about — says nothing
// about who looks at the page.
func callerRole(l *list, x [2]int) bool {
	dot := func(i int) bool { return l.tok(i).is(".") || l.tok(i).is("?.") }
	if x[1]-x[0] != 4 || !l.word(x[0], "auth") || l.member(x[0]) || !dot(x[0]+1) || !l.word(x[0]+2, "me") || !dot(x[0]+3) || !l.word(x[0]+4, "role") {
		return false
	}
	d := flowOf(l.file).resolve("auth", l.tok(x[0]).pos)
	switch {
	case d == nil:
		return false
	case d.kind == dImport:
		return d.origin == "auth" && (d.from == "lib/state.svelte" || d.from == "lib/state.svelte.ts")
	}
	// The module itself.
	return d.top && strings.HasPrefix(l.file.path, "lib/state.svelte")
}

// literalValue: a string, a number or true — a value a missing object never
// equals.
func literalValue(l *list, x [2]int) bool {
	t := l.tok(x[0])
	return x[0] == x[1] && (t.kind == tStr || t.kind == tNum || t.kind == tIdent && t.text == "true")
}

// roleName says whether the name at i is the role: a name the file derives
// from role === 'admin', or an honest admin prop.
func (g *roles) roleName(f *webFile, l *list, i int) bool {
	t := l.tok(i)
	if t.kind != tIdent || l.member(i) {
		return false
	}
	d := flowOf(f).resolve(t.text, t.pos)
	switch {
	case d == nil:
		return false
	case d.kind == dVar && d.top && d.hasInit():
		// const admin = $derived(auth.me?.role === 'admin'), or the
		// comparison alone: nothing else, so that the name is the role.
		dl, r := d.list, d.list.trim(d.init)
		if dl.word(r[0], "$derived") && dl.tok(r[0]+1).is("(") && dl.closing(r[0]+1) == r[1] {
			r = dl.trim([2]int{r[0] + 2, r[1] - 1})
		}
		op, a, b := comparison(dl, r)
		return (op == "===" || op == "==") && roleCompare(dl, a, b)
	case d.kind == dProp && d.name == "admin":
		return g.honestAdmin(f)
	}
	return false
}

// stateChain says whether a range is a variable filled in guarded places
// only, or a property chain on it (del, del.login, del?.unix_uid, !!del).
func (g *roles) stateChain(f *webFile, l *list, r [2]int) bool {
	for r[0] < r[1] && l.tok(r[0]).is("!") && l.tok(r[0]+1).is("!") {
		r[0] += 2
	}
	t := l.tok(r[0])
	if t.kind != tIdent || l.member(r[0]) {
		return false
	}
	for k := r[0] + 1; k <= r[1]; {
		switch {
		case (l.tok(k).is(".") || l.tok(k).is("?.")) && l.tok(k+1).kind == tIdent:
			k += 2
		case l.tok(k).is("["):
			k = l.after(k)
		default:
			return false
		}
	}
	d := flowOf(f).resolve(t.text, t.pos)
	return d != nil && g.state[d]
}

// honestAdmin says whether a component's admin prop is the role: every
// user of the component leaves it out (when its default is false), passes
// the role on (admin={admin}) or sets it only in guarded places.
func (g *roles) honestAdmin(f *webFile) bool {
	switch g.honest[f.path] {
	case 1, 3:
		return false
	case 2:
		return true
	}
	g.honest[f.path] = 1
	start, outer := g.seq, g.low
	g.low = math.MaxInt
	ok := len(g.uses[f.path]) > 0
	for _, u := range g.uses[f.path] {
		a := u.attr("admin")
		v := a.value
		switch {
		case a.spread || a.bound:
			ok = false
		case !a.given:
			// The default holds: let { admin = true } is an administrator
			// wherever the attribute is left out.
			ok = ok && (!propDefaultSet(f, "admin") || g.guarded(u.f, u.pos))
		case a.bare, len(v.toks) == 1 && v.toks[0].kind == tIdent && v.toks[0].text == "true":
			ok = ok && g.guarded(u.f, u.pos)
		case len(v.toks) == 1 && falsy(v.toks[0]):
		default:
			ok = ok && g.pos(u.f, v, [2]int{0, len(v.toks) - 1})
		}
	}
	// A yes that took a place still in progress for guarded is provisional
	// (see open): it is worked out again when asked.
	leaned := g.low <= start
	g.low = min(outer, g.low)
	switch {
	case ok && leaned:
		delete(g.honest, f.path)
	case ok:
		g.honest[f.path] = 2
	default:
		g.honest[f.path] = 3
	}
	return ok
}
