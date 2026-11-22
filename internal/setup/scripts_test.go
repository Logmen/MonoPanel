package setup

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/config"
)

// packageScript is a script of the package, as text.
func packageScript(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The package's scripts as text: the socket is enabled with the services on
// a fresh install and follows web.listen, never failing the install. On an
// upgrade a socket that listens keeps the port while the services restart;
// one that does not (a panel of 0.8.19 or older listens on the port itself)
// or whose address moved restarts in the same systemctl call as a running
// API — systemd orders it between the old API's exit and the new one's
// start — and never after a stop of the API of its own, which would leave
// the port free for as long as two systemctl calls take. Removal takes the
// socket too.
func TestPackageScriptsOrderTheSocket(t *testing.T) {
	post := packageScript(t, "postinstall.sh")
	at := func(s string) int {
		t.Helper()
		i := strings.Index(post, s)
		if i < 0 {
			t.Fatalf("postinstall.sh lacks %q", s)
		}
		return i
	}
	upgrade := at(`[ "${1:-}" = "2" ] && upgrade=1`)
	prune := at("/usr/bin/monopanel prune-unit-copies ||")
	reload := at("systemctl daemon-reload >/dev/null 2>&1 || true")
	enable := at("systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service")
	enableSocket := at("systemctl enable monopanel-api.socket >/dev/null 2>&1 || true")
	before := at(`before=$(cat "$dropin" 2>/dev/null || true)`)
	sync := at("/usr/bin/monopanel api-socket-sync ||")
	moved := at(`[ "$(cat "$dropin" 2>/dev/null || true)" = "$before" ] || moved=1`)
	restarts := at(`if [ "$upgrade" = "1" ]; then`)
	keep := at(`if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then`)
	tryRestart := at("systemctl try-restart monopanel-agent.service monopanel-api.service")
	running := at("elif systemctl -q is-active monopanel-api.service; then")
	together := at("systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true")
	stopped := at("systemctl try-restart monopanel-api.socket monopanel-agent.service >/dev/null 2>&1 || true")
	if order := []int{upgrade, prune, reload, enable, enableSocket, before, sync, moved, restarts, keep, tryRestart, running, together, stopped}; !slices.IsSorted(order) {
		t.Errorf("postinstall.sh: upgrade %d, prune %d, reload %d, enable %d, socket enabled %d, drop-in before %d, sync %d, moved %d, restarts %d, keep %d, try-restart %d, API running %d, socket with the API %d, API stopped %d",
			upgrade, prune, reload, enable, enableSocket, before, sync, moved, restarts, keep, tryRestart, running, together, stopped)
	}
	var acts []string
	for _, line := range strings.Split(post, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "systemctl") && strings.Contains(line, "monopanel-api.socket") {
			acts = append(acts, line)
		}
	}
	if want := []string{
		"systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true",
		"systemctl enable monopanel-api.socket >/dev/null 2>&1 || true",
		`if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then`,
		"systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true",
		"systemctl try-restart monopanel-api.socket monopanel-agent.service >/dev/null 2>&1 || true",
	}; !slices.Equal(acts, want) {
		t.Errorf("postinstall.sh acts on the socket outside the branch that restarts it with the API:\n%s", strings.Join(acts, "\n"))
	}
	for _, gap := range []string{"systemctl stop monopanel-api.service", "systemctl start monopanel-api.socket", "systemctl stop monopanel-api.socket", "systemctl restart monopanel-api.socket "} {
		if strings.Contains(post, gap) {
			t.Errorf("postinstall.sh runs %q", gap)
		}
	}
	pre := packageScript(t, "preremove.sh")
	if !strings.Contains(pre, "systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service monopanel-mailpolicy.service monopanel-mailpolicy-unix.socket monopanel-mailpolicy-inet.socket monopanel-mailguard.service") {
		t.Error("preremove.sh leaves the socket listening")
	}
	// Neither script may fail the transaction it runs in.
	for name, script := range map[string]string{"preremove.sh": pre, "postremove.sh": packageScript(t, "postremove.sh")} {
		for _, line := range strings.Split(script, "\n") {
			if l := strings.TrimSpace(line); l == "set -e" || strings.HasPrefix(l, "set -e ") {
				t.Errorf("%s runs with set -e: any failing command would fail the transaction", name)
			}
		}
		if !strings.HasSuffix(script, "\nexit 0\n") {
			t.Errorf("%s does not end with exit 0", name)
		}
	}
	// The paths the preremove looks at are the panel's.
	for _, line := range []string{"\nunits=" + systemUnitDir + "\n", "\npackaged=" + packageUnitDir + "\n", "\nbin=" + packageBinary + "\n",
		"\naccounts=" + strings.TrimSuffix(confcheck.ApacheInstanceConf("%"), "/%/httpd.conf") + "\n",
		"\nshares=" + filepath.Join(config.Default().WWWRoot, ".shares") + "\n"} {
		if !strings.Contains(pre, line) {
			t.Errorf("preremove.sh lacks %q", line)
		}
	}
	if !strings.Contains(pre, `for f in "$accounts"/*/httpd.conf; do`) || confcheck.ApacheInstanceConf("x") != strings.TrimSuffix(confcheck.ApacheInstanceConf("%"), "/%/httpd.conf")+"/x/httpd.conf" {
		t.Error("preremove.sh looks for the accounts' Apaches elsewhere than their configuration is")
	}
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "templates", "systemd", "app.service.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tmpl), "\nExecStart={{ .Binary }} app-exec --env-file ") || !strings.Contains(string(tmpl), "\nDescription=MonoPanel app {{ .Login }}/{{ .Name }}") ||
		!strings.Contains(pre, `grep -qF "ExecStart=$bin app-exec " "$f"`) {
		t.Error("preremove.sh does not tell the apps mp app-exec starts the way their unit names it")
	}
}

