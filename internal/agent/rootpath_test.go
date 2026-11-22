package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

// Which directories root goes through: root's and the system's own, not the
// API's or an account's, nothing others may rearrange (but sticky ones), a
// group's only where the distribution makes it so.
func TestDirTrustRules(t *testing.T) {
	tr := &dirTrust{rootUID: 0, serviceUser: "daemon", strict: true} // daemon (uid 2) plays the API's user
	mailGID := -1
	if g, err := user.LookupGroup("mail"); err == nil {
		mailGID = atoiOr(g.Gid, -1)
	}
	for _, c := range []struct {
		why  string
		dir  string
		uid  uint32
		gid  uint32
		mode uint32
		ok   bool
	}{
		{"root's", "/etc/nginx", 0, 0, 0o755, true},
		{"a system account's", "/var/lib/sphinx", 1, 1, 0o750, true},
		{"the API's", "/var/lib/monopanel", 2, 2, 0o751, false},
		{"an account's", "/var/www/alex", 1500, 1500, 0o750, false},
		{"writable by everyone", "/etc/nginx", 0, 0, 0o777, false},
		{"sticky and writable by everyone", "/tmp", 0, 0, 0o1777, true},
		{"writable by root's group", "/etc/x", 0, 0, 0o775, true},
		{"writable by another group", "/etc/nginx", 0, 1500, 0o775, false},
		{"sticky and writable by a group", "/run/monopanel", 0, 1500, 0o1771, true},
	} {
		st := &unix.Stat_t{Uid: c.uid, Gid: c.gid, Mode: unix.S_IFDIR | c.mode}
		if err := tr.check(c.dir, st, false); (err == nil) != c.ok {
			t.Errorf("%s: %v, want trusted=%v", c.why, err, c.ok)
		}
	}
	// root:root 0775 with an ACL: the group bits are the mask, and a named
	// entry may be the API's.
	if err := tr.check("/etc/x", &unix.Stat_t{Uid: 0, Gid: 0, Mode: unix.S_IFDIR | 0o775}, true); err == nil {
		t.Error("a group-writable mask of an ACL")
	}
	if err := tr.check("/etc/x", &unix.Stat_t{Uid: 0, Gid: 0, Mode: unix.S_IFDIR | 0o755}, true); err != nil {
		t.Errorf("an ACL whose mask lets nobody write: %v", err)
	}
	if err := tr.check("/run/monopanel", &unix.Stat_t{Uid: 0, Gid: 0, Mode: unix.S_IFDIR | 0o1775}, true); err != nil {
		t.Errorf("a sticky directory with an ACL: %v", err)
	}
	if mailGID > 0 {
		st := &unix.Stat_t{Uid: 0, Gid: uint32(mailGID), Mode: unix.S_IFDIR | 0o2775}
		if err := tr.check("/var/mail", st, false); err != nil {
			t.Errorf("Debian's mail spool: %v", err)
		}
		if err := tr.check("/etc/nginx", st, false); err == nil {
			t.Error("a directory the mail group may write to outside the mail spool")
		}
	}
}

