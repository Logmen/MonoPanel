package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
)

// legacyTree is a shared Apache's directory of 0.8.19 in a temporary
// directory reached without a symlink (TMPDIR may hold one).
func legacyTree(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sites := filepath.Join(dir, "monopanel", "sites")
	if err := os.MkdirAll(sites, 0o755); err != nil {
		t.Fatal(err)
	}
	return sites
}

func writeLegacy(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The agent hands the API a site's custom directives of the shared Apache —
// the *.conf files its IncludeOptional took, root's even with mode 0600 —
// and the names in the directory; nothing when there is nothing.
func TestReadApacheLegacy(t *testing.T) {
	sites := legacyTree(t)
	d := filepath.Join(sites, "shop.example.com.d")
	writeLegacy(t, filepath.Join(d, "b.conf"), "Header set X-B b\n")
	writeLegacy(t, filepath.Join(d, "a.conf"), "Redirect 301 /old /new\n")
	writeLegacy(t, filepath.Join(d, "README"), "Custom Apache directives for shop.example.com.\n")
	writeLegacy(t, filepath.Join(d, "old.conf.bak"), "Require all denied\n")
	writeLegacy(t, filepath.Join(d, ".hidden.conf"), "Require all denied\n")
	writeLegacy(t, filepath.Join(sites, "shop.example.com.conf"), "<VirtualHost 127.0.0.1:8080>\n</VirtualHost>\n")

	if err := os.Chmod(filepath.Join(d, "b.conf"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadApacheLegacy(sites, "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Files[0].Name != "a.conf" || res.Files[0].Content != "Redirect 301 /old /new\n" || res.Files[1].Name != "b.conf" {
		t.Fatalf("files: %+v", res.Files)
	}
	// The permission bits come along: a file only root reads stays so.
	if res.Files[0].Mode != 0o600 || res.Files[1].Mode != 0o644 {
		t.Fatalf("modes: %o %o", res.Files[0].Mode, res.Files[1].Mode)
	}
	names, err := ReadApacheLegacy(sites, "")
	if err != nil || !slices.Equal(names.Names, []string{"shop.example.com.conf", "shop.example.com.d"}) {
		t.Fatalf("names: %+v %v", names, err)
	}
	for _, c := range []struct{ dir, domain string }{
		{sites, "other.example.com"},                       // no directory of the site
		{filepath.Join(sites, "gone"), ""},                 // no shared Apache at all
		{filepath.Join(sites, "gone"), "shop.example.com"}, // ditto
	} {
		if res, err := ReadApacheLegacy(c.dir, c.domain); err != nil || len(res.Files) != 0 || len(res.Names) != 0 {
			t.Fatalf("%s %s: %+v %v", c.dir, c.domain, res, err)
		}
	}
}

// What the agent does not read as a file of directives fails the read, by
// name, before root opens it — a symlink (to /etc/shadow, say), a symlink on
// the way, another name of a file, a FIFO that would hang a reader, a
// directory, a file too large — and a site whose directives the shared
// Apache read is not moved without them.
func TestReadApacheLegacyRefuses(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	writeLegacy(t, secret, "root:$6$hash:19000:0:99999:7:::\n")
	cases := []struct {
		name  string
		setup func(t *testing.T, d string)
		want  string
	}{
		{"symlinked file", func(t *testing.T, d string) {
			if err := os.Symlink(secret, filepath.Join(d, "custom.conf")); err != nil {
				t.Fatal(err)
			}
		}, "custom.conf: a symlink"},
		{"hard link", func(t *testing.T, d string) {
			writeLegacy(t, filepath.Join(d, "orig.txt"), "Header set X-A a\n")
			if err := os.Link(filepath.Join(d, "orig.txt"), filepath.Join(d, "custom.conf")); err != nil {
				t.Skip("no hard links here:", err)
			}
		}, "other names"},
		{"fifo", func(t *testing.T, d string) {
			if err := unix.Mkfifo(filepath.Join(d, "custom.conf"), 0o600); err != nil {
				t.Skip("no FIFO here:", err)
			}
		}, "not a regular file"},
		{"directory", func(t *testing.T, d string) {
			if err := os.Mkdir(filepath.Join(d, "custom.conf"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"too large", func(t *testing.T, d string) {
			writeLegacy(t, filepath.Join(d, "custom.conf"), strings.Repeat("#", apacheLegacyFileMax+1))
		}, "larger than"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sites := legacyTree(t)
			d := filepath.Join(sites, "shop.example.com.d")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			writeLegacy(t, filepath.Join(d, "fine.conf"), "Header set X-Fine yes\n")
			c.setup(t, d)
			done := make(chan struct{})
			var res *ApacheLegacyResponse
			var err error
			go func() { defer close(done); res, err = ReadApacheLegacy(sites, "shop.example.com") }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the read hangs")
			}
			var ae *Error
			if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, c.want) || !strings.Contains(ae.Message, "custom.conf") {
				t.Fatalf("read: %+v %v", res, err)
			}
			if strings.Contains(ae.Message+ae.Output, "$6$") {
				t.Fatalf("the refusal shows what it refused to read: %v", ae)
			}
		})
	}

	// The site's directory, or a directory on the way, swapped for a symlink.
	sites := legacyTree(t)
	target := filepath.Join(filepath.Dir(secret), "elsewhere.d")
	writeLegacy(t, filepath.Join(target, "x.conf"), "Header set X-Elsewhere yes\n")
	if err := os.Symlink(target, filepath.Join(sites, "shop.example.com.d")); err != nil {
		t.Fatal(err)
	}
	var ae *Error
	if _, err := ReadApacheLegacy(sites, "shop.example.com"); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("symlinked site directory: %v", err)
	}
	link := filepath.Join(filepath.Dir(sites), "linked")
	if err := os.Symlink(sites, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadApacheLegacy(link, ""); !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, "refusing "+link) {
		t.Fatalf("symlink on the way: %v", err)
	}
}

// The way to the shared Apache's directory is walked as the agent walks the
// system's directories: a symlink of root's in a directory of root's — an
// /etc/apache2 an administrator keeps elsewhere — is followed, as for the
// files the agent writes there; one someone else could have made is not.
// The directory itself, and what is below it, is never reached through a
// symlink, root's or not.
func TestReadApacheLegacyWalksTheWayAsTheSystemsDirectories(t *testing.T) {
	asRoot(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "srv", "apache2")
	writeLegacy(t, filepath.Join(moved, "monopanel", "sites", "shop.example.com.d", "custom.conf"), "Header set X-A a\n")
	etc := filepath.Join(base, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(etc, "apache2")); err != nil {
		t.Fatal(err)
	}
	sites := filepath.Join(etc, "apache2", "monopanel", "sites")
	res, err := ReadApacheLegacy(sites, "shop.example.com")
	if err != nil || len(res.Files) != 1 || res.Files[0].Content != "Header set X-A a\n" {
		t.Fatalf("through root's symlink on the way: %+v %v", res, err)
	}
	// Nothing there at all: nothing to carry, as on a host of this version.
	if res, err := ReadApacheLegacy(filepath.Join(etc, "apache2", "gone", "sites"), "shop.example.com"); err != nil || len(res.Files) != 0 {
		t.Fatalf("a missing directory behind the symlink: %+v %v", res, err)
	}

	// The same symlink in a directory others may write: not root's to trust.
	open := filepath.Join(base, "open")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(open, "apache2")); err != nil {
		t.Fatal(err)
	}
	var ae *Error
	if _, err := ReadApacheLegacy(filepath.Join(open, "apache2", "monopanel", "sites"), "shop.example.com"); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("a symlink in a directory others write: %v", err)
	}

	// The directory itself, even root's symlink to it, and the site's.
	if err := os.Symlink(filepath.Join(moved, "monopanel", "sites"), filepath.Join(moved, "monopanel", "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadApacheLegacy(filepath.Join(etc, "apache2", "monopanel", "linked"), "shop.example.com"); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("the directory as root's symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(moved, "monopanel", "sites", "shop.example.com.d"), filepath.Join(moved, "monopanel", "sites", "other.example.com.d")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadApacheLegacy(sites, "other.example.com"); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("the site's directory as root's symlink: %v", err)
	}
}

// Over the socket: the domain is one plain name, and EL, which never had the
// shared Apache, has nothing to carry.
func TestApacheLegacyEndpoint(t *testing.T) {
	ctx := context.Background()
	s, cl := testServer(t, t.TempDir())
	s.apacheLegacySites = legacyTree(t)
	writeLegacy(t, filepath.Join(s.apacheLegacySites, "shop.example.com.d", "custom.conf"), "Header set X-A a\n")
	res, err := cl.ApacheLegacy(ctx, "shop.example.com")
	if err != nil || len(res.Files) != 1 || res.Files[0].Content != "Header set X-A a\n" {
		t.Fatalf("legacy: %+v %v", res, err)
	}
	for _, bad := range []string{"../../etc", "shop.example.com/../x", "SHOP.example.com", ".example.com", "localhost", "a..b.com", strings.Repeat("a", 64) + ".com"} {
		var ae *Error
		if _, err := cl.ApacheLegacy(ctx, bad); !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("%q: %v", bad, err)
		}
	}
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"))
	s.profile, _ = osprofile.FromRelease(rel)
	if res, err := cl.ApacheLegacy(ctx, "shop.example.com"); err != nil || len(res.Files) != 0 {
		t.Fatalf("EL: %+v %v", res, err)
	}
}
