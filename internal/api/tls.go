package api

import (
	"net"
	"os"

	"monopanel/internal/acme"
)

// EnsureSelfSigned creates the panel's fallback certificate (see acme.EnsureSelfSigned).
func EnsureSelfSigned(certPath, keyPath string, names []string) (bool, error) {
	return acme.EnsureSelfSigned(certPath, keyPath, names)
}

// CertFingerprint returns the SHA-256 fingerprint of a PEM certificate.
func CertFingerprint(certPath string) (string, error) {
	info, err := acme.ParseCertificateFile(certPath)
	if err != nil {
		return "", err
	}
	return info.Fingerprint, nil
}

// LocalNames returns the host's names and addresses for the self-signed cert.
func LocalNames(extra ...string) []string {
	names := []string{}
	for _, e := range extra {
		if e != "" {
			names = append(names, e)
		}
	}
	if h, err := os.Hostname(); err == nil {
		names = append(names, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				names = append(names, ipn.IP.String())
			}
		}
	}
	return names
}
