package acme

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	ok := []string{"example.com", "panel.example.com", "xn--80ak6aa92e.com", "a-b.example.co.uk"}
	bad := []string{"", "localhost", "*.example.com", "bad_host.example.com", "-x.example.com", "1.2.3.4", strings.Repeat("a", 64) + ".com"}
	if err := ValidateNameDNS("*.example.com"); err != nil {
		t.Errorf("wildcard must be valid for DNS-01: %v", err)
	}
	if err := ValidateNameDNS("a.*.example.com"); err == nil {
		t.Error("inner wildcard must be rejected")
	}
	if CertDirName("*.example.com") != "_wildcard_.example.com" {
		t.Error("cert dir name for wildcard")
	}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestWriteParseAndProbe(t *testing.T) {
	dir := t.TempDir()
	m := New(filepath.Join(dir, "acme"), filepath.Join(dir, "webroot"), filepath.Join(dir, "certs"), "test", nil)
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if _, err := EnsureSelfSigned(certPath, keyPath, []string{"probe.example.com"}); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(certPath)
	key, _ := os.ReadFile(keyPath)
	cp, kp, _, err := m.WriteFiles("Probe.Example.COM.", cert, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cp, "certs/probe.example.com/fullchain.pem") {
		t.Fatalf("path %s", cp)
	}
	if st, _ := os.Stat(kp); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	info, err := ParseCertificateFile(cp)
	if err != nil || !info.SelfSigned || info.Names[0] != "probe.example.com" || len(info.Fingerprint) != 95 {
		t.Fatalf("info %+v %v", info, err)
	}

	// account key persistence
	a1, created1, err := m.loadOrCreateAccount("https://acme.example/directory", "ops@example.com")
	if err != nil || !created1 || a1.Registration != nil {
		t.Fatalf("account create: %+v %v %v", a1, created1, err)
	}
	a2, created2, _ := m.loadOrCreateAccount("https://acme.example/directory", "ops@example.com")
	if created2 || a2.GetPrivateKey() == nil {
		t.Fatal("account key must be reused")
	}

	// probe against a tiny webroot server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(m.Webroot))}
	go srv.Serve(ln)
	defer srv.Close()
	os.MkdirAll(m.Webroot, 0o755)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if err := m.Probe(context.Background(), "probe.example.com", "127.0.0.1:"+port); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := m.Remove("probe.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Fatal("remove did not delete files")
	}
}
