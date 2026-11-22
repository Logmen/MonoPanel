package parity

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"monopanel/internal/tui"
)

// The checks that follow values: what the web UI's calls send, whether its
// calls and fields for administrators are behind a check of the role on the
// pages ordinary accounts open too, and whether the TUI keeps the
// administrators' flags out of ordinary accounts' forms.

// A found is a call of an operation at one of its manifest places.
type found struct {
	f *webFile
	c webCall
}

// opCalls are the calls of an operation at the places the manifest gives
// it, and the files of the places where no call can be pinned down (a path
// handed to a component, a pattern): there a field counts when the file
// names it.
func (w *world) opCalls(a apiOp, m Op) (calls []found, loose []*webFile) {
	seen := map[string]bool{}
	for _, loc := range m.Web {
		files, _ := w.files(loc.In)
		if loc.Loose || loc.Pattern != "" {
			loose = append(loose, files...)
			continue
		}
		for _, f := range files {
			for _, c := range f.calls {
				ok := reaches(w.ops, c.method, c.segs, a) ||
					loc.Dyn != "" && (c.method == "" || c.method == a.method) && a.fits(c.segs, loc.Dyn) && f.hasString(loc.Dyn)
				key := fmt.Sprintf("%s:%d:%s", f.path, c.pos, c.method)
				if ok && !seen[key] {
					seen[key] = true
					calls = append(calls, found{f, c})
				}
			}
		}
	}
	return calls, loose
}

// sent reads what a call sends in its body or query string, once.
func (w *world) sent(fc found, in string) keys {
	key := fmt.Sprintf("%s:%d:%s:%s", fc.f.path, fc.c.pos, fc.c.method, in)
	if k, ok := w.sentCache[key]; ok {
		return k
	}
	var k keys
	if in == "body" {
		k, _ = bodyOf(fc.f, fc.c)
	} else {
		k = queryOf(fc.f, fc.c)
	}
	if w.sentCache == nil {
		w.sentCache = map[string]keys{}
	}
	w.sentCache[key] = k
	return k
}

// webFieldProblems: every body field and query parameter of an operation is
// sent by a call of it at its places in the web UI — a key of the body or
// of the query string the call builds, followed through variables and
// functions (webbody.go). Only where the reading cannot follow the value
// (an import, a prop, a callback) does the field count when the file names
// it.
func (w *world) webFieldProblems() []string {
	p := &problems{}
	for _, m := range w.man {
		a, ok := w.byID[m.ID]
		if !ok || len(m.Web) == 0 {
			continue
		}
		calls, loose := w.opCalls(a, m)
		if len(calls) == 0 && len(loose) == 0 {
			continue // no call at all: the web check says so
		}
		for _, in := range w.inputs[m.ID] {
			if in.in == "path" || ignored(m.ID, in.name) {
				continue // part of the call's path, or nothing to set
			}
			if w.sends(calls, loose, in) {
				continue
			}
			var where, sends []string
			for _, fc := range calls {
				where = append(where, "web/src/"+fc.f.path)
				k := w.sent(fc, in.in)
				what := strings.Join(k.names(), ", ")
				if what == "" {
					what = "nothing"
				}
				if len(k.open) > 0 {
					what += " (and what " + strings.Join(dedupe(k.open), "; ") + " holds, which does not name it either)"
				}
				sends = append(sends, fc.c.where()+" "+fc.c.method+" sends "+what)
			}
			for _, f := range loose {
				where = append(where, "web/src/"+f.path)
			}
			also := ""
			if len(m.CLI) > 0 {
				also = " (mp can)"
			}
			kind, fix := "request body", "make it a key of the body one of them builds"
			if in.in == "query" {
				kind, fix = "query string", "add it to the query string of one of them"
			}
			msg := fmt.Sprintf("%s (%s): %s is in the %s of no call of it in %s — the web UI cannot set it%s; %s",
				m.ID, m.at, in, kind, strings.Join(dedupe(where), ", "), also, fix)
			if len(sends) > 0 {
				if len(sends) > 6 {
					sends = append(sends[:6:6], fmt.Sprintf("… %d more", len(sends)-6))
				}
				msg += ".\n    The calls: " + strings.Join(sends, "\n               ")
			}
			p.add("%s", msg)
		}
	}
	return p.list
}

