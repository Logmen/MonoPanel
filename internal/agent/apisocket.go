package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"monopanel/internal/config"
	"monopanel/internal/systemd"
)

// The panel's HTTPS port belongs to PID 1: monopanel-api.socket binds it and
// hands it to monopanel-api. Whenever the API is not running — while it
// restarts after a change of config.yaml, an update or a crash, while it is
// stopped, at boot before its unit starts — the port is still bound, and no
// local account (a client with ssh, a site's PHP) can take it and pose as
// the panel to collect the administrators' passwords and one-time codes: a
// port from 1024 up needs no privilege to bind.
//
// The socket's address follows web.listen through a drop-in only root
// writes, from config.yaml and nothing else: the agent when web.listen
// changes through it, mp setup, mp config set --local and the package's
// scripts (monopanel api-socket-sync). The API cannot write /etc
// (ProtectSystem=strict), and no RPC takes the content from it: the units
// of /v1/units/* know no socket.

// The drop-ins, below /etc/systemd/system/<unit>.d/.
const (
	// apiSocketDropIn gives the socket the address of web.listen.
	apiSocketDropIn = "listen.conf"
	// apiServiceDropIn ties the API's unit to the socket.
	apiServiceDropIn = "socket.conf"
)

// LookupIP resolves a host name (net.Resolver.LookupIPAddr).
type LookupIP func(ctx context.Context, host string) ([]net.IPAddr, error)

// APISocketAddr is where monopanel-api.socket listens.
type APISocketAddr struct {
	// IP is the one address to listen on; nil means every address of the
	// host.
	IP   net.IP
	Port int
}

// ListenStream is the address in the syntax of ListenStream=
// (systemd.socket(5)): the port alone for every address.
func (a APISocketAddr) ListenStream() string {
	if a.IP == nil {
		return strconv.Itoa(a.Port)
	}
	return net.JoinHostPort(a.IP.String(), strconv.Itoa(a.Port))
}

// Holds tells whether addr, the address of a listener, is this one: the same
// port, and on every address (the IPv6 wildcard, or IPv4's where systemd
// found no IPv6) or on this very address.
func (a APISocketAddr) Holds(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.Port != a.Port {
		return false
	}
	if a.IP == nil {
		return tcp.IP == nil || tcp.IP.IsUnspecified()
	}
	return a.IP.Equal(tcp.IP)
}

// ListenIP is the address a listener on a host name takes, as net.Listen
// chooses it: the first IPv4 address of the name, else its first one.
func ListenIP(found []net.IPAddr) net.IP {
	if len(found) == 0 {
		return nil
	}
	for _, a := range found {
		if a.IP.To4() != nil {
			return a.IP
		}
	}
	return found[0].IP
}

// ResolveAPISocket is where monopanel-api.socket listens for listen, a
// web.listen checked by the rules of config.NormalizePanelValue:
//
//	:8443, 0.0.0.0:8443, [::]:8443  8443, every address: net.Listen binds all
//	                                three on the IPv6 wildcard with IPv4 in it,
//	                                as BindIPv6Only=both of the socket does
//	203.0.113.5:8443                203.0.113.5:8443
//	[2001:db8::5]:8443              [2001:db8::5]:8443
//	panel.example.com:8443          the address net.Listen would take (ListenIP)
//
// Anything else is an error. lookup nil is the system's resolver.
func ResolveAPISocket(ctx context.Context, listen string, lookup LookupIP) (APISocketAddr, error) {
	v, err := config.NormalizePanelValue(config.KeyListen, listen)
	if err != nil {
		return APISocketAddr{}, err
	}
	host, p, err := net.SplitHostPort(v)
	if err != nil {
		return APISocketAddr{}, err
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return APISocketAddr{}, err
	}
	ip := net.ParseIP(host)
	if host != "" && ip == nil {
		if lookup == nil {
			lookup = net.DefaultResolver.LookupIPAddr
		}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		found, err := lookup(lctx, host)
		cancel()
		if ip = ListenIP(found); err != nil || ip == nil {
			why := "no address"
			if err != nil {
				why = err.Error()
			}
			return APISocketAddr{}, fmt.Errorf("web.listen: %s does not resolve on this server (%s); give an IP address of the server instead", host, why)
		}
	}
	a := APISocketAddr{Port: port}
	if ip != nil && !ip.IsUnspecified() {
		a.IP = ip
	}
	return a, nil
}

// ForeignListenIP is the address of web.listen when it is an IP this host
// does not have, nil otherwise (every address, loopback, a name, or the
// host's addresses cannot be read). The socket binds such an address all
// the same (FreeBind=yes, for one that comes up only after it at boot), so a
// typo does not fail: the panel listens where nobody reaches it.
func ForeignListenIP(listen string) net.IP {
	host, _, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		return nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return nil
		}
	}
	return ip
}

