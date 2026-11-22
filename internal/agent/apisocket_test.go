package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"monopanel/internal/config"
	"monopanel/internal/systemd"
)

// testLookup is the DNS of the tests: panel.example.com has both families
// (IPv6 first), v6.example.com IPv6 only, any.example.com the IPv4 wildcard,
// nowhere.example.com no address and broken.example.com an error.
func testLookup(asked *[]string) LookupIP {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		if asked != nil {
			*asked = append(*asked, host)
		}
		ips := map[string][]string{
			"panel.example.com": {"2001:db8::7", "203.0.113.9", "203.0.113.10"},
			"v6.example.com":    {"2001:db8::8", "2001:db8::9"},
			"any.example.com":   {"0.0.0.0"},
			"localhost":         {"::1", "127.0.0.1"},
		}[host]
		if host == "broken.example.com" {
			return nil, errors.New("server misbehaving")
		}
		out := make([]net.IPAddr, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
}

// web.listen becomes ListenStream= of the socket the way net.Listen would
// listen on it: every address dual-stack for an empty or unspecified host,
// an IP as it is, a name as its first IPv4 address. Anything the panel's
// rules refuse is refused here too.
func TestResolveAPISocket(t *testing.T) {
	var asked []string
	for listen, want := range map[string]string{
		":8443":                     "8443",
		"0.0.0.0:8443":              "8443",
		"[::]:9443":                 "9443",
		" :8443 ":                   "8443",
		"203.0.113.5:8443":          "203.0.113.5:8443",
		"[2001:DB8:0::5]:8443":      "[2001:db8::5]:8443",
		"[::ffff:203.0.113.5]:8443": "203.0.113.5:8443",
		"[::1]:8443":                "[::1]:8443",
		"127.0.0.1:65535":           "127.0.0.1:65535",
		"Panel.Example.COM:8443":    "203.0.113.9:8443",
		"v6.example.com:1":          "[2001:db8::8]:1",
		"any.example.com:8443":      "8443",
		"localhost:9443":            "127.0.0.1:9443",
	} {
		got, err := ResolveAPISocket(context.Background(), listen, testLookup(&asked))
		if err != nil || got.ListenStream() != want {
			t.Errorf("%q: %q %v, want %q", listen, got.ListenStream(), err, want)
		}
	}
	for _, host := range asked {
		if host != strings.ToLower(host) {
			t.Errorf("looked up %q, not the normalized name", host)
		}
	}
	for _, listen := range []string{
		"", "8443", ":0", ":70000", ":08443", ":http", "nowhere", "panel_1.example.com:8443",
		"[fe80::1%eth0]:8443", "203.0.113.5:8443\nListenStream=1", "a\nb:8443", "[::1]",
		"nowhere.example.com:8443", "broken.example.com:8443",
	} {
		if got, err := ResolveAPISocket(context.Background(), listen, testLookup(nil)); err == nil {
			t.Errorf("%q was taken for %q", listen, got.ListenStream())
		}
	}
}

// The socket binds an address the host does not have (FreeBind=yes): the
// root tools warn about one. Every address, loopback, names and the host's
// own addresses are fine.
func TestForeignListenIP(t *testing.T) {
	for _, listen := range []string{":8443", "0.0.0.0:8443", "[::]:8443", "127.0.0.1:8443", "[::1]:8443", "panel.example.com:8443", "nonsense"} {
		if ip := ForeignListenIP(listen); ip != nil {
			t.Errorf("%s: %s taken for foreign", listen, ip)
		}
	}
	if ip := ForeignListenIP("203.0.113.77:8443"); !ip.Equal(net.ParseIP("203.0.113.77")) {
		t.Errorf("a documentation address: %v", ip)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
			if ip := ForeignListenIP(net.JoinHostPort(n.IP.String(), "8443")); ip != nil {
				t.Errorf("this host's %s taken for foreign", n.IP)
			}
		}
	}
}

