package parity

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"monopanel/internal/tui"
)

// The checks compare the API, the command tree, the TUI's menus and the web
// UI's sources through the manifest. Each returns its problems, worded for
// the person who has to fix them: which operation or command, which client,
// what was expected and what is there instead.

// nonOperations are pages under /api/v1 that are not operations: the API
// reference the web UI links to.
var nonOperations = regexp.MustCompile(`^/(docs|openapi[^/]*|schemas)(/|$)`)

// world is what the checks compare: the API, the command tree, the web UI.
type world struct {
	ops    []apiOp
	byID   map[string]apiOp
	tags   map[string]string
	inputs map[string][]input // per operation

	root    *cobra.Command
	all     map[string]*cobra.Command // every command by path ("site add")
	visible []string                  // runnable commands nobody hid, sorted

	web map[string]*webFile // by path under web/src

	man     []Op
	entries map[string]Op       // the manifest by operation id
	opsOf   map[string][]string // command path → operations the manifest maps to it

	// boundCommands is tui.BoundCommands; tests of the checks fake it.
	boundCommands func(root *cobra.Command, opts tui.Options, role string) []string
	// boundFlags is the TUI's form of a command for a role (tuiflags_test.go).
	boundFlags func(root *cobra.Command, opts tui.Options, role, path string) []string

	sentCache map[string]keys // what each call sends, read once
}

// newWorld indexes what the checks compare.
func newWorld(ops []apiOp, tags map[string]string, inputs map[string][]input, root *cobra.Command, web map[string]*webFile, man []Op) *world {
	w := &world{ops: ops, byID: map[string]apiOp{}, tags: tags, inputs: inputs, root: root, all: map[string]*cobra.Command{},
		web: web, man: man, entries: map[string]Op{}, opsOf: map[string][]string{}, boundCommands: tui.BoundCommands, boundFlags: tuiBoundFlags}
	sort.Slice(w.ops, func(i, j int) bool { return w.ops[i].id < w.ops[j].id })
	for _, a := range w.ops {
		w.byID[a.id] = a
	}
	var walk func(c *cobra.Command, hidden bool)
	walk = func(c *cobra.Command, hidden bool) {
		hidden = hidden || c.Hidden
		if c != root {
			p := cmdPath(c)
			w.all[p] = c
			if c.Runnable() && !hidden {
				w.visible = append(w.visible, p)
			}
		}
		for _, s := range c.Commands() {
			walk(s, hidden)
		}
	}
	walk(root, false)
	sort.Strings(w.visible)
	for _, m := range man {
		if _, dup := w.entries[m.ID]; !dup {
			w.entries[m.ID] = m
		}
		for _, p := range m.CLI {
			if !slices.Contains(w.opsOf[p], m.ID) {
				w.opsOf[p] = append(w.opsOf[p], m.ID)
			}
		}
	}
	return w
}

// top is the top-level command of a path: "completion" for "completion bash".
func top(path string) string {
	t, _, _ := strings.Cut(path, " ")
	return t
}

// input is a request field: a path or query parameter or a body property.
type input struct {
	name string
	in   string // path, query or body
	typ  string // JSON schema type
	enum []string
}

func (in input) String() string { return in.in + " " + in.name }

// cmdPath is the command path without "mp": "site add".
func cmdPath(c *cobra.Command) string {
	return strings.TrimSpace(strings.TrimPrefix(c.CommandPath(), c.Root().Name()))
}

func (w *world) hidden(c *cobra.Command) bool {
	for x := c; x != nil; x = x.Parent() {
		if x.Hidden {
			return true
		}
	}
	return false
}

// problems collects the failures of one check, to report them together.
type problems struct{ list []string }

