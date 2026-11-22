package parity

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent/agenttest"
	"monopanel/internal/api"
	mpcli "monopanel/internal/cli"
	"monopanel/internal/config"
	"monopanel/internal/i18n"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// The deliberate exceptions. A client may leave out an operation or a
// command only for these reasons; anything else must be reachable from the
// web UI, mp and the TUI alike. Adding to these lists is a design decision,
// not a way to make the test pass.
var (
	// Browser sessions (login, logout) and the console, which runs mp
	// itself; the clients' own version probe; the server-to-server half of
	// a migration.
	allowedNoCLI = []string{"auth-login", "auth-logout", "system-console", "health",
		"migrate-plan-source", "migrate-state", "migrate-files", "migrate-dump"}
	// Only the server-to-server half of a migration: a person never calls it.
	allowedNoWeb = []string{"migrate-plan-source", "migrate-state", "migrate-files", "migrate-dump"}
	// Installation, daemon roles and shell conveniences (exempt.go).
	allowedCLIOnly = []string{"setup", "version", "completion", "help", "api", "agent", "helper", "fsop", "update-run", "demo"}
	// The release trust anchor: root on the server only (exempt.go).
	allowedLocalOnly = []string{"update trust"}
	// Operations the handler refuses to ordinary accounts without the API
	// marking them adminOnly (admin(…) in the manifest).
	allowedAdmin = []string{"sites-suspend", "sites-unsuspend"}
	// Operations ordinary accounts may call for which the web UI gives them
	// no page, so their TUI menus may leave them out (adminPage(…) in the
	// manifest).
	allowedAdminPage = []string{"php-list", "web-tls", "dns-provider-types"}
	// Request fields the API still declares but its handler ignores, on
	// their way out of the API: no client needs to set them. The manifest
	// check reports an entry whose field is gone, so the list only shrinks.
	ignoredFields = map[string]string{}
)

// TestParity fails when the web UI, mp and the TUI drift apart; its output
// says what to fix where.
func TestParity(t *testing.T) {
	w := load(t)
	flagsSkip := ""
	if w.boundFlags == nil {
		flagsSkip = tuiFlagsMissing
	}
	for _, c := range []struct {
		name, title string
		problems    func() []string
		skip        string
	}{
		{"manifest", "The manifest (internal/parity/manifest_<tag>.go) does not match the API", w.manifestProblems, ""},
		{"cli", "mp (internal/cli) and the manifest disagree", w.cliProblems, ""},
		{"cli-fields", "Request fields that mp cannot set", w.cliFieldProblems, ""},
		{"tui", "The TUI menus (tui.BoundCommands) do not offer what mp offers, to administrators and to ordinary accounts", w.tuiProblems, ""},
		{"tui-flags", "The TUI's forms (tui.BoundFlags) give ordinary accounts flags only administrators may use", w.tuiFlagProblems, flagsSkip},
		{"web", "The web UI (web/src) does not call what the manifest says", w.webProblems, ""},
		{"web-fields", "Request fields that the web UI never sends", w.webFieldProblems, ""},
		{"web-roles", "The web UI offers ordinary accounts what only administrators may do", w.webRoleProblems, ""},
		{"web-calls", "The web UI calls paths the API does not have", w.webCallProblems, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.skip != "" {
				t.Skip(c.skip)
			}
			if list := c.problems(); len(list) > 0 {
				t.Errorf("%s — %d problem(s):\n\n%s\n", c.title, len(list), strings.Join(list, "\n\n"))
			}
		})
	}
}

