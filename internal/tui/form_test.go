package tui

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"monopanel/internal/i18n"
)

func TestParseUse(t *testing.T) {
	for _, c := range []struct {
		use  string
		want []argSpec
	}{
		{"list", nil},
		{"add <domain>", []argSpec{{name: "domain", required: true}}},
		{"ls [path]", []argSpec{{name: "path"}}},
		{"get <remote> [local]", []argSpec{{name: "remote", required: true}, {name: "local"}}},
		{"add <info@example.com> <dest> [dest...]", []argSpec{{name: "info@example.com", required: true}, {name: "dest", required: true}, {name: "dest", variadic: true}}},
		{"rm <path>...", []argSpec{{name: "path", required: true, variadic: true}}},
		{"set key=value ...", []argSpec{{name: "key=value", required: true, variadic: true}}},
		{"add <login> --site <domain> --path <folder>", []argSpec{{name: "login", required: true}}},
		{"selinux [enforcing|permissive]", []argSpec{{name: "enforcing|permissive"}}},
		{"issue <hostname> [hostname...]", []argSpec{{name: "hostname", required: true}, {name: "hostname", variadic: true}}},
	} {
		if got := parseUse(c.use); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseUse(%q) = %+v, want %+v", c.use, got, c.want)
		}
	}
}

func TestChoicesFromUsage(t *testing.T) {
	for _, c := range []struct {
		usage, def string
		want       []string
	}{
		{"fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) or proxy (nginx → backend)", "fpm", []string{"fpm", "apache", "proxy"}},
		{"true|false — offer POP3", "", []string{"true", "false"}},
		{"the panel to move from: monopanel | fastpanel | bitrixvm", "monopanel", []string{"monopanel", "fastpanel", "bitrixvm"}},
		{"none, to_www or to_root", "", []string{"none", "to_www", "to_root"}},
		{"local, sftp, s3, b2, rest", "local", []string{"local", "sftp", "s3", "b2", "rest"}},
		{"access, error, php, slow, apache-access, apache-error", "access", []string{"access", "error", "php", "slow", "apache-access", "apache-error"}},
		{"1h, 6h, 24h, 7d, 30d", "1h", []string{"1h", "6h", "24h", "7d", "30d"}},
		{"role: user or admin", "user", []string{"user", "admin"}},
		{"always (default), on-failure or no", "", []string{"always", "on-failure", "no"}},
		{"where PHP keeps sessions: files (the account's tmp) or valkey (the account's sessions instance, mp valkey add sessions)", "", []string{"files", "valkey"}},
		{"CMS preset: wordpress, joomla, bitrix, opencart; an empty string selects the universal one", "", []string{"wordpress", "joomla", "bitrix", "opencart"}},
		{"auto (Let's Encrypt, issued automatically) or none", "auto", []string{"auto", "none"}},
		// Free text that only looks like a list.
		{"port or range", "", nil},
		{"version or tag (default: the latest in the channel)", "", nil},
		{"IP or CIDR", "", nil},
		{"server | user:<login> | site:<domain> | db:<name>", "server", nil},
		{"Bitrix solution: clean (the Marketplace clean install, default), demo (the demo site from the distribution) or the id of a Marketplace solution", "", nil},
		{"docroot subdirectory, for example public", "", nil},
		{"daily — a backup of the whole server every day", "", nil},
		{"repository name", "", nil},
		{"comma-separated list: job.failed, site.apply.done, cert.issue.*, backup.run.*, *", "*", nil},
		{"path or URL of the restic repository", "", nil},
	} {
		if got := choicesFromUsage(c.usage, c.def); !reflect.DeepEqual(got, c.want) {
			t.Errorf("choicesFromUsage(%q) = %q, want %q", c.usage, got, c.want)
		}
	}
	i18n.Set("ru")
	defer i18n.Set("en")
	if got := choicesFromUsage("ondemand, dynamic или static", ""); !reflect.DeepEqual(got, []string{"ondemand", "dynamic", "static"}) {
		t.Errorf("Russian list: %q", got)
	}
	if got := choicesFromUsage("fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) или proxy (nginx → backend)", "fpm"); !reflect.DeepEqual(got, []string{"fpm", "apache", "proxy"}) {
		t.Errorf("Russian list with remarks: %q", got)
	}
	if got := choicesFromUsage("роль: user или admin", "user"); !reflect.DeepEqual(got, []string{"user", "admin"}) {
		t.Errorf("Russian labelled list: %q", got)
	}
	if got := choicesFromUsage("порт или диапазон", ""); got != nil {
		t.Errorf("Russian free text: %q", got)
	}
}

func fieldNames(f *form) []string {
	var out []string
	for _, fd := range f.fields {
		out = append(out, fd.label)
	}
	return out
}

func fieldByName(f *form, name string) *field {
	for _, fd := range f.fields {
		if fd.name == name {
			return fd
		}
	}
	return nil
}

func adminEnv() formEnv { return formEnv{admin: true} }

