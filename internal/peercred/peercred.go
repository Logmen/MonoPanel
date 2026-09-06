// Package peercred reads SO_PEERCRED on unix sockets and tags accepted
// connections with the caller's uid/gid/pid. Both the agent socket and the
// API's local CLI socket rely on it for authentication.
package peercred

import (
	"context"
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// Cred is the peer's identity.
type Cred struct {
	UID uint32
	GID uint32
	PID int32
}

// Get returns the peer credentials of a unix socket connection.
func Get(conn net.Conn) (*Cred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("peercred: not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var ucred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		ucred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, cerr
	}
	return &Cred{UID: ucred.Uid, GID: ucred.Gid, PID: ucred.Pid}, nil
}

type ctxKey struct{}

// FromContext returns the credentials stored by ConnContext.
func FromContext(ctx context.Context) (*Cred, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Cred)
	return c, ok
}

// WithCred attaches credentials to a context.
func WithCred(ctx context.Context, c *Cred) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Listener wraps a unix listener: peers failing Allow are dropped, accepted
// connections carry their credentials.
type Listener struct {
	net.Listener
	Allow func(*Cred) bool
}

// Conn is an accepted connection with credentials.
type Conn struct {
	net.Conn
	Cred *Cred
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cred, err := Get(c)
		if err != nil || (l.Allow != nil && !l.Allow(cred)) {
			c.Close() //nolint:errcheck // cleanup
			continue
		}
		return &Conn{Conn: c, Cred: cred}, nil
	}
}

// ConnContext is for http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if cc, ok := c.(*Conn); ok {
		return WithCred(ctx, cc.Cred)
	}
	return ctx
}
