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
	c := &cobra.Command{Use: "cms", Short: "установка CMS в сайт: wordpress, joomla, opencart, bitrix"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "какие CMS панель умеет ставить и откуда берёт", RunE: func(cmd *cobra.Command, _ []string) error {
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
	install := &cobra.Command{Use: "install <domain> <cms>", Short: "поставить CMS в сайт (пресет, база, файлы, установщик CMS; доступы печатаются один раз)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf("  Админка:  %s\n", res.AdminURL)
		fmt.Printf("  Логин:    %s\n", res.AdminLogin)
		if res.AdminPassword != "" {
			fmt.Printf("  Пароль:   %s (сгенерирован, показан один раз)\n", res.AdminPassword)
		}
		fmt.Printf("  E-mail:   %s\n", res.AdminEmail)
		fmt.Printf("  База:     %s\n", res.Database)
		return followJob(cmd, cl, res.JobID)
	}}
	install.Flags().StringVar(&req.Title, "title", "", "название сайта (по умолчанию домен)")
	install.Flags().StringVar(&req.AdminLogin, "admin-login", "", "логин администратора CMS (по умолчанию admin)")
	install.Flags().StringVar(&req.AdminPassword, "admin-password", "", "пароль администратора, 12–20 символов (иначе генерируется)")
	install.Flags().BoolVar(&passwordStdin, "password-stdin", false, "прочитать пароль администратора из stdin")
	install.Flags().StringVar(&req.AdminEmail, "admin-email", "", "e-mail администратора (по умолчанию e-mail владельца или admin@<домен>)")
	install.Flags().StringVar(&req.Edition, "edition", "", "редакция Битрикса: start (по умолчанию) или business")
	install.Flags().BoolVar(&req.Force, "force", false, "ставить в непустой docroot: его файлы удаляются")
	c.AddCommand(install)
	return c
}
