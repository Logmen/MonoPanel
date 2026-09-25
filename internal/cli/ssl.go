package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

func resolveCert(ctx context.Context, cl *client.Client, ref string) (*store.Certificate, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return cl.GetCertificate(ctx, id)
	}
	list, err := cl.Certificates(ctx)
	if err != nil {
		return nil, err
	}
	ref = strings.ToLower(strings.TrimSuffix(ref, "."))
	for _, c := range list {
		if c.Name == ref {
			return c, nil
		}
		for _, n := range c.Names {
			if n == ref {
				return c, nil
			}
		}
	}
	return nil, &exitError{code: 2, msg: T("сертификат не найден: ", "certificate not found: ") + ref}
}

func daysLeft(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := int(time.Until(*t).Hours() / 24)
	return fmt.Sprintf(T("%s (%dд)", "%s (%dd)"), t.Local().Format("2006-01-02"), d)
}

func sslCmd() *cobra.Command {
	c := &cobra.Command{Use: "ssl", Short: T("TLS-сертификаты (Let's Encrypt / ACME)", "TLS certificates (Let's Encrypt / ACME)")}
	var req apitypes.IssueCertificateRequest
	var rsa, noRenew bool
	issue := &cobra.Command{Use: "issue <hostname> [hostname...]", Short: T("выпустить сертификат через ACME (HTTP-01 по webroot nginx)", "issue a certificate via ACME (HTTP-01 through the nginx webroot)"), Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Names = args
		if rsa {
			req.KeyType = "rsa2048"
		}
		if noRenew {
			f := false
			req.AutoRenew = &f
		}
		res, err := cl.IssueCertificate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if !g.json {
			fmt.Printf(T("сертификат #%d %s: %s\n", "certificate #%d %s: %s\n"), res.Certificate.ID, res.Certificate.Name, strings.Join(res.Certificate.Names, ", "))
		} else if g.noWait {
			return printJSON(res)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	issue.Flags().StringVar(&req.Email, "email", "", T("e-mail аккаунта ACME (запоминается)", "ACME account e-mail (remembered)"))
	issue.Flags().BoolVar(&req.Staging, "staging", false, T("staging-директория Let's Encrypt (тестовый, недоверенный сертификат)", "Let's Encrypt staging directory (untrusted test certificate)"))
	issue.Flags().StringVar(&req.Directory, "directory", "", T("свой ACME directory URL", "custom ACME directory URL"))
	issue.Flags().BoolVar(&rsa, "rsa", false, T("ключ RSA-2048 вместо ECDSA P-256", "RSA-2048 key instead of ECDSA P-256"))
	issue.Flags().BoolVar(&noRenew, "no-auto-renew", false, T("не продлевать автоматически", "do not renew automatically"))
	issue.Flags().StringVar(&req.DNS, "dns", "", T("DNS-провайдер для DNS-01 (нужен для wildcard *.example.com)", "DNS provider for DNS-01 (needed for a wildcard *.example.com)"))

	list := &cobra.Command{Use: "list", Short: T("список сертификатов", "list certificates"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Certificates(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, c := range list {
			renew := T("нет", "no")
			if c.AutoRenew {
				renew = T("да", "yes")
			}
			rows = append(rows, []string{strconv.FormatInt(c.ID, 10), c.Name, strings.Join(c.Names, ","), c.Status, c.Issuer, daysLeft(c.NotAfter), renew, firstLine(c.LastError, "")})
		}
		table([]string{"ID", "NAME", "NAMES", "STATUS", "ISSUER", "EXPIRES", "AUTO", "ERROR"}, rows)
		return nil
	}}
	show := &cobra.Command{Use: "show <id|hostname>", Short: T("показать сертификат", "show a certificate"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		c, err := resolveCert(cmd.Context(), cl, args[0])
		if err != nil {
			return err
		}
		return printJSON(c)
	}}
	var all bool
	renew := &cobra.Command{Use: "renew <id|hostname>", Short: T("продлить (перевыпустить) сертификат сейчас", "renew (reissue) a certificate now"), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var targets []*store.Certificate
		if all {
			targets, err = cl.Certificates(cmd.Context())
			if err != nil {
				return err
			}
		} else {
			if len(args) != 1 {
				return &exitError{code: 2, msg: T("укажите id/hostname или --all", "specify an id/hostname or --all")}
			}
			c, err := resolveCert(cmd.Context(), cl, args[0])
			if err != nil {
				return err
			}
			targets = []*store.Certificate{c}
		}
		for _, c := range targets {
			if c.Kind != store.CertKindACME {
				continue
			}
			res, err := cl.RenewCertificate(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			if err := followJob(cmd, cl, res.JobID); err != nil {
				return err
			}
		}
		return nil
	}}
	renew.Flags().BoolVar(&all, "all", false, T("все ACME-сертификаты", "all ACME certificates"))
	rm := &cobra.Command{Use: "rm <id|hostname>", Short: T("удалить сертификат и его файлы", "delete a certificate and its files"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		c, err := resolveCert(cmd.Context(), cl, args[0])
		if err != nil {
			return err
		}
		return cl.DeleteCertificate(cmd.Context(), c.ID)
	}}
	c.AddCommand(issue, sslImportCmd(), list, show, renew, rm, sslPanelCmd())
	return c
}

func webCmd() *cobra.Command {
	c := &cobra.Command{Use: "web", Short: T("HTTPS-порт панели", "the panel's HTTPS port")}
	tlsCmd := &cobra.Command{Use: "tls", Short: T("какой сертификат отдаёт панель", "which certificate the panel serves"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		info, err := cl.WebTLS(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(info)
		}
		src := T("самоподписанный", "self-signed")
		if info.Source == "acme" {
			src = "ACME (Let's Encrypt)"
		}
		fmt.Printf(T("hostname:   %s\nисточник:   %s\nsubject:    %s\nnames:      %s\nissuer:     %s\nдействует:  %s — %s\nsha256:     %s\n",
			"hostname:   %s\nsource:     %s\nsubject:    %s\nnames:      %s\nissuer:     %s\nvalid:      %s — %s\nsha256:     %s\n"),
			info.Hostname, src, info.Certificate.Subject, strings.Join(info.Certificate.Names, ", "), info.Certificate.Issuer,
			info.Certificate.NotBefore.Local().Format("2006-01-02"), info.Certificate.NotAfter.Local().Format("2006-01-02"), info.Certificate.Fingerprint)
		return nil
	}}
	c.AddCommand(tlsCmd)
	return c
}

func configSetCmd() *cobra.Command {
	var restart bool
	c := &cobra.Command{Use: "set <key> <value>", Short: T("изменить параметр config.yaml (root)", "change a config.yaml setting (root)"), Long: T("Ключи: web.hostname, web.listen, log.level, log.format, jobs.workers", "Keys: web.hostname, web.listen, log.level, log.format, jobs.workers"), Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		key, val := args[0], args[1]
		switch key {
		case "web.hostname":
			cfg.Web.Hostname = strings.ToLower(strings.TrimSpace(val))
		case "web.listen":
			cfg.Web.Listen = val
		case "log.level":
			cfg.Log.Level = val
		case "log.format":
			cfg.Log.Format = val
		case "jobs.workers":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return &exitError{code: 2, msg: T("jobs.workers должен быть положительным числом", "jobs.workers must be a positive number")}
			}
			cfg.Jobs.Workers = n
		default:
			return &exitError{code: 2, msg: T("неизвестный ключ: ", "unknown key: ") + key}
		}
		if err := cfg.Save(cfg.Path()); err != nil {
			return err
		}
		fmt.Printf("%s = %s → %s\n", key, val, cfg.Path())
		if restart {
			sd, err := systemd.Connect(cmd.Context())
			if err != nil {
				return err
			}
			defer sd.Close()
			if err := sd.Restart(cmd.Context(), "monopanel-api.service"); err != nil {
				return err
			}
			fmt.Println(T("monopanel-api перезапущен", "monopanel-api restarted"))
		} else {
			fmt.Println(T("применится после перезапуска: systemctl restart monopanel-api (или --restart)", "takes effect after a restart: systemctl restart monopanel-api (or --restart)"))
		}
		return nil
	}}
	c.Flags().BoolVar(&restart, "restart", false, T("перезапустить monopanel-api после изменения", "restart monopanel-api after the change"))
	_ = config.DefaultPath
	return c
}

func dnsProviderCmd() *cobra.Command {
	c := &cobra.Command{Use: "dns-provider", Short: T("DNS-провайдеры для DNS-01 (wildcard-сертификаты)", "DNS providers for DNS-01 (wildcard certificates)")}
	var req apitypes.DNSProviderRequest
	var creds []string
	add := &cobra.Command{Use: "add <name>", Short: T("добавить провайдера (учётные данные шифруются)", "add a provider (credentials are stored encrypted)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Name = args[0]
		req.Credentials = map[string]string{}
		for _, c := range creds {
			k, v, ok := strings.Cut(c, "=")
			if !ok {
				return &exitError{code: 2, msg: T("--cred ожидает KEY=VALUE", "--cred expects KEY=VALUE")}
			}
			req.Credentials[k] = v
		}
		p, err := cl.DNSProviderCreate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(p)
		}
		fmt.Printf(T("провайдер %s (%s) добавлен; выпуск: mp ssl issue '*.example.com' example.com --dns %s\n",
			"provider %s (%s) added; to issue a certificate: mp ssl issue '*.example.com' example.com --dns %s\n"), p.Name, p.Type, p.Name)
		return nil
	}}
	add.Flags().StringVar(&req.Type, "type", "cloudflare", "cloudflare, hetzner, digitalocean, gandiv5, desec, namecheap, rfc2136")
	add.Flags().StringSliceVar(&creds, "cred", nil, T("KEY=VALUE, например CLOUDFLARE_DNS_API_TOKEN=…", "KEY=VALUE, e.g. CLOUDFLARE_DNS_API_TOKEN=…"))
	list := &cobra.Command{Use: "list", Short: T("список", "list the providers"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ps, err := cl.DNSProviders(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(ps)
		}
		rows := make([][]string, 0, len(ps))
		for _, p := range ps {
			rows = append(rows, []string{p.Name, p.Type, p.CreatedAt.Std().Local().Format("2006-01-02")})
		}
		table([]string{"NAME", "TYPE", "CREATED"}, rows)
		return nil
	}}
	types := &cobra.Command{Use: "types", Short: T("поддерживаемые типы и их ключи", "supported types and their keys"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		t, err := cl.DNSProviderTypes(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(t)
		}
		for k, v := range t {
			fmt.Printf("%-14s %s\n", k, strings.Join(v, ", "))
		}
		return nil
	}}
	rm := &cobra.Command{Use: "rm <name>", Short: T("удалить", "delete a provider"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.DNSProviderDelete(cmd.Context(), args[0])
	}}
	c.AddCommand(add, list, types, rm)
	return c
}
