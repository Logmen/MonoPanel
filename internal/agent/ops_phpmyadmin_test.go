package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/phpmyadmin"
)

// pmaHost points phpMyAdmin's places at a temporary tree and has the test's
// own account stand in for its pool user.
func pmaHost(t *testing.T) (s *Server, c *Client, root, run string) {
	t.Helper()
	s, c = testServer(t, t.TempDir())
	base := t.TempDir()
	root, run = filepath.Join(base, "pma"), filepath.Join(base, "run")
	for _, d := range []string{root, run} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prevRoot, prevRun, prevAcct := pmaRoot, pmaRunDir, pmaAccount
	pmaRoot, pmaRunDir = root, run
	pmaAccount = func() (int, int, error) { return os.Getuid(), os.Getgid(), nil }
	t.Cleanup(func() { pmaRoot, pmaRunDir, pmaAccount = prevRoot, prevRun, prevAcct })
	return s, c, root, run
}

func wantRefused(t *testing.T, err error, what string) {
	t.Helper()
	var ae *Error
	if err == nil || !errors.As(err, &ae) || ae.Status < 400 || ae.Status >= 500 {
		t.Fatalf("%s: want a refusal, got %v", what, err)
	}
}

// The call writes nothing but its own shapes.
func TestPhpMyAdminRequestChecks(t *testing.T) {
	ok := []PhpMyAdminRequest{
		{Action: "files", Version: "5.2.3", Config: "<?php\n", Signon: "<?php\n"},
		{Action: "ticket", Name: strings.Repeat("a1", 32), Content: "{}"},
		{Action: "sweep"},
		{Action: "prune", Version: "5.2.3"},
	}
	for _, r := range ok {
		if err := CheckPhpMyAdminRequest(&r); err != nil {
			t.Errorf("%+v refused: %v", r, err)
		}
	}
	bad := []PhpMyAdminRequest{
		{Action: "write", Version: "5.2.3"},
		{Action: "files", Version: "../../etc", Config: "x", Signon: "x"},
		{Action: "files", Version: "5.2.3", Config: "x"},
		{Action: "files", Version: "5.2.3", Config: "x\x00", Signon: "x"},
		{Action: "ticket", Name: "../../etc/cron.d/x", Content: "{}"},
		{Action: "ticket", Name: strings.Repeat("A1", 32), Content: "{}"},
		{Action: "ticket", Name: strings.Repeat("a1", 32), Content: strings.Repeat("x", phpmyadmin.MaxTicket+1)},
		{Action: "prune", Version: ""},
	}
	for _, r := range bad {
		if CheckPhpMyAdminRequest(&r) == nil {
			t.Errorf("%+v passed", r)
		}
	}
}

// The tree's two files land root's (here: the test's own), the
// configuration readable by the pool user's group alone; a tree someone
// else may write to is refused.
func TestPhpMyAdminFiles(t *testing.T) {
	ctx := context.Background()
	_, c, root, _ := pmaHost(t)
	tree := filepath.Join(root, "5.2.3")
	if _, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "files", Version: "5.2.3", Config: "<?php // c\n", Signon: "<?php // s\n"}); err == nil {
		t.Fatal("files went into a tree that is not there")
	}
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "files", Version: "5.2.3", Config: "<?php // c\n", Signon: "<?php // s\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 2 {
		t.Fatalf("written: %v", res.Written)
	}
	for name, mode := range map[string]os.FileMode{phpmyadmin.ConfigFile: 0o640, phpmyadmin.SignonScript: 0o644} {
		st, err := os.Lstat(filepath.Join(tree, name))
		if err != nil || st.Mode().Perm() != mode || !st.Mode().IsRegular() {
			t.Errorf("%s: %v %v, want %v", name, st.Mode(), err, mode)
		}
	}
	if again, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "files", Version: "5.2.3", Config: "<?php // c\n", Signon: "<?php // s\n"}); err != nil || len(again.Written) != 0 {
		t.Errorf("the same files written again: %v %v", again, err)
	}
	// A symlink under the name is replaced, never written through.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(tree, phpmyadmin.SignonScript))
	if err := os.Symlink(victim, filepath.Join(tree, phpmyadmin.SignonScript)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "files", Version: "5.2.3", Config: "<?php // c\n", Signon: "<?php // s2\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the write went through a symlink: %q", b)
	}
	if err := os.Chmod(tree, 0o775); err != nil {
		t.Fatal(err)
	}
	_, err = c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "files", Version: "5.2.3", Config: "<?php\n", Signon: "<?php\n"})
	wantRefused(t, err, "a group-writable tree")
}

