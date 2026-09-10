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
	c := &cobra.Command{Use: "panel", Short: "сертификат самой панели (HTTPS-порт)", RunE: func(cmd *cobra.Command, _ []string) error {
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
		src := "самоподписанный"
		if p.Source == "acme" {
			src = "выпущенный (Let's Encrypt / ACME)"
		}
		fmt.Printf("hostname:   %s\nотдаётся:   %s\nsubject:    %s\nissuer:     %s\nдействует:  %s — %s\n",
			p.Hostname, src, p.Certificate.Subject, p.Certificate.Issuer,
			p.Certificate.NotBefore.Local().Format("2006-01-02"), p.Certificate.NotAfter.Local().Format("2006-01-02"))
		if p.Record != nil {
			fmt.Printf("запись:     #%d %s, авто-продление: %v\n", p.Record.ID, p.Record.Status, p.Record.AutoRenew)
			if p.Record.LastError != "" {
				fmt.Printf("ошибка:     %s\n", p.Record.LastError)
			}
			if len(p.Record.UsedBySites) > 0 {
				fmt.Printf("сайты:      %s (тот же сертификат)\n", strings.Join(p.Record.UsedBySites, ", "))
			}
		}
		if len(p.DNSProviders) > 0 {
			fmt.Printf("DNS-01:     %s\n", strings.Join(p.DNSProviders, ", "))
		}
		return nil
	}}

	var req apitypes.PanelTLSIssueRequest
	var rsa bool
	issue := &cobra.Command{Use: "issue", Short: "выпустить сертификат для имени панели (HTTP-01 или --dns для DNS-01)", RunE: func(cmd *cobra.Command, _ []string) error {
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
	issue.Flags().StringVar(&req.Email, "email", "", "e-mail аккаунта ACME (запоминается)")
	issue.Flags().BoolVar(&req.Staging, "staging", false, "staging-директория Let's Encrypt (тестовый, недоверенный сертификат)")
	issue.Flags().StringVar(&req.DNS, "dns", "", "DNS-провайдер для DNS-01 (когда порт 80 недоступен снаружи)")
	issue.Flags().BoolVar(&rsa, "rsa", false, "ключ RSA-2048 вместо ECDSA P-256")

	var certFile, keyFile string
	imp := &cobra.Command{Use: "import", Short: "поставить готовый сертификат для имени панели (PEM)", RunE: func(cmd *cobra.Command, _ []string) error {
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
		fmt.Printf("сертификат %s установлен, панель отдаёт его на HTTPS-порту\n", c.Name)
		return nil
	}}
	imp.Flags().StringVar(&certFile, "cert", "", "файл сертификата с цепочкой (PEM)")
	imp.Flags().StringVar(&keyFile, "key", "", "файл закрытого ключа (PEM)")
	imp.MarkFlagRequired("cert") //nolint:errcheck // флаг объявлен строкой выше
	imp.MarkFlagRequired("key")  //nolint:errcheck // флаг объявлен строкой выше

	reset := &cobra.Command{Use: "self-signed", Short: "убрать сертификат панели и вернуться к самоподписанному", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.PanelTLSReset(cmd.Context()); err != nil {
			return err
		}
		fmt.Println("панель отдаёт самоподписанный сертификат")
		return nil
	}}
	c.AddCommand(issue, imp, reset)
	return c
}