// sends says whether one of the calls sends a field.
func (w *world) sends(calls []found, loose []*webFile, in input) bool {
	for _, fc := range calls {
		k := w.sent(fc, in.in)
		if len(k.at[in.name]) > 0 || len(k.open) > 0 && fc.f.hasWord(in.name) {
			return true
		}
	}
	for _, f := range loose {
		if f.hasWord(in.name) {
			return true
		}
	}
	return false
}

// webRoleProblems: on the pages ordinary accounts open too (the
// navigation's list, the layout, lib/), every call of an operation only
// administrators may make is behind a check of the role (webroles.go): so is
// the computed value that picks such an operation (the 'suspend' of
// /sites/${domain}/${action}), and so are the fields only administrators
// may set (adminField(…)) — where a request is built from something the
// reading cannot follow (webbody.go), its whole call has to be.
func (w *world) webRoleProblems() []string {
	p := &problems{}
	g, why := newRoles(w.web)
	if g == nil {
		p.add("%s", why)
		return p.list
	}
	names := make([]string, 0, len(w.web))
	for n := range w.web {
		names = append(names, n)
	}
	sort.Strings(names)
	advice := "put it inside the page's {#if admin} or behind if (admin), as its other administrator controls are"
	for _, n := range names {
		f := w.web[n]
		if !g.shared(f.path) {
			continue
		}
		for _, c := range f.calls {
			if nonOperations.MatchString(segsString(c.segs)) {
				continue
			}
			direct := route(w.ops, c.method, c.segs)
			for _, o := range direct {
				if w.adminOnly(o.id) && !g.guarded(f, c.pos) {
					p.add("%s (%s): the web UI calls %s, which only administrators may, at web/src/%s — a page ordinary accounts open too — with no check of the role on the way there: %s",
						o.id, w.placeOf(o.id), o, c.where(), advice)
				}
			}
			for _, o := range mayReach(w.ops, c.method, c.segs) {
				if !w.adminOnly(o.id) || slices.ContainsFunc(direct, func(d apiOp) bool { return d.id == o.id }) {
					continue
				}
				for k, s := range c.segs {
					if k == 0 || s != "" || o.segs[k] == param {
						continue
					}
					for _, at := range literalsOf(f, o.segs[k]) {
						if !g.guarded(f, at) {
							p.add("%s (%s): web/src/%s picks %s, which only administrators may call, with the value '%s' written at web/src/%s:%d — a page ordinary accounts open too — with no check of the role on the way there: %s",
								o.id, w.placeOf(o.id), c.where(), o, o.segs[k], f.path, f.line(at), advice)
						}
					}
				}
			}
		}
	}
	for _, m := range w.man {
		a, ok := w.byID[m.ID]
		if !ok || len(m.AdminFields) == 0 {
			continue
		}
		for _, n := range names {
			f := w.web[n]
			if !g.shared(f.path) {
				continue
			}
			for _, c := range f.calls {
				if !reaches(w.ops, c.method, c.segs, a) {
					continue
				}
				for _, in := range w.inputs[m.ID] {
					if m.AdminFields[in.name] == "" || in.in == "path" {
						continue
					}
					k := w.sent(found{f, c}, in.in)
					for _, at := range k.at[in.name] {
						if !g.guarded(f, at) {
							p.add("%s (%s): web/src/%s:%d sets %s of %s — only administrators may (%s) — on a page ordinary accounts open too, with no check of the role on the way there: %s",
								m.ID, m.at, f.path, f.line(at), in, a, m.AdminFields[in.name], advice)
						}
					}
					// What the reading cannot follow may set the field too, and
					// then nothing shows where: unless the whole call is for
					// administrators, that is no answer.
					if len(k.open) > 0 && !g.guarded(f, c.pos) {
						what := "request body"
						if in.in == "query" {
							what = "query string"
						}
						p.add("%s (%s): web/src/%s %s builds its %s from what the check cannot follow (%s), so it cannot tell whether %s — only administrators may set it (%s) — is set only behind a check of the role, on a page ordinary accounts open too: build the request in the page, the field behind if (admin)",
							m.ID, m.at, c.where(), a, what, strings.Join(dedupe(slices.Clone(k.open)), "; "), in, m.AdminFields[in.name])
					}
				}
			}
		}
	}
	return p.list
}

// literalsOf are the offsets of the string literals of a file with that
// text.
func literalsOf(f *webFile, text string) []int {
	var out []int
	for _, l := range flowOf(f).all {
		for _, t := range l.toks {
			if t.literal() && t.text == text {
				out = append(out, t.pos)
			}
		}
	}
	sort.Ints(out)
	return out
}