func atoiOr(s string, def int) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// A symlink on the way is followed only when root made it in a directory
// nobody else may write to; the walk reports where it really went.
func TestWalkDirSymlinks(t *testing.T) {
	me := os.Getuid()
	base := t.TempDir()
	real := filepath.Join(base, "real", "dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real/dir", filepath.Join(base, "real", "up")); err != nil {
		t.Fatal(err)
	}
	trusted := &dirTrust{rootUID: me, strict: true}
	for _, p := range []string{"rel/dir", "abs/dir", "real/up"} {
		w, err := walkDir("/", filepath.Join(base, p), walkOpts{trust: trusted, follow: followAll, uid: -1, gid: -1})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if w.resolved != real {
			t.Errorf("%s resolved to %s, want %s", p, w.resolved, real)
		}
		w.close()
	}
	refused := func(p string, o walkOpts) {
		t.Helper()
		w, err := walkDir("/", filepath.Join(base, p), o)
		if err == nil {
			w.close()
			t.Fatalf("%s: followed", p)
		}
		if !errors.Is(err, errSymlinkInPath) && !errors.Is(err, errUntrusted) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	// No trust rules (the web root): no symlink at all.
	refused("rel/dir", walkOpts{uid: -1, gid: -1})
	// Somebody else's symlink.
	refused("rel/dir", walkOpts{trust: &dirTrust{rootUID: me + 1}, follow: followAll, uid: -1, gid: -1})
	// A symlink in a directory others may write to: they could have made it.
	if err := os.Chmod(base, 0o775); err != nil {
		t.Fatal(err)
	}
	refused("rel/dir", walkOpts{trust: &dirTrust{rootUID: me}, follow: followAll, uid: -1, gid: -1})
	// Strictly, such a directory is not gone through at all.
	refused("real/dir", walkOpts{trust: trusted, follow: followAll, uid: -1, gid: -1})
	if err := os.Chmod(base, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if w, err := walkDir("/", real, walkOpts{trust: trusted, follow: followAll, uid: -1, gid: -1}); err != nil {
		t.Errorf("a sticky directory: %v", err)
	} else {
		w.close()
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// A loop ends.
	os.Symlink("loop2", filepath.Join(base, "loop1")) //nolint:errcheck // test setup
	os.Symlink("loop1", filepath.Join(base, "loop2")) //nolint:errcheck // test setup
	if w, err := walkDir("/", filepath.Join(base, "loop1", "x"), walkOpts{trust: trusted, follow: followAll, uid: -1, gid: -1}); err == nil || !errors.Is(err, unix.ELOOP) {
		w.close()
		t.Errorf("a symlink loop: %v", err)
	}
	// Missing levels are made, with the owner.
	w, err := walkDir("/", filepath.Join(base, "made", "a", "b"), walkOpts{trust: trusted, follow: followAll, create: true, mode: 0o750, uid: me, gid: -1})
	if err != nil {
		t.Fatal(err)
	}
	w.close()
	if len(w.made) != 3 || w.made[0] != filepath.Join(base, "made") {
		t.Errorf("made %v", w.made)
	}
	if st, err := os.Stat(filepath.Join(base, "made", "a", "b")); err != nil || !st.IsDir() {
		t.Errorf("not made: %v", err)
	}
}

// The trees only root manages stay closed whatever the allow-list says.
func TestRootTreesWhateverTheAllowList(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.ExtraWritePrefixes = []string{"/etc/", "/var/spool/", "/lib/", "/"}
	s := &Server{cfg: cfg}
	for p, want := range map[string]bool{
		"/etc/systemd/system/x.service":           false,
		"/etc/systemd/system.conf.d/x.conf":       false,
		"/run/systemd/system/x.service":           false,
		"/lib/systemd/system-generators/x":        false,
		"/etc/cron.d/x":                           false,
		"/var/spool/cron/crontabs/root":           false,
		"/etc/sudoers.d/x":                        false,
		"/etc/ld.so.preload":                      false,
		"/etc/shadow":                             false,
		"/root/.ssh/authorized_keys":              false,
		"/var/lib/monopanel-agent/confhistory/x":  false,
		"/etc":                                    false, // above them
		"/etc/nginx/monopanel/sites/example.conf": true,
	} {
		if got := s.pathAllowed(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

// What config/apply may give a file: root's (or opendkim's keys, dovecot's
// users file), never writable by group or others, no set-id bit.
func TestCheckFileSpec(t *testing.T) {
	for _, c := range []struct {
		f  FileSpec
		ok bool
	}{
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o644}, true},
		{FileSpec{Path: "/etc/monopanel/apps/x.env", Mode: 0o600, Owner: "root", Group: "root"}, true},
		{FileSpec{Path: "/etc/opendkim/keys/example.com/mail.private", Mode: 0o600, Owner: "opendkim", Group: "opendkim"}, true},
		{FileSpec{Path: "/etc/dovecot/monopanel/users", Mode: 0o640, Owner: "root", Group: "dovecot"}, true},
		{FileSpec{Path: "/usr/local/bin/composer", Mode: 0o755}, true},
		{FileSpec{Path: "/usr/local/bin/composer", Mode: 0o4755}, false},
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o666}, false},
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o664}, false},
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o644, Owner: "monopanel"}, false},
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o644, Group: "monopanel"}, false},
		{FileSpec{Path: "/etc/nginx/nginx.conf", Mode: 0o644, Owner: "opendkim"}, false},
		{FileSpec{Path: "/etc/postfix/main.cf", Mode: 0o644, Group: "dovecot"}, false},
	} {
		if err := checkFileSpec(&c.f); (err == nil) != c.ok {
			t.Errorf("%+v: %v, want ok=%v", c.f, err, c.ok)
		}
	}
}

