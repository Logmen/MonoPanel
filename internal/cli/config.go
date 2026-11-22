package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/systemd"
	"monopanel/internal/updater"
)

// configKeys are the config.yaml settings mp config set changes: the same five
// PUT /system/config takes. The release key is not among them on purpose:
// only `mp update trust` as root on the server changes it.
var configKeys = config.PanelKeys()

// Who runs the command, how config set --local moves the socket of the
// panel's port and restarts monopanel-api without the API, and the pauses of
// waiting for it to come back. Tests replace them.
var (
	geteuid        = os.Geteuid
	syncAPISocket  = agent.SyncAPISocket
	restartAPIUnit = restartAPIWithSocket
	restartPoll    = 500 * time.Millisecond
	restartSettle  = 1500 * time.Millisecond
	restartTimeout = time.Minute
)

// restartAPIWithSocket restarts monopanel-api for config set --local. The
// socket that holds the panel's port goes along when it has to take the port
// anew: moved says that its address was written just now, and a socket that
// is not listening, or was started on an older address, is stale too. It is
// stopped first and comes back with the API, whose unit wants it, once the
// old API process — which still holds the port — is gone: started while the
// API runs, systemd would refuse it. Otherwise only the API restarts, and
// the socket keeps the port throughout.
func restartAPIWithSocket(ctx context.Context, cfg config.Config, moved bool) error {
	sd, err := systemd.Connect(ctx)
	if err != nil {
		return err
	}
	defer sd.Close()
	socket := cfg.APISocketUnit()
	if st, err := sd.Status(ctx, socket); err == nil && st.LoadState == "loaded" && (moved || agent.APISocketStale(st, fileTime(agent.APISocketDropIn(cfg)))) {
		if err := sd.Stop(ctx, socket); err != nil {
			return err
		}
	}
	return sd.Restart(ctx, cfg.Update.APIUnit)
}

// fileTime is when path was last written, zero when it is not there.
func fileTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: T("настройки панели (config.yaml) и шаблоны конфигураций", "panel settings (config.yaml) and configuration templates")}

	var local bool
	show := &cobra.Command{Use: "show", Short: T("настройки панели: имя, адрес HTTPS-порта, журнал, задачи", "panel settings: hostname, HTTPS address, logging, jobs"),
		Long: T(`Настройки панели из config.yaml: имя, адрес HTTPS-порта, журнал, число одновременных задач, каталоги
и ключ подписи релизов.

Без флагов — через API панели. --local читает config.yaml напрямую и показывает файл целиком: для
восстановления, когда API не запускается.`, `The panel's settings from config.yaml: its name, the HTTPS address, logging, how many jobs run at once,
the directories and the release signing key.

Without flags it asks the panel API. --local reads config.yaml directly and shows the whole file: for
recovery when the API does not start.`),
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if local {
				return configShowLocal()
			}
			cl, err := newClient()
			if err != nil {
				return err
			}
			pc, err := cl.PanelConfig(cmd.Context())
			if err != nil {
				return localHint(err, "mp config show --local")
			}
			if g.json {
				return printJSON(pc)
			}
			printPanelConfig(pc)
			return nil
		}}
	show.Flags().BoolVar(&local, "local", false, T("прочитать config.yaml напрямую, без API (на сервере; файл целиком)", "read config.yaml directly, without the API (on the server; the whole file)"))

	templates := &cobra.Command{Use: "templates", Short: T("встроенные шаблоны конфигураций и их переопределения", "the built-in configuration templates and their overrides"), Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Templates(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list.Templates))
		overridden := 0
		for _, t := range list.Templates {
			mark, file := "-", ""
			if t.Override {
				mark, file = T("да", "yes"), t.Path
				overridden++
			}
			rows = append(rows, []string{t.Name, mark, file})
		}
		table([]string{"TEMPLATE", "OVERRIDE", "FILE"}, rows)
		fmt.Printf(T("\nпереопределено %d из %d; свой шаблон — файл с тем же именем в %s\n", "\n%d of %d overridden; to override a template, put a file of the same name into %s\n"), overridden, len(list.Templates), list.Dir)
		return nil
	}}

	c.AddCommand(show, templates, configSetCmd())
	return c
}