// A socket systemd passed is the panel's when it listens on the port of
// web.listen, on every address for every address, else on the very one.
func TestAPISocketAddrHolds(t *testing.T) {
	tcp := func(ip string, port int) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: port} }
	every := APISocketAddr{Port: 8443}
	one := APISocketAddr{IP: net.ParseIP("203.0.113.5"), Port: 8443}
	six := APISocketAddr{IP: net.ParseIP("2001:db8::5"), Port: 8443}
	for _, c := range []struct {
		want APISocketAddr
		addr net.Addr
		ok   bool
	}{
		{every, tcp("::", 8443), true},
		{every, tcp("0.0.0.0", 8443), true}, // systemd's own fallback without IPv6
		{every, tcp("::", 9443), false},
		{every, tcp("127.0.0.1", 8443), false},
		{every, tcp("203.0.113.5", 8443), false},
		{one, tcp("203.0.113.5", 8443), true},
		{one, tcp("::ffff:203.0.113.5", 8443), true},
		{one, tcp("203.0.113.6", 8443), false},
		{one, tcp("::", 8443), false},
		{one, tcp("203.0.113.5", 8444), false},
		{six, tcp("2001:db8::5", 8443), true},
		{six, tcp("2001:db8::6", 8443), false},
		{every, &net.UnixAddr{Name: "/run/x.sock", Net: "unix"}, false},
		{every, &net.UDPAddr{IP: net.IPv6unspecified, Port: 8443}, false},
	} {
		if got := c.want.Holds(c.addr); got != c.ok {
			t.Errorf("%s holds %s: %v, want %v", c.want.ListenStream(), c.addr, got, c.ok)
		}
	}
}

// apiSocketSyncIn is the sync into a temporary unit directory, with the
// test DNS and a reload that counts.
func apiSocketSyncIn(t *testing.T) (APISocketSync, string, *int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "system")
	reloads := 0
	return APISocketSync{UnitDir: dir, Lookup: testLookup(nil), Reload: func(context.Context) error { reloads++; return nil }, Restorecon: func(context.Context, ...string) {},
		PortStart: func() int { return 1024 }, Binary: "/usr/bin/monopanel"}, dir, &reloads
}

// probeLine is the ExecStartPre of the API unit's drop-in for the socket
// monopanel-api.socket and the binary /usr/bin/monopanel.
const probeLine = `ExecStartPre=-+/bin/sh -c '"/usr/bin/monopanel" api-socket-sync --probe >/dev/null 2>&1 || { systemctl stop --no-block "monopanel-api.socket"; sleep 1; }'`

// The API unit's drop-in wants the socket and, before every start, asks the
// binary the unit runs whether it takes the socket: one that does not (0.8.19
// or older) has the socket stopped and binds the port itself. The binary goes
// into a line systemd expands and the shell reads, so only a plain path does;
// anything else is the package's.
func TestAPIServiceDropInProbesTheBinary(t *testing.T) {
	want := "# Generated by MonoPanel — do not edit: monopanel-api takes its HTTPS port from\n# monopanel-api.socket, which PID 1 keeps bound while the API restarts.\n" +
		"[Unit]\nWants=monopanel-api.socket\nAfter=monopanel-api.socket\nStartLimitIntervalSec=0\n\n" +
		"# A binary that knows no socket (0.8.19 or older: a self-update rolled back, a\n# downgraded package) would find its port taken: the socket stops, it binds the port.\n" +
		"[Service]\n" + probeLine + "\n"
	if got := string(apiServiceDropInContent("monopanel-api.socket", "/usr/bin/monopanel")); got != want {
		t.Fatalf("socket.conf:\n%s\nwant:\n%s", got, want)
	}
	for binary, used := range map[string]string{
		"/usr/local/bin/monopanel":            "/usr/local/bin/monopanel",
		"/opt/monopanel-0.8.20+dev/monopanel": "/opt/monopanel-0.8.20+dev/monopanel",
		"/usr/local/bin/../sbin//monopanel":   "/usr/local/sbin/monopanel",
		"":                                    "/usr/bin/monopanel",
		"monopanel":                           "/usr/bin/monopanel",
		"/opt/my panel/monopanel":             "/usr/bin/monopanel",
		"/opt/$HOME/monopanel":                "/usr/bin/monopanel",
		"/opt/%n/monopanel":                   "/usr/bin/monopanel",
		"/opt/a\"b/monopanel":                 "/usr/bin/monopanel",
		"/opt/a'b/monopanel":                  "/usr/bin/monopanel",
		"/opt/a\\b/monopanel":                 "/usr/bin/monopanel",
		"/opt/a;b/monopanel":                  "/usr/bin/monopanel",
		"/opt/`id`/monopanel":                 "/usr/bin/monopanel",
		"/opt/x\nExecStartPre=/bin/sh":        "/usr/bin/monopanel",
	} {
		got := string(apiServiceDropInContent("monopanel-api.socket", binary))
		if !strings.Contains(got, "\nExecStartPre=-+/bin/sh -c '\""+used+"\" api-socket-sync --probe ") || strings.Count(got, "ExecStartPre=") != 1 {
			t.Errorf("binary %q:\n%s", binary, got)
		}
	}
}