// What dirs/ensure may give a directory outside the web root.
func TestCheckDirSpec(t *testing.T) {
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"))
	p, _ := osprofile.FromRelease(rel)
	s := &Server{cfg: config.Default(), profile: p}
	for _, c := range []struct {
		d  DirSpec
		ok bool
	}{
		{DirSpec{Path: "/run/monopanel/php", Mode: 0o755, Owner: "root", Group: "root"}, true},
		{DirSpec{Path: "/var/mail/monopanel", Mode: 0o750, Owner: "vmail", Group: "vmail"}, true},
		{DirSpec{Path: "/var/mail/monopanel/example.com", Mode: 0o750, Owner: "vmail", Group: "vmail"}, true},
		{DirSpec{Path: "/etc/opendkim/keys", Mode: 0o750, Owner: "root", Group: "opendkim"}, true},
		{DirSpec{Path: "/var/log/mysql", Mode: 0o750, Owner: "mysql", Group: "mysql"}, true},
		{DirSpec{Path: "/var/lib/sphinx", Mode: 0o750, Owner: "sphinx", Group: "sphinx"}, true},
		{DirSpec{Path: "/opt/monopanel/sphinx", Mode: 0o755, Owner: "root", Group: "root"}, true},
		{DirSpec{Path: "/etc/nginx/monopanel", Mode: 0o755, Owner: "monopanel"}, false},
		{DirSpec{Path: "/etc/nginx/monopanel", Mode: 0o777}, false},
		{DirSpec{Path: "/etc/nginx/monopanel", Mode: 0o1777}, false},
		{DirSpec{Path: "/etc/nginx/monopanel", Mode: 0o755, Owner: "vmail"}, false},
		{DirSpec{Path: "/opt/monopanel/sphinx", Mode: 0o755, Owner: "sphinx"}, false},
		{DirSpec{Path: "/etc/opendkim", Mode: 0o755, Group: "opendkim"}, false},
	} {
		if err := s.checkDirSpec(c.d); (err == nil) != c.ok {
			t.Errorf("%+v: %v, want ok=%v", c.d, err, c.ok)
		}
	}
}

// Below the allow-list config/apply goes through trusted directories only,
// and dirs/ensure does not take over a directory someone else may have made.
func TestRootWritesNeedTrustedDirectories(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, cl := testServer(t, dir)
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(open, "x.conf"), Content: "x"}}}); agentStatus(err) != http.StatusForbidden {
		t.Fatalf("a write below a directory everyone may write to: %v", err)
	}
	if _, err := os.Stat(filepath.Join(open, "x.conf")); err == nil {
		t.Fatal("written")
	}
	if _, err := cl.EnsureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: filepath.Join(open, "d"), Mode: 0o755}}}); agentStatus(err) != http.StatusForbidden {
		t.Fatalf("a directory below it: %v", err)
	}
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(open, "x")}}); agentStatus(err) != http.StatusForbidden {
		t.Fatalf("a removal below it: %v", err)
	}
	// Sticky, it is fine: nobody may rename what root made there.
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(open, "x.conf"), Content: "x"}}}); err != nil {
		t.Fatalf("below a sticky directory: %v", err)
	}
	for _, f := range []FileSpec{
		{Path: filepath.Join(dir, "a.conf"), Content: "x", Mode: 0o666},
		{Path: filepath.Join(dir, "a.conf"), Content: "x", Mode: 0o4755},
		{Path: filepath.Join(dir, "a.conf"), Content: "x", Owner: "nobody"},
	} {
		if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{f}}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("%+v: %v", f, err)
		}
	}
	// A directory somebody else owns in a directory others may write to is
	// not taken over (here "root" is another user, so the test's own
	// directory counts as somebody else's).
	sticky := filepath.Join(dir, "sticky")
	os.Mkdir(sticky, 0o755)                           //nolint:errcheck // test setup
	os.Chmod(sticky, 0o777|os.ModeSticky)             //nolint:errcheck // test setup
	os.Mkdir(filepath.Join(sticky, "planted"), 0o777) //nolint:errcheck // test setup
	os.Chmod(filepath.Join(sticky, "planted"), 0o777) //nolint:errcheck // test setup
	if _, _, err := ensureDirAt(DirSpec{Path: filepath.Join(sticky, "planted"), Mode: 0o755}, &dirTrust{rootUID: os.Getuid() + 1}, followAll); !errors.Is(err, errUntrusted) {
		t.Fatalf("took over a planted directory: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(sticky, "planted")); st.Mode().Perm() != 0o777 {
		t.Fatal("the planted directory was changed")
	}
}

