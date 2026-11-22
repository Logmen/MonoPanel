package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// stage writes files below staging the way restic restores them there: at
// staging + their absolute path.
func stage(t *testing.T, staging string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(staging, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runMerge(t *testing.T, s *Server, staging string, includes ...string) (*restoreMerge, string) {
	t.Helper()
	var out bytes.Buffer
	m := &restoreMerge{s: s, ctx: context.Background(), out: &out}
	if err := m.sanitize(staging); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	m.merge(fd, staging, includes)
	return m, out.String()
}

// An in-place restore moves the staged home over the live one: a symlink
// the account left in its home is replaced, not followed; set-user-ID bits
// and FIFOs do not arrive; files the snapshot does not have stay.
func TestRestoreInPlaceMerge(t *testing.T) {
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	outside := t.TempDir()
	mkdirs(t, home)
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	for n, body := range map[string]string{"keep.txt": "keep", "old.txt": "old"} {
		if err := os.WriteFile(filepath.Join(home, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staging := t.TempDir()
	stage(t, staging, map[string]string{
		home + "/data/f":       "restored",
		home + "/data/www/x":   "site",
		home + "/old.txt":      "new",
		home + "/bin/suid":     "x",
		home + "/.ssh/authkey": "key",
	})
	if err := unix.Chmod(filepath.Join(staging, home, "bin", "suid"), 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(staging, home, "fifo"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(staging, home), 0o750); err != nil {
		t.Fatal(err)
	}
	m, out := runMerge(t, s, staging, home)
	if m.failed != 0 {
		t.Fatalf("failed: %s", out)
	}
	if readFileT(t, filepath.Join(outside, "f")) != "outside" {
		t.Fatal("the restore followed the planted symlink")
	}
	if st, err := os.Lstat(filepath.Join(home, "data")); err != nil || !st.IsDir() || readFileT(t, filepath.Join(home, "data", "f")) != "restored" {
		t.Fatalf("data: %v %v", st, err)
	}
	if readFileT(t, filepath.Join(home, "old.txt")) != "new" || readFileT(t, filepath.Join(home, "keep.txt")) != "keep" || readFileT(t, filepath.Join(home, "data", "www", "x")) != "site" {
		t.Fatal("content")
	}
	if st, _ := os.Stat(filepath.Join(home, "bin", "suid")); st.Mode()&os.ModeSetuid != 0 {
		t.Fatal("set-user-ID arrived")
	}
	if _, err := os.Lstat(filepath.Join(home, "fifo")); err == nil {
		t.Fatal("a FIFO arrived")
	}
	if st, _ := os.Stat(home); st.Mode().Perm() != 0o750 {
		t.Fatalf("home mode %v: the snapshot's", st.Mode().Perm())
	}
	if m.skipped < 2 {
		t.Fatalf("skipped %d (%s)", m.skipped, m.skippedWhy())
	}
}

// config.yaml and the secret key are never restored in place, and the
// configuration directory keeps its owner and mode.
func TestRestoreInPlaceSparesRootsFiles(t *testing.T) {
	s := policyServer(t, debian13)
	conf := s.cfg.ConfigDir
	if err := os.Chmod(conf, 0o750); err != nil {
		t.Fatal(err)
	}
	for n, body := range map[string]string{"config.yaml": "live", "secret.key": "live key"} {
		if err := os.WriteFile(filepath.Join(conf, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := t.TempDir()
	stage(t, staging, map[string]string{
		conf + "/config.yaml":           "restored",
		conf + "/secret.key":            "restored key",
		conf + "/templates/site.tmpl":   "tmpl",
		conf + "/selinux/monopanel.cil": "(allow)",
	})
	if err := os.Chmod(filepath.Join(staging, conf), 0o777); err != nil {
		t.Fatal(err)
	}
	m, out := runMerge(t, s, staging, conf)
	if m.failed != 0 {
		t.Fatalf("failed: %s", out)
	}
	if readFileT(t, filepath.Join(conf, "config.yaml")) != "live" || readFileT(t, filepath.Join(conf, "secret.key")) != "live key" {
		t.Fatal("root's files were restored")
	}
	if readFileT(t, filepath.Join(conf, "templates", "site.tmpl")) != "tmpl" {
		t.Fatal("templates")
	}
	if st, _ := os.Stat(conf); st.Mode().Perm() != 0o750 {
		t.Fatalf("configuration directory mode %v", st.Mode().Perm())
	}
	if !strings.Contains(m.skippedWhy(), "root's") {
		t.Fatalf("skipped: %s", m.skippedWhy())
	}
}

// An include the snapshot does not have is reported, a file where a
// directory stands is a failure, and nothing else is touched.
func TestRestoreInPlaceEdges(t *testing.T) {
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, filepath.Join(home, "x"))
	staging := t.TempDir()
	stage(t, staging, map[string]string{home + "/x": "a file where a directory is"})
	m, out := runMerge(t, s, staging, home+"/x", filepath.Join(s.cfg.WWWRoot, "nobody"))
	if m.failed != 1 || !strings.Contains(out, "nothing at") {
		t.Fatalf("failed %d: %s", m.failed, out)
	}
	if st, err := os.Stat(filepath.Join(home, "x")); err != nil || !st.IsDir() {
		t.Fatal("the directory was replaced")
	}
}

// The in-place flow: restic is asked to restore into the staging directory,
// and only after it succeeded is the result moved into place.
func TestRestoreInPlaceFlow(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	staging := t.TempDir()
	var got []string
	restic := func(code int) toolRun {
		return func(args []string, _ io.Writer) (int, error) {
			got = args
			stage(t, staging, map[string]string{home + "/data/www/index.php": "restored"})
			return code, nil
		}
	}
	var out bytes.Buffer
	stages := []*inPlaceStage{{dir: staging, includes: []string{home}, fd: -1}}
	if code, err := s.stageAndMerge(ctx, restic(1), stages, "1a2b3c4d", &out); err != nil || code != 1 {
		t.Fatalf("a failed restic: %d %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(home, "data")); err == nil {
		t.Fatal("a failed restore must not be moved into place")
	}
	code, err := s.stageAndMerge(ctx, restic(0), stages, "1a2b3c4d", &out)
	if err != nil || code != 0 {
		t.Fatalf("restore: %d %v %s", code, err, out.String())
	}
	if want := []string{"restore", "1a2b3c4d", "--target", staging, "--include", home}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("restic argv %q, want %q", got, want)
	}
	if readFileT(t, filepath.Join(home, "data", "www", "index.php")) != "restored" {
		t.Fatal("not moved into place")
	}
}

const snapID = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"

// lsOutput is restic ls --json of a snapshot holding the nodes (name, type,
// path), in the order given.
func lsOutput(nodes ...[3]string) string {
	var b strings.Builder
	b.WriteString(`{"time":"2026-11-21T10:00:00Z","tree":"ab","paths":["/var/www"],"hostname":"h","id":"` + snapID + `","short_id":"1a2b3c4d","struct_type":"snapshot"}` + "\n")
	for _, n := range nodes {
		b.WriteString(`{"name":"` + n[0] + `","type":"` + n[1] + `","path":"` + n[2] + `","uid":0,"gid":0,"mode":2147484141,"struct_type":"node"}` + "\n")
	}
	return b.String()
}

// fakeRestic answers ls with listing and records the other calls.
func fakeRestic(listing string, calls *[][]string, restore func(args []string)) toolRun {
	return func(args []string, stdout io.Writer) (int, error) {
		*calls = append(*calls, args)
		if args[0] == "ls" {
			io.WriteString(stdout, listing)
			return 0, nil
		}
		if restore != nil {
			restore(args)
		}
		return 0, nil
	}
}

// A snapshot is restored only after its listing showed one entry per path
// and names restic restores safely; the restore names the snapshot's full
// id, so that the snapshot vetted is the one restored.
func TestVetSnapshot(t *testing.T) {
	s := policyServer(t, debian13)
	good := lsOutput(
		[3]string{"var", "dir", "/var"}, [3]string{"www", "dir", "/var/www"}, [3]string{"alex", "dir", "/var/www/alex"},
		[3]string{"index.php", "file", "/var/www/alex/index.php"}, [3]string{"current", "symlink", "/var/www/alex/current"},
		[3]string{"bob", "dir", "/var/www/bob"}, [3]string{"index.php", "file", "/var/www/bob/index.php"},
	)
	var calls [][]string
	id, code, err := s.vetSnapshot(fakeRestic(good, &calls, nil), "latest", nil)
	if err != nil || code != 0 || id != snapID {
		t.Fatalf("a good snapshot: %q %d %v", id, code, err)
	}
	if strings.Join(calls[0], " ") != "ls --json latest" {
		t.Fatalf("ls argv %q", calls[0])
	}
	// restic 0.17's message_type lines are read as well
	modern := strings.ReplaceAll(good, `"struct_type":"node"`, `"message_type":"node"`)
	modern = strings.ReplaceAll(modern, `"struct_type":"snapshot"`, `"message_type":"snapshot"`)
	if id, _, err := s.vetSnapshot(fakeRestic(modern, &calls, nil), "latest", nil); err != nil || id != snapID {
		t.Fatalf("message_type: %q %v", id, err)
	}
	for name, listing := range map[string]string{
		// the attack: an empty directory, a symlink of the same name, and
		// the directory again with what is to land in /etc
		"twins":                 lsOutput([3]string{"var", "dir", "/var"}, [3]string{"x", "dir", "/var/x"}, [3]string{"x", "symlink", "/var/x"}, [3]string{"x", "dir", "/var/x"}, [3]string{"shadow", "file", "/var/x/shadow"}),
		"twins after a subtree": lsOutput([3]string{"a", "dir", "/a"}, [3]string{"b", "file", "/a/b"}, [3]string{"c", "dir", "/c"}, [3]string{"a", "symlink", "/a"}),
		"dot dot":               lsOutput([3]string{"..", "dir", "/"}),
		"slash":                 lsOutput([3]string{"a/b", "file", "/a/b"}),
		"empty name":            lsOutput([3]string{"", "file", "/"}),
		"out of order":          lsOutput([3]string{"x", "file", "/nowhere/x"}),
		"no snapshot":           `{"name":"x","type":"file","path":"/x","struct_type":"node"}` + "\n",
		"broken json":           lsOutput() + `{"name":"x","type":` + "\n",
		"two snapshots":         lsOutput() + lsOutput(),
	} {
		_, _, err := s.vetSnapshot(fakeRestic(listing, &calls, nil), "latest", nil)
		var ae *Error
		if err == nil || (name != "no snapshot" && (!errors.As(err, &ae) || ae.Status != 403)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a listing that is not a snapshot's at all names no id
	if _, _, err := s.vetSnapshot(fakeRestic("", &calls, nil), "latest", nil); err == nil {
		t.Error("an empty listing")
	}
	// a notice of restic's own between the lines is no node
	if id, _, err := s.vetSnapshot(fakeRestic("repository 1a2b3c4d opened (version 2)\n"+good, &calls, nil), "latest", nil); err != nil || id != snapID {
		t.Errorf("a notice: %q %v", id, err)
	}
	// restic's own failure comes back as its exit status
	failing := func(args []string, _ io.Writer) (int, error) { return 1, nil }
	if _, code, err := s.vetSnapshot(failing, "latest", nil); err != nil || code != 1 {
		t.Errorf("a failing ls: %d %v", code, err)
	}
}

// A snapshot restored aside lands in a fresh directory: what an earlier
// restore left there — a symlink of another snapshot's under "latest" — is
// gone before restic writes, and the restore names the vetted full id.
func TestRestoreAsideStartsAfresh(t *testing.T) {
	s := policyServer(t, debian13)
	target := filepath.Join(t.TempDir(), "latest")
	outside := t.TempDir()
	mkdirs(t, target)
	if err := os.Symlink(outside, filepath.Join(target, "x")); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	run := fakeRestic(lsOutput([3]string{"x", "dir", "/x"}), &calls, func(args []string) {
		if _, err := os.Lstat(filepath.Join(target, "x")); err == nil {
			t.Error("the earlier restore was still there when restic ran")
		}
	})
	var out bytes.Buffer
	code, err := s.restoreAside(context.Background(), run, "latest", target, []string{"/x"}, &out)
	if err != nil || code != 0 {
		t.Fatalf("restore aside: %d %v", code, err)
	}
	if want := "restore " + snapID + " --target " + target + " --include /x"; strings.Join(calls[1], " ") != want {
		t.Fatalf("restore argv %q, want %q", calls[1], want)
	}
	if !strings.Contains(out.String(), "replacing the earlier restore") {
		t.Fatalf("output: %s", out.String())
	}
	// a snapshot that fails the check is not restored
	calls = nil
	twins := lsOutput([3]string{"x", "dir", "/x"}, [3]string{"x", "symlink", "/x"})
	if _, err := s.restoreAside(context.Background(), fakeRestic(twins, &calls, nil), "latest", target, nil, &out); err == nil || len(calls) != 1 {
		t.Fatalf("twins: %v, calls %q", err, calls)
	}
}

// What a restore aside leaves to be taken "by hand" holds no set-user-ID
// file and no FIFO or device node.
func TestRestoreAsideIsMadeSafe(t *testing.T) {
	s := policyServer(t, debian13)
	target := filepath.Join(t.TempDir(), "1a2b3c4d")
	var calls [][]string
	run := fakeRestic(lsOutput([3]string{"bin", "dir", "/bin"}), &calls, func(args []string) {
		stage(t, target, map[string]string{"/var/www/alex/bin/suid": "x", "/var/www/alex/index.php": "<?php"})
		if err := os.Chmod(filepath.Join(target, "var/www/alex/bin/suid"), 0o4755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(target, "var/www/alex/fifo"), 0o666); err != nil {
			t.Fatal(err)
		}
	})
	var out bytes.Buffer
	if code, err := s.restoreAside(context.Background(), run, "1a2b3c4d", target, nil, &out); err != nil || code != 0 {
		t.Fatalf("restore aside: %d %v", code, err)
	}
	if st, err := os.Stat(filepath.Join(target, "var/www/alex/bin/suid")); err != nil || st.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("set-user-ID left: %v %v", st, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "var/www/alex/fifo")); err == nil {
		t.Fatal("a FIFO left")
	}
	if readFileT(t, filepath.Join(target, "var/www/alex/index.php")) != "<?php" {
		t.Fatal("content")
	}
}

// A home is a directory: a snapshot's symlink in a home's place in the web
// root stays out, a symlink inside a home is the account's own business.
func TestRestoreInPlaceNoSymlinkForAHome(t *testing.T) {
	s := policyServer(t, debian13)
	www := s.cfg.WWWRoot
	staging := t.TempDir()
	stage(t, staging, map[string]string{www + "/bob/index.html": "bob"})
	if err := os.Symlink("/etc", filepath.Join(staging, www, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/5", filepath.Join(staging, www, "bob", "current")); err != nil {
		t.Fatal(err)
	}
	m, out := runMerge(t, s, staging, www)
	if m.failed != 0 {
		t.Fatalf("failed: %s", out)
	}
	if _, err := os.Lstat(filepath.Join(www, "evil")); err == nil {
		t.Fatal("a symlink took a home's place")
	}
	if target, err := os.Readlink(filepath.Join(www, "bob", "current")); err != nil || target != "releases/5" {
		t.Fatalf("the account's symlink: %q %v", target, err)
	}
}
