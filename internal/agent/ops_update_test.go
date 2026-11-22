package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"monopanel/internal/buildinfo"
	"monopanel/internal/updater"
)

func openTestDir(t *testing.T, p string) int {
	t.Helper()
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) }) //nolint:errcheck // test
	return fd
}

// The agent installs a copy of the package that only root can reach: the
// API keeps no way to swap what was checked for something else.
func TestCopyPackage(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	name := "monopanel_0.9.0_amd64.deb"
	body := []byte("!<arch>\nthe package\n")
	if err := os.WriteFile(filepath.Join(src, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := copyPackage(openTestDir(t, src), openTestDir(t, dst), name)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("sum %s, want %x", sum, want)
	}
	got, err := os.ReadFile(filepath.Join(dst, name))
	if err != nil || string(got) != string(body) {
		t.Fatalf("copy: %q %v", got, err)
	}
	st, _ := os.Stat(filepath.Join(dst, name))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode %v, want 0600", st.Mode().Perm())
	}
	// changing the original afterwards changes nothing the agent installs
	if err := os.WriteFile(filepath.Join(src, name), []byte("swapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, name)); string(got) != string(body) {
		t.Fatalf("the copy followed the original: %q", got)
	}

	// a symlink in place of the package is not followed
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("root's"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := "monopanel_0.9.1_amd64.deb"
	if err := os.Symlink(secret, filepath.Join(src, link)); err != nil {
		t.Fatal(err)
	}
	if _, err := copyPackage(openTestDir(t, src), openTestDir(t, dst), link); !errors.Is(err, errNotPackage) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, link)); err == nil {
		t.Fatal("a symlink's target was copied")
	}
	// neither is a FIFO read (it would block the agent)
	fifo := "monopanel_0.9.2_amd64.deb"
	if err := syscall.Mkfifo(filepath.Join(src, fifo), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := copyPackage(openTestDir(t, src), openTestDir(t, dst), fifo); !errors.Is(err, errNotPackage) {
		t.Fatalf("fifo: %v", err)
	}
	// and a missing package is the request's fault
	if _, err := copyPackage(openTestDir(t, src), openTestDir(t, dst), "monopanel_0.9.3_amd64.deb"); !errors.Is(err, errNotPackage) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPruneStaged(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"monopanel_0.8.1_amd64.deb", "monopanel-0.8.1.x86_64.rpm", ".monopanel_0.8.2_amd64.deb.part", "monopanel-0.8.0", "state.json", "monopanel_0.9.0_amd64.deb"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneStaged(openTestDir(t, dir), dir, "monopanel_0.9.0_amd64.deb")
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	// the binary backup of update-run and other files stay
	if want := "monopanel-0.8.0 monopanel_0.9.0_amd64.deb state.json"; strings.Join(left, " ") != want {
		t.Fatalf("left %q, want %q", left, want)
	}
}

// The request names the release's own package and no older version than the
// one running.
func TestInstallPanelRefusals(t *testing.T) {
	s := policyServer(t, debian13)
	old := buildinfo.Version
	buildinfo.Version = "0.9.0"
	t.Cleanup(func() { buildinfo.Version = old })
	name := func(v string) string {
		return filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("deb", runtime.GOARCH, v))
	}
	for _, c := range []struct {
		what string
		req  InstallPanelRequest
		code int
		msg  string
	}{
		{"another file of the downloads", InstallPanelRequest{Version: "0.9.1", Package: filepath.Join(s.cfg.DownloadsDir(), "x.deb")}, http.StatusBadRequest, "package must be"},
		{"the package of another version", InstallPanelRequest{Version: "0.9.1", Package: name("0.9.2")}, http.StatusBadRequest, "package must be"},
		{"a package outside the downloads", InstallPanelRequest{Version: "0.9.1", Package: filepath.Join(s.cfg.DataDir, filepath.Base(name("0.9.1")))}, http.StatusBadRequest, "package must be"},
		{"an rpm on Debian", InstallPanelRequest{Version: "0.9.1", Package: filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("rpm", runtime.GOARCH, "0.9.1"))}, http.StatusBadRequest, "package must be"},
		{"a downgrade", InstallPanelRequest{Version: "0.8.19", Package: name("0.8.19")}, http.StatusForbidden, "downgrade"},
		{"a bad version", InstallPanelRequest{Version: "../0.9.1", Package: name("0.9.1")}, http.StatusBadRequest, "invalid version"},
		{"a package that is not there", InstallPanelRequest{Version: "0.9.1", Package: name("0.9.1")}, http.StatusBadRequest, "no such package"},
	} {
		_, err := s.installPanel(context.Background(), &c.req)
		var e *Error
		if !errors.As(err, &e) || e.Status != c.code || !strings.Contains(e.Message, c.msg) {
			t.Errorf("%s: %v, want %d %q", c.what, err, c.code, c.msg)
		}
	}
}
