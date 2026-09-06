package cli

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"monopanel/internal/agent"
	"monopanel/internal/api"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

func newLogger(cfg config.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.Log.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "версия панели", Run: func(cmd *cobra.Command, _ []string) {
		if g.json {
			printJSON(map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit, "date": buildinfo.Date})
			return
		}
		fmt.Println("monopanel", buildinfo.String())
	}}
}

func apiCmd() *cobra.Command {
	return &cobra.Command{Use: "api", Short: "запустить API-сервер (monopanel-api.service)", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		logger := newLogger(cfg)
		slog.SetDefault(logger)
		profile, generic, err := osprofile.DetectOrGeneric()
		if err != nil {
			return err
		}
		if generic {
			logger.Warn("unsupported distribution; running with the generic profile (no package management)", "os", profile.Release().PrettyName)
		}
		ctx := cmd.Context()
		db, err := store.Open(ctx, cfg.DBPath())
		if err != nil {
			return err
		}
		defer db.Close()
		runner := jobs.NewRunner(db, cfg.Jobs.Workers, logger)
		srv := api.New(cfg, db, agent.NewClient(cfg.AgentSocket()), runner, profile, logger)
		runner.Start(ctx)
		logger.Info("monopanel api starting", "version", buildinfo.Version, "config", cfg.Path())
		err = srv.Run(ctx)
		runner.Wait()
		return err
	}}
}

func agentCmd() *cobra.Command {
	return &cobra.Command{Use: "agent", Short: "запустить привилегированный агент (monopanel-agent.service)", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		logger := newLogger(cfg)
		slog.SetDefault(logger)
		profile, generic, err := osprofile.DetectOrGeneric()
		if err != nil {
			return err
		}
		if generic {
			logger.Warn("unsupported distribution; running with the generic profile (no package management)", "os", profile.Release().PrettyName)
		}
		logger.Info("monopanel agent starting", "version", buildinfo.Version)
		return agent.NewServer(cfg, profile, logger).ListenAndServe(cmd.Context())
	}}
}

func helperCmd() *cobra.Command {
	var uid, gid int
	var groups string
	c := &cobra.Command{
		Use:    "helper --uid N --gid N [--groups a,b] -- команда [аргументы]",
		Short:  "выполнить команду от имени клиента, необратимо сбросив привилегии",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if uid <= 0 || gid <= 0 {
				return fmt.Errorf("helper refuses to run as uid/gid 0")
			}
			var gids []int
			if groups != "" {
				for _, name := range strings.Split(groups, ",") {
					grp, err := user.LookupGroup(strings.TrimSpace(name))
					if err != nil {
						return err
					}
					n, _ := strconv.Atoi(grp.Gid)
					gids = append(gids, n)
				}
			}
			if err := syscall.Setgroups(gids); err != nil {
				return fmt.Errorf("setgroups: %w", err)
			}
			if err := syscall.Setgid(gid); err != nil {
				return fmt.Errorf("setgid: %w", err)
			}
			if err := syscall.Setuid(uid); err != nil {
				return fmt.Errorf("setuid: %w", err)
			}
			if os.Getuid() != uid || os.Geteuid() != uid {
				return fmt.Errorf("privilege drop failed")
			}
			path, err := exec.LookPath(args[0])
			if err != nil {
				return err
			}
			env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
			if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
				env = append(env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
			}
			return syscall.Exec(path, args, env)
		},
	}
	c.Flags().IntVar(&uid, "uid", 0, "uid клиента")
	c.Flags().IntVar(&gid, "gid", 0, "gid клиента")
	c.Flags().StringVar(&groups, "groups", "", "дополнительные группы через запятую")
	return c
}
