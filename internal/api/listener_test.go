package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
)

// listenerServer is a Server with nothing but web.listen and a log the test
// reads.
func listenerServer(listen string) (*Server, *bytes.Buffer) {
	var logs bytes.Buffer
	cfg := config.Default()
	cfg.Web.Listen = listen
	return &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

func tcpListen(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func portOf(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

func closed(ln net.Listener) bool {
	_, err := ln.Accept()
	return errors.Is(err, net.ErrClosed)
}

// The socket systemd passed is the panel's listener when it is where
// web.listen says — every address for every address, however written, the
// very address otherwise, a name as net.Listen resolves it.
func TestHTTPSListenerTakesTheSocketOnWebListen(t *testing.T) {
	ctx := context.Background()
	all := tcpListen(t, ":0")
	p := portOf(all)
	for _, listen := range []string{fmt.Sprintf(":%d", p), fmt.Sprintf("0.0.0.0:%d", p), fmt.Sprintf("[::]:%d", p)} {
		s, logs := listenerServer(listen)
		ln, onSocket, err := s.httpsListener(ctx, []net.Listener{nil, all})
		if err != nil || !onSocket || ln != all || logs.Len() != 0 {
			t.Fatalf("%s: %v %v %v, log %q", listen, ln, onSocket, err, logs)
		}
	}
	one := tcpListen(t, "127.0.0.1:0")
	for _, listen := range []string{fmt.Sprintf("127.0.0.1:%d", portOf(one)), fmt.Sprintf("localhost:%d", portOf(one))} {
		s, logs := listenerServer(listen)
		if ln, onSocket, err := s.httpsListener(ctx, []net.Listener{one}); err != nil || !onSocket || ln != one || logs.Len() != 0 {
			t.Fatalf("%s: %v %v %v, log %q", listen, ln, onSocket, err, logs)
		}
	}
}

// A socket elsewhere is closed with a warning that names both addresses and
// the way to align them, and the panel listens on web.listen itself —
// config.yaml wins; without a socket it simply does, as before.
func TestHTTPSListenerListensItselfWithoutTheRightSocket(t *testing.T) {
	ctx := context.Background()
	other := tcpListen(t, "127.0.0.1:0")
	want := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	s, logs := listenerServer(want)
	ln, onSocket, err := s.httpsListener(ctx, []net.Listener{other})
	if err != nil || onSocket || ln == other {
		t.Fatalf("%v %v %v", ln, onSocket, err)
	}
	defer ln.Close()
	if ln.Addr().String() != want || !closed(other) {
		t.Fatalf("listens on %s, the socket closed: %v", ln.Addr(), closed(other))
	}
	for _, part := range []string{other.Addr().String(), want, "mp setup", "mp config set web.listen", "level=WARN"} {
		if !strings.Contains(logs.String(), part) {
			t.Errorf("the warning lacks %q: %s", part, logs)
		}
	}

	// Of two sockets the one on web.listen is taken, the other closed.
	match := tcpListen(t, "127.0.0.1:0")
	extra := tcpListen(t, "127.0.0.1:0")
	s, logs = listenerServer(match.Addr().String())
	if ln, onSocket, err := s.httpsListener(ctx, []net.Listener{extra, match}); err != nil || !onSocket || ln != match || !closed(extra) || !strings.Contains(logs.String(), extra.Addr().String()) {
		t.Fatalf("two sockets: %v %v %v, log %q", ln, onSocket, err, logs)
	}

	s, logs = listenerServer("127.0.0.1:0")
	ln, onSocket, err = s.httpsListener(ctx, nil)
	if err != nil || onSocket || logs.Len() != 0 {
		t.Fatalf("no socket: %v %v %v, log %q", ln, onSocket, err, logs)
	}
	ln.Close()
}

// A web.listen on the port the socket keeps elsewhere cannot be bound while
// the socket listens: the error says how to move the socket.
func TestHTTPSListenerNamesTheSocketInTheWay(t *testing.T) {
	all := tcpListen(t, ":0")
	// systemd's own copy, which closing the passed one leaves in place
	copyOf, err := heldCopy(all)
	if err != nil {
		t.Fatal(err)
	}
	defer copyOf.Close()
	s, _ := listenerServer(fmt.Sprintf("127.0.0.1:%d", portOf(all)))
	_, _, err = s.httpsListener(context.Background(), []net.Listener{all})
	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "monopanel-api.socket holds the port elsewhere: run mp setup") {
		t.Fatalf("%v", err)
	}

	// The agent cannot stop the socket: the same error.
	s.stopAPISocket = func(context.Context) error { return errors.New("agent: connection refused") }
	all = tcpListen(t, ":0")
	again, err := heldCopy(all)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	s.cfg.Web.Listen = fmt.Sprintf("127.0.0.1:%d", portOf(all))
	if _, _, err = s.httpsListener(context.Background(), []net.Listener{all}); !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "run mp setup") {
		t.Fatalf("a socket the agent cannot stop: %v", err)
	}
}

// A socket that keeps web.listen's port on another address — on every
// address while web.listen names one, or the other way round, after an edit
// of config.yaml by hand or a drop-in that could not follow — is stopped
// through the agent, and the panel listens on web.listen itself: config.yaml
// wins. Otherwise the panel would fail at every start, after a reboot too.
func TestHTTPSListenerStopsTheSocketInTheWay(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ socket, listen string }{
		{":0", "127.0.0.1:%d"}, // the socket on every address, web.listen on one
		{"127.0.0.1:0", ":%d"}, // the socket on one address, web.listen on every one
		{"[::1]:0", "[::]:%d"}, // the same in IPv6
	} {
		passed, err := net.Listen("tcp", c.socket)
		if err != nil {
			t.Logf("%s: %v", c.socket, err)
			continue
		}
		pid1, err := heldCopy(passed)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(c.listen, portOf(passed))
		s, logs := listenerServer(want)
		var stops int
		s.stopAPISocket = func(context.Context) error { stops++; return pid1.Close() }
		ln, onSocket, err := s.httpsListener(ctx, []net.Listener{passed})
		if err != nil || onSocket || stops != 1 {
			pid1.Close()
			t.Fatalf("%s beside %s: %v %v, %d stops, log %s", want, c.socket, onSocket, err, stops, logs)
		}
		if !closed(passed) || portOf(ln) != portOf(passed) {
			t.Errorf("%s: listens on %s, the passed socket closed: %v", want, ln.Addr(), closed(passed))
		}
		if !strings.Contains(logs.String(), "stopping the socket systemd passed") {
			t.Errorf("%s: no warning about the stop: %s", want, logs)
		}
		ln.Close()
	}

	// A web.listen on another port needs no stop: the socket keeps its port,
	// the panel listens on its own.
	other := tcpListen(t, ":0")
	s, _ := listenerServer(fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	s.stopAPISocket = func(context.Context) error { t.Error("stopped a socket that was in nobody's way"); return nil }
	ln, _, err := s.httpsListener(ctx, []net.Listener{other})
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

// The second descriptor keeps the port bound after the listener is closed,
// until it is closed too.
func TestHeldCopyKeepsThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held, err := heldCopy(ln)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if other, err := net.Listen("tcp", addr); err == nil {
		other.Close()
		t.Fatal("the port was free while the copy held it")
	}
	held.Close()
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port stayed taken: %v", err)
	}
	again.Close()
}

