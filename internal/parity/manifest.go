// Package parity keeps the three clients of the panel equal: the web UI, the
// CLI (mp) and the TUI. Its test fails when they drift apart — when an API
// operation has no mp command or no place in the web UI, when a command is
// missing from the TUI menus, when the web UI calls something the API does
// not have, or when a request field cannot be set from a client.
//
// The manifest says how every operation is reached: manifest_<tag>.go, one
// file per OpenAPI tag, lists the operations of that tag with their mp
// commands, the web UI files that call them and the request fields that go
// by another name on the command line. The few deliberate exceptions carry
// their reason: operations in the manifest itself (NoCLI, NoWeb), commands
// that are no panel feature in exempt.go.
//
// The test (TestParity) compares the manifest with the OpenAPI document of
// the API server, with mp's command tree (cli.NewRoot), with the commands
// the TUI menus offer (tui.BoundCommands) and with the sources under
// web/src, which it reads for api(…) calls and their methods. When it fails,
// each line names the operation or command, the client and what is missing:
//
//   - a new operation: add an entry to manifest_<tag>.go, and the command
//     and the web UI control that reach it;
//   - a new command: add it to cli(…) of the operation it calls — the TUI
//     then has to offer it, and the web UI the operation; ordinary accounts
//     get it in their TUI menus too unless all its operations are for
//     administrators (adminOnly in the API, or admin(…) in the entry when
//     only the handler refuses them) or the web UI keeps them on
//     administrators' pages (adminPage(…));
//   - a new request field: a flag or argument of its name (or field(…) in the
//     entry when the flag is named differently), and a control in the page
//     that names the field in the request it builds;
//   - a page that builds a path the check cannot read: webDyn(…) for a
//     computed segment, webLoose(…) for a path handed to a component,
//     webPattern(…) as the last resort;
//   - a request field missing from the web UI: the page must send it — a
//     key of the body or of the query string the call builds (webbody.go
//     follows variables, the functions that return them and the arguments
//     of the function whose parameter they are), not merely name it;
//   - an operation for administrators called on a page ordinary accounts
//     open too (the navigation's list): the call must sit behind a check of
//     the role, as must the fields only administrators may set
//     (adminField(…)); webroles.go says what counts as one. Their flags
//     stay out of ordinary accounts' TUI forms (tui.BoundFlags).
//
// The exceptions are few on purpose and listed in the test as well (browser
// sign-in and the console are web-only, the server-to-server half of a
// migration has no client, installation and daemon roles are mp-only, the
// release key is changed as root on the server only, a few operations are
// kept from ordinary accounts' menus): adding one is a design decision, not a
// way to make the test pass.
package parity

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Op is one API operation and how the clients reach it.
type Op struct {
	ID string // the OpenAPI operationId

	// CLI are the mp commands that reach the operation, as command paths
	// ("site add"). The TUI's actions are mp commands, so they are its way
	// to the operation as well.
	CLI []string
	// Web are the places in the web UI that call the operation; each one
	// must.
	Web []Web
	// NoCLI and NoWeb say why a client deliberately does not offer the
	// operation. Without an mp command the TUI has none either.
	NoCLI, NoWeb string
	// Admin says why the operation is for administrators although the API
	// does not mark it adminOnly: its handler refuses ordinary accounts. The
	// TUI must keep its commands from them, as for adminOnly operations.
	Admin string
	// AdminPage says why ordinary accounts, whom the API lets call the
	// operation, need no command for it in the TUI: the web UI gives them no
	// page for it (it shows it on administrators' pages, at most as the
	// choices of a form). Every other operation they may call must be in
	// their menus.
	AdminPage string
	// Fields says how mp sets the request fields (body properties, query
	// and path parameters) that do not follow the default: a flag of the
	// field's name in kebab case (fpm_max_children → --fpm-max-children,
	// --no-x for a boolean x, --user for an account's login) or a
	// positional argument of that name. In the web UI a field is always
	// named as it is: the code that builds the request spells it out.
	Fields map[string][]How
	// AdminFields are request fields that only administrators may set,
	// with the reason (the handler refuses or ignores them for anyone
	// else): the web UI sets them behind a check of the role, and the TUI
	// keeps their flags out of ordinary accounts' forms. A --user flag,
	// acting for another account, is that everywhere without saying so.
	AdminFields map[string]string

	at string // where in the manifest, for messages
}

