package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

type certImportInput struct {
	Body apitypes.ImportCertificateRequest
}

type certImportOutput struct {
	Status int
	Body   *store.Certificate
}

// splitChain returns the leaf (first CERTIFICATE block) and the rest of a PEM bundle.
func splitChain(bundle []byte) (leaf, chain []byte) {
	rest := bundle
	first := true
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		enc := pem.EncodeToMemory(block)
		if first {
			leaf = enc
			first = false
		} else {
			chain = append(chain, enc...)
		}
	}
	return leaf, chain
}

func (s *Server) registerCertImport() {
	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-import", Method: http.MethodPost, Path: "/certificates/import", Summary: "Install an existing certificate (PEM); Let's Encrypt certificates keep renewing via ACME", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *certImportInput) (*certImportOutput, error) {
		p := principalFrom(ctx)
		certPEM := []byte(strings.TrimSpace(in.Body.Certificate) + "\n")
		keyPEM := []byte(strings.TrimSpace(in.Body.PrivateKey) + "\n")
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("certificate/key: " + err.Error())
		}
		info, err := acme.ParseCertificatePEM(certPEM)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		names := info.Names
		if len(names) == 0 && info.Subject != "" {
			names = []string{acme.NormalizeName(info.Subject)}
		}
		if len(names) == 0 {
			return nil, huma.Error422UnprocessableEntity("certificate has no DNS names")
		}
		name := acme.NormalizeName(in.Body.Name)
		if name == "" {
			name = names[0]
			if info.Subject != "" && containsName(names, acme.NormalizeName(info.Subject)) {
				name = acme.NormalizeName(info.Subject)
			}
		}
		if !containsName(names, name) {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("certificate does not cover %s (names: %s)", name, strings.Join(names, ", ")))
		}
		if time.Now().After(info.NotAfter) {
			return nil, huma.Error422UnprocessableEntity("certificate expired on " + info.NotAfter.Format("2006-01-02"))
		}
		isLE := strings.Contains(strings.ToLower(info.IssuerOrg), "let's encrypt")
		auto := isLE
		if in.Body.AutoRenew != nil {
			auto = *in.Body.AutoRenew
		}
		if auto && !isLE {
			return nil, huma.Error422UnprocessableEntity("auto_renew works only for Let's Encrypt certificates (issuer: " + info.IssuerOrg + " " + info.Issuer + ")")
		}
		leaf, chain := splitChain(certPEM)
		fullchain := append(append([]byte{}, leaf...), chain...)
		certPath, keyPath, chainPath, err := s.acme.WriteFiles(name, fullchain, keyPEM, chain)
		if err != nil {
			return nil, err
		}
		c, err := s.db.GetCertificateByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			c = &store.Certificate{Name: name}
		} else if err != nil {
			return nil, err
		}
		keyType := "rsa2048"
		if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); ok {
			keyType = "ec256"
		}
		c.Names, c.KeyType, c.AutoRenew, c.DNSProvider = names, keyType, auto, ""
		if auto {
			c.Kind, c.DirectoryURL = store.CertKindACME, acme.LetsEncrypt
			c.Email, _ = s.db.GetSetting(ctx, settingACMEEmail)
		} else {
			c.Kind, c.DirectoryURL, c.Email = store.CertKindCustom, "", ""
		}
		now := time.Now()
		nb, na := info.NotBefore, info.NotAfter
		c.CertPath, c.KeyPath, c.ChainPath = certPath, keyPath, chainPath
		c.Issuer, c.Serial = info.Issuer, info.Serial
		c.NotBefore, c.NotAfter, c.LastAttempt = &nb, &na, &now
		c.Status, c.LastError = store.CertValid, ""
		if p.UserID != 0 {
			uid := p.UserID
			c.UserID = &uid
		}
		if err := s.db.UpsertCertificate(ctx, c); err != nil {
			return nil, err
		}
		s.reapplySitesForCert(ctx, func(format string, args ...any) { s.log.Info(fmt.Sprintf(format, args...), "cert", name) }, c)
		if containsName(c.Names, s.cfg.Web.Hostname) {
			if err := s.tls.Reload(); err != nil {
				s.log.Warn("panel certificate reload", "err", err)
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cert.import", Target: name, IP: requestInfo(ctx).IP, Details: map[string]any{"names": names, "issuer": info.Issuer, "auto_renew": auto}})
		return &certImportOutput{Status: http.StatusCreated, Body: c}, nil
	})
}
