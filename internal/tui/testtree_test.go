package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"monopanel/internal/client"
	"monopanel/internal/i18n"
)

func TestMain(m *testing.M) {
	if os.Getenv("MP_TUI_TEST_SLEEP") == "1" {
		// A stand-in for another running TUI (TestSweepEdits): this very
		// binary, alive until the test kills it.
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	i18n.Set("en")
	os.Exit(m.Run())
}

func noop(*cobra.Command, []string) error { return nil }

// testTree is a small mp: every shape of command the real tree has — the
// real one is checked by internal/parity, which may import the CLI.
func testTree() *cobra.Command {
	root := &cobra.Command{Use: "mp", RunE: noop}
	pf := root.PersistentFlags()
	pf.String("config", "", "path to config.yaml")
	pf.String("server", "", "panel address")
	pf.String("token", "", "API token for --server")
	pf.Bool("insecure", false, "do not verify TLS")
	pf.Bool("json", false, "JSON output")
	pf.Bool("no-wait", false, "do not wait for jobs")

	cmd := func(use, short string, args cobra.PositionalArgs) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: noop}
	}
	group := func(use string, subs ...*cobra.Command) *cobra.Command {
		g := &cobra.Command{Use: use, Short: use + " group"}
		g.AddCommand(subs...)
		return g
	}

	root.AddCommand(&cobra.Command{Use: "api", Hidden: true, RunE: noop}, cmd("setup", "initial setup", nil), cmd("version", "panel version", nil), cmd("status", "summary", nil))

	siteAdd := cmd("add <domain>", "create a site", cobra.ExactArgs(1))
	siteAdd.Flags().String("user", "", "owner (login); required for an administrator")
	siteAdd.Flags().String("php", "", "PHP version")
	siteAdd.Flags().String("mode", "fpm", "fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) or proxy (nginx → backend)")
	siteAdd.Flags().Bool("www", false, "add the alias www.<domain>")
	siteAdd.Flags().StringSlice("alias", nil, "additional names (repeatable)")
	siteAdd.Flags().Int("rate-limit", 0, "requests per second")
	siteSet := cmd("set <domain>", "change a site", cobra.ExactArgs(1))
	siteSet.Flags().String("php", "", "PHP version")
	siteSet.Flags().Bool("http2", true, "HTTP/2")
	siteSet.Flags().String("docroot", "", "docroot subdirectory (an empty string means the site root)")
	siteSet.Flags().StringSlice("ini", nil, "php_value key=value")
	siteSet.Flags().String("redirect-www", "", "none, to_www or to_root")
	siteRm := cmd("rm <domain>", "delete a site", cobra.ExactArgs(1))
	siteRm.Flags().Bool("purge", false, "delete the site directory")
	siteLogs := cmd("logs <domain>", "tail of a site's log", cobra.ExactArgs(1))
	siteLogs.Flags().IntP("lines", "n", 100, "number of lines")
	siteLogs.Flags().String("type", "access", "access, error, php, slow, apache-access, apache-error")
	moveIP := cmd("move-ip <new-address>", "move sites to another address", cobra.ExactArgs(1))
	moveIP.Flags().String("from", "", "the previous address")
	nginx := cmd("nginx <domain>", "the site's custom nginx directives", cobra.ExactArgs(1))
	nginx.Flags().String("set", "", "write the directives from a file ('-' = stdin); checked with nginx -t")
	nginx.Flags().Bool("clear", false, "delete the custom directives")
	root.AddCommand(group("site", siteAdd, siteSet, siteRm, siteLogs, moveIP, nginx, cmd("apply <domain>", "apply", cobra.ExactArgs(1)), cmd("fix <domain>", "fix", cobra.ExactArgs(1)),
		cmd("suspend <domain>", "suspend a site", cobra.ExactArgs(1)), cmd("unsuspend <domain>", "resume a site", cobra.ExactArgs(1)), cmd("list", "list sites", nil)))

	userAdd := cmd("add <login>", "create a user", cobra.ExactArgs(1))
	userAdd.Flags().String("password", "", "password for signing in to the panel")
	userAdd.Flags().Bool("password-stdin", false, "read the password from stdin")
	userAdd.Flags().Bool("generate", false, "generate a password and show it")
	userAdd.Flags().String("role", "user", "role: user or admin")
	userRm := cmd("rm <login>", "delete a user", cobra.ExactArgs(1))
	userRm.Flags().Bool("purge", false, "delete the home directory")
	userRm.Flags().BoolP("yes", "y", false, "do not ask for confirmation")
	userSet := cmd("set <login>", "change a user", cobra.ExactArgs(1))
	userSet.Annotations = map[string]string{StdinOrderAnnotation: "current-password-stdin,password-stdin"}
	userSet.Flags().String("email", "", "e-mail")
	userSet.Flags().Bool("current-password-stdin", false, "read the current password from stdin")
	userSet.Flags().String("password", "", "new password")
	userSet.Flags().Bool("password-stdin", false, "read the new password from stdin")
	userSet.Flags().String("status", "", "active or suspended")
	userSet.Flags().Bool("shell", false, "allow an SSH shell")
	root.AddCommand(group("user", userAdd, userRm, userSet, cmd("list", "list users", nil)))

	cronGroup := group("cron")
	cronGroup.PersistentFlags().String("user", "", "user login")
	cronAdd := cmd("add", "add a cron job", nil)
	cronAdd.Flags().String("schedule", "", "schedule")
	cronAdd.Flags().String("command", "", "command")
	cronAdd.Flags().Bool("disabled", false, "create it disabled")
	cronAdd.MarkFlagRequired("schedule") //nolint:errcheck // declared above
	cronAdd.MarkFlagRequired("command")  //nolint:errcheck // declared above
	cronGroup.AddCommand(cronAdd, cmd("rm <id>", "delete a cron job", cobra.ExactArgs(1)), cmd("list", "list cron jobs", nil))
	root.AddCommand(cronGroup)

	alias := cmd("add <info@example.com> <dest> [dest...]", "create an alias", cobra.MinimumNArgs(2))
	root.AddCommand(group("mail", group("alias", alias), cmd("apply", "regenerate and apply the mail configuration", nil)))

	dbSet := cmd("set key=value ...", "set parameters", cobra.MinimumNArgs(1))
	db := group("db", group("config", dbSet))
	db.Commands()[0].RunE = noop // db config shows itself: a runnable group
	root.AddCommand(db)

	appGroup := group("app")
	appGroup.PersistentFlags().String("user", "", "owner's login")
	appSet := cmd("set <name>", "change an app service", cobra.ExactArgs(1))
	appSet.Flags().StringSlice("env", nil, "replace the list of KEY=value variables")
	appSet.Flags().Bool("enable", false, "enable autostart and start")
	appSet.Flags().Bool("disable", false, "stop and disable autostart")
	appGroup.AddCommand(appSet)
	root.AddCommand(appGroup)

	// One secret and one list of them, both with stdin twins.
	target := cmd("add <name>", "register a repository", cobra.ExactArgs(1))
	target.Flags().String("password", "", "repository password (generated if omitted)")
	target.Flags().Bool("password-stdin", false, "read the repository password from stdin")
	target.Flags().StringSlice("env", nil, "variables for cloud storage, for example AWS_ACCESS_KEY_ID=…")
	target.Flags().Bool("env-stdin", false, "read the KEY=VALUE variables from stdin, one per line")
	target.Flags().Int("keep-daily", 7, "daily snapshots to keep")
	root.AddCommand(group("backup", group("target", target)))

	dnsAdd := cmd("add <name>", "add a provider", cobra.ExactArgs(1))
	dnsAdd.Flags().String("type", "cloudflare", "cloudflare, hetzner, digitalocean, rfc2136")
	dnsAdd.Flags().StringSlice("cred", nil, "KEY=VALUE, e.g. CLOUDFLARE_DNS_API_TOKEN=… (a type's keys: mp dns-provider types)")
	dnsAdd.Flags().Bool("cred-stdin", false, "read the KEY=VALUE credentials from stdin, one per line")
	root.AddCommand(group("dns-provider", dnsAdd))

	update := &cobra.Command{Use: "update", Short: "panel updates", RunE: noop}
	trust := cmd("trust", "the release key", nil)
	trust.Flags().String("key", "", "ed25519 public key in base64")
	trust.Flags().Bool("off", false, "switch the signature check off")
	trust.Flags().Bool("clear", false, "back to the built-in key")
	settings := cmd("settings", "update settings", nil)
	settings.Flags().String("token", "", "repository access token")
	settings.Flags().Bool("token-stdin", false, "read the token from stdin")
	settings.Flags().Bool("clear-token", false, "delete the saved token")
	settings.Flags().Int("check-hours", 24, "how often to check")
	settings.Flags().String("channel", "", "stable or beta")
	update.AddCommand(trust, settings)
	root.AddCommand(update)

	plan := cmd("plan", "dry run", nil)
	plan.Flags().String("from", "monopanel", "the panel to move from: monopanel | fastpanel | bitrixvm")
	plan.Flags().String("source", "", "source")
	plan.Flags().String("token", "", "the token issued by the source")
	plan.Flags().Bool("insecure", false, "do not verify the source's certificate")
	plan.Flags().Bool("password-stdin", false, "read the source's ssh password from stdin")
	plan.Flags().String("key", "", "private ssh key for the source (defaults to ~/.ssh/id_ed25519 or id_rsa)")
	plan.MarkFlagRequired("source") //nolint:errcheck // declared above
	// Two one-line secrets: the command cannot read both from one stdin.
	run := cmd("run", "move the account here", nil)
	run.Flags().String("from", "monopanel", "the panel to move from: monopanel | fastpanel | bitrixvm")
	run.Flags().String("source", "", "source")
	run.Flags().String("token", "", "the token issued by the source")
	run.Flags().Bool("token-stdin", false, "read the source's token from stdin")
	run.Flags().Bool("password-stdin", false, "read the source's ssh password from stdin")
	root.AddCommand(group("migrate", plan, run))

	selinux := cmd("selinux [enforcing|permissive]", "SELinux mode", cobra.MaximumNArgs(1))
	selinux.Flags().Bool("yes", false, "do not ask")
	root.AddCommand(selinux)

	vk := cmd("add <cache|sessions>", "create an instance", cobra.ExactArgs(1))
	vk.ValidArgs = []string{"cache", "sessions"}
	vk.Flags().Int("memory", 0, "memory in MB")
	root.AddCommand(group("valkey", vk))

	cfgSet := cmd("set <key> <value>", "change a setting", cobra.ExactArgs(2))
	cfgSet.Flags().Bool("restart", false, "restart the API")
	cfgSet.Flags().Bool("local", false, "write the file directly (root)")
	root.AddCommand(group("config", cfgSet))

	files := group("files", cmd("ls [path]", "list a directory", cobra.MaximumNArgs(1)), cmd("get <remote> [local]", "download a file", cobra.RangeArgs(1, 2)), cmd("rm <path>", "delete a file", cobra.ExactArgs(1)))
	files.PersistentFlags().String("user", "", "owner of the files")
	root.AddCommand(files)

	renew := cmd("renew <id|hostname>", "renew a certificate", cobra.MaximumNArgs(1))
	renew.Flags().Bool("all", false, "all ACME certificates")
	root.AddCommand(group("ssl", renew))

	fw := group("firewall", cmd("apply", "regenerate and apply", nil), cmd("enable", "enable", nil), cmd("disable", "disable", nil), cmd("unban <ip>", "unban an address", cobra.ExactArgs(1)))
	root.AddCommand(fw)

	tokCreate := cmd("create", "create a token", nil)
	tokCreate.Flags().String("name", "cli", "token name")
	tokCreate.Flags().String("user", "", "account to issue the token for")
	tokCreate.Flags().Int("expires", 0, "lifetime in days")
	root.AddCommand(group("token", tokCreate, cmd("revoke <id>", "revoke a token", cobra.ExactArgs(1)), cmd("list", "an account's tokens", nil)))

	jobList := cmd("list", "recent jobs", nil)
	jobList.Flags().Int("limit", 30, "how many jobs to show")
	jobList.Flags().String("status", "", "filter: queued running done failed")
	root.AddCommand(group("job", jobList, cmd("show <id>", "a job with its log", cobra.ExactArgs(1)), cmd("wait <id>", "wait for the job", cobra.ExactArgs(1))))

	svc := group("service", cmd("status [unit]", "service status", cobra.MaximumNArgs(1)))
	for _, action := range []string{"start", "stop", "reload", "restart", "enable", "disable"} {
		svc.AddCommand(cmd(action+" <unit>", action+" a service", cobra.ExactArgs(1)))
	}
	root.AddCommand(svc)

	// A group no area knows: it must land in "Other".
	root.AddCommand(group("extra", cmd("run <thing>", "an unknown command", cobra.ExactArgs(1))))
	return root
}