// stubs are systemctl, dpkg and the panel's binary for the package's scripts
// in a test: each logs its arguments to $STUB_LOG and answers as the
// environment says — $ACTIVE and $ENABLED list the units that are, $OLDER the
// versions dpkg takes for older than the one asked, $PROBE is the panel
// binary's answer to the probe, $MOVE=1 makes api-socket-sync write $DROPIN.
var stubs = map[string]string{
	"systemctl": `#!/bin/sh
echo "systemctl $*" >> "$STUB_LOG"
[ "$1" = "-q" ] && shift
case "$1" in
is-active) case " $ACTIVE " in *" $2 "*) exit 0 ;; esac; exit 3 ;;
is-enabled) case " $ENABLED " in *" $2 "*) exit 0 ;; esac; exit 1 ;;
esac
exit 0
`,
	"postfix": `#!/bin/sh
echo "postfix $*" >> "$STUB_LOG"
exit 0
`,
	"dpkg": `#!/bin/sh
echo "dpkg $*" >> "$STUB_LOG"
[ "$1" = "--compare-versions" ] && [ "$3" = "lt" ] || exit 2
case " $OLDER " in *" $2 "*) exit 0 ;; esac
exit 1
`,
	"monopanel": `#!/bin/sh
echo "monopanel $*" >> "$STUB_LOG"
case "$1" in
mail-policy) exit "${MAILPOLICY:-0}" ;;
api-socket-sync)
	[ "$2" = "--probe" ] && exit "${PROBE:-0}"
	[ "${MOVE:-0}" = 1 ] && printf '[Socket]\nListenStream=moved\n' > "$DROPIN"
	exit 0 ;;
prune-unit-copies) exit "${PRUNE:-0}" ;;
esac
exit 0
`,
}

// tmpfilesDir holds mp setup's tmpfiles copy of the run directory
// (tmpfilesConf) and the API's monopanel-php.conf of 0.8.19, which the
// package's scripts look after.
const tmpfilesDir = "/etc/tmpfiles.d"

// removalMark is where the preremove marks that a removal turned the panel
// off, for the postinstall of a later install over what it left: in the
// agent's own directory, which only root writes — the API must not make an
// upgrade enable units the administrator disabled.
var removalMark = filepath.Join(config.DefaultAgentStateDir, "package-removed")

// scriptRun runs a package script, its paths pointed into a temporary root
// and the commands it calls replaced by stubs.
type scriptRun struct {
	t              *testing.T
	root, bin, log string
	env            []string
	// Where the script's paths point.
	units, packaged, accounts, shares, dropin, config, removed, tmpfiles, maincf string
	// What the last run printed and called.
	stdout, stderr string
	calls          []string
}

func newScriptRun(t *testing.T) *scriptRun {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	r := &scriptRun{t: t, root: root, log: filepath.Join(root, "calls.log"),
		units: filepath.Join(root, "etc/systemd/system"), packaged: filepath.Join(root, "usr/lib/systemd/system"),
		accounts: filepath.Join(root, "etc/apache2/monopanel/accounts"), shares: filepath.Join(root, "var/www/.shares"),
		config: filepath.Join(root, "etc/monopanel/config.yaml"), removed: filepath.Join(root, "var/lib/monopanel-agent/package-removed"),
		tmpfiles: filepath.Join(root, "etc/tmpfiles.d"), maincf: filepath.Join(root, "etc/postfix/main.cf")}
	r.dropin = filepath.Join(r.units, "monopanel-api.socket.d", "listen.conf")
	stubDir := filepath.Join(root, "stubs")
	for _, d := range []string{stubDir, r.units, r.packaged, filepath.Dir(r.dropin), filepath.Dir(r.config), filepath.Dir(r.removed), r.tmpfiles, filepath.Dir(r.maincf)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.bin = filepath.Join(stubDir, "monopanel")
	r.env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"), "STUB_LOG="+r.log, "DROPIN="+r.dropin)
	return r
}

// run runs script (its text) with args and the environment env on top.
func (r *scriptRun) run(script string, env []string, args ...string) {
	r.t.Helper()
	path := filepath.Join(r.root, "script.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
	_ = os.Remove(r.log)
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Env = append(append([]string{}, r.env...), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("%s %v: %v\n%s", filepath.Base(path), args, err, errOut.String())
	}
	r.stdout, r.stderr = out.String(), errOut.String()
	b, _ := os.ReadFile(r.log)
	r.calls = nil
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			r.calls = append(r.calls, l)
		}
	}
}

// called says whether a stub was called exactly so.
func (r *scriptRun) called(call string) bool { return slices.Contains(r.calls, call) }

// preremove is preremove.sh with its paths in the temporary root.
func (r *scriptRun) preremove() string {
	r.t.Helper()
	s := packageScript(r.t, "preremove.sh")
	for from, to := range map[string]string{
		"\nunits=" + systemUnitDir + "\n":              "\nunits=" + r.units + "\n",
		"\npackaged=" + packageUnitDir + "\n":          "\npackaged=" + r.packaged + "\n",
		"\nbin=" + packageBinary + "\n":                "\nbin=" + r.bin + "\n",
		"\nremoved=" + removalMark + "\n":              "\nremoved=" + r.removed + "\n",
		"\naccounts=/etc/apache2/monopanel/accounts\n": "\naccounts=" + r.accounts + "\n",
		"\nshares=/var/www/.shares\n":                  "\nshares=" + r.shares + "\n",
		"\ntmpfiles=" + tmpfilesDir + "\n":             "\ntmpfiles=" + r.tmpfiles + "\n",
		"\nmaincf=/etc/postfix/main.cf\n":              "\nmaincf=" + r.maincf + "\n",
	} {
		if !strings.Contains(s, from) {
			r.t.Fatalf("preremove.sh lacks %q", from)
		}
		s = strings.Replace(s, from, to, 1)
	}
	r.noSystemPaths("preremove.sh", s)
	return s
}

// postremove is postremove.sh with its paths in the temporary root.
func (r *scriptRun) postremove() string {
	r.t.Helper()
	s := packageScript(r.t, "postremove.sh")
	for from, to := range map[string]string{
		"\nbin=" + packageBinary + "\n":   "\nbin=" + r.bin + "\n",
		"\nmaincf=/etc/postfix/main.cf\n": "\nmaincf=" + r.maincf + "\n",
	} {
		if !strings.Contains(s, from) {
			r.t.Fatalf("postremove.sh lacks %q", from)
		}
		s = strings.Replace(s, from, to, 1)
	}
	r.noSystemPaths("postremove.sh", s)
	return s
}

