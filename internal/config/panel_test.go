package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePanelValue(t *testing.T) {
	good := []struct{ key, in, want string }{
		{KeyHostname, " Panel.Example.COM. ", "panel.example.com"},
		{KeyHostname, "", ""},
		{KeyHostname, "server1", "server1"},
		{KeyListen, ":8443", ":8443"},
		{KeyListen, " 203.0.113.5:9443 ", "203.0.113.5:9443"},
		{KeyListen, "[2001:DB8::1]:9443", "[2001:db8::1]:9443"},
		{KeyListen, "LocalHost:9443", "localhost:9443"},
		{KeyLogLevel, "WARNING", "warn"},
		{KeyLogLevel, "debug", "debug"},
		{KeyLogFormat, "JSON", "json"},
		{KeyJobsWorkers, " 4 ", "4"},
	}
	for _, c := range good {
		got, err := NormalizePanelValue(c.key, c.in)
		if err != nil || got != c.want {
			t.Errorf("%s=%q: got %q, %v; want %q", c.key, c.in, got, err, c.want)
		}
	}
	bad := []struct{ key, in string }{
		{KeyHostname, "203.0.113.5"},
		{KeyHostname, "2001:db8::1"},
		{KeyHostname, "under_score.example.com"},
		{KeyHostname, "a..example.com"},
		{KeyHostname, "-a.example.com"},
		{KeyHostname, "panel.example.com/x"},
		{KeyHostname, strings.Repeat("a", 64) + ".com"},
		{KeyListen, "8443"},
		{KeyListen, ":0"},
		{KeyListen, ":65536"},
		{KeyListen, ":08443"},
		{KeyListen, ":+8443"},
		{KeyListen, "[::1]"},
		{KeyListen, "bad host:8443"},
		{KeyListen, "fe80::1%eth0:8443"},
		{KeyLogLevel, "verbose"},
		{KeyLogFormat, "xml"},
		{KeyJobsWorkers, "0"},
		{KeyJobsWorkers, "65"},
		{KeyJobsWorkers, "two"},
		{"update.public_key", "AAAA"},
		{"data_dir", "/tmp"},
	}
	for _, c := range bad {
		if got, err := NormalizePanelValue(c.key, c.in); err == nil {
			t.Errorf("%s=%q accepted as %q", c.key, c.in, got)
		}
	}
}

// The file mp setup writes: only the requested lines change, the release key
// and everything else stay byte for byte.
func TestEditPanelKeysTouchesOnlyTheirLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Default()
	cfg.Web.Hostname = "panel.example.com"
	cfg.Update.PublicKey = "c2lnbmF0dXJlLWtleS1mb3ItdGhlLXRlc3Q="
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(p)
	out, changed, err := EditPanelKeys(orig, map[string]string{KeyListen: "127.0.0.1:9443", KeyJobsWorkers: "4", KeyHostname: "panel.example.com", KeyLogLevel: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, " ") != "web.listen log.level jobs.workers" {
		t.Fatalf("changed: %v", changed)
	}
	before, after := strings.Split(string(orig), "\n"), strings.Split(string(out), "\n")
	if len(before) != len(after) {
		t.Fatalf("line count %d → %d:\n%s", len(before), len(after), out)
	}
	diff := []string{}
	for i := range before {
		if before[i] != after[i] {
			diff = append(diff, after[i])
		}
	}
	if strings.Join(diff, "|") != "  listen: 127.0.0.1:9443|  workers: 4|  level: debug" {
		t.Fatalf("changed lines: %q", diff)
	}
	if err := os.WriteFile(p, out, 0o640); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Web.Listen != "127.0.0.1:9443" || back.Jobs.Workers != 4 || back.Log.Level != "debug" || back.Update.PublicKey != cfg.Update.PublicKey || back.Web.Hostname != "panel.example.com" {
		t.Fatalf("loaded back: %+v", back)
	}

	// Nothing to change: the same bytes come back.
	same, changed, err := EditPanelKeys(out, map[string]string{KeyListen: "127.0.0.1:9443"})
	if err != nil || len(changed) != 0 || string(same) != string(out) {
		t.Fatalf("no-op edit: %v %v", changed, err)
	}
}

