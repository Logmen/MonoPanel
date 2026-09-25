package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/setup"
)

func setupCmd() *cobra.Command {
	var opts setup.Options
	var passwordStdin, noStart bool
	c := &cobra.Command{
		Use:   "setup",
		Short: T("первичная настройка панели (запускать от root)", "initial setup of the panel (run as root)"),
		Long: T(`Создаёт служебного пользователя и группы, каталоги, config.yaml, секретный ключ, самоподписанный
сертификат, базу данных с администратором и systemd-units, затем запускает сервисы.
Команду можно запускать повторно: существующее не трогается, отсутствующее создаётся.`,
			`Creates the service user and groups, the directories, config.yaml, the secret key, a self-signed
certificate, the database with an administrator and the systemd units, then starts the services.
The command can be run again: what exists is left alone, what is missing is created.`),
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ConfigPath = g.config
			opts.Start = !noStart
			if passwordStdin {
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				opts.AdminPassword = strings.TrimRight(line, "\r\n")
			}
			res, err := setup.Run(cmd.Context(), opts, os.Stderr)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(res)
			}
			fmt.Println()
			fmt.Println(T("MonoPanel готова.", "MonoPanel is ready."))
			for _, u := range res.URLs {
				fmt.Println("  Web:      ", u)
			}
			fmt.Println("  API docs: ", T("/api/v1/docs (после входа)", "/api/v1/docs (once signed in)"))
			fmt.Println(T("  Логин:    ", "  Login:    "), res.AdminLogin)
			if res.AdminCreated {
				if res.GeneratedPassword {
					fmt.Println(T("  Пароль:   ", "  Password: "), res.AdminPassword, T("(сгенерирован, показан один раз)", "(generated, shown only once)"))
				} else {
					fmt.Println(T("  Пароль:    задан", "  Password:  as specified"))
				}
			}
			fmt.Println("  TLS SHA-256:", res.Fingerprint)
			if !res.Healthy && opts.Start {
				fmt.Println(T("  Внимание: API не ответил на проверку здоровья; см. journalctl -u monopanel-api", "  Warning: the API did not answer the health check; see journalctl -u monopanel-api"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&opts.Hostname, "hostname", "", T("имя хоста панели (для сертификата и ссылок)", "the panel's hostname (for the certificate and links)"))
	c.Flags().StringVar(&opts.Listen, "listen", "", T("адрес HTTPS-порта панели, например :8443 или 203.0.113.10:8443", "address of the panel's HTTPS port, e.g. :8443 or 203.0.113.10:8443"))
	c.Flags().StringVar(&opts.AdminLogin, "admin-login", "admin", T("логин администратора", "administrator login"))
	c.Flags().StringVar(&opts.AdminPassword, "admin-password", "", T("пароль администратора (иначе генерируется)", "administrator password (generated if omitted)"))
	c.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль администратора из stdin", "read the administrator password from stdin"))
	c.Flags().BoolVar(&opts.InstallUnits, "install-units", false, T("перезаписать systemd-units даже если они существуют", "overwrite the systemd units even if they exist"))
	c.Flags().StringVar(&opts.BinaryPath, "binary", "", T("путь к бинарнику для units (по умолчанию текущий)", "path to the binary for the units (default: the current one)"))
	c.Flags().BoolVar(&noStart, "no-start", false, T("не запускать сервисы", "do not start the services"))
	return c
}
