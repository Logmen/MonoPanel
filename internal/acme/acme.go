// Package acme issues certificates through any ACME directory (Let's Encrypt
// by default) with the lego library. HTTP-01 challenges are answered from a
// webroot that nginx serves on every :80 server (snippet acme.conf).
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/go-acme/lego/v4/registration"
)

// Well-known directories.
const (
	LetsEncrypt        = lego.LEDirectoryProduction
	LetsEncryptStaging = lego.LEDirectoryStaging
)

// Manager holds the on-disk layout: <Dir>/accounts/<host>/<email>/ for ACME
// accounts, <Webroot>/.well-known/acme-challenge/ for HTTP-01 tokens and
// <CertsDir>/<name>/ for issued certificates.
type Manager struct {
	Dir       string
	Webroot   string
	CertsDir  string
	UserAgent string
	Log       *slog.Logger
}

var legoLoggerOnce sync.Once

// New creates a manager and routes lego's log output to slog.
func New(dir, webroot, certsDir, userAgent string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	legoLoggerOnce.Do(func() { legolog.Logger = slogAdapter{log: log.With("component", "lego")} })
	return &Manager{Dir: dir, Webroot: webroot, CertsDir: certsDir, UserAgent: userAgent, Log: log}
}

type slogAdapter struct{ log *slog.Logger }

func (a slogAdapter) Fatal(args ...any)                 { a.log.Error(fmt.Sprint(args...)) }
func (a slogAdapter) Fatalln(args ...any)               { a.log.Error(fmt.Sprint(args...)) }
func (a slogAdapter) Fatalf(format string, args ...any) { a.log.Error(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Print(args ...any)                 { a.log.Debug(fmt.Sprint(args...)) }
func (a slogAdapter) Println(args ...any)               { a.log.Debug(fmt.Sprint(args...)) }
func (a slogAdapter) Printf(format string, args ...any) { a.log.Debug(fmt.Sprintf(format, args...)) }

var (
	labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidateName checks a hostname for HTTP-01 issuance (no wildcards, no IPs).
func ValidateName(name string) error {
	if strings.Contains(name, "*") {
		return errors.New("wildcard names need DNS-01: add a DNS provider and pass --dns")
	}
	return ValidateNameDNS(name)
}

// ValidateNameDNS validates a hostname that may start with "*." (DNS-01).
func ValidateNameDNS(name string) error {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if n == "" || len(n) > 253 {
		return errors.New("empty or too long hostname")
	}
	n = strings.TrimPrefix(n, "*.")
	if strings.Contains(n, "*") {
		return errors.New("wildcard is only allowed as the leftmost label")
	}
	labels := strings.Split(n, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%q is not a fully qualified hostname", name)
	}
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("invalid hostname label %q in %q", l, name)
		}
	}
	if regexp.MustCompile(`^[0-9.]+$`).MatchString(n) {
		return errors.New("IP addresses cannot be issued via HTTP-01")
	}
	return nil
}

// NormalizeName lowercases and trims a hostname.
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

type account struct {
	Email        string
	Registration *registration.Resource
	key          crypto.PrivateKey
}

func (a *account) GetEmail() string                        { return a.Email }
func (a *account) GetRegistration() *registration.Resource { return a.Registration }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

func sanitize(s string) string {
	s = strings.ToLower(s)
	return regexp.MustCompile(`[^a-z0-9._@-]+`).ReplaceAllString(s, "_")
}

func (m *Manager) accountDir(directory, email string) string {
	host := directory
	if u, err := url.Parse(directory); err == nil && u.Host != "" {
		host = u.Host
	}
	if email == "" {
		email = "default"
	}
	return filepath.Join(m.Dir, "accounts", sanitize(host), sanitize(email))
}

func (m *Manager) loadOrCreateAccount(directory, email string) (*account, bool, error) {
	dir := m.accountDir(directory, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	acct := &account{Email: email}
	keyPath := filepath.Join(dir, "account.key")
	created := false
	if b, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, false, errors.New("account key is not PEM: " + keyPath)
		}
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("parse account key: %w", err)
		}
		acct.key = k
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, false, err
		}
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, false, err
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, false, err
		}
		acct.key = k
		created = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, "registration.json")); err == nil {
		var reg registration.Resource
		if json.Unmarshal(b, &reg) == nil && reg.URI != "" {
			acct.Registration = &reg
		}
	}
	return acct, created, nil
}

