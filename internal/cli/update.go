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
	"monopanel/internal/systemd"
	"monopanel/internal/updater"
)

func updateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: T("обновление панели из релизов репозитория", "panel updates from the repository's releases"), RunE: func(cmd *cobra.Command, _ []string) error {
		return showUpdate(cmd, false)
	}}
	c.AddCommand(&cobra.Command{Use: "check", Short: T("спросить репозиторий о новой версии", "ask the repository for a new version"), RunE: func(cmd *cobra.Command, _ []string) error {
		return showUpdate(cmd, true)
	}})

	var version string
	apply := &cobra.Command{Use: "apply", Short: T("установить новую версию (панель перезапустится)", "install the new version (the panel restarts)"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Fprintln(os.Stderr, T("установка идёт в фоне; проверить: mp update", "installing in the background; to check: mp update"))
		}
		return nil
	}}
	apply.Flags().StringVar(&version, "version", "", T("версия или тег (по умолчанию последняя в канале)", "version or tag (default: the latest in the channel)"))
	c.AddCommand(apply)

	var req apitypes.UpdateSettingsRequest
	var hours int
	var auto bool
	var tokenStdin bool
	settings := &cobra.Command{Use: "settings", Short: T("репозиторий, канал, токен и расписание проверок", "repository, channel, token and check schedule"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	settings.Flags().StringVar(&req.Repo, "repo", "", T("репозиторий с релизами (owner/name; \"-\" отключить обновления)", "repository with the releases (owner/name; \"-\" turns updates off)"))
	settings.Flags().StringVar(&req.API, "api", "", T("адрес API репозитория (по умолчанию api.github.com; \"-\" вернуть обратно)", "the repository's API address (default api.github.com; \"-\" resets it)"))
	settings.Flags().StringVar(&req.Channel, "channel", "", T("stable или beta", "stable or beta"))
	settings.Flags().StringVar(&req.Token, "token", "", T("токен доступа к репозиторию", "repository access token"))
	settings.Flags().BoolVar(&tokenStdin, "token-stdin", false, T("прочитать токен из stdin, не оставляя его в истории команд", "read the token from stdin, keeping it out of the shell history"))
	settings.Flags().BoolVar(&req.ClearToken, "clear-token", false, T("удалить сохранённый токен", "delete the saved token"))
	settings.Flags().IntVar(&hours, "check-hours", 24, T("как часто проверять обновления (0 — не проверять)", "how often to check for updates (0 — never)"))
	settings.Flags().BoolVar(&auto, "auto-apply", false, T("устанавливать обновления автоматически", "install updates automatically"))
	c.AddCommand(settings)

	var key string
	var restart, clear bool
	trust := &cobra.Command{Use: "trust", Short: T("ключ, которым подписаны релизы (пишется в config.yaml)", "the key releases are signed with (written to config.yaml)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if clear {
			cfg.Update.PublicKey = ""
			if err := cfg.Save(cfg.Path()); err != nil {
				return err
			}
			fmt.Println(T("ключ удалён: релизы будут приниматься по контрольной сумме", "key removed: releases will be accepted by their checksum"))
			key = "-"
		}
		if key == "" {
			if cfg.Update.PublicKey == "" {
				fmt.Println(T("ключ обновлений не задан: релизы принимаются по контрольной сумме", "no update key set: releases are accepted by their checksum"))
				return nil
			}
			fmt.Println(cfg.Update.PublicKey)
			return nil
		}
		if key != "-" {
			if _, err := updater.ParsePublicKey(key); err != nil {
				return err
			}
			cfg.Update.PublicKey = key
			if err := cfg.Save(cfg.Path()); err != nil {
				return err
			}
			fmt.Println(T("ключ сохранён в", "key saved to"), cfg.Path())
		}
		// Both daemons read the key at startup: the API to check a release
		// before downloading it, the agent to check it again before install.
		if !restart {
			fmt.Println(T("применится после перезапуска: systemctl restart monopanel-agent monopanel-api (или --restart)", "takes effect after a restart: systemctl restart monopanel-agent monopanel-api (or --restart)"))
			return nil
		}
		sd, err := systemd.Connect(cmd.Context())
		if err != nil {
			return err
		}
		defer sd.Close()
		for _, unit := range []string{cfg.Update.AgentUnit, cfg.Update.APIUnit} {
			if err := sd.Restart(cmd.Context(), unit); err != nil {
				return err
			}
		}
		fmt.Println(T("панель перезапущена", "panel restarted"))
		return nil
	}}
	trust.Flags().StringVar(&key, "key", "", T("публичный ключ ed25519 в base64 (без флага — показать текущий)", "ed25519 public key in base64 (omit it to see the current one)"))
	trust.Flags().BoolVar(&clear, "clear", false, T("убрать ключ и принимать релизы без подписи", "remove the key and accept unsigned releases"))
	trust.Flags().BoolVar(&restart, "restart", false, T("перезапустить панель, чтобы ключ начал действовать", "restart the panel so that the key takes effect"))
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
	fmt.Printf(T("Версия:      %s\n", "Version:    %s\n"), st.Current)
	switch {
	case st.Settings.Repo == "":
		fmt.Println(T("Репозиторий: не настроен (mp update settings --repo owner/name --token-stdin)", "Repository: not configured (mp update settings --repo owner/name --token-stdin)"))
	default:
		where := st.Settings.Repo
		if st.Settings.API != "" {
			where += " @ " + st.Settings.API
		}
		if st.Settings.RepoBuiltIn {
			where += T(", из сборки", ", built-in")
		}
		fmt.Printf(T("Репозиторий: %s (%s)\n", "Repository: %s (%s)\n"), where, st.Settings.Channel)
	}
	if st.Latest != "" {
		when := ""
		if st.PublishedAt != nil {
			when = ", " + st.PublishedAt.Local().Format("2006-01-02")
		}
		if st.Available {
			fmt.Printf(T("Доступна:    %s%s — mp update apply\n", "Available:  %s%s — mp update apply\n"), st.Latest, when)
		} else {
			fmt.Printf(T("Последняя:   %s%s — установлена свежая версия\n", "Latest:     %s%s — up to date\n"), st.Latest, when)
		}
	}
	if st.CheckedAt != nil {
		fmt.Printf(T("Проверено:   %s\n", "Checked:    %s\n"), st.CheckedAt.Local().Format("2006-01-02 15:04"))
	}
	if st.LastError != "" {
		fmt.Printf(T("Ошибка:      %s\n", "Error:      %s\n"), st.LastError)
	}
	auto := T("по запросу", "installed on request")
	if st.Settings.AutoApply {
		auto = T("автоматически", "installed automatically")
	}
	every := fmt.Sprintf(T("каждые %d ч", "checked every %d h"), st.Settings.CheckHours)
	if st.Settings.CheckHours == 0 {
		every = T("проверка выключена", "checks off")
	}
	key := T("без подписи", "no signature check")
	if st.KeyPinned {
		key = T("подпись обязательна", "signature required")
	}
	fmt.Printf(T("Обновления:  %s, %s, %s\n", "Updates:    %s, %s, %s\n"), every, auto, key)
	if a := st.LastAttempt; a != nil {
		line := fmt.Sprintf(T("Последняя установка: %s → %s, %s", "Last install: %s → %s, %s"), a.From, a.To, a.Status)
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
	c := &cobra.Command{Use: "update-run", Short: T("установить подготовленный пакет обновления", "install a staged update package"), Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Geteuid() != 0 {
			return &exitError{code: 4, msg: T("update-run выполняется только от root", "update-run must run as root")}
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
	c.Flags().StringVar(&pkg, "package", "", T("путь к .deb или .rpm", "path to the .deb or .rpm"))
	c.Flags().StringVar(&sha, "sha256", "", T("ожидаемая контрольная сумма пакета", "expected checksum of the package"))
	c.Flags().StringVar(&version, "version", "", T("устанавливаемая версия", "version being installed"))
	c.Flags().StringVar(&from, "from", buildinfo.Version, T("версия, которую заменяем", "version being replaced"))
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