func TestFormFromFlags(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	if got, want := fieldNames(f), []string{"<domain>", "--user", "--php", "--mode", "--www", "--alias", "--rate-limit"}; !slices.Equal(got, want) {
		t.Fatalf("fields %q, want %q (no global flags, no help)", got, want)
	}
	if d := f.fields[0]; !d.arg || !d.required || d.kind != kindText {
		t.Errorf("<domain>: %+v", d)
	}
	mode := fieldByName(f, "mode")
	if mode.kind != kindEnum || !slices.Equal(mode.choices, []string{"fpm", "apache", "proxy"}) || mode.value() != "fpm" {
		t.Errorf("--mode: kind %d choices %q value %q", mode.kind, mode.choices, mode.value())
	}
	if fd := fieldByName(f, "www"); fd.kind != kindBool || fd.value() != "false" {
		t.Errorf("--www: %+v", fd)
	}
	if fd := fieldByName(f, "alias"); fd.kind != kindList || fd.value() != "" {
		t.Errorf("--alias: %+v", fd)
	}
	if fd := fieldByName(f, "rate-limit"); fd.kind != kindNumber || fd.value() != "0" {
		t.Errorf("--rate-limit: %+v", fd)
	}
	if fd := fieldByName(f, "php"); fd.usage != "PHP version" {
		t.Errorf("help line from the usage: %q", fd.usage)
	}

	// Group flags come first; required flags are marked.
	f = newForm(findCmd(root, "cron add"), adminEnv(), nil, nil)
	if got, want := fieldNames(f), []string{"--user", "--schedule*", "--command*", "--disabled"}; !slices.Equal(got, want) {
		t.Errorf("cron add fields %q, want %q", got, want)
	}

	// A local --token shadows the global one and is shown, masked.
	f = newForm(findCmd(root, "migrate plan"), adminEnv(), nil, nil)
	tok := fieldByName(f, "token")
	if tok == nil || !tok.secret {
		t.Fatalf("migrate plan --token: %+v", tok)
	}
	if fd := fieldByName(f, "key"); fd == nil || fd.secret {
		t.Errorf("--key with a file path must not be masked: %+v", fd)
	}
	if fd := fieldByName(f, "from"); fd.kind != kindEnum || fd.value() != "monopanel" {
		t.Errorf("--from: %+v", fd)
	}
	if fd := fieldByName(f, "insecure"); fd == nil || fd.kind != kindBool {
		t.Errorf("the local --insecure of migrate plan is its own flag: %+v", fd)
	}

	// Positional enums from ValidArgs and from the known lists; "<id|hostname>"
	// stays text.
	f = newForm(findCmd(root, "valkey add"), adminEnv(), nil, nil)
	if fd := f.fields[0]; fd.kind != kindEnum || !slices.Equal(fd.choices, []string{"", "cache", "sessions"}) {
		t.Errorf("valkey add <cache|sessions>: %+v", fd)
	}
	f = newForm(findCmd(root, "selinux"), adminEnv(), nil, nil)
	if fd := f.fields[0]; fd.kind != kindEnum || !slices.Contains(fd.choices, "permissive") {
		t.Errorf("selinux [mode]: %+v", fd)
	}
	f = newForm(findCmd(root, "ssl renew"), adminEnv(), nil, nil)
	if fd := f.fields[0]; fd.kind != kindText {
		t.Errorf("<id|hostname> is not a list of values: %+v", fd)
	}
	f = newForm(findCmd(root, "config set"), adminEnv(), nil, nil)
	if fd := f.fields[0]; fd.kind != kindEnum || !slices.Contains(fd.choices, "web.listen") {
		t.Errorf("config set <key>: %+v", fd)
	}
}

func TestSecretsAndStdin(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "user add"), adminEnv(), nil, nil)
	if fd := fieldByName(f, "password-stdin"); fd != nil {
		t.Errorf("--password-stdin is not a toggle: %+v", fd)
	}
	pw := fieldByName(f, "password")
	if pw == nil || pw.stdin != "password-stdin" || !pw.secret {
		t.Fatalf("the password goes through stdin: %+v", pw)
	}
	for _, fd := range f.fields {
		if fd.label == "--password" {
			t.Errorf("--password next to --password-stdin must not be on the command line")
		}
	}
	f.fields[0].setValue("bob")
	pw.setValue("s3cr3t pass")
	argv, shown, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"user", "add", "bob", "--password-stdin"}) || stdin != "s3cr3t pass" {
		t.Errorf("argv %q stdin %q", argv, stdin)
	}
	if strings.Contains(strings.Join(shown, " "), "s3cr3t") || strings.Contains(f.line(), "s3cr3t") {
		t.Errorf("the secret is shown: %q / %q", shown, f.line())
	}

	// --token next to --token-stdin goes, --clear-token (a switch) stays.
	f = newForm(findCmd(root, "update settings"), adminEnv(), nil, nil)
	if fd := fieldByName(f, "clear-token"); fd == nil || fd.kind != kindBool {
		t.Errorf("--clear-token: %+v", fd)
	}
	if fd := fieldByName(f, "token"); fd == nil || fd.stdin != "token-stdin" {
		t.Errorf("--token goes through stdin: %+v", fd)
	}
	if fd := fieldByName(f, "channel"); fd.kind != kindEnum || !slices.Equal(fd.choices, []string{"", "stable", "beta"}) {
		t.Errorf("--channel: %+v", fd)
	}

	// A plain secret flag is masked on screen and in the line, but runs as is.
	f = newForm(findCmd(root, "migrate plan"), adminEnv(), nil, nil)
	fieldByName(f, "source").setValue("https://old:8443")
	fieldByName(f, "token").setValue("tok-123")
	argv, _, _, err = f.build()
	if err != nil || !slices.Contains(argv, "--token=tok-123") {
		t.Errorf("argv %q err %v", argv, err)
	}
	if line := f.line(); strings.Contains(line, "tok-123") || !strings.Contains(line, "--token •••") {
		t.Errorf("line %q", line)
	}
}

