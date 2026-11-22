package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// update-run writes its state.json into the API's DataDir/updates as root.
// The directory is made when it is missing and used as it is otherwise: in
// the data directory the API can put any directory under that name — one of
// root's 0700 ones too, which a chmod to 0755 would open up — or a symlink.
func TestInstallFileIn(t *testing.T) {
	data := t.TempDir()
	updates := filepath.Join(data, "updates")
	if err := InstallFileIn(updates, "state.json", []byte("one\n"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(updates)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("made: %v %v", st, err)
	}
	if b, err := os.ReadFile(filepath.Join(updates, "state.json")); err != nil || string(b) != "one\n" {
		t.Fatalf("state: %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(updates, "state.json")); fi.Mode().Perm() != 0o644 {
		t.Fatalf("state mode %v", fi.Mode().Perm())
	}

	// an existing directory keeps its mode
	if err := os.Chmod(updates, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallFileIn(updates, "state.json", []byte("two\n"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(updates); st.Mode().Perm() != 0o700 {
		t.Fatalf("the directory was re-moded to %v", st.Mode().Perm())
	}
	if left := entries(t, updates); left != "state.json" {
		t.Fatalf("left %q", left)
	}

	// a symlink under the file's name is replaced, not written through
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("root's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(updates, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(updates, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := InstallFileIn(updates, "state.json", []byte("three\n"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "root's\n" {
		t.Fatalf("written through a symlink: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(updates, "state.json")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("state.json: %v %v", fi, err)
	}

	// and a symlink in place of the directory is refused
	elsewhere := t.TempDir()
	if err := os.RemoveAll(updates); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, updates); err != nil {
		t.Fatal(err)
	}
	if err := InstallFileIn(updates, "state.json", []byte("four\n"), 0o755, 0o644); !errors.Is(err, errSymlinkInPath) {
		t.Fatalf("a symlinked directory: %v", err)
	}
	if left := entries(t, elsewhere); left != "" {
		t.Fatalf("written below a symlink: %q", left)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := InstallFileIn(updates, bad, nil, 0o755, 0o644); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}
