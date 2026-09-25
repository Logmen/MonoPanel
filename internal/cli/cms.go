package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// cmsCmd installs a CMS into a site the way its vendor documents it, only
// without the clicking: preset, database, files, the CMS's own installer.
func cmsCmd() *cobra.Command {
	c := &cobra.Command{Use: "cms", Short: T("установка CMS в сайт: wordpress, joomla, opencart, bitrix", "install a CMS into a site: wordpress, joomla, opencart, bitrix")}
	c.AddCommand(&cobra.Command{Use: "list", Short: T("какие CMS панель умеет ставить и откуда берёт", "which CMSs the panel can install and where it gets them"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.CMSList(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := [][]string{}
		for _, d := range list {
			ed := ""
			if len(d.Editions) > 0 {
				ed = strings.Join(d.Editions, "|")
			}
			rows = append(rows, []string{d.ID, d.Name, d.Preset, ed, d.Source})
		}
		table([]string{"CMS", "NAME", "PRESET", "EDITIONS", "SOURCE"}, rows)
		return nil
	}})

	var req apitypes.CMSInstallRequest
	var passwordStdin bool
	install := &cobra.Command{Use: "install <domain> <cms>", Short: T("поставить CMS в сайт (пресет, база, файлы, установщик CMS; доступы печатаются один раз)", "install a CMS into a site (preset, database, files, the CMS's own installer; the credentials are printed once)"), Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.CMS = args[1]
		if passwordStdin {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			req.AdminPassword = strings.TrimRight(line, "\r\n")
		}
		res, err := cl.CMSInstall(cmd.Context(), args[0], req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("%s → %s\n", args[1], args[0])
		fmt.Printf(T("  Админка:  %s\n", "  Admin URL:  %s\n"), res.AdminURL)
		fmt.Printf(T("  Логин:    %s\n", "  Login:      %s\n"), res.AdminLogin)
		if res.AdminPassword != "" {
			fmt.Printf(T("  Пароль:   %s (сгенерирован, показан один раз)\n", "  Password:   %s (generated, shown once)\n"), res.AdminPassword)
		}
		fmt.Printf(T("  E-mail:   %s\n", "  E-mail:     %s\n"), res.AdminEmail)
		fmt.Printf(T("  База:     %s\n", "  Database:   %s\n"), res.Database)
		return followJob(cmd, cl, res.JobID)
	}}
	install.Flags().StringVar(&req.Title, "title", "", T("название сайта (по умолчанию домен)", "site title (the domain by default)"))
	install.Flags().StringVar(&req.AdminLogin, "admin-login", "", T("логин администратора CMS (по умолчанию admin)", "CMS administrator login (admin by default)"))
	install.Flags().StringVar(&req.AdminPassword, "admin-password", "", T("пароль администратора, 12–20 символов (иначе генерируется)", "administrator password, 12–20 characters (generated if omitted)"))
	install.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль администратора из stdin", "read the administrator password from stdin"))
	install.Flags().StringVar(&req.AdminEmail, "admin-email", "", T("e-mail администратора (по умолчанию e-mail владельца или admin@<домен>)", "administrator e-mail (by default the owner's e-mail or admin@<domain>)"))
	install.Flags().StringVar(&req.Edition, "edition", "", T("редакция Битрикса: start (по умолчанию), standard, small_business или business", "Bitrix edition: start (default), standard, small_business or business"))
	install.Flags().StringVar(&req.Solution, "solution", "", T("решение Битрикса: clean (чистая установка из Маркетплейса, по умолчанию), demo (демо-сайт из дистрибутива) или id решения из Маркетплейса", "Bitrix solution: clean (the Marketplace clean install, default), demo (the demo site from the distribution) or the id of a Marketplace solution"))
	install.Flags().BoolVar(&req.Force, "force", false, T("ставить в непустой docroot: его файлы удаляются, прежняя база этой CMS на сайте очищается (чужие базы не трогаются)", "install into a non-empty docroot: its files are deleted and the site's previous database of this CMS is emptied (other databases are left alone)"))
	c.AddCommand(install)
	return c
}
