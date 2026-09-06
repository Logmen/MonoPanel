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
		Short: "первичная настройка панели (запускать от root)",
		Long: `Создаёт служебного пользователя и группы, каталоги, config.yaml, секретный ключ, самоподписанный
сертификат, базу данных с администратором и systemd-units, затем запускает сервисы.
Команду можно запускать повторно: существующее не трогается, отсутствующее создаётся.`,
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
			fmt.Println("MonoPanel готова.")
			for _, u := range res.URLs {
				fmt.Println("  Web:      ", u)
			}
			fmt.Println("  API docs: ", "/api/v1/docs")
			fmt.Println("  Логин:    ", res.AdminLogin)
			if res.AdminCreated {
				if res.GeneratedPassword {
					fmt.Println("  Пароль:   ", res.AdminPassword, "(сгенерирован, показан один раз)")
				} else {
					fmt.Println("  Пароль:    задан")
				}
			}
			fmt.Println("  TLS SHA-256:", res.Fingerprint)
			if !res.Healthy && opts.Start {
				fmt.Println("  Внимание: API не ответил на проверку здоровья; см. journalctl -u monopanel-api")
			}
			return nil
		},
	}
	c.Flags().StringVar(&opts.Hostname, "hostname", "", "имя хоста панели (для сертификата и ссылок)")
	c.Flags().StringVar(&opts.Listen, "listen", "", "адрес HTTPS-порта панели, например :8443 или 203.0.113.10:8443")
	c.Flags().StringVar(&opts.AdminLogin, "admin-login", "admin", "логин администратора")
	c.Flags().StringVar(&opts.AdminPassword, "admin-password", "", "пароль администратора (иначе генерируется)")
	c.Flags().BoolVar(&passwordStdin, "password-stdin", false, "прочитать пароль администратора из stdin")
	c.Flags().BoolVar(&opts.InstallUnits, "install-units", false, "перезаписать systemd-units даже если они существуют")
	c.Flags().StringVar(&opts.BinaryPath, "binary", "", "путь к бинарнику для units (по умолчанию текущий)")
	c.Flags().BoolVar(&noStart, "no-start", false, "не запускать сервисы")
	return c
}