// The probe line run the way PID 1 runs it: a binary that answers the probe
// leaves the socket alone; one that does not, or is not there, has it
// stopped without waiting for the job, and the shell succeeds either way.
// A unit name with an escape of systemd's (\x2d) reaches systemctl as it is.
func TestAPIServiceDropInProbeRuns(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script("systemctl", `printf 'systemctl %s\n' "$*" >> `+log)
	script("sleep", `printf 'sleep %s\n' "$*" >> `+log)
	knows := script("monopanel-new", `[ "$1 $2" = "api-socket-sync --probe" ] || exit 1`)
	// What cobra makes of a command a binary does not have.
	old := script("monopanel-old", `echo "error: unknown command \"$1\" for \"mp\"" >&2; exit 1`)
	for _, c := range []struct {
		name, binary, socket, calls string
	}{
		{"a binary that takes the socket", knows, "test-api.socket", ""},
		{"a binary of 0.8.19", old, "test-api.socket", "systemctl stop --no-block test-api.socket\nsleep 1\n"},
		{"no binary", filepath.Join(dir, "missing"), "test-api.socket", "systemctl stop --no-block test-api.socket\nsleep 1\n"},
		{"an escaped unit name", old, `a\x3bid\x2dapi.socket`, "systemctl stop --no-block a\\x3bid\\x2dapi.socket\nsleep 1\n"},
	} {
		os.Remove(log)
		content := string(apiServiceDropInContent(c.socket, c.binary))
		line := ""
		for _, l := range strings.Split(content, "\n") {
			if strings.HasPrefix(l, "ExecStartPre=") {
				line = l
			}
		}
		// systemd: "-" ignores a failure, "+" runs it as root without the
		// unit's sandbox; the rest is the command line, one single-quoted
		// argument with nothing systemd expands in it but the doubled
		// backslashes it reads as one.
		cmdline, ok := strings.CutPrefix(line, "ExecStartPre=-+/bin/sh -c '")
		if !ok || !strings.HasSuffix(cmdline, "'") {
			t.Fatalf("%s: %q", c.name, line)
		}
		sh := strings.TrimSuffix(cmdline, "'")
		if strings.ContainsAny(sh, "'$%`") || strings.Count(sh, `\`) != 2*strings.Count(sh, `\\`) {
			t.Fatalf("%s: systemd would expand or end the argument: %q", c.name, sh)
		}
		sh = strings.ReplaceAll(sh, `\\`, `\`)
		cmd := exec.Command("/bin/sh", "-c", sh)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: the shell failed: %v %s", c.name, err, out)
		}
		calls, _ := os.ReadFile(log)
		if string(calls) != c.calls {
			t.Errorf("%s: calls %q, want %q", c.name, calls, c.calls)
		}
	}
}

// An API unit's drop-in written before the probe (0.8.20 before H9) is
// rewritten by the next sync, once; the socket's address does not move for
// it.
func TestSyncAPISocketAddsTheProbe(t *testing.T) {
	ctx := context.Background()
	y, dir, reloads := apiSocketSyncIn(t)
	if _, err := y.Sync(ctx, listenCfg(":8443")); err != nil || *reloads != 1 {
		t.Fatalf("first sync: %v, %d reloads", err, *reloads)
	}
	service := filepath.Join(dir, "monopanel-api.service.d", "socket.conf")
	const before = "# Generated by MonoPanel — do not edit: monopanel-api takes its HTTPS port from\n# monopanel-api.socket, which PID 1 keeps bound while the API restarts.\n[Unit]\nWants=monopanel-api.socket\nAfter=monopanel-api.socket\nStartLimitIntervalSec=0\n"
	if err := os.WriteFile(service, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, err := y.Sync(ctx, listenCfg(":8443"))
	if err != nil || moved || *reloads != 2 {
		t.Fatalf("over the old drop-in: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	if b, _ := os.ReadFile(service); !strings.Contains(string(b), "\n"+probeLine+"\n") || !strings.HasPrefix(string(b), strings.TrimSuffix(before, "StartLimitIntervalSec=0\n")) {
		t.Fatalf("socket.conf:\n%s", b)
	}
	if moved, err := y.Sync(ctx, listenCfg(":8443")); err != nil || moved || *reloads != 2 {
		t.Fatalf("again: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	// The drop-in names the binary of the sync's caller: mp setup gives the
	// one it writes into the units.
	y.Binary = "/usr/local/bin/monopanel"
	if moved, err := y.Sync(ctx, listenCfg(":8443")); err != nil || moved || *reloads != 3 {
		t.Fatalf("another binary: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	if b, _ := os.ReadFile(service); !strings.Contains(string(b), `'"/usr/local/bin/monopanel" api-socket-sync --probe`) {
		t.Fatalf("socket.conf:\n%s", b)
	}
}

// A port below ip_unprivileged_port_start keeps the socket off: no local
// account can take it, and PID 1 binding it would hand the unprivileged API
// a port it cannot bind itself. Where the host lets everyone bind it, the
// socket holds it like any other.
func TestSyncAPISocketKeepsOffPrivilegedPorts(t *testing.T) {
	ctx := context.Background()
	y, dir, _ := apiSocketSyncIn(t)
	listen := filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")
	for _, c := range []struct {
		listen string
		start  int
		off    bool
	}{
		{":443", 1024, true},
		{"203.0.113.5:25", 1024, true},
		{":1023", 1024, true},
		{":1024", 1024, false},
		{":8443", 1024, false},
		{":443", 80, false},
		{":80", 80, false},
		{":79", 80, true},
	} {
		y.PortStart = func() int { return c.start }
		if _, err := y.Sync(ctx, listenCfg(c.listen)); err != nil {
			t.Fatalf("%s: %v", c.listen, err)
		}
		b, _ := os.ReadFile(listen)
		off := strings.Contains(string(b), "\n[Unit]\nConditionPathExists=!/\n")
		stream := strings.HasSuffix(string(b), "\n[Socket]\nListenStream=\nListenStream="+strings.TrimPrefix(c.listen, ":")+"\n")
		if off != c.off || !stream {
			t.Errorf("%s with ports from %d: off=%v, want %v:\n%s", c.listen, c.start, off, c.off, b)
		}
	}
}

func listenCfg(listen string) config.Config {
	cfg := config.Default()
	cfg.Web.Listen = listen
	return cfg
}

// The drop-ins hold exactly the address of web.listen and the API unit's
// dependency on the socket; what is already there is not written again,
// and systemd is reloaded only after a change.
func TestSyncAPISocketWritesTheDropIns(t *testing.T) {
	ctx := context.Background()
	y, dir, reloads := apiSocketSyncIn(t)
	listen := filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")
	service := filepath.Join(dir, "monopanel-api.service.d", "socket.conf")

	moved, err := y.Sync(ctx, listenCfg(":9443"))
	if err != nil || !moved || *reloads != 1 {
		t.Fatalf("first sync: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	wantListen := "# Generated by MonoPanel from web.listen in config.yaml — do not edit.\n[Socket]\nListenStream=\nListenStream=9443\n"
	if b, _ := os.ReadFile(listen); string(b) != wantListen {
		t.Fatalf("listen.conf:\n%s", b)
	}
	b, _ := os.ReadFile(service)
	for _, line := range []string{"[Unit]\n", "\nWants=monopanel-api.socket\n", "\nAfter=monopanel-api.socket\n", "\nStartLimitIntervalSec=0\n"} {
		if !strings.Contains(string(b), line) {
			t.Errorf("socket.conf lacks %q:\n%s", line, b)
		}
	}
	for _, p := range []string{listen, service} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	before, _ := os.Stat(listen)

	// The same address, however it is written, changes nothing.
	for _, same := range []string{":9443", "0.0.0.0:9443", "[::]:9443"} {
		if moved, err := y.Sync(ctx, listenCfg(same)); err != nil || moved || *reloads != 1 {
			t.Fatalf("%s again: moved=%v %v, %d reloads", same, moved, err, *reloads)
		}
	}
	if after, _ := os.Stat(listen); !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged drop-in was written again")
	}

	if moved, err := y.Sync(ctx, listenCfg("panel.example.com:8443")); err != nil || !moved || *reloads != 2 {
		t.Fatalf("a new address: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	if b, _ := os.ReadFile(listen); !strings.HasSuffix(string(b), "\nListenStream=\nListenStream=203.0.113.9:8443\n") {
		t.Fatalf("listen.conf:\n%s", b)
	}

	// Refused: nothing written, no reload.
	for _, bad := range []string{"nowhere.example.com:8443", "8443", "[fe80::1%eth0]:8443"} {
		if _, err := y.Sync(ctx, listenCfg(bad)); err == nil {
			t.Errorf("%q was written", bad)
		}
	}
	if b, _ := os.ReadFile(listen); !strings.Contains(string(b), "203.0.113.9:8443") || *reloads != 2 {
		t.Fatalf("a refused web.listen changed the drop-in (%d reloads):\n%s", *reloads, b)
	}

	// Only the service's drop-in missing: written and reloaded, but the
	// socket's address did not move.
	if err := os.Remove(service); err != nil {
		t.Fatal(err)
	}
	if moved, err := y.Sync(ctx, listenCfg("panel.example.com:8443")); err != nil || moved || *reloads != 3 {
		t.Fatalf("the service's drop-in again: moved=%v %v, %d reloads", moved, err, *reloads)
	}
	if _, err := os.Stat(service); err != nil {
		t.Fatal(err)
	}
}

// The units' names come from update.api_unit; one that is no unit name is
// no path either.
func TestSyncAPISocketFollowsTheAPIUnit(t *testing.T) {
	ctx := context.Background()
	y, dir, _ := apiSocketSyncIn(t)
	cfg := listenCfg(":8443")
	cfg.Update.APIUnit = "test-api"
	if _, err := y.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"test-api.socket.d/listen.conf", "test-api.service.d/socket.conf"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "test-api.service.d/socket.conf")); !strings.Contains(string(b), "\nWants=test-api.socket\n") {
		t.Errorf("socket.conf:\n%s", b)
	}
	for _, unit := range []string{"../etc/passwd", "a/b.service", "api.timer", "x y.service"} {
		cfg.Update.APIUnit = unit
		if _, err := y.Sync(ctx, cfg); err == nil {
			t.Errorf("api_unit %q was used", unit)
		}
	}
}

// Root writes the drop-ins without following a symlink: one left under
// either name is not written through, and a drop-in directory that is a
// symlink is refused.
func TestSyncAPISocketFollowsNoSymlink(t *testing.T) {
	ctx := context.Background()
	y, dir, reloads := apiSocketSyncIn(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "monopanel-api.socket.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := y.Sync(ctx, listenCfg(":8443")); err == nil {
		t.Fatal("a symlink in place of listen.conf was accepted")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatalf("written through the symlink: %q", b)
	}
	// The other drop-in did go in, and systemd was told.
	if _, err := os.Stat(filepath.Join(dir, "monopanel-api.service.d", "socket.conf")); err != nil || *reloads != 1 {
		t.Fatalf("socket.conf: %v, %d reloads", err, *reloads)
	}

	z, dir2, _ := apiSocketSyncIn(t)
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir2, "monopanel-api.socket.d")); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Sync(ctx, listenCfg(":8443")); err == nil {
		t.Fatal("a symlinked drop-in directory was accepted")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("written through the symlinked directory: %v", entries)
	}
}

