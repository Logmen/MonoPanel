package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

// plainServer is an agent on a host without SELinux, with the given log
// directory and web root.
func plainServer(t *testing.T, logDir, wwwRoot string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.LogDir, cfg.WWWRoot = logDir, wwwRoot
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	p, err := osprofile.FromRelease(rel)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(cfg, p, nil)
}

// myIDs stands the test's own user in for root and for the account: only
// root may give files away, and the modes and checks are the point here.
func myIDs() siteLogIDs {
	return siteLogIDs{rootUID: os.Getuid(), rootGID: os.Getgid(), uid: os.Getuid(), gid: os.Getgid()}
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// The site logs leave the account's home for a tree only root writes: the
// agent makes it with the modes that let the account read and the nginx
// workers pass, makes the servers' logs root's and the PHP error log the
// account's, and puts a mode someone changed back.
func TestSiteLogTreeLayout(t *testing.T) {
	logDir := t.TempDir()
	if err := os.Chmod(logDir, 0o750); err != nil { // how mp setup made it up to 0.8.19
		t.Fatal(err)
	}
	files := []string{"example.com.access.log", "example.com.error.log", "example.com.php.error.log", "example.com.php.slow.log"}
	resp, err := ensureSiteLogTree(logDir, "alex", myIDs(), files)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(logDir, "sites", "alex")
	if resp.Dir != dir || len(resp.Created) != 2+len(files) || resp.Created[0] != filepath.Join(logDir, "sites") || resp.Created[1] != dir {
		t.Fatalf("response %+v", resp)
	}
	for p, want := range map[string]os.FileMode{
		logDir:                         0o751,
		filepath.Join(logDir, "sites"): 0o711,
		dir:                            0o751,
		filepath.Join(dir, "example.com.access.log"):    0o640,
		filepath.Join(dir, "example.com.error.log"):     0o640,
		filepath.Join(dir, "example.com.php.slow.log"):  0o640,
		filepath.Join(dir, "example.com.php.error.log"): 0o660,
	} {
		if got := perm(t, p); got != want {
			t.Errorf("%s: mode %#o, want %#o", p, got, want)
		}
	}

	// Again: nothing to make, nothing to change.
	if resp, err = ensureSiteLogTree(logDir, "alex", myIDs(), files); err != nil || len(resp.Created) != 0 || resp.Changed != 0 {
		t.Fatalf("second run: %+v %v", resp, err)
	}
	// nginx made its log 0644 before the panel came round, someone opened
	// the directory to everyone's eyes: both go back.
	if err := os.Chmod(filepath.Join(dir, "example.com.access.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if resp, err = ensureSiteLogTree(logDir, "alex", myIDs(), files); err != nil || resp.Changed != 2 {
		t.Fatalf("third run: %+v %v", resp, err)
	}
	if perm(t, filepath.Join(dir, "example.com.access.log")) != 0o640 || perm(t, dir) != 0o751 {
		t.Fatal("modes not put back")
	}
}

// Whatever stands in the place of a directory or a log and is not root's
// plain own stops the call: a symlink (to /etc), a directory the API's user
// or an account could write to, a FIFO. Nothing behind them is touched.
func TestSiteLogTreeRefusesWhatRootDidNotMake(t *testing.T) {
	setup := func(t *testing.T) (logDir, outside string) {
		t.Helper()
		base := t.TempDir()
		logDir, outside = filepath.Join(base, "log"), filepath.Join(base, "etc")
		for _, d := range []string{logDir, outside} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(outside, "ld.so.preload"), []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return logDir, outside
	}
	untouched := func(t *testing.T, outside string) {
		t.Helper()
		if b, _ := os.ReadFile(filepath.Join(outside, "ld.so.preload")); string(b) != "keep\n" || perm(t, filepath.Join(outside, "ld.so.preload")) != 0o600 || perm(t, outside) != 0o755 {
			t.Errorf("the target behind the link was changed: %q %#o", b, perm(t, filepath.Join(outside, "ld.so.preload")))
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 1 {
			t.Errorf("something was made behind the link: %v", entries)
		}
	}
	run := func(logDir string, ids siteLogIDs) error {
		_, err := ensureSiteLogTree(logDir, "alex", ids, []string{"ld.so.preload", "example.com.access.log"})
		return err
	}

	t.Run("log is a symlink", func(t *testing.T) {
		logDir, outside := setup(t)
		if _, err := ensureSiteLogTree(logDir, "alex", myIDs(), nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "ld.so.preload"), filepath.Join(logDir, "sites", "alex", "ld.so.preload")); err != nil {
			t.Fatal(err)
		}
		if err := run(logDir, myIDs()); !errors.Is(err, errSymlinkInPath) {
			t.Fatalf("got %v, want a refusal", err)
		}
		untouched(t, outside)
	})
	t.Run("account directory is a symlink", func(t *testing.T) {
		logDir, outside := setup(t)
		if err := os.Mkdir(filepath.Join(logDir, "sites"), 0o711); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(logDir, "sites", "alex")); err != nil {
			t.Fatal(err)
		}
		if err := run(logDir, myIDs()); !errors.Is(err, errSymlinkInPath) {
			t.Fatalf("got %v, want a refusal", err)
		}
		untouched(t, outside)
	})
	t.Run("sites is a symlink", func(t *testing.T) {
		logDir, outside := setup(t)
		if err := os.Mkdir(filepath.Join(outside, "alex"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(logDir, "sites")); err != nil {
			t.Fatal(err)
		}
		if err := run(logDir, myIDs()); !errors.Is(err, errSymlinkInPath) {
			t.Fatalf("got %v, want a refusal", err)
		}
		if perm(t, filepath.Join(outside, "alex")) != 0o755 {
			t.Error("the directory behind the link was changed")
		}
		if _, err := os.Stat(filepath.Join(outside, "alex", "example.com.access.log")); err == nil {
			t.Error("a log was made behind the link")
		}
	})
	t.Run("sites belongs to someone else", func(t *testing.T) {
		// The API's user owns <log_dir> and could make a sites of its own:
		// here the test's user plays that one and root is somebody else.
		logDir, _ := setup(t)
		if err := os.Mkdir(filepath.Join(logDir, "sites"), 0o711); err != nil {
			t.Fatal(err)
		}
		ids := myIDs()
		ids.rootUID = os.Getuid() + 1
		if err := run(logDir, ids); !errors.Is(err, errNotRootsAlone) {
			t.Fatalf("got %v, want a refusal", err)
		}
		if _, err := os.Stat(filepath.Join(logDir, "sites", "alex")); err == nil {
			t.Error("a directory was made in a tree that is not root's")
		}
	})
	t.Run("account directory writable by its group", func(t *testing.T) {
		logDir, _ := setup(t)
		dir := filepath.Join(logDir, "sites", "alex")
		if err := os.MkdirAll(dir, 0o711); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o771); err != nil {
			t.Fatal(err)
		}
		if err := run(logDir, myIDs()); !errors.Is(err, errNotRootsAlone) {
			t.Fatalf("got %v, want a refusal", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "example.com.access.log")); err == nil {
			t.Error("a log was made in a directory the account could write to")
		}
	})
	t.Run("log is a FIFO", func(t *testing.T) {
		logDir, outside := setup(t)
		if _, err := ensureSiteLogTree(logDir, "alex", myIDs(), nil); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(logDir, "sites", "alex", "ld.so.preload"), 0o644); err != nil {
			t.Skip(err)
		}
		if err := run(logDir, myIDs()); !errors.Is(err, errNotPlainLog) {
			t.Fatalf("got %v, want a refusal", err)
		}
		untouched(t, outside)
	})
}