func TestBuildArgv(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "site set"), adminEnv(), nil, nil)
	f.fields[0].setValue("example.com")
	fieldByName(f, "http2").setValue("false")
	fieldByName(f, "ini").setValue("memory_limit=256M, upload_max_filesize=64M,")
	argv, _, _, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"site", "set", "example.com", "--http2=false", "--ini=memory_limit=256M,upload_max_filesize=64M"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if line := f.line(); line != "mp site set example.com --http2=false --ini memory_limit=256M,upload_max_filesize=64M" {
		t.Errorf("line %q", line)
	}

	// Unchanged fields stay off the line; ctrl+p passes a default as is.
	f = newForm(findCmd(root, "site set"), adminEnv(), nil, nil)
	f.fields[0].setValue("example.com")
	fieldByName(f, "http2").pinned = true
	argv, _, _, _ = f.build()
	if !slices.Equal(argv, []string{"site", "set", "example.com", "--http2"}) {
		t.Errorf("pinned: %q", argv)
	}

	// An emptied number means the default.
	f = newForm(findCmd(root, "site logs"), adminEnv(), nil, nil)
	f.fields[0].setValue("example.com")
	fieldByName(f, "lines").setValue("")
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"site", "logs", "example.com"}) {
		t.Errorf("empty number: %q", argv)
	}
	fieldByName(f, "lines").setValue("x1")
	if _, _, _, err := f.build(); err == nil {
		t.Error("a number field takes numbers")
	}

	// A value starting with "-" is not a flag.
	f = newForm(findCmd(root, "files rm"), adminEnv(), nil, nil)
	f.fields[0].setValue("-odd-name")
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"files", "rm", "--", "-odd-name"}) {
		t.Errorf("dash value: %q", argv)
	}

	// Several values in one field, with quotes.
	f = newForm(findCmd(root, "mail alias add"), adminEnv(), nil, nil)
	f.fields[0].setValue("info@example.com")
	f.fields[1].setValue("bob@example.com")
	f.fields[2].setValue(`alice@example.com "carol x@example.com"`)
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"mail", "alias", "add", "info@example.com", "bob@example.com", "alice@example.com", "carol x@example.com"}) {
		t.Errorf("variadic: %q", argv)
	}

	// Required arguments and flags are checked before running.
	f = newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "<domain>") {
		t.Errorf("missing <domain>: %v", err)
	}
	f = newForm(findCmd(root, "cron add"), adminEnv(), nil, nil)
	fieldByName(f, "schedule").setValue("@daily")
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "--command") {
		t.Errorf("missing --command: %v", err)
	}
	// cobra decides the count: renew runs with --all and no argument.
	f = newForm(findCmd(root, "ssl renew"), adminEnv(), nil, nil)
	fieldByName(f, "all").setValue("true")
	if argv, _, _, err := f.build(); err != nil || !slices.Equal(argv, []string{"ssl", "renew", "--all"}) {
		t.Errorf("renew --all: %q %v", argv, err)
	}
	// A gap in the positional arguments.
	f = newForm(findCmd(root, "files get"), adminEnv(), nil, nil)
	f.fields[1].setValue("local.txt")
	if _, _, _, err := f.build(); err == nil {
		t.Error("[local] without <remote> must be refused")
	}
}

