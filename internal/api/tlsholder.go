package api

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/config"
)

// certHolder serves the panel's own TLS certificate and hot-swaps it: an
// issued certificate for web.hostname wins over the self-signed fallback.
type certHolder struct {
	cfg  config.Config
	log  *slog.Logger
	cur  atomic.Pointer[tls.Certificate]
	info atomic.Pointer[apitypes.TLSInfo]
}

func newCertHolder(cfg config.Config, log *slog.Logger) *certHolder {
	return &certHolder{cfg: cfg, log: log}
}

// Reload picks the best certificate available right now.
func (h *certHolder) Reload() error {
	if name := acme.NormalizeName(h.cfg.Web.Hostname); name != "" {
		dir := filepath.Join(h.cfg.DataDir, "certs", name)
		certPath := filepath.Join(dir, "fullchain.pem")
		if cert, err := tls.LoadX509KeyPair(certPath, filepath.Join(dir, "privkey.pem")); err == nil {
			if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil && time.Now().Before(leaf.NotAfter) {
				cert.Leaf = leaf
				h.set(&cert, "acme", certPath)
				return nil
			}
		}
	}
	certPath, keyPath := h.cfg.TLSCertPath(), h.cfg.TLSKeyPath()
	if created, err := acme.EnsureSelfSigned(certPath, keyPath, LocalNames(h.cfg.Web.Hostname)); err != nil {
		return err
	} else if created {
		h.log.Info("generated self-signed certificate", "cert", certPath)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	h.set(&cert, "selfsigned", certPath)
	return nil
}

func (h *certHolder) set(cert *tls.Certificate, source, path string) {
	prev := h.info.Load()
	info := apitypes.TLSInfo{Hostname: h.cfg.Web.Hostname, Source: source}
	if i, err := acme.ParseCertificateFile(path); err == nil {
		info.Certificate = *i
	}
	h.cur.Store(cert)
	h.info.Store(&info)
	if prev == nil || prev.Source != source || prev.Certificate.Fingerprint != info.Certificate.Fingerprint {
		h.log.Info("panel certificate", "source", source, "subject", info.Certificate.Subject, "issuer", info.Certificate.Issuer, "expires", info.Certificate.NotAfter.Format(time.RFC3339))
	}
}

// GetCertificate is the tls.Config callback.
func (h *certHolder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := h.cur.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("no panel certificate loaded")
}

// Info describes the certificate currently served.
func (h *certHolder) Info() apitypes.TLSInfo {
	if i := h.info.Load(); i != nil {
		return *i
	}
	return apitypes.TLSInfo{Hostname: h.cfg.Web.Hostname}
}
