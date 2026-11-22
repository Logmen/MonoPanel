package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
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

// fakeCA answers the part of RFC 8555 an account setup needs — directory,
// nonces, new accounts — and remembers which key registered each account.
type fakeCA struct {
	srv      *httptest.Server
	certFile string

	mu       sync.Mutex
	accounts []string // the "x" of the JWK of every newAccount request
}

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	ca := &fakeCA{}
	nonce := func(w http.ResponseWriter) {
		b := make([]byte, 12)
		rand.Read(b) //nolint:errcheck // test nonce
		w.Header().Set("Replay-Nonce", base64.RawURLEncoding.EncodeToString(b))
	}
	mux := http.NewServeMux()
	ca.srv = httptest.NewTLSServer(mux)
	t.Cleanup(ca.srv.Close)
	base := ca.srv.URL
	mux.HandleFunc("/dir", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"newNonce": base + "/nonce", "newAccount": base + "/acct", "newOrder": base + "/order", "revokeCert": base + "/revoke", "keyChange": base + "/key"}) //nolint:errcheck // test double
	})
	mux.HandleFunc("/nonce", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/acct", func(w http.ResponseWriter, r *http.Request) {
		var jws struct {
			Protected string `json:"protected"`
		}
		json.NewDecoder(r.Body).Decode(&jws) //nolint:errcheck // test double
		raw, _ := base64.RawURLEncoding.DecodeString(jws.Protected)
		var hdr struct {
			JWK struct {
				X string `json:"x"`
			} `json:"jwk"`
		}
		json.Unmarshal(raw, &hdr) //nolint:errcheck // test double
		// A real CA takes its time; so does this one, which is what lets
		// unserialised setups overlap.
		time.Sleep(20 * time.Millisecond)
		ca.mu.Lock()
		ca.accounts = append(ca.accounts, hdr.JWK.X)
		n := len(ca.accounts)
		ca.mu.Unlock()
		nonce(w)
		w.Header().Set("Location", fmt.Sprintf("%s/acct/%d", base, n))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"valid"}`)) //nolint:errcheck // test double
	})
	ca.certFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ca
}

func (ca *fakeCA) registered() []string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return append([]string{}, ca.accounts...)
}

// Parallel orders for one account (renew all, the renewal loop) set it up
// one at a time: one key on disk, one registration with the CA, made with
// that key, and every order acting for it.
func TestParallelOrdersShareOneAccount(t *testing.T) {
	ca := newFakeCA(t)
	t.Setenv("LEGO_CA_CERTIFICATES", ca.certFile) // lego's HTTP client trusts the fake CA
	dir := t.TempDir()
	m := New(filepath.Join(dir, "acme"), filepath.Join(dir, "webroot"), filepath.Join(dir, "certs"), "test", nil)
	directory := ca.srv.URL + "/dir"
	logf := func(string, ...any) {}

	const orders = 8
	var wg sync.WaitGroup
	errs := make(chan error, orders)
	for range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.accountClient(directory, "ops@example.com", certcrypto.EC256, logf)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("account setup: %v", err)
		}
	}
	regs := ca.registered()
	if len(regs) != 1 {
		t.Fatalf("%d accounts registered for one (directory, e-mail)", len(regs))
	}

	// The registration on disk is the one the CA made, with the key on disk.
	acct, created, err := m.loadOrCreateAccount(directory, "ops@example.com")
	if err != nil || created || acct.Registration == nil || acct.Registration.URI != ca.srv.URL+"/acct/1" {
		t.Fatalf("account on disk: %+v created=%v %v", acct, created, err)
	}
	pub, err := acct.key.(*ecdsa.PrivateKey).PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	point := pub.Bytes() // 0x04 || X || Y
	if x := base64.RawURLEncoding.EncodeToString(point[1:33]); x != regs[0] {
		t.Fatal("the key on disk is not the key that registered the account")
	}

	// Later orders reuse it; another e-mail is another account.
	if _, err := m.accountClient(directory, "ops@example.com", certcrypto.EC256, logf); err != nil {
		t.Fatal(err)
	}
	if _, err := m.accountClient(directory, "dev@example.com", certcrypto.EC256, logf); err != nil {
		t.Fatal(err)
	}
	if n := len(ca.registered()); n != 2 {
		t.Fatalf("%d registrations after a reuse and a second e-mail, want 2", n)
	}
}