func TestBinding(t *testing.T) {
	root := testTree()
	row := map[string]string{"domain": "example.com", "login": "bob", "php": "8.3", "http2": "false", "docroot": "public"}
	a := &act{path: "site set", args: []string{"domain"}, prefill: same("php", "http2", "docroot")}
	f := newForm(findCmd(root, "site set"), adminEnv(), a, row)
	if f.fields[0].value() != "example.com" {
		t.Fatalf("argument from the row: %q", f.fields[0].value())
	}
	if fieldByName(f, "php").value() != "8.3" || fieldByName(f, "http2").value() != "false" {
		t.Errorf("prefill from the row")
	}
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"site", "set", "example.com"}) {
		t.Errorf("an unchanged prefill is not passed: %q", argv)
	}
	if f.focus == 0 {
		t.Error("the focus skips the bound argument")
	}
	fieldByName(f, "php").setValue("8.4")
	fieldByName(f, "http2").setValue("true")
	fieldByName(f, "docroot").setValue("")
	argv, _, _, _ := f.build()
	if !slices.Equal(argv, []string{"site", "set", "example.com", "--php=8.4", "--http2", "--docroot="}) {
		t.Errorf("changed values are passed, an emptied one as empty: %q", argv)
	}

	// Context flags are passed whenever they have a value; literals too.
	a = &act{path: "cron add", flags: map[string]string{"user": "login", "disabled": "=true"}}
	f = newForm(findCmd(root, "cron add"), adminEnv(), a, row)
	fieldByName(f, "schedule").setValue("@daily")
	fieldByName(f, "command").setValue("php x.php")
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"cron", "add", "--user=bob", "--schedule=@daily", "--command=php x.php", "--disabled"}) {
		t.Errorf("context flags: %q", argv)
	}
	if line := f.line(); line != "mp cron add --user bob --schedule @daily --command 'php x.php' --disabled" {
		t.Errorf("line %q", line)
	}

	// Ordinary accounts do not get --user; only: a focused form.
	f = newForm(findCmd(root, "cron add"), formEnv{}, a, row)
	if fieldByName(f, "user") != nil {
		t.Error("--user is for administrators")
	}
	f = newForm(findCmd(root, "user add"), adminEnv(), &act{path: "user add", only: []string{"password"}}, nil)
	if got := fieldNames(f); !slices.Equal(got, []string{"<login>", "password (stdin)"}) {
		t.Errorf("only: %q", got)
	}

	// rowBound: an action that needs a row versus one the screen binds.
	if !(&act{args: []string{"id"}}).rowBound(map[string]string{"login": "bob"}) {
		t.Error("id comes from a row")
	}
	if (&act{flags: map[string]string{"user": "login"}}).rowBound(map[string]string{"login": "bob"}) {
		t.Error("login comes from the screen")
	}
	if (&act{args: []string{"", "dirslash"}}).rowBound(map[string]string{"dirslash": "/data/"}) {
		t.Error("an argument left to the user needs no row")
	}
}

func TestRemoteAndLocalFlags(t *testing.T) {
	root := testTree()
	if f := newForm(findCmd(root, "config set"), formEnv{admin: true, remote: true}, nil, nil); fieldByName(f, "local") != nil {
		t.Error("--local is hidden in remote mode")
	}
	if f := newForm(findCmd(root, "config set"), adminEnv(), nil, nil); fieldByName(f, "local") == nil {
		t.Error("--local is offered locally")
	}
	if offered(findCmd(root, "update trust"), Options{Server: "https://x"}) {
		t.Error("update trust is local only")
	}
	if !offered(findCmd(root, "update trust"), Options{}) {
		t.Error("update trust is offered locally")
	}
	for _, p := range []string{"api", "setup", "version"} {
		if offered(findCmd(root, p), Options{}) {
			t.Errorf("%s is CLI only", p)
		}
	}
}

func TestConfirmPolicy(t *testing.T) {
	root := testTree()
	form := func(path string, set map[string]string, args ...string) *form {
		f := newForm(findCmd(root, path), adminEnv(), nil, nil)
		i := 0
		for _, fd := range f.fields {
			if fd.arg && i < len(args) {
				fd.setValue(args[i])
				i++
			}
		}
		for k, v := range set {
			fieldByName(f, k).setValue(v)
		}
		return f
	}
	if c := confirmFor(form("site logs", nil, "example.com")); c != nil {
		t.Errorf("reading a log needs no confirmation: %+v", c)
	}
	// The web UI asks before regenerating a site's configuration.
	if c := confirmFor(form("site apply", nil, "example.com")); c == nil || !strings.Contains(c.note, "example.com") || c.word != "" {
		t.Errorf("site apply asks, y/N, saying what it rewrites: %+v", c)
	}
	if c := confirmFor(form("site rm", nil, "example.com")); c == nil || c.word != "" {
		t.Errorf("site rm: y/N, got %+v", c)
	}
	if c := confirmFor(form("site rm", map[string]string{"purge": "true"}, "example.com")); c == nil || c.word != "example.com" {
		t.Errorf("site rm --purge: type the domain, got %+v", c)
	}
	if c := confirmFor(form("user rm", nil, "bob")); c == nil || c.word != "bob" {
		t.Errorf("user rm: type the login, got %+v", c)
	}
	if c := confirmFor(form("firewall apply", nil)); c == nil {
		t.Error("firewall apply asks")
	}
	if c := confirmFor(form("firewall enable", nil)); c != nil {
		t.Error("firewall enable does not ask")
	}
	if c := confirmFor(form("selinux", nil, "enforcing")); c != nil {
		t.Error("selinux enforcing does not ask")
	}
	if c := confirmFor(form("selinux", nil, "permissive")); c == nil || c.note == "" {
		t.Error("selinux permissive asks, with what it costs")
	}
	f := form("user rm", nil, "bob")
	if !f.yes {
		t.Error("user rm has --yes: the TUI asks and passes it")
	}
	if strings.Contains(f.line(), "--yes") != true {
		t.Errorf("the line shows --yes: %q", f.line())
	}
}

