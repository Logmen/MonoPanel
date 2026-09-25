package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func sslImportCmd() *cobra.Command {
	var certFile, keyFile, chainFile string
	var noRenew bool
	c := &cobra.Command{Use: "import [hostname]", Short: T("импортировать готовый сертификат (PEM); сертификаты Let's Encrypt дальше продлеваются через ACME", "import an existing certificate (PEM); Let's Encrypt certificates are then renewed via ACME"), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		cert, err := os.ReadFile(certFile)
		if err != nil {
			return err
		}
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return err
		}
		if chainFile != "" {
			chain, err := os.ReadFile(chainFile)
			if err != nil {
				return err
			}
			cert = append(append(cert, '\n'), chain...)
		}
		req := apitypes.ImportCertificateRequest{Certificate: string(cert), PrivateKey: string(key)}
		if len(args) == 1 {
			req.Name = args[0]
		}
		if noRenew {
			f := false
			req.AutoRenew = &f
		}
		res, err := cl.ImportCertificate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		renew := T("без автопродления", "no auto-renewal")
		if res.AutoRenew {
			renew = T("автопродление через Let's Encrypt", "auto-renewal via Let's Encrypt")
		}
		fmt.Printf(T("сертификат #%d %s: %s, выдан %s, действует до %s, %s\n", "certificate #%d %s: %s, issued by %s, valid until %s, %s\n"), res.ID, res.Name, strings.Join(res.Names, ", "), res.Issuer, res.NotAfter.Format("2006-01-02"), renew)
		return nil
	}}
	c.Flags().StringVar(&certFile, "cert", "", T("файл сертификата (leaf или fullchain, PEM)", "certificate file (leaf or fullchain, PEM)"))
	c.Flags().StringVar(&keyFile, "key", "", T("файл закрытого ключа (PEM)", "private key file (PEM)"))
	c.Flags().StringVar(&chainFile, "chain", "", T("файл промежуточных сертификатов, если их нет в --cert", "file with the intermediate certificates, if --cert lacks them"))
	c.Flags().BoolVar(&noRenew, "no-auto-renew", false, T("не продлевать через ACME", "do not renew via ACME"))
	c.MarkFlagRequired("cert")
	c.MarkFlagRequired("key")
	return c
}
