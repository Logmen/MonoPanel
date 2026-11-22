package mailpolicy

import (
	"bufio"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNoSocket says that no socket of this host is the client of a
// connection: none in /proc/net, or several of different owners.
var ErrNoSocket = errors.New("no socket of this host is that connection's client")

// SocketUID is the uid that owns the client end of a TCP connection of this
// host — the socket whose local address is client and whose remote address
// is server — read from root/proc/net/tcp and tcp6 (root is "/" but in
// tests). An IPv4 client may hold an IPv6 socket (::ffff:127.0.0.1): the
// addresses are compared unmapped. An invalid server matches any remote
// end. The kernel records the uid that made the socket: passing it to
// another process changes nothing. An entry without an inode names no
// owner: the kernel prints a socket its process closed — orphaned in
// FIN_WAIT or CLOSING, or in TIME_WAIT — with uid 0 and inode 0, and a
// client that writes its whole dialog and closes at once is one such by the
// time it is looked up; it is nobody's, never root's.
func SocketUID(root string, client, server netip.AddrPort) (int, error) {
	if !client.IsValid() {
		return -1, ErrNoSocket
	}
	uid := -1
	for _, f := range []string{"proc/net/tcp", "proc/net/tcp6"} {
		fh, err := os.Open(filepath.Join(root, f))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return -1, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			e, ok := procNetLine(sc.Text())
			// 0A is LISTEN: a listening socket is nobody's client.
			if !ok || e.state == "0A" || e.inode == 0 || e.local != client || (server.IsValid() && e.remote != server) {
				continue
			}
			owner := e.uid
			if uid >= 0 && uid != owner {
				_ = fh.Close()
				return -1, ErrNoSocket
			}
			uid = owner
		}
		err = sc.Err()
		_ = fh.Close()
		if err != nil {
			return -1, err
		}
	}
	if uid < 0 {
		return -1, ErrNoSocket
	}
	return uid, nil
}

// procNetEntry is one socket of /proc/net/tcp{,6}.
type procNetEntry struct {
	local, remote netip.AddrPort
	state         string
	uid           int
	inode         uint64
}

// procNetLine reads one line of /proc/net/tcp{,6}: "sl local remote st
// tx:rx tr:when retrnsmt uid timeout inode ...".
func procNetLine(line string) (e procNetEntry, ok bool) {
	f := strings.Fields(line)
	if len(f) < 10 || f[0] == "sl" {
		return e, false
	}
	var lok, rok bool
	e.local, lok = procNetAddr(f[1])
	e.remote, rok = procNetAddr(f[2])
	n, err := strconv.Atoi(f[7])
	inode, ierr := strconv.ParseUint(f[9], 10, 64)
	if !lok || !rok || err != nil || ierr != nil || n < 0 {
		return procNetEntry{}, false
	}
	e.state, e.uid, e.inode = f[3], n, inode
	return e, true
}

// procNetAddr decodes "0100007F:0019": the address as the kernel prints it,
// each 32-bit word in host order (little-endian here), the port in hex.
func procNetAddr(s string) (netip.AddrPort, bool) {
	h, p, found := strings.Cut(s, ":")
	if !found {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i+4 <= len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}
