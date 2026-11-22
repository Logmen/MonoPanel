package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/config"
)

// mp setup's tmpfiles file takes the place of the package's at boot (the
// same name in /etc wins), so for the default layout it says what the
// package's says: the run directory, and the php-fpm socket directory in it
// root's from boot.
func TestTmpfilesAreThePackages(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "tmpfiles.d", "monopanel.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if got, want := tmpfilesConf(config.Default()), strings.Join(lines, "\n")+"\n"; got != want {
		t.Errorf("mp setup writes\n%s\nthe package ships\n%s", got, want)
	}
	if !strings.Contains(tmpfilesConf(config.Default()), "\nd /run/monopanel/php 0755 root root -\n") {
		t.Error("the php-fpm socket directory is not root's from boot")
	}
	cfg := config.Default()
	cfg.RunDir, cfg.ServiceGroup = "/run/panel", "panel"
	if got := tmpfilesConf(cfg); got != "d /run/panel 1771 root panel -\nd /run/panel/php 0755 root root -\n" {
		t.Errorf("another layout: %q", got)
	}
}

// An install without the package keeps php/'s line of its own where the API
// of 0.8.19 kept it: going back below 0.8.20 there runs no preremove, and the
// older version's mp setup writes monopanel.conf without the line. With the
// package nothing is written (its preremove writes the file when it goes
// back); a file there already, whatever it holds, and a symlink stay.
func TestKeepPHPTmpfiles(t *testing.T) {
	dir := t.TempDir()
	pkg, php := filepath.Join(dir, "usr-lib-monopanel.conf"), filepath.Join(dir, "monopanel-php.conf")
	cfg := config.Default()
	cfg.RunDir = "/srv/run"
	if wrote, err := keepPHPTmpfiles(php, pkg, cfg); err != nil || !wrote {
		t.Fatalf("without the package: %v %v", wrote, err)
	}
	if b, _ := os.ReadFile(php); string(b) != "d /srv/run/php 0755 root root -\n" || !strings.HasSuffix(tmpfilesConf(cfg), string(b)) {
		t.Fatalf("written: %q", b)
	}
	if st, _ := os.Stat(php); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode())
	}
	if err := os.WriteFile(php, []byte("# the administrator's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := keepPHPTmpfiles(php, pkg, cfg); err != nil || wrote {
		t.Fatalf("over a file there: %v %v", wrote, err)
	}
	if b, _ := os.ReadFile(php); string(b) != "# the administrator's\n" {
		t.Fatalf("the file there was rewritten: %q", b)
	}
	os.Remove(php)
	victim := filepath.Join(dir, "victim")
	if err := os.Symlink(victim, php); err != nil {
		t.Fatal(err)
	}
	if wrote, err := keepPHPTmpfiles(php, pkg, cfg); err != nil || wrote {
		t.Fatalf("a symlink: %v %v", wrote, err)
	}
	if _, err := os.Lstat(victim); err == nil {
		t.Fatal("written through the symlink")
	}
	os.Remove(php)
	if err := os.WriteFile(pkg, []byte(tmpfilesConf(config.Default())), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := keepPHPTmpfiles(php, pkg, cfg); err != nil || wrote {
		t.Fatalf("with the package: %v %v", wrote, err)
	}
	// The package's scripts look after the same file in the same directory.
	if phpTmpfilesFile != tmpfilesDir+"/monopanel-php.conf" || packageTmpfilesFile != "/usr/lib/tmpfiles.d/monopanel.conf" {
		t.Errorf("tmpfiles: %s, %s", phpTmpfilesFile, packageTmpfilesFile)
	}
	if pre := packageScript(t, "preremove.sh"); !strings.Contains(pre, "\nphp=$tmpfiles/monopanel-php.conf\n") {
		t.Error("the preremove writes php/'s line elsewhere than mp setup does")
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dst: "+packageTmpfilesFile+"\n") {
		t.Errorf("the package does not ship %s", packageTmpfilesFile)
	}
}
