// Package cli implements the `mp` command: daemon entry points (api, agent,
// helper) and the operator commands, all thin clients of the REST API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"monopanel/internal/termsafe"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"monopanel/internal/buildinfo"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/i18n"
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

// T is the text in the language of the terminal: T("Сайты", "Sites"). The
// language is known once Main has started, so texts live in functions, not in
// package-level vars.
func T(ru, en string) string { return i18n.T(ru, en) }

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	i18n.Detect(os.Getenv)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "error:", termsafe.Text(ee.msg))
			}
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", termsafe.Text(err.Error()))
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintln(os.Stderr, T("сокет панели открыт root и аккаунтам с shell-доступом (группа monopanel-cli); остальным — --server и --token", "the panel socket is open to root and to accounts with shell access (group monopanel-cli); everyone else uses --server and --token"))
			return 4
		}
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

// extraCommands are added by build variants: the demo build (-tags demo)
// brings `monopanel demo`; the released binary has none.
var extraCommands []func() *cobra.Command

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "mp",
		Short: T("MonoPanel — панель управления веб-сервером", "MonoPanel — a web hosting control panel"),
		Long: T(`MonoPanel — панель управления веб-сервером (nginx + php-fpm, nginx + Apache, PHP 5.6–8.5, MySQL/Percona 8.4).

Без аргументов в терминале открывается TUI-меню. Команды работают через REST API панели — с теми же
правами и проверками, что и веб-интерфейс: локально — через /run/monopanel/api.sock (root = администратор,
аккаунт с shell-доступом — он сам), удалённо — через --server и --token ($MP_SERVER, $MP_TOKEN).

Без API работают только help, completion и version, а от root на сервере — ещё setup (первичная
настройка), config show --local и config set --local (восстановление, когда API не запускается) и
update trust (ключ подписи релизов: самой панели он нарочно недоступен).`, `MonoPanel — a web hosting control panel (nginx + php-fpm, nginx + Apache, PHP 5.6–8.5, MySQL/Percona 8.4).

Without arguments in a terminal it opens the TUI menu. Commands go through the panel's REST API, with the
same rights and checks as the web interface: locally over /run/monopanel/api.sock (root = administrator,
an account with shell access = that account), remotely with --server and --token ($MP_SERVER, $MP_TOKEN).

Only help, completion and version work without the API, and, as root on the server, also setup (the
initial setup), config show --local and config set --local (recovery when the API does not start) and
update trust (the release signing key, kept out of the panel's reach on purpose).`),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
				cl, err := newClient()
				if err != nil {
					return err
				}
				return tui.Run(cmd.Context(), cl, cmd.Root(), tui.Options{Server: g.server, Token: panelToken(), Insecure: g.insecure, Config: g.config})
			}
			return cmd.Help()
		},
	}
	root.Version = buildinfo.String()
	root.SetVersionTemplate("monopanel {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&g.config, "config", "", T("путь к config.yaml (по умолчанию /etc/monopanel/config.yaml или $MONOPANEL_CONFIG)", "path to config.yaml (default /etc/monopanel/config.yaml or $MONOPANEL_CONFIG)"))
	pf.StringVar(&g.server, "server", os.Getenv("MP_SERVER"), T("адрес панели для удалённого доступа, например https://host:8443 ($MP_SERVER)", "panel address for remote access, e.g. https://host:8443 ($MP_SERVER)"))
	// No default from $MP_TOKEN here: cobra prints a flag's default in every
	// --help, and the token would be on screen (and in the console's output).
	// panelToken reads the variable when the flag is not given.
	pf.StringVar(&g.token, "token", "", T("API-токен для --server ($MP_TOKEN)", "API token for --server ($MP_TOKEN)"))
	pf.BoolVar(&g.insecure, "insecure", os.Getenv("MP_INSECURE") == "1", T("не проверять TLS-сертификат панели ($MP_INSECURE=1)", "do not verify the panel's TLS certificate ($MP_INSECURE=1)"))
	pf.BoolVar(&g.json, "json", false, T("машинный вывод JSON", "machine-readable JSON output"))
	pf.BoolVar(&g.noWait, "no-wait", false, T("не ждать завершения задач", "do not wait for jobs to finish"))
	for _, extra := range extraCommands {
		root.AddCommand(extra())
	}
	root.AddCommand(versionCmd(), apiCmd(), agentCmd(), helperCmd(), fsopCmd(), setupCmd(), statusCmd(), userCmd(), jobCmd(), auditCmd(), tokenCmd(), serviceCmd(), stackCmd(), configCmd(), sslCmd(), webCmd(), phpCmd(), siteCmd(), cmsCmd(), dbCmd(), cronCmd(), firewallCmd(), doctorCmd(), selinuxCmd(), logsCmd(), metricsCmd(), backupCmd(), webhookCmd(), filesCmd(), dnsProviderCmd(), appCmd(), valkeyCmd(), updateCmd(), updateRunCmd(), apiSocketSyncCmd(), mailCmd(), migrateCmd(), totpCmd(), whoamiCmd(), appExecCmd())
	return root
}

func loadConfig() (config.Config, error) { return config.Load(g.config) }

// panelToken is the token for --server: the flag, else $MP_TOKEN.
func panelToken() string {
	if g.token != "" {
		return g.token
	}
	return os.Getenv("MP_TOKEN")
}

func newClient() (*client.Client, error) {
	if g.server != "" {
		return client.NewRemote(g.server, panelToken(), g.insecure), nil
	}
	sock, err := apiSocket()
	if err != nil {
		return nil, err
	}
	return client.NewUnix(sock), nil
}

// apiSocket is the panel socket mp talks to without --server.
func apiSocket() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return "", err
		}
		// config.yaml is root:monopanel 0640; other local users still reach the
		// socket at the default path and are authenticated by their uid.
		cfg = config.Default()
	}
	return cfg.APISocket(), nil
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
		// The cells are the server's values (names, comments, a login a
		// stranger typed): no escape sequence of theirs reaches the terminal,
		// and a tab or a line break cannot shift the columns.
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = strings.ReplaceAll(termsafe.Line(c), "\t", " ")
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush() //nolint:errcheck // best effort; the caller reports the real failure
}
