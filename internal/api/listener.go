package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent"
)

// Where the HTTPS listener came from, once Run has it (Server.listenVia).
const (
	viaNone   int32 = iota // Run has not listened (tests)
	viaSocket              // the socket systemd passed: monopanel-api.socket
	viaSelf                // a listener of this process's own
)

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
// Run lets go when it is done; the daemon not before the process ends
// (KeepPortUntilExit), since after Run it waits for the jobs in flight.
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