// A file an administrator wrote by hand keeps its comments, the keys this
// version does not know and audit_file: none (which Save would undo); missing
// keys and sections are added.
func TestEditPanelKeysKeepsAHandWrittenFile(t *testing.T) {
	src := "# written by hand\n" +
		"data_dir: /srv/monopanel   # big disk\n" +
		"future_setting: kept\n" +
		"web:\n" +
		"  listen: :8443 # the panel port\n" +
		"update:\n" +
		"  public_key: none\n" +
		"log:\n" +
		"  audit_file: none\n"
	out, changed, err := EditPanelKeys([]byte(src), map[string]string{KeyHostname: "panel.example.com", KeyListen: ":9443", KeyLogFormat: "json", KeyJobsWorkers: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 4 {
		t.Fatalf("changed: %v", changed)
	}
	text := string(out)
	for _, want := range []string{"# written by hand", "# big disk", "future_setting: kept", "# the panel port", "public_key: none", "audit_file: none", "hostname: panel.example.com", "format: json", "workers: 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q lost:\n%s", want, text)
		}
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, out, 0o640)
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Web.Listen != ":9443" || back.Web.Hostname != "panel.example.com" || back.Log.Format != "json" || back.Jobs.Workers != 3 ||
		back.Log.AuditFile != "" || back.Update.PublicKey != "none" || back.DataDir != "/srv/monopanel" {
		t.Fatalf("loaded back: %+v", back)
	}
}

func TestEditPanelKeysEmptyAndCommentOnlyFiles(t *testing.T) {
	for _, src := range []string{"", "# nothing here yet\n", "# no newline"} {
		out, _, err := EditPanelKeys([]byte(src), map[string]string{KeyListen: ":9443", KeyJobsWorkers: "5"})
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if !strings.HasPrefix(string(out), src) {
			t.Fatalf("%q: comments lost: %q", src, out)
		}
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, out, 0o640)
		back, err := Load(p)
		if err != nil || back.Web.Listen != ":9443" || back.Jobs.Workers != 5 {
			t.Fatalf("%q: %+v %v\n%s", src, back.Web, err, out)
		}
	}
}

// What the editor does not understand it refuses rather than writes; it never
// panics, and only the panel keys may be set.
func TestEditPanelKeysRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		src string
		set map[string]string
	}{
		"release key":     {"web:\n  listen: :8443\n", map[string]string{"update.public_key": "AAAA"}},
		"bad value":       {"web:\n  listen: :8443\n", map[string]string{KeyListen: "nowhere"}},
		"two documents":   {"web:\n  listen: :8443\n---\nweb:\n  listen: :9\n", map[string]string{KeyListen: ":9443"}},
		"null section":    {"web:\nlog:\n  level: info\n", map[string]string{KeyHostname: "a.example.com"}},
		"scalar section":  {"web: 5\n", map[string]string{KeyHostname: "a.example.com"}},
		"not a mapping":   {"- a\n- b\n", map[string]string{KeyListen: ":9443"}},
		"does not parse":  {"web:\n  listen: [\n", map[string]string{KeyListen: ":9443"}},
		"duplicate key":   {"web:\n  listen: :1\n  listen: :2\n", map[string]string{KeyListen: ":9443"}},
		"anchored values": {"base: &b\n  listen: :8443\nweb: *b\n", map[string]string{KeyListen: ":9443"}},
	} {
		out, _, err := EditPanelKeys([]byte(c.src), c.set)
		if err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}
}

// Root's tools (mp update trust, mp config set --local, mp setup) change the
// release key and the panel keys in place: comments, other settings and the
// file's mode stay, a default the file leaves out is not written again, and a
// symlink is refused.
func TestWriteFileKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	orig := "# hand-written\nlog:\n  audit_file: none # no audit file\nweb:\n  listen: \":9443\" # panel port\n"
	if err := os.WriteFile(path, []byte(orig), 0o604); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteFileKeys(path, map[string]string{KeyPublicKey: "RWQexamplekey", KeyLogLevel: "info"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != KeyPublicKey {
		t.Errorf("changed %v, want only %s (info is the default already)", changed, KeyPublicKey)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"# hand-written", "audit_file: none # no audit file", `listen: ":9443" # panel port`, "public_key: RWQexamplekey"} {
		if !strings.Contains(got, want) {
			t.Errorf("file lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "level:") {
		t.Errorf("the default log level was written:\n%s", got)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o604 {
		t.Errorf("mode %v, %v; want 0604 kept", st.Mode().Perm(), err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Update.PublicKey != "RWQexamplekey" || cfg.Log.AuditFile != "" {
		t.Errorf("Load after the edit: key %q audit %q, %v", cfg.Update.PublicKey, cfg.Log.AuditFile, err)
	}
	if _, err := WriteFileKeys(path, map[string]string{"agent.allowed_write_prefixes": "/"}, -1); err == nil {
		t.Error("a key outside the panel keys and the release key was written")
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteFileKeys(link, map[string]string{KeyLogLevel: "debug"}, -1); err == nil {
		t.Error("a symlinked config.yaml was replaced")
	}
	fresh := filepath.Join(dir, "sub", "config.yaml")
	if _, err := WriteFileKeys(fresh, map[string]string{KeyHostname: "panel.example.com"}, -1); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(fresh); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("new file mode %v, %v; want 0640", st.Mode().Perm(), err)
	}
}
