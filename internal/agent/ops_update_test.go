package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// entries lists a directory's names, sorted.
func entries(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return strings.Join(out, " ")
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
	if err := copyPackage(openTestDir(t, src), openTestDir(t, dst), name, strings.ToUpper(sha256Hex(body))); err != nil {
		t.Fatal(err)
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
	// and a copy that is not the package checked takes nothing's place:
	// the earlier copy stays, no partial file is left behind
	if err := copyPackage(openTestDir(t, src), openTestDir(t, dst), name, sha256Hex(body)); !errors.Is(err, errDigest) {
		t.Fatalf("a copy of another digest: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, name)); string(got) != string(body) {
		t.Fatalf("a refused copy replaced the staged package: %q", got)
	}
	if left := entries(t, dst); left != name {
		t.Fatalf("left in the update directory: %q", left)
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
	if err := copyPackage(openTestDir(t, src), openTestDir(t, dst), link, sha256Hex([]byte("root's"))); !errors.Is(err, errNotPackage) {
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
	if err := copyPackage(openTestDir(t, src), openTestDir(t, dst), fifo, sha256Hex(nil)); !errors.Is(err, errNotPackage) {
		t.Fatalf("fifo: %v", err)
	}
	// and a missing package is the request's fault
	if err := copyPackage(openTestDir(t, src), openTestDir(t, dst), "monopanel_0.9.3_amd64.deb", sha256Hex(nil)); !errors.Is(err, errNotPackage) {
		t.Fatalf("missing: %v", err)
	}
	if left := entries(t, dst); left != name {
		t.Fatalf("left in the update directory: %q", left)
	}
}

func TestPruneStaged(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"monopanel_0.8.1_amd64.deb", "monopanel-0.8.1.x86_64.rpm", ".monopanel_0.8.2_amd64.deb.part", ".0123456789abcdef.part", "monopanel-0.8.0", "state.json", "monopanel_0.9.0_amd64.deb"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "old.deb"), 0o700); err != nil {
		t.Fatal(err)
	}
	pruneStaged(openTestDir(t, dir), "monopanel_0.9.0_amd64.deb")
	// the binary backup of update-run, other files and directories stay
	if want := "monopanel-0.8.0 monopanel_0.9.0_amd64.deb old.deb state.json"; entries(t, dir) != want {
		t.Fatalf("left %q, want %q", entries(t, dir), want)
	}
}

// releaseServer is a Debian agent on version 0.9.0 with a release key of the
// test's own and its update directory in a temporary directory (the real one
// is root's).
func releaseServer(t *testing.T) (*Server, ed25519.PrivateKey) {
	t.Helper()
	s := policyServer(t, debian13)
	old := buildinfo.Version
	buildinfo.Version = "0.9.0"
	t.Cleanup(func() { buildinfo.Version = old })
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Update.PublicKey = base64.StdEncoding.EncodeToString(pub)
	s.cfg.Agent.StateDir = filepath.Join(t.TempDir(), "agent")
	prev := openUpdatesDir
	openUpdatesDir = func(p string) (int, error) {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return -1, err
		}
		return unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	t.Cleanup(func() { openUpdatesDir = prev })
	if err := os.MkdirAll(s.cfg.DownloadsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, priv
}

// signedRelease is the checksum list of a release of version with the
// package body, and its signature.
func signedRelease(priv ed25519.PrivateKey, version string, body []byte) (sums, sig string) {
	sums = sha256Hex(body) + "  " + updater.AssetName("deb", runtime.GOARCH, version) + "\n"
	return sums, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(sums)))
}

// An update-run started for a checked package hands the staged copy to the
// package manager some time after it hashed it. A later request — the API
// asks as often as it likes — must not be able to put anything else under
// that name meanwhile: a refused request leaves the staged package as it
// was. Up to this fix the copy took the name first and was checked after.
func TestStagingKeepsTheCheckedPackage(t *testing.T) {
	s, priv := releaseServer(t)
	name := updater.AssetName("deb", runtime.GOARCH, "0.9.1")
	pkg := filepath.Join(s.cfg.DownloadsDir(), name)
	genuine := []byte("!<arch>\nthe release\n")
	if err := os.WriteFile(pkg, genuine, 0o644); err != nil {
		t.Fatal(err)
	}
	sums, sig := signedRelease(priv, "0.9.1", genuine)
	req := InstallPanelRequest{Package: pkg, SHA256: sha256Hex(genuine), Version: "0.9.1", Sums: sums, Sig: sig}
	staged, sum, signed, err := s.stageRelease(&req)
	if err != nil || !signed || sum != sha256Hex(genuine) || staged != filepath.Join(s.cfg.AgentUpdatesDir(), name) {
		t.Fatalf("the release: %q %q %v %v", staged, sum, signed, err)
	}

	// the API swaps the download and asks again, with the digest the
	// signature vouches for and with the digest of what it put there
	evil := []byte("!<arch>\npostinst: a root shell\n")
	if err := os.WriteFile(pkg, evil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what, sha, msg string
	}{
		{"the signed digest", sha256Hex(genuine), "package digest does not match the request"},
		{"its own digest", sha256Hex(evil), "package is not the one the signed checksums describe"},
	} {
		r := req
		r.SHA256 = c.sha
		_, _, _, err := s.stageRelease(&r)
		var e *Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, c.msg) {
			t.Errorf("%s: %v, want 400 %q", c.what, err, c.msg)
		}
		if got, _ := os.ReadFile(staged); string(got) != string(genuine) {
			t.Fatalf("%s: a refused request replaced the staged package with %q", c.what, got)
		}
	}
	if left := entries(t, s.cfg.AgentUpdatesDir()); left != name {
		t.Fatalf("left in the update directory: %q", left)
	}
}