func TestFill(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	ready, err := f.fill([]string{"example.com", "--php", "8.4", "--www", "--alias", "a.example", "--alias=b.example", "--json"})
	if err != nil || !ready {
		t.Fatalf("fill: %v %v", ready, err)
	}
	argv, _, _, err := f.build()
	if err != nil || !slices.Equal(argv, []string{"site", "add", "example.com", "--php=8.4", "--www", "--alias=a.example,b.example", "--json"}) {
		t.Errorf("argv %q err %v", argv, err)
	}
	f = newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	if _, err := f.fill([]string{"x", "--server", "https://evil"}); err == nil {
		t.Error("--server is not part of a command")
	}
	f = newForm(findCmd(root, "user add"), adminEnv(), nil, nil)
	ready, err = f.fill([]string{"bob", "--password-stdin"})
	if err != nil || ready {
		t.Errorf("--password-stdin needs the secret in the form: %v %v", ready, err)
	}
	f = newForm(findCmd(root, "user add"), adminEnv(), nil, nil)
	if _, err := f.fill([]string{"bob", "--password", "p4ss"}); err != nil {
		t.Fatal(err)
	}
	if _, _, stdin, _ := f.build(); stdin != "p4ss" {
		t.Errorf("a typed --password goes through stdin, got %q", stdin)
	}
	f = newForm(findCmd(root, "migrate plan"), adminEnv(), nil, nil)
	if _, err := f.fill([]string{"--source", "s", "--token", "t"}); err != nil {
		t.Errorf("the local --token of migrate plan is allowed: %v", err)
	}
	f = newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	if _, err := f.fill([]string{"a", "b"}); err == nil {
		t.Error("too many arguments")
	}
}

func TestChildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "MP_SERVER=https://stale", "MP_TOKEN=stale", "MP_LANG=ru", "MONOPANEL_CONFIG=/x.yaml", "HOME=/root"}
	env := childEnv(base, Options{Server: "https://panel:8443", Token: "tok", Insecure: true})
	for _, want := range []string{"PATH=/bin", "HOME=/root", "MP_SERVER=https://panel:8443", "MP_TOKEN=tok", "MP_INSECURE=1", "MP_LANG=en", "MONOPANEL_CONFIG=/x.yaml"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
	for _, bad := range []string{"MP_SERVER=https://stale", "MP_TOKEN=stale", "MP_LANG=ru"} {
		if slices.Contains(env, bad) {
			t.Errorf("env keeps %s", bad)
		}
	}
	env = childEnv(base, Options{Config: "/etc/other.yaml"})
	if !slices.Contains(env, "MONOPANEL_CONFIG=/etc/other.yaml") || slices.Contains(env, "MONOPANEL_CONFIG=/x.yaml") || slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "MP_SERVER=") }) {
		t.Errorf("local env: %q", env)
	}
}

func TestBoundCommands(t *testing.T) {
	root := testTree()
	admin := BoundCommands(root, Options{}, "admin")
	// Every runnable command a menu may offer is reachable from the menus.
	for _, c := range runnables(root, Options{}) {
		if c == root {
			continue
		}
		if p := cmdPath(c); !slices.Contains(admin, p) {
			t.Errorf("%s is not reachable from the menus", p)
		}
	}
	for _, p := range []string{"extra run", "db config", "db config set", "update", "update trust", "config set", "selinux"} {
		if !slices.Contains(admin, p) {
			t.Errorf("BoundCommands(admin) lacks %q", p)
		}
	}
	for _, p := range []string{"api", "setup", "version"} {
		if slices.Contains(admin, p) {
			t.Errorf("BoundCommands has the CLI-only %q", p)
		}
	}
	remote := BoundCommands(root, Options{Server: "https://x"}, "admin")
	if slices.Contains(remote, "update trust") {
		t.Error("remote: update trust is local only")
	}
	if !slices.Contains(remote, "config set") {
		t.Error("remote: config set goes through the API")
	}
	user := BoundCommands(root, Options{}, "user")
	for _, p := range []string{"site add", "cron add", "files ls", "extra run"} {
		if !slices.Contains(user, p) {
			t.Errorf("BoundCommands(user) lacks %q", p)
		}
	}
	for _, p := range []string{"user add", "user rm", "site move-ip", "firewall apply", "migrate plan", "update settings", "config set", "selinux"} {
		if slices.Contains(user, p) {
			t.Errorf("BoundCommands(user) has the administrator's %q", p)
		}
	}
}