// listenStreamRe is every value ListenStream() makes: digits, dots, colons
// and brackets. Anything else in the file PID 1 reads as root is refused.
var listenStreamRe = regexp.MustCompile(`^([0-9]{1,5}|[0-9.]{7,15}:[0-9]{1,5}|\[[0-9a-fA-F:.]{2,45}\]:[0-9]{1,5})$`)

// apiSocketDropInContent is the socket's drop-in for addr. The empty
// ListenStream= first drops the unit's own address: a socket listens on
// every ListenStream= it has.
//
// A port below portStart (net.ipv4.ip_unprivileged_port_start) keeps the
// socket off: no local account can bind such a port while the API is down,
// so it needs no keeping — and PID 1 binding it would hand the unprivileged
// API a port it cannot bind itself (443, 25 or 53, early at boot, ahead of
// the daemons they belong to). The API listens on it itself then, as before,
// which takes CAP_NET_BIND_SERVICE in its unit. The condition is never met
// ("/" always exists), and a socket skipped for it does not fail.
func apiSocketDropInContent(addr APISocketAddr, portStart int) ([]byte, error) {
	ls := addr.ListenStream()
	if !listenStreamRe.MatchString(ls) {
		return nil, fmt.Errorf("%q is not an address for monopanel-api.socket", ls)
	}
	head := "# Generated by MonoPanel from web.listen in config.yaml — do not edit.\n"
	if addr.Port < portStart {
		head += fmt.Sprintf("# Port %d is below %d (net.ipv4.ip_unprivileged_port_start): no local account can\n# take it, and monopanel-api listens on it itself. This socket stays off.\n[Unit]\nConditionPathExists=!/\n\n", addr.Port, portStart)
	}
	return []byte(head + "[Socket]\nListenStream=\nListenStream=" + ls + "\n"), nil
}

// unprivilegedPortStart is the first port any process may bind.
func unprivilegedPortStart() int {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return 1024
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 1024
	}
	return n
}

// apiServiceDropInContent ties the API's unit to its socket. The units of
// the package and of mp setup say the same, but mp setup up to 0.8.19 wrote
// its own monopanel-api.service into /etc/systemd/system, which overrides
// the package's and knows no socket — and only an API unit that wants the
// socket brings it back when a restart moves it to a new address.
// StartLimitIntervalSec=0: a unit that runs into its start limit takes its
// socket down with it (service-start-limit-hit), and the port would be free.
//
// It also keeps a binary that knows no socket from failing at every start.
// Up to 0.8.19 the API listened on the port itself and ignored the sockets
// systemd passed (LISTEN_FDS). A self-update that rolls back puts back the
// binary alone, a downgraded package leaves this drop-in behind: the socket
// would go on holding the port, the old API fail on EADDRINUSE at every
// start, and the panel stay down until somebody stopped the socket. So
// before each start PID 1 runs, as root ("+"), failure ignored ("-"), the
// probe of binary, the panel's binary the unit runs: every version that
// takes the socket answers "api-socket-sync --probe" with 0, an older one
// knows no such command. When it fails, the socket is stopped:
//
//   - --no-block: systemctl only queues the stop job and returns. The shell
//     waits for no job, so nothing it waits on can wait on the API's own
//     start job, which is running (it is at its ExecStartPre).
//   - The job is the socket's alone: the API only Wants= it, and Wants=,
//     After= and the socket's Triggers= carry no stop over to the service,
//     so the transaction neither cancels nor replaces the API's start job.
//     The job engine holds a stop job back for no start job, whichever way
//     After= goes (job_compare: the stop goes first), so it runs at once:
//     the socket closes its listening descriptor, the only one, as no
//     process has taken it yet, and is inactive.
//   - sleep 1 gives PID 1 that moment before the shell exits: it runs its
//     job queue at idle priority, after a pending SIGCHLD, and the shell's
//     exit is what starts ExecStart.
//   - ExecStart finds the socket inactive and passes no descriptor (a
//     stopped socket holds none); the port is free, and the old binary binds
//     it itself. Each later start pulls the socket in (Wants=), and the
//     probe stops it again.
//
// binary is a plain path (panelBinaryRe), else the package's.
func apiServiceDropInContent(socket, binary string) []byte {
	if binary = filepath.Clean(binary); !panelBinaryRe.MatchString(binary) {
		binary = defaultPanelBinary
	}
	script := shellWord(binary) + " api-socket-sync --probe >/dev/null 2>&1 || { systemctl stop --no-block " + shellWord(socket) + "; sleep 1; }"
	return []byte("# Generated by MonoPanel — do not edit: monopanel-api takes its HTTPS port from\n# " + socket + ", which PID 1 keeps bound while the API restarts.\n" +
		"[Unit]\nWants=" + socket + "\nAfter=" + socket + "\nStartLimitIntervalSec=0\n\n" +
		"# A binary that knows no socket (0.8.19 or older: a self-update rolled back, a\n# downgraded package) would find its port taken: the socket stops, it binds the port.\n" +
		"[Service]\nExecStartPre=-+/bin/sh -c '" + script + "'\n")
}

