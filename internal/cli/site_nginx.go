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
	c := &cobra.Command{Use: "nginx <domain>", Short: "свои nginx-директивы сайта (sites/<domain>.d/custom.conf): показать, --set файл|-, --clear", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
				fmt.Printf("%s: свои директивы убраны, nginx перезагружен\n", args[0])
			} else {
				fmt.Printf("%s: записано в %s (%d байт), nginx проверен и перезагружен\n", args[0], res.CustomPath, len(res.Custom))
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
		fmt.Printf("# %s — свои директивы (%s)\n", args[0], res.CustomPath)
		if len(res.Others) > 0 {
			fmt.Printf("# другие include-файлы в %s: %s\n", res.IncludeDir, strings.Join(res.Others, ", "))
		}
		if strings.TrimSpace(res.Custom) == "" {
			fmt.Println("# (пусто)")
		} else {
			fmt.Print(res.Custom)
		}
		return nil
	}}
	c.Flags().StringVar(&setFile, "set", "", "записать директивы из файла ('-' = stdin); проверка nginx -t, откат при ошибке")
	c.Flags().BoolVar(&clear, "clear", false, "удалить свои директивы")
	c.Flags().BoolVar(&generated, "generated", false, "показать сгенерированный server-блок")
	return c
}

func sitePHPCmd() *cobra.Command {
	c := &cobra.Command{Use: "php <domain>", Short: "эффективные PHP-параметры сайта (значения панели + переопределения; менять: mp site set --ini)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf("PHP %s · пул %s\n", res.Version, res.PoolPath)
		rows := make([][]string, 0, len(res.Values))
		for _, v := range res.Values {
			rows = append(rows, []string{v.Key, v.Value, v.Source})
		}
		table([]string{"KEY", "VALUE", "SOURCE"}, rows)
		fmt.Printf("допустимые ключи: %s\n", strings.Join(res.Allowed, ", "))
		return nil
	}}
	return c
}
