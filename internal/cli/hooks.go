package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func webhookCmd() *cobra.Command {
	c := &cobra.Command{Use: "webhook", Short: T("webhooks для биллинга и мониторинга (HMAC-SHA256)", "webhooks for billing and monitoring (HMAC-SHA256)")}
	var req apitypes.WebhookRequest
	var events string
	add := &cobra.Command{Use: "add", Short: T("добавить подписчика", "add a subscriber"), RunE: func(cmd *cobra.Command, _ []string) error {
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
		fmt.Printf(T("webhook %s → %s, события: %s\nсекрет для проверки подписи: %s\n", "webhook %s → %s, events: %s\nsigning secret: %s\n"), h.ID, h.URL, strings.Join(h.Events, ","), h.Secret)
		return nil
	}}
	add.Flags().StringVar(&req.URL, "url", "", T("адрес приёмника", "receiver URL"))
	add.Flags().StringVar(&req.Secret, "secret", "", T("секрет HMAC (иначе генерируется)", "HMAC secret (generated if omitted)"))
	add.Flags().StringVar(&events, "events", "*", T("список через запятую: job.failed, site.apply.done, cert.issue.*, backup.run.*, *", "comma-separated list: job.failed, site.apply.done, cert.issue.*, backup.run.*, *"))
	add.MarkFlagRequired("url")
	list := &cobra.Command{Use: "list", Short: T("список", "list webhooks"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	rm := &cobra.Command{Use: "rm <id>", Short: T("удалить", "delete a webhook"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.WebhookDelete(cmd.Context(), args[0])
	}}
	test := &cobra.Command{Use: "test <id>", Short: T("отправить тестовое событие", "send a test event"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.WebhookTest(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Println(T("доставлено", "delivered"))
		return nil
	}}
	c.AddCommand(add, list, rm, test)
	return c
}