// The request names the release's own package, a SHA-256, no older version
// than the one running, and a signature when the host has a release key.
func TestInstallPanelRefusals(t *testing.T) {
	s, priv := releaseServer(t)
	name := func(v string) string {
		return filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("deb", runtime.GOARCH, v))
	}
	body := []byte("the package")
	sum := sha256Hex(body)
	sums, sig := signedRelease(priv, "0.9.1", body)
	otherSums, otherSig := signedRelease(priv, "0.9.1", []byte("another build"))
	ok := InstallPanelRequest{Version: "0.9.1", Package: name("0.9.1"), SHA256: sum, Sums: sums, Sig: sig}
	with := func(f func(*InstallPanelRequest)) InstallPanelRequest { r := ok; f(&r); return r }
	for _, c := range []struct {
		what string
		req  InstallPanelRequest
		code int
		msg  string
	}{
		{"another file of the downloads", with(func(r *InstallPanelRequest) { r.Package = filepath.Join(s.cfg.DownloadsDir(), "x.deb") }), http.StatusBadRequest, "package must be"},
		{"the package of another version", with(func(r *InstallPanelRequest) { r.Package = name("0.9.2") }), http.StatusBadRequest, "package must be"},
		{"a package outside the downloads", with(func(r *InstallPanelRequest) { r.Package = filepath.Join(s.cfg.DataDir, filepath.Base(name("0.9.1"))) }), http.StatusBadRequest, "package must be"},
		{"an rpm on Debian", with(func(r *InstallPanelRequest) {
			r.Package = filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("rpm", runtime.GOARCH, "0.9.1"))
		}), http.StatusBadRequest, "package must be"},
		{"a downgrade", with(func(r *InstallPanelRequest) { r.Version, r.Package = "0.8.19", name("0.8.19") }), http.StatusForbidden, "downgrade"},
		{"a bad version", with(func(r *InstallPanelRequest) { r.Version = "../0.9.1" }), http.StatusBadRequest, "invalid version"},
		{"a digest that is no SHA-256", with(func(r *InstallPanelRequest) { r.SHA256 = "abc" }), http.StatusBadRequest, "not a SHA-256"},
		{"no signature on a host with a key", with(func(r *InstallPanelRequest) { r.Sig = "" }), http.StatusBadRequest, "requires a signature"},
		{"a signature of other checksums", with(func(r *InstallPanelRequest) { r.Sig = otherSig }), http.StatusBadRequest, "release signature"},
		{"checksums of another build", with(func(r *InstallPanelRequest) { r.Sums, r.Sig = otherSums, otherSig }), http.StatusBadRequest, "signed checksums"},
		{"a package that is not there", ok, http.StatusBadRequest, "not a package file"},
	} {
		_, err := s.installPanel(context.Background(), &c.req)
		var e *Error
		if !errors.As(err, &e) || e.Status != c.code || !strings.Contains(e.Message, c.msg) {
			t.Errorf("%s: %v, want %d %q", c.what, err, c.code, c.msg)
		}
	}
}