func TestMissingCommandsAreSkipped(t *testing.T) {
	root := testTree()
	if findCmd(root, "totp setup") != nil {
		t.Fatal("the test tree has no totp")
	}
	m, _ := newTestModel(t, "admin", nil)
	h := totpScreen(m).(*hubScreen)
	h.id = 99
	if items := h.items(m); len(items) != 0 {
		t.Errorf("entries for commands that do not exist: %d", len(items))
	}
}

// TestNewerCommandsFindTheirPlace: mp whoami and mp service
// reload-or-restart come with a newer mp. The menus place them (Settings,
// Services) without knowing more, and an mp without them loses nothing.
func TestNewerCommandsFindTheirPlace(t *testing.T) {
	root := newerTree()
	for _, role := range []string{"admin", "user"} {
		if !slices.Contains(BoundCommands(root, Options{}, role), "whoami") {
			t.Errorf("%s: whoami is not reachable from the menus", role)
		}
	}
	if !slices.Contains(BoundCommands(root, Options{}, "admin"), "service reload-or-restart") {
		t.Error("service reload-or-restart is not reachable")
	}
	areas := map[string]*area{}
	for _, a := range areaDefs(true) {
		areas[a.id] = a
	}
	has := func(list []*cobra.Command, path string) bool {
		return slices.ContainsFunc(list, func(c *cobra.Command) bool { return cmdPath(c) == path })
	}
	if !has(commandsOf(root, Options{}, areas["settings"], false), "whoami") {
		t.Error("whoami belongs to Settings (for ordinary accounts too)")
	}
	if has(otherCommands(root, Options{}), "whoami") {
		t.Error("whoami must not land in Other")
	}
	if !has(commandsOf(root, Options{}, areas["services"], true), "service reload-or-restart") {
		t.Error("reload-or-restart belongs to Services")
	}

	routes := apiRoutes()
	routes["/api/v1/services"] = []map[string]any{
		{"unit": "nginx.service", "load_state": "loaded", "active_state": "active", "sub_state": "running", "unit_file_state": "enabled"},
		{"unit": "monopanel-api.service", "load_state": "loaded", "active_state": "active", "sub_state": "running", "unit_file_state": "enabled"},
	}
	m, fr := newTestModelOf(t, root, "user", routes)
	openArea(t, m, "Settings")
	press(m, "i")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"whoami"}) {
		t.Fatalf("i in Settings runs mp whoami: %+v", jobs)
	}

	// The unit's page offers it; for the panel's own API it asks first.
	m, fr = newTestModelOf(t, root, "admin", routes)
	openArea(t, m, "Services")
	s := m.top().(*listScreen)
	s.cursor = 1 // monopanel-api.service
	press(m, "enter")
	h, ok := m.top().(*hubScreen)
	if !ok {
		t.Fatalf("the unit's page: %T", m.top())
	}
	found := false
	for i, e := range h.items(m) {
		if e.act != nil && e.act.path == "service reload-or-restart" {
			h.cursor, found = i, true
		}
	}
	if !found {
		t.Fatal("the unit's page lacks reload-or-restart")
	}
	press(m, "enter")
	if ov, ok := m.overlay.(*confirmOverlay); !ok || !strings.Contains(ov.spec.note, "API restarts") {
		t.Fatalf("reload-or-restart of the panel's API asks: %T", m.overlay)
	}
	press(m, "y")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"service", "reload-or-restart", "monopanel-api.service"}) {
		t.Errorf("jobs %+v", jobs)
	}

	// Without them: nothing to show, nothing breaks.
	m, fr = newTestModelOf(t, testTree(), "admin", routes)
	openArea(t, m, "Settings")
	for _, e := range m.top().(*hubScreen).items(m) {
		if e.key == "i" {
			t.Error("whoami is offered by an mp that has no such command")
		}
	}
	press(m, "i")
	openArea(t, m, "Services")
	press(m, "enter")
	for _, e := range m.top().(*hubScreen).items(m) {
		if e.act != nil && e.act.path == "service reload-or-restart" {
			t.Error("reload-or-restart is offered by an mp that has no such command")
		}
	}
	if len(fr.started()) != 0 || m.flashText != "" {
		t.Errorf("a missing command must neither run nor complain: %+v %q", fr.started(), m.flashText)
	}
}