// postinstall is the part of postinstall.sh that runs as anybody — from the
// upgrade's detection on — with its paths in the temporary root.
func (r *scriptRun) postinstall() string {
	r.t.Helper()
	s := packageScript(r.t, "postinstall.sh")
	i := strings.Index(s, "\nupgrade=0\n")
	if i < 0 {
		r.t.Fatal("postinstall.sh: no upgrade=0")
	}
	s = "#!/bin/sh\nset -e\n" + s[i:]
	for from, to := range map[string]string{
		"/usr/bin/monopanel ": r.bin + " ",
		"/etc/systemd/system/monopanel-api.socket.d/listen.conf": r.dropin,
		"/etc/monopanel/config.yaml":                             r.config,
		"\nremoved=" + removalMark + "\n":                        "\nremoved=" + r.removed + "\n",
	} {
		if !strings.Contains(s, from) {
			r.t.Fatalf("postinstall.sh lacks %q", from)
		}
		s = strings.ReplaceAll(s, from, to)
	}
	r.noSystemPaths("postinstall.sh", s)
	return s
}

// postinstallTmpfiles is the part of postinstall.sh that brings the run
// directory's boot-time files up to date, with its paths in the temporary
// root.
func (r *scriptRun) postinstallTmpfiles() string {
	r.t.Helper()
	s := packageScript(r.t, "postinstall.sh")
	from, to := "\ntmpfiles="+tmpfilesDir+"\n", "\nsystemd-tmpfiles --create "
	i, j := strings.Index(s, from), strings.Index(s, to)
	if i < 0 || j < i {
		r.t.Fatalf("postinstall.sh: no %q before %q", from, to)
	}
	s = "#!/bin/sh\nset -e\ntmpfiles=" + r.tmpfiles + "\n" + s[i+len(from):j+1]
	r.noSystemPaths("postinstall.sh", s)
	return s
}

// noSystemPaths makes sure the script about to run touches nothing of the
// host's: outside comments and the messages it echoes, every path below
// /etc, /usr or /var it names is in the temporary root — which may itself
// lie below /var (TMPDIR=/var/tmp), so its own paths are taken out first.
func (r *scriptRun) noSystemPaths(name, s string) {
	r.t.Helper()
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if i := strings.Index(l, "echo "); i >= 0 {
			l = l[:i]
		}
		if strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.ReplaceAll(l, r.root+"/", "ROOT:")
		for _, dir := range []string{"/etc/", "/usr/", "/var/"} {
			if strings.Contains(l, dir) {
				r.t.Fatalf("%s would touch the host: %s", name, line)
			}
		}
	}
}

