package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// consoleRaw posts to the console and returns the streamed text as is; lang
// is the interface language the page sends along.
func consoleRaw(f *siteFixture, args []string, lang ...string) (int, string, string) {
	f.t.Helper()
	payload := map[string]any{"args": args}
	if len(lang) > 0 {
		payload["lang"] = lang[0]
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.ts.URL+"/api/v1/system/console", bytes.NewReader(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), string(body)
}

// The console runs mp as a client of the API with a token that lives for
// the command only; the output streams back with the exit status. The panel's
// address and the token travel in the environment, not on the command line:
// there they would shadow a command's own --token and --insecure, and show in
// ps to everyone on the host.
func TestConsoleRunsMPWithAOneOffToken(t *testing.T) {
	f := newSiteFixture(t)
	script := filepath.Join(t.TempDir(), "mp")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\necho \"home: $HOME\"\necho \"lang: $MP_LANG\"\necho \"server: $MP_SERVER\"\necho \"token: $MP_TOKEN\"\necho \"insecure: $MP_INSECURE\"\necho \"stdin: $(cat | tr '\\n' '|')\"\necho oops 1>&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := consoleBinary
	consoleBinary = func() string { return script }
	t.Cleanup(func() { consoleBinary = prev })

	status, ctype, body := consoleRaw(f, []string{"site", "list", "--json"})
	if status != http.StatusOK || !strings.HasPrefix(ctype, "text/plain") {
		t.Fatalf("console: %d %s", status, ctype)
	}
	if !strings.Contains(body, "$ mp site list --json\n") || !strings.Contains(body, "\nargs: site list --json\n") || !strings.Contains(body, "\nserver: https://127.0.0.1:") ||
		!strings.Contains(body, "\ninsecure: 1\n") || !strings.Contains(body, "oops") || !strings.Contains(body, "[exit 3]") {
		t.Fatalf("output:\n%s", body)
	}
	token := ""
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, "token: "); ok {
			token = v
		}
	}
	if len(token) < 32 {
		t.Fatalf("a token must be passed in MP_TOKEN: %q", token)
	}
	// mp speaks the interface's language, English unless the page says ru.
	if !strings.Contains(body, "lang: en\n") {
		t.Fatalf("default language: %s", body)
	}
	if _, _, body := consoleRaw(f, []string{"status"}, "ru"); !strings.Contains(body, "lang: ru\n") {
		t.Fatalf("Russian interface: %s", body)
	}
	admin, err := f.db.GetUserByLogin(f.ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := f.db.ListAPITokens(f.ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tokens {
		if strings.HasPrefix(tk.Name, "console ") {
			t.Fatalf("the one-off token must be gone: %+v", tk)
		}
	}
	if _, err := f.db.GetAPITokenByHash(f.ctx, auth.HashToken(token)); err == nil {
		t.Fatal("the token the command had still works")
	}

	// The daemons and the installer are not for a browser, nor is the demo
	// build's server; the release key is root's on the server alone; the
	// client flags are the console's own.
	for _, args := range [][]string{{"agent"}, {"helper", "--uid", "1"}, {"--json", "agent"}, {"--no-wait", "setup"}, {"app-exec", "--", "/bin/sh", "-c", "id"}, {"--json", "app-exec", "--env-file", "/x", "--", "/bin/id"}, {"sandbox-exec", "--rx", "/", "--", "/bin/sh", "-c", "id"}, {"--json", "demo", "serve", "--listen", ":9090"}, {"demo", "prepare"},
		{"update", "trust", "--off"}, {"update", "--no-wait", "trust"}, {"--json", "update", "trust", "--key", "made-up-key"},
		{"doctor", "--server", "https://x"}, {"doctor", "--token=abc"},
		{"site", "list", "--token", "abc"}, {"--token", "abc", "status"}, {"migrate", "run", "--server=https://x"}, {"migrate", "plan", "--config", "/tmp/c.yaml"},
		{"update", "settings", "--config", "/tmp/c.yaml"}, {"update", "settings", "--insecure"}, {"status", "--insecure=true"}, {"migrate", "grant", "user:alex", "--insecure"}} {
		if st, _, _ := consoleRaw(f, args); st != http.StatusUnprocessableEntity {
			t.Fatalf("%v must be refused, got %d", args, st)
		}
	}
	// The rest of update stays: the settings and the check go through the API.
	if st, _, body := consoleRaw(f, []string{"update", "check"}); st != http.StatusOK || !strings.Contains(body, "args: update check\n") {
		t.Fatalf("update check: %d\n%s", st, body)
	}

	// A command whose own --token is the source's or the repository's gets it,
	// and still reaches the panel with the console's token; so does the
	// migration's --insecure, the check of the source's certificate, which
	// leaves the console's own connection as it is (MP_INSECURE=1). The
	// token itself goes to the command's stdin (--token-stdin): its command
	// line is readable by every local user in ps.
	for _, c := range []struct{ args, run []string }{
		{[]string{"migrate", "plan", "--source", "https://old.example.com:8443", "--token", "SOURCE-TOKEN", "--scope", "user:alex"},
			[]string{"migrate", "plan", "--source", "https://old.example.com:8443", "--scope", "user:alex", "--token-stdin"}},
		{[]string{"--json", "migrate", "run", "--token=SOURCE-TOKEN", "--scope", "user:alex"},
			[]string{"--json", "migrate", "run", "--scope", "user:alex", "--token-stdin"}},
		{[]string{"update", "settings", "--repo", "owner/name", "--token", "SOURCE-TOKEN"},
			[]string{"update", "settings", "--repo", "owner/name", "--token-stdin"}},
		{[]string{"migrate", "plan", "--source", "https://old.example.com:8443", "--insecure", "--token", "SOURCE-TOKEN", "--scope", "user:alex"},
			[]string{"migrate", "plan", "--source", "https://old.example.com:8443", "--insecure", "--scope", "user:alex", "--token-stdin"}},
		{[]string{"--token", "SOURCE-TOKEN", "--insecure=true", "migrate", "run", "--scope", "user:alex"},
			[]string{"--insecure=true", "migrate", "run", "--scope", "user:alex", "--token-stdin"}},
	} {
		st, _, body := consoleRaw(f, c.args)
		if st != http.StatusOK || !strings.Contains(body, "args: "+strings.Join(c.run, " ")+"\n") || !strings.Contains(body, "\nstdin: SOURCE-TOKEN|\n") ||
			strings.Contains(body, "token: SOURCE-TOKEN") || !strings.Contains(body, "\ninsecure: 1\n") || strings.Count(body, "SOURCE-TOKEN") != 1 {
			t.Fatalf("%v: %d\n%s", c.args, st, body)
		}
	}
	// That token opens the source account's secrets: the audit log, which
	// every administrator reads, records the command without it.
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "console.run", Limit: 50})
	runs := 0
	for _, e := range entries {
		if strings.Contains(e.Target, "SOURCE-TOKEN") {
			t.Fatalf("the audit log keeps a token: %q", e.Target)
		}
		if strings.Contains(e.Target, "--token [redacted]") || strings.Contains(e.Target, "--token=[redacted]") {
			runs++
		}
	}
	if runs != 5 {
		t.Fatalf("audited runs with a token: %d of 5", runs)
	}
}