// A failed reload is reported; the drop-in stays, so that the next reload
// takes it.
func TestSyncAPISocketReportsTheReload(t *testing.T) {
	y, dir, _ := apiSocketSyncIn(t)
	y.Reload = func(context.Context) error { return errors.New("systemd unreachable") }
	moved, err := y.Sync(context.Background(), listenCfg(":8443"))
	if err == nil || !strings.Contains(err.Error(), "daemon-reload") || !moved {
		t.Fatalf("moved=%v %v", moved, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")); err != nil {
		t.Fatal(err)
	}
}

// A socket has to start afresh when it does not listen, or when it was
// started before its drop-in was written; one that is not there needs
// nothing.
func TestAPISocketStale(t *testing.T) {
	written := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		st     systemd.Status
		dropIn time.Time
		stale  bool
	}{
		{"no socket", systemd.Status{LoadState: "not-found", ActiveState: "inactive"}, written, false},
		{"masked", systemd.Status{LoadState: "masked", ActiveState: "inactive"}, written, false},
		{"stopped", systemd.Status{LoadState: "loaded", ActiveState: "inactive"}, written, true},
		{"failed", systemd.Status{LoadState: "loaded", ActiveState: "failed"}, time.Time{}, true},
		{"started before the drop-in", systemd.Status{LoadState: "loaded", ActiveState: "active", ActiveSince: written.Add(-time.Minute)}, written, true},
		{"started after the drop-in", systemd.Status{LoadState: "loaded", ActiveState: "active", ActiveSince: written.Add(time.Second)}, written, false},
		{"listening, no drop-in", systemd.Status{LoadState: "loaded", ActiveState: "active", ActiveSince: written}, time.Time{}, false},
	} {
		if got := APISocketStale(c.st, c.dropIn); got != c.stale {
			t.Errorf("%s: %v, want %v", c.name, got, c.stale)
		}
	}
}