func (m *Manager) saveRegistration(directory, email string, reg *registration.Resource) error {
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.accountDir(directory, email), "registration.json"), b, 0o600)
}

// IssueRequest describes an order.
type IssueRequest struct {
	Names     []string
	Email     string
	Directory string
	KeyType   string // ec256 (default) | rsa2048
	Logf      func(format string, args ...any)
	// DNSProviderType with DNSCredentials switches to DNS-01 (wildcards).
	DNSProviderType string
	DNSCredentials  map[string]string
}

// Result describes an issued certificate.
type Result struct {
	Name      string
	Names     []string
	CertPath  string
	KeyPath   string
	ChainPath string
	CertInfo  *CertInfo
	CertURL   string
}

// Issue orders a certificate and writes it under CertsDir/<primary name>/.
func (m *Manager) Issue(ctx context.Context, req IssueRequest) (*Result, error) {
	logf := req.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(req.Names) == 0 {
		return nil, errors.New("no names")
	}
	names := make([]string, 0, len(req.Names))
	for _, n := range req.Names {
		n = NormalizeName(n)
		var err error
		if req.DNSProviderType != "" {
			err = ValidateNameDNS(n)
		} else {
			err = ValidateName(n)
		}
		if err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	directory := req.Directory
	if directory == "" {
		directory = LetsEncrypt
	}
	keyType := certcrypto.EC256
	switch strings.ToLower(req.KeyType) {
	case "", "ec256":
	case "rsa2048":
		keyType = certcrypto.RSA2048
	default:
		return nil, fmt.Errorf("unsupported key type %q", req.KeyType)
	}
	if err := m.ensureWebroot(); err != nil {
		return nil, err
	}

	acct, created, err := m.loadOrCreateAccount(directory, req.Email)
	if err != nil {
		return nil, err
	}
	cfg := lego.NewConfig(acct)
	cfg.CADirURL = directory
	cfg.Certificate.KeyType = keyType
	cfg.UserAgent = m.UserAgent
	cfg.HTTPClient.Timeout = 90 * time.Second
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("acme client: %w", err)
	}
	if req.DNSProviderType != "" {
		p, release, err := dnsProvider(req.DNSProviderType, req.DNSCredentials)
		if err != nil {
			return nil, err
		}
		defer release()
		if err := client.Challenge.SetDNS01Provider(p, dns01.AddRecursiveNameservers([]string{"1.1.1.1:53", "8.8.8.8:53"})); err != nil {
			return nil, err
		}
		logf("challenge: DNS-01 via %s", req.DNSProviderType)
	} else {
		provider, err := webroot.NewHTTPProvider(m.Webroot)
		if err != nil {
			return nil, err
		}
		if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
			return nil, err
		}
	}
	if acct.Registration == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("acme registration: %w", err)
		}
		acct.Registration = reg
		if err := m.saveRegistration(directory, req.Email, reg); err != nil {
			return nil, err
		}
		logf("ACME account registered at %s (new key: %v)", directory, created)
	} else {
		logf("ACME account: %s", acct.Registration.URI)
	}

	logf("ordering certificate for %s (%s)", strings.Join(names, ", "), keyType)
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: names, Bundle: true})
	if err != nil {
		return nil, fmt.Errorf("acme order: %w", err)
	}
	certPath, keyPath, chainPath, err := m.WriteFiles(names[0], res.Certificate, res.PrivateKey, res.IssuerCertificate)
	if err != nil {
		return nil, err
	}
	info, err := ParseCertificatePEM(res.Certificate)
	if err != nil {
		return nil, err
	}
	logf("issued by %s, valid until %s", info.Issuer, info.NotAfter.Format(time.RFC3339))
	return &Result{Name: names[0], Names: names, CertPath: certPath, KeyPath: keyPath, ChainPath: chainPath, CertInfo: info, CertURL: res.CertURL}, nil
}

