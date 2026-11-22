package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/osprofile"
)

// The package ships the modprobe rule, word for word what mp setup writes on
// an install without it, as /usr/lib/modprobe.d/monopanel.conf.
func TestModprobeRuleIsThePackages(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "modprobe.d", "monopanel.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != osprofile.ModprobeConf {
		t.Errorf("packaging/modprobe.d/monopanel.conf differs from mp setup's:\n%s\n---\n%s", b, osprofile.ModprobeConf)
	}
	nfpm, err := os.ReadFile(filepath.Join("..", "..", "packaging", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nfpm), "src: ./packaging/modprobe.d/monopanel.conf\n    dst: "+osprofile.ModprobePackageFile+"\n") {
		t.Error("the package does not ship the modprobe rule as " + osprofile.ModprobePackageFile)
	}
	// /bin/false, not /usr/bin/false: Debian without the merged /usr has
	// false in /bin only.
	for _, m := range osprofile.BlockedModules {
		if !strings.Contains(osprofile.ModprobeConf, "\ninstall "+m+" /bin/false\n") {
			t.Errorf("the rule does not keep %s from loading", m)
		}
	}
	if len(osprofile.BlockedModules) != 4 {
		t.Errorf("modules: %v", osprofile.BlockedModules)
	}
}

// mp setup writes /etc/modprobe.d/monopanel.conf only where the package's
// file is missing, and never over anything of that name: a file there is
// the administrator's word on which modules may load, a symlink is not
// written through.
func TestModprobeOnlyOnBinaryInstalls(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "usr/lib/modprobe.d/monopanel.conf")
	local := filepath.Join(dir, "etc/modprobe.d/monopanel.conf")

	// From the binary alone: the rule lands in /etc.
	wrote, err := ensureModprobe(pkg, local)
	if err != nil || !wrote {
		t.Fatalf("binary install: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(local); string(b) != osprofile.ModprobeConf {
		t.Fatalf("written:\n%s", b)
	}
	if st, _ := os.Stat(local); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode())
	}
	// Run again, after the administrator allowed sctp: their file stays.
	edited := strings.Replace(osprofile.ModprobeConf, "install sctp /bin/false\n", "", 1)
	if err := os.WriteFile(local, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("again: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(local); string(b) != edited {
		t.Fatal("the administrator's file was replaced")
	}

	// From the package: nothing is written.
	os.Remove(local)
	if err := os.MkdirAll(filepath.Dir(pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte(osprofile.ModprobeConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("package install: wrote=%v err=%v", wrote, err)
	}
	if _, err := os.Lstat(local); err == nil {
		t.Fatal("mp setup wrote the rule next to the package's")
	}

	// A symlink in /etc's place is not written through.
	os.Remove(pkg)
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, local); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("symlink: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("written through the symlink")
	}
}
