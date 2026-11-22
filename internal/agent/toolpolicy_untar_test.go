package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/peercred"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func makeTar(t *testing.T, gz bool, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = tar.NewWriter(zw)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, ModTime: time.Unix(1700000000, 0)}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Typeflag == tar.TypeChar || h.Typeflag == tar.TypeBlock {
			h.Devmajor, h.Devminor = 8, 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func extractInto(t *testing.T, dest string, o untarOpts, archive []byte) (int, string) {
	t.Helper()
	fd, err := openDirNoFollow(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var out bytes.Buffer
	code := untar(context.Background(), bytes.NewReader(archive), fd, dest, o, &out)
	return code, out.String()
}

func readFileT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What GNU tar -x --no-same-owner --strip-components does for the panel's
// archives, the agent does too.
func TestUntarExtractsLikeTar(t *testing.T) {
	dest := t.TempDir()
	archive := makeTar(t, true,
		tarEntry{name: "wordpress/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "wordpress/index.php", body: "<?php", mode: 0o644},
		tarEntry{name: "wordpress/wp-admin/", typ: tar.TypeDir, mode: 0o750},
		tarEntry{name: "wordpress/wp-admin/x.php", body: "x", mode: 0o600},
		tarEntry{name: "wordpress/bin/run", body: "#!/bin/sh", mode: 0o755},
		tarEntry{name: "wordpress/link", typ: tar.TypeSymlink, link: "index.php"},
		tarEntry{name: "wordpress/hard", typ: tar.TypeLink, link: "wordpress/index.php"},
	)
	code, out := extractInto(t, dest, untarOpts{gzip: true, strip: 1}, archive)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if readFileT(t, filepath.Join(dest, "index.php")) != "<?php" || readFileT(t, filepath.Join(dest, "wp-admin", "x.php")) != "x" {
		t.Fatal("content")
	}
	for p, want := range map[string]os.FileMode{"index.php": 0o644, "wp-admin": 0o750 | os.ModeDir, "wp-admin/x.php": 0o600, "bin/run": 0o755} {
		st, err := os.Lstat(filepath.Join(dest, p))
		if err != nil || st.Mode() != want {
			t.Errorf("%s: mode %v, want %v (%v)", p, st.Mode(), want, err)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dest, "link")); l != "index.php" {
		t.Errorf("symlink %q", l)
	}
	a, _ := os.Stat(filepath.Join(dest, "index.php"))
	b, _ := os.Stat(filepath.Join(dest, "hard"))
	if !os.SameFile(a, b) {
		t.Error("hard link")
	}
	if st, _ := os.Stat(filepath.Join(dest, "index.php")); !st.ModTime().Equal(time.Unix(1700000000, 0)) {
		t.Errorf("mtime %v", st.ModTime())
	}
}

// Members are matched before stripping (OpenCart's upload/).
func TestUntarMembers(t *testing.T) {
	dest := t.TempDir()
	archive := makeTar(t, false,
		tarEntry{name: "upload/index.php", body: "shop"},
		tarEntry{name: "upload/admin/x.php", body: "admin"},
		tarEntry{name: "install.txt", body: "no"},
	)
	code, out := extractInto(t, dest, untarOpts{strip: 1, members: []string{"upload"}}, archive)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if readFileT(t, filepath.Join(dest, "index.php")) != "shop" || readFileT(t, filepath.Join(dest, "admin", "x.php")) != "admin" {
		t.Fatal("upload/ not extracted")
	}
	if _, err := os.Stat(filepath.Join(dest, "install.txt")); err == nil {
		t.Fatal("a member outside upload/ was extracted")
	}
	code, out = extractInto(t, t.TempDir(), untarOpts{members: []string{"missing"}}, archive)
	if code == 0 || !strings.Contains(out, "Not found in archive") {
		t.Fatalf("a missing member: exit %d %s", code, out)
	}
}

// What GNU tar as root would do with an archive of the API's choosing — a
// set-user-ID binary, a device node, a path through a symlink, a hard link
// to /etc/shadow — the agent does not.
func TestUntarRefusesWhatRootMustNotDo(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	// an account left symlinks in its tree
	if err := os.Symlink(outside, filepath.Join(dest, "planted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dest, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "dir-link")); err != nil {
		t.Fatal(err)
	}
	archive := makeTar(t, false,
		tarEntry{name: "suid", body: "x", mode: 0o4755},
		tarEntry{name: "sgid", body: "x", mode: 0o2755},
		tarEntry{name: "sticky", body: "x", mode: 0o1644},
		tarEntry{name: "sda", typ: tar.TypeBlock, mode: 0o666},
		tarEntry{name: "mem", typ: tar.TypeChar, mode: 0o666},
		tarEntry{name: "fifo", typ: tar.TypeFifo, mode: 0o666},
		tarEntry{name: "planted/victim", body: "pwned"},
		tarEntry{name: "planted/new", body: "pwned"},
		tarEntry{name: "../escape", body: "pwned"},
		tarEntry{name: "a/../../escape2", body: "pwned"},
		tarEntry{name: "/abs/file", body: "inside"},
		// the archive's own symlink out, then a file through it
		tarEntry{name: "out", typ: tar.TypeSymlink, link: outside},
		tarEntry{name: "out/victim", body: "pwned"},
		// a hard link to a file this archive did not make
		tarEntry{name: "shadow", typ: tar.TypeLink, link: "../" + victim},
		tarEntry{name: "shadow2", typ: tar.TypeLink, link: victim},
		tarEntry{name: "shadow3", typ: tar.TypeLink, link: "file-link"},
		// a file in place of a planted symlink replaces the link itself
		tarEntry{name: "file-link", body: "replaced"},
		// so does a directory
		tarEntry{name: "dir-link/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "dir-link/inside", body: "inside"},
	)
	code, out := extractInto(t, dest, untarOpts{}, archive)
	if code != 2 {
		t.Fatalf("exit %d, want 2 after the refusals: %s", code, out)
	}
	if readFileT(t, victim) != "intact" {
		t.Fatal("the file outside was written")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("something was made outside: %v", entries)
	}
	for _, n := range []string{"suid", "sgid", "sticky"} {
		st, err := os.Lstat(filepath.Join(dest, n))
		if err != nil || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s: mode %v (%v)", n, st.Mode(), err)
		}
	}
	for _, n := range []string{"sda", "mem", "fifo", "shadow", "shadow2", "shadow3", "escape", "escape2"} {
		if _, err := os.Lstat(filepath.Join(dest, n)); err == nil {
			t.Errorf("%s was made", n)
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "escape")); err == nil {
		t.Error("../escape was made")
	}
	if readFileT(t, filepath.Join(dest, "abs", "file")) != "inside" {
		t.Error("a leading / is stripped, as by GNU tar")
	}
	if st, err := os.Lstat(filepath.Join(dest, "file-link")); err != nil || !st.Mode().IsRegular() {
		t.Errorf("file-link: %v %v", st, err)
	}
	if st, err := os.Lstat(filepath.Join(dest, "dir-link")); err != nil || !st.IsDir() || readFileT(t, filepath.Join(dest, "dir-link", "inside")) != "inside" {
		t.Errorf("dir-link: %v %v", st, err)
	}
	if !strings.Contains(out, "device nodes or FIFOs left out") {
		t.Errorf("output: %s", out)
	}
}

// The destination itself keeps its owner and mode (a migration's "./").
func TestUntarKeepsTheDestination(t *testing.T) {
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o711); err != nil {
		t.Fatal(err)
	}
	archive := makeTar(t, false, tarEntry{name: "./", typ: tar.TypeDir, mode: 0o777}, tarEntry{name: "./data/", typ: tar.TypeDir, mode: 0o750}, tarEntry{name: "./data/f", body: "f"})
	if code, out := extractInto(t, dest, untarOpts{}, archive); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o711 {
		t.Fatalf("destination mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dest, "data")); st.Mode().Perm() != 0o750 {
		t.Fatalf("data mode %v", st.Mode().Perm())
	}
}

// A migration's tar reaches the agent through /v1/stream/in and lands in
// the home; the same stream aimed at /etc is refused before anything is read.
func TestStreamInUntar(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext}
	go srv.Serve(&peercred.Listener{Listener: ln}) //nolint:errcheck // closed below
	t.Cleanup(func() { srv.Close() })
	cl := NewClient(sock)
	archive := makeTar(t, false, tarEntry{name: "./data/www/x/index.php", body: "<?php echo 1;"}, tarEntry{name: "./data/www/x/run", body: "x", mode: 0o4755})
	res, err := cl.StreamIn(ctx, &StreamRequest{Name: "tar", Args: []string{"--extract", "--file", "-", "--directory", home, "--no-same-owner", "--warning=no-timestamp"}}, bytes.NewReader(archive))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("stream in: %+v %v", res, err)
	}
	if readFileT(t, filepath.Join(home, "data", "www", "x", "index.php")) != "<?php echo 1;" {
		t.Fatal("content")
	}
	if st, _ := os.Stat(filepath.Join(home, "data", "www", "x", "run")); st.Mode()&os.ModeSetuid != 0 {
		t.Fatal("set-user-ID survived")
	}
	_, err = cl.StreamIn(ctx, &StreamRequest{Name: "tar", Args: []string{"--extract", "--file", "-", "--directory", "/etc", "--no-same-owner"}}, bytes.NewReader(archive))
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("into /etc: %v", err)
	}
}

// A home streamed out for a migration is archived by GNU tar from the
// directory the agent opened (/proc/self/fd/3): the archive holds the home.
func TestStreamOutTarFromThePinnedDirectory(t *testing.T) {
	if _, err := resolveTool("tar"); err != nil {
		t.Skip("no tar")
	}
	ctx := context.Background()
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, filepath.Join(home, "data", "www"), filepath.Join(home, "data", "logs"))
	if err := os.WriteFile(filepath.Join(home, "data", "www", "index.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "logs", "old.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext}
	go srv.Serve(&peercred.Listener{Listener: ln}) //nolint:errcheck // closed below
	t.Cleanup(func() { srv.Close() })
	var out bytes.Buffer
	args := []string{"--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--exclude=./data/logs", "."}
	if err := NewClient(sock).StreamOut(ctx, &StreamRequest{Name: "tar", Args: args}, &out); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names[h.Name] = true
	}
	if !names["./data/www/index.php"] || names["./data/logs/old.log"] {
		t.Fatalf("archive: %v", names)
	}
}
