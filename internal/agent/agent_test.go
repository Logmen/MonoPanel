package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
)

func testServer(t *testing.T, allowed string) (*Server, *Client) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Agent.ExtraWritePrefixes = []string{allowed + "/"}
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	p, _ := osprofile.FromRelease(rel)
	s := NewServer(cfg, p, nil)
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext}
	go srv.Serve(&peercred.Listener{Listener: ln})
	t.Cleanup(func() { srv.Close() })
	return s, NewClient(sock)
}

func TestApplyConfigSetAndRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, cl := testServer(t, dir)
	truePath, _ := exec.LookPath("true")
	falsePath, _ := exec.LookPath("false")
	if truePath == "" || falsePath == "" {
		t.Skip("true/false not found")
	}
	existing := filepath.Join(dir, "sites", "a.conf")
	os.MkdirAll(filepath.Dir(existing), 0o755)
	os.WriteFile(existing, []byte("old"), 0o600)
	fresh := filepath.Join(dir, "sites", "b.conf")

	resp, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{
		Files:    []FileSpec{{Path: existing, Content: "new", Mode: 0o644}, {Path: fresh, Content: "b"}},
		Validate: [][]string{{truePath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Written) != 2 || resp.Validated != 1 {
		t.Fatalf("resp %+v", resp)
	}
	if b, _ := os.ReadFile(existing); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	if st, _ := os.Stat(existing); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	hist := filepath.Join(s.cfg.ConfHistoryDir(), strings.TrimPrefix(existing, "/"))
	if entries, _ := os.ReadDir(hist); len(entries) != 1 {
		t.Fatalf("history entries: %d in %s", len(entries), hist)
	}

	// unchanged content is reported, not rewritten
	resp, _ = cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: existing, Content: "new", Mode: 0o644}}})
	if len(resp.Unchanged) != 1 || len(resp.Written) != 0 {
		t.Fatalf("unchanged detection: %+v", resp)
	}

	// failing validator rolls back both files
	third := filepath.Join(dir, "sites", "c.conf")
	_, err = cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{
		Files:    []FileSpec{{Path: existing, Content: "broken"}, {Path: third, Content: "c"}},
		Validate: [][]string{{falsePath}},
	})
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 validation error, got %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "new" {
		t.Fatalf("rollback failed: %q", b)
	}
	if _, err := os.Stat(third); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new file must be removed on rollback")
	}

	// outside the allow-list
	_, err = cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: "/etc/passwd", Content: "x"}}})
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", err)
	}

	// symlinks are never followed
	link := filepath.Join(dir, "sites", "link.conf")
	os.Symlink("/etc/hostname", link)
	_, err = cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: link, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlink must be refused, got %v", err)
	}
}

func TestPingAndInfo(t *testing.T) {
	ctx := context.Background()
	_, cl := testServer(t, t.TempDir())
	p, err := cl.Ping(ctx)
	if err != nil || !p.OK || p.PeerUID != os.Getuid() {
		t.Fatalf("ping %+v %v", p, err)
	}
	info, err := cl.SystemInfo(ctx)
	if err != nil || info.CPUs == 0 || info.Family != "debian" {
		t.Fatalf("info %+v %v", info, err)
	}
}

func TestValidationErrors(t *testing.T) {
	ctx := context.Background()
	_, cl := testServer(t, t.TempDir())
	var ae *Error
	if _, err := cl.EnsureGroup(ctx, &EnsureGroupRequest{Name: "Bad Name"}); !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("group name validation: %v", err)
	}
	if _, err := cl.EnsureUnixUser(ctx, &EnsureUnixUserRequest{Login: "../root"}); !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("login validation: %v", err)
	}
	if _, err := cl.Pkg(ctx, "install", "nginx; rm -rf /"); !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("package validation: %v", err)
	}
	if _, err := cl.Service(ctx, "nginx", "explode"); !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("action validation: %v", err)
	}
}