func configSetCmd() *cobra.Command {
	var restart, local bool
	c := &cobra.Command{Use: T("set <ключ> <значение>", "set <key> <value>"), Short: T("изменить настройку панели в config.yaml", "change a panel setting in config.yaml"),
		Long: T(`Ключи: web.hostname (имя панели; пустая строка — панель открывается по IP), web.listen (адрес
HTTPS-порта, например :8443 или 203.0.113.10:8443), log.level (debug, info, warn, error), log.format
(text, json), jobs.workers (сколько задач выполняется одновременно, 1–64).

Изменение проходит через API панели и вступает в силу после перезапуска monopanel-api — --restart
перезапускает его сразу. --local пишет файл напрямую, без API: для восстановления, когда API не
запускается (например, после неудачного web.listen); только от root на сервере.`, `Keys: web.hostname (the panel's name; an empty string means the panel is reached by IP), web.listen
(the HTTPS address, e.g. :8443 or 203.0.113.10:8443), log.level (debug, info, warn, error), log.format
(text, json), jobs.workers (how many jobs run at once, 1–64).

The change goes through the panel API and takes effect once monopanel-api restarts; --restart restarts
it right away. --local writes the file directly, without the API: for recovery when the API does not
start (say, after a bad web.listen); as root on the server only.`),
		Args: cobra.ExactArgs(2), ValidArgs: configKeys, RunE: func(cmd *cobra.Command, args []string) error {
			if local {
				return configSetLocal(cmd.Context(), args[0], args[1], restart)
			}
			key, val := args[0], normalizeConfigValue(args[0], args[1])
			upd, err := panelConfigUpdate(key, val)
			if err != nil {
				return err
			}
			upd.Restart = restart
			cl, err := newClient()
			if err != nil {
				return err
			}
			res, err := cl.SetPanelConfig(cmd.Context(), upd)
			if err != nil {
				hint := fmt.Sprintf("mp config set %s %q --local", key, val)
				if restart {
					hint += " --restart"
				}
				return localHint(err, hint)
			}
			if g.json {
				if err := printJSON(res); err != nil {
					return err
				}
			} else {
				fmt.Printf("%s = %s → %s\n", key, configValue(&res.Config, key), res.Config.Path)
			}
			switch {
			case res.Restarted:
				return waitRestarted(cmd.Context(), cl)
			case res.Config.RestartPending && !g.json:
				fmt.Println(T("применится после перезапуска monopanel-api (--restart)", "takes effect once monopanel-api restarts (--restart)"))
				printRunning(&res.Config)
			}
			return nil
		}}
	c.Flags().BoolVar(&restart, "restart", false, T("перезапустить monopanel-api после изменения", "restart monopanel-api after the change"))
	c.Flags().BoolVar(&local, "local", false, T("записать config.yaml напрямую, без API (восстановление; root на сервере)", "write config.yaml directly, without the API (recovery; root on the server)"))
	return c
}

// normalizeConfigValue tidies what people type before it goes to the API,
// which checks and normalizes the value itself.
func normalizeConfigValue(key, val string) string {
	val = strings.TrimSpace(val)
	switch key {
	case "web.hostname":
		val = strings.ToLower(strings.TrimSuffix(val, "."))
	case "log.level", "log.format":
		val = strings.ToLower(val)
	}
	return val
}

func unknownConfigKey(key string) error {
	return &exitError{code: 2, msg: T("неизвестный ключ: ", "unknown key: ") + key + " (" + strings.Join(configKeys, ", ") + ")"}
}

