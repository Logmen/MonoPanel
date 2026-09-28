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
