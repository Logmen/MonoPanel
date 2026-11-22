package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	return &cobra.Command{Use: "version", Short: T("версия панели", "panel version"), Run: func(cmd *cobra.Command, _ []string) {
		if g.json {
			printJSON(map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit, "date": buildinfo.Date}) //nolint:errcheck // best effort; the caller reports the real failure
			return
		}
		fmt.Println("monopanel", buildinfo.String())
	}}
}

// failedStartPause is how long a failed start of the API that
// monopanel-api.socket made lingers before it exits; RestartSec of its unit.
var failedStartPause = 2 * time.Second

// startedBySocket tells whether systemd passed this process listening
// sockets: monopanel-api.socket started it (sd_listen_fds(3)). It is asked
// before Listen takes them, which unsets the variables.
func startedBySocket() bool {
	return os.Getenv("LISTEN_FDS") != "" && os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid())
}

// lingerAfterFailure keeps a failed start of the API from exiting at once
// when monopanel-api.socket made it. A connection waiting on the socket
// starts the API again as soon as it is gone, past RestartSec — on every
// version when the unit does not restart by itself (seen on systemd 262),
// and, by the source of systemd before 254, even while it waits to restart
// — and a start that fails in a moment would run twenty times in two
// seconds: the socket then gives up (trigger-limit-hit) and closes the
// panel's port, which is anybody's while the API keeps failing. Paced like
// this, a failing API comes back no faster than its unit restarts it.
func lingerAfterFailure(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(failedStartPause):
	}
}

func apiCmd() *cobra.Command {
	return &cobra.Command{Use: "api", Short: T("запустить API-сервер (monopanel-api.service)", "run the API server (monopanel-api.service)"), Hidden: true, RunE: func(cmd *cobra.Command, _ []string) (err error) {
		if startedBySocket() {
			defer func() {
				if err != nil && cmd.Context().Err() == nil {
					lingerAfterFailure(cmd.Context())
				}
			}()
		}
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
		defer db.Close() //nolint:errcheck // cleanup
		runner := jobs.NewRunner(db, cfg.Jobs.Workers, logger)
		srv := api.New(cfg, db, agent.NewClient(cfg.AgentSocket()), runner, profile, logger)
		srv.KeepPortUntilExit()
		logger.Info("monopanel api starting", "version", buildinfo.Version, "config", cfg.Path())
		// The listeners first: a start that cannot have them changes nothing
		// on the host and returns, so that systemd starts it again
		// (Restart=, lingerAfterFailure) and the journal says why.
		ln, err := srv.Listen(ctx)
		if err != nil {
			logger.Error("monopanel api cannot start, and exits having changed nothing", "err", err)
			return err
		}
		// The workers end with the servers: a Serve that failed must not
		// leave them running while nothing serves the panel.
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		runner.Start(rctx)
		err = srv.Serve(rctx, ln)
		if err != nil && ctx.Err() == nil {
			logger.Error("monopanel api stopped", "err", err)
		}
		cancel()
		runner.Wait()
		return err
	}}
}

func agentCmd() *cobra.Command {
	return &cobra.Command{Use: "agent", Short: T("запустить привилегированный агент (monopanel-agent.service)", "run the privileged agent (monopanel-agent.service)"), Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
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

// cleanPrivateTmp empties a unit's own /tmp and /var/tmp; tests replace it.
var cleanPrivateTmp = agent.CleanPrivateTmp

// cleanPrivateTmpWait is how long clean-private-tmp may take
// (agent.PrivateTmpCleanWait); tests shorten it.
var cleanPrivateTmpWait = agent.PrivateTmpCleanWait

// cleanPrivateTmpCmd is the ExecStopPost of the units whose processes run
// package scripts — the agent's, and each run of the package manager the
// agent starts (monopanel-pkg-<run>.service): it empties the unit's own /tmp
// and /var/tmp (PrivateTmp=), and nothing else, before systemd removes them,
// which on EL with SELinux it may not do with what those scripts leave there
// (agent.CleanPrivateTmp). It prints nothing unless something stays; the
// unit ignores its failure. It gives up after cleanPrivateTmpWait, leaving
// the rest to systemd: one still going at the unit's TimeoutStopSec would
// fail the unit, its "-" notwithstanding. Every later binary keeps it: a run
// a release started that ends after the upgrade to the next one has the new
// binary as its ExecStopPost, and so has the agent's unit.
func cleanPrivateTmpCmd() *cobra.Command {
	return &cobra.Command{Use: "clean-private-tmp <unit>", Short: T("служебная: очистить собственные /tmp и /var/tmp юнита, пока их не удалил systemd (ExecStopPost)", "internal: empty a unit's own /tmp and /var/tmp before systemd removes them (ExecStopPost)"), Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if geteuid() != 0 {
				return &exitError{code: 4, msg: T("clean-private-tmp выполняется только от root", "clean-private-tmp must run as root")}
			}
			clean, unit, done := cleanPrivateTmp, args[0], make(chan error, 1)
			go func() { done <- clean(unit) }()
			select {
			case err := <-done:
				return err
			case <-time.After(cleanPrivateTmpWait):
				// the process ends here, the removal under way with it
				return &exitError{code: 1, msg: fmt.Sprintf(T("clean-private-tmp: не уложилась в %s, остаток /tmp и /var/tmp юнита %s удалит systemd", "clean-private-tmp: not done within %s, systemd removes what is left of %s's /tmp and /var/tmp"), cleanPrivateTmpWait, args[0])}
			}
		}}
}

func helperCmd() *cobra.Command {
	var uid, gid int
	var groups string
	c := &cobra.Command{
		Use:    T("helper --uid N --gid N [--groups a,b] -- команда [аргументы]", "helper --uid N --gid N [--groups a,b] -- command [args]"),
		Short:  T("выполнить команду от имени клиента, необратимо сбросив привилегии", "run a command as a client, dropping privileges irreversibly"),
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
	c.Flags().IntVar(&uid, "uid", 0, T("uid клиента", "client's uid"))
	c.Flags().IntVar(&gid, "gid", 0, T("gid клиента", "client's gid"))
	c.Flags().StringVar(&groups, "groups", "", T("дополнительные группы через запятую", "comma-separated supplementary groups"))
	return c
}