// panelConfigUpdate is the PUT /system/config body for one key; the API checks
// the value.
func panelConfigUpdate(key, val string) (apitypes.PanelConfigUpdate, error) {
	var u apitypes.PanelConfigUpdate
	switch key {
	case "web.hostname":
		u.Hostname = &val
	case "web.listen":
		u.Listen = &val
	case "log.level":
		u.LogLevel = &val
	case "log.format":
		u.LogFormat = &val
	case "jobs.workers":
		n, err := strconv.Atoi(val)
		if err != nil {
			return u, &exitError{code: 2, msg: T("jobs.workers должен быть числом", "jobs.workers must be a number")}
		}
		u.JobsWorkers = &n
	default:
		return u, unknownConfigKey(key)
	}
	return u, nil
}

func configValue(pc *apitypes.PanelConfig, key string) string {
	switch key {
	case "web.hostname":
		return pc.Hostname
	case "web.listen":
		return pc.Listen
	case "log.level":
		return pc.LogLevel
	case "log.format":
		return pc.LogFormat
	case "jobs.workers":
		return strconv.Itoa(pc.JobsWorkers)
	}
	return ""
}

func printPanelConfig(pc *apitypes.PanelConfig) {
	fmt.Println(T("# файл:", "# file:"), pc.Path)
	host := pc.Hostname
	if host == "" {
		host = T("(не задано: панель открывается по IP)", "(not set: the panel is reached by IP)")
	}
	table([]string{"KEY", "VALUE"}, [][]string{{"web.hostname", host}, {"web.listen", pc.Listen}, {"log.level", pc.LogLevel}, {"log.format", pc.LogFormat}, {"jobs.workers", strconv.Itoa(pc.JobsWorkers)}})
	fmt.Printf(T("\nданные:   %s\nшаблоны:  %s (mp config templates)\n", "\ndata:      %s\ntemplates: %s (mp config templates)\n"), pc.DataDir, pc.TemplatesDir)
	if pc.KeyPinned {
		fmt.Println(T("релизы:   подпись проверяется (ключ меняет только root на сервере: mp update trust)", "releases:  signatures are checked (only root on the server changes the key: mp update trust)"))
	} else {
		fmt.Println(T("релизы:   подпись не проверяется (ключ задаёт только root на сервере: mp update trust)", "releases:  signatures are not checked (only root on the server sets the key: mp update trust)"))
	}
	if pc.RestartPending {
		fmt.Println(T("! файл изменён после запуска monopanel-api: изменения вступят в силу после перезапуска (mp config set … --restart)", "! the file changed after monopanel-api started: the changes take effect after a restart (mp config set … --restart)"))
	}
	printRunning(pc)
}

// printRunning says, while a new name or address waits for the restart, where
// the panel is still reached.
func printRunning(pc *apitypes.PanelConfig) {
	if running := runningAddress(pc); running != "" {
		fmt.Printf(T("  ждёт перезапуска: monopanel-api пока работает с %s\n", "  pending restart: monopanel-api still runs with %s\n"), running)
	}
}

// runningAddress is the name and address the running monopanel-api still
// has where config.yaml already says otherwise ("" when they agree): until the
// restart, the panel is reached there and not at the address in the file.
func runningAddress(pc *apitypes.PanelConfig) string {
	// An API from before running_listen leaves both empty; a running one
	// always listens somewhere.
	if pc.RunningListen == "" {
		return ""
	}
	var parts []string
	if pc.RunningHostname != pc.Hostname {
		host := pc.RunningHostname
		if host == "" {
			host = T("(не задано)", "(not set)")
		}
		parts = append(parts, "web.hostname "+host)
	}
	if pc.RunningListen != pc.Listen {
		parts = append(parts, "web.listen "+pc.RunningListen)
	}
	return strings.Join(parts, ", ")
}

// localHint names, for root on the server, the way around an API that does
// not answer: the --local form of the command. It never switches by itself:
// the API is the normal path, and its checks apply there.
func localHint(err error, cmdline string) error {
	var op *net.OpError
	if g.server == "" && geteuid() == 0 && errors.As(err, &op) {
		fmt.Fprintf(os.Stderr, T("API панели не отвечает; от root на сервере можно обойтись без него: %s\n", "the panel API does not answer; as root on the server you can do without it: %s\n"), cmdline)
	}
	return err
}