// Run lets the port go when it is done; the daemon keeps it until the process
// ends, past the jobs it waits for after Run.
func TestHoldPortUntilRunOrExit(t *testing.T) {
	free := func(addr string) bool {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
		}
		return err == nil
	}
	for _, keep := range []bool{false, true} {
		s := &Server{}
		if keep {
			s.KeepPortUntilExit()
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		release := s.holdPort(ln)
		ln.Close() // Shutdown
		if free(addr) {
			t.Fatalf("keep=%v: the port was free once the listener closed", keep)
		}
		release() // Run is done
		if free(addr) != !keep {
			t.Fatalf("keep=%v: the port free after Run: %v", keep, free(addr))
		}
		if kept := s.keptPort.Load(); keep {
			kept.Close() // the end of the process
			if !free(addr) {
				t.Fatal("the port stayed taken after the kept copy closed")
			}
		} else if kept != nil {
			t.Fatal("a copy kept without KeepPortUntilExit")
		}
	}
}

// The console reaches the API on the loopback side of a wildcard listener:
// 127.0.0.1 for a dual-stack one, ::1 for one systemd made for IPv6 alone,
// where 127.0.0.1 on the port would be anyone's.
func TestConsoleURLOfAnIPv6OnlySocket(t *testing.T) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1) }); err != nil {
			return err
		}
		return serr
	}}
	only, err := lc.Listen(context.Background(), "tcp6", "[::]:0")
	if err != nil {
		t.Skip("no IPv6 here:", err)
	}
	defer only.Close()
	dual := tcpListen(t, ":0")
	if !v6Only(only) || v6Only(dual) || v6Only(tcpListen(t, "127.0.0.1:0")) {
		t.Fatalf("v6Only: %v %v", v6Only(only), v6Only(dual))
	}
	s := &Server{}
	s.listenAddr.Store(only.Addr())
	s.listenV6Only.Store(true)
	if got, want := s.consoleServerURL(), fmt.Sprintf("https://[::1]:%d", portOf(only)); got != want {
		t.Errorf("IPv6 only: %s, want %s", got, want)
	}
	s.listenAddr.Store(dual.Addr())
	s.listenV6Only.Store(false)
	if got, want := s.consoleServerURL(), fmt.Sprintf("https://127.0.0.1:%d", portOf(dual)); got != want {
		t.Errorf("dual-stack: %s, want %s", got, want)
	}
}

