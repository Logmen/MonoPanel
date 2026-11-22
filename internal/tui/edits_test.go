package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The private copies of the edit flow that a killed TUI leaves behind.

func TestEditDirNamesItsOwner(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := newEditDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := editDirOwner(filepath.Base(dir)); got != os.Getpid() {
		t.Errorf("%s: owner %d, want %d", dir, got, os.Getpid())
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the copy's directory is private: %v %v", fi.Mode(), err)
	}
	for name, want := range map[string]int{"mp-edit-123456": 0, "mp-edit-x-1": 0, "mp-edit-0-1": 0, "mp-edit-42-987": 42} {
		if got := editDirOwner(name); got != want {
			t.Errorf("editDirOwner(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestSameBinaryAfterAnUpdate: a TUI started before mp was updated runs the
// replaced file; it is still a running TUI whose copy must stay.
func TestSameBinaryAfterAnUpdate(t *testing.T) {
	for _, c := range []struct {
		exe, self string
		want      bool
	}{
		{"/usr/local/bin/mp", "/usr/local/bin/mp", true},
		{"/usr/local/bin/mp (deleted)", "/usr/local/bin/mp", true},
		{"/usr/bin/vim", "/usr/local/bin/mp", false},
		{"/usr/local/bin/mp.old", "/usr/local/bin/mp", false},
	} {
		if got := sameBinary(c.exe, c.self); got != c.want {
			t.Errorf("sameBinary(%q, %q) = %v", c.exe, c.self, got)
		}
	}
}

// deadPID is the PID of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// liveTUI starts this test binary as a stand-in for another running TUI.
func liveTUI(t *testing.T) int {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), "MP_TUI_TEST_SLEEP=1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	return c.Process.Pid
}

func TestSweepEdits(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()
	mk := func(name string, age time.Duration, files ...string) string {
		t.Helper()
		dir := filepath.Join(tmp, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		for _, f := range files {
			p := filepath.Join(dir, f)
			if err := os.WriteFile(p, []byte("edited"), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(p, at, at) //nolint:errcheck // the test's own file
		}
		os.Chtimes(dir, at, at) //nolint:errcheck // the test's own directory
		return dir
	}
	dead, live := deadPID(t), liveTUI(t)
	if alive(dead) || !alive(live) {
		t.Fatalf("alive: dead %v, live %v", alive(dead), alive(live))
	}
	day := 24 * time.Hour
	old := mk(fmt.Sprintf("mp-edit-%d-1", dead), 2*day, "custom.conf")
	young := mk(fmt.Sprintf("mp-edit-%d-2", dead), time.Hour, "notes.txt", ".notes.txt.swp", "notes.txt~")
	empty := mk(fmt.Sprintf("mp-edit-%d-3", dead), time.Hour)
	junkOnly := mk(fmt.Sprintf("mp-edit-%d-11", dead), time.Hour, ".x.swp")
	legacyOld := mk("mp-edit-4", 2*day, "a.txt")
	legacyYoung := mk("mp-edit-5", time.Hour, "b.txt")
	running := mk(fmt.Sprintf("mp-edit-%d-6", live), 2*day, "c.txt")
	shared := mk("mp-edit-7", 2*day, "d.txt")
	os.Chmod(shared, 0o755) //nolint:errcheck // not a private copy: not ours to touch
	link := filepath.Join(tmp, "mp-edit-8")
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	unrelated := mk("other-9", 2*day, "e.txt")
	// An old directory whose copy was just written again (an editor saved it).
	touched := mk(fmt.Sprintf("mp-edit-%d-10", dead), 2*day, "f.txt")
	os.Chtimes(filepath.Join(touched, "f.txt"), now, now) //nolint:errcheck // the test's own file

	kept := sweepEdits(tmp, now)
	slices.Sort(kept)
	want := []string{filepath.Join(young, "notes.txt"), filepath.Join(legacyYoung, "b.txt"), filepath.Join(touched, "f.txt")}
	slices.Sort(want)
	if !slices.Equal(kept, want) {
		t.Errorf("reported %q, want %q", kept, want)
	}
	gone := func(p string) bool { _, err := os.Lstat(p); return os.IsNotExist(err) }
	for _, p := range []string{old, empty, junkOnly, legacyOld} {
		if !gone(p) {
			t.Errorf("%s: a stale copy is removed", p)
		}
	}
	for _, p := range []string{young, legacyYoung, running, shared, link, unrelated, touched} {
		if gone(p) {
			t.Errorf("%s must stay", p)
		}
	}
	if sweepEdits(filepath.Join(tmp, "missing"), now) != nil {
		t.Error("no directory, nothing to say")
	}
}

func TestLeftoversOnTheMenu(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	m.leftovers = []string{"/tmp/mp-edit-1-2/custom.conf"}
	m.refreshMenu()
	m.width = 200
	if v := viewText(m); !strings.Contains(v, "/tmp/mp-edit-1-2/custom.conf") || !strings.Contains(v, "may not have been saved") {
		t.Errorf("the main menu names the kept copy:\n%s", v)
	}
}
