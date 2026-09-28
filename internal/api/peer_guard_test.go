package api

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A fake /proc: each process is its name and parent.
func fakeProc(t *testing.T, procs map[int][2]string) {
	t.Helper()
	root := t.TempDir()
	for pid, np := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\t"+np[0]+"\nState:\tS (sleeping)\nPPid:\t"+np[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

func TestFromWebWorker(t *testing.T) {
	fakeProc(t, map[int][2]string{
		1:   {"systemd", "0"},
		100: {"php-fpm8.4", "1"},   // master
		200: {"php-fpm8.4", "100"}, // pool worker of the site's account
		300: {"sh", "200"},         // exec() from PHP
		301: {"mp", "300"},         // … running the CLI
		310: {"php-fpm", "1"},      // Remi's name for the same thing
		311: {"mp", "310"},
		400: {"sshd", "1"},
		401: {"bash", "400"},
		402: {"mp", "401"}, // a person on SSH
		500: {"cron", "1"},
		501: {"mp", "500"}, // the account's cron job
		600: {"httpd", "1"},
		601: {"mp", "600"},
		700: {"mp", "9999"}, // parent already gone
	})
	for pid, want := range map[int]bool{301: true, 200: true, 311: true, 601: true, 402: false, 501: false, 700: false, 8888: false} {
		if got := fromWebWorker(pid); got != want {
			t.Errorf("fromWebWorker(%d) = %v, want %v", pid, got, want)
		}
	}
}
