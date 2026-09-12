// Package cli implements the `mp` command: daemon entry points (api, agent,
// helper) and the operator commands, all thin clients of the REST API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/tui"
)

type globals struct {
	config   string
	server   string
	token    string
	json     bool
	insecure bool
	noWait   bool
}

var g globals

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "error:", ee.msg)
			}
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		var ae *client.APIError
		if errors.As(err, &ae) {
			switch {
			case ae.Status == 401 || ae.Status == 403:
				return 4
			case ae.Status == 400 || ae.Status == 422:
				return 2
			}
		}
		return 1
	}
	return 0
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "mp",
		Short: "MonoPanel — панель управления веб-сервером",
		Long: `MonoPanel — панель управления веб-сервером (nginx + php-fpm, nginx + Apache, PHP 5.6–8.5, MySQL/Percona 8.4).

Без аргументов в терминале открывается TUI-меню. Все команды работают через REST API панели:
локально — через /run/monopanel/api.sock (root = администратор), удалённо — через --server и --token.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
				cl, err := newClient()
				if err != nil {
					return err
				}
				return tui.Run(cmd.Context(), cl)
			}
			return cmd.Help()
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.config, "config", "", "путь к config.yaml (по умолчанию /etc/monopanel/config.yaml или $MONOPANEL_CONFIG)")
	pf.StringVar(&g.server, "server", os.Getenv("MP_SERVER"), "адрес панели для удалённого доступа, например https://host:8443 ($MP_SERVER)")
	pf.StringVar(&g.token, "token", os.Getenv("MP_TOKEN"), "API-токен для --server ($MP_TOKEN)")
	pf.BoolVar(&g.insecure, "insecure", false, "не проверять TLS-сертификат панели")
	pf.BoolVar(&g.json, "json", false, "машинный вывод JSON")
	pf.BoolVar(&g.noWait, "no-wait", false, "не ждать завершения задач")
	root.AddCommand(versionCmd(), apiCmd(), agentCmd(), helperCmd(), fsopCmd(), setupCmd(), statusCmd(), userCmd(), jobCmd(), tokenCmd(), serviceCmd(), stackCmd(), configCmd(), sslCmd(), webCmd(), phpCmd(), siteCmd(), cmsCmd(), dbCmd(), cronCmd(), firewallCmd(), doctorCmd(), selinuxCmd(), logsCmd(), metricsCmd(), backupCmd(), webhookCmd(), filesCmd(), dnsProviderCmd(), appCmd(), updateCmd(), updateRunCmd(), mailCmd(), migrateCmd())
	return root
}

func loadConfig() (config.Config, error) { return config.Load(g.config) }

func newClient() (*client.Client, error) {
	if g.server != "" {
		return client.NewRemote(g.server, g.token, g.insecure), nil
	}
	cfg, err := loadConfig()
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		// config.yaml is root:monopanel 0640; other local users still reach the
		// socket at the default path and are authenticated by their uid.
		cfg = config.Default()
	}
	return client.NewUnix(cfg.APISocket()), nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(header []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush() //nolint:errcheck // best effort; the caller reports the real failure
}
