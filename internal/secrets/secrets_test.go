package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.key")
	os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), 0o600)
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := b.Encrypt("hunter2")
	if enc == "hunter2" || enc == "" {
		t.Fatal("not encrypted")
	}
	dec, err := b.Decrypt(enc)
	if err != nil || dec != "hunter2" {
		t.Fatalf("decrypt: %q %v", dec, err)
	}
	if _, err := b.Decrypt("v1:garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
	if e, _ := b.Encrypt(""); e != "" {
		t.Fatal("empty must stay empty")
	}
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := Open(p); err == nil {
		t.Fatal("short key accepted")
	}
}
