package mailpolicy

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postfix asks the policy only where main.cf's milter lists begin with it:
// a main.cf that lost it — a removal of the package and an install again,
// an older version's mp mail apply — lets mail go unchecked (review r2 F1).
func TestMainCFAsks(t *testing.T) {
	deb, el := ListenerFor(true), ListenerFor(false)
	full := "# x\nsmtpd_milters = " + deb.MilterToken() + ", local:/run/opendkim/opendkim.sock\nnon_smtpd_milters = " + deb.MilterToken() + ", local:/run/opendkim/opendkim.sock\n"
	for _, tc := range []struct {
		name, content string
		l             Listener
		want          bool
	}{
		{"the panel's", full, deb, true},
		{"the other family's", full, el, false},
		{"EL", "smtpd_milters = " + el.MilterToken() + ", inet:127.0.0.1:8891\nnon_smtpd_milters = " + el.MilterToken() + "\n", el, true},
		{"opendkim alone", "smtpd_milters = local:/run/opendkim/opendkim.sock\nnon_smtpd_milters = local:/run/opendkim/opendkim.sock\n", deb, false},
		{"one list only", "smtpd_milters = " + deb.MilterToken() + "\nnon_smtpd_milters = local:/run/opendkim/opendkim.sock\n", deb, false},
		{"not first", "smtpd_milters = local:/x, " + deb.MilterToken() + "\nnon_smtpd_milters = " + deb.MilterToken() + "\n", deb, false},
		{"not set", "myhostname = x\n", deb, false},
		{"a later assignment wins", full + "smtpd_milters =\n", deb, false},
		{"continued on the next line", "smtpd_milters =\n  " + deb.MilterToken() + ",\n  local:/x\nnon_smtpd_milters = " + deb.MilterToken() + "\n", deb, true},
		{"commented out", "#smtpd_milters = " + deb.MilterToken() + "\nnon_smtpd_milters = " + deb.MilterToken() + "\n", deb, false},
	} {
		got, why := MainCFAsks(tc.content, tc.l)
		if got != tc.want || (!got && why == "") {
			t.Errorf("%s: %v (%s), want %v", tc.name, got, why, tc.want)
		}
	}
}

// What listens where postfix reaches the policy must be the panel's: on EL
// the port's socket root's (PID 1's) — an account that took the port while
// nothing held it would get postfix's connections, and its answer to the
// probe would prove nothing (reviews r1 F2, r2 F4/F10, r3 F2) —, on Debian
// postfix's own 0600 socket.
func TestCheckListener(t *testing.T) {
	el := ListenerFor(false)
	for _, tc := range []struct {
		name, tcp string
		ok        bool
	}{
		{"systemd's", "   0: 0100007F:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3001 1 0000000000000000 100 0 0 10 0\n", true},
		{"the policy's own (a run by hand)", "   0: 0100007F:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000   983        0 3001 1 0000000000000000 100 0 0 10 0\n", true},
		{"an account's", "   0: 0100007F:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3001 1 0000000000000000 100 0 0 10 0\n", false},
		{"an account's on the wildcard", "   0: 0100007F:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3001 1 0000000000000000 100 0 0 10 0\n" +
			"   1: 00000000:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3002 1 0000000000000000 100 0 0 10 0\n", false},
		{"nobody's", "", false},
		{"a client's, not a listener", "   0: 0100007F:9276 0100007F:22BA 01 00000000:00000000 00:00000000 00000000     0        0 3001 1 0000000000000000 20 4 30 10 -1\n", false},
	} {
		err := CheckListener(fakeProc(t, tc.tcp, ""), el, 983, 89)
		if (err == nil) != tc.ok {
			t.Errorf("EL, %s: %v, want ok=%v", tc.name, err, tc.ok)
		}
	}

	root := t.TempDir()
	deb := ListenerFor(true)
	sock := filepath.Join(root, deb.Address)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckListener(root, deb, 983, os.Getuid()); err == nil || !strings.Contains(err.Error(), "no socket") {
		t.Errorf("Debian, no socket: %v", err)
	}
	// A unix socket's path is at most 107 bytes: it is bound from its
	// directory.
	t.Chdir(filepath.Dir(sock))
	ln, err := net.Listen("unix", filepath.Base(sock))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckListener(root, deb, 983, os.Getuid()); err != nil {
		t.Errorf("Debian, postfix's own socket: %v", err)
	}
	if err := CheckListener(root, deb, 983, os.Getuid()+1); err == nil {
		t.Error("Debian, a socket of another owner passes")
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := CheckListener(root, deb, 983, os.Getuid()); err == nil {
		t.Error("Debian, a socket others may open passes")
	}
}

// The guard resets the connections of everyone but root and postfix to the
// policy's port and to opendkim's, in a table of its own, replacing itself.
func TestGuardRuleset(t *testing.T) {
	r := GuardRuleset(89, 0, -1)
	for _, want := range []string{
		"table inet monopanel_mailguard\ndelete table inet monopanel_mailguard\ntable inet monopanel_mailguard {\n",
		"type filter hook output priority 0; policy accept;",
		"ip daddr 127.0.0.0/8 tcp dport { 8890, 8891 } meta skuid != { 0, 89 } counter reject with tcp reset",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("the guard's ruleset lacks %q:\n%s", want, r)
		}
	}
	if strings.Count(r, "skuid") != 1 || strings.Contains(r, "accept\n") && strings.Contains(r, "skuid == ") {
		t.Errorf("the guard's ruleset:\n%s", r)
	}
}