// An adminFlag is a flag of a command that only administrators may use.
type adminFlag struct {
	flag, why string
}

// adminFlags are the flags of a command that set what only administrators
// may: --user (acting for another account), the fields of its operations
// for administrators when the command serves ordinary accounts as well
// (site nginx --set), and the fields marked adminField(…).
func (w *world) adminFlags(path string) []adminFlag {
	c := w.all[path]
	if c == nil {
		return nil
	}
	flags := w.flagsOf(c)
	var out []adminFlag
	seen := map[string]bool{}
	addFlag := func(name, why string) {
		if flags[name] != nil && !seen[name] {
			seen[name] = true
			out = append(out, adminFlag{name, why})
		}
	}
	ops := w.opsOf[path]
	addFlag("user", "it acts for another account, which only administrators may ("+w.entriesAt(ops)+")")
	for _, id := range ops {
		m := w.entries[id]
		for _, in := range w.inputs[id] {
			if in.in == "path" {
				continue
			}
			why := m.AdminFields[in.name]
			if why == "" && w.adminOnly(id) && !w.allAdmin(ops) {
				why = fmt.Sprintf("it sets %s of %s, which only administrators may call (%s)", in, id, m.at)
			} else if why != "" {
				why = fmt.Sprintf("it sets %s of %s, which only administrators may: %s (%s)", in, id, why, m.at)
			}
			if why == "" {
				continue
			}
			for _, name := range w.flagNames(m, in) {
				addFlag(name, why)
			}
		}
	}
	return out
}

// flagNames are the flags that may set a request field: the manifest's
// field(…) flags, or the default names.
func (w *world) flagNames(m Op, in input) []string {
	var out []string
	for _, h := range m.Fields[in.name] {
		if h.kind == howFlag {
			out = append(out, h.names...)
		}
	}
	out = append(out, kebab(in.name))
	if in.typ == "boolean" {
		out = append(out, "no-"+kebab(in.name))
	}
	if in.name == "login" {
		out = append(out, "user")
	}
	return out
}

// tuiFlagProblems: the forms the TUI's menus open for an ordinary account
// leave out the flags only administrators may use, and an administrator's
// forms keep them.
func (w *world) tuiFlagProblems() []string {
	p := &problems{}
	if w.boundFlags == nil {
		return nil
	}
	const panel = "https://panel.example:8443"
	for _, mode := range []struct {
		opts  tui.Options
		label string
	}{{tui.Options{}, "on the server"}, {tui.Options{Server: panel}, "with --server"}} {
		for _, path := range w.boundCommands(w.root, mode.opts, "user") {
			afs := w.adminFlags(path)
			if len(afs) == 0 {
				continue
			}
			user := w.boundFlags(w.root, mode.opts, "user", path)
			admin := w.boundFlags(w.root, mode.opts, "admin", path)
			for _, af := range afs {
				if slices.Contains(user, af.flag) {
					p.add("mp %s --%s: the TUI offers it to ordinary accounts (%s), but %s: keep it out of their form as the web UI keeps the control from them", path, af.flag, mode.label, af.why)
				}
				if !slices.Contains(admin, af.flag) {
					p.add("mp %s --%s: the TUI's form leaves it out for administrators too (%s); %s, and administrators must be able to set it", path, af.flag, mode.label, af.why)
				}
			}
		}
	}
	return p.list
}

// placeOf is where the manifest describes an operation, for messages.
func (w *world) placeOf(id string) string {
	if m, ok := w.entries[id]; ok && m.at != "" {
		return m.at
	}
	return "not in the manifest"
}

// entriesAt names operations with their manifest places, for messages.
func (w *world) entriesAt(ids []string) string {
	var out []string
	for _, id := range ids {
		if m, ok := w.entries[id]; ok {
			out = append(out, id+" "+m.at)
		} else {
			out = append(out, id)
		}
	}
	return strings.Join(out, ", ")
}