// The postinstall on a fresh install enables the panel and starts nothing;
// on an upgrade it leaves the units the administrator enabled or disabled
// as they are — but for the socket of the panel's port the first time,
// which follows the API — and restarts only what ran. A panel that was
// stopped stays stopped: its socket is not started either, and would start
// the API at the first connection. The copies of the units that shadow the
// package's are pruned first, and a failed prune is no failed install.
func TestPostinstallKeepsAStoppedPanelStopped(t *testing.T) {
	const (
		agent = "monopanel-agent.service"
		api   = "monopanel-api.service"
		sock  = "monopanel-api.socket"
	)
	const installed, updated = "MonoPanel installed. Run: mp setup\n", "MonoPanel updated.\n"
	for _, c := range []struct {
		what      string
		args      []string
		env       []string
		dropin    bool // the socket's drop-in is there already
		removed   bool // a removal left its mark (preremove.sh)
		want, not []string
		says      string
	}{
		{what: "a fresh install", args: []string{"configure", ""},
			want: []string{"monopanel prune-unit-copies", "systemctl daemon-reload", "systemctl enable " + agent + " " + sock + " " + api},
			not:  []string{"systemctl try-restart " + agent + " " + api, "systemctl restart " + agent + " " + sock + " " + api}, says: installed},
		{what: "an rpm install", args: []string{"1"},
			want: []string{"systemctl enable " + agent + " " + sock + " " + api}, says: installed},
		// dpkg names the version a removal left behind (its conffiles) as it
		// names the one an upgrade replaces; the removal disabled the units.
		{what: "dpkg, over what a removal left", args: []string{"configure", "0.9.0"}, removed: true, dropin: true,
			want: []string{"systemctl enable " + agent + " " + sock + " " + api, "systemctl try-restart " + sock + " " + agent},
			not:  []string{"systemctl restart " + agent + " " + sock + " " + api}, says: installed},
		{what: "rpm, after an erase", args: []string{"1"}, removed: true, dropin: true,
			want: []string{"systemctl enable " + agent + " " + sock + " " + api}, says: installed},
		{what: "from 0.8.19, running", args: []string{"configure", "0.8.19"}, env: []string{"ACTIVE=" + api + " " + agent, "ENABLED=" + api, "MOVE=1"},
			want: []string{"monopanel prune-unit-copies", "systemctl enable " + sock, "monopanel api-socket-sync", "systemctl restart " + agent + " " + sock + " " + api},
			not:  []string{"systemctl enable " + agent + " " + sock + " " + api}},
		{what: "from 0.8.19, disabled and stopped", args: []string{"configure", "0.8.19"}, env: []string{"MOVE=1"},
			want: []string{"systemctl try-restart " + sock + " " + agent},
			not:  []string{"systemctl enable " + sock, "systemctl enable " + agent + " " + sock + " " + api, "systemctl restart " + agent + " " + sock + " " + api}, says: updated},
		{what: "stopped with its socket", args: []string{"2"}, env: []string{"ENABLED=" + api}, dropin: true,
			want: []string{"systemctl try-restart " + sock + " " + agent},
			not:  []string{"systemctl enable " + sock, "systemctl restart " + agent + " " + sock + " " + api}},
		{what: "stopped, its socket on a moved address", args: []string{"2"}, env: []string{"ACTIVE=" + sock, "ENABLED=" + api + " " + sock, "MOVE=1"}, dropin: true,
			want: []string{"systemctl try-restart " + sock + " " + agent}},
		{what: "running on its socket", args: []string{"configure", "0.9.0"}, env: []string{"ACTIVE=" + sock + " " + api + " " + agent, "ENABLED=" + api + " " + sock}, dropin: true,
			want: []string{"systemctl try-restart " + agent + " " + api},
			not:  []string{"systemctl enable " + sock, "systemctl try-restart " + sock + " " + agent}},
		{what: "a prune that fails", args: []string{"2"}, env: []string{"PRUNE=1", "ACTIVE=" + sock + " " + api, "ENABLED=" + api}, dropin: true,
			want: []string{"monopanel prune-unit-copies", "systemctl try-restart " + agent + " " + api}},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := newScriptRun(t)
			if err := os.WriteFile(r.config, []byte("web:\n  listen: \":8443\"\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if c.dropin {
				if err := os.WriteFile(r.dropin, []byte("[Socket]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.removed {
				if err := os.WriteFile(r.removed, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r.run(r.postinstall(), c.env, c.args...)
			if c.says != "" && r.stdout != c.says {
				t.Errorf("says %q, want %q", r.stdout, c.says)
			}
			if _, err := os.Lstat(r.removed); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the removal's mark is still there: %v", err)
			}
			for _, w := range c.want {
				if !r.called(w) {
					t.Errorf("no %q in\n%s", w, strings.Join(r.calls, "\n"))
				}
			}
			for _, n := range c.not {
				if r.called(n) {
					t.Errorf("%q in\n%s", n, strings.Join(r.calls, "\n"))
				}
			}
			for _, call := range r.calls {
				if strings.HasPrefix(call, "systemctl start ") || strings.HasPrefix(call, "systemctl stop ") || call == "systemctl restart "+sock {
					t.Errorf("postinstall ran %q", call)
				}
			}
		})
	}
}

// A downgrade below 0.9.0 is no failure of the package's transaction: the
// preremove of 0.9.0 and later keeps the accounts' Apaches (their unit
// template) and the php-fpm socket directory at boot (monopanel-php.conf),
// and says what the older version breaks and how to mend it. dpkg names the
// version, rpm's is asked whether it takes the probe. An upgrade and a
// downgrade within 0.9.0 and later do none of it.
func TestPreremoveOnADowngrade(t *testing.T) {
	const tmpl = "monopanel-apache@.service"
	setupHost := func(r *scriptRun, apache bool) {
		t.Helper()
		writeFiles(t, r.packaged, map[string]string{tmpl: apacheUnit})
		writeFiles(t, r.units, map[string]string{
			"monopanel-app-alex-bot.service": "# Generated by MonoPanel for alex/bot — do not edit; use `mp app`.\n[Unit]\nDescription=MonoPanel app alex/bot — the shop's bot\n\n[Service]\nExecStart=" + r.bin + " app-exec --env-file /var/www/alex/data/.env -- /var/www/alex/data/bot\n",
			"monopanel-app-kate-web.service": "[Unit]\nDescription=MonoPanel app kate/web\n\n[Service]\nExecStart=/var/www/kate/data/web\n",
		})
		if apache {
			writeFiles(t, r.accounts, map[string]string{"alex/httpd.conf": "ServerRoot /etc/apache2\n"})
			writeFiles(t, r.shares, map[string]string{"kate/README": "a guest's folder is mounted next to this\n"})
		}
	}
	t.Run("dpkg to 0.8.19", func(t *testing.T) {
		r := newScriptRun(t)
		setupHost(r, true)
		r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
		if !r.called("dpkg --compare-versions 0.8.19 lt 0.9.0~") || !r.called("systemctl daemon-reload") || r.stdout != "" {
			t.Fatalf("%v, stdout %q", r.calls, r.stdout)
		}
		if b, err := os.ReadFile(filepath.Join(r.units, tmpl)); err != nil || string(b) != apacheUnit {
			t.Fatalf("the template was not kept: %v\n%s", err, b)
		}
		php := filepath.Join(r.tmpfiles, "monopanel-php.conf")
		if b, err := os.ReadFile(php); err != nil || string(b) != "d /run/monopanel/php 0755 root root -\n" {
			t.Fatalf("the php-fpm socket directory is not made at boot: %v %q", err, b)
		}
		for _, s := range []string{"(0.8.19) is older than 0.9.0", "/var/log/monopanel/sites", "monopanel-apache@<login>", "their unit template is in " + filepath.Join(r.units, tmpl) + ", so they keep running",
			"  - php-fpm makes the pools' sockets in /run/monopanel/php, which its mp setup leaves out of\n    " + filepath.Join(r.tmpfiles, "monopanel.conf") + ": " + php + " makes it at boot, keep that file;\n",
			"at their next start: alex/bot\n", "mp stack install apache", "mp site apply <domain>", "mp mail apply", "mp app set <name> --user <login>",
			"systemctl disable --now monopanel-apache@<login>", "/var/www/.shares", `"Going back to an older version": https://monopanel.app/en/docs/update/`} {
			if !strings.Contains(r.stderr, s) {
				t.Errorf("the notice lacks %q:\n%s", s, r.stderr)
			}
		}
		if strings.Contains(r.stderr, "kate/web") {
			t.Errorf("an app without an env file is named:\n%s", r.stderr)
		}
		for _, dir := range []string{r.units, r.tmpfiles} {
			if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		}
	})
	t.Run("rpm to 0.8.19", func(t *testing.T) {
		r := newScriptRun(t)
		setupHost(r, false)
		r.run(r.preremove(), []string{"PROBE=1"}, "1")
		if !r.called("monopanel api-socket-sync --probe") || !strings.Contains(r.stderr, "the version being installed is older than 0.9.0") {
			t.Fatalf("%v\n%s", r.calls, r.stderr)
		}
		// No Apache here (EL): no template kept, no word of it.
		if _, err := os.Lstat(filepath.Join(r.units, tmpl)); err == nil || strings.Contains(r.stderr, "monopanel-apache@") || strings.Contains(r.stderr, "mp stack install apache") {
			t.Fatalf("Apache on a host without the accounts' Apaches:\n%s", r.stderr)
		}
	})
	t.Run("the run directory of mp setup's copy", func(t *testing.T) {
		r := newScriptRun(t)
		setupHost(r, false)
		writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": tmpfilesConf(config.Config{RunDir: "/srv/run", ServiceGroup: "panel"})})
		r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
		if b, _ := os.ReadFile(filepath.Join(r.tmpfiles, "monopanel-php.conf")); string(b) != "d /srv/run/php 0755 root root -\n" || !strings.Contains(r.stderr, " sockets in /srv/run/php, which its mp setup leaves out of") || !strings.Contains(r.stderr, "monopanel-php.conf makes it at boot, keep that file;") {
			t.Fatalf("monopanel-php.conf: %q\n%s", b, r.stderr)
		}
	})
	t.Run("monopanel-php.conf of 0.8.19 there already", func(t *testing.T) {
		r := newScriptRun(t)
		setupHost(r, false)
		for _, text := range []string{"d /run/monopanel/php 0755 root root -\n", "# the administrator's\n"} {
			writeFiles(t, r.tmpfiles, map[string]string{"monopanel-php.conf": text})
			r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
			if b, _ := os.ReadFile(filepath.Join(r.tmpfiles, "monopanel-php.conf")); string(b) != text {
				t.Fatalf("the file there was rewritten: %q", b)
			}
		}
		// It does not hold the line: the notice says to put it there.
		if !strings.Contains(r.stderr, "monopanel.conf, and every php-fpm fails to start after a reboot: put\n") || !strings.Contains(r.stderr, `    "d /run/monopanel/php 0755 root root -" into `+filepath.Join(r.tmpfiles, "monopanel-php.conf")+";\n") {
			t.Fatalf("%s", r.stderr)
		}
	})
	t.Run("a template there already", func(t *testing.T) {
		r := newScriptRun(t)
		setupHost(r, true)
		writeFiles(t, r.units, map[string]string{tmpl: "# the administrator's\n"})
		r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
		if b, _ := os.ReadFile(filepath.Join(r.units, tmpl)); string(b) != "# the administrator's\n" || r.called("systemctl daemon-reload") {
			t.Fatalf("an existing unit was replaced: %q, %v", b, r.calls)
		}
	})
	t.Run("no way to keep the template", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a read-only directory")
		}
		r := newScriptRun(t)
		setupHost(r, true)
		if err := os.Chmod(r.units, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(r.units, 0o755) }) //nolint:errcheck // the temporary root goes anyway
		r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
		if !strings.Contains(r.stderr, "their unit template goes with this package") {
			t.Fatalf("%s", r.stderr)
		}
	})
	for _, c := range []struct {
		what string
		env  []string
		args []string
		want []string
	}{
		{"dpkg to 0.9.1", nil, []string{"upgrade", "0.9.1"}, []string{"dpkg --compare-versions 0.9.1 lt 0.9.0~"}},
		{"rpm to 0.9.1", nil, []string{"1"}, []string{"monopanel api-socket-sync --probe"}},
		{"a failed upgrade", []string{"OLDER=0.8.19"}, []string{"failed-upgrade", "0.9.0", "0.8.19"}, nil},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := newScriptRun(t)
			setupHost(r, true)
			r.run(r.preremove(), c.env, c.args...)
			if r.stderr != "" || r.stdout != "" || !slices.Equal(r.calls, c.want) {
				t.Fatalf("%v, stderr:\n%s", r.calls, r.stderr)
			}
			for _, p := range []string{filepath.Join(r.units, tmpl), filepath.Join(r.tmpfiles, "monopanel-php.conf"), r.removed} {
				if _, err := os.Lstat(p); err == nil {
					t.Fatalf("%s was made", p)
				}
			}
		})
	}
}