// waitRestarted follows monopanel-api through the restart config set asked
// for, so the next command finds the panel answering. The old process answers
// until systemd stops it: wait for it to go, then for the new one. Only over
// the socket: a remote address may change with web.listen or web.hostname.
func waitRestarted(ctx context.Context, cl *client.Client) error {
	if g.server != "" || g.noWait {
		if !g.json {
			fmt.Println(T("monopanel-api перезапускается; через несколько секунд панель снова ответит (по новому адресу, если он изменился)", "monopanel-api is restarting; the panel answers again in a few seconds (at the new address if it changed)"))
		}
		return nil
	}
	if !g.json {
		fmt.Fprintln(os.Stderr, T("monopanel-api перезапускается…", "monopanel-api is restarting…"))
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && apiUp(ctx, cl); {
		if err := pause(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	if err := waitUp(ctx, cl); err != nil {
		return err
	}
	if !g.json {
		fmt.Println(T("monopanel-api перезапущен", "monopanel-api restarted"))
	}
	return nil
}

// waitUp waits up to a minute for monopanel-api to answer, and to go on
// answering a moment later: a process that cannot take its address exits
// right after it starts, and systemd starts it again and again.
func waitUp(ctx context.Context, cl *client.Client) error {
	for end := time.Now().Add(restartTimeout); ; {
		if apiUp(ctx, cl) {
			if err := pause(ctx, restartSettle); err != nil {
				return err
			}
			if apiUp(ctx, cl) {
				return nil
			}
		}
		if time.Now().After(end) {
			return &exitError{code: 1, msg: T("monopanel-api не ответил в течение минуты после перезапуска: journalctl -u monopanel-api", "monopanel-api did not answer within a minute of the restart: journalctl -u monopanel-api")}
		}
		if err := pause(ctx, restartPoll); err != nil {
			return err
		}
	}
}

func apiUp(ctx context.Context, cl *client.Client) bool {
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := cl.Health(hctx)
	return err == nil
}

func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// configShowLocal prints the whole effective config.yaml without the API.
func configShowLocal() error {
	cfg, err := loadConfig()
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return &exitError{code: 4, msg: T("--local читает config.yaml напрямую: нужен root на сервере", "--local reads config.yaml directly: run it as root on the server")}
		}
		return err
	}
	if g.json {
		return printJSON(cfg)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	fmt.Println(T("# файл:", "# file:"), cfg.Path())
	fmt.Print(string(b))
	return nil
}

// configSetLocal writes config.yaml directly: the recovery path for when the
// API cannot start. Nothing else checks the value here, and a bad one would
// keep the panel down, so it is checked by the rules of PUT /system/config.
func configSetLocal(ctx context.Context, key, raw string, restart bool) error {
	if geteuid() != 0 {
		return &exitError{code: 4, msg: T("--local пишет config.yaml напрямую: только от root на сервере", "--local writes config.yaml directly: run it as root on the server")}
	}
	val, err := panelValue(key, raw)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Only this key changes in the file: comments, keys this version does not
	// know, "audit_file: none" and the release key stay as they are, and so do
	// the file's owner and mode (root:<service group> 0640, so that the API
	// can read it).
	got, err := config.WriteFileKeys(cfg.Path(), map[string]string{key: val}, serviceGID(cfg))
	if err != nil {
		return err
	}
	changed := len(got) > 0
	if changed {
		if err := cfg.SetPanelValue(key, val); err != nil {
			return err
		}
		restoreconFile(ctx, cfg.Path())
	}
	if !g.json {
		if changed {
			fmt.Printf("%s = %s → %s\n", key, val, cfg.Path())
		} else {
			fmt.Printf(T("%s = %s: в %s уже так\n", "%s = %s: %s already says so\n"), key, val, cfg.Path())
		}
	}
	// The socket that holds the panel's port follows web.listen — also when
	// the file said so already: that is how a socket that missed a change by
	// hand gets there.
	moved := false
	if key == config.KeyListen {
		if moved, err = syncAPISocket(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, T("внимание: %s не перешёл на web.listen (%v): monopanel-api будет слушать адрес сам, и пока он перезапускается, порт свободен; mp setup настроит сокет заново\n",
				"warning: %s did not follow web.listen (%v): monopanel-api will listen on the address itself, and the port is free while it restarts; mp setup sets the socket up again\n"), cfg.APISocketUnit(), err)
		}
	}
	if changed && key == "web.listen" {
		listenNotes(val)
	}
	if restart {
		if err := restartAPIUnit(ctx, cfg, moved); err != nil {
			// After a run of failed starts systemd refuses to start the unit
			// for a while, which is exactly when this path is used.
			return fmt.Errorf(T("%w (если юнит исчерпал лимит запусков: systemctl reset-failed %s и повторите)", "%w (if the unit has used up its start limit: systemctl reset-failed %s, then try again)"), err, cfg.Update.APIUnit)
		}
		// systemd is done once the process is started; whether it takes the
		// new address shows only when it answers.
		if !g.noWait {
			if !g.json {
				fmt.Fprintln(os.Stderr, T("monopanel-api перезапускается…", "monopanel-api is restarting…"))
			}
			if err := waitUp(ctx, client.NewUnix(cfg.APISocket())); err != nil {
				return err
			}
		}
	}
	if g.json {
		return printJSON(apitypes.PanelConfigResult{Config: panelConfigOf(cfg, changed && !restart), Restarted: restart})
	}
	switch {
	case restart:
		fmt.Println(T("monopanel-api перезапущен", "monopanel-api restarted"))
	case moved:
		// A plain restart of the API would leave the socket where it was.
		fmt.Printf(T("применится после перезапуска вместе с сокетом порта: systemctl stop %s && systemctl restart monopanel-api (или --restart)\n",
			"takes effect after a restart along with the port's socket: systemctl stop %s && systemctl restart monopanel-api (or --restart)\n"), cfg.APISocketUnit())
	case changed:
		fmt.Println(T("применится после перезапуска: systemctl restart monopanel-api (или --restart)", "takes effect after a restart: systemctl restart monopanel-api (or --restart)"))
	}
	return nil
}

// apiSocketSyncCmd gives the socket that holds the panel's port the address
// of web.listen in config.yaml, for the package's scripts after an install
// or an upgrade: mp setup and config set --local do the same themselves. It
// moves no socket that is listening already; that takes a restart of the
// socket while no API process holds the port.
func apiSocketSyncCmd() *cobra.Command {
	return &cobra.Command{Use: "api-socket-sync", Short: T("адрес сокета HTTPS-порта панели из web.listen (для скриптов пакета)", "the address of the panel's HTTPS port socket from web.listen (for the package scripts)"), Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return &exitError{code: 4, msg: T("api-socket-sync выполняется только от root", "api-socket-sync must run as root")}
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			moved, err := syncAPISocket(cmd.Context(), cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", cfg.APISocketUnit(), err)
			}
			if moved && !g.json {
				fmt.Printf(T("%s: адрес из web.listen (%s)\n", "%s: the address of web.listen (%s)\n"), cfg.APISocketUnit(), cfg.Web.Listen)
			}
			return nil
		}}
}