// A ticket is the pool user's 0400 in its own directory, and only there.
func TestPhpMyAdminTicket(t *testing.T) {
	ctx := context.Background()
	_, c, _, run := pmaHost(t)
	name := strings.Repeat("ab", 32)
	tickets := filepath.Join(run, "tickets")
	if err := os.Mkdir(tickets, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "ticket", Name: name, Content: `{"user":"x"}`})
	wantRefused(t, err, "a ticket directory its group may read")
	if err := os.Chmod(tickets, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "ticket", Name: name, Content: `{"user":"x"}`}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(tickets, name))
	if err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("ticket: %v %v", st, err)
	}
}

// The sweep takes away the tickets nobody took in time and what does not
// belong there, answers the sign-out marks of temporary accounts and takes
// them away; prune keeps the release it is told to and what is no release.
func TestPhpMyAdminSweepAndPrune(t *testing.T) {
	ctx := context.Background()
	_, c, root, run := pmaHost(t)
	tickets, logout := filepath.Join(run, "tickets"), filepath.Join(run, "logout")
	for _, d := range []string{tickets, logout} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fresh, stale := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	for _, n := range []string{fresh, stale, "junk"} {
		if err := os.WriteFile(filepath.Join(tickets, n), []byte("{}"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(tickets, stale), old, old); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"mp.pma.0123456789abcdef", "root"} {
		if err := os.WriteFile(filepath.Join(logout, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "sweep"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Logouts, []string{"mp.pma.0123456789abcdef"}) {
		t.Errorf("logouts: %v", res.Logouts)
	}
	left, _ := os.ReadDir(tickets)
	if len(left) != 1 || left[0].Name() != fresh {
		t.Errorf("tickets left: %v", left)
	}
	if marks, _ := os.ReadDir(logout); len(marks) != 0 {
		t.Errorf("marks left: %v", marks)
	}

	for _, d := range []string{"5.2.2", "5.2.3", "keep-me"} {
		if err := os.MkdirAll(filepath.Join(root, d, "libraries"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.PhpMyAdmin(ctx, &PhpMyAdminRequest{Action: "prune", Version: "5.2.3"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"5.2.3", "keep-me"}) {
		t.Errorf("after prune: %v", names)
	}
}

// The release unpacks into its own tree and nowhere else of phpMyAdmin's.
func TestTarIntoThePhpMyAdminTree(t *testing.T) {
	s, _, root, _ := pmaHost(t)
	if err := os.Mkdir(filepath.Join(root, "5.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.cfg.DownloadsDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(s.cfg.DownloadsDir(), phpmyadmin.Archive())
	if err := os.WriteFile(archive, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	plan, err := s.planTool(&toolCall{name: "tar", mode: modeTool, args: []string{"-xzf", archive, "-C", filepath.Join(root, "5.2.3"), "--strip-components=1", "--no-same-owner"}})
	if err != nil {
		t.Fatalf("the release's tree refused: %v", err)
	}
	plan.close()
	for _, dest := range []string{root, filepath.Join(root, "current"), filepath.Join(root, "5.2.3", "libraries"), filepath.Dir(root)} {
		if plan, err := s.planTool(&toolCall{name: "tar", mode: modeTool, args: []string{"-xzf", archive, "-C", dest, "--no-same-owner"}}); err == nil {
			plan.close()
			t.Errorf("extracting into %s passed", dest)
		}
	}
}