// Removals reach the entry through its directory: a symlink is removed
// itself, never followed, and a directory on the way swapped for one stops
// the call.
func TestRemovePathsDoNotFollowSymlinks(t *testing.T) {
	ctx := context.Background()
	www := t.TempDir()
	s, cl := testServer(t, t.TempDir())
	s.cfg.WWWRoot = www
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(www, "alex", "data", "www", "example.com")
	if err := os.MkdirAll(filepath.Join(site, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(site, "sub", "f"), []byte("x"), 0o644) //nolint:errcheck // test setup
	if err := os.Symlink(outside, filepath.Join(site, "sub", "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(www, "alex", "data", "www", "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(www, "alex", "data", "www", "evil", "victim")}}); agentStatus(err) != http.StatusForbidden {
		t.Fatalf("through a symlinked directory: %v", err)
	}
	res, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{site}, Recursive: true})
	if err != nil || len(res.Removed) != 1 {
		t.Fatalf("remove the site: %+v %v", res, err)
	}
	if _, err := os.Stat(site); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the site stayed")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("the removal went through the symlink")
	}
	// A path that is a symlink itself is refused, as it always was below
	// the web root; its target stays.
	link := filepath.Join(www, "alex", "data", "www", "evil")
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{link}, Recursive: true}); agentStatus(err) != http.StatusForbidden {
		t.Fatalf("remove a link: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the link's target went")
	}
}