// listenNotes says what config set --local does not do for a new web.listen,
// unlike the API: check that the unprivileged panel may take the port, and
// let the port through the firewall.
func listenNotes(listen string) {
	_, p, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(p)
	start := 1024
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			start = n
		}
	}
	if port < start {
		fmt.Fprintf(os.Stderr, T("внимание: monopanel-api работает без привилегий и не сможет слушать порт %d (меньше %d), если его юнит не даёт CAP_NET_BIND_SERVICE\n", "warning: monopanel-api runs without privileges and cannot listen on port %d (below %d) unless its unit grants CAP_NET_BIND_SERVICE\n"), port, start)
	}
	if ip := agent.ForeignListenIP(listen); ip != nil {
		fmt.Fprintf(os.Stderr, T("внимание: %s — не адрес этого сервера; сокет порта панели всё равно займёт его, и панель будет слушать там, где её никто не достанет\n", "warning: %s is not an address of this server; the socket of the panel's port takes it all the same, and the panel would listen where nobody reaches it\n"), ip)
	}
	fmt.Fprintln(os.Stderr, T("--local не трогает firewall: если он включён, после запуска панели выполните mp firewall apply — его правила откроют новый порт панели (firewalld: firewall-cmd --add-port)",
		"--local leaves the firewall alone: if it is on, run mp firewall apply once the panel is up, and its rules open the panel's new port (firewalld: firewall-cmd --add-port)"))
}

