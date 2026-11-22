package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
	"monopanel/internal/systemd"
)

// treeHost is an agent whose run and log directories are the test's own,
// 1771 as the package makes them (the test's user stands in for root), and
// a directory outside them for the planted links to point to.
func treeHost(t *testing.T) (s *Server, run, logDir, outside string) {
	t.Helper()
	base := t.TempDir()
	run, logDir, outside = filepath.Join(base, "run"), filepath.Join(base, "log"), filepath.Join(base, "etc")
	for _, d := range []string{run, logDir, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{run, logDir} {
		if err := unix.Chmod(d, 0o1771); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "shadow"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = plainServer(t, logDir, filepath.Join(base, "www"))
	s.cfg.RunDir = run
	return s, run, logDir, outside
}

// outsideUntouched fails when anything behind the planted links changed.
func outsideUntouched(t *testing.T, outside string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(outside, "shadow")); err != nil || string(b) != "keep\n" || perm(t, filepath.Join(outside, "shadow")) != 0o600 {
		t.Errorf("the file behind the link was changed: %q %v", b, err)
	}
	if perm(t, outside) != 0o755 {
		t.Errorf("the directory behind the link was changed: %#o", perm(t, outside))
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("something was made behind the link: %v", entries)
	}
}

func gone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
}

func kind(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return st.Mode().Type()
}

// quarantined is the quarantine the agent made in dir: there is one, root's
// alone (0700), or none when want is empty; want are the names in it.
func quarantined(t *testing.T, dir string, want ...string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, quarantinePrefix+"*"))
	if len(want) == 0 {
		if len(matches) != 0 {
			t.Errorf("a quarantine where nothing was moved: %v", matches)
		}
		return ""
	}
	if len(matches) != 1 {
		t.Fatalf("quarantines in %s: %v", dir, matches)
	}
	q := matches[0]
	if perm(t, q) != 0o700 {
		t.Errorf("quarantine %s: mode %#o", q, perm(t, q))
	}
	entries, err := os.ReadDir(q)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("quarantine holds %v, want %v", names, want)
	}
	return q
}

// setDefaultACL gives dir a default ACL for the test's user, when setfacl
// is there and the file system takes ACLs; ok tells whether it did.
func setDefaultACL(t *testing.T, dir string) bool {
	t.Helper()
	bin, err := exec.LookPath("setfacl")
	if err != nil {
		return false
	}
	return exec.Command(bin, "-d", "-m", "u:"+strconv.Itoa(os.Getuid())+":rwx", dir).Run() == nil
}

func hasXattr(p, name string) bool {
	n, err := unix.Getxattr(p, name, nil)
	return err == nil && n > 0
}