// waitServiceActions waits until the fake agent was asked for exactly these
// service actions, in this order. The restart of the API comes last in
// every sequence, so a socket action before it cannot slip by.
func waitServiceActions(t *testing.T, f *siteFixture, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.agent.ServiceActions()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service actions %q, want %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A restart that moves nothing restarts monopanel-api alone: the socket
// keeps the port throughout. A restart that applies a new web.listen — with
// the change or later, alone — stops the socket first, and the API's unit
// brings it back on the new address.
func TestRestartMovesTheSocketOnlyWithTheAddress(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.s.listenVia.Store(viaSocket)
	f.call(http.MethodPut, "/system/config", map[string]any{"log_level": "debug", "restart": true}, http.StatusOK, nil)
	waitServiceActions(t, f, "monopanel-api.service restart")
	f.call(http.MethodPut, "/system/config", map[string]any{"restart": true}, http.StatusOK, nil)
	waitServiceActions(t, f, "monopanel-api.service restart", "monopanel-api.service restart")

	g, _ := newConfigFixture(t, nil)
	g.s.listenVia.Store(viaSocket)
	g.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", freePort(t)), "restart": true}, http.StatusOK, nil)
	waitServiceActions(t, g, "monopanel-api.socket stop", "monopanel-api.service restart")

	h, _ := newConfigFixture(t, nil)
	h.s.listenVia.Store(viaSocket)
	h.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", freePort(t))}, http.StatusOK, nil)
	time.Sleep(50 * time.Millisecond)
	if acts := h.agent.ServiceActions(); len(acts) != 0 {
		t.Fatalf("a change without a restart acted on units: %q", acts)
	}
	h.call(http.MethodPut, "/system/config", map[string]any{"restart": true}, http.StatusOK, nil)
	waitServiceActions(t, h, "monopanel-api.socket stop", "monopanel-api.service restart")
}

// An API that listens on a port of its own (the socket was not there, not
// listening, or elsewhere when it started) takes the socket along at its
// next restart, so that the API after it is on the socket again. A host
// without the socket restarts the API alone.
func TestRestartBringsTheSocketBack(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.s.listenVia.Store(viaSelf)
	f.call(http.MethodPut, "/system/config", map[string]any{"restart": true}, http.StatusOK, nil)
	waitServiceActions(t, f, "monopanel-api.socket stop", "monopanel-api.service restart")

	g, _ := newConfigFixture(t, nil)
	g.s.listenVia.Store(viaSelf)
	g.agent.LoadState = map[string]string{"monopanel-api.socket": "not-found"}
	g.call(http.MethodPut, "/system/config", map[string]any{"restart": true}, http.StatusOK, nil)
	waitServiceActions(t, g, "monopanel-api.service restart")
}

// The Services page restarts the API the same way. Stopping the API, or
// restarting the agent, leaves the socket alone: it keeps the port.
func TestServicesPageMovesTheSocketWithTheAPI(t *testing.T) {
	f, path := newConfigFixture(t, nil)
	f.s.listenVia.Store(viaSocket)
	raw, _ := os.ReadFile(path)
	edited, _, err := config.EditPanelKeys(raw, map[string]string{config.KeyListen: fmt.Sprintf(":%d", freePort(t))})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o640); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, c := range []struct{ unit, action string }{
		{"monopanel-agent.service", "restart"},
		{"monopanel-api.service", "stop"},
		{"monopanel-api", "restart"},
	} {
		if code, _, raw := serviceCall(f, c.unit, c.action); code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", c.action, c.unit, code, raw)
		}
		if c.unit == "monopanel-api" {
			seen = append(seen, "monopanel-api.socket stop")
		}
		seen = append(seen, serviceUnit(c.unit)+" "+c.action)
		waitServiceActions(t, f, seen...)
	}
}