// setfacl gets the very files the walk opened: a symlink in the tree is
// left alone, a directory on the way swapped for one stops the call.
func TestSetACLDoesNotFollowSymlinks(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is not installed")
	}
	me := currentUserName(t)
	ctx := context.Background()
	www := t.TempDir()
	s, cl := testServer(t, t.TempDir())
	s.cfg.WWWRoot = www
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	os.WriteFile(secret, []byte("x"), 0o600) //nolint:errcheck // test setup
	site := filepath.Join(www, "alex", "data", "www", "example.com")
	if err := os.MkdirAll(filepath.Join(site, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(site, "f"), []byte("x"), 0o644)        //nolint:errcheck // test setup
	os.WriteFile(filepath.Join(site, "sub", "g"), []byte("x"), 0o644) //nolint:errcheck // test setup
	os.Symlink(outside, filepath.Join(site, "sub", "out"))            //nolint:errcheck // test setup
	os.Symlink(secret, filepath.Join(site, "secret"))                 //nolint:errcheck // test setup
	if out, err := exec.Command("setfacl", "-m", "u:"+me+":r", filepath.Join(site, "f")).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "not supported") {
			t.Skip("the file system has no POSIX ACLs")
		}
		t.Fatalf("setfacl: %v %s", err, out)
	}
	entry := "u:" + me + ":rwx"
	if err := cl.SetACL(ctx, &SetACLRequest{Path: site, Entries: []string{entry}, Default: true, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	acl := func(p string) string {
		out, _ := exec.Command("getfacl", "-cp", p).CombinedOutput()
		return string(out)
	}
	if a := acl(filepath.Join(site, "sub", "g")); !strings.Contains(a, "user:"+me+":rwx") || strings.Contains(a, "default:") {
		t.Errorf("a file below: %s", a)
	}
	if a := acl(filepath.Join(site, "sub")); !strings.Contains(a, "default:user:"+me+":rwx") {
		t.Errorf("a directory below: %s", a)
	}
	for _, p := range []string{outside, secret} {
		if a := acl(p); strings.Contains(a, "user:"+me+":") {
			t.Errorf("%s behind a symlink got an entry: %s", p, a)
		}
	}
	if err := cl.SetACL(ctx, &SetACLRequest{Path: filepath.Join(site, "sub", "out", "secret"), Entries: []string{entry}}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("through a symlinked directory: %v", err)
	}
	if err := cl.SetACL(ctx, &SetACLRequest{Path: filepath.Join(site, "secret"), Entries: []string{entry}}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("a symlink itself: %v", err)
	}
	// A home itself, right below the web root (the site job gives the web
	// group x there).
	if err := cl.SetACL(ctx, &SetACLRequest{Path: filepath.Join(www, "alex"), Entries: []string{"u:" + me + ":x"}}); err != nil {
		t.Fatalf("a home: %v", err)
	}
	if a := acl(filepath.Join(www, "alex")); !strings.Contains(a, "user:"+me+":--x") {
		t.Errorf("a home: %s", a)
	}
	if err := cl.SetACL(ctx, &SetACLRequest{Path: site, Remove: []string{"u:" + me}, Default: true, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if a := acl(filepath.Join(site, "sub", "g")); strings.Contains(a, "user:"+me+":") {
		t.Errorf("the entry stayed: %s", a)
	}
}

// The log directory: someone else's goes to root (where the agent can give
// it away), root's keeps its mode and gains what keeps root's entries root's.
func TestLogDirFix(t *testing.T) {
	for _, c := range []struct {
		why      string
		uid      uint32
		mode     uint32
		takeOver bool
		wantUID  int
		wantMode uint32
		change   bool
	}{
		{"the API's, as up to 0.8.19", 900, 0o751, true, 0, 0o1771, true},
		{"the API's, unprivileged", 900, 0o751, false, -1, 0o751, false},
		{"root's, done", 0, 0o1771, true, -1, 0o1771, false},
		{"root's, writable by its group", 0, 0o771, true, -1, 0o1771, true},
		{"root's, closed to others", 0, 0o750, true, -1, 0o751, true},
	} {
		uid, gid, mode, change := logDirFix(&unix.Stat_t{Uid: c.uid, Gid: 900, Mode: unix.S_IFDIR | c.mode}, siteLogIDs{rootUID: 0, serviceGID: 900, takeOver: c.takeOver})
		if uid != c.wantUID || mode != c.wantMode || change != c.change || (c.wantUID == 0 && gid != 900) {
			t.Errorf("%s: uid %d gid %d mode %#o change %v", c.why, uid, gid, mode, change)
		}
	}
}

// The history lives in the agent's own directory, 0700 throughout, the
// newest historyKeep versions of each file.
func TestConfHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent", "confhistory")
	n := 0
	h := &confHistory{dir: dir, trust: &dirTrust{rootUID: os.Getuid(), strict: true}, now: func() time.Time { n++; return time.Unix(int64(n), 0) }}
	for i := 0; i < historyKeep+5; i++ {
		if err := h.store("/etc/nginx/nginx.conf", []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	versions := filepath.Join(dir, "etc", "nginx", "nginx.conf")
	entries, err := os.ReadDir(versions)
	if err != nil || len(entries) != historyKeep {
		t.Fatalf("%d versions: %v", len(entries), err)
	}
	if entries[0].Name() != time.Unix(6, 0).UTC().Format("20060102T150405.000Z") {
		t.Errorf("the oldest kept is %s", entries[0].Name())
	}
	for _, p := range []string{filepath.Dir(dir), dir, filepath.Join(dir, "etc"), versions} {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v", p, st.Mode())
		}
	}
	if st, _ := os.Stat(filepath.Join(versions, entries[0].Name())); st.Mode().Perm() != 0o600 {
		t.Errorf("a version: mode %v", st.Mode())
	}
}

// Root's symlinks are followed only above the tree a path lies in (EL's
// /var/mail -> spool/mail above /var/mail/monopanel), never inside it: there
// root itself makes links for the API, from the archives it unpacks into the
// mail store or Sphinx's directory, and one of them followed would carry the
// write anywhere.
func TestWalkDirFollowsRootsLinksOnlyAboveTheTree(t *testing.T) {
	me := os.Getuid()
	base := t.TempDir()
	outside := t.TempDir()
	tree := filepath.Join(base, "real", "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink("real", filepath.Join(base, "alias"))       //nolint:errcheck // test setup
	os.Symlink(outside, filepath.Join(tree, "sub", "lnk")) //nolint:errcheck // test setup
	os.Symlink(outside, filepath.Join(tree, "lnk"))        //nolint:errcheck // test setup
	trusted := &dirTrust{rootUID: me, strict: true}
	depth := len(pathParts(filepath.Join(base, "alias", "tree")))
	w, err := walkDir("/", filepath.Join(base, "alias", "tree", "sub"), walkOpts{trust: trusted, follow: depth, uid: -1, gid: -1})
	if err != nil {
		t.Fatalf("a link above the tree: %v", err)
	}
	if w.resolved != filepath.Join(tree, "sub") {
		t.Errorf("resolved to %s", w.resolved)
	}
	w.close()
	for _, p := range []string{filepath.Join("sub", "lnk"), "lnk", filepath.Join("lnk", "x")} {
		w, err := walkDir("/", filepath.Join(base, "alias", "tree", p), walkOpts{trust: trusted, follow: depth, create: true, mode: 0o755, uid: -1, gid: -1})
		if err == nil {
			w.close()
			t.Errorf("%s: a link inside the tree was followed", p)
		} else if !errors.Is(err, errSymlinkInPath) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("made behind the link: %v", entries)
	}
	// Nothing to follow at all by default.
	if w, err := walkDir("/", filepath.Join(base, "alias", "tree"), walkOpts{trust: trusted, uid: -1, gid: -1}); err == nil {
		w.close()
		t.Error("followed with follow 0")
	}
}

// The tree a path lies in: the deepest entry of the allow-list holding it.
func TestManagedDepth(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.ExtraWritePrefixes = []string{"/var/mail/", "/srv/exact.conf"}
	s := &Server{cfg: cfg}
	for p, want := range map[string]int{
		"/var/mail/monopanel/example.com/info/cur/x": 3, // /var/mail/monopanel/, deeper than the extra /var/mail/
		"/var/mail/other/x":                          2,
		"/etc/nginx/nginx.conf":                      2, // an exact entry: its directory
		"/etc/nginx/monopanel/sites/a.conf":          3,
		"/opt/monopanel/sphinx/bin/searchd":          2,
		"/srv/exact.conf":                            1,
		"/srv/other.conf":                            0,
		"/etc/shadow":                                0,
	} {
		if got := s.managedDepth(p); got != want {
			t.Errorf("%s: %d, want %d", p, got, want)
		}
	}
	if got := s.managedDepth("/run/monopanel/php", "/run/monopanel"); got != 2 {
		t.Errorf("the run directory: %d", got)
	}
}

// config/apply, paths/remove and dirs/ensure go through root's link above
// the tree (the allow-list names /var/mail/monopanel/, EL keeps it in
// /var/spool/mail) but stop at one inside it, even one root made in a
// directory only root may write to.
func TestRootWritesFollowNoLinkInsideTheTree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink("real", alias); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("keep\n"), 0o644) //nolint:errcheck // test setup
	if err := os.Symlink(outside, filepath.Join(real, "sub", "lnk")); err != nil {
		t.Fatal(err)
	}
	_, cl := testServer(t, alias)

	// Above the tree: the link is the system's, the write lands behind it.
	if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(alias, "sub", "a.conf"), Content: "a"}}}); err != nil {
		t.Fatalf("through the link above the tree: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(real, "sub", "a.conf")); string(b) != "a" {
		t.Fatalf("a.conf holds %q", b)
	}
	if _, err := cl.EnsureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: filepath.Join(alias, "sub", "d"), Mode: 0o755}}}); err != nil {
		t.Fatalf("a directory through the link above the tree: %v", err)
	}
	if res, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(alias, "sub", "a.conf")}}); err != nil || len(res.Removed) != 1 {
		t.Fatalf("a removal through the link above the tree: %+v %v", res, err)
	}

	// Inside the tree: refused, nothing happens behind the link.
	if _, err := cl.ApplyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(alias, "sub", "lnk", "evil.conf"), Content: "x"}}}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("a write through a link inside the tree: %v", err)
	}
	if _, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(alias, "sub", "lnk", "victim")}}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("a removal through a link inside the tree: %v", err)
	}
	if _, err := cl.EnsureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: filepath.Join(alias, "sub", "lnk", "made"), Mode: 0o755}}}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("a directory through a link inside the tree: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("the victim was changed")
	}
	for _, n := range []string{"evil.conf", "made"} {
		if _, err := os.Lstat(filepath.Join(outside, n)); err == nil {
			t.Errorf("%s was made behind the link", n)
		}
	}
	// The link itself goes like any entry, its target stays.
	if res, err := cl.RemovePaths(ctx, &RemovePathsRequest{Paths: []string{filepath.Join(alias, "sub", "lnk")}}); err != nil || len(res.Removed) != 1 {
		t.Fatalf("remove the link itself: %+v %v", res, err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the link's target went")
	}
}

// A directory whose ACL lets a named user write is not gone through: its
// mode shows the mask, not who may write.
func TestWalkDirRefusesAnACLThatLetsOthersWrite(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is not installed")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "acl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-m", "u:nobody:rwx", dir).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "not supported") {
			t.Skip("the file system has no POSIX ACLs")
		}
		t.Fatalf("setfacl: %v %s", err, out)
	}
	trusted := &dirTrust{rootUID: os.Getuid(), strict: true}
	w, err := walkDir("/", filepath.Join(dir), walkOpts{trust: trusted, follow: followAll, uid: -1, gid: -1})
	if err == nil {
		w.close()
		t.Fatal("went through a directory a named user may write to")
	}
	if !errors.Is(err, errUntrusted) || !strings.Contains(err.Error(), "ACL") {
		t.Fatalf("%v", err)
	}
}