// The preremove's threshold as the real dpkg orders it (where there is one,
// as on CI's Ubuntu; the tests above have a stub answer for it), against the
// versions the panel's packages carry — nfpm turns git describe's
// 0.8.19-75-gf5801c6 into 0.8.19~75-gf5801c6: the previous release and this
// release's development builds are older; the release's own prereleases,
// the release and the builds after it are not.
func TestPreremoveThresholdAsDpkgOrdersIt(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("no dpkg")
	}
	_, rest, ok := strings.Cut(packageScript(t, "preremove.sh"), `dpkg --compare-versions "$2" lt `)
	threshold, _, _ := strings.Cut(rest, " ")
	if !ok || threshold == "" {
		t.Fatal("preremove.sh: no dpkg --compare-versions")
	}
	for v, older := range map[string]bool{
		"0.8.19": true, "0.8.19~75-gf5801c6": true, "0.8.19~dirty": true,
		"0.9.0~rc.1": false, "0.9.0": false, "0.9.0~dirty": false, "0.9.0~5-gabc1234": false, "0.9.0+rebuild": false, "0.9.1": false,
	} {
		err := exec.Command(dpkg, "--compare-versions", v, "lt", threshold).Run()
		var exit *exec.ExitError
		if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
			t.Fatalf("dpkg --compare-versions %s lt %s: %v", v, threshold, err)
		}
		if got := err == nil; got != older {
			t.Errorf("dpkg --compare-versions %s lt %s: older %v, want %v", v, threshold, got, older)
		}
	}
}