// A flag in front of the command takes the next argument as its value, as
// cobra reads it: it can neither hide a refused command from the console nor
// pass a client flag to a command that has no such flag of its own.
func TestConsoleReadsTheCommandAsCobraDoes(t *testing.T) {
	f := newSiteFixture(t)
	script := filepath.Join(t.TempDir(), "mp")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := consoleBinary
	consoleBinary = func() string { return script }
	t.Cleanup(func() { consoleBinary = prev })

	for _, args := range [][]string{
		// mp --cwd . fsop run -- php -r …: fsop run with --cwd=. (PHP as the
		// panel's service user), not a command named "."
		{"--cwd", ".", "fsop", "run", "--", "php", "-r", "echo 1;"},
		{"--uid", "0", "--gid", "0", "helper", "id"},
		{"--hostname", "x", "setup"},
		{"--strip", "1", "fsop", "extract", "a.zip", "x"},
		{"--json", "", "fsop", "read", "panel.db"},
		{"-", "agent"},
		{"-n", "5", "api"},
		// --token takes "plan" as its value: this is migrate grant, whose
		// --insecure and --token would be the console's own
		{"migrate", "--token", "plan", "grant", "user:alex", "--insecure"},
	} {
		if st, _, body := consoleRaw(f, args); st != http.StatusUnprocessableEntity {
			t.Errorf("%q must be refused, got %d:\n%s", args, st, body)
		}
	}
	// mp migrate --token x plan is migrate plan with its own --token, which
	// travels on stdin (--token-stdin).
	args := []string{"migrate", "--token", "SOURCE-TOKEN", "plan", "--insecure", "--scope", "user:alex"}
	if st, _, body := consoleRaw(f, args); st != http.StatusOK || !strings.Contains(body, "args: migrate plan --insecure --scope user:alex --token-stdin\n") {
		t.Errorf("%q: %d\n%s", args, st, body)
	}
}