// The operation takes a login and file names, never a path: anything that
// could leave the account's directory, a system account and root are refused
// before the disk is touched.
func TestEnsureSiteLogsValidates(t *testing.T) {
	logDir := t.TempDir()
	s := plainServer(t, logDir, filepath.Join(t.TempDir(), "www"))
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	cases := []SiteLogsRequest{
		{Login: "root", Files: []string{"x.access.log"}},
		{Login: "../etc", Files: []string{"x.access.log"}},
		{Login: "Alex", Files: []string{"x.access.log"}},
		{Login: me.Username, Files: []string{"../../etc/cron.d/x.log"}},
		{Login: me.Username, Files: []string{"x/y.log"}},
		{Login: me.Username, Files: []string{".x.log"}},
		{Login: me.Username, Files: []string{"ld.so.preload"}},
		{Login: me.Username, Files: []string{""}},
		{Login: "no-such-account-here", Files: []string{"x.access.log"}},
	}
	for _, sys := range []string{"daemon", "bin", "mail"} {
		if u, err := user.Lookup(sys); err == nil && u.Uid != "0" {
			cases = append(cases, SiteLogsRequest{Login: sys, Files: []string{"x.access.log"}})
			break
		}
	}
	for _, req := range cases {
		_, err := s.ensureSiteLogs(context.Background(), &req)
		var ae *Error
		if !errors.As(err, &ae) || (ae.Status != http.StatusBadRequest && ae.Status != http.StatusForbidden) {
			t.Errorf("%+v: %v, want a refusal", req, err)
		}
	}
	if entries, _ := os.ReadDir(logDir); len(entries) != 0 {
		t.Fatalf("a refused request touched the disk: %v", entries)
	}
}