func (p *problems) add(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

// manifestProblems: every operation of the API is in the manifest exactly once,
// in the file of its tag, and the manifest knows no other operations.
func (w *world) manifestProblems() []string {
	p := &problems{}
	seen := map[string]Op{}
	for _, m := range w.man {
		if prev, dup := seen[m.ID]; dup {
			p.add("%s is listed twice: %s and %s", m.ID, prev.at, m.at)
			continue
		}
		seen[m.ID] = m
		a, ok := w.byID[m.ID]
		if !ok {
			p.add("%s (%s) is not an operation of the API: no operationId %q in the OpenAPI document; fix the id or remove the entry", m.ID, m.at, m.ID)
			continue
		}
		if want := "manifest_" + w.tags[m.ID] + ".go"; !strings.HasPrefix(m.at, want+":") {
			p.add("%s (%s) has the tag %q: it belongs in %s", m.ID, m.at, w.tags[m.ID], want)
		}
		switch {
		case len(m.CLI) == 0 && m.NoCLI == "":
			p.add("%s (%s, %s): name the mp commands that reach it (cli(…))", m.ID, m.at, a)
		case len(m.CLI) > 0 && m.NoCLI != "":
			p.add("%s (%s): has mp commands and a reason for having none (NoCLI); keep one", m.ID, m.at)
		case m.NoCLI != "" && !slices.Contains(allowedNoCLI, m.ID):
			p.add("%s (%s): only the deliberate exceptions may have no mp command (%s); this one must get a command", m.ID, m.at, strings.Join(allowedNoCLI, ", "))
		}
		switch {
		case len(m.Web) == 0 && m.NoWeb == "":
			p.add("%s (%s, %s): name the web UI files that call it (web(…))", m.ID, m.at, a)
		case len(m.Web) > 0 && m.NoWeb != "":
			p.add("%s (%s): has web UI places and a reason for having none (NoWeb); keep one", m.ID, m.at)
		case m.NoWeb != "" && !slices.Contains(allowedNoWeb, m.ID):
			p.add("%s (%s): only the deliberate exceptions may be missing from the web UI (%s); this one must get a place", m.ID, m.at, strings.Join(allowedNoWeb, ", "))
		}
		switch {
		case m.Admin != "" && m.AdminPage != "":
			p.add("%s (%s): admin(…) and adminPage(…) contradict each other; keep one", m.ID, m.at)
		case m.Admin != "" && a.admin:
			p.add("%s (%s): the API marks it adminOnly now; drop admin(…)", m.ID, m.at)
		case m.Admin != "" && !slices.Contains(allowedAdmin, m.ID):
			p.add("%s (%s): admin(…) keeps a command from ordinary accounts; only %s may say so without adminOnly in the API (mark the operation adminOnly there instead)", m.ID, m.at, strings.Join(allowedAdmin, ", "))
		case m.AdminPage != "" && a.admin:
			p.add("%s (%s): adminPage(…) is for operations ordinary accounts may call, and this one is adminOnly; drop it", m.ID, m.at)
		case m.AdminPage != "" && !slices.Contains(allowedAdminPage, m.ID):
			p.add("%s (%s): adminPage(…) keeps a command from ordinary accounts' menus; only %s may (offer it to them in the TUI as the web UI does)", m.ID, m.at, strings.Join(allowedAdminPage, ", "))
		}
		names := map[string]bool{}
		for _, in := range w.inputs[m.ID] {
			names[in.name] = true
		}
		for _, name := range sortedKeys(m.Fields) {
			if !names[name] {
				p.add("%s (%s): field(%q) — the operation has no such request field (it has: %s)", m.ID, m.at, name, inputNames(w.inputs[m.ID]))
			}
		}
		for _, name := range sortedKeys(m.AdminFields) {
			if !names[name] {
				p.add("%s (%s): adminField(…, %q) — the operation has no such request field (it has: %s)", m.ID, m.at, name, inputNames(w.inputs[m.ID]))
			}
		}
		if len(m.AdminFields) > 0 && w.adminOnly(m.ID) {
			p.add("%s (%s): adminField(…) on an operation only administrators may call says nothing more; drop it", m.ID, m.at)
		}
		for _, loc := range m.Web {
			if loc.Dyn != "" && !slices.Contains(a.segs, loc.Dyn) {
				p.add("%s (%s): webDyn(%q) — %s has no such segment", m.ID, m.at, loc.Dyn, a)
			}
		}
	}
	for _, a := range w.ops {
		if _, ok := seen[a.id]; !ok {
			p.add("%s (%s, tag %q) is not in the manifest: add it to manifest_%s.go with the mp commands and the web UI files that reach it", a.id, a, w.tags[a.id], w.tags[a.id])
		}
	}
	for _, key := range sortedKeys(ignoredFields) {
		id, name, _ := strings.Cut(key, " ")
		if _, ok := w.byID[id]; ok && !slices.ContainsFunc(w.inputs[id], func(in input) bool { return in.name == name }) {
			p.add("ignoredFields (parity_test.go): %s has no request field %s any more; drop the entry", id, name)
		}
	}
	return p.list
}

func inputNames(list []input) string {
	if len(list) == 0 {
		return "none"
	}
	var names []string
	for _, in := range list {
		names = append(names, in.name)
	}
	return strings.Join(names, ", ")
}

// cliProblems: the commands the manifest names exist and run, and every command
// mp offers is in the manifest or is one of the exceptions in exempt.go.
func (w *world) cliProblems() []string {
	p := &problems{}
	for _, m := range w.man {
		for _, path := range m.CLI {
			c := w.all[path]
			switch {
			case c == nil:
				p.add("%s (%s): mp has no command %q%s", m.ID, m.at, path, w.siblings(path))
			case w.hidden(c):
				p.add("%s (%s): mp %s is hidden; a panel feature needs a visible command", m.ID, m.at, path)
			case !c.Runnable():
				p.add("%s (%s): mp %s only groups other commands; name the ones that run", m.ID, m.at, path)
			}
		}
	}
	for _, path := range w.visible {
		_, exempt := cliOnly[top(path)]
		_, local := localOnly[path]
		switch {
		case len(w.opsOf[path]) > 0 && (exempt || local):
			p.add("mp %s is in the manifest (%s) and in exempt.go; it is one or the other", path, w.entriesAt(w.opsOf[path]))
		case len(w.opsOf[path]) == 0 && !exempt && !local:
			p.add("mp %s reaches no operation in the manifest: add it to cli(…) of the operation(s) it calls in manifest_<tag>.go (then the web UI must offer it too and the TUI picks it up); only installation, daemon roles and shell conveniences are exempt (exempt.go)", path)
		}
	}
	for _, path := range sortedKeys(cliOnly) {
		if !slices.Contains(allowedCLIOnly, path) {
			p.add("exempt.go: %q is not a deliberate exception (installation, daemon roles, shell conveniences: %s); map it in the manifest", path, strings.Join(allowedCLIOnly, ", "))
		}
	}
	for _, path := range sortedKeys(localOnly) {
		switch c := w.all[path]; {
		case !slices.Contains(allowedLocalOnly, path):
			p.add("exempt.go: %q may not bypass the API; only %s does", path, strings.Join(allowedLocalOnly, ", "))
		case c == nil || w.hidden(c) || !c.Runnable():
			p.add("exempt.go: mp has no visible command %q to run on the server", path)
		}
	}
	for _, spec := range sortedKeys(localFlags) {
		path, name, _ := strings.Cut(spec, " --")
		if c := w.all[path]; c == nil || c.Flags().Lookup(name) == nil {
			p.add("exempt.go: mp %s has no --%s", path, name)
		}
	}
	return p.list
}

// siblings suggests the commands of the group a missing path points into.
func (w *world) siblings(path string) string {
	parent := ""
	if i := strings.LastIndexByte(path, ' '); i >= 0 {
		parent = path[:i]
	}
	var near []string
	for _, v := range w.visible {
		if parent == "" && !strings.Contains(v, " ") || parent != "" && strings.HasPrefix(v, parent+" ") && !strings.Contains(v[len(parent)+1:], " ") {
			near = append(near, v)
		}
	}
	if len(near) == 0 || len(near) > 40 {
		return ""
	}
	return " (there are: " + strings.Join(near, ", ") + ")"
}

// flagsOf are the flags a command takes, without mp's global ones (--server,
// --token, --json…), which set no request field. A command's own flag of the
// same name (migrate plan --token: the source's token) is its own.
func (w *world) flagsOf(c *cobra.Command) map[string]*pflag.Flag {
	out := map[string]*pflag.Flag{}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && f.Deprecated == "" {
			out[f.Name] = f
		}
	})
	c.InheritedFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && f.Deprecated == "" && out[f.Name] == nil && w.root.PersistentFlags().Lookup(f.Name) != f {
			out[f.Name] = f
		}
	})
	return out
}

