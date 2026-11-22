package mailpolicy

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

const procTCPHead = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// fakeProc writes proc/net/tcp and tcp6 under a temporary root.
func fakeProc(t *testing.T, tcp, tcp6 string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "proc", "net")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(procTCPHead+tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp6"), []byte(procTCPHead+tcp6), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The lines are /proc/net/tcp's own: 127.0.0.1:25 listening (root's
// master), a client 127.0.0.1:37494 → 127.0.0.1:25 of uid 1000 and the
// server's end of it (postfix, 105), a client 192.0.2.10:41000 →
// 192.0.2.10:587 of uid 1001; in tcp6 a client [::1]:45000 → [::1]:25 of
// uid 1002 and an IPv4 client in an IPv6 socket, ::ffff:127.0.0.1:46000 →
// ::ffff:127.0.0.1:25, of uid 1003.
const (
	procTCP = "   0: 0100007F:0019 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 0100007F:9276 0100007F:0019 01 00000000:00000000 00:00000000 00000000  1000        0 1002 1 0000000000000000 20 4 30 10 -1\n" +
		"   2: 0100007F:0019 0100007F:9276 01 00000000:00000000 00:00000000 00000000   105        0 1003 1 0000000000000000 20 4 30 10 -1\n" +
		"   3: 0A0200C0:A028 0A0200C0:024B 01 00000000:00000000 00:00000000 00000000  1001        0 1004 1 0000000000000000 20 4 30 10 -1\n"
	procTCP6 = "   0: 00000000000000000000000001000000:AFC8 00000000000000000000000001000000:0019 01 00000000:00000000 00:00000000 00000000  1002        0 2001 1 0000000000000000 20 4 30 10 -1\n" +
		"   1: 0000000000000000FFFF00000100007F:B3B0 0000000000000000FFFF00000100007F:0019 01 00000000:00000000 00:00000000 00000000  1003        0 2002 1 0000000000000000 20 4 30 10 -1\n"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestSocketUID(t *testing.T) {
	root := fakeProc(t, procTCP, procTCP6)
	for _, tc := range []struct {
		client, server string
		uid            int
	}{
		{"127.0.0.1:37494", "127.0.0.1:25", 1000},
		{"192.0.2.10:41000", "192.0.2.10:587", 1001},
		{"[::1]:45000", "[::1]:25", 1002},
		{"127.0.0.1:46000", "127.0.0.1:25", 1003},
	} {
		uid, err := SocketUID(root, ap(tc.client), ap(tc.server))
		if err != nil || uid != tc.uid {
			t.Errorf("SocketUID(%s → %s) = %d, %v; want %d", tc.client, tc.server, uid, err, tc.uid)
		}
	}
	// No server known: the client end alone.
	if uid, err := SocketUID(root, ap("127.0.0.1:37494"), netip.AddrPort{}); err != nil || uid != 1000 {
		t.Errorf("without the server: %d, %v", uid, err)
	}
	for _, tc := range [][2]string{
		{"127.0.0.1:37494", "127.0.0.1:587"}, // another server end
		{"127.0.0.1:25", "0.0.0.0:0"},        // the listening socket is nobody's client
		{"127.0.0.1:40000", "127.0.0.1:25"},  // no such socket
		{"203.0.113.5:40000", "127.0.0.1:25"},
	} {
		if uid, err := SocketUID(root, ap(tc[0]), ap(tc[1])); !errors.Is(err, ErrNoSocket) {
			t.Errorf("SocketUID(%s → %s) = %d, %v; want ErrNoSocket", tc[0], tc[1], uid, err)
		}
	}
}

// Two sockets with the same ends and different owners tell nothing.
func TestSocketUIDAmbiguous(t *testing.T) {
	dup := procTCP + "   9: 0100007F:9276 0100007F:0019 08 00000000:00000000 00:00000000 00000000  1001        0 1009 1 0000000000000000 20 4 30 10 -1\n"
	root := fakeProc(t, dup, "")
	if uid, err := SocketUID(root, ap("127.0.0.1:37494"), ap("127.0.0.1:25")); !errors.Is(err, ErrNoSocket) {
		t.Errorf("two owners: %d, %v", uid, err)
	}
}

// The kernel prints a socket its process closed — orphaned in FIN_WAIT1,
// FIN_WAIT2, CLOSING, or in TIME_WAIT — with uid 0 and inode 0. A client
// that writes its whole SMTP dialog and closes at once is one such by the
// time the milter looks it up (seen on mp-rocky9: "st 04 uid 0 inode 0"
// 0.2 ms after close(), and the account's MAIL FROM went as root's). Such
// an entry names no owner: it is nobody's, never root's (review r1 F7, r3
// F9).
func TestSocketUIDOrphaned(t *testing.T) {
	orphan := "   5: 0100007F:CC34 0100007F:0019 04 00000000:00000000 01:00000014 00000000     0        0 0 2 0000000000000000 20 4 30 10 -1\n" +
		"   6: 0100007F:CC3E 0100007F:0019 05 00000000:00000000 03:00001770 00000000     0        0 0 3 0000000000000000\n" +
		"   7: 0100007F:CC48 0100007F:0019 06 00000000:00000000 03:00001770 00000000     0        0 0 3 0000000000000000\n" +
		"   8: 0100007F:CC52 0100007F:0019 04 00000000:00000000 01:00000014 00000000  1000        0 1777 2 0000000000000000 20 4 30 10 -1\n"
	root := fakeProc(t, procTCP+orphan, "")
	for _, client := range []string{"127.0.0.1:52276", "127.0.0.1:52286", "127.0.0.1:52296"} {
		if uid, err := SocketUID(root, ap(client), ap("127.0.0.1:25")); !errors.Is(err, ErrNoSocket) {
			t.Errorf("SocketUID(%s → 127.0.0.1:25) = %d, %v for an orphaned socket (inode 0); want ErrNoSocket", client, uid, err)
		}
	}
	// A socket its process half-closed is still its own.
	if uid, err := SocketUID(root, ap("127.0.0.1:52306"), ap("127.0.0.1:25")); err != nil || uid != 1000 {
		t.Errorf("a half-closed socket of uid 1000: %d, %v", uid, err)
	}
}
