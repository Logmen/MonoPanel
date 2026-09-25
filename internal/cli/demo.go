//go:build demo

package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"monopanel/internal/demo"
)

func init() { extraCommands = append(extraCommands, demoCmd) }

// demoCmd runs the public live demo: `prepare` when the image is built,
// `serve` in every container (internal/demo).
func demoCmd() *cobra.Command {
	o := demo.Options{Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	c := &cobra.Command{Use: "demo", Hidden: true, Short: "the live demo: the panel over a pretend server (demo build only)"}
	pf := c.PersistentFlags()
	pf.StringVar(&o.Dir, "dir", "/opt/monopanel-demo", "the prepared demo: config, database, the pretend server's state")
	pf.StringVar(&o.WWW, "www", "/var/www", "the account homes (real files)")
	pf.StringVar(&o.PanelAddr, "panel", "127.0.0.1:8443", "where the API listens with HTTPS inside")
	pf.StringVar(&o.Run, "run", "", "the socket directory (default <dir>/run)")
	self := func() error {
		exe, err := os.Executable()
		o.Self = exe
		return err
	}
	c.AddCommand(&cobra.Command{Use: "prepare", Short: "build the demo: accounts, sites, databases, mail, backups, a day of metrics", RunE: func(cmd *cobra.Command, _ []string) error {
		if err := self(); err != nil {
			return err
		}
		return demo.Prepare(cmd.Context(), o)
	}})
	serve := &cobra.Command{Use: "serve", Short: "run a prepared demo and answer the Worker on --listen", RunE: func(cmd *cobra.Command, _ []string) error {
		if err := self(); err != nil {
			return err
		}
		return demo.Serve(cmd.Context(), o)
	}}
	serve.Flags().StringVar(&o.Listen, "listen", ":8080", "plain HTTP address for the Worker")
	c.AddCommand(serve)
	return c
}
