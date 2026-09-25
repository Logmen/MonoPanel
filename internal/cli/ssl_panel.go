package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// sslPanelCmd manages the certificate of the panel's own HTTPS port, apart
// from the certificates of sites.
func sslPanelCmd() *cobra.Command {
	c := &cobra.Command{Use: "panel", Short: T("сертификат самой панели (HTTPS-порт)", "the panel's own certificate (HTTPS port)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		p, err := cl.PanelTLS(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(p)
		}
		src := T("самоподписанный", "self-signed")
		if p.Source == "acme" {
			src = T("выпущенный (Let's Encrypt / ACME)", "issued (Let's Encrypt / ACME)")
		}
		fmt.Printf(T("hostname:   %s\nотдаётся:   %s\nsubject:    %s\nissuer:     %s\nдействует:  %s — %s\n",
			"hostname:   %s\nserving:    %s\nsubject:    %s\nissuer:     %s\nvalid:      %s — %s\n"),
			p.Hostname, src, p.Certificate.Subject, p.Certificate.Issuer,
			p.Certificate.NotBefore.Local().Format("2006-01-02"), p.Certificate.NotAfter.Local().Format("2006-01-02"))
		if p.Record != nil {
			fmt.Printf(T("запись:     #%d %s, авто-продление: %v\n", "record:     #%d %s, auto-renew: %v\n"), p.Record.ID, p.Record.Status, p.Record.AutoRenew)
			if p.Record.LastError != "" {
				fmt.Printf(T("ошибка:     %s\n", "error:      %s\n"), p.Record.LastError)
			}
			if len(p.Record.UsedBySites) > 0 {
				fmt.Printf(T("сайты:      %s (тот же сертификат)\n", "sites:      %s (the same certificate)\n"), strings.Join(p.Record.UsedBySites, ", "))
			}
		}
		if len(p.DNSProviders) > 0 {
			fmt.Printf("DNS-01:     %s\n", strings.Join(p.DNSProviders, ", "))
		}
		return nil
	}}

	var req apitypes.PanelTLSIssueRequest
	var rsa bool
	issue := &cobra.Command{Use: "issue", Short: T("выпустить сертификат для имени панели (HTTP-01 или --dns для DNS-01)", "issue a certificate for the panel's hostname (HTTP-01, or --dns for DNS-01)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if rsa {
			req.KeyType = "rsa2048"
		}
		res, err := cl.PanelTLSIssue(cmd.Context(), req)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	issue.Flags().StringVar(&req.Email, "email", "", T("e-mail аккаунта ACME (запоминается)", "ACME account e-mail (remembered)"))
	issue.Flags().BoolVar(&req.Staging, "staging", false, T("staging-директория Let's Encrypt (тестовый, недоверенный сертификат)", "Let's Encrypt staging directory (untrusted test certificate)"))
	issue.Flags().StringVar(&req.DNS, "dns", "", T("DNS-провайдер для DNS-01 (когда порт 80 недоступен снаружи)", "DNS provider for DNS-01 (when port 80 is not reachable from outside)"))
	issue.Flags().BoolVar(&rsa, "rsa", false, T("ключ RSA-2048 вместо ECDSA P-256", "RSA-2048 key instead of ECDSA P-256"))

	var certFile, keyFile string
	imp := &cobra.Command{Use: "import", Short: T("поставить готовый сертификат для имени панели (PEM)", "install an existing certificate for the panel's hostname (PEM)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cert, err := os.ReadFile(certFile)
		if err != nil {
			return err
		}
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return err
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		c, err := cl.PanelTLSImport(cmd.Context(), apitypes.PanelTLSImportRequest{Certificate: string(cert), PrivateKey: string(key)})
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(c)
		}
		fmt.Printf(T("сертификат %s установлен, панель отдаёт его на HTTPS-порту\n", "certificate %s installed, the panel serves it on its HTTPS port\n"), c.Name)
		return nil
	}}
	imp.Flags().StringVar(&certFile, "cert", "", T("файл сертификата с цепочкой (PEM)", "certificate file with the chain (PEM)"))
	imp.Flags().StringVar(&keyFile, "key", "", T("файл закрытого ключа (PEM)", "private key file (PEM)"))
	imp.MarkFlagRequired("cert") //nolint:errcheck // флаг объявлен строкой выше
	imp.MarkFlagRequired("key")  //nolint:errcheck // флаг объявлен строкой выше

	reset := &cobra.Command{Use: "self-signed", Short: T("убрать сертификат панели и вернуться к самоподписанному", "remove the panel certificate and revert to a self-signed one"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.PanelTLSReset(cmd.Context()); err != nil {
			return err
		}
		fmt.Println(T("панель отдаёт самоподписанный сертификат", "the panel serves a self-signed certificate"))
		return nil
	}}
	c.AddCommand(issue, imp, reset)
	return c
}