// A removal turns the panel off and leaves the mark of it; what the panel
// set up keeps running, and the preremove says what does not. The accounts'
// Apaches keep their unit template, as on a downgrade, and php/ of the run
// directory is made at boot, where the sites' pools listen: an older version
// installed next (dnf remove, then dnf install), whose mp setup writes the
// tmpfiles copy without that line, needs it, and so do the pools without a
// panel; where the line cannot be kept, the notice says where it goes. A
// host mp setup never set up runs no pools, and is left nothing. Apps that
// read an env file stop at their next start, and are named. A host with
// none of it hears nothing.
func TestPreremoveOnARemoval(t *testing.T) {
	const tmpl = "monopanel-apache@.service"
	const disable = "systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service monopanel-mailpolicy.service monopanel-mailpolicy-unix.socket monopanel-mailpolicy-inet.socket monopanel-mailguard.service"
	for _, c := range []struct {
		what string
		env  []string
		args []string
	}{
		{"dpkg", nil, []string{"remove"}},
		{"dpkg purge", nil, []string{"purge"}},
		{"rpm", []string{"PROBE=1"}, []string{"0"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := newScriptRun(t)
			writeFiles(t, r.packaged, map[string]string{tmpl: apacheUnit})
			writeFiles(t, r.accounts, map[string]string{"alex/httpd.conf": "ServerRoot /etc/apache2\n"})
			writeFiles(t, r.units, map[string]string{
				"monopanel-app-alex-bot.service": "[Unit]\nDescription=MonoPanel app alex/bot — the shop's bot\n\n[Service]\nExecStart=" + r.bin + " app-exec --env-file /var/www/alex/data/.env -- /var/www/alex/data/bot\n",
				"monopanel-app-kate-web.service": "[Unit]\nDescription=MonoPanel app kate/web\n\n[Service]\nExecStart=/var/www/kate/data/web\n",
			})
			writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": tmpfilesConf(config.Default())})
			r.run(r.preremove(), c.env, c.args...)
			if !slices.Equal(r.calls, []string{disable, "systemctl daemon-reload"}) || r.stdout != "" {
				t.Fatalf("%v, stdout %q", r.calls, r.stdout)
			}
			if _, err := os.Lstat(r.removed); err != nil {
				t.Fatalf("the removal's mark: %v", err)
			}
			if b, err := os.ReadFile(filepath.Join(r.units, tmpl)); err != nil || string(b) != apacheUnit {
				t.Fatalf("the template was not kept: %v\n%s", err, b)
			}
			// mp setup's line word for word: the two are silent together
			// at boot (phpTmpfilesLine).
			php := filepath.Join(r.tmpfiles, "monopanel-php.conf")
			if b, err := os.ReadFile(php); err != nil || string(b) != phpTmpfilesLine(config.Default()) {
				t.Fatalf("monopanel-php.conf: %v %q", err, b)
			}
			for _, s := range []string{"MonoPanel: the package goes; what the panel set up on this host keeps running, but:\n",
				"'monopanel app-exec', which goes with the\n    package, and fail at their next start: alex/bot\n", "installing the panel again brings them back",
				"their unit template is in " + filepath.Join(r.units, tmpl) + ", so they keep running", "systemctl disable --now monopanel-apache@<login> stops one for good",
				php + " makes /run/monopanel/php at boot, where the sites'\n    php-fpm pools listen.\n", `"Going back to an older version":`} {
				if !strings.Contains(r.stderr, s) {
					t.Errorf("the notice lacks %q:\n%s", s, r.stderr)
				}
			}
			if strings.Contains(r.stderr, "kate/web") || strings.Contains(r.stderr, "older than 0.9.0") || strings.Contains(r.stderr, "mp site apply") {
				t.Errorf("the removal speaks of a downgrade, or names an app without an env file:\n%s", r.stderr)
			}
		})
	}
	// postfix, told to keep mail waiting while the mail policy is down,
	// would keep it waiting for good once the policy's binary is gone:
	// main.cf loses the policy, opendkim stays — and postfix reloads before
	// the policy's units stop, so that no message waits in between (review
	// r2 F11).
	t.Run("the mail policy leaves main.cf", func(t *testing.T) {
		for _, c := range []struct{ before, after string }{
			{"smtpd_milters = { unix:/var/spool/postfix/private/monopanel-mailpolicy, default_action=tempfail }, local:/run/opendkim/opendkim.sock\nnon_smtpd_milters = { unix:/var/spool/postfix/private/monopanel-mailpolicy, default_action=tempfail }, local:/run/opendkim/opendkim.sock\n",
				"smtpd_milters = local:/run/opendkim/opendkim.sock\nnon_smtpd_milters = local:/run/opendkim/opendkim.sock\n"},
			{"smtpd_milters = { inet:127.0.0.1:8890, default_action=tempfail }, inet:127.0.0.1:8891\nnon_smtpd_milters = { inet:127.0.0.1:8890, default_action=tempfail }\n",
				"smtpd_milters = inet:127.0.0.1:8891\nnon_smtpd_milters = \n"},
		} {
			r := newScriptRun(t)
			writeFiles(t, filepath.Dir(r.maincf), map[string]string{"main.cf": c.before})
			r.run(r.preremove(), nil, "remove")
			if b, err := os.ReadFile(r.maincf); err != nil || string(b) != c.after {
				t.Errorf("main.cf after the removal: %v\n%s\nwant\n%s", err, b, c.after)
			}
			reload, stop := slices.Index(r.calls, "postfix reload"), slices.Index(r.calls, disable)
			if reload < 0 || stop < 0 || reload > stop {
				t.Errorf("postfix reload at %d, the units stopped at %d: %v", reload, stop, r.calls)
			}
		}
		r := newScriptRun(t)
		writeFiles(t, filepath.Dir(r.maincf), map[string]string{"main.cf": "smtpd_milters = local:/run/opendkim/opendkim.sock\n"})
		r.run(r.preremove(), []string{"OLDER=0.8.19"}, "upgrade", "0.8.19")
		if b, _ := os.ReadFile(r.maincf); string(b) != "smtpd_milters = local:/run/opendkim/opendkim.sock\n" || r.called("postfix reload") {
			t.Errorf("an upgrade touched main.cf: %s %v", b, r.calls)
		}
	})
	t.Run("a host with nothing to say", func(t *testing.T) {
		r := newScriptRun(t)
		writeFiles(t, r.tmpfiles, map[string]string{"monopanel-php.conf": "d /run/monopanel/php 0755 root root -\n"})
		r.run(r.preremove(), nil, "remove")
		if r.stderr != "" || r.stdout != "" || !slices.Equal(r.calls, []string{disable}) {
			t.Fatalf("%v, stderr:\n%s", r.calls, r.stderr)
		}
	})
	t.Run("php/ alone", func(t *testing.T) {
		r := newScriptRun(t)
		writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": tmpfilesConf(config.Default())})
		r.run(r.preremove(), nil, "remove")
		if !strings.Contains(r.stderr, "monopanel-php.conf makes /run/monopanel/php at boot") || strings.Contains(r.stderr, "Apache") || strings.Contains(r.stderr, "app-exec") {
			t.Fatalf("%s", r.stderr)
		}
	})
	// mp setup never ran: no run directory of the panel's, no pools — the
	// package goes without a word and leaves nothing in /etc/tmpfiles.d.
	t.Run("a host mp setup never set up", func(t *testing.T) {
		r := newScriptRun(t)
		r.run(r.preremove(), nil, "remove")
		if r.stderr != "" || r.stdout != "" || !slices.Equal(r.calls, []string{disable}) {
			t.Fatalf("%v, stderr:\n%s", r.calls, r.stderr)
		}
		if left, _ := filepath.Glob(filepath.Join(r.tmpfiles, "*")); len(left) != 0 {
			t.Fatalf("left in tmpfiles.d: %v", left)
		}
	})
	// The file is there without the panel's line, or cannot be written: the
	// notice says where the line goes, as on a downgrade.
	t.Run("monopanel-php.conf without the line", func(t *testing.T) {
		r := newScriptRun(t)
		writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": tmpfilesConf(config.Config{RunDir: "/srv/run", ServiceGroup: "panel"}), "monopanel-php.conf": "# the administrator's\n"})
		r.run(r.preremove(), nil, "remove")
		if b, _ := os.ReadFile(filepath.Join(r.tmpfiles, "monopanel-php.conf")); string(b) != "# the administrator's\n" {
			t.Fatalf("the file there was rewritten: %q", b)
		}
		if !strings.Contains(r.stderr, "MonoPanel: the package goes;") || !strings.Contains(r.stderr, `"d /srv/run/php 0755 root root -" into `+filepath.Join(r.tmpfiles, "monopanel-php.conf")) ||
			!strings.Contains(r.stderr, "listen in /srv/run/php, which the mp setup of a version older\n    than 0.9.0 leaves out of "+filepath.Join(r.tmpfiles, "monopanel.conf")+": ") {
			t.Fatalf("the notice does not say where the line goes:\n%s", r.stderr)
		}
	})
}