// A build made after a release holds commits the release lacks: going back
// to the release is a downgrade like any other.
func TestInstallPanelRefusesTheTagUnderADescribeBuild(t *testing.T) {
	s, priv := releaseServer(t)
	buildinfo.Version = "0.9.1-4-gabc1234"
	body := []byte("the package")
	sums, sig := signedRelease(priv, "0.9.1", body)
	req := InstallPanelRequest{Version: "0.9.1", Package: filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("deb", runtime.GOARCH, "0.9.1")), SHA256: sha256Hex(body), Sums: sums, Sig: sig}
	_, _, _, err := s.stageRelease(&req)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || !strings.Contains(e.Message, "downgrade") {
		t.Fatalf("0.9.1 over 0.9.1-4-gabc1234: %v", err)
	}
}

// The refusal of a downgrade invites nobody to install the older package
// casually: it says what going back loses — below 0.8.20 also what that
// release changed on the host — and where the way back is described, the
// page the package's preremove names when it is taken all the same.
func TestDowngradeRefusalPointsToTheWayBack(t *testing.T) {
	s, _ := releaseServer(t)
	buildinfo.Version = "0.8.20"
	req := InstallPanelRequest{Version: "0.8.19", Package: filepath.Join(s.cfg.DownloadsDir(), updater.AssetName("deb", runtime.GOARCH, "0.8.19"))}
	_, _, _, err := s.stageRelease(&req)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Message != downgradeRefusal("0.8.19", "0.8.20") {
		t.Fatalf("0.8.19 over 0.8.20: %v", err)
	}
	for _, c := range []struct {
		to, from string
		layout   bool
	}{
		{"0.8.19", "0.8.20", true},
		{"0.8.19", "0.8.19-5-gabc1234", true},
		{"0.8.12", "0.8.23", true},
		{"0.8.21", "0.8.22", false},
		{"0.8.20-rc.1", "0.8.20", false},
	} {
		msg := downgradeRefusal(c.to, c.from)
		for _, want := range []string{"installing " + c.to + " over " + c.from + " is a downgrade", "brings back what later ones fixed", downgradeDoc} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s over %s: %q lacks %q", c.to, c.from, msg, want)
			}
		}
		if strings.Contains(msg, "by hand as root") {
			t.Errorf("%s over %s: %q", c.to, c.from, msg)
		}
		if got := strings.Contains(msg, "older than 0.8.20") && strings.Contains(msg, "Apache mode") && strings.Contains(msg, "env file") && strings.Contains(msg, "site logs"); got != c.layout {
			t.Errorf("%s over %s: what 0.8.20 changed told %v: %q", c.to, c.from, got, msg)
		}
	}
	pre, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", "preremove.sh"))
	if err != nil {
		t.Fatal(err)
	}
	title, url, ok := strings.Cut(strings.TrimSuffix(downgradeDoc, ")"), " in the documentation (")
	if !ok || !strings.Contains(string(pre), strings.Trim(title, `"`)) || !strings.Contains(string(pre), url) {
		t.Errorf("preremove.sh points elsewhere than %s", downgradeDoc)
	}
}
