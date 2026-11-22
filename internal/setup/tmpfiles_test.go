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
