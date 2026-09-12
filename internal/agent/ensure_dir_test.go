package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// EnsureDir names every level it creates, so each can get the owner: a
// root-owned intermediate directory used to block the service user.
func TestEnsureDirOwnsEveryCreatedLevel(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "acme", "webroot", ".well-known", "acme-challenge")
	if got := missingLevels(leaf); len(got) != 3 || got[0] != filepath.Join(root, "acme") || got[2] != filepath.Join(root, "acme", "webroot", ".well-known") {
		t.Fatalf("missing levels topmost first: %v", got)
	}
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	created, err := EnsureDir(DirSpec{Path: leaf, Mode: 0o755, Owner: me.Username})
	if err != nil || !created {
		t.Fatalf("EnsureDir: created=%v err=%v", created, err)
	}
	for _, d := range append(missingLevels(filepath.Join(leaf, "x")), leaf) {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Fatalf("%s must be a directory: %v", d, err)
		}
	}
	if created, err := EnsureDir(DirSpec{Path: leaf, Mode: 0o755}); err != nil || created {
		t.Fatalf("second call must find it: created=%v err=%v", created, err)
	}
}