// WriteFiles stores fullchain.pem (0644), privkey.pem (0600) and chain.pem
// atomically under CertsDir/<name>/.
func (m *Manager) WriteFiles(name string, fullchain, key, chain []byte) (certPath, keyPath, chainPath string, err error) {
	dir := filepath.Join(m.CertsDir, CertDirName(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	certPath, keyPath, chainPath = filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem"), filepath.Join(dir, "chain.pem")
	if err := atomicWrite(keyPath, key, 0o600); err != nil {
		return "", "", "", err
	}
	if err := atomicWrite(certPath, fullchain, 0o644); err != nil {
		return "", "", "", err
	}
	if len(chain) > 0 {
		if err := atomicWrite(chainPath, chain, 0o644); err != nil {
			return "", "", "", err
		}
	}
	return certPath, keyPath, chainPath, nil
}

// CertDirName maps a (possibly wildcard) name to a directory name.
func CertDirName(name string) string {
	return strings.ReplaceAll(NormalizeName(name), "*", "_wildcard_")
}

// Remove deletes the certificate directory of a name.
func (m *Manager) Remove(name string) error {
	dir := filepath.Join(m.CertsDir, CertDirName(name))
	if !strings.HasPrefix(dir, filepath.Clean(m.CertsDir)+"/") {
		return errors.New("refusing to remove outside certs dir")
	}
	return os.RemoveAll(dir)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CertInfo summarises a leaf certificate.
type CertInfo struct {
	Subject     string    `json:"subject"`
	Names       []string  `json:"names"`
	Issuer      string    `json:"issuer"`
	IssuerOrg   string    `json:"issuer_org,omitempty"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	SelfSigned  bool      `json:"self_signed"`
	Fingerprint string    `json:"fingerprint_sha256"`
}

// ParseCertificateFile reads the first certificate of a PEM file.
func ParseCertificateFile(path string) (*CertInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCertificatePEM(b)
}

// ParseCertificatePEM parses the first certificate of a PEM bundle.
func ParseCertificatePEM(b []byte) (*CertInfo, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(leaf.Raw)
	hexs := hex.EncodeToString(sum[:])
	parts := make([]string, 0, 32)
	for i := 0; i < len(hexs); i += 2 {
		parts = append(parts, strings.ToUpper(hexs[i:i+2]))
	}
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return &CertInfo{
		Subject:     leaf.Subject.CommonName,
		Names:       names,
		Issuer:      leaf.Issuer.CommonName,
		IssuerOrg:   strings.Join(leaf.Issuer.Organization, " "),
		Serial:      leaf.SerialNumber.Text(16),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		SelfSigned:  leaf.Issuer.String() == leaf.Subject.String(),
		Fingerprint: strings.Join(parts, ":"),
	}, nil
}

// ensureWebroot creates the challenge directory world-readable regardless of
// the process umask: nginx workers run as another user.
func (m *Manager) ensureWebroot() error {
	for _, d := range []string{m.Webroot, filepath.Join(m.Webroot, ".well-known"), filepath.Join(m.Webroot, ".well-known", "acme-challenge")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("webroot: %w", err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return fmt.Errorf("webroot: %w", err)
		}
	}
	return nil
}

// Probe verifies that the local web server answers HTTP-01 requests for name
// from the webroot: it writes a token and fetches it via localIP with the Host
// header set to name.
func (m *Manager) Probe(ctx context.Context, name, localIP string) error {
	dir := filepath.Join(m.Webroot, ".well-known", "acme-challenge")
	if err := m.ensureWebroot(); err != nil {
		return err
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := "monopanel-probe-" + hex.EncodeToString(tokenBytes)
	path := filepath.Join(dir, token)
	if err := os.WriteFile(path, []byte(token), 0o644); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(path)
	_ = os.Chmod(path, 0o644) // the web server's user must read challenge files whatever the umask
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+localIP+"/.well-known/acme-challenge/"+token, nil)
	if err != nil {
		return err
	}
	req.Host = name
	res, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return fmt.Errorf("port 80 on %s is not answering: %w (install nginx: mp stack install nginx)", localIP, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	if res.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != token {
		return fmt.Errorf("HTTP-01 path is not served from the panel webroot for %s (HTTP %d): another web server owns port 80 or nginx config is stale", name, res.StatusCode)
	}
	return nil
}
