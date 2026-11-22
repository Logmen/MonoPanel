package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fsop run executes nothing but a PHP interpreter, and extract can drop a
// distribution's top-level folder.
func TestFsopRunAllowsOnlyPHPAndStripsComponents(t *testing.T) {
	home := "/var/www/alex"
	for bin, ok := range map[string]bool{
		"php": true, home + "/data/bin/php": true, "/usr/bin/php8.4": true, "/usr/bin/php84": true, "/opt/remi/php84/root/usr/bin/php": true,
		"/bin/sh": false, "/usr/bin/php-cgi": false, home + "/data/www/x/php": false, "/usr/bin/python3": false,
	} {
		if got := phpBinaryAllowed(home, bin); got != ok {
			t.Errorf("phpBinaryAllowed(%q) = %v, want %v", bin, got, ok)
		}
	}
	for _, c := range []struct {
		name string
		n    int
		want string
	}{
		{"wordpress/wp-config.php", 1, "wp-config.php"},
		{"wordpress/", 1, ""},
		{"opencart-4.1.0.4/upload/index.php", 2, "upload/index.php"[len("upload/"):]},
		{"opencart-4.1.0.4/upload/", 2, ""},
		{"index.php", 0, "index.php"},
		{"./bitrix/.settings.php", 1, "bitrix/.settings.php"},
	} {
		if got := stripComponents(c.name, c.n); got != c.want {
			t.Errorf("stripComponents(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
}

// fsop tail gives the size of a regular file and its last bytes, says that
// a file is missing, and refuses what is no regular file — a FIFO among
// them, without waiting for a writer.
func TestFsopTail(t *testing.T) {
	dir := t.TempDir()
	read := func(p string, n int64) (fsopTailOut, error) {
		t.Helper()
		var b bytes.Buffer
		if err := fsopTail(p, n, &b); err != nil {
			return fsopTailOut{}, err
		}
		var out fsopTailOut
		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, nil
	}
	log := filepath.Join(dir, "example.com.access.log")
	if err := os.WriteFile(log, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := read(log, 6); err != nil || got.Size != 14 || got.Content != "three\n" || got.Missing {
		t.Errorf("the tail: %+v %v", got, err)
	}
	if got, err := read(log, 100); err != nil || got.Content != "one\ntwo\nthree\n" {
		t.Errorf("more than there is: %+v %v", got, err)
	}
	if got, err := read(filepath.Join(dir, "gone.log"), 10); err != nil || !got.Missing {
		t.Errorf("a missing file: %+v %v", got, err)
	}
	fifo := filepath.Join(dir, "fifo.log")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := read(fifo, 10); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a FIFO holds the read")
	}
	if _, err := read(dir, 10); err == nil {
		t.Error("a directory was read")
	}
}