// mp's local files are the server's in the console, read and written as the
// panel's own service account: the commands and flags that take them are
// refused there (the web interface has forms for the same), the rest of those
// commands is not.
func TestConsoleKeepsTheServersFiles(t *testing.T) {
	f := newSiteFixture(t)
	script := filepath.Join(t.TempDir(), "mp")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := consoleBinary
	consoleBinary = func() string { return script }
	t.Cleanup(func() { consoleBinary = prev })

	for _, args := range [][]string{
		{"files", "put", "/etc/monopanel/secret.key", "key.txt", "--user", "alex"},
		{"files", "get", "notes.txt", "/var/lib/monopanel/panel.db", "--user", "alex"},
		{"--json", "files", "--user", "alex", "get", "notes.txt", "-"},
		{"ssl", "import", "--cert", "/etc/ssl/site.pem", "--key", "/etc/ssl/site.key"},
		{"ssl", "panel", "import", "--cert=/etc/ssl/panel.pem", "--key=/etc/ssl/panel.key"},
		{"site", "nginx", "example.com", "--set", "/etc/monopanel/secret.key"},
		{"migrate", "plan", "--from", "fastpanel", "--source", "root@old.example.com", "--key", "/var/lib/monopanel/.ssh/id_ed25519"},
		{"migrate", "run", "--from", "bitrixvm", "--source", "root@old.example.com", "--key=/root/.ssh/id_rsa"},
	} {
		if st, _, body := consoleRaw(f, args); st != http.StatusUnprocessableEntity || !strings.Contains(body, "service account") {
			t.Errorf("%q must be refused, got %d:\n%s", args, st, body)
		}
	}
	for _, args := range [][]string{
		{"site", "nginx", "example.com"}, {"site", "nginx", "example.com", "--clear"}, {"ssl", "panel", "issue", "--dns", "cf"}, {"files", "ls", "--user", "alex"},
	} {
		if st, _, body := consoleRaw(f, args); st != http.StatusOK || !strings.Contains(body, "args: "+strings.Join(args, " ")+"\n") {
			t.Errorf("%q: %d\n%s", args, st, body)
		}
	}
}

