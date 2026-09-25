package agent

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"monopanel/internal/config"
)

// A file created in a directory with the web group's default ACL inherits a
// group:: entry with x. Recalculating the mask on the next fix used to let
// that x through, and files showed as 650/654. siteACL sets the mask itself:
// plain files stay 640/644 however many times it runs, executables keep
// their x, the web group reads everything, and a symlink out of the tree is
// left alone.
func TestSiteACLKeepsFileModes(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is not installed")
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("no group name for the test's gid")
	}
	cfg := config.Default()
	cfg.WWWRoot = t.TempDir()
	s := &Server{cfg: cfg}
	site := filepath.Join(cfg.WWWRoot, "alex", "data", "www", "example.com")
	if err := os.MkdirAll(filepath.Join(site, "upload"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{site, filepath.Join(site, "upload")} {
		os.Chmod(d, 0o750) //nolint:errcheck // test setup
	}
	if out, err := exec.Command("setfacl", "-m", "g:"+g.Name+":r-x", "-m", "d:g:"+g.Name+":r-x", site, filepath.Join(site, "upload")).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "not supported") {
			t.Skip("the file system has no POSIX ACLs")
		}
		t.Fatalf("setfacl: %v %s", err, out)
	}
	// Files as PHP or SFTP would create them: they inherit the default ACL.
	plain := filepath.Join(site, "upload", "photo.jpg")
	script := filepath.Join(site, "cron.sh")
	shared := filepath.Join(site, "shared.txt")
	os.WriteFile(plain, []byte("x"), 0o644)  //nolint:errcheck // test setup
	os.WriteFile(script, []byte("x"), 0o750) //nolint:errcheck // test setup
	os.WriteFile(shared, []byte("x"), 0o664) //nolint:errcheck // test setup
	// Inherited, its group write is masked off; the owner opens it on purpose.
	os.Chmod(shared, 0o664) //nolint:errcheck // test setup
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("x"), 0o600) //nolint:errcheck // test setup
	if err := os.Symlink(outside, filepath.Join(site, "link")); err != nil {
		t.Fatal(err)
	}

	for run := 0; run < 2; run++ {
		res, err := s.siteACL(context.Background(), &SiteACLRequest{Root: site, Group: g.Name})
		if err != nil {
			t.Fatalf("siteACL: %v", err)
		}
		if res.Dirs != 2 || res.Files != 3 {
			t.Fatalf("counted %+v", res)
		}
	}
	perm := func(p string) os.FileMode {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	// The group bits shown by ls are the mask.
	for p, want := range map[string]os.FileMode{plain: 0o040, script: 0o050, shared: 0o060} {
		if got := perm(p) & 0o070; got != want {
			t.Errorf("%s: group bits %03o, want %03o (mode %04o)", filepath.Base(p), got, want, perm(p))
		}
	}
	acl := func(p string) string {
		out, err := exec.Command("getfacl", "-cp", p).CombinedOutput()
		if err != nil {
			t.Fatalf("getfacl %s: %v %s", p, err, out)
		}
		return string(out)
	}
	if a := acl(plain); !strings.Contains(a, "group:"+g.Name+":r-x") && !strings.Contains(a, "group:"+g.Name+":r--") {
		t.Errorf("web group entry missing on a file:\n%s", a)
	}
	if a := acl(filepath.Join(site, "upload")); !strings.Contains(a, "default:group:"+g.Name+":r-x") {
		t.Errorf("default entry missing on a directory:\n%s", a)
	}
	if a := acl(outside); strings.Contains(a, "group:"+g.Name) {
		t.Errorf("the symlink's target outside the tree got an ACL:\n%s", a)
	}
	if _, err := s.siteACL(context.Background(), &SiteACLRequest{Root: site, Group: "bad group"}); err == nil {
		t.Error("a malformed group name was accepted")
	}
	if _, err := s.siteACL(context.Background(), &SiteACLRequest{Root: "/etc", Group: g.Name}); err == nil {
		t.Error("a tree outside the web root was accepted")
	}
}