// panelValue checks a value for config set --local and returns it the way
// config.yaml keeps it, by the rules PUT /system/config applies
// (config.NormalizePanelValue): a host name in lower case without the final
// dot, an address with the IP written canonically, a log level in lower case.
// Only the form: whether this host can listen there shows when monopanel-api
// starts.
func panelValue(key, value string) (string, error) {
	if !config.IsPanelKey(key) {
		return "", unknownConfigKey(key)
	}
	v, err := config.NormalizePanelValue(key, value)
	if err != nil {
		return "", &exitError{code: 2, msg: key + ": " + panelHint(key) + " (" + err.Error() + ")"}
	}
	return v, nil
}

// panelHint says what a panel key takes.
func panelHint(key string) string {
	switch key {
	case config.KeyHostname:
		return T("нужно DNS-имя, например panel.example.com (не IP-адрес), или пустая строка — тогда панель открывается по IP", "needs a DNS name such as panel.example.com (not an IP address), or an empty string to reach the panel by IP")
	case config.KeyListen:
		return T("нужно хост:порт или :порт с портом от 1 до 65535, например :8443 или 203.0.113.10:8443; хост — IP-адрес этого сервера или его DNS-имя", "needs host:port or :port with a port from 1 to 65535, e.g. :8443 or 203.0.113.10:8443; the host is an IP address of this server or its DNS name")
	case config.KeyLogLevel:
		return T("debug, info, warn или error", "debug, info, warn or error")
	case config.KeyLogFormat:
		return T("text или json", "text or json")
	case config.KeyJobsWorkers:
		return T("число от 1 до 64", "a number from 1 to 64")
	}
	return ""
}

// serviceGID is the group a new config.yaml gets (-1 when it is missing).
func serviceGID(cfg config.Config) int {
	g, err := user.LookupGroup(cfg.ServiceGroup)
	if err != nil {
		return -1
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return -1
	}
	return gid
}

// restoreconFile gives a file replaced by rename the SELinux label of its
// place (no-op without SELinux).
func restoreconFile(ctx context.Context, path string) {
	const bin = "/usr/sbin/restorecon"
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		return
	}
	if _, err := os.Stat(bin); err != nil {
		return
	}
	_ = exec.CommandContext(ctx, bin, "-F", path).Run()
}

// panelConfigOf is what GET /system/config would say about a file read
// locally.
func panelConfigOf(cfg config.Config, restartPending bool) apitypes.PanelConfig {
	_, source := updater.ReleaseKey(cfg.Update.PublicKey)
	return apitypes.PanelConfig{Path: cfg.Path(), Hostname: cfg.Web.Hostname, Listen: cfg.Web.Listen, LogLevel: cfg.Log.Level, LogFormat: cfg.Log.Format, JobsWorkers: cfg.Jobs.Workers,
		DataDir: cfg.DataDir, TemplatesDir: cfg.TemplatesDir, KeyPinned: source != "", RestartPending: restartPending}
}
