package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"monopanel/internal/osprofile"
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
			if err := secretFlag(cmd, "password-stdin", "admin-password", T("Пароль администратора: ", "Administrator password: "), &opts.AdminPassword); err != nil {
				return err
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
	c.Flags().BoolVar(&opts.InstallUnits, "install-units", false, T("перезаписать systemd-units в /etc/systemd/system, даже если они есть (кроме тех, что ставит пакет)", "overwrite the systemd units in /etc/systemd/system even if they exist (but those the package installs)"))
	c.Flags().StringVar(&opts.BinaryPath, "binary", "", T("путь к бинарнику для units (по умолчанию текущий)", "path to the binary for the units (default: the current one)"))
	c.Flags().BoolVar(&noStart, "no-start", false, T("не запускать сервисы", "do not start the services"))
	return c
}

// pruneUnitCopies removes the panel's own copies of the package's units
// from /etc/systemd/system, pruneModprobeCopy its copy of the package's
// modprobe rule from /etc/modprobe.d; tests replace them.
var (
	pruneUnitCopies   = setup.PruneUnitCopies
	pruneModprobeCopy = setup.PruneModprobeCopy
)

// pruneUnitCopiesCmd is monopanel prune-unit-copies, for the package's
// postinstall: mp setup up to 0.8.19 copied the panel's units into
// /etc/systemd/system on every host it set up, and a removal or a downgrade
// below 0.9.0 keeps Apache's template there (preremove.sh); mp setup of an
// install without the package writes the modprobe rule into /etc/modprobe.d.
// Such a copy overrides the package's file for good once the package is
// there; the upgrade removes the ones that are provably the panel's own and
// no newer than the package's (setup.PruneUnitCopies,
// setup.PruneModprobeCopy), so the package's files, and what later releases
// change in them, apply. A modprobe rule the administrator changed stays,
// and the command says so: it is how a module may load again, and it keeps
// out what a later release adds to the rule.
func pruneUnitCopiesCmd() *cobra.Command {
	return &cobra.Command{Use: "prune-unit-copies", Short: T("удалить копии юнитов и правила modprobe пакета, которые панель оставила в /etc (для скриптов пакета)", "remove the copies of the package's units and modprobe rule the panel left in /etc (for the package scripts)"), Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return &exitError{code: 4, msg: T("prune-unit-copies выполняется только от root", "prune-unit-copies must run as root")}
			}
			removed, err := pruneUnitCopies(cmd.Context())
			for _, p := range removed {
				fmt.Printf(T("удалён %s — копия юнита пакета; действует юнит пакета\n", "removed %s, a copy of the package's unit: the package's unit applies\n"), p)
			}
			gone, kept, merr := pruneModprobeCopy()
			switch {
			case gone:
				fmt.Printf(T("удалён %s — копия правила пакета; действует %s\n", "removed %s, a copy of the package's rule: %s applies\n"), osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile)
			case kept:
				fmt.Printf(T("предупреждение: %s заменяет правило пакета %s, и его изменения в следующих версиях сюда не доходят; если файл не нужен, удалите его\n",
					"warning: %s replaces the package's rule %s, and what later releases change in it does not reach this host; unless you keep it on purpose, remove it\n"), osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile)
			}
			return errors.Join(err, merr)
		}}
}
