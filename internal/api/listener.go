package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/systemd"
)

// Where the HTTPS listener came from, once Serve has it (Server.listenVia).
const (
	viaNone   int32 = iota // Serve has not run (tests)
	viaSocket              // the socket systemd passed: monopanel-api.socket
	viaSelf                // a listener of this process's own
)

// listenViaName is listenVia as the system status names it: the doctor
// warns when the socket could hold the port but the API listens on its own
// (apiSocketCheck).
func (s *Server) listenViaName() string {
	switch s.listenVia.Load() {
	case viaSocket:
		return apitypes.ListenViaSocket
	case viaSelf:
		return apitypes.ListenViaItself
	}
	return ""
}

// apiSocketCheck is the doctor's word on the socket that keeps the panel's
// port bound while the API restarts. A warning whenever the API listens on
// the port itself while the socket could hold it — the socket failed or was
// stopped (the API unit stops it before a binary of 0.8.19 or older, as
// after a self-update that rolled back), or it listens elsewhere than
// web.listen — so that the port is free for any local account during a
// restart. That holds for a disabled socket too: monopanel-api.service wants
// it, and so starts it with itself, enabled or not. All is well on the
// socket, without one (not-found), with one the administrator masked (the
// way to have the API listen itself on purpose: systemd then starts it for
// nobody), and for a port below ip_unprivileged_port_start, which the socket
// leaves to the API on purpose: no account can take it. On the socket's
// port, the socket must still be listening: stopped since — a stop leaves
// the API running on its copy of the port (the Services page's stop does
// just that), and so does a restart of it by hand, which systemd does not
// start again while the API runs — it holds nothing once the API stops (the
// port stays free) or crashes (free until the restart, RestartSec), which is
// a warning too, but for a socket that is masked or gone. It is the API's
// line, judged on its own host — listener, web.listen, sysctl, the socket's
// state — so the summary, --json, the dashboard and the TUI carry it, and mp
// --server judges the right host; nil before Serve listened (tests).
func (s *Server) apiSocketCheck(ctx context.Context) *apitypes.Check {
	const name = "api socket"
	unit := s.cfg.APISocketUnit()
	var c apitypes.Check
	switch s.listenVia.Load() {
	case viaNone:
		return nil
	case viaSocket:
		c = check(name, "ok", unit+" holds the port while monopanel-api restarts")
		// A state the agent cannot tell leaves the line as it is: the API
		// did get the port from the socket.
		if res, err := s.agent.Service(ctx, unit, "status"); err == nil && res.Status.ActiveState != "active" {
			const next = "monopanel-api serves on the port %s passed it, and listens on it itself from its next start: "
			switch st := res.Status; {
			case st.LoadState == "not-found":
				c = check(name, "ok", fmt.Sprintf(next+"there is no %s any more", unit, unit))
			case agent.APISocketMasked(st):
				c = check(name, "ok", fmt.Sprintf(next+"%s is masked, so on purpose (systemctl unmask %s, then mp setup, brings the socket back)", unit, unit, unit))
			default:
				c = check(name, "warn", fmt.Sprintf("%s passed monopanel-api the port but is %s now (stopped, or restarted by hand while the API ran, which systemd does not start again): once the API stops or crashes nothing holds the port, and any local account can take it. mp setup brings the socket back, as does systemctl restart monopanel-api; systemctl mask %s has the API listen on the port itself on purpose",
					unit, st.ActiveState, unit))
			}
		}
		return &c
	}
	port := 0
	if a, ok := s.listenAddr.Load().(*net.TCPAddr); ok {
		port = a.Port
	} else if _, p, err := net.SplitHostPort(s.cfg.Web.Listen); err == nil {
		port, _ = strconv.Atoi(p)
	}
	if start := s.portStart(); port > 0 && port < start {
		c = check(name, "ok", fmt.Sprintf("monopanel-api listens on port %d itself, below net.ipv4.ip_unprivileged_port_start (%d): no account can take it while the API restarts", port, start))
		return &c
	}
	res, err := s.agent.Service(ctx, unit, "status")
	if err != nil {
		c = check(name, "warn", "monopanel-api listens on the port itself, and the state of "+unit+" is unknown: "+err.Error())
		return &c
	}
	switch st := res.Status; {
	case st.LoadState == "not-found":
		c = check(name, "ok", "monopanel-api listens on the port itself: there is no "+unit)
	case agent.APISocketMasked(st):
		c = check(name, "ok", fmt.Sprintf("monopanel-api listens on the port itself: %s is masked, so it does on purpose (systemctl unmask %s, then mp setup, brings the socket back)", unit, unit))
	default:
		c = check(name, "warn", apiSocketWarning(unit, st))
	}
	return &c
}

// apiSocketWarning is the doctor's warning on a socket that could hold the
// port while the API listens on it itself, whatever the state of its unit
// file (enabled, disabled, static, linked …), or its load state when systemd
// could not load it (bad-setting, error): where to see why, the repair, and
// the way to the API's own listener on purpose.
func apiSocketWarning(unit string, st systemd.Status) string {
	state := st.UnitFileState
	if st.LoadState != "" && st.LoadState != "loaded" {
		state = st.LoadState
	}
	if state == "" {
		state = "in no unit file state"
	}
	why := "the socket failed, was stopped or listens elsewhere than web.listen"
	if st.UnitFileState != "enabled" && st.UnitFileState != "enabled-runtime" {
		why += " (monopanel-api.service wants it, so it starts with the API, enabled or not)"
	}
	return fmt.Sprintf("%s is %s, but monopanel-api listens on the port itself: %s, and while the API restarts any local account can take the port. See journalctl -u monopanel-api -u %s for why; mp setup brings the socket back, and systemctl mask %s has the API listen on the port itself on purpose",
		unit, state, why, unit, unit)
}

