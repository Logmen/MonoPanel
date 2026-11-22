package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// The web console runs the same mp binary an administrator would run over
// ssh, as a client of this API: a one-off token carries the caller's own
// rights and dies with the command, the output streams back as it comes.

// consoleBinary is the mp binary to run — the panel itself.
var consoleBinary = func() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "/usr/bin/monopanel"
}

// consoleForbidden are the commands that make no sense from a browser: the
// daemons (the mail policy's milter among them, and EL's mail guard, a unit's
// root command), the privilege-dropping helper, the installer, the demo build's
// server (mp demo; the demo's own guard looks only at the first argument),
// and app-exec and sandbox-exec, which exec any program (as the panel's
// service account, from the console).
var consoleForbidden = map[string]bool{"api": true, "agent": true, "helper": true, "fsop": true, "setup": true, "update-run": true, "demo": true, "app-exec": true, "sandbox-exec": true, "mail-policy": true, "mail-guard": true}

// consoleRootOnly are the commands for root on the server alone. mp update
// trust sets the key every release is checked with before the agent installs
// it as root; the console runs mp as the panel's own service account, which
// must never change that key — not even where config.yaml's permissions were
// loosened by hand.
var consoleRootOnly = map[string]bool{"update trust": true}

// consoleClientFlags are the console's own: it points the command at this
// panel with the caller's one-off token. Typed, they would send it to another
// panel, with another configuration or as someone else.
var consoleClientFlags = []string{"--server", "--token", "--config", "--insecure"}

// consoleOwnFlags are, per command, the client flags that are flags of the
// command itself, not the panel's: the token a source panel issued and
// whether to check the source's certificate (migrate), the repository's token
// (update settings). The console's credentials travel in the environment, so
// such flags are free for what the command means by them. --server and
// --config never are.
var consoleOwnFlags = map[string][]string{
	"migrate plan":    {"--token", "--insecure"},
	"migrate run":     {"--token", "--insecure"},
	"update settings": {"--token"},
}

// consoleLocalFiles are the commands (the whole command when no flags are
// listed) and flags that read or write files on the machine mp runs on. In
// the console that machine is the server, and the files are read and written
// as the panel's own service account: its database, its secret key. The web
// interface takes the same content through its own forms — the file manager's
// upload and download, the certificate import, the site's nginx editor, the
// key field of the migration page.
var consoleLocalFiles = map[string][]string{
	"files get":    nil,
	"files put":    nil,
	"ssl import":   nil,
	"ssl panel":    {"--cert", "--key"}, // ssl panel import
	"site nginx":   {"--set"},
	"migrate plan": {"--key"},
	"migrate run":  {"--key"},
}

// consoleSwitches are mp's global flags that take no value. When cobra looks
// for the command to run, any other flag written without "=" takes the next
// argument as its value — --help and --version too: cobra adds them only
// once it has found the command.
var consoleSwitches = map[string]bool{"--json": true, "--no-wait": true, "--insecure": true}

// consoleSecretFlags carry passwords, tokens and credentials. The audit log
// keeps the command line without their values: every administrator reads the
// log, and the audit file may be shipped off the server. --cred and --env are
// KEY=VALUE lists, their keys stay.
var consoleSecretFlags = map[string]bool{"--password": true, "--current-password": true, "--admin-password": true, "--token": true, "--secret": true, "--cred": true, "--env": true}

// consoleStdinTwins are, per command, its secret flags and the -stdin twins
// that read the same value from stdin. The console runs mp with a command
// line every local user can read in ps for as long as the command runs (up
// to half an hour), so a secret typed with the plain flag goes to the
// command's stdin instead. --cred and --env are KEY=VALUE lists: their lines
// follow the one secret value, the order the commands read them in.
var consoleStdinTwins = map[string]map[string]string{
	"app add":           {"--env": "--env-stdin"},
	"app set":           {"--env": "--env-stdin"},
	"backup target add": {"--password": "--password-stdin", "--env": "--env-stdin"},
	"cms install":       {"--admin-password": "--password-stdin"},
	"db create":         {"--password": "--password-stdin"},
	"db passwd":         {"--password": "--password-stdin"},
	"dns-provider add":  {"--cred": "--cred-stdin"},
	"mail box add":      {"--password": "--password-stdin"},
	"mail box set":      {"--password": "--password-stdin"},
	"migrate plan":      {"--token": "--token-stdin"},
	"migrate run":       {"--token": "--token-stdin"},
	"update settings":   {"--token": "--token-stdin"},
	"user add":          {"--password": "--password-stdin"},
	"user set":          {"--password": "--password-stdin", "--current-password": "--current-password-stdin"},
	"webhook add":       {"--secret": "--secret-stdin"},
}

// consoleStdinOrder are the commands that read more than one one-line
// secret from stdin, and in which order (mp user set: the current password,
// then the new one).
var consoleStdinOrder = map[string][]string{
	"user set": {"--current-password-stdin", "--password-stdin"},
}

