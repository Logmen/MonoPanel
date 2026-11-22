package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// Below a tree it may write to, a program makes files, directories and
// symlinks — never a FIFO or a socket, nor a device node, which the test
// cannot even try without CAP_MKNOD (the rights are in TestRights):
// restic needs none of them, and the agent refuses a snapshot that would
// have a restore make one.
func TestReadWriteMakesNoNodes(t *testing.T) {
	needLandlock(t)
	// a unix socket's path is short: t.TempDir() of a long test name is not
	tree, err := os.MkdirTemp("", "mpsb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tree) })
	out, code := run(t, sandboxed(t, []string{"--rw", tree},
		"mkdir", filepath.Join(tree, "d"),
		"write", filepath.Join(tree, "d", "f"),
		"mkfifo", filepath.Join(tree, "fifo"),
		"mksock", filepath.Join(tree, "s"),
	))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantLines(t, out,
		"mkdir "+filepath.Join(tree, "d")+": ok",
		"write "+filepath.Join(tree, "d", "f")+": ok",
		"mkfifo "+filepath.Join(tree, "fifo")+": denied",
		"mksock "+filepath.Join(tree, "s")+": denied",
	)
	for _, n := range []string{"fifo", "s"} {
		if _, err := os.Lstat(filepath.Join(tree, n)); err == nil {
			t.Errorf("%s was made", n)
		}
	}
}