func load(t *testing.T) *world {
	t.Helper()
	// mp words its Use strings in the terminal's language ("enable <код>",
	// "set <ключ> <значение>"), and the checks read the arguments' names
	// from them: the manifest names them in English.
	i18n.Set("en")
	// The API as the server registers it, behind a fake agent. Nothing of the
	// host's own panel (/etc/monopanel, /var/log/monopanel) is read or written.
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.RunDir = t.TempDir()
	cfg.SecretKeyFile = filepath.Join(cfg.DataDir, "secret.key")
	cfg.Log.AuditFile = ""
	db, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	profile, _ := osprofile.FromRelease(rel)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(cfg, db, agenttest.Start(t).Client(), jobs.NewRunner(db, 1, quiet), profile, quiet)
	doc := srv.OpenAPI()
	var ops []apiOp
	tags, inputs := map[string]string{}, map[string][]input{}
	for p, item := range doc.Paths {
		for _, o := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch, item.Head, item.Options} {
			if o == nil || o.Hidden {
				continue
			}
			admin, _ := o.Metadata["admin"].(bool)
			ops = append(ops, newAPIOp(o.OperationID, o.Method, p, admin))
			if len(o.Tags) > 0 {
				tags[o.OperationID] = o.Tags[0]
			}
			inputs[o.OperationID] = inputsOf(doc, o)
		}
	}
	// The web UI's sources.
	web, err := webSources(filepath.Join("..", "..", "web", "src"))
	if err != nil {
		t.Fatal(err)
	}
	// The command tree as mp runs it: cobra adds help and completion when it
	// executes, so the tree the TUI gets has them as well.
	root := mpcli.NewRoot()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return newWorld(ops, tags, inputs, root, web, manifest())
}

// inputsOf lists an operation's request fields.
func inputsOf(doc *huma.OpenAPI, o *huma.Operation) []input {
	var out []input
	for _, p := range o.Parameters {
		if p.In != "path" && p.In != "query" {
			continue
		}
		in := input{name: p.Name, in: p.In}
		if p.Schema != nil {
			in.typ, in.enum = p.Schema.Type, enumOf(p.Schema)
		}
		out = append(out, in)
	}
	if o.RequestBody == nil {
		return out
	}
	mt := o.RequestBody.Content["application/json"]
	if mt == nil || mt.Schema == nil {
		return out
	}
	s := mt.Schema
	if s.Ref != "" {
		s = doc.Components.Schemas.SchemaFromRef(s.Ref)
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ps := s.Properties[name]
		if name == "$schema" || ps.ReadOnly {
			continue
		}
		out = append(out, input{name: name, in: "body", typ: ps.Type, enum: enumOf(ps)})
	}
	return out
}

func enumOf(s *huma.Schema) []string {
	var out []string
	for _, v := range s.Enum {
		if str, ok := v.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out
}

// The manifest must cover the operations listed in the exceptions, so that
// the lists cannot go stale unnoticed.
func TestExceptionsAreInTheManifest(t *testing.T) {
	ids := map[string]Op{}
	for _, m := range manifest() {
		ids[m.ID] = m
	}
	for _, id := range allowedNoCLI {
		if m, ok := ids[id]; !ok || m.NoCLI == "" {
			t.Errorf("%s is an allowed exception without mp command, but the manifest does not say why (noCLI)", id)
		}
	}
	for _, id := range allowedNoWeb {
		if m, ok := ids[id]; !ok || m.NoWeb == "" {
			t.Errorf("%s is an allowed exception without web UI, but the manifest does not say why (noWeb)", id)
		}
	}
	for _, path := range allowedLocalOnly {
		if _, ok := localOnly[path]; !ok {
			t.Errorf("%s is an allowed local-only command, but exempt.go does not say why", path)
		}
	}
	for _, id := range allowedAdmin {
		if m, ok := ids[id]; !ok || m.Admin == "" {
			t.Errorf("%s is allowed to be for administrators by its handler only, but the manifest does not say why (admin)", id)
		}
	}
	for _, id := range allowedAdminPage {
		if m, ok := ids[id]; !ok || m.AdminPage == "" {
			t.Errorf("%s is allowed to stay out of ordinary accounts' menus, but the manifest does not say why (adminPage)", id)
		}
	}
}

// ignored says whether a request field is one the handler ignores.
func ignored(id, name string) bool {
	_, ok := ignoredFields[id+" "+name]
	return ok
}
