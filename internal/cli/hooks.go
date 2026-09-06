package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func webhookCmd() *cobra.Command {
	c := &cobra.Command{Use: "webhook", Short: "webhooks для биллинга и мониторинга (HMAC-SHA256)"}
	var req apitypes.WebhookRequest
	var events string
	add := &cobra.Command{Use: "add", Short: "добавить подписчика", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if events != "" {
			req.Events = strings.Split(events, ",")
		}
		h, err := cl.WebhookCreate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(h)
		}
		fmt.Printf("webhook %s → %s, события: %s\nсекрет для проверки подписи: %s\n", h.ID, h.URL, strings.Join(h.Events, ","), h.Secret)
		return nil
	}}
	add.Flags().StringVar(&req.URL, "url", "", "адрес приёмника")
	add.Flags().StringVar(&req.Secret, "secret", "", "секрет HMAC (иначе генерируется)")
	add.Flags().StringVar(&events, "events", "*", "список через запятую: job.failed, site.apply.done, cert.issue.*, backup.run.*, *")
	add.MarkFlagRequired("url")
	list := &cobra.Command{Use: "list", Short: "список", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		hs, err := cl.Webhooks(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(hs)
		}
		rows := make([][]string, 0, len(hs))
		for _, h := range hs {
			rows = append(rows, []string{h.ID, h.URL, strings.Join(h.Events, ","), h.CreatedAt})
		}
		table([]string{"ID", "URL", "EVENTS", "CREATED"}, rows)
		return nil
	}}
	rm := &cobra.Command{Use: "rm <id>", Short: "удалить", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.WebhookDelete(cmd.Context(), args[0])
	}}
	test := &cobra.Command{Use: "test <id>", Short: "отправить тестовое событие", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.WebhookTest(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Println("доставлено")
		return nil
	}}
	c.AddCommand(add, list, rm, test)
	return c
}