// Going back below 0.9.0 and running that version's mp setup, which writes
// /etc/tmpfiles.d/monopanel.conf without the php-fpm socket directory, leaves
// a line that makes it at boot: monopanel-php.conf, which 0.8.19's API wrote
// and the upgrade keeps — the same line in two files is no warning to
// systemd-tmpfiles — or which the preremove writes where PHP came with
// 0.9.0. The upgrade after that puts the line back into the copy and keeps
// the file again. A file that holds more than the panel's line is worth a
// warning, and stays.
func TestGoingBackKeepsThePHPSocketDirectory(t *testing.T) {
	// mp setup of 0.8.19 (setup.go of v0.8.19) and the API's file of 0.8.19
	// (ops_php.go of v0.8.19).
	setup0819 := func(runDir, group string) string { return "d " + runDir + " 0771 root " + group + " -\n" }
	api0819 := func(runDir string) string { return "d " + runDir + "/php 0755 root root -\n" }
	// The copy as the upgrade leaves it: the php line added, and the sticky
	// bit on the default line (postinstall.sh leaves another layout's mode
	// to mp setup).
	upgraded := func(runDir, group string) string {
		if runDir == "/run/monopanel" && group == "monopanel" {
			return tmpfilesConf(config.Config{RunDir: runDir, ServiceGroup: group})
		}
		return setup0819(runDir, group) + api0819(runDir)
	}
	phpAtBoot := func(r *scriptRun, runDir string) bool {
		t.Helper()
		files, _ := filepath.Glob(filepath.Join(r.tmpfiles, "*.conf"))
		for _, f := range files {
			b, _ := os.ReadFile(f)
			if slices.Contains(strings.Split(string(b), "\n"), "d "+runDir+"/php 0755 root root -") {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		what, runDir, group string
		php0819             bool // PHP came with 0.8.19, whose API wrote monopanel-php.conf
		env, args           []string
	}{
		{"dpkg, PHP of 0.8.19", "/run/monopanel", "monopanel", true, []string{"OLDER=0.8.19"}, []string{"upgrade", "0.8.19"}},
		{"dpkg, PHP of 0.9.0", "/run/monopanel", "monopanel", false, []string{"OLDER=0.8.19"}, []string{"upgrade", "0.8.19"}},
		{"rpm, PHP of 0.9.0, another run directory", "/srv/run", "panel", false, []string{"PROBE=1"}, []string{"1"}},
		{"dnf remove, then the older install", "/run/monopanel", "monopanel", false, []string{"PROBE=1"}, []string{"0"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := newScriptRun(t)
			copyPath, phpPath := filepath.Join(r.tmpfiles, "monopanel.conf"), filepath.Join(r.tmpfiles, "monopanel-php.conf")
			writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": setup0819(c.runDir, c.group)})
			if c.php0819 {
				writeFiles(t, r.tmpfiles, map[string]string{"monopanel-php.conf": api0819(c.runDir)})
			}
			// The upgrade to this version: the line goes into the copy, the
			// API's file stays.
			r.run(r.postinstallTmpfiles(), nil, "configure", "0.8.19")
			if b, _ := os.ReadFile(copyPath); string(b) != upgraded(c.runDir, c.group) {
				t.Fatalf("the copy after the upgrade:\n%s", b)
			}
			if b, err := os.ReadFile(phpPath); c.php0819 != (err == nil) || c.php0819 && string(b) != api0819(c.runDir) {
				t.Fatalf("monopanel-php.conf after the upgrade: %v %q", err, b)
			}
			if r.stderr != "" {
				t.Fatalf("the upgrade warns: %s", r.stderr)
			}
			// Back to 0.8.19, whose mp setup writes the copy anew.
			r.run(r.preremove(), c.env, c.args...)
			writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": setup0819(c.runDir, c.group)})
			if !phpAtBoot(r, c.runDir) {
				t.Fatal("nothing makes the php-fpm socket directory at boot")
			}
			if b, _ := os.ReadFile(phpPath); string(b) != api0819(c.runDir) {
				t.Fatalf("monopanel-php.conf: %q", b)
			}
			// Up again: the copy has its line back, the file stays, no word.
			r.run(r.postinstallTmpfiles(), nil, "configure", "0.8.19")
			if b, _ := os.ReadFile(copyPath); string(b) != upgraded(c.runDir, c.group) || !phpAtBoot(r, c.runDir) || r.stderr != "" {
				t.Fatalf("the copy after the next upgrade:\n%s\n%s", b, r.stderr)
			}
		})
	}
	t.Run("more than the panel's line", func(t *testing.T) {
		r := newScriptRun(t)
		const more = "d /run/monopanel/php 0755 root root -\nL+ /etc/cron.d/x - - - - /var/www/alex/x\n"
		writeFiles(t, r.tmpfiles, map[string]string{"monopanel.conf": setup0819("/run/monopanel", "monopanel"), "monopanel-php.conf": more})
		r.run(r.postinstallTmpfiles(), nil, "configure", "0.8.19")
		if b, _ := os.ReadFile(filepath.Join(r.tmpfiles, "monopanel-php.conf")); string(b) != more {
			t.Fatalf("the file was changed: %q", b)
		}
		if !strings.Contains(r.stderr, "monopanel-php.conf holds more than the line the panel wrote there: check what it makes as root at boot, and leave the panel's line alone in it (d /run/monopanel/php 0755 root root -), which a version older than 0.9.0 needs") {
			t.Fatalf("no warning: %q", r.stderr)
		}
	})
}

// A removal disables the panel, and the package installed again over what
// it left gets the removed version from dpkg, as an upgrade does: that
// install enables the panel again, as a fresh one would and as every
// install up to 0.8.19 did, instead of leaving it off for good. An upgrade
// that follows enables nothing the administrator disabled.
func TestAReinstallAfterARemovalEnablesThePanel(t *testing.T) {
	const enable = "systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service"
	r := newScriptRun(t)
	if err := os.WriteFile(r.config, []byte("web:\n  listen: \":8443\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.dropin, []byte("[Socket]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.run(r.preremove(), nil, "remove")
	r.run(r.postinstall(), nil, "configure", "0.9.0")
	if !r.called(enable) || r.stdout != "MonoPanel installed. Run: mp setup\n" {
		t.Fatalf("the install over a removal: %q\n%s", r.stdout, strings.Join(r.calls, "\n"))
	}
	r.run(r.postinstall(), nil, "configure", "0.9.0")
	if r.called(enable) || r.stdout != "MonoPanel updated.\n" {
		t.Fatalf("the upgrade after it: %q\n%s", r.stdout, strings.Join(r.calls, "\n"))
	}
}

// Going back below 0.9.0 (or the package going): once the binary that stays
// knows no mail policy, postremove takes the policy's token out of main.cf
// and reloads postfix — otherwise postfix would keep every message, inbound
// mail too, waiting for a policy nobody runs as soon as its last process
// ended (review r2 F2, the lead's decision 5). Where the binary that stays
// has the policy — an upgrade — or main.cf does not name it, nothing
// happens; nor for what dpkg passes when it backs out.
func TestPostremoveTakesThePolicyOutOfMainCf(t *testing.T) {
	const withPolicy = "myhostname = mail.example.com\nsmtpd_milters = { inet:127.0.0.1:8890, default_action=tempfail }, inet:127.0.0.1:8891\nnon_smtpd_milters = { inet:127.0.0.1:8890, default_action=tempfail }, inet:127.0.0.1:8891\n"
	const without = "myhostname = mail.example.com\nsmtpd_milters = inet:127.0.0.1:8891\nnon_smtpd_milters = inet:127.0.0.1:8891\n"
	const stop = "systemctl disable --now monopanel-mailpolicy.service monopanel-mailpolicy-unix.socket monopanel-mailpolicy-inet.socket"
	for _, c := range []struct {
		what string
		env  []string
		args []string
		act  bool
	}{
		{"dpkg, an older version", []string{"MAILPOLICY=1"}, []string{"upgrade", "0.8.19"}, true},
		{"rpm, an older version", []string{"MAILPOLICY=1"}, []string{"1"}, true},
		{"dpkg, the package removed", []string{"MAILPOLICY=127"}, []string{"remove"}, true},
		{"rpm, the package removed", []string{"MAILPOLICY=127"}, []string{"0"}, true},
		{"dpkg, an upgrade", nil, []string{"upgrade", "0.9.1"}, false},
		{"rpm, an upgrade", nil, []string{"1"}, false},
		{"dpkg backs out", []string{"MAILPOLICY=1"}, []string{"abort-upgrade", "0.9.0"}, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := newScriptRun(t)
			writeFiles(t, filepath.Dir(r.maincf), map[string]string{"main.cf": withPolicy})
			r.run(r.postremove(), c.env, c.args...)
			b, _ := os.ReadFile(r.maincf)
			if want := map[bool]string{true: without, false: withPolicy}[c.act]; string(b) != want {
				t.Errorf("main.cf:\n%s\nwant\n%s", b, want)
			}
			if r.called("postfix reload") != c.act || r.called(stop) != c.act {
				t.Errorf("acted %v: %v", c.act, r.calls)
			}
		})
	}
	t.Run("main.cf without the policy", func(t *testing.T) {
		r := newScriptRun(t)
		writeFiles(t, filepath.Dir(r.maincf), map[string]string{"main.cf": without})
		r.run(r.postremove(), []string{"MAILPOLICY=1"}, "upgrade", "0.8.19")
		if b, _ := os.ReadFile(r.maincf); string(b) != without || r.called("postfix reload") {
			t.Errorf("main.cf: %s, calls %v", b, r.calls)
		}
	})
	t.Run("no mail server", func(t *testing.T) {
		r := newScriptRun(t)
		r.run(r.postremove(), []string{"MAILPOLICY=1"}, "1")
		if r.called("postfix reload") {
			t.Errorf("calls %v", r.calls)
		}
	})
}