// Web is a place in the web UI that calls an operation.
type Web struct {
	// In are files or directories under web/src, relative to it; the call
	// must be in one of them. None means anywhere under web/src.
	In []string
	// Dyn is a static segment of the operation's path that the page
	// computes, as in `/firewall/${action}` with action = 'apply': a call
	// with any expression there counts if the file also has the value as a
	// string.
	Dyn string
	// Loose lets the path count wherever it is written, not only in a
	// call: for a path handed to a component that makes the call (LogView's
	// src). The method is not checked then.
	Loose bool
	// Pattern replaces the search with a regular expression that the code
	// (without comments) of one of the files must match.
	Pattern string
}

// How is one way mp sets a request field. No field is left out on purpose:
// whatever the API takes, every client can set.
type How struct {
	kind  howKind
	names []string
}

type howKind uint8

const (
	howFlag howKind = iota + 1 // one of these flags of the operation's commands
	howArg                     // one of these positional arguments, named as in Use
	howCmd                     // implied by which of these commands runs (enable, disable)
)

func (h How) String() string {
	switch h.kind {
	case howFlag:
		return "flag --" + strings.Join(h.names, " | --")
	case howArg:
		return "argument <" + strings.Join(h.names, "> | <") + ">"
	default:
		return "command " + strings.Join(h.names, " | ")
	}
}

var registry []Op

// register adds a manifest file's operations.
func register(ops ...Op) { registry = append(registry, ops...) }

// manifest is every operation the manifest describes, in file order.
func manifest() []Op { return registry }

type part func(*Op)

// op describes one operation; it remembers where it is written.
func op(id string, parts ...part) Op {
	o := Op{ID: id}
	if _, file, line, ok := runtime.Caller(1); ok {
		o.at = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	for _, p := range parts {
		p(&o)
	}
	return o
}

// cli names the mp commands that reach the operation.
func cli(paths ...string) part { return func(o *Op) { o.CLI = append(o.CLI, paths...) } }

// web is a call of the operation in one of these files or directories.
func web(in ...string) part { return func(o *Op) { o.Web = append(o.Web, Web{In: in}) } }

// webDyn is a call that computes the static segment value of the path.
func webDyn(value string, in ...string) part {
	return func(o *Op) { o.Web = append(o.Web, Web{In: in, Dyn: value}) }
}

// webLoose is the path written anywhere in these files (handed to a
// component that calls it).
func webLoose(in ...string) part {
	return func(o *Op) { o.Web = append(o.Web, Web{In: in, Loose: true}) }
}

// webPattern is code matching the regular expression re in one of these
// files: the last resort for a call the check cannot follow.
func webPattern(re string, in ...string) part {
	return func(o *Op) { o.Web = append(o.Web, Web{In: in, Pattern: re}) }
}

// noCLI: the operation has no mp command on purpose.
func noCLI(why string) part { return func(o *Op) { o.NoCLI = why } }

// noWeb: the web UI does not offer the operation on purpose.
func noWeb(why string) part { return func(o *Op) { o.NoWeb = why } }

// admin: the handler refuses ordinary accounts, though the API does not mark
// the operation adminOnly.
func admin(why string) part { return func(o *Op) { o.Admin = why } }

// adminPage: ordinary accounts may call the operation, but the web UI gives
// them no page for it, so their TUI menus may leave it out.
func adminPage(why string) part { return func(o *Op) { o.AdminPage = why } }

// field says how a request field is set where the default does not apply.
func field(name string, hows ...How) part {
	return func(o *Op) {
		if o.Fields == nil {
			o.Fields = map[string][]How{}
		}
		o.Fields[name] = append(o.Fields[name], hows...)
	}
}

// adminField: these request fields are for administrators only, for the
// reason given; ordinary accounts do not get controls for them.
func adminField(why string, names ...string) part {
	return func(o *Op) {
		if o.AdminFields == nil {
			o.AdminFields = map[string]string{}
		}
		for _, n := range names {
			o.AdminFields[n] = why
		}
	}
}

// flag: the field is set with one of these flags.
func flag(names ...string) How { return How{kind: howFlag, names: names} }

// arg: the field is a positional argument, named as in the command's Use.
func arg(names ...string) How { return How{kind: howArg, names: names} }

// by: the field follows from which of these commands runs.
func by(paths ...string) How { return How{kind: howCmd, names: paths} }
