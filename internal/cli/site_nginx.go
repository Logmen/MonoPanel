package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func siteNginxCmd() *cobra.Command {
	var setFile string
	var clear, generated bool
	c := &cobra.Command{Use: "nginx <domain>", Short: T("свои nginx-директивы сайта (sites/<domain>.d/custom.conf): показать, --set файл|-, --clear", "the site's custom nginx directives (sites/<domain>.d/custom.conf): show, --set file|-, --clear"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if clear || setFile != "" {
			content := ""
			if setFile == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				content = string(b)
			} else if setFile != "" {
				b, err := os.ReadFile(setFile)
				if err != nil {
					return err
				}
				content = string(b)
			}
			res, err := cl.SetSiteNginx(cmd.Context(), args[0], content)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(res)
			}
			if strings.TrimSpace(res.Custom) == "" {
				fmt.Printf(T("%s: свои директивы убраны, nginx перезагружен\n", "%s: custom directives removed, nginx reloaded\n"), args[0])
			} else {
				fmt.Printf(T("%s: записано в %s (%d байт), nginx проверен и перезагружен\n", "%s: written to %s (%d bytes), nginx checked and reloaded\n"), args[0], res.CustomPath, len(res.Custom))
			}
			return nil
		}
		res, err := cl.SiteNginx(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		if generated {
			fmt.Print(res.Generated)
			return nil
		}
		fmt.Printf(T("# %s — свои директивы (%s)\n", "# %s — custom directives (%s)\n"), args[0], res.CustomPath)
		if len(res.Others) > 0 {
			fmt.Printf(T("# другие include-файлы в %s: %s\n", "# other include files in %s: %s\n"), res.IncludeDir, strings.Join(res.Others, ", "))
		}
		if strings.TrimSpace(res.Custom) == "" {
			fmt.Println(T("# (пусто)", "# (empty)"))
		} else {
			fmt.Print(res.Custom)
		}
		return nil
	}}
	c.Flags().StringVar(&setFile, "set", "", T("записать директивы из файла ('-' = stdin); проверка nginx -t, откат при ошибке", "write the directives from a file ('-' = stdin); checked with nginx -t, rolled back on error"))
	c.Flags().BoolVar(&clear, "clear", false, T("удалить свои директивы", "delete the custom directives"))
	c.Flags().BoolVar(&generated, "generated", false, T("показать сгенерированный server-блок", "show the generated server block"))
	return c
}

func sitePHPCmd() *cobra.Command {
	c := &cobra.Command{Use: "php <domain>", Short: T("эффективные PHP-параметры сайта: панель → глобально → пресет → сайт (для сайта: mp site set --ini, для всех: mp php ini set)", "the site's effective PHP settings: panel → global → preset → site (for the site: mp site set --ini, for all sites: mp php ini set)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.SitePHP(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf(T("PHP %s · пул %s\n", "PHP %s · pool %s\n"), res.Version, res.PoolPath)
		rows := make([][]string, 0, len(res.Values))
		for _, v := range res.Values {
			rows = append(rows, []string{v.Key, v.Value, v.Source})
		}
		table([]string{"KEY", "VALUE", "SOURCE"}, rows)
		fmt.Printf(T("допустимые ключи: %s\n", "allowed keys: %s\n"), strings.Join(res.Allowed, ", "))
		return nil
	}}
	return c
}