// shellWord is s as one word of the shell script inside a single-quoted
// argument of a systemd command line: double-quoted for the shell, which
// keeps a backslash before anything but $ ` " \ and a line break there, and
// with every backslash doubled for the shell and again for systemd, which
// reads C escapes inside quotes too — a unit name may hold \x2d. s holds
// nothing else either of them expands or that ends a quote ($ ` " ' %):
// unitRe and panelBinaryRe let none of it through.
func shellWord(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\\\`) + `"` }

// defaultPanelBinary is where the package installs the panel's binary.
const defaultPanelBinary = "/usr/bin/monopanel"

// panelBinaryRe is a binary path the drop-in may carry: none of the
// characters systemd expands in a command line ("%", "$", "\") or that end a
// quoted word, and nothing the shell would read inside double quotes.
var panelBinaryRe = regexp.MustCompile(`^/[A-Za-z0-9._+@/-]+$`)

// thisBinary is the binary this process runs, the one the units name (mp
// setup writes it into them; the package's units and scripts use
// /usr/bin/monopanel, mp being a symlink to it). /proc/self/exe is the
// resolved path, without the " (deleted)" a replaced file has.
func thisBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return defaultPanelBinary
	}
	return exe
}

// apiUnits are the socket and the service unit of cfg's API, checked to be
// unit names: they become paths and lines of the drop-ins.
func apiUnits(cfg config.Config) (socket, service string, err error) {
	socket, service = cfg.APISocketUnit(), fullUnit(cfg.Update.APIUnit)
	if !unitRe.MatchString(socket) || !unitRe.MatchString(service) || !strings.HasSuffix(service, ".service") {
		return "", "", fmt.Errorf("update.api_unit %q is not the name of a service unit", cfg.Update.APIUnit)
	}
	return socket, service, nil
}

// APISocketSync writes the drop-ins of the panel's socket. The zero value
// works on this host; tests point the fields elsewhere.
type APISocketSync struct {
	// UnitDir is where the units are, /etc/systemd/system.
	UnitDir string
	// Lookup resolves a host name of web.listen; the system's resolver.
	Lookup LookupIP
	// Reload is systemd's daemon-reload, run after a file changed.
	Reload func(ctx context.Context) error
	// Restorecon gives what was written its SELinux label; by default
	// restorecon where SELinux is on.
	Restorecon func(ctx context.Context, paths ...string)
	// PortStart is the first port any process may bind; by default
	// net.ipv4.ip_unprivileged_port_start. The socket keeps none below it.
	PortStart func() int
	// Binary is the panel's binary the API unit runs, which the unit's
	// drop-in probes before each start (apiServiceDropInContent); by default
	// the one this process runs.
	Binary string

	// trust are the rules for the directories on the way (the agent's
	// own); root's, strict, by default.
	trust *dirTrust
}

// SyncAPISocket brings the drop-ins of the panel's socket to cfg on this
// host (APISocketSync.Sync), for root's own tools: mp setup, mp config set
// --local and the package's scripts. The agent does the same when web.listen
// changes through it.
func SyncAPISocket(ctx context.Context, cfg config.Config) (moved bool, err error) {
	return APISocketSync{}.Sync(ctx, cfg)
}

// Sync writes the socket's address for cfg's web.listen (ResolveAPISocket)
// and the API unit's dependency on the socket, each file only when its
// content differs, and reloads systemd when one changed. A web.listen the
// socket cannot take is an error, and nothing is written. moved reports
// that the address changed on disk: a socket that is listening does not move
// by itself, it has to be started afresh while no process holds its port
// (APISocketStale).
func (y APISocketSync) Sync(ctx context.Context, cfg config.Config) (moved bool, err error) {
	addr, err := ResolveAPISocket(ctx, cfg.Web.Listen, y.Lookup)
	if err != nil {
		return false, err
	}
	return y.write(ctx, cfg, addr)
}

