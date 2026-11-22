package setup

import (
	"os"
	"path/filepath"
	"testing"

	"monopanel/internal/config"
)

// mp setup makes the panel's certificate as root in the API's own data
// directory: a symlink the API left there in place of the key is replaced,
// never written through, and the pair is made again; plain files stay.
func TestSelfSignedReplacesAPlantedSymlink(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	tls := filepath.Join(cfg.DataDir, "tls")
	if err := os.Mkdir(tls, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(victim, []byte("root:x:0:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, key := cfg.TLSCertPath(), cfg.TLSKeyPath()
	if err := os.Symlink(victim, key); err != nil {
		t.Fatal(err)
	}
	created, err := ensureSelfSigned(cfg, cert, key, []string{"panel.example.com"})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "root:x:0:0\n" {
		t.Fatalf("written through the symlink: %q", b)
	}
	for _, p := range []string{cert, key} {
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Errorf("the key: mode %v", st.Mode())
	}
	before, _ := os.ReadFile(key)
	if created, err := ensureSelfSigned(cfg, cert, key, []string{"panel.example.com"}); err != nil || created {
		t.Fatalf("again: created=%v err=%v", created, err)
	}
	if after, _ := os.ReadFile(key); string(after) != string(before) {
		t.Error("an existing key was replaced")
	}
}

// The database and its journals are opened by name as root: anything but a
// plain file there stops mp setup.
func TestDBFilesMustBePlain(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "panel.db")
	if err := checkDBFiles(db); err != nil {
		t.Fatalf("no database yet: %v", err)
	}
	if err := os.WriteFile(db, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkDBFiles(db); err != nil {
		t.Fatalf("a plain database: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), db+"-wal"); err != nil {
		t.Fatal(err)
	}
	if err := checkDBFiles(db); err == nil {
		t.Fatal("a symlinked journal was accepted")
	}
}
