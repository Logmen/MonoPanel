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
	c := &cobra.Command{Use: "import [hostname]", Short: "импортировать готовый сертификат (PEM); сертификаты Let's Encrypt дальше продлеваются через ACME", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		renew := "без автопродления"
		if res.AutoRenew {
			renew = "автопродление через Let's Encrypt"
		}
		fmt.Printf("сертификат #%d %s: %s, выдан %s, действует до %s, %s\n", res.ID, res.Name, strings.Join(res.Names, ", "), res.Issuer, res.NotAfter.Format("2006-01-02"), renew)
		return nil
	}}
	c.Flags().StringVar(&certFile, "cert", "", "файл сертификата (leaf или fullchain, PEM)")
	c.Flags().StringVar(&keyFile, "key", "", "файл закрытого ключа (PEM)")
	c.Flags().StringVar(&chainFile, "chain", "", "файл промежуточных сертификатов, если их нет в --cert")
	c.Flags().BoolVar(&noRenew, "no-auto-renew", false, "не продлевать через ACME")
	c.MarkFlagRequired("cert")
	c.MarkFlagRequired("key")
	return c
}