func TestConsoleAuditLineHidesSecrets(t *testing.T) {
	for args, want := range map[string]string{
		"migrate plan --source https://a:8443 --token T0K --scope user:alex": "migrate plan --source https://a:8443 --token [redacted] --scope user:alex",
		"user add bob --password=hunter2 --email b@example.com":              "user add bob --password=[redacted] --email b@example.com",
		"backup target add s3 --url s3:x --env AWS_SECRET_ACCESS_KEY=abc":    "backup target add s3 --url s3:x --env AWS_SECRET_ACCESS_KEY=[redacted]",
		"dns-provider add cf --cred=CLOUDFLARE_DNS_API_TOKEN=xyz":            "dns-provider add cf --cred=CLOUDFLARE_DNS_API_TOKEN=[redacted]",
		"cms install example.com wordpress --admin-password p4ss":            "cms install example.com wordpress --admin-password [redacted]",
		"webhook add --url https://h --secret s3cr3t":                        "webhook add --url https://h --secret [redacted]",
		"db password shop --password":                                        "db password shop --password",
		"site list --json":                                                   "site list --json",
		"files put x -- --password literal":                                  "files put x -- --password literal",
	} {
		if got := consoleAuditLine(strings.Fields(args)); got != want {
			t.Errorf("%q:\n got %q\nwant %q", args, got, want)
		}
	}
}

// The command reaches the HTTPS listener where it listens: on 127.0.0.1 for
// an address of every interface, else on the one address it was given.
func TestConsoleServerURL(t *testing.T) {
	f := newSiteFixture(t)
	for listen, want := range map[string]string{
		"":                    "https://127.0.0.1:8443",
		":9443":               "https://127.0.0.1:9443",
		"0.0.0.0:8443":        "https://127.0.0.1:8443",
		"[::]:8443":           "https://127.0.0.1:8443",
		"203.0.113.5:8443":    "https://203.0.113.5:8443",
		"[2001:db8::5]:8443":  "https://[2001:db8::5]:8443",
		"panel.example.com:7": "https://panel.example.com:7",
		"garbage":             "https://127.0.0.1:8443",
	} {
		f.s.cfg.Web.Listen = listen
		if got := f.s.consoleServerURL(); got != want {
			t.Errorf("%q: %s, want %s", listen, got, want)
		}
	}
}

