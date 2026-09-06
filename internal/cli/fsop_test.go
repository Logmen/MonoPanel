package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fsop runs after the helper dropped privileges to the client, with $HOME as
// the root of everything it may touch.
func runFsop(t *testing.T, stdin string, args ...string) error {
	t.Helper()
	if stdin != "" || args[0] == "write" {
		f, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(stdin); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = old; f.Close() }()
	}
	c := fsopCmd()
	c.SetArgs(args)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

func TestFsopTouchDoesNotClobber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := runFsop(t, "", "touch", "/notes.txt"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, "notes.txt"))
	if err != nil || len(b) != 0 {
		t.Fatalf("touch should leave an empty file: %q %v", b, err)
	}

	// Файл менеджера создаётся именно так, поэтому повтор не должен стирать
	// то, что уже лежит под этим именем.
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("важное"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runFsop(t, "", "touch", "notes.txt"); err == nil {
		t.Fatal("touch overwrote an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "notes.txt")); string(b) != "важное" {
		t.Fatalf("content changed: %q", b)
	}

	// Опустошение — отдельная операция, и она содержимое как раз убирает.
	if err := runFsop(t, "", "write", "notes.txt"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "notes.txt")); len(b) != 0 {
		t.Fatalf("write with empty stdin should empty the file: %q", b)
	}
}

// Пути ведут себя как в chroot у SFTP: ".." не отклоняется, а упирается в
// домашний каталог. Проверяем именно это — наружу не выходит ничего.
func TestFsopStaysInsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := filepath.Join(filepath.Dir(home), "outside.txt")

	for _, c := range [][2]string{
		{"../a.txt", "a.txt"},
		{"/../b.txt", "b.txt"},
		{"data/../../c.txt", "c.txt"},
	} {
		if err := runFsop(t, "", "touch", c[0]); err != nil {
			t.Errorf("touch %q: %v", c[0], err)
			continue
		}
		if _, err := os.Stat(filepath.Join(home, c[1])); err != nil {
			t.Errorf("touch %q should have landed on %s inside the home: %v", c[0], c[1], err)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a file was created outside the home directory")
	}
}