// newerTree is the test tree with the commands a newer mp brings: the menus
// pick them up where they belong.
func newerTree() *cobra.Command {
	root := testTree()
	root.AddCommand(&cobra.Command{Use: "whoami", Short: "who am I", RunE: noop})
	findCmd(root, "service").AddCommand(&cobra.Command{Use: "reload-or-restart <unit>", Short: "reload a service, or restart it", Args: cobra.ExactArgs(1), RunE: noop})
	return root
}

// fakeRunner records the commands instead of running them.
type fakeRunner struct {
	mu    sync.Mutex
	jobs  []runJob
	envs  [][]string
	terms []runJob
	out   string
	code  int
}

func (f *fakeRunner) start(job runJob, env []string) (<-chan runEvent, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, job)
	f.envs = append(f.envs, env)
	ch := make(chan runEvent, 4)
	if f.out != "" {
		ch <- runEvent{out: f.out}
	}
	ch <- runEvent{done: true, code: f.code}
	close(ch)
	return ch, func() {}, nil
}

func (f *fakeRunner) terminal(job runJob, env []string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terms = append(f.terms, job)
	return exec.Command("true")
}

func (f *fakeRunner) editor(string, []string) *exec.Cmd { return exec.Command("true") }

func (f *fakeRunner) started() []runJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runJob{}, f.jobs...)
}