// argsOf reads the positional arguments from a Use string such as
// "add <info@example.com> <dest> [dest...]" or "renew <id|hostname>".
func argsOf(use string) []string {
	words := strings.Fields(use)
	var out []string
	for _, a := range words[min(1, len(words)):] {
		if strings.HasPrefix(a, "-") {
			break
		}
		a = strings.TrimSuffix(strings.TrimSuffix(a, "..."), "…")
		a = strings.Trim(a, "<>[]")
		a = strings.TrimSuffix(strings.TrimSuffix(a, "..."), "…")
		if a != "" && a != "…" {
			out = append(out, a)
		}
	}
	return out
}

func kebab(s string) string { return strings.ReplaceAll(s, "_", "-") }

// argNamed says whether a command takes a positional argument of that name;
// "<id|hostname>" is named both.
func argNamed(c *cobra.Command, names ...string) bool {
	for _, a := range argsOf(c.Use) {
		for _, n := range names {
			if a == n || slices.Contains(strings.Split(a, "|"), n) {
				return true
			}
		}
	}
	return false
}

// cliFieldProblems: every request field of an operation can be set with one of
// its mp commands (and so in the TUI's form for it).
func (w *world) cliFieldProblems() []string {
	p := &problems{}
	for _, m := range w.man {
		var cmds []*cobra.Command
		for _, path := range m.CLI {
			if c := w.all[path]; c != nil && !w.hidden(c) {
				cmds = append(cmds, c)
			}
		}
		if len(cmds) == 0 || len(cmds) != len(m.CLI) {
			continue // no commands, or missing ones (reported by the cli check)
		}
		for _, in := range w.inputs[m.ID] {
			if ignored(m.ID, in.name) {
				continue
			}
			hows := m.Fields[in.name]
			if len(hows) == 0 {
				if !w.settableByDefault(cmds, in) {
					want := "--" + kebab(in.name)
					if in.typ == "boolean" {
						want += " or --no-" + kebab(in.name)
					}
					p.add("%s (%s): %s cannot be set with mp %s: no flag %s and no argument <%s>; add one, or say in the manifest how it is set: field(%q, flag(…)/arg(…)/by(…))",
						m.ID, m.at, in, strings.Join(m.CLI, ", mp "), want, in.name, in.name)
				}
				continue
			}
			ok := false
			var tried []string
			for _, h := range hows {
				switch h.kind {
				case howFlag:
					for _, c := range cmds {
						for _, n := range h.names {
							if w.flagsOf(c)[n] != nil {
								ok = true
							}
						}
					}
				case howArg:
					for _, c := range cmds {
						if argNamed(c, h.names...) {
							ok = true
						}
					}
				case howCmd:
					if w.byCommands(p, m, in, h) {
						ok = true
					}
				}
				tried = append(tried, h.String())
			}
			if !ok && len(tried) > 0 {
				p.add("%s (%s): %s — the manifest says it is set with %s, but mp %s has no such thing (flags: %s; arguments: %s)",
					m.ID, m.at, in, strings.Join(tried, " or "), strings.Join(m.CLI, ", mp "), w.flagList(cmds), argList(cmds))
			}
		}
	}
	return p.list
}