// A restart of the socket from the Services page, which systemd would answer
// by stopping the socket and refusing to start it while the API runs, stops
// the socket and restarts the API, whose unit brings the socket back. A stop
// of the socket leaves the API alone: it keeps its copy of the port.
func TestServicesPageRestartsTheSocketWithTheAPI(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.s.listenVia.Store(viaSocket)
	if code, _, raw := serviceCall(f, "monopanel-api.socket", "restart"); code != http.StatusOK {
		t.Fatalf("restart: %d %s", code, raw)
	}
	waitServiceActions(t, f, "monopanel-api.socket stop", "monopanel-api.service restart")
	if code, _, raw := serviceCall(f, "monopanel-api.socket", "stop"); code != http.StatusOK {
		t.Fatalf("stop: %d %s", code, raw)
	}
	waitServiceActions(t, f, "monopanel-api.socket stop", "monopanel-api.service restart", "monopanel-api.socket stop")
	if entries := serviceAudit(f, "service.restart"); len(entries) != 1 || entries[0].Target != "monopanel-api.socket" || entries[0].Details["after_answer"] != true {
		t.Fatalf("audit: %+v", entries)
	}
}

// A start that cannot listen gives up before anything of it changes the
// host: the error comes back, the local socket it made is gone again, the
// sockets systemd passed are closed (PID 1 keeps its own copy) and the agent
// heard nothing. Once the listeners exist, the start-up refresh begins with
// Serve, not before, and ends with it.
func TestListenFailsBeforeAnythingStarts(t *testing.T) {
	busy := tcpListen(t, "127.0.0.1:0")
	f := newFixture(t, func(c *config.Config) { c.Web.Listen = busy.Addr().String() })
	f.s.activation = func() ([]net.Listener, error) { return nil, nil }
	f.agent.Reset()
	if _, err := f.s.Listen(f.ctx); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a busy web.listen: %v", err)
	}
	gone := func() bool {
		_, err := os.Stat(f.s.cfg.APISocket())
		return errors.Is(err, os.ErrNotExist)
	}
	if !gone() {
		t.Fatal("api.sock stays behind")
	}

	// An unreadable key, with the socket systemd passed on web.listen.
	passed := tcpListen(t, "127.0.0.1:0")
	cert := filepath.Join(t.TempDir(), "panel.crt")
	if err := os.WriteFile(cert, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Web.Listen = passed.Addr().String()
	f.s.cfg.Web.TLSCert, f.s.cfg.Web.TLSKey = cert, t.TempDir()
	f.s.tls = newCertHolder(f.s.cfg, f.s.log)
	f.s.activation = func() ([]net.Listener, error) { return []net.Listener{passed}, nil }
	if _, err := f.s.Listen(f.ctx); err == nil || !closed(passed) || !gone() {
		t.Fatalf("an unreadable key: %v, the passed socket closed %v", err, closed(passed))
	}
	if calls := f.agent.Calls(); len(calls) != 0 {
		t.Fatalf("a start that failed reached the agent: %v", calls)
	}

	f.s.cfg.Web.Listen = "127.0.0.1:0"
	f.s.cfg.Web.TLSCert, f.s.cfg.Web.TLSKey = "", ""
	f.s.tls = newCertHolder(f.s.cfg, f.s.log)
	f.s.activation = func() ([]net.Listener, error) { return nil, nil }
	ln, err := f.s.Listen(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if f.agent.Called("/v1/ping") || f.s.refreshed.Load() {
		t.Fatal("the start-up refresh began before Serve")
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- f.s.Serve(ctx, ln) }()
	deadline := time.Now().Add(20 * time.Second)
	for !f.s.refreshed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the start-up refresh did not run with Serve")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// The doctor's line on the socket of the panel's port, judged on the API's
// own host: a warning whenever the API listens on the port itself while the
// socket could hold it — enabled or not, since the API's unit wants it and
// starts it with itself —, counted in the summary, and when the API runs on
// the port the socket passed but the socket stopped since; all well on the
// socket, without one, with one the administrator masked, and on a port no
// account may bind (the socket is not even asked about); nothing before
// Serve listened.
func TestDoctorJudgesTheAPISocket(t *testing.T) {
	f := newSiteFixture(t)
	if c := f.s.apiSocketCheck(f.ctx); c != nil {
		t.Fatalf("before Serve: %+v", c)
	}
	line := func() (string, string) {
		t.Helper()
		d := f.s.doctor(f.ctx)
		for _, c := range d.Checks {
			if c.Name == "api socket" {
				return c.Status + ": " + c.Detail, d.Summary
			}
		}
		return "", d.Summary
	}
	f.s.listenVia.Store(viaSocket)
	if got, _ := line(); got != "ok: monopanel-api.socket holds the port while monopanel-api restarts" {
		t.Errorf("on the socket: %q", got)
	}
	// On the port the socket passed, which no longer listens: stopped since
	// (the Services page's stop), or restarted by hand while the API ran,
	// which systemd does not start again. The API's copy is all that holds
	// the port, until the API stops or crashes — unless the administrator
	// masked the socket, or it is gone, and the API listens itself from its
	// next start on purpose.
	for _, c := range []struct{ active, file, load, want string }{
		{"inactive", "enabled", "loaded", "warn: monopanel-api.socket passed monopanel-api the port but is inactive now (stopped, or restarted by hand while the API ran, which systemd does not start again): once the API stops or crashes nothing holds the port, and any local account can take it. mp setup brings the socket back, as does systemctl restart monopanel-api; systemctl mask monopanel-api.socket has the API listen on the port itself on purpose"},
		{"failed", "disabled", "loaded", "warn: monopanel-api.socket passed monopanel-api the port but is failed now"},
		{"inactive", "masked", "masked", "ok: monopanel-api serves on the port monopanel-api.socket passed it, and listens on it itself from its next start: monopanel-api.socket is masked, so on purpose (systemctl unmask monopanel-api.socket, then mp setup, brings the socket back)"},
		{"inactive", "", "not-found", "ok: monopanel-api serves on the port monopanel-api.socket passed it, and listens on it itself from its next start: there is no monopanel-api.socket any more"},
		{"active", "masked", "masked", "ok: monopanel-api.socket holds the port while monopanel-api restarts"},
	} {
		f.agent.ActiveState = map[string]string{"monopanel-api.socket": c.active}
		f.agent.UnitFileState = map[string]string{"monopanel-api.socket": c.file}
		f.agent.LoadState = map[string]string{"monopanel-api.socket": c.load}
		if got, _ := line(); !strings.HasPrefix(got, c.want) {
			t.Errorf("on the socket, the socket %s (%s, %s):\n%q\nwant\n%q", c.active, c.file, c.load, got, c.want)
		}
	}
	f.agent.ActiveState = map[string]string{"monopanel-api.socket": "inactive"}
	f.agent.UnitFileState, f.agent.LoadState = nil, nil
	f.agent.Fail["/v1/service"] = "Failed to connect to bus"
	if c := f.s.apiSocketCheck(f.ctx); c == nil || c.Status != "ok" || c.Detail != "monopanel-api.socket holds the port while monopanel-api restarts" {
		t.Errorf("on the socket, the agent cannot tell: %+v", c)
	}
	delete(f.agent.Fail, "/v1/service")
	f.agent.ActiveState = nil
	f.s.listenVia.Store(viaSelf)
	f.s.listenAddr.Store(&net.TCPAddr{IP: net.IPv4zero, Port: 8443})
	// The fake's units are enabled unless told otherwise.
	got, summary := line()
	if !strings.HasPrefix(got, "warn: monopanel-api.socket is enabled, but monopanel-api listens on the port itself") {
		t.Errorf("the socket enabled, the API on its own: %q", got)
	}
	f.s.listenVia.Store(viaSocket)
	if _, onSocket := line(); summary == onSocket {
		t.Errorf("the warning is not counted: %q, on the socket %q", summary, onSocket)
	}
	f.s.listenVia.Store(viaSelf)
	// Every state systemd gives a unit file (systemctl is-enabled), and the
	// load states of a socket it could not load: only a masked socket or
	// none at all leaves the port to the API on purpose.
	const advice = ", and while the API restarts any local account can take the port. See journalctl -u monopanel-api -u monopanel-api.socket for why; mp setup brings the socket back, and systemctl mask monopanel-api.socket has the API listen on the port itself on purpose"
	const wanted = " (monopanel-api.service wants it, so it starts with the API, enabled or not)"
	for _, c := range []struct{ file, load, want string }{
		{"enabled", "loaded", "warn: monopanel-api.socket is enabled, but monopanel-api listens on the port itself: the socket failed, was stopped or listens elsewhere than web.listen" + advice},
		{"enabled-runtime", "loaded", "warn: monopanel-api.socket is enabled-runtime, but monopanel-api listens on the port itself: the socket failed, was stopped or listens elsewhere than web.listen" + advice},
		{"disabled", "loaded", "warn: monopanel-api.socket is disabled, but monopanel-api listens on the port itself: the socket failed, was stopped or listens elsewhere than web.listen" + wanted + advice},
		{"static", "loaded", "warn: monopanel-api.socket is static, but monopanel-api listens on the port itself: the socket failed, was stopped or listens elsewhere than web.listen" + wanted + advice},
		{"indirect", "loaded", "warn: monopanel-api.socket is indirect, but"},
		{"linked", "loaded", "warn: monopanel-api.socket is linked, but"},
		{"linked-runtime", "loaded", "warn: monopanel-api.socket is linked-runtime, but"},
		{"alias", "loaded", "warn: monopanel-api.socket is alias, but"},
		{"generated", "loaded", "warn: monopanel-api.socket is generated, but"},
		{"transient", "loaded", "warn: monopanel-api.socket is transient, but"},
		{"bad", "bad-setting", "warn: monopanel-api.socket is bad-setting, but monopanel-api listens on the port itself: the socket failed, was stopped or listens elsewhere than web.listen" + wanted + advice},
		{"", "error", "warn: monopanel-api.socket is error, but"},
		{"", "loaded", "warn: monopanel-api.socket is in no unit file state, but"},
		{"masked", "masked", "ok: monopanel-api listens on the port itself: monopanel-api.socket is masked, so it does on purpose (systemctl unmask monopanel-api.socket, then mp setup, brings the socket back)"},
		{"masked-runtime", "masked", "ok: monopanel-api listens on the port itself: monopanel-api.socket is masked, so it does on purpose"},
		{"", "not-found", "ok: monopanel-api listens on the port itself: there is no monopanel-api.socket"},
	} {
		f.agent.UnitFileState = map[string]string{"monopanel-api.socket": c.file}
		f.agent.LoadState = map[string]string{"monopanel-api.socket": c.load}
		if got, _ := line(); !strings.HasPrefix(got, c.want) || strings.HasPrefix(c.want, "warn") != strings.HasSuffix(got, advice) {
			t.Errorf("%s (%s):\n%q\nwant\n%q", c.file, c.load, got, c.want)
		}
	}
	f.agent.LoadState = nil
	f.agent.UnitFileState = nil
	f.s.listenAddr.Store(&net.TCPAddr{IP: net.IPv4zero, Port: 443})
	f.agent.Reset()
	if c := f.s.apiSocketCheck(f.ctx); c == nil || c.Status != "ok" || !strings.HasPrefix(c.Detail, "monopanel-api listens on port 443 itself, below net.ipv4.ip_unprivileged_port_start (1024)") || f.agent.Called("/v1/service") {
		t.Errorf("a privileged port: %+v", c)
	}
	f.s.listenAddr.Store(&net.TCPAddr{IP: net.IPv4zero, Port: 8443})
	f.agent.Fail["/v1/service"] = "Failed to connect to bus"
	if c := f.s.apiSocketCheck(f.ctx); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "the state of monopanel-api.socket is unknown") {
		t.Errorf("the agent cannot tell: %+v", c)
	}
}