// fakeAPI answers the reads the screens make with canned JSON.
func fakeAPI(t *testing.T, role string, routes map[string]any) *client.Client {
	t.Helper()
	me := map[string]any{"user_id": 2, "login": "bob", "role": role, "via": "token"}
	if role == "admin" {
		me = map[string]any{"user_id": 1, "login": "admin", "role": role, "via": "token"}
	}
	base := map[string]any{
		"/api/v1/system/status": map[string]any{"panel": map[string]any{"version": "9.9.9", "agent_ok": true}, "host": map[string]any{"hostname": "h1", "load": []float64{0.1, 0.2, 0.3}, "mem_total_bytes": 1 << 30, "mem_available_bytes": 1 << 29}},
		"/api/v1/auth/me":       me,
	}
	for k, v := range routes {
		base[k] = v
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := base[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":404,"title":"Not Found"}`)) //nolint:errcheck // test server
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return client.NewRemote(srv.URL, "secret-token", false)
}

// key builds a key press the way Bubble Tea delivers it.
func key(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	if c, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return tea.KeyPressMsg{Code: rune(c[0]), Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// drive runs the commands an update returned and feeds their messages
// back, the way the program loop would; ticks and terminal hand-overs are
// skipped.
func drive(m *model, cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0 && steps < 500; steps++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- c() }()
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(500 * time.Millisecond):
			continue // a tick
		}
		switch x := msg.(type) {
		case nil, tea.QuitMsg:
			continue
		case tea.BatchMsg:
			queue = append(queue, x...)
			continue
		}
		if strings.HasPrefix(typeName(msg), "tea.execMsg") {
			continue
		}
		_, next := m.Update(msg)
		queue = append(queue, next)
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

func press(m *model, keys ...string) {
	for _, k := range keys {
		_, cmd := m.Update(key(k))
		drive(m, cmd)
	}
}

func typeText(m *model, s string) {
	for _, r := range s {
		_, cmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		drive(m, cmd)
	}
}

func newTestModel(t *testing.T, role string, routes map[string]any) (*model, *fakeRunner) {
	t.Helper()
	return newTestModelOf(t, testTree(), role, routes)
}

func newTestModelOf(t *testing.T, root *cobra.Command, role string, routes map[string]any) (*model, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{}
	m := newModel(fakeAPI(t, role, routes), root, Options{Server: "https://panel:8443", Token: "secret-token"}, fr)
	m.width, m.height = 80, 24
	m.tick = func() tea.Cmd { return nil }
	m.runTick = func(int) tea.Cmd { return nil }
	drive(m, m.Init())
	return m, fr
}