// A new web.listen through the agent moves the socket's drop-in along, from
// the checked value; other keys leave it alone. A name the host cannot
// resolve changes nothing, and a drop-in that cannot be written does not
// keep config.yaml from changing.
func TestPanelConfigMovesTheSocket(t *testing.T) {
	ctx := context.Background()
	_, path, cfg := panelConfigFixture(t)
	var reloads atomic.Int32
	s, cl := serveAgent(t, cfg, func(s *Server) {
		s.daemonReload = func(context.Context) error { reloads.Add(1); return nil }
	})
	listen := filepath.Join(s.unitDir, "monopanel-api.socket.d", "listen.conf")

	res, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": "127.0.0.1:9443"}})
	if err != nil || res.SocketError != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(listen); !strings.HasSuffix(string(b), "\nListenStream=127.0.0.1:9443\n") || reloads.Load() != 1 {
		t.Fatalf("drop-in (%d reloads):\n%s", reloads.Load(), b)
	}

	if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"log.level": "debug"}}); err != nil || reloads.Load() != 1 {
		t.Fatalf("another key: %v, %d reloads", err, reloads.Load())
	}

	before, _ := os.ReadFile(path)
	if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": "nowhere.example.com:8443"}}); agentStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("an unresolvable name: %v, want 422", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || reloads.Load() != 1 {
		t.Fatalf("an unresolvable name was written (%d reloads):\n%s", reloads.Load(), after)
	}
	// A name goes in as the address the API would listen on.
	if _, err := cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": "panel.example.com:9443"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(listen); !strings.HasSuffix(string(b), "\nListenStream=203.0.113.9:9443\n") {
		t.Fatalf("a name in the drop-in:\n%s", b)
	}

	// The drop-in's directory taken by a file: config.yaml changes, and the
	// answer says why the socket did not follow.
	if err := os.RemoveAll(filepath.Dir(listen)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(listen), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = cl.SetPanelConfig(ctx, &PanelConfigRequest{Path: path, Set: map[string]string{"web.listen": ":9555"}})
	if err != nil || res.SocketError == "" || strings.Join(res.Changed, " ") != "web.listen" {
		t.Fatalf("a drop-in that cannot be written: %+v %v", res, err)
	}
	if back, _ := config.Load(path); back.Web.Listen != ":9555" {
		t.Fatalf("config.yaml: %s", back.Web.Listen)
	}
}