// consoleStdin moves the secrets typed for a command to its stdin: args
// without them plus the -stdin twins, and what to write to stdin. An empty
// value asks for nothing and is dropped. The console has no stdin of its
// own, so a -stdin flag typed by hand is refused with what to type instead.
func consoleStdin(args []string) ([]string, string, error) {
	path := consolePath(args, 3)
	twins := consoleStdinTwins[path]
	if twins == nil {
		twins = consoleStdinTwins[consolePath(args, 2)]
	}
	singles := map[string]string{} // twin → value
	var listTwin string
	var list []string
	out := make([]string, 0, len(args)+2)
	rest := []string(nil)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = args[i:]
			break
		}
		flag, value, hasValue := strings.Cut(a, "=")
		if strings.HasPrefix(flag, "--") && strings.HasSuffix(flag, "-stdin") {
			plain := strings.TrimSuffix(flag, "-stdin")
			for p, t := range twins {
				if t == flag {
					plain = p
				}
			}
			return nil, "", huma.Error422UnprocessableEntity(flag + " has nothing to read in the console: type the value with " + plain + " — the console passes it to the command on stdin, not on its command line")
		}
		twin, ok := twins[flag]
		if !ok {
			out = append(out, a)
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				out = append(out, a) // mp itself says the value is missing
				continue
			}
			i++
			value = args[i]
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, "", huma.Error422UnprocessableEntity(flag + " takes a single line")
		}
		if value == "" {
			continue
		}
		if flag == "--cred" || flag == "--env" {
			list, listTwin = append(list, value), twin
			continue
		}
		if prev, ok := singles[twin]; ok && prev != value {
			return nil, "", huma.Error422UnprocessableEntity(flag + " is given twice with different values")
		}
		singles[twin] = value
	}
	order := consoleStdinOrder[path]
	if order == nil {
		order = consoleStdinOrder[consolePath(args, 2)]
	}
	if len(singles) > 1 && order == nil {
		return nil, "", huma.Error422UnprocessableEntity("one secret per command in the console: they would share the command's stdin")
	}
	if len(singles) == 1 {
		for twin := range singles {
			order = []string{twin}
		}
	}
	var stdin strings.Builder
	for _, twin := range order {
		if v, ok := singles[twin]; ok {
			out = append(out, twin)
			stdin.WriteString(v + "\n")
		}
	}
	if listTwin != "" {
		out = append(out, listTwin)
		for _, l := range list {
			stdin.WriteString(l + "\n")
		}
	}
	return append(out, rest...), stdin.String(), nil
}

// consoleAuditLine is the command line as the audit log records it.
func consoleAuditLine(args []string) string {
	hide := func(flag, value string) string {
		if k, _, ok := strings.Cut(value, "="); ok && (flag == "--cred" || flag == "--env") {
			return k + "=[redacted]"
		}
		return "[redacted]"
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if flag, value, ok := strings.Cut(a, "="); ok && consoleSecretFlags[flag] {
			out = append(out, flag+"="+hide(flag, value))
			continue
		}
		out = append(out, a)
		if consoleSecretFlags[a] && i+1 < len(args) {
			i++
			out = append(out, hide(a, args[i]))
		}
	}
	return strings.Join(out, " ")
}

// consoleCommand is the command path the arguments name ("migrate plan"): the
// first two words, read the way cobra finds the command it runs. A flag
// before them that is not one of mp's switches takes the next argument as its
// value, so "--cwd . fsop run" is fsop run, not ". fsop"; "" and "-" are
// neither commands nor flags; "--" ends the search. Read any other way, a
// flag in front would let a refused command through, or another command's
// own flags.
func consoleCommand(args []string) string { return consolePath(args, 2) }

// consolePath is consoleCommand with up to n words: "mail box add".
func consolePath(args []string, n int) string {
	var words []string
scan:
	for i := 0; i < len(args) && len(words) < n; i++ {
		switch a := args[i]; {
		case a == "--":
			break scan // the rest are arguments
		case a == "" || a == "-":
			// neither a command nor a flag
		case strings.HasPrefix(a, "--") || (len(a) == 2 && a[0] == '-'):
			if !strings.Contains(a, "=") && !consoleSwitches[a] {
				i++ // its value
			}
		case strings.HasPrefix(a, "-"):
			// -n5 or -xy: a short flag with its value, or switches run together
		default:
			words = append(words, a)
		}
	}
	return strings.Join(words, " ")
}

type consoleInput struct {
	Body apitypes.ConsoleRequest
}