func TestFormKeys(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "a") // site add: an empty form
	f := m.top().(*form)
	if f.focus != 0 {
		t.Fatalf("the focus starts on the empty <domain>: %d", f.focus)
	}
	typeText(m, "new.test")
	press(m, "tab", "tab", "tab") // → --mode
	if fd := f.fields[f.focus]; fd.name != "mode" {
		t.Fatalf("focus on %s", fd.name)
	}
	press(m, "right")
	if fd := fieldByName(f, "mode"); fd.value() != "apache" {
		t.Errorf("→ cycles the choices: %q", fd.value())
	}
	typeText(m, "p") // a letter jumps to the choice
	if fd := fieldByName(f, "mode"); fd.value() != "proxy" {
		t.Errorf("a letter picks the choice: %q", fd.value())
	}
	press(m, "tab", "space") // --www
	if fieldByName(f, "www").value() != "true" {
		t.Error("space toggles a switch")
	}
	press(m, "tab")
	typeText(m, "q?r:") // global keys are text in a form
	if fieldByName(f, "alias").value() != "q?r:" || m.overlay != nil || m.top() != f {
		t.Errorf("a form takes every key: %q", fieldByName(f, "alias").value())
	}
	f.focus = len(f.fields) // the run button
	m.Update(tea.PasteMsg{Content: "pasted"})
	press(m, "ctrl+p")
	if !strings.Contains(f.line(), "--mode proxy --www") {
		t.Errorf("line %q", f.line())
	}
	press(m, "esc")
	if _, ok := m.top().(*listScreen); !ok {
		t.Error("esc cancels the form")
	}
}

func TestEditNeedsAChange(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "s") // the site's settings, unchanged
	f := m.top().(*form)
	press(m, "ctrl+s")
	if len(fr.started()) != 0 || !strings.Contains(f.err, "nothing changed") {
		t.Errorf("an unchanged edit form must not run: %q %v", f.err, fr.started())
	}
	fieldByName(f, "php").setValue("8.3")
	press(m, "ctrl+s")
	if len(fr.started()) != 1 {
		t.Error("a changed one runs")
	}
}

func TestTemplate(t *testing.T) {
	root := testTree()
	a := &act{path: "cron add", flags: map[string]string{"user": "login", "disabled": "=true"}}
	if got := a.template(findCmd(root, "cron add"), map[string]string{"login": "bob"}); got != "mp cron add --disabled --user bob" {
		t.Errorf("template %q", got)
	}
	a = &act{path: "mail alias add", args: []string{"address"}}
	if got := a.template(findCmd(root, "mail alias add"), map[string]string{"address": "info@example.com"}); got != "mp mail alias add info@example.com <dest>" {
		t.Errorf("template %q", got)
	}
}

func TestListValuesKeepCommas(t *testing.T) {
	if got := splitList(`"LIST=a,b", A=1,, "Q=say ""hi"""`); !slices.Equal(got, []string{"LIST=a,b", "A=1", `Q=say "hi"`}) {
		t.Errorf("splitList %q", got)
	}
	if got := splitList("a, b,,c"); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("plain list %q", got)
	}
	if got := splitList(`A="x"`); !slices.Equal(got, []string{`A="x"`}) {
		t.Errorf("a quote inside a value %q", got)
	}

	// The app's variables come back unchanged, and a changed list reaches
	// the command as the same items: pflag reads a slice flag as CSV.
	root := testTree()
	env := []string{"LIST=a,b", "A=1"}
	row := map[string]string{"name": "bot", "env": joinList(env)}
	f := newForm(findCmd(root, "app set"), adminEnv(), &act{path: "app set", args: []string{"name"}, prefill: same("env")}, row)
	if argv, _, _, _ := f.build(); !slices.Equal(argv, []string{"app", "set", "bot"}) {
		t.Fatalf("an unchanged list is not passed: %q", argv)
	}
	fd := fieldByName(f, "env")
	fd.setValue(fd.value() + `, B=2`)
	argv, _, _, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	var flagArg string
	for _, a := range argv {
		if strings.HasPrefix(a, "--env=") {
			flagArg = a
		}
	}
	fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
	got := fs.StringSlice("env", nil, "")
	if err := fs.Parse([]string{flagArg}); err != nil {
		t.Fatalf("%q: %v", flagArg, err)
	}
	if !slices.Equal(*got, []string{"LIST=a,b", "A=1", "B=2"}) {
		t.Errorf("%q reads back as %q", flagArg, *got)
	}
}

func TestStdinDashRefused(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "site nginx"), adminEnv(), nil, nil)
	f.fields[0].setValue("example.com")
	fieldByName(f, "set").setValue("-")
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Errorf("--set - from a form would write empty directives: %v", err)
	}
	fieldByName(f, "set").setValue("/tmp/custom.conf")
	if _, _, _, err := f.build(); err != nil {
		t.Errorf("a file is fine: %v", err)
	}
}

func TestValuesTrimmed(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "user rm"), adminEnv(), nil, nil)
	f.fields[0].setValue(" bob ")
	argv, _, _, err := f.build()
	if err != nil || !slices.Equal(argv, []string{"user", "rm", "bob"}) {
		t.Errorf("argv %q err %v", argv, err)
	}
	if c := confirmFor(f); c == nil || c.word != "bob" {
		t.Errorf("the name to type is the argument as passed: %+v", c)
	}
	f = newForm(findCmd(root, "site add"), adminEnv(), nil, nil)
	f.fields[0].setValue("example.com")
	fieldByName(f, "php").setValue(" 8.4 ")
	if argv, _, _, _ := f.build(); !slices.Contains(argv, "--php=8.4") {
		t.Errorf("a text flag is trimmed: %q", argv)
	}
	f = newForm(findCmd(root, "user add"), adminEnv(), nil, nil)
	f.fields[0].setValue("bob")
	fieldByName(f, "password").setValue(" pass with spaces ")
	if _, _, stdin, _ := f.build(); stdin != " pass with spaces " {
		t.Errorf("a secret is passed as typed: %q", stdin)
	}
}