func TestPathAllowed(t *testing.T) {
	cfg := config.Default()
	s := &Server{cfg: cfg}
	for p, want := range map[string]bool{
		"/etc/nginx/nginx.conf":                    true,
		"/etc/nginx/monopanel/sites/a.conf":        true,
		"/etc/nginx/conf.d/custom.conf":            false,
		"/etc/passwd":                              false,
		"/etc/monopanel/../shadow":                 false,
		"/var/lib/monopanel/certs/x/fullchain.pem": true,
	} {
		if got := s.pathAllowed(filepath.Clean(p)); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}

func TestUnusedHTTPTest(t *testing.T) {
	// keep httptest imported for future handler-level tests
	_ = httptest.NewRecorder()
}

func TestHomeFileOps(t *testing.T) {
	ctx := context.Background()
	www := t.TempDir()
	s, cl := testServer(t, t.TempDir())
	s.cfg.WWWRoot = www
	me := currentUserName(t)
	home := filepath.Join(www, "alex", "data", "www", "example.com")
	os.MkdirAll(home, 0o755)
	res, err := cl.EnsureFile(ctx, &EnsureFileRequest{Path: filepath.Join(home, "index.php"), Content: "<?php", Owner: me})
	if err != nil || !res.Written {
		t.Fatalf("ensure file: %+v %v", res, err)
	}
	res, _ = cl.EnsureFile(ctx, &EnsureFileRequest{Path: filepath.Join(home, "index.php"), Content: "changed", Owner: me, OnlyIfMissing: true})
	if res.Written {
		t.Fatal("only-if-missing must not overwrite")
	}
	// symlinked directory component is refused
	os.Symlink("/etc", filepath.Join(www, "alex", "data", "www", "evil"))
	if _, err := cl.EnsureFile(ctx, &EnsureFileRequest{Path: filepath.Join(www, "alex", "data", "www", "evil", "passwd"), Content: "x", Owner: me}); err == nil {
		t.Fatal("symlink component must be refused")
	}
	if _, err := cl.EnsureFile(ctx, &EnsureFileRequest{Path: "/tmp/outside", Content: "x", Owner: me}); err == nil {
		t.Fatal("outside www root must be refused")
	}
	if err := cl.EnsureSymlink(ctx, &EnsureSymlinkRequest{Path: filepath.Join(www, "alex", "data", "bin-php"), Target: "/usr/bin/php8.4", Owner: me}); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(filepath.Join(www, "alex", "data", "bin-php")); target != "/usr/bin/php8.4" {
		t.Fatalf("symlink target %q", target)
	}
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(www, "alex", "data")}, Recursive: true}); err == nil {
		t.Fatal("data dir itself must not be removable")
	}
	rr, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{home, filepath.Join(www, "alex", "data", "www", "missing")}, Recursive: true})
	if err != nil || len(rr.Removed) != 1 {
		t.Fatalf("remove: %+v %v", rr, err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tree not removed")
	}
	if err := cl.SetACL(ctx, &SetACLRequest{Path: home, Entries: []string{"g:web:rX; rm -rf /"}}); err == nil {
		t.Fatal("bad ACL entry must be refused")
	}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	return u.Username
}

func TestToolAllowList(t *testing.T) {
	ctx := context.Background()
	_, cl := testServer(t, t.TempDir())
	if _, err := cl.Tool(ctx, &ToolRequest{Name: "bash", Args: []string{"-c", "id"}}); err == nil {
		t.Fatal("bash must not be allowed")
	}
	res, err := cl.Tool(ctx, &ToolRequest{Name: "du", Args: []string{"-sh", t.TempDir()}})
	if err != nil {
		t.Skipf("du not available: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "\t") {
		t.Fatalf("du: %+v", res)
	}
	res, _ = cl.Tool(ctx, &ToolRequest{Name: "du", Args: []string{"/definitely/missing"}})
	if res == nil || res.ExitCode == 0 {
		t.Fatalf("exit code must be reported: %+v", res)
	}
}
