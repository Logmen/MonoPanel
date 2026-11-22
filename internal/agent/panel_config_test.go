package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
)

// serveAgent runs an agent with the given configuration on a socket. Its
// units and the drop-ins of the panel's socket go into a temporary
// directory, names resolve in the tests' DNS (testLookup), and systemd is
// never asked; tweak changes the agent before it serves.
func serveAgent(t *testing.T, cfg config.Config, tweak ...func(*Server)) (*Server, *Client) {
	t.Helper()
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	p, _ := osprofile.FromRelease(rel)
	s := NewServer(cfg, p, nil)
	s.unitDir = filepath.Join(t.TempDir(), "system")
	s.lookupIP = testLookup(nil)
	s.daemonReload = func(context.Context) error { return nil }
	for _, f := range tweak {
		f(s)
	}
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext}
	go srv.Serve(&peercred.Listener{Listener: ln}) //nolint:errcheck // stops with the test
	t.Cleanup(func() { srv.Close() })
	return s, NewClient(sock)
}

// panelConfigFixture writes config.yaml and the secret key into a temporary
// /etc/monopanel and loads the configuration from there, the way the agent
// starts. The temporary tree is on the write allow-list, so that a refusal can
// only come from the protection of the two files.
func panelConfigFixture(t *testing.T) (root, cfgPath string, cfg config.Config) {
	t.Helper()
	root = t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	grp, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Skip("no current group")
	}
	c := config.Default()
	c.ConfigDir = etc
	c.TemplatesDir = filepath.Join(etc, "templates")
	c.SecretKeyFile = filepath.Join(etc, "secret.key")
	c.DataDir = filepath.Join(root, "data")
	c.Agent.StateDir = filepath.Join(root, "agent")
	c.ServiceGroup = grp.Name
	c.Agent.ExtraWritePrefixes = []string{root + "/"}
	c.Web.Hostname = "panel.example.com"
	c.Update.PublicKey = "c2lnbmluZy1rZXktb2YtdGhlLXRlc3Q="
	cfgPath = filepath.Join(etc, "config.yaml")
	if err := c.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.SecretKeyFile, []byte(strings.Repeat("ab", 32)+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.Load(cfgPath); err != nil {
		t.Fatal(err)
	}
	return root, cfgPath, cfg
}

func agentStatus(err error) int {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// The dedicated RPC changes the five panel keys and nothing else: the release
// key stays byte for byte, the file keeps its mode and owner, the previous
// version goes to the history, and any other key is refused outright.
func TestPanelConfigChangesOnlyThePanelKeys(t *testing.T) {
	ctx := context.Background()
	_, path, cfg := panelConfigFixture(t)
	s, cl := serveAgent(t, cfg)
	before, _ := os.ReadFile(path)
	stBefore, _ := os.Stat(path)

	res, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": "127.0.0.1:9443", "jobs.workers": "4", "web.hostname": "panel.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != path || strings.Join(res.Changed, " ") != "web.listen jobs.workers" {
		t.Fatalf("answer: %+v", res)
	}
	after, _ := os.ReadFile(path)
	back, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Web.Listen != "127.0.0.1:9443" || back.Jobs.Workers != 4 || back.Web.Hostname != "panel.example.com" {
		t.Fatalf("loaded back: %+v", back)
	}
	if back.Update.PublicKey != cfg.Update.PublicKey || !bytes.Contains(after, []byte("  public_key: "+cfg.Update.PublicKey+"\n")) {
		t.Fatalf("the release key changed:\n%s", after)
	}
	stAfter, _ := os.Stat(path)
	if stAfter.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640", stAfter.Mode().Perm())
	}
	ob, nb := stBefore.Sys().(*syscall.Stat_t), stAfter.Sys().(*syscall.Stat_t)
	if ob.Uid != nb.Uid || ob.Gid != nb.Gid {
		t.Fatalf("owner %d:%d → %d:%d", ob.Uid, ob.Gid, nb.Uid, nb.Gid)
	}
	hist := filepath.Join(s.cfg.ConfHistoryDir(), strings.TrimPrefix(path, "/"))
	entries, _ := os.ReadDir(hist)
	if len(entries) != 1 {
		t.Fatalf("history: %d entries in %s", len(entries), hist)
	}
	if old, _ := os.ReadFile(filepath.Join(hist, entries[0].Name())); !bytes.Equal(old, before) {
		t.Fatal("the history keeps something else than the previous file")
	}

	// Nothing else passes — not even alongside a panel key.
	for _, set := range []map[string]string{
		{"update.public_key": "QUFBQQ=="},
		{"web.listen": ":9555", "update.public_key": "none"},
		{"agent.extra_write_prefixes": "/"},
		{"data_dir": "/tmp"},
		{"web.tls_cert": "/tmp/x.pem"},
	} {
		if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: set}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("%v: %v, want 403", set, err)
		}
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, after) {
		t.Fatalf("a refused request changed the file:\n%s", now)
	}

	for name, c := range map[string]struct {
		req  PanelConfigRequest
		want int
	}{
		"invalid value":   {PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": "nowhere"}}, http.StatusUnprocessableEntity},
		"too many":        {PanelConfigRequest{Path: path, Set: map[string]string{"jobs.workers": "1000"}}, http.StatusUnprocessableEntity},
		"empty":           {PanelConfigRequest{Path: path}, http.StatusBadRequest},
		"another file":    {PanelConfigRequest{Path: "/etc/monopanel/other.yaml", Set: map[string]string{"log.level": "debug"}}, http.StatusConflict},
		"IP as host name": {PanelConfigRequest{Path: path, Set: map[string]string{"web.hostname": "203.0.113.5"}}, http.StatusUnprocessableEntity},
	} {
		if _, err := cl.SetPanelConfig(ctx, &c.req); agentStatus(err) != c.want {
			t.Errorf("%s: %v, want %d", name, err, c.want)
		}
	}

	// The same value again writes nothing.
	res, err = cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"jobs.workers": "4"}})
	if err != nil || len(res.Changed) != 0 {
		t.Fatalf("no-op: %+v %v", res, err)
	}
	if entries, _ := os.ReadDir(hist); len(entries) != 1 {
		t.Fatalf("a no-op wrote history: %d entries", len(entries))
	}
}