func TestVariadicRowValueStaysOneArgument(t *testing.T) {
	root := testTree()
	a := &act{path: "db config set", args: []string{"kv"}}
	f := newForm(findCmd(root, "db config set"), adminEnv(), a, map[string]string{"kv": "sql_mode=A & B"})
	argv, _, _, err := f.build()
	if err != nil || !slices.Equal(argv, []string{"db", "config", "set", "sql_mode=A & B"}) {
		t.Errorf("argv %q err %v", argv, err)
	}
	f = newForm(findCmd(root, "db config set"), adminEnv(), a, map[string]string{"kv": "memory_limit=256M"})
	if fd := f.fields[0]; fd.value() != "memory_limit=256M" {
		t.Errorf("a plain value is not quoted: %q", fd.value())
	}
}

func TestBackupCredentialsMasked(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "backup target add"), adminEnv(), nil, nil)
	f.fields[0].setValue("s3")
	fieldByName(f, "env").setValue("AWS_SECRET_ACCESS_KEY=topsecret")
	if fd := fieldByName(f, "env"); !fd.secret {
		t.Error("the repository's variables are credentials")
	}
	if strings.Contains(f.line(), "topsecret") {
		t.Errorf("line %q", f.line())
	}
	if !strings.Contains(f.stdinNote(), "stdin") {
		t.Errorf("a piped list: %q", f.stdinNote())
	}
	// No stdin form of the flag: the form says the secret shows in ps.
	f = newForm(findCmd(root, "migrate plan"), adminEnv(), nil, nil)
	fieldByName(f, "token").setValue("tok-1")
	if !strings.Contains(f.stdinNote(), "--token") || !strings.Contains(f.stdinNote(), "ps") {
		t.Errorf("note %q", f.stdinNote())
	}
	f = newForm(findCmd(root, "user add"), adminEnv(), nil, nil)
	fieldByName(f, "password").setValue("x")
	if !strings.Contains(f.stdinNote(), "stdin") {
		t.Errorf("a piped secret: %q", f.stdinNote())
	}
}

func TestConfirmMore(t *testing.T) {
	root := testTree()
	form := func(path string, set map[string]string, args ...string) *form {
		f := newForm(findCmd(root, path), adminEnv(), nil, nil)
		i := 0
		for _, fd := range f.fields {
			if fd.arg && i < len(args) {
				fd.setValue(args[i])
				i++
			}
		}
		for k, v := range set {
			fieldByName(f, k).setValue(v)
		}
		return f
	}
	if c := confirmFor(form("ssl renew", nil, "7")); c == nil || !strings.Contains(c.note, "Let's Encrypt") {
		t.Errorf("a renewal asks, like the web UI: %+v", c)
	}
	if c := confirmFor(form("update trust", nil)); c != nil {
		t.Error("showing the key asks nothing")
	}
	for _, flag := range []string{"off", "clear"} {
		if c := confirmFor(form("update trust", map[string]string{flag: "true"})); c == nil || c.note == "" {
			t.Errorf("update trust --%s changes the trust anchor: %+v", flag, c)
		}
	}
	if c := confirmFor(form("update trust", map[string]string{"key": "AAAA"})); c == nil {
		t.Error("update trust --key changes the trust anchor")
	}
	if c := confirmFor(form("app set", map[string]string{"disable": "true"}, "bot")); c == nil {
		t.Error("app set --disable stops the service, like app stop")
	}
	if c := confirmFor(form("app set", map[string]string{"enable": "true"}, "bot")); c != nil {
		t.Error("app set --enable asks nothing")
	}
	if c := confirmFor(form("user set", map[string]string{"status": "suspended"}, "bob")); c == nil || c.note == "" {
		t.Error("suspending an account asks, like the web UI")
	}
	// The web UI asks both ways.
	if c := confirmFor(form("user set", map[string]string{"status": "active"}, "bob")); c == nil || c.note == "" {
		t.Error("reactivating an account asks, like the web UI")
	}
	if c := confirmFor(form("user set", map[string]string{"email": "bob@example.com"}, "bob")); c != nil {
		t.Errorf("a new e-mail asks nothing: %+v", c)
	}
	if c := confirmFor(form("user set", map[string]string{"shell": "true"}, "bob")); c == nil || !strings.Contains(c.note, "shell") {
		t.Errorf("an SSH shell asks: %+v", c)
	}
}