// TestTUIFlagsOnASmallWorld runs the tui-flags check on a made-up command
// tree and TUI: a command shared by an operation for everyone and one for
// administrators (thing show --set), fields only administrators may set
// (adminField) and --user.
func TestTUIFlagsOnASmallWorld(t *testing.T) {
	ops := []apiOp{
		newAPIOp("thing-get", "GET", "/things/{name}", false),
		newAPIOp("thing-set", "PUT", "/things/{name}", true),
		newAPIOp("thing-create", "POST", "/things", false),
	}
	inputs := map[string][]input{
		"thing-set":    {{name: "name", in: "path", typ: "string"}, {name: "custom", in: "body", typ: "string"}},
		"thing-create": {{name: "name", in: "body", typ: "string"}, {name: "owner", in: "body", typ: "string"}, {name: "shell", in: "body", typ: "boolean"}},
	}
	run := func(*cobra.Command, []string) {}
	root := &cobra.Command{Use: "mp"}
	thing := &cobra.Command{Use: "thing"}
	show := &cobra.Command{Use: "show <name>", Run: run}
	show.Flags().String("set", "", "")
	show.Flags().Bool("clear", false, "")
	add := &cobra.Command{Use: "add <name>", Run: run}
	add.Flags().String("user", "", "")
	add.Flags().String("owner", "", "")
	add.Flags().Bool("shell", false, "")
	add.Flags().Bool("sftp-only", false, "")
	add.Flags().Bool("www", false, "")
	thing.AddCommand(show, add)
	root.AddCommand(thing)
	man := []Op{
		op("thing-get", cli("thing show"), web()),
		op("thing-set", cli("thing show"), web(), field("custom", flag("set", "clear"))),
		op("thing-create", cli("thing add"), web(), field("shell", flag("shell", "sftp-only")), adminField("the handler ignores it", "owner", "shell")),
	}
	w := newWorld(ops, map[string]string{}, inputs, root, map[string]*webFile{}, man)
	w.boundCommands = func(*cobra.Command, tui.Options, string) []string { return []string{"thing add", "thing show"} }
	w.boundFlags = func(_ *cobra.Command, _ tui.Options, role, path string) []string {
		switch {
		case role == "admin" && path == "thing add":
			return []string{"owner", "shell", "sftp-only", "www"} // --user is left out for administrators too
		case role == "admin":
			return []string{"clear", "set"}
		case path == "thing add":
			return []string{"sftp-only", "www"} // --sftp-only sets shell
		default:
			return []string{"set"} // PUT /things/{name} is for administrators
		}
	}
	got := w.tuiFlagProblems()
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"mp thing add --sftp-only: the TUI offers it to ordinary accounts (on the server), but it sets body shell of thing-create, which only administrators may: the handler ignores it (checks_flow_test.go:",
		"mp thing add --sftp-only: the TUI offers it to ordinary accounts (with --server)",
		"mp thing show --set: the TUI offers it to ordinary accounts (on the server), but it sets body custom of thing-set, which only administrators may call",
		"mp thing add --user: the TUI's form leaves it out for administrators too (on the server); it acts for another account",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no problem mentions %q; got:\n%s", want, all)
		}
	}
	if len(got) != 6 {
		t.Errorf("%d problems, want 6 (three, on the server and with --server):\n%s", len(got), all)
	}
	if w.boundFlags = nil; len(w.tuiFlagProblems()) != 0 {
		t.Error("without the TUI's forms there is nothing to compare")
	}
}

// An ignoredFields entry stops the field checks for its field only, and is
// reported once the API no longer has the field.
func TestIgnoredFields(t *testing.T) {
	saved := ignoredFields
	defer func() { ignoredFields = saved }()
	ignoredFields = map[string]string{"thing-create owner": "ignored", "thing-create gone": "removed from the API"}
	ops := []apiOp{newAPIOp("thing-create", "POST", "/things", false)}
	inputs := map[string][]input{"thing-create": {{name: "owner", in: "body", typ: "string"}, {name: "name", in: "body", typ: "string"}}}
	root := &cobra.Command{Use: "mp"}
	add := &cobra.Command{Use: "add", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(add)
	page := parseWebFile("routes/x/+page.svelte", `<script>api('/things', { method: 'POST', json: { size: 1 } });</script>`)
	w := newWorld(ops, map[string]string{"thing-create": "things"}, inputs, root, map[string]*webFile{page.path: page},
		[]Op{op("thing-create", cli("add"), web("routes/x/"))})
	all := strings.Join(append(append(w.manifestProblems(), w.cliFieldProblems()...), w.webFieldProblems()...), "\n")
	for _, want := range []string{"ignoredFields (parity_test.go): thing-create has no request field gone any more", "body name cannot be set with mp add", "body name is in the request body of no call"} {
		if !strings.Contains(all, want) {
			t.Errorf("no problem mentions %q; got:\n%s", want, all)
		}
	}
	if strings.Contains(all, "body owner") {
		t.Errorf("owner is ignored by the handler, so no client needs to set it:\n%s", all)
	}
}