func TestConsoleCommandPath(t *testing.T) {
	for args, want := range map[string]string{
		"migrate plan --token x":                   "migrate plan",
		"--json migrate run":                       "migrate run",
		"update --no-wait settings":                "update settings",
		"site":                                     "site",
		"-- migrate plan":                          "",
		"migrate -- plan":                          "migrate",
		"migrate --token x plan":                   "migrate plan",
		"--token x --insecure migrate plan":        "migrate plan",
		"migrate --token plan grant":               "migrate grant",
		"--json=true migrate --insecure=false run": "migrate run",
		"update settings --token=x -y":             "update settings",
		"--cwd . fsop run -- php":                  "fsop run",
		"-n 5 api":                                 "api",
		"-xy agent":                                "agent",
		"- agent":                                  "agent",
		"-v agent":                                 "", // mp -v agent prints the version
		"-h site list":                             "list",
		"files --user bob ls":                      "files ls",
	} {
		if got := consoleCommand(strings.Fields(args)); got != want {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
	if got := consoleCommand([]string{"--json", "", "fsop", "read"}); got != "fsop read" {
		t.Errorf("an empty argument: %q, want fsop read", got)
	}
}

// Secrets typed in the console reach the command on its stdin, through the
// -stdin twin of their flag, never on the command line every local user can
// read; the echo of the command hides them like the audit log. One single
// value goes first, then the KEY=VALUE lines of a list.
func TestConsolePassesSecretsOnStdin(t *testing.T) {
	f := newSiteFixture(t)
	script := filepath.Join(t.TempDir(), "mp")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\necho \"stdin: $(cat | tr '\\n' '|')\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := consoleBinary
	consoleBinary = func() string { return script }
	t.Cleanup(func() { consoleBinary = prev })

	for _, c := range []struct {
		args       []string
		run, stdin string
	}{
		{[]string{"user", "add", "bob", "--password", "Made-Up-Pass-1"}, "user add bob --password-stdin", "Made-Up-Pass-1|"},
		// two secrets of one command, in the order mp reads them
		{[]string{"user", "set", "admin", "--password", "Made-Up-New-2", "--current-password=Made-Up-Old-1"}, "user set admin --current-password-stdin --password-stdin", "Made-Up-Old-1|Made-Up-New-2|"},
		{[]string{"mail", "box", "set", "a@example.com", "--password=Made-Up-Pass-1"}, "mail box set a@example.com --password-stdin", "Made-Up-Pass-1|"},
		{[]string{"cms", "install", "example.com", "wordpress", "--admin-password", "Made-Up-Pass-1"}, "cms install example.com wordpress --password-stdin", "Made-Up-Pass-1|"},
		{[]string{"backup", "target", "add", "s3", "--type", "s3", "--env", "AWS_ACCESS_KEY_ID=K", "--password", "Made-Up-Pass-1", "--env=AWS_SECRET_ACCESS_KEY=S"},
			"backup target add s3 --type s3 --password-stdin --env-stdin", "Made-Up-Pass-1|AWS_ACCESS_KEY_ID=K|AWS_SECRET_ACCESS_KEY=S|"},
		{[]string{"dns-provider", "add", "cf", "--type", "cloudflare", "--cred", "CLOUDFLARE_DNS_API_TOKEN=Made-Up"}, "dns-provider add cf --type cloudflare --cred-stdin", "CLOUDFLARE_DNS_API_TOKEN=Made-Up|"},
		{[]string{"app", "set", "web", "--user", "alex", "--env", "API_KEY=Made-Up"}, "app set web --user alex --env-stdin", "API_KEY=Made-Up|"},
		{[]string{"webhook", "add", "--url", "https://hooks.example.com/x", "--secret", "Made-Up"}, "webhook add --url https://hooks.example.com/x --secret-stdin", "Made-Up|"},
		// an empty value asks for nothing: nothing goes to stdin
		{[]string{"user", "set", "bob", "--password="}, "user set bob", ""},
		// a command without such flags keeps its arguments
		{[]string{"site", "list", "--json"}, "site list --json", ""},
	} {
		st, _, body := consoleRaw(f, c.args)
		if st != http.StatusOK || !strings.Contains(body, "args: "+c.run+"\n") || !strings.Contains(body, "\nstdin: "+c.stdin+"\n") {
			t.Fatalf("%v: %d\n%s", c.args, st, body)
		}
		if strings.Contains(body, "Made-Up") && !strings.Contains(c.stdin, "Made-Up") || strings.Count(body, "Made-Up") > strings.Count(c.stdin, "Made-Up") {
			t.Fatalf("%v: a secret shows outside stdin:\n%s", c.args, body)
		}
	}
	// No stdin to read in the console, two values for one stdin, a line break.
	for _, args := range [][]string{
		{"user", "add", "bob", "--password-stdin"},
		{"backup", "target", "add", "s3", "--password", "a-made-up-one", "--password", "another-made-up"},
		{"user", "add", "bob", "--password", "line\nbreak"},
	} {
		if st, _, body := consoleRaw(f, args); st != http.StatusUnprocessableEntity {
			t.Fatalf("%v must be refused: %d\n%s", args, st, body)
		}
	}
}

// Once the listener is bound, the console reaches the API at that very
// address — never a host name looked up again, which may lead to an address
// where another process listens with any certificate.
func TestConsoleUsesTheBoundAddress(t *testing.T) {
	f := newSiteFixture(t)
	f.s.cfg.Web.Listen = "panel.example.com:9443"
	for _, c := range []struct{ addr, want string }{
		{"203.0.113.5:9443", "https://203.0.113.5:9443"},
		{"[2001:db8::5]:9443", "https://[2001:db8::5]:9443"},
		{"0.0.0.0:9443", "https://127.0.0.1:9443"},
		{"[::]:9443", "https://127.0.0.1:9443"},
	} {
		a, err := net.ResolveTCPAddr("tcp", c.addr)
		if err != nil {
			t.Fatal(err)
		}
		f.s.listenAddr.Store(net.Addr(a))
		if got := f.s.consoleServerURL(); got != c.want {
			t.Errorf("bound to %s: %s, want %s", c.addr, got, c.want)
		}
	}
}