// settableByDefault: a flag of the field's name in kebab case (--no-x for a
// boolean), an argument of that name, or --user for an account's login.
func (w *world) settableByDefault(cmds []*cobra.Command, in input) bool {
	names := []string{kebab(in.name)}
	if in.typ == "boolean" {
		names = append(names, "no-"+kebab(in.name))
	}
	if in.name == "login" {
		names = append(names, "user")
	}
	for _, c := range cmds {
		flags := w.flagsOf(c)
		for _, n := range names {
			if flags[n] != nil {
				return true
			}
		}
		if argNamed(c, in.name, kebab(in.name)) {
			return true
		}
	}
	return false
}

// byCommands checks a field that follows from which command runs: the
// commands must be the operation's, and each value of an enumerated field
// needs a command of that name (service reload-or-restart).
func (w *world) byCommands(p *problems, m Op, in input, h How) bool {
	ok := true
	for _, path := range h.names {
		if !slices.Contains(m.CLI, path) {
			p.add("%s (%s): field(%q, by(%q)) — %q is not among the operation's commands (%s)", m.ID, m.at, in.name, path, path, strings.Join(m.CLI, ", "))
			ok = false
		}
	}
	for _, v := range in.enum {
		found := false
		for _, path := range h.names {
			if c := w.all[path]; c != nil && c.Name() == v {
				found = true
			}
		}
		if !found {
			p.add("%s (%s): %s takes %q, and no mp command does that (the ones for the other values: %s); add mp … %s", m.ID, m.at, in, v, strings.Join(h.names, ", "), v)
		}
	}
	return ok
}