// write writes the drop-ins for addr (Sync).
func (y APISocketSync) write(ctx context.Context, cfg config.Config, addr APISocketAddr) (moved bool, err error) {
	socket, service, err := apiUnits(cfg)
	if err != nil {
		return false, err
	}
	portStart := unprivilegedPortStart
	if y.PortStart != nil {
		portStart = y.PortStart
	}
	listen, err := apiSocketDropInContent(addr, portStart())
	if err != nil {
		return false, err
	}
	binary := y.Binary
	if binary == "" {
		binary = thisBinary()
	}
	dir := y.unitDir()
	trust := y.trust
	if trust == nil {
		trust = &dirTrust{rootUID: os.Geteuid(), serviceUser: cfg.ServiceUser, strict: true}
	}
	var written []string
	var errs []error
	for _, f := range []struct {
		dir, name string
		content   []byte
	}{
		{filepath.Join(dir, socket+".d"), apiSocketDropIn, listen},
		{filepath.Join(dir, service+".d"), apiServiceDropIn, apiServiceDropInContent(socket, binary)},
	} {
		wrote, err := writeDropIn(trust, len(pathParts(dir)), f.dir, f.name, f.content)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(f.dir, f.name), err))
		} else if wrote {
			written = append(written, f.dir, filepath.Join(f.dir, f.name))
			moved = moved || f.name == apiSocketDropIn
		}
	}
	if len(written) > 0 {
		if y.Restorecon != nil {
			y.Restorecon(ctx, written...)
		} else {
			restoreconSELinux(ctx, written...)
		}
		// What is on disk is what systemd goes by, also when the other file
		// failed.
		if err := y.reload(ctx); err != nil {
			errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
		}
	}
	return moved, errors.Join(errs...)
}

func (y APISocketSync) unitDir() string {
	if y.UnitDir == "" {
		return systemUnitDir
	}
	return filepath.Clean(y.UnitDir)
}

func (y APISocketSync) reload(ctx context.Context) error {
	if y.Reload != nil {
		return y.Reload(ctx)
	}
	return systemdReload(ctx)
}

// systemdReload is daemon-reload over D-Bus.
func systemdReload(ctx context.Context) error {
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.DaemonReload(ctx)
}

// writeDropIn writes content as name in the drop-in directory dir, 0644 and
// the writer's (root's), unless it holds exactly that already. The unit
// directory is root's: the walk resolves root's symlinks on the way to it
// (follow components, as for the units) and none below, refuses a directory
// someone else could rearrange, and makes the drop-in directory when it is
// missing. The file is made next to its name and renamed over it, never
// written through whatever is there.
func writeDropIn(trust *dirTrust, follow int, dir, name string, content []byte) (bool, error) {
	w, err := walkDir("/", dir, walkOpts{trust: trust, follow: follow, create: true, mode: 0o755, uid: -1, gid: -1})
	if err != nil {
		return false, err
	}
	defer w.close()
	old, st, existed, err := readFileAt(w.fd, name)
	switch {
	case errors.Is(err, errNotPlainFile):
		return false, fmt.Errorf("%w: remove it by hand", err)
	case err != nil:
		return false, err
	case existed && bytes.Equal(old, content) && st.Mode&0o777 == 0o644:
		return false, nil
	}
	if err := writeFileAt(w.fd, w.resolved, name, content, 0o644, -1, -1); err != nil {
		return false, err
	}
	return true, nil
}

// restoreconSELinux gives files the label of their place where SELinux is
// on; the agent has its own (Server.restorecon).
func restoreconSELinux(ctx context.Context, paths ...string) {
	const bin = "/usr/sbin/restorecon"
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil || len(paths) == 0 {
		return
	}
	if _, err := os.Stat(bin); err != nil {
		return
	}
	_ = exec.CommandContext(ctx, bin, append([]string{"-F"}, paths...)...).Run()
}

// APISocketDropIn is the drop-in with the socket's address on this host.
func APISocketDropIn(cfg config.Config) string {
	return filepath.Join(systemUnitDir, cfg.APISocketUnit()+".d", apiSocketDropIn)
}

// APISocketStale tells whether the socket has to be started afresh to listen
// where its drop-in says: it is loaded, and either not listening or started
// before the drop-in was last written — daemon-reload then left it without
// the socket of the old address and without one for the new ("Unit
// configuration changed while unit was running, and no socket file
// descriptors are open"). dropIn is the drop-in's modification time, zero
// when there is none.
func APISocketStale(st systemd.Status, dropIn time.Time) bool {
	if st.LoadState != "loaded" {
		return false
	}
	return st.ActiveState != "active" || !dropIn.IsZero() && dropIn.After(st.ActiveSince)
}

// apiSocket is APISocketSync for the agent: its unit directory, trust rules,
// resolver, reload and labels.
func (s *Server) apiSocket() APISocketSync {
	return APISocketSync{UnitDir: s.unitDirectory(), Lookup: s.lookupIP, Reload: s.daemonReload, Restorecon: s.restorecon, trust: s.trust()}
}
