package updater

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseKeyResolution(t *testing.T) {
	if _, err := ParsePublicKey(BuiltinKey()); err != nil {
		t.Fatalf("built-in key: %v", err)
	}
	for _, tc := range []struct{ configured, key, source string }{
		{"", BuiltinKey(), "builtin"},
		{"  \n", BuiltinKey(), "builtin"},
		{KeyOff, "", ""},
		{" none ", "", ""},
		{"AAAA", "AAAA", "config"},
	} {
		key, source := ReleaseKey(tc.configured)
		if key != tc.key || source != tc.source {
			t.Errorf("ReleaseKey(%q) = %q, %q; want %q, %q", tc.configured, key, source, tc.key, tc.source)
		}
	}
}

// The install script carries the same key, so the first download is checked
// against the same signature the panel checks every update against.
func TestInstallScriptCarriesTheBuiltinKey(t *testing.T) {
	script, err := os.ReadFile("../../packaging/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `RELEASE_KEY="`+BuiltinKey()+`"`) {
		t.Fatalf("packaging/install.sh does not carry the built-in release key %s", BuiltinKey())
	}
}

// The install script runs apt as the panel does (defaultEnv, osprofile's
// Env): noninteractive, and needrestart listing the services on old
// libraries instead of restarting them all.
func TestInstallScriptKeepsNeedrestartToAList(t *testing.T) {
	script, err := os.ReadFile("../../packaging/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(script), "\n") {
		if strings.Contains(line, "apt-get") && strings.Contains(line, " install ") && !strings.Contains(line, "DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l apt-get ") {
			t.Errorf("packaging/install.sh installs without NEEDRESTART_MODE=l: %s", strings.TrimSpace(line))
		}
	}
	for _, kv := range []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l"} {
		if !strings.Contains(strings.Join(defaultEnv, " "), kv) {
			t.Errorf("update-run's default environment lacks %s", kv)
		}
	}
}
