package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/updater"
)

func updateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "обновление панели из релизов репозитория", RunE: func(cmd *cobra.Command, _ []string) error {
		return showUpdate(cmd, false)
	}}
	c.AddCommand(&cobra.Command{Use: "check", Short: "спросить репозиторий о новой версии", RunE: func(cmd *cobra.Command, _ []string) error {
		return showUpdate(cmd, true)
	}})

	var version string
	apply := &cobra.Command{Use: "apply", Short: "установить новую версию (панель перезапустится)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.ApplyUpdate(cmd.Context(), version)
		if err != nil {
			return err
		}
		if err := followJob(cmd, cl, ref.JobID); err != nil {
			return err
		}
		if !g.json {
			fmt.Fprintln(os.Stderr, "установка идёт в фоне; проверить: mp update")
		}
		return nil
	}}
	apply.Flags().StringVar(&version, "version", "", "версия или тег (по умолчанию последняя в канале)")
	c.AddCommand(apply)

	var req apitypes.UpdateSettingsRequest
	var hours int
	var auto bool
	var tokenStdin bool
	settings := &cobra.Command{Use: "settings", Short: "репозиторий, канал, токен и расписание проверок", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("check-hours") {
			req.CheckHours = &hours
		}
		if cmd.Flags().Changed("auto-apply") {
			req.AutoApply = &auto
		}
		if tokenStdin {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			req.Token = strings.TrimRight(line, "\r\n")
		}
		st, err := cl.SetUpdateSettings(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		printUpdate(st)
		return nil
	}}
	settings.Flags().StringVar(&req.Repo, "repo", "", "репозиторий с релизами (owner/name)")
	settings.Flags().StringVar(&req.Channel, "channel", "", "stable или beta")
	settings.Flags().StringVar(&req.Token, "token", "", "токен доступа к репозиторию")
	settings.Flags().BoolVar(&tokenStdin, "token-stdin", false, "прочитать токен из stdin, не оставляя его в истории команд")
	settings.Flags().BoolVar(&req.ClearToken, "clear-token", false, "удалить сохранённый токен")
	settings.Flags().IntVar(&hours, "check-hours", 24, "как часто проверять обновления (0 — не проверять)")
	settings.Flags().BoolVar(&auto, "auto-apply", false, "устанавливать обновления автоматически")
	c.AddCommand(settings)

	var key string
	trust := &cobra.Command{Use: "trust", Short: "ключ, которым подписаны релизы (пишется в config.yaml)", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if key == "" {
			if cfg.Update.PublicKey == "" {
				fmt.Println("ключ обновлений не задан: релизы принимаются по контрольной сумме")
				return nil
			}
			fmt.Println(cfg.Update.PublicKey)
			return nil
		}
		if _, err := updater.ParsePublicKey(key); err != nil {
			return err
		}
		cfg.Update.PublicKey = key
		if err := cfg.Save(cfg.Path()); err != nil {
			return err
		}
		fmt.Println("ключ сохранён в", cfg.Path())
		return nil
	}}
	trust.Flags().StringVar(&key, "key", "", "публичный ключ ed25519 в base64 (без флага — показать текущий)")
	c.AddCommand(trust)
	return c
}

func showUpdate(cmd *cobra.Command, refresh bool) error {
	cl, err := newClient()
	if err != nil {
		return err
	}
	var st *apitypes.UpdateStatus
	if refresh {
		st, err = cl.CheckUpdate(cmd.Context())
	} else {
		st, err = cl.UpdateStatus(cmd.Context())
	}
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(st)
	}
	printUpdate(st)
	return nil
}

func printUpdate(st *apitypes.UpdateStatus) {
	fmt.Printf("Версия:      %s\n", st.Current)
	switch {
	case st.Settings.Repo == "":
		fmt.Println("Репозиторий: не настроен (mp update settings --repo owner/name --token-stdin)")
	default:
		fmt.Printf("Репозиторий: %s (%s)\n", st.Settings.Repo, st.Settings.Channel)
	}
	if st.Latest != "" {
		when := ""
		if st.PublishedAt != nil {
			when = ", " + st.PublishedAt.Local().Format("2006-01-02")
		}
		if st.Available {
			fmt.Printf("Доступна:    %s%s — mp update apply\n", st.Latest, when)
		} else {
			fmt.Printf("Последняя:   %s%s — установлена свежая версия\n", st.Latest, when)
		}
	}
	if st.CheckedAt != nil {
		fmt.Printf("Проверено:   %s\n", st.CheckedAt.Local().Format("2006-01-02 15:04"))
	}
	if st.LastError != "" {
		fmt.Printf("Ошибка:      %s\n", st.LastError)
	}
	auto := "по запросу"
	if st.Settings.AutoApply {
		auto = "автоматически"
	}
	every := fmt.Sprintf("каждые %d ч", st.Settings.CheckHours)
	if st.Settings.CheckHours == 0 {
		every = "проверка выключена"
	}
	key := "без подписи"
	if st.KeyPinned {
		key = "подпись обязательна"
	}
	fmt.Printf("Обновления:  %s, %s, %s\n", every, auto, key)
	if a := st.LastAttempt; a != nil {
		line := fmt.Sprintf("Последняя установка: %s → %s, %s", a.From, a.To, a.Status)
		if a.Error != "" {
			line += ": " + a.Error
		}
		fmt.Println(line)
	}
}

// updateRunCmd installs a staged package and restarts the panel. The agent
// starts it as a transient systemd unit, so it survives the restart it causes;
// it is never meant to be run by hand.
func updateRunCmd() *cobra.Command {
	var pkg, sha, version, from string
	c := &cobra.Command{Use: "update-run", Short: "установить подготовленный пакет обновления", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Geteuid() != 0 {
			return &exitError{code: 4, msg: "update-run выполняется только от root"}
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		profile, err := osprofile.Detect()
		if err != nil {
			return err
		}
		inst := &updater.Installer{
			Package: pkg,
			SHA256:  sha,
			To:      version,
			From:    from,
			Dir:     cfg.UpdatesDir(),
			Binary:  panelBinary(),
			Argv:    profile.Packages().InstallArgv([]string{pkg}),
			Units:   []string{cfg.Update.AgentUnit, cfg.Update.APIUnit},
			Ready:   panelVersionProbe(cfg),
		}
		return inst.Run(cmd.Context())
	}}
	c.Flags().StringVar(&pkg, "package", "", "путь к .deb или .rpm")
	c.Flags().StringVar(&sha, "sha256", "", "ожидаемая контрольная сумма пакета")
	c.Flags().StringVar(&version, "version", "", "устанавливаемая версия")
	c.Flags().StringVar(&from, "from", buildinfo.Version, "версия, которую заменяем")
	c.MarkFlagRequired("package")
	c.MarkFlagRequired("sha256")
	c.MarkFlagRequired("version")
	return c
}

func panelBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "/usr/bin/monopanel"
	}
	return exe
}

// panelVersionProbe asks the freshly restarted panel what version it is, over
// the local socket: the update is only done once the new code answers.
func panelVersionProbe(cfg config.Config) func(context.Context) (string, error) {
	sock := cfg.APISocket()
	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
	return func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://panel/api/v1/health", nil)
		if err != nil {
			return "", err
		}
		res, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		var body struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&body); err != nil {
			return "", err
		}
		return body.Version, nil
	}
}
