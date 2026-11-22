package mailpolicy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	milter "github.com/d--j/go-milter"
)

// Listen opens l itself — for a run by hand and for tests: the service takes
// its listener from systemd (FromSystemd). A unix socket left by an earlier
// run goes first; the new one is 0660 and goes when the listener is closed.
func Listen(l Listener) (net.Listener, error) {
	if l.Network != "unix" {
		return net.Listen(l.Network, l.Address)
	}
	if fi, err := os.Lstat(l.Address); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(l.Address)
	}
	ln, err := net.Listen("unix", l.Address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(l.Address, 0o660); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	return ln, nil
}

// FromSystemd is the listener systemd passed the service (LISTEN_FDS): the
// one socket of SocketUnitUnix or SocketUnitInet. PID 1 keeps it bound while
// the service restarts: the service never binds the address itself, which on
// EL any local account could take in between.
func FromSystemd() (net.Listener, error) {
	lns, err := activation.Listeners()
	if err != nil {
		return nil, fmt.Errorf("the sockets systemd passed: %w", err)
	}
	var got []net.Listener
	for _, ln := range lns {
		if ln != nil {
			got = append(got, ln)
		}
	}
	switch len(got) {
	case 1:
		return got[0], nil
	case 0:
		return nil, errors.New("systemd passed no listening socket: the service runs on its socket unit, " + SocketUnitUnix + " (Debian, Ubuntu) or " + SocketUnitInet + " (EL); for a run by hand, --listen")
	}
	for _, ln := range got {
		_ = ln.Close()
	}
	return nil, fmt.Errorf("systemd passed %d listening sockets; the policy takes one", len(got))
}

// Probe speaks the milter protocol's option negotiation to the milter at l,
// within timeout, and says goodbye: nil when it answers as a milter.
func Probe(ctx context.Context, l Listener, timeout time.Duration) error {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return errors.New("no time left to ask the mail policy")
	}
	c := milter.NewClient(l.Network, l.Address,
		milter.WithDialer(&net.Dialer{Timeout: timeout}),
		milter.WithReadTimeout(timeout),
		milter.WithWriteTimeout(timeout))
	s, err := c.Session(nil)
	if err != nil {
		return err
	}
	return s.Close()
}

// CheckListener tells whether what listens at l is the panel's, before the
// probe asks it anything: an answer from an account's process that took the
// address would say nothing about the policy. On EL the port's listening
// socket must be root's — PID 1's, which holds it for the service — or
// User's (a run by hand), and nobody else may listen there: an account
// listening there would get postfix's connections. On Debian and Ubuntu
// the socket must be postfix's own, 0600, so that only postfix and root
// may open it. root is "/" but in tests; the uids are root's (0), User's
// and postfix's (-1 when unknown).
func CheckListener(root string, l Listener, policyUID, postfixUID int) error {
	switch l.Network {
	case "unix":
		fi, err := os.Lstat(filepath.Join(root, l.Address))
		if err != nil {
			return fmt.Errorf("no socket at %s (systemctl status %s): %w", l.Address, l.Socket, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case fi.Mode()&os.ModeSocket == 0:
			return fmt.Errorf("%s is no socket", l.Address)
		case !ok || postfixUID < 0 || int(st.Uid) != postfixUID:
			return fmt.Errorf("%s is not postfix's socket (owner uid %d)", l.Address, statUID(st))
		case fi.Mode().Perm()&0o077 != 0:
			return fmt.Errorf("%s may be opened by others than postfix (mode %04o)", l.Address, fi.Mode().Perm())
		}
		return nil
	case "tcp":
		ap, err := netip.ParseAddrPort(l.Address)
		if err != nil {
			return err
		}
		uids, err := ListenerUIDs(root, ap)
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			return fmt.Errorf("nothing listens on %s (systemctl status %s)", l.Address, l.Socket)
		}
		for _, uid := range uids {
			if uid != 0 && (policyUID < 0 || uid != policyUID) {
				return fmt.Errorf("uid %d listens on %s, not systemd for the policy: a local account took the port, and postfix hands it the mail (ss -ltnpe 'sport = :%d')", uid, l.Address, ap.Port())
			}
		}
		return nil
	}
	return fmt.Errorf("a listener of the network %q", l.Network)
}

func statUID(st *syscall.Stat_t) int {
	if st == nil {
		return -1
	}
	return int(st.Uid)
}

// ListenerUIDs are the owners of the sockets that listen on addr, read from
// root/proc/net/tcp and tcp6 — a socket bound to the wildcard address on
// addr's port among them (it gets addr's connections too).
func ListenerUIDs(root string, addr netip.AddrPort) ([]int, error) {
	var uids []int
	for _, f := range []string{"proc/net/tcp", "proc/net/tcp6"} {
		fh, err := os.Open(filepath.Join(root, f))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			e, ok := procNetLine(sc.Text())
			if !ok || e.state != "0A" || e.local.Port() != addr.Port() {
				continue
			}
			if a := e.local.Addr(); a == addr.Addr() || a.IsUnspecified() {
				uids = append(uids, e.uid)
			}
		}
		err = sc.Err()
		_ = fh.Close()
		if err != nil {
			return nil, err
		}
	}
	return uids, nil
}

// MainCFFile is postfix's main.cf, which the agent's probe reads.
const MainCFFile = "/etc/postfix/main.cf"

// MainCFAsks tells whether main.cf has postfix ask the policy at l first —
// smtpd_milters and non_smtpd_milters both begin with l.MilterToken() — and
// says what is amiss when not: a main.cf that lost the policy (a removal of
// the package and an install again, an older version's mp mail apply
// after a downgrade) lets mail go unchecked.
func MainCFAsks(content string, l Listener) (bool, string) {
	values := mainCFValues(content)
	want := l.MilterToken()
	for _, key := range []string{"smtpd_milters", "non_smtpd_milters"} {
		v, ok := values[key]
		switch {
		case !ok:
			return false, key + " is not set"
		case !strings.HasPrefix(strings.Join(strings.Fields(v), " "), want):
			return false, fmt.Sprintf("%s does not begin with %s", key, want)
		}
	}
	return true, ""
}

// mainCFValues reads main.cf's parameters as postfix does: "name = value",
// a line that begins with a blank continuing the one before, comments and
// blank lines skipped, the last assignment of a name winning.
func mainCFValues(content string) map[string]string {
	out := map[string]string{}
	key := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key != "" {
				out[key] += " " + strings.TrimSpace(line)
			}
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(name)
		out[key] = strings.TrimSpace(value)
	}
	return out
}
