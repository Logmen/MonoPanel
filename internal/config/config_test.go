package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Listen != ":8443" || cfg.DBPath() != "/var/lib/monopanel/panel.db" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadPartialFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("data_dir: /tmp/mp\nweb:\n  listen: \"127.0.0.1:9443\"\n"), 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/mp" || cfg.Web.Listen != "127.0.0.1:9443" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.RunDir != "/run/monopanel" || cfg.Jobs.Workers != 2 {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if cfg.APISocket() != "/run/monopanel/api.sock" || cfg.DBPath() != "/tmp/mp/panel.db" {
		t.Fatalf("derived paths wrong: %s %s", cfg.APISocket(), cfg.DBPath())
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "c.yaml")
	cfg := Default()
	cfg.Web.Hostname = "panel.example.com"
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Web.Hostname != "panel.example.com" || len(back.WritePrefixes()) != len(DefaultAllowedWritePrefixes) {
		t.Fatalf("round trip lost data: %+v", back)
	}
	back.Agent.ExtraWritePrefixes = []string{"/opt/custom/"}
	if n := len(back.WritePrefixes()); n != len(DefaultAllowedWritePrefixes)+1 {
		t.Fatalf("extras not appended: %d", n)
	}
}