// httpsListener is where the panel serves HTTPS. A socket systemd passed
// (monopanel-api.socket, which PID 1 keeps bound while this process
// restarts, so that no local account can take the port in between and pose
// as the panel) is taken when it is where web.listen says: the address
// ResolveAPISocket gives — every address for a web.listen on every address,
// else that very one, on the same port. Any other is closed with a warning,
// and the panel listens on web.listen itself, as without systemd:
// config.yaml wins. onSocket tells which it was.
func (s *Server) httpsListener(ctx context.Context, passed []net.Listener) (ln net.Listener, onSocket bool, err error) {
	var (
		want     agent.APISocketAddr
		werr     error
		resolved bool
		closed   bool
	)
	unit, listen := s.cfg.APISocketUnit(), s.cfg.Web.Listen
	for _, p := range passed {
		if p == nil {
			continue
		}
		if !resolved {
			want, werr = agent.ResolveAPISocket(ctx, listen, nil)
			resolved = true
		}
		why := "it listens elsewhere than web.listen of config.yaml"
		switch {
		case werr != nil:
			why = "web.listen of config.yaml is no address for it: " + werr.Error()
		case want.Holds(p.Addr()) && ln == nil:
			ln = p
			continue
		case want.Holds(p.Addr()):
			why = "a second socket on web.listen, of which the panel takes one"
		}
		s.log.Warn("closing a socket systemd passed: "+why, "socket", unit, "socket_address", p.Addr().String(), "web.listen", listen)
		_ = p.Close()
		closed = true
	}
	if ln != nil {
		return ln, true, nil
	}
	if closed {
		s.log.Warn("monopanel-api listens on web.listen itself, and the port is free while it restarts: run mp setup, or mp config set web.listen, to move the socket there", "socket", unit, "web.listen", listen)
	}
	ln, err = net.Listen("tcp", listen)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) && closed && s.stopAPISocket != nil {
		// The socket still holds what it listens on, and web.listen on the
		// same port collides with it: the socket on every address and
		// web.listen on one of them, or the other way round — web.listen
		// edited by hand, or a drop-in that could not follow it. Left so,
		// the panel would fail here at every start, after a reboot as well,
		// until somebody moved the socket. config.yaml wins: the socket is
		// stopped, which closes PID 1's copy of the port, and the panel
		// listens itself. Its unit wants the socket, so the next start of
		// the API brings it back on the old address, and this repeats until
		// mp setup gives it web.listen.
		s.log.Warn("stopping the socket systemd passed: it holds the port of web.listen elsewhere, and the panel could not listen there", "socket", unit, "web.listen", listen)
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		serr := s.stopAPISocket(sctx)
		cancel()
		if serr != nil {
			s.log.Warn("stopping the socket systemd passed", "socket", unit, "err", serr)
		}
		for try := 0; serr == nil && try < 5; try++ {
			if ln, err = net.Listen("tcp", listen); err == nil || !errors.Is(err, syscall.EADDRINUSE) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err != nil && errors.Is(err, syscall.EADDRINUSE) && closed {
		// The socket still holds what it listens on: a web.listen on the
		// same port collides with it.
		err = fmt.Errorf("%w (%s holds the port elsewhere: run mp setup to move it to web.listen)", err, unit)
	}
	return ln, false, err
}

// v6Only tells whether a listener on the IPv6 wildcard leaves IPv4 out
// (IPV6_V6ONLY). Go never binds one so, and BindIPv6Only=both of the
// socket unit neither, but a socket systemd made under other settings may be
// one: 127.0.0.1 on the port is then not this listener but anyone's.
func v6Only(ln net.Listener) bool {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return false
	}
	if a, ok := tl.Addr().(*net.TCPAddr); !ok || a.IP.To4() != nil || !a.IP.IsUnspecified() {
		return false
	}
	rc, err := tl.SyscallConn()
	if err != nil {
		return false
	}
	only := false
	_ = rc.Control(func(fd uintptr) {
		v, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
		only = err == nil && v == 1
	})
	return only
}

// heldCopy is a second descriptor of the listener's socket, which keeps the
// port bound after the listener itself is closed.
func heldCopy(ln net.Listener) (*os.File, error) {
	f, ok := ln.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return f.File()
}

// holdPort keeps the port of the HTTPS listener bound after the listener is
// closed, and returns what lets it go. Shutdown closes the listener first
// and then waits for the requests in flight (an open event stream keeps it
// the full ten seconds): a port closed that early would be anybody's while
// the socket moves to a new address, and wherever there is no socket. With
// the socket listening this changes nothing: PID 1 holds a copy of its own.
// Serve lets go when it is done; the daemon not before the process ends
// (KeepPortUntilExit), since after Serve it waits for the jobs in flight.
func (s *Server) holdPort(ln net.Listener) (release func()) {
	held, err := heldCopy(ln)
	if err != nil {
		return func() {}
	}
	if s.keepPort.Load() {
		s.keptPort.Store(held)
		return func() {}
	}
	return func() { held.Close() }
}
