package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The agent makes directories and writes files as root in trees the clients
// own. A component swapped for a symlink must stop the call, never redirect
// the mkdir, chmod, chown or write to where the link points (/etc/systemd/
// system, /root/.ssh, /etc/cron.d).
func TestHomeWritesDoNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "www")
	outside := filepath.Join(base, "etc")
	for _, d := range []string{filepath.Join(root, "alex", "data"), filepath.Join(outside, "system")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// alex replaced data/www with a link to /etc.
	if err := os.Symlink(outside, filepath.Join(root, "alex", "data", "www")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		filepath.Join(root, "alex", "data", "www", "system"),              // an existing directory behind the link
		filepath.Join(root, "alex", "data", "www", "example.com", "logs"), // a new one
		filepath.Join(root, "alex", "data", "www"),                        // the link itself
	} {
		if _, err := ensureHomeDir(root, DirSpec{Path: p, Mode: 0o777}); !errors.Is(err, errSymlinkInPath) {
			t.Errorf("ensureHomeDir(%s): %v, want a refusal", p, err)
		}
	}
	if st, err := os.Stat(filepath.Join(outside, "system")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the directory behind the link was changed: %v %v", st.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(outside, "example.com")); err == nil {
		t.Error("a directory was made behind the link")
	}

	if err := writeHomeFile(root, filepath.Join(root, "alex", "data", "www", "cron"), []byte("* * * * * root id\n"), 0o644, -1, -1); !errors.Is(err, errSymlinkInPath) {
		t.Errorf("writeHomeFile through the link: %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "cron")); err == nil {
		t.Error("a file was written behind the link")
	}

	// Without links both work: the levels are made with the mode, and a file
	// replaces a symlink planted in its place instead of writing through it.
	site := filepath.Join(root, "alex", "data", "sites", "example.com", "public_html")
	created, err := ensureHomeDir(root, DirSpec{Path: site, Mode: 0o750})
	if err != nil || !created {
		t.Fatalf("ensureHomeDir: %v %v", created, err)
	}
	if st, err := os.Stat(site); err != nil || st.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v, %v", st.Mode(), err)
	}
	if created, err := ensureHomeDir(root, DirSpec{Path: site, Mode: 0o755}); err != nil || created {
		t.Fatalf("ensureHomeDir again: %v %v", created, err)
	}
	if st, _ := os.Stat(site); st.Mode().Perm() != 0o755 {
		t.Fatalf("an existing directory keeps the requested mode: %v", st.Mode())
	}
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(site, "index.html")
	if err := os.Symlink(target, index); err != nil {
		t.Fatal(err)
	}
	if err := writeHomeFile(root, index, []byte("<h1>hello</h1>\n"), 0o644, -1, -1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Errorf("the write went through the planted link: %q", b)
	}
	if st, err := os.Lstat(index); err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o644 {
		t.Errorf("index.html: %v %v", st.Mode(), err)
	}
	if b, _ := os.ReadFile(index); string(b) != "<h1>hello</h1>\n" {
		t.Errorf("index.html holds %q", b)
	}
	if entries, _ := os.ReadDir(site); len(entries) != 1 {
		t.Errorf("a temporary file stayed behind: %v", entries)
	}
}

// Root reads logs in a tree someone else may write to (the panel's log
// directory is the API user's); a log swapped for a symlink (to /etc/shadow)
// or a directory on the way swapped for one must not be read.
func TestHomeReadsDoNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "www")
	logs := filepath.Join(root, "alex", "data", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "shadow")
	if err := os.WriteFile(secret, []byte("root:$6$hash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "ok.log"), []byte("GET /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(logs, "evil.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(root, "alex", "data", "elsewhere")); err != nil {
		t.Fatal(err)
	}
	f, err := openBelow(root, filepath.Join(logs, "ok.log"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, p := range []string{filepath.Join(logs, "evil.log"), filepath.Join(root, "alex", "data", "elsewhere", "shadow")} {
		if f, err := openBelow(root, p); !errors.Is(err, errSymlinkInPath) {
			if f != nil {
				f.Close()
			}
			t.Errorf("openBelow(%s): %v, want a refusal", p, err)
		}
	}
	if _, err := openBelow(root, filepath.Join(logs, "missing.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing log: %v", err)
	}
}