// Purging an account takes its log directory along; a symlink in the tree
// goes itself, never what it points to.
func TestRemoveSiteLogTree(t *testing.T) {
	base := t.TempDir()
	logDir, outside := filepath.Join(base, "log"), filepath.Join(base, "etc")
	for _, d := range []string{logDir, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSiteLogTree(logDir, "alex", myIDs(), []string{"example.com.access.log"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSiteLogTree(logDir, "bob", myIDs(), []string{"bob.example.com.access.log"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(logDir, "sites", "alex")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old", "example.com.access.log.9.gz"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeSiteLogTree(logDir, "alex", os.Getuid()); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "keep")); string(b) != "keep\n" {
		t.Fatal("the removal went through the link")
	}
	if _, err := os.Stat(filepath.Join(logDir, "sites", "bob", "bob.example.com.access.log")); err != nil {
		t.Fatalf("another account's logs went too: %v", err)
	}
	if removed, err := removeSiteLogTree(logDir, "alex", os.Getuid()); err != nil || removed {
		t.Fatalf("a second removal: %v %v", removed, err)
	}

	// sites swapped for a link: refused, the target stays.
	if err := os.Rename(filepath.Join(logDir, "sites"), filepath.Join(base, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(logDir, "sites")); err != nil {
		t.Fatal(err)
	}
	if _, err := removeSiteLogTree(logDir, "bob", os.Getuid()); !errors.Is(err, errSymlinkInPath) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(base, "real", "bob")); err != nil {
		t.Fatalf("the removal went through the link: %v", err)
	}
}

// The API reads the site logs through the agent: below <log_dir> (the API
// user's directory) a symlink on the way or a second name of another file is
// refused, and the accounts' homes are no longer readable at all.
func TestReadFileInLogDir(t *testing.T) {
	base := t.TempDir()
	s := plainServer(t, filepath.Join(base, "log"), filepath.Join(base, "www"))
	cfg := s.cfg
	dir := filepath.Join(cfg.LogDir, "sites", "alex")
	homeLogs := filepath.Join(cfg.WWWRoot, "alex", "data", "logs")
	for _, d := range []string{dir, homeLogs, filepath.Join(base, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(base, "etc", "shadow")
	if err := os.WriteFile(secret, []byte("root:$6$hash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "example.com.access.log"), []byte("GET /\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeLogs, "example.com.access.log"), []byte("GET /old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "etc"), filepath.Join(cfg.LogDir, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(dir, "example.com.error.log")); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	res, err := s.readFile(ctx, &ReadFileRequest{Path: filepath.Join(dir, "example.com.access.log")})
	if err != nil || res.Content != "GET /\n" {
		t.Fatalf("site log: %+v %v", res, err)
	}
	for _, p := range []string{
		filepath.Join(cfg.LogDir, "elsewhere", "shadow"),  // a symlink on the way
		filepath.Join(dir, "example.com.error.log"),       // a second name of the secret
		filepath.Join(homeLogs, "example.com.access.log"), // the old place, in the account's home
		filepath.Join(cfg.LogDir, "sites", "..", "..", "etc", "shadow"),
	} {
		res, err := s.readFile(ctx, &ReadFileRequest{Path: p})
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || res != nil && strings.Contains(res.Content, "hash") {
			t.Errorf("%s: %+v %v, want a refusal", p, res, err)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Stat(secret, &st); err != nil || st.Nlink != 2 {
		t.Fatalf("the test's hard link: %v %d", err, st.Nlink)
	}
}