// A config.yaml that is a symlink is not replaced (the rename would turn it
// into a file); a missing one is created the way mp setup makes it.
func TestPanelConfigFileShapes(t *testing.T) {
	ctx := context.Background()
	root, path, cfg := panelConfigFixture(t)
	_, cl := serveAgent(t, cfg)

	real := filepath.Join(root, "elsewhere.yaml")
	os.Rename(path, real)
	os.Symlink(real, path)
	if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"log.level": "debug"}}); agentStatus(err) != http.StatusConflict {
		t.Fatalf("symlink: %v", err)
	}
	if b, _ := os.ReadFile(real); bytes.Contains(b, []byte("level: debug")) {
		t.Fatal("the symlink target was written")
	}

	os.Remove(path)
	if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"log.level": "debug", "web.listen": ":9443"}}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o640 {
		t.Fatalf("new file: %v %v", st, err)
	}
	back, err := config.Load(path)
	if err != nil || back.Log.Level != "debug" || back.Web.Listen != ":9443" {
		t.Fatalf("new file loads as %+v, %v", back.Log, err)
	}
}

// /etc/monopanel/ is on the write allow-list, but config.yaml and the secret
// key are not the panel's to write: no generic operation reaches them, also not
// through a symlinked directory, a symlink or a second name left in a
// directory the API process owns.
func TestGenericWritesSpareConfigAndSecretKey(t *testing.T) {
	ctx := context.Background()
	root, path, cfg := panelConfigFixture(t)
	_, cl := serveAgent(t, cfg)
	etc := filepath.Dir(path)
	key := cfg.SecretKeyFile
	keyBefore, _ := os.ReadFile(key)
	cfgBefore, _ := os.ReadFile(path)
	alias := filepath.Join(root, "alias")
	os.Symlink(etc, alias)

	for _, p := range []string{path, key, filepath.Join(alias, "config.yaml"), filepath.Join(etc, ".", "secret.key")} {
		if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: p, Content: "update:\n  public_key: none\n"}}}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("config/apply %s: %v, want 403", p, err)
		}
	}
	// The rest of the directory is still the panel's.
	if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(etc, "templates", "nginx", "site.conf.tmpl"), Content: "x"}}}); err != nil {
		t.Fatalf("a template override: %v", err)
	}

	for _, req := range []RemovePathsRequest{
		{Paths: []string{path}},
		{Paths: []string{key}},
		{Paths: []string{etc}, Recursive: true},
		{Paths: []string{root}, Recursive: true},
		{Paths: []string{filepath.Join(alias, "config.yaml")}},
	} {
		if _, err := cl.RemovePaths(ctx, &req); agentStatus(err) != http.StatusForbidden {
			t.Errorf("paths/remove %v: %v, want 403", req.Paths, err)
		}
	}

	for _, d := range []string{etc, path, key + ".d/../secret.key"} {
		if _, err := cl.EnsureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: d, Mode: 0o777}}}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("dirs/ensure %s: %v, want 403", d, err)
		}
	}
	if st, _ := os.Stat(etc); st.Mode().Perm() != 0o750 {
		t.Fatalf("the directory mode changed to %v", st.Mode().Perm())
	}

	if _, err := cl.Tool(ctx, &ToolRequest{Name: "du", Args: []string{"-s", root}, OutputFile: key}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("tool output into the secret key: %v, want 403", err)
	}
	link := filepath.Join(root, "data", "dump.sql")
	os.MkdirAll(filepath.Dir(link), 0o750)
	os.Symlink(key, link)
	if _, err := cl.Tool(ctx, &ToolRequest{Name: "du", Args: []string{"-s", root}, OutputFile: link}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("tool output through a symlink: %v, want 403", err)
	}
	second := filepath.Join(root, "data", "dump2.sql")
	if err := os.Link(path, second); err == nil {
		if _, err := cl.Tool(ctx, &ToolRequest{Name: "du", Args: []string{"-s", root}, OutputFile: second}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("tool output into a second name of config.yaml: %v, want 403", err)
		}
	}

	if b, _ := os.ReadFile(key); !bytes.Equal(b, keyBefore) {
		t.Fatal("the secret key changed")
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, cfgBefore) {
		t.Fatal("config.yaml changed")
	}
	// A symlink to the directory is only a name: removing it leaves the
	// directory and its files alone.
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{alias}, Recursive: true}); err != nil {
		t.Fatalf("removing the symlink: %v", err)
	}
	if _, err := os.Lstat(alias); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the symlink is still there")
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, cfgBefore) {
		t.Fatal("removing a symlink to the directory reached config.yaml")
	}
}
