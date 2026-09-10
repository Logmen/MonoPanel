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
	return nil, &exitError{code: 2, msg: "сертификат не найден: " + ref}
}

func daysLeft(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := int(time.Until(*t).Hours() / 24)
	return fmt.Sprintf("%s (%dд)", t.Local().Format("2006-01-02"), d)
}

func sslCmd() *cobra.Command {
	c := &cobra.Command{Use: "ssl", Short: "TLS-сертификаты (Let's Encrypt / ACME)"}
	var req apitypes.IssueCertificateRequest
	var rsa, noRenew bool
	issue := &cobra.Command{Use: "issue <hostname> [hostname...]", Short: "выпустить сертификат через ACME (HTTP-01 по webroot nginx)", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			fmt.Printf("сертификат #%d %s: %s\n", res.Certificate.ID, res.Certificate.Name, strings.Join(res.Certificate.Names, ", "))
		} else if g.noWait {
			return printJSON(res)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	issue.Flags().StringVar(&req.Email, "email", "", "e-mail аккаунта ACME (запоминается)")
	issue.Flags().BoolVar(&req.Staging, "staging", false, "staging-директория Let's Encrypt (тестовый, недоверенный сертификат)")
	issue.Flags().StringVar(&req.Directory, "directory", "", "свой ACME directory URL")
	issue.Flags().BoolVar(&rsa, "rsa", false, "ключ RSA-2048 вместо ECDSA P-256")
	issue.Flags().BoolVar(&noRenew, "no-auto-renew", false, "не продлевать автоматически")
	issue.Flags().StringVar(&req.DNS, "dns", "", "DNS-провайдер для DNS-01 (нужен для wildcard *.example.com)")

	list := &cobra.Command{Use: "list", Short: "список сертификатов", RunE: func(cmd *cobra.Command, _ []string) error {
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
			renew := "нет"
			if c.AutoRenew {
				renew = "да"
			}
			rows = append(rows, []string{strconv.FormatInt(c.ID, 10), c.Name, strings.Join(c.Names, ","), c.Status, c.Issuer, daysLeft(c.NotAfter), renew, firstLine(c.LastError, "")})
		}
		table([]string{"ID", "NAME", "NAMES", "STATUS", "ISSUER", "EXPIRES", "AUTO", "ERROR"}, rows)
		return nil
	}}
	show := &cobra.Command{Use: "show <id|hostname>", Short: "показать сертификат", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	renew := &cobra.Command{Use: "renew <id|hostname>", Short: "продлить (перевыпустить) сертификат сейчас", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
				return &exitError{code: 2, msg: "укажите id/hostname или --all"}
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
	renew.Flags().BoolVar(&all, "all", false, "все ACME-сертификаты")
	rm := &cobra.Command{Use: "rm <id|hostname>", Short: "удалить сертификат и его файлы", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	c := &cobra.Command{Use: "web", Short: "HTTPS-порт панели"}
	tlsCmd := &cobra.Command{Use: "tls", Short: "какой сертификат отдаёт панель", RunE: func(cmd *cobra.Command, _ []string) error {
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
		src := "самоподписанный"
		if info.Source == "acme" {
			src = "ACME (Let's Encrypt)"
		}
		fmt.Printf("hostname:   %s\nисточник:   %s\nsubject:    %s\nnames:      %s\nissuer:     %s\nдействует:  %s — %s\nsha256:     %s\n",
			info.Hostname, src, info.Certificate.Subject, strings.Join(info.Certificate.Names, ", "), info.Certificate.Issuer,
			info.Certificate.NotBefore.Local().Format("2006-01-02"), info.Certificate.NotAfter.Local().Format("2006-01-02"), info.Certificate.Fingerprint)
		return nil
	}}
	c.AddCommand(tlsCmd)
	return c
}

func configSetCmd() *cobra.Command {
	var restart bool
	c := &cobra.Command{Use: "set <key> <value>", Short: "изменить параметр config.yaml (root)", Long: "Ключи: web.hostname, web.listen, log.level, log.format, jobs.workers", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
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
				return &exitError{code: 2, msg: "jobs.workers должен быть положительным числом"}
			}
			cfg.Jobs.Workers = n
		default:
			return &exitError{code: 2, msg: "неизвестный ключ: " + key}
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
			fmt.Println("monopanel-api перезапущен")
		} else {
			fmt.Println("применится после перезапуска: systemctl restart monopanel-api (или --restart)")
		}
		return nil
	}}
	c.Flags().BoolVar(&restart, "restart", false, "перезапустить monopanel-api после изменения")
	_ = config.DefaultPath
	return c
}

func dnsProviderCmd() *cobra.Command {
	c := &cobra.Command{Use: "dns-provider", Short: "DNS-провайдеры для DNS-01 (wildcard-сертификаты)"}
	var req apitypes.DNSProviderRequest
	var creds []string
	add := &cobra.Command{Use: "add <name>", Short: "добавить провайдера (учётные данные шифруются)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Name = args[0]
		req.Credentials = map[string]string{}
		for _, c := range creds {
			k, v, ok := strings.Cut(c, "=")
			if !ok {
				return &exitError{code: 2, msg: "--cred ожидает KEY=VALUE"}
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
		fmt.Printf("провайдер %s (%s) добавлен; выпуск: mp ssl issue '*.example.com' example.com --dns %s\n", p.Name, p.Type, p.Name)
		return nil
	}}
	add.Flags().StringVar(&req.Type, "type", "cloudflare", "cloudflare, hetzner, digitalocean, gandiv5, desec, namecheap, rfc2136")
	add.Flags().StringSliceVar(&creds, "cred", nil, "KEY=VALUE, например CLOUDFLARE_DNS_API_TOKEN=…")
	list := &cobra.Command{Use: "list", Short: "список", RunE: func(cmd *cobra.Command, _ []string) error {
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
	types := &cobra.Command{Use: "types", Short: "поддерживаемые типы и их ключи", RunE: func(cmd *cobra.Command, _ []string) error {
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
	rm := &cobra.Command{Use: "rm <name>", Short: "удалить", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.DNSProviderDelete(cmd.Context(), args[0])
	}}
	c.AddCommand(add, list, types, rm)
	return c
}