// At its start the agent makes <run_dir>/php root's alone before the API
// runs: it makes it, or takes over one somebody else made — a symlink in
// its place goes itself, a directory others may write to gets root's mode
// and no ACL — and clears it of all but the sockets: what a symlink there
// points to is never touched, a directory goes to a quarantine of root's
// alone. The check before php-fpm acts refuses what the start would fix.
func TestPHPSocketDirAtStart(t *testing.T) {
	ctx := context.Background()
	t.Run("missing", func(t *testing.T) {
		s, run, _, _ := treeHost(t)
		if err := s.preparePHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
		php := filepath.Join(run, "php")
		if kind(t, php) != os.ModeDir || perm(t, php) != 0o755 {
			t.Fatalf("php/: %v %#o", kind(t, php), perm(t, php))
		}
		if err := s.checkPHPSocketDir(ctx); err != nil {
			t.Fatalf("the agent's own: %v", err)
		}
		quarantined(t, run)
	})
	t.Run("a symlink in its place", func(t *testing.T) {
		s, run, _, outside := treeHost(t)
		php := filepath.Join(run, "php")
		if err := os.Symlink(outside, php); err != nil {
			t.Fatal(err)
		}
		if err := s.checkPHPSocketDir(ctx); !errors.Is(err, errSymlinkInPath) {
			t.Fatalf("checked before the start: %v", err)
		}
		if err := s.preparePHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
		if kind(t, php) != os.ModeDir || perm(t, php) != 0o755 {
			t.Fatalf("php/: %v %#o", kind(t, php), perm(t, php))
		}
		outsideUntouched(t, outside)
		if err := s.checkPHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("someone may write to it", func(t *testing.T) {
		s, run, _, outside := treeHost(t)
		php := filepath.Join(run, "php")
		if err := os.Mkdir(php, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(php, 0o777); err != nil {
			t.Fatal(err)
		}
		acl := setDefaultACL(t, php)
		sock := filepath.Join(php, "example.com.sock")
		if err := unix.Mknod(sock, unix.S_IFSOCK|0o660, 0); err != nil {
			t.Fatal(err)
		}
		// Planted while others could write there: a link under a pool's
		// socket name, a file, a FIFO, and a directory under one.
		if err := os.Symlink(filepath.Join(outside, "shadow"), filepath.Join(php, "shop.example.com.sock")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(php, "file"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(php, "fifo"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(php, "x.sock", "deeper"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(php, "x.sock", "deeper", "link")); err != nil {
			t.Fatal(err)
		}
		if err := s.checkPHPSocketDir(ctx); !errors.Is(err, errNotRootsAlone) {
			t.Fatalf("checked before the start: %v", err)
		}
		if err := s.preparePHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
		if perm(t, php) != 0o755 {
			t.Errorf("php/: mode %#o", perm(t, php))
		}
		if acl && hasXattr(php, "system.posix_acl_default") {
			t.Error("the default ACL stayed: the old owner would get every socket made there")
		}
		if kind(t, sock) != os.ModeSocket {
			t.Errorf("the pool's socket: %v", kind(t, sock))
		}
		gone(t, filepath.Join(php, "shop.example.com.sock"), filepath.Join(php, "file"), filepath.Join(php, "fifo"), filepath.Join(php, "x.sock"))
		q := quarantined(t, run, "1-x.sock")
		if kind(t, filepath.Join(q, "1-x.sock", "deeper", "link")) != os.ModeSymlink {
			t.Error("the quarantined directory was not moved whole")
		}
		outsideUntouched(t, outside)
		if err := s.checkPHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
		// The next start finds nothing to do.
		if err := s.preparePHPSocketDir(ctx); err != nil {
			t.Fatal(err)
		}
		quarantined(t, run, "1-x.sock")
		if kind(t, sock) != os.ModeSocket {
			t.Error("the pool's socket went at the second start")
		}
	})
	t.Run("someone else's", func(t *testing.T) {
		// The API's user made it: the test's user plays that one, and root
		// is somebody else.
		_, run, _, _ := treeHost(t)
		php := filepath.Join(run, "php")
		if err := os.Mkdir(php, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(php, "keep"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(run, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		d := treeDir{rootUID: os.Geteuid() + 1, gid: os.Getegid(), mode: phpSocketDirMode}
		if _, _, _, err := openTreeDir(fd, run, "php", d, true, false); !errors.Is(err, errNotRootsAlone) {
			t.Fatalf("the check: %v", err)
		}
		if os.Geteuid() != 0 {
			// Only root gives a directory away, as the agent's start does.
			if _, _, took, err := openTreeDir(fd, run, "php", d, true, true); !took || !errors.Is(err, unix.EPERM) {
				t.Fatalf("the takeover: %v %v", took, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(php, "keep")); err != nil {
			t.Error("a directory the agent could not take over was cleared")
		}
	})
}

// Until the chown, the old owner of a directory being taken over may rename
// it away and put another under its name: the agent notices.
func TestTakeOverNoticesARename(t *testing.T) {
	base := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(base, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, "a", dirFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	// "a" was opened, then moved away; "b" stands under the name now.
	err = takeOverDir(parent, fd, "b", filepath.Join(base, "b"), treeDir{rootUID: os.Geteuid(), gid: os.Getegid(), mode: 0o755})
	if !errors.Is(err, errMovedAway) {
		t.Fatalf("got %v", err)
	}
}

// A pool's socket is its account's or root's: one of the service user's in
// the socket directory is an API process's, listening under a site's name,
// and goes; the others stay. (The test's user plays the service user.)
func TestPHPSocketSweepDropsTheAPIsSockets(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "example.com.sock")
	if err := unix.Mknod(sock, unix.S_IFSOCK|0o660, 0); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	q := newQuarantine(fd, dir, os.Geteuid())
	defer q.close()
	r := &treeSweep{}
	if err := sweepPHPSocketDir(fd, dir, -1, q, r); err != nil || !r.empty() {
		t.Fatalf("a pool's socket: %v %+v", err, r)
	}
	if err := sweepPHPSocketDir(fd, dir, os.Getuid(), q, r); err != nil || len(r.removed) != 1 {
		t.Fatalf("the service user's socket: %v %+v", err, r)
	}
	gone(t, sock)
}

// At its start the agent takes over a site log tree someone besides root
// could write to — up to 0.8.19 the log directory was the API's user's, who
// could make sites/ — and clears it of what root does not put there, while
// the logs stay: nothing a symlink points to is touched, a file with a
// second name loses this one unless it is the account's own, and a
// directory in an account's goes to the quarantine. Before that, nginx and
// its kin are refused.
func TestSiteLogTreeAtStart(t *testing.T) {
	ctx := context.Background()
	s, _, logDir, outside := treeHost(t)
	base := filepath.Dir(logDir)
	sites := filepath.Join(logDir, "sites")
	if err := os.Mkdir(sites, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sites, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sites, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sites, "evil")); err != nil {
		t.Fatal(err)
	}
	// A directory of no account, as its old owner left it.
	stranger := filepath.Join(sites, "nosuch-h14")
	if err := os.Mkdir(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stranger, 0o777); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(stranger, "example.com.access.log")
	if err := os.WriteFile(log, []byte("GET /\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "shadow"), filepath.Join(stranger, "example.com.error.log")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(stranger, "example.com.php.slow.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stranger, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stranger, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "other.log")
	if err := os.WriteFile(other, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, filepath.Join(stranger, "linked.access.log")); err != nil {
		t.Fatal(err)
	}
	// The directory of the test's own account, when it can be one: its PHP
	// error log is its own and may have a second name. A log root opens may
	// not, the account's own file or not: nginx even hands it to its worker
	// user by name when it reopens it.
	var own string
	if me, err := user.Current(); err == nil && nameRe.MatchString(me.Username) && os.Getuid() >= uidMin() {
		own = filepath.Join(sites, me.Username)
		if err := os.Mkdir(own, 0o777); err != nil {
			t.Fatal(err)
		}
		mine := filepath.Join(base, "mine.php.error.log")
		if err := os.WriteFile(mine, nil, 0o660); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"example.org.php.error.log", "example.org.access.log"} {
			if err := os.Link(mine, filepath.Join(own, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.vetRootDaemonTrees(ctx, []string{"nginx"}, nil); agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), "not root's alone: "+sites+" ") || !strings.Contains(err.Error(), "restart monopanel-agent") {
		t.Fatalf("nginx before the start: %v", err)
	}

	if err := s.prepareSiteLogTree(ctx); err != nil {
		t.Fatal(err)
	}
	if perm(t, sites) != 0o711 || perm(t, stranger) != 0o751 {
		t.Errorf("modes: sites %#o, account %#o", perm(t, sites), perm(t, stranger))
	}
	if b, err := os.ReadFile(log); err != nil || string(b) != "GET /\n" {
		t.Errorf("the log: %q %v", b, err)
	}
	gone(t, filepath.Join(sites, "stray"), filepath.Join(sites, "evil"), filepath.Join(stranger, "example.com.error.log"),
		filepath.Join(stranger, "example.com.php.slow.log"), filepath.Join(stranger, "linked.access.log"), filepath.Join(stranger, "sub"))
	if b, err := os.ReadFile(other); err != nil || string(b) != "other\n" {
		t.Errorf("the second name's other file: %q %v", b, err)
	}
	q := quarantined(t, logDir, "1-sub")
	if kind(t, filepath.Join(q, "1-sub", "link")) != os.ModeSymlink {
		t.Error("the quarantined directory was not moved whole")
	}
	outsideUntouched(t, outside)
	if own != "" {
		if perm(t, own) != 0o751 {
			t.Errorf("the account's directory: %#o", perm(t, own))
		}
		if _, err := os.Lstat(filepath.Join(own, "example.org.php.error.log")); err != nil {
			t.Errorf("the account's own PHP error log: %v", err)
		}
		gone(t, filepath.Join(own, "example.org.access.log"))
	}
	if err := s.vetRootDaemonTrees(ctx, []string{"nginx"}, nil); err != nil {
		t.Fatalf("nginx after the start: %v", err)
	}
	// The next start finds nothing to do.
	if err := s.prepareSiteLogTree(ctx); err != nil {
		t.Fatal(err)
	}
	quarantined(t, logDir, "1-sub")
	if _, err := os.Lstat(log); err != nil {
		t.Error("the log went at the second start")
	}
}

// php-fpm, nginx and Apache act as root by name in <run_dir>/php and
// <log_dir>/sites: while the tree one of them uses is not root's alone,
// the agent starts, restarts and reloads none of them and runs none of
// their checks that open the logs — refused before anything is written or
// removed. Missing trees are made; other units and checks go on.
func TestRootDaemonTreesBeforeActing(t *testing.T) {
	ctx := context.Background()
	s, run, logDir, _ := treeHost(t)
	allowed := filepath.Join(filepath.Dir(run), "conf")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.Agent.ExtraWritePrefixes = []string{allowed + "/"}
	php, sites := filepath.Join(run, "php"), filepath.Join(logDir, "sites")
	nginxT := s.profile.Web().NginxCheckArgv
	fpmT := osprofile.PHP(s.profile, "8.4").FPMCheckArgv
	refused := func(t *testing.T, err error, path string) {
		t.Helper()
		if agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "restart monopanel-agent") {
			t.Errorf("got %v, want a refusal naming %s", err, path)
		}
	}
	// /v1/pkg on a host without a package manager, which answers 501 past
	// the checks: no apt or dnf runs here, refused or not.
	pkg := func(action, name string) error {
		real := s.profile
		s.profile = osprofile.Generic(real.Release())
		defer func() { s.profile = real }()
		_, err := s.pkg(ctx, &PkgRequest{Action: action, Packages: []string{name}})
		return err
	}

	if err := s.vetRootDaemonTrees(ctx, []string{"php8.4-fpm"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := pkg("install", "php8.4-fpm"); agentStatus(err) != http.StatusNotImplemented {
		t.Errorf("a package with the trees root's: %v", err)
	}
	for _, d := range []string{php, sites} {
		if kind(t, d) != os.ModeDir || perm(t, d)&0o022 != 0 {
			t.Errorf("%s was not made root's alone: %#o", d, perm(t, d))
		}
	}

	t.Run("php/ others may write to", func(t *testing.T) {
		if err := os.Chmod(php, 0o777); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(php, 0o755)
		for _, u := range []string{"php8.4-fpm", "php8.4-fpm.service", "php84-php-fpm.service"} {
			refused(t, s.vetRootDaemonTrees(ctx, []string{"nginx", u}, nil), php)
		}
		// The logs are fine, and php-fpm -t binds no socket.
		if err := s.vetRootDaemonTrees(ctx, []string{"nginx", "monopanel-apache@alex", "mysql"}, [][]string{nginxT, fpmT}); err != nil {
			t.Errorf("nginx: %v", err)
		}
		conf := filepath.Join(allowed, "pool.conf")
		_, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: conf, Content: "x"}}, Reload: []string{"php8.4-fpm.service"}})
		refused(t, err, php)
		gone(t, conf)
		_, err = s.applyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: conf, Content: "x"}}, Restart: []string{"php84-php-fpm.service"}})
		refused(t, err, php)
		_, err = s.service(ctx, &ServiceRequest{Unit: "php8.4-fpm.service", Action: "restart"})
		refused(t, err, php)
		_, err = s.service(ctx, &ServiceRequest{Unit: "php8.4-fpm.service", Action: "start"})
		refused(t, err, php)
		// php8.4-fpm's postinst starts it, and any php8.4-* package's
		// trigger restarts it: refused before the package manager runs.
		for _, action := range []string{"install", "remove"} {
			refused(t, pkg(action, "php8.4-fpm"), php)
		}
	})

	t.Run("sites/ others may write to", func(t *testing.T) {
		if err := os.Chmod(sites, 0o777); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(sites, 0o711)
		for _, c := range []struct {
			units  []string
			checks [][]string
		}{
			{units: []string{"nginx"}},
			{units: []string{"nginx-debug.service"}},
			{units: []string{"apache2"}},
			{units: []string{"httpd.service"}},
			{units: []string{"monopanel-apache@alex"}},
			{units: []string{"php8.4-fpm"}},
			{checks: [][]string{nginxT}},
			{checks: [][]string{{"/usr/sbin/sshd", "-t"}, fpmT}},
		} {
			refused(t, s.vetRootDaemonTrees(ctx, c.units, c.checks), sites)
		}
		if err := s.vetRootDaemonTrees(ctx, []string{"mysql", "postfix.service", "sshd"}, [][]string{{"/usr/sbin/sshd", "-t"}}); err != nil {
			t.Errorf("units that open no site logs: %v", err)
		}
		conf := filepath.Join(allowed, "site.conf")
		_, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: conf, Content: "x"}}, Validate: [][]string{nginxT}})
		refused(t, err, sites)
		gone(t, conf)
		old := filepath.Join(allowed, "old.conf")
		if err := os.WriteFile(old, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = s.removePaths(ctx, &RemovePathsRequest{Paths: []string{old}, Reload: []string{"nginx"}})
		refused(t, err, sites)
		if _, err := os.Lstat(old); err != nil {
			t.Error("removed although the reload was refused")
		}
		_, err = s.service(ctx, &ServiceRequest{Unit: "nginx", Action: "reload"})
		refused(t, err, sites)
		_, err = s.service(ctx, &ServiceRequest{Unit: "monopanel-apache@alex", Action: "reload-or-restart"})
		refused(t, err, sites)
		// needrestart restarts nginx after any package that changed a
		// library of its own.
		refused(t, pkg("install", "mc"), sites)
		// Reading the index and the database runs no scripts.
		for _, action := range []string{"update-index", "query", "available"} {
			if err := pkg(action, "mc"); agentStatus(err) == http.StatusForbidden {
				t.Errorf("%s: %v", action, err)
			}
		}
	})

	// Without the sticky bit the API's group could rename sites/ away and
	// put a tree of its own there.
	t.Run("the log directory without the sticky bit", func(t *testing.T) {
		if err := unix.Chmod(logDir, 0o771); err != nil {
			t.Fatal(err)
		}
		defer unix.Chmod(logDir, 0o1771)
		err := s.vetRootDaemonTrees(ctx, []string{"nginx"}, nil)
		refused(t, err, sites)
		if err == nil || !strings.Contains(err.Error(), "not root's alone: "+logDir+" ") {
			t.Errorf("got %v, want the log directory named", err)
		}
	})

	t.Run("sites/ a symlink", func(t *testing.T) {
		if err := os.Rename(sites, sites+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sites+".old", sites); err != nil {
			t.Fatal(err)
		}
		refused(t, s.vetRootDaemonTrees(ctx, []string{"nginx"}, nil), sites)
	})
}

// Which units' root masters act on which tree.
func TestRootDaemonTrees(t *testing.T) {
	for _, c := range []struct {
		unit      string
		php, logs bool
	}{
		{"php8.4-fpm.service", true, true},
		{"php5.6-fpm.service", true, true},
		{"php84-php-fpm.service", true, true},
		{"php-fpm.service", true, true},
		{"nginx.service", false, true},
		{"nginx-debug.service", false, true},
		{"apache2.service", false, true},
		{"httpd.service", false, true},
		{"monopanel-apache@alex.service", false, true},
		{"mysql.service", false, false},
		{"monopanel-api.service", false, false},
		{"phpsessionclean.service", false, false},
		{"nginx.socket", false, false},
		{"php8.4-fpm-reload.timer", false, false},
		// what systemctl reads as php8.4-fpm.service (the agent refuses the
		// name before it gets here: it has no type)
		{"php8.4-fpm", true, true},
	} {
		if php, logs := rootDaemonTrees(c.unit); php != c.php || logs != c.logs {
			t.Errorf("%s: php %v logs %v, want %v %v", c.unit, php, logs, c.php, c.logs)
		}
	}
}

// The API writes nothing for systemd-tmpfiles any more: the php-fpm socket
// directory comes with the package and mp setup, and a file of its own in
// /etc/tmpfiles.d could otherwise take the package's place.
func TestTmpfilesAreNotTheAPIs(t *testing.T) {
	s := plainServer(t, t.TempDir(), t.TempDir())
	for _, p := range []string{"/etc/tmpfiles.d/monopanel-php.conf", "/etc/tmpfiles.d/monopanel.conf", "/etc/tmpfiles.d/x.conf"} {
		if s.pathAllowed(p) || s.dirAllowed(p) {
			t.Errorf("%s is on the allow-list", p)
		}
	}
	_, err := s.applyConfigSet(context.Background(), &ApplyConfigSetRequest{Files: []FileSpec{{Path: "/etc/tmpfiles.d/monopanel-php.conf", Content: "d /run/monopanel/php 0755 root root -\n"}}})
	if agentStatus(err) != http.StatusForbidden {
		t.Fatalf("config/apply: %v", err)
	}
	_, err = s.removePaths(context.Background(), &RemovePathsRequest{Paths: []string{"/etc/tmpfiles.d/monopanel-php.conf"}})
	if agentStatus(err) != http.StatusForbidden {
		t.Fatalf("paths/remove: %v", err)
	}
}

// When clearing the site log tree at the start does not finish — the
// quarantine cannot be made (a full disk; here a log directory its owner may
// not write to) — sites/ is root's alone already, but what its old owner made
// may still be inside: here a directory with a link in an account's. So
// nginx and its kin are refused until clearing succeeds; each check tries
// it again.
func TestSiteLogSweepFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory without the write bit")
	}
	ctx := context.Background()
	s, _, logDir, outside := treeHost(t)
	t.Cleanup(func() { unix.Chmod(logDir, 0o1771) }) //nolint:errcheck // the temporary directory goes next
	sites := filepath.Join(logDir, "sites")
	acct := filepath.Join(sites, "nosuch-h14")
	sub := filepath.Join(acct, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{sites, acct, sub} {
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(sub, "example.com.access.log")
	if err := os.Symlink(filepath.Join(outside, "shadow"), link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(logDir, 0o1551); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareSiteLogTree(ctx); err == nil {
		t.Fatal("the tree was cleared without a quarantine")
	}
	if perm(t, sites) != 0o711 || perm(t, acct) != 0o751 {
		t.Fatalf("not taken over: sites %#o, account %#o", perm(t, sites), perm(t, acct))
	}
	if kind(t, link) != os.ModeSymlink {
		t.Fatal("the link went")
	}
	for _, c := range []struct {
		units  []string
		checks [][]string
	}{
		{units: []string{"nginx.service"}},
		{units: []string{"php8.4-fpm.service"}},
		{units: []string{"monopanel-apache@alex.service"}},
		{checks: [][]string{s.profile.Web().NginxCheckArgv}},
	} {
		if err := s.vetRootDaemonTrees(ctx, c.units, c.checks); agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), "clearing") {
			t.Errorf("%v %v after clearing failed: %v", c.units, c.checks, err)
		}
	}
	// Units that open no site logs go on.
	if err := s.vetRootDaemonTrees(ctx, []string{"mysql.service"}, [][]string{{"/usr/sbin/sshd", "-t"}}); err != nil {
		t.Errorf("mysql: %v", err)
	}
	// The cause gone, the next check clears the tree and lets nginx through.
	if err := unix.Chmod(logDir, 0o1771); err != nil {
		t.Fatal(err)
	}
	if err := s.vetRootDaemonTrees(ctx, []string{"nginx.service"}, nil); err != nil {
		t.Fatalf("nginx once the tree is cleared: %v", err)
	}
	gone(t, sub)
	quarantined(t, logDir, "1-sub")
	outsideUntouched(t, outside)
}

// dnf install, the mail's packages from CodeReady Builder, is the package
// manager as well: the scriptlets of what it upgrades on the way restart
// php-fpm and nginx. Checked when it is about to run, not before. It runs as
// /v1/pkg's install does, in a unit of its own (pkg_unit.go), with the
// caller's name for the run; its exit status and output come back.
func TestDnfInstallVetsTheTrees(t *testing.T) {
	ctx := context.Background()
	s, run, logDir, _ := treeHost(t)
	rel, err := osprofile.ParseOSRelease(strings.NewReader(alma9))
	if err != nil {
		t.Fatal(err)
	}
	if s.profile, err = osprofile.FromRelease(rel); err != nil {
		t.Fatal(err)
	}
	units := withUnits(t, s)
	units.play = func(systemd.Oneshot) (string, string, systemd.Exit) {
		return "Error: Unable to find a match: dovecot\n", "failed", systemd.Exit{Code: 1, Status: 1, Result: "exit-code"}
	}
	php := filepath.Join(run, "php")
	for _, d := range []string{php, filepath.Join(logDir, "sites")} {
		if err := os.Mkdir(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(php, 0o777); err != nil {
		t.Fatal(err)
	}
	call := tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "dovecot")
	call.run = testRun
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if plan.wrap == nil {
		t.Fatal("dnf install runs without the check")
	}
	ran := false
	dnf := func([]string, io.Writer) (int, error) { ran = true; return 0, nil }
	if _, err := plan.wrap(ctx, dnf, io.Discard); agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), php) || ran || len(units.started) != 0 {
		t.Fatalf("php/ others may write to: %v, dnf ran: %v %v", err, ran, units.started)
	}
	if err := os.Chmod(php, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	code, err := plan.wrap(ctx, dnf, &out)
	if err != nil || ran || len(units.started) != 1 {
		t.Fatalf("the trees root's: %v, dnf ran as the agent's child: %v, units %v", err, ran, units.started)
	}
	o := units.started[0]
	if o.Unit != pkgUnit(testRun) || o.Path != "/usr/bin/dnf" || !slices.Equal(o.Argv, []string{"dnf", "-y", "install", "--enablerepo=crb", "dovecot"}) || !slices.Equal(o.Env, s.profile.Packages().Env()) {
		t.Fatalf("the run: %+v", o)
	}
	if code != 1 || !strings.Contains(out.String(), "Unable to find a match") || !strings.Contains(out.String(), "exit status 1") {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
	bad := tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "dovecot")
	bad.run = "../x"
	if _, err := s.planTool(&bad); agentStatus(err) != http.StatusBadRequest {
		t.Fatalf("a bad run name: %v", err)
	}
}

// One entry that cannot be cleared stops nothing else: with the quarantine
// out of reach, every account's directory is still taken over and the links
// in it go — only the directories that needed the quarantine stay, and the
// start reports them.
func TestSiteLogSweepGoesOnAfterAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory without the write bit")
	}
	ctx := context.Background()
	s, _, logDir, outside := treeHost(t)
	t.Cleanup(func() { unix.Chmod(logDir, 0o1771) }) //nolint:errcheck // the temporary directory goes next
	sites := filepath.Join(logDir, "sites")
	if err := os.Mkdir(sites, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sites, 0o777); err != nil {
		t.Fatal(err)
	}
	var accts []string
	for _, n := range []string{"nosuch-a", "nosuch-b", "nosuch-c", "nosuch-d", "nosuch-e"} {
		acct := filepath.Join(sites, n)
		accts = append(accts, acct)
		if err := os.MkdirAll(filepath.Join(acct, "sub"), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(acct, 0o777); err != nil {
			t.Fatal(err)
		}
		for _, l := range []string{"a.access.log", "z.error.log"} {
			if err := os.Symlink(filepath.Join(outside, "shadow"), filepath.Join(acct, l)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := unix.Chmod(logDir, 0o1551); err != nil {
		t.Fatal(err)
	}
	err := s.prepareSiteLogTree(ctx)
	if err == nil || !strings.Contains(err.Error(), string(filepath.Separator)+"sub stays") {
		t.Errorf("the directories that stay are not reported: %v", err)
	}
	for _, acct := range accts {
		if perm(t, acct) != 0o751 {
			t.Errorf("%s not taken over: %#o", acct, perm(t, acct))
		}
		gone(t, filepath.Join(acct, "a.access.log"), filepath.Join(acct, "z.error.log"))
	}
	outsideUntouched(t, outside)
}

// postinstall runs systemd-tmpfiles before it restarts the agent, and its d
// line gives an API-made php/ to root, owner and mode only: the agent finds
// it root's alone, and drops the ACL the old owner may have left all the
// same — a default one hands whoever it names every pool's socket bound
// there later.
func TestPHPSocketDirLosesItsACL(t *testing.T) {
	ctx := context.Background()
	s, run, _, _ := treeHost(t)
	php := filepath.Join(run, "php")
	if err := os.Mkdir(php, 0o755); err != nil {
		t.Fatal(err)
	}
	if !setDefaultACL(t, php) || !hasXattr(php, "system.posix_acl_default") {
		t.Skip("no setfacl, or no ACLs on this file system")
	}
	if err := s.checkPHPSocketDir(ctx); err != nil {
		t.Fatalf("root's alone, ACL or not: %v", err)
	}
	if err := s.preparePHPSocketDir(ctx); err != nil {
		t.Fatal(err)
	}
	if hasXattr(php, "system.posix_acl_default") || hasXattr(php, "system.posix_acl_access") {
		t.Error("the ACL stayed")
	}
	if perm(t, php) != 0o755 {
		t.Errorf("php/: %#o", perm(t, php))
	}
}