// consoleServerURL is where the command reaches this API: the loopback side
// of the HTTPS listener, or the one address it listens on (web.listen may
// name a single address, and then nothing answers on 127.0.0.1).
func (s *Server) consoleServerURL() string {
	// The address the listener is bound to: the console's one-off token
	// then goes to this very process. A host name in web.listen would be
	// looked up again, and the name may lead elsewhere — an address of this
	// host the API does not listen on, where a local account could listen
	// on the same port with any certificate (the console does not check it).
	if a, ok := s.listenAddr.Load().(net.Addr); ok {
		if tcp, ok := a.(*net.TCPAddr); ok {
			ip := tcp.IP
			if ip == nil || ip.IsUnspecified() {
				// Go binds a wildcard listener dual-stack (IPV6_V6ONLY off),
				// and so does monopanel-api.socket (BindIPv6Only=both), so
				// 127.0.0.1 is this socket too — also where ::1 is not
				// configured (IPv6 switched off by sysctl). A socket systemd
				// made for IPv6 alone leaves 127.0.0.1 on the port to whoever
				// binds it: ::1 is this socket then.
				ip = net.IPv4(127, 0, 0, 1)
				if s.listenV6Only.Load() {
					ip = net.IPv6loopback
				}
			}
			return "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port))
		}
	}
	listen := s.cfg.Web.Listen
	if listen == "" {
		listen = ":8443"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		host, port = "", "8443"
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}

func (s *Server) registerConsole() {
	huma.Register(s.api, huma.Operation{
		OperationID: "system-console", Method: http.MethodPost, Path: "/system/console", Summary: "Run an mp command with the caller's rights and stream its output", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *consoleInput) (*huma.StreamResponse, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return nil, huma.Error422UnprocessableEntity("the console runs on behalf of a panel account")
		}
		args := in.Body.Args
		if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
			return nil, huma.Error422UnprocessableEntity("a command is required, e.g. doctor")
		}
		// The command, not the first argument: "mp --json agent" is the agent.
		command := consoleCommand(args)
		if first, _, _ := strings.Cut(command, " "); consoleForbidden[first] {
			return nil, huma.Error422UnprocessableEntity("mp " + first + " is not for the console")
		}
		if consoleRootOnly[command] {
			return nil, huma.Error422UnprocessableEntity("mp " + command + " is run by root on the server only, never by the panel's own service account")
		}
		local, hasLocal := consoleLocalFiles[command]
		if hasLocal && len(local) == 0 {
			return nil, huma.Error422UnprocessableEntity("mp " + command + " works with files on the server, which the console would read and write as the panel's own service account; use the web interface for it")
		}
		own := consoleOwnFlags[command]
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return nil, huma.Error422UnprocessableEntity("invalid argument")
			}
			for _, flag := range consoleClientFlags {
				if slices.Contains(own, flag) {
					continue
				}
				if a == flag || strings.HasPrefix(a, flag+"=") {
					return nil, huma.Error422UnprocessableEntity(flag + " is set by the console itself")
				}
			}
			for _, flag := range local {
				if a == flag || strings.HasPrefix(a, flag+"=") {
					return nil, huma.Error422UnprocessableEntity("mp " + command + " " + flag + " reads a file on the server, which the console would read as the panel's own service account; use the web interface for it")
				}
			}
		}
		// Secrets typed on the command line go to the command's stdin.
		runArgs, stdin, err := consoleStdin(args)
		if err != nil {
			return nil, err
		}
		// a token for this one command, gone when it ends
		plain, err := auth.NewToken(32)
		if err != nil {
			return nil, err
		}
		exp := time.Now().Add(30 * time.Minute)
		rec := &store.APIToken{UserID: p.UserID, Name: "console " + time.Now().UTC().Format("15:04:05"), Hash: auth.HashToken(plain), ExpiresAt: &exp}
		if err := s.db.CreateAPIToken(ctx, rec); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "console.run", Target: consoleAuditLine(args), IP: requestInfo(ctx).IP})
		server := s.consoleServerURL()
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer s.db.DeleteAPIToken(context.Background(), rec.ID, p.UserID) //nolint:errcheck // it expires on its own anyway
			hctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Cache-Control", "no-store")
			out := &flushWriter{w: hctx.BodyWriter()}
			cctx, cancel := context.WithTimeout(hctx.Context(), 30*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(cctx, consoleBinary(), runArgs...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			// mp speaks the language of the interface that asked, not of the
			// server's locale.
			mpLang := "en"
			if in.Body.Lang == "ru" {
				mpLang = "ru"
			}
			// The panel's address and the token go in the environment, where
			// mp takes the defaults of --server, --token and --insecure from:
			// on the command line they would shadow the commands' own
			// --token and --insecure (mp migrate plan|run, mp update settings),
			// and anyone on the host could read the token in ps.
			cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "MP_LANG=" + mpLang, "NO_COLOR=1", "TERM=dumb", "HOME=" + s.cfg.DataDir,
				"MP_SERVER=" + server, "MP_TOKEN=" + plain, "MP_INSECURE=1"}
			cmd.Stdout, cmd.Stderr = out, out
			cmd.WaitDelay = 5 * time.Second
			// The echo hides the secrets as the audit log does: the output
			// stays on screen, may be copied and shared.
			fmt.Fprintf(out, "$ mp %s\n", consoleAuditLine(args))
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				fmt.Fprintf(out, "console: %v\n", err)
				code = 1
			}
			fmt.Fprintf(out, "\n[exit %d]\n", code)
		}}, nil
	})
}

// flushWriter pushes every write to the browser: the command's progress
// lines are the point of a console.
type flushWriter struct {
	w io.Writer
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
