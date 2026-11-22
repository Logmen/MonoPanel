package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// EnsureDir names every level it creates, so each can get the owner: a
// root-owned intermediate directory used to block the service user.
func TestEnsureDirOwnsEveryCreatedLevel(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "acme", "webroot", ".well-known", "acme-challenge")
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	created, made, err := ensureDirAt(DirSpec{Path: leaf, Mode: 0o755, Owner: me.Username}, &dirTrust{rootUID: os.Geteuid()}, followAll)
	if err != nil || !created {
		t.Fatalf("ensureDirAt: created=%v err=%v", created, err)
	}
	want := []string{filepath.Join(root, "acme"), filepath.Join(root, "acme", "webroot"), filepath.Join(root, "acme", "webroot", ".well-known"), leaf}
	if len(made) != len(want) {
		t.Fatalf("made %v, want %v", made, want)
	}
	uid, _ := strconv.Atoi(me.Uid)
	for i, d := range want {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() || made[i] != d {
			t.Fatalf("%s must be a directory made in order: %v %v", d, made, err)
		}
		if int(st.Sys().(*syscall.Stat_t).Uid) != uid {
			t.Errorf("%s: not given to the owner", d)
		}
	}
	if created, err := EnsureDir(DirSpec{Path: leaf, Mode: 0o755}); err != nil || created {
		t.Fatalf("second call must find it: created=%v err=%v", created, err)
	}
	// The special bits are kept: mp setup makes the log and run directories
	// sticky.
	if _, err := EnsureDir(DirSpec{Path: leaf, Mode: 0o1771}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(leaf); st.Mode()&os.ModeSticky == 0 || st.Mode().Perm() != 0o771 {
		t.Fatalf("mode %v, want sticky 0771", st.Mode())
	}
}