func (w *world) flagList(cmds []*cobra.Command) string {
	var names []string
	for _, c := range cmds {
		for n := range w.flagsOf(c) {
			if !slices.Contains(names, "--"+n) {
				names = append(names, "--"+n)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

func argList(cmds []*cobra.Command) string {
	var names []string
	for _, c := range cmds {
		for _, a := range argsOf(c.Use) {
			if !slices.Contains(names, "<"+a+">") {
				names = append(names, "<"+a+">")
			}
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// tuiProblems: the TUI's menus offer every command the manifest maps (and the
// local-only ones when the TUI runs on the server itself), hide the
// local-only ones from a remote TUI and from ordinary accounts, offer no
// command that is not a panel feature, keep the administrators' commands from
// ordinary accounts and give ordinary accounts every command of an operation
// they may call (on the server and with --server alike).
func (w *world) tuiProblems() []string {
	p := &problems{}
	onServer, withServer := "on the server", "with --server"
	const panel = "https://panel.example:8443"
	local := w.bound(p, tui.Options{}, "admin", onServer+", as an administrator")
	remote := w.bound(p, tui.Options{Server: panel}, "admin", withServer+", as an administrator")
	user := w.bound(p, tui.Options{}, "user", onServer+", for an ordinary account")
	userRemote := w.bound(p, tui.Options{Server: panel}, "user", withServer+", for an ordinary account")
	for _, path := range w.visible {
		ops := w.opsOf[path]
		_, isLocal := localOnly[path]
		if len(ops) == 0 && !isLocal {
			continue // CLI-only, or unmapped (reported by the cli check)
		}
		what := w.entriesAt(ops)
		if isLocal {
			what = "works without the API, on the server only"
		}
		var gaps []string
		if !local[path] && len(local) > 0 {
			gaps = append(gaps, onServer)
		}
		if !isLocal && !remote[path] && len(remote) > 0 {
			gaps = append(gaps, withServer)
		}
		if len(gaps) > 0 {
			p.add("mp %s (%s) is in no TUI menu %s: put it on a screen of its area, or map its group to an area so that the area's command list has it", path, what, strings.Join(gaps, " nor "))
		}
		if isLocal && (remote[path] || userRemote[path]) {
			p.add("mp %s works only on the server itself (exempt.go), but the TUI offers it %s", path, withServer)
		}
		if isLocal && user[path] {
			p.add("the TUI offers ordinary accounts mp %s, which changes the server as root (exempt.go): keep it for administrators", path)
		}
		if len(ops) == 0 {
			continue
		}
		if (user[path] || userRemote[path]) && w.allAdmin(ops) {
			p.add("the TUI offers ordinary accounts mp %s, but its operations (%s) are for administrators only: hide it from them as the web UI does", path, what)
		}
		if mine := w.forUsers(ops); len(mine) > 0 {
			gaps = nil
			if !user[path] && len(user) > 0 {
				gaps = append(gaps, onServer)
			}
			if !userRemote[path] && len(userRemote) > 0 {
				gaps = append(gaps, withServer)
			}
			if len(gaps) > 0 {
				p.add("mp %s is in no TUI menu of an ordinary account %s, though they may call %s: offer it to them as the web UI does; if the web UI gives them no page for it, that is a design decision for adminPage(…) in the manifest and allowedAdminPage; if the API refuses them, mark the operation adminOnly",
					path, strings.Join(gaps, " nor "), w.entriesAt(mine))
			}
		}
	}
	return p.list
}

// bound asks the TUI which commands its menus offer and checks that it names
// real commands that are panel features.
func (w *world) bound(p *problems, opts tui.Options, role, label string) map[string]bool {
	out := map[string]bool{}
	list := w.boundCommands(w.root, opts, role)
	if len(list) == 0 {
		p.add("BoundCommands (%s) returns no commands at all: the TUI menus offer none of mp's %d commands", label, len(w.visible))
	}
	for _, path := range list {
		out[path] = true
		c := w.all[path]
		_, exempt := cliOnly[top(path)]
		switch {
		case c == nil:
			p.add("BoundCommands (%s) returns %q, which mp does not have", label, path)
		case w.hidden(c) || exempt:
			p.add("BoundCommands (%s) returns mp %s, which is no panel feature (hidden or exempt.go)", label, path)
		case !c.Runnable():
			p.add("BoundCommands (%s) returns mp %s, which only groups other commands", label, path)
		}
	}
	return out
}

// adminOnly says whether only administrators may call the operation: the API
// marks it adminOnly, or the manifest says its handler refuses anyone else.
func (w *world) adminOnly(id string) bool {
	a, ok := w.byID[id]
	return ok && (a.admin || w.entries[id].Admin != "")
}

func (w *world) allAdmin(ids []string) bool {
	for _, id := range ids {
		if !w.adminOnly(id) {
			return false
		}
	}
	return true
}

// forUsers are the operations among ids that ordinary accounts may call and
// that the web UI offers them: their TUI menus must have the commands too.
func (w *world) forUsers(ids []string) []string {
	var out []string
	for _, id := range ids {
		if _, ok := w.byID[id]; ok && !w.adminOnly(id) && w.entries[id].AdminPage == "" {
			out = append(out, id)
		}
	}
	return out
}

// files resolves a location: files and directories under web/src.
func (w *world) files(in []string) (out []*webFile, missing []string) {
	if len(in) == 0 {
		for _, f := range w.web {
			out = append(out, f)
		}
	}
	for _, loc := range in {
		found := false
		if f := w.web[loc]; f != nil {
			out = append(out, f)
			found = true
		} else {
			dir := strings.TrimSuffix(loc, "/") + "/"
			for name, f := range w.web {
				if strings.HasPrefix(name, dir) {
					out = append(out, f)
					found = true
				}
			}
		}
		if !found {
			missing = append(missing, loc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, missing
}

// webProblems: every place the manifest names exists and calls the operation.
func (w *world) webProblems() []string {
	p := &problems{}
	for _, m := range w.man {
		a, ok := w.byID[m.ID]
		if !ok {
			continue // reported by the manifest check
		}
		for _, loc := range m.Web {
			files, missing := w.files(loc.In)
			for _, f := range missing {
				p.add("%s (%s): web/src/%s does not exist", m.ID, m.at, f)
			}
			if len(files) == 0 {
				continue
			}
			if msg := w.finds(a, loc, files); msg != "" {
				p.add("%s (%s): %s", m.ID, m.at, msg)
			}
		}
	}
	return p.list
}

// finds looks for the operation in the files of a location; it returns ""
// when found, otherwise what is wrong and what was found instead.
func (w *world) finds(a apiOp, loc Web, files []*webFile) string {
	where := "web/src/" + strings.Join(loc.In, ", web/src/")
	if len(loc.In) == 0 {
		where = "web/src"
	}
	if loc.Pattern != "" {
		re, err := regexp.Compile(loc.Pattern)
		if err != nil {
			return fmt.Sprintf("bad Pattern %q: %v", loc.Pattern, err)
		}
		for _, f := range files {
			if re.MatchString(f.code) {
				return ""
			}
		}
		return fmt.Sprintf("nothing in %s matches %q", where, loc.Pattern)
	}
	for _, f := range files {
		for _, c := range f.calls {
			if reaches(w.ops, c.method, c.segs, a) {
				return ""
			}
			if loc.Dyn != "" && (c.method == "" || c.method == a.method) && a.fits(c.segs, loc.Dyn) && f.hasString(loc.Dyn) {
				return ""
			}
		}
		if loc.Loose {
			for _, c := range f.paths {
				if reaches(w.ops, "", c.segs, a) {
					return ""
				}
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no call of %s in %s", a, where)
	switch {
	case loc.Loose:
		fmt.Fprintf(&b, "; not even the path is written there")
	case loc.Dyn != "":
		fmt.Fprintf(&b, " (a call with %q computed counts too, when the file has the string '%s')", loc.Dyn, loc.Dyn)
	}
	b.WriteString(".\n    Expected e.g. " + example(a))
	var same, dyn, vague []string
	for _, f := range files {
		for _, c := range f.calls {
			switch {
			case c.method != "" && c.method != a.method && reaches(w.ops, a.method, c.segs, a) && !w.otherRoute(c, a):
				same = append(same, c.String())
			case slices.ContainsFunc(mayReach(w.ops, c.method, c.segs), func(o apiOp) bool { return o.id == a.id }):
				dyn = append(dyn, c.String())
			}
		}
		for _, c := range f.vague {
			vague = append(vague, c.String())
		}
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		if len(items) > 6 {
			items = append(items[:6:6], fmt.Sprintf("… %d more", len(items)-6))
		}
		b.WriteString("\n    " + title + ":\n      " + strings.Join(items, "\n      "))
	}
	list("Calls of the same path with another method", same)
	list("Calls with a computed segment that may be it (give the entry webDyn(<value>, …) if so)", dyn)
	list("Calls whose path the check cannot read (give the entry a Pattern if one of them is it)", vague)
	return b.String()
}

// otherRoute says whether the call goes to a more specific path than the
// operation's (POST /certificates/import is not /certificates/{id}).
func (w *world) otherRoute(c webCall, a apiOp) bool {
	for _, o := range route(w.ops, c.method, c.segs) {
		if compareStatic(o, []apiOp{a}) > 0 {
			return true
		}
	}
	return false
}

// example writes a call of the operation the way the pages do.
func example(a apiOp) string {
	var b strings.Builder
	b.WriteString("api(`")
	for _, s := range strings.Split(strings.Trim(a.path, "/"), "/") {
		b.WriteByte('/')
		if strings.HasPrefix(s, "{") {
			b.WriteString("${" + strings.Trim(s, "{}") + "}")
		} else {
			b.WriteString(s)
		}
	}
	b.WriteString("`")
	if a.method != "GET" {
		b.WriteString(", { method: '" + a.method + "', … }")
	}
	b.WriteString(")")
	return b.String()
}

func dedupe(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// webCallProblems: the web UI calls nothing the API does not have.
func (w *world) webCallProblems() []string {
	p := &problems{}
	names := make([]string, 0, len(w.web))
	for name := range w.web {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, c := range w.web[name].calls {
			if nonOperations.MatchString(segsString(c.segs)) {
				continue
			}
			if len(mayReach(w.ops, c.method, c.segs)) == 0 {
				var others []string
				for _, o := range route(w.ops, "", c.segs) {
					others = append(others, o.String())
				}
				hint := "no operation has this path"
				if len(others) > 0 {
					hint = c.method + " is not served there; the path takes " + strings.Join(others, ", ")
				}
				p.add("web/src/%s: %s", c.String(), hint)
			}
		}
	}
	return p.list
}

// TestChecksOnASmallWorld runs every check on a made-up API, command tree,
// TUI and web page with known faults, so that each fault is reported and
// nothing else is.
func TestChecksOnASmallWorld(t *testing.T) {
	ops := []apiOp{
		newAPIOp("thing-list", "GET", "/things", false),
		newAPIOp("thing-create", "POST", "/things", true),
		newAPIOp("thing-act", "POST", "/things/{name}/{action}", false),
		newAPIOp("secret-get", "GET", "/secret", true),
		newAPIOp("unmapped", "GET", "/unmapped", false),
		newAPIOp("thing-delete", "DELETE", "/things/{name}", false),
		newAPIOp("thing-pause", "POST", "/things/{name}/pause", false), // the handler refuses ordinary accounts
		newAPIOp("thing-stats", "GET", "/things/stats", false),         // on an administrators' page only
	}
	tags := map[string]string{}
	for _, o := range ops {
		tags[o.id] = "things"
	}
	inputs := map[string][]input{
		"thing-create": {{name: "enabled", in: "body", typ: "boolean"}, {name: "kind", in: "body", typ: "string", enum: []string{"a", "b"}}, {name: "name", in: "body", typ: "string"}, {name: "quota_mb", in: "body", typ: "integer"}},
		"thing-act":    {{name: "name", in: "path", typ: "string"}, {name: "action", in: "path", typ: "string", enum: []string{"start", "stop", "kick"}}},
	}
	run := func(*cobra.Command, []string) {}
	root := &cobra.Command{Use: "mp"}
	root.PersistentFlags().String("token", "", "")
	thing := &cobra.Command{Use: "thing"}
	add := &cobra.Command{Use: "add <name>", Run: run}
	add.Flags().Int("quota", 0, "")
	add.Flags().Bool("disabled", false, "")
	add.Flags().String("kind-of", "", "") // not --kind
	thing.AddCommand(&cobra.Command{Use: "list", Run: run}, add, &cobra.Command{Use: "start <name>", Run: run}, &cobra.Command{Use: "stop <name>", Run: run}, &cobra.Command{Use: "rm <name>", Run: run},
		&cobra.Command{Use: "pause <name>", Run: run}, &cobra.Command{Use: "stats", Run: run})
	secret := &cobra.Command{Use: "secret"}
	secret.AddCommand(&cobra.Command{Use: "show", Run: run})
	update := &cobra.Command{Use: "update"}
	update.AddCommand(&cobra.Command{Use: "trust", Run: run})
	completion := &cobra.Command{Use: "completion"} // cobra's, exempt with all its shells
	completion.AddCommand(&cobra.Command{Use: "bash", Run: run})
	root.AddCommand(thing, secret, update, completion, &cobra.Command{Use: "orphan", Run: run}, &cobra.Command{Use: "version", Run: run}, &cobra.Command{Use: "agent", Hidden: true, Run: run})

	page := parseWebFile("routes/things/+page.svelte", `<script lang="ts">
  const list = await api('/things');
  await api('/things', { method: 'POST', json: { name: form.name, quota_mb: form.quota } });
  await api(`+"`/things/${n}/${a}`"+`, { method: 'POST' });
  await api(`+"`/things/${n}/pause`"+`, { method: 'POST' });
  await api('/things/stats');
  await api('/secret');
  await api('/things/x/y/z');
</script>`)
	man := []Op{
		op("thing-list", cli("thing list"), web("routes/things/"), web()), // web(): anywhere under web/src
		op("thing-create", cli("thing add"), web("routes/things/"), field("quota_mb", flag("quota")), field("enabled", flag("disabled")), field("colour", flag("x"))),
		op("thing-act", cli("thing start", "thing stop"), web("routes/things/"), field("action", by("thing start", "thing stop"))),
		op("secret-get", cli("secret show"), web("routes/things/", "routes/missing/")),
		op("thing-delete", cli("thing rm"), web("routes/things/")),
		op("thing-pause", cli("thing pause"), web("routes/things/"), admin("the handler refuses ordinary accounts")),
		op("thing-stats", cli("thing stats"), web(), adminPage("an administrators' page")),
		op("ghost", cli("thing ghost"), web("routes/things/")),
	}
	w := newWorld(ops, tags, inputs, root, map[string]*webFile{page.path: page}, man)
	w.boundCommands = func(_ *cobra.Command, opts tui.Options, role string) []string {
		switch {
		case role != "admin" && opts.Remote():
			return []string{"thing list", "thing start", "thing stop"}
		case role != "admin":
			return []string{"thing list", "thing start", "thing stop", "secret show", "thing pause", "update trust"}
		case opts.Remote():
			return []string{"thing list", "thing add", "thing start", "thing stop", "thing rm", "thing pause", "thing stats", "secret show", "update trust", "orphan"}
		default:
			return []string{"thing list", "thing add", "thing start", "thing rm", "thing pause", "thing stats", "secret show", "update trust", "orphan", "agent", "completion bash"}
		}
	}

	expect := func(name string, got []string, want ...string) {
		t.Helper()
		all := strings.Join(got, "\n")
		for _, s := range want {
			if !strings.Contains(all, s) {
				t.Errorf("%s: no problem mentions %q; got:\n%s", name, s, all)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d problems, want %d:\n%s", name, len(got), len(want), all)
		}
	}
	expect("manifest", w.manifestProblems(),
		"ghost (checks_test.go:", // not an operation
		"unmapped (GET /unmapped, tag \"things\") is not in the manifest",
		// not in manifest_things.go
		"thing-list (checks_test.go:", "thing-create (checks_test.go:", "thing-act (checks_test.go:", "secret-get (checks_test.go:", "thing-delete (checks_test.go:", "thing-pause (checks_test.go:", "thing-stats (checks_test.go:",
		`field("colour")`,
		"thing-pause (checks_test.go:", // admin(…) without being allowed
		"thing-stats (checks_test.go:") // adminPage(…) without being allowed
	expect("cli", w.cliProblems(),
		`mp has no command "thing ghost" (there are: thing add, thing list, thing pause, thing rm, thing start, thing stats, thing stop)`,
		"mp orphan reaches no operation",
		"exempt.go: mp config set has no --local", // the real tree's local flags are not in this one
		"exempt.go: mp config show has no --local")
	expect("cli-fields", w.cliFieldProblems(),
		"body kind cannot be set with mp thing add: no flag --kind",
		`takes "kick", and no mp command does that`)
	expect("tui", w.tuiProblems(),
		"BoundCommands (on the server, as an administrator) returns mp agent, which is no panel feature",
		"BoundCommands (on the server, as an administrator) returns mp completion bash, which is no panel feature",
		"mp thing stop (thing-act checks_test.go:",
		"mp update trust works only on the server itself (exempt.go), but the TUI offers it with --server",
		"the TUI offers ordinary accounts mp secret show",
		"the TUI offers ordinary accounts mp thing pause", // admin(…) counts like adminOnly
		"the TUI offers ordinary accounts mp update trust, which changes the server as root",
		"mp thing rm is in no TUI menu of an ordinary account on the server nor with --server, though they may call thing-delete")
	expect("web", w.webProblems(), "web/src/routes/missing/ does not exist",
		"no call of DELETE /things/{name} in web/src/routes/things/.\n    Expected e.g. api(`/things/${name}`, { method: 'DELETE', … })")
	expect("web-fields", w.webFieldProblems(), "body enabled is in the request body of no call of it", "body kind is in the request body of no call of it")
	expect("web-calls", w.webCallProblems(), "GET /things/x/y/z")

	// The details of the manifest's role annotations.
	all := strings.Join(w.manifestProblems(), "\n")
	for _, s := range []string{"thing-pause (checks_test.go:", "admin(…) keeps a command from ordinary accounts; only sites-suspend, sites-unsuspend may",
		"adminPage(…) keeps a command from ordinary accounts' menus; only php-list, web-tls, dns-provider-types may"} {
		if !strings.Contains(all, s) {
			t.Errorf("manifest: no problem mentions %q; got:\n%s", s, all)
		}
	}
}
