package mailpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	milter "github.com/d--j/go-milter"
)

// testServer runs the milter on a unix socket of a temporary directory,
// with the policy of testPolicy, pickup's header table, a fake /proc/net
// (procTCP: a client 127.0.0.1:37494 of uid 1000, tenanta) and fake uid
// lookups, as postfix's own milter client would see it.
type testServer struct {
	sock, policy, checks string
	srv                  *Server
}

var testLogins = map[int]string{1000: "tenanta", 1001: "tenantb", 995: "vmail", 33: "www-data", 105: "postfix", 1500: "newbie", 1600: "operator"}

// testHomes are the logins' homes: the accounts' below /var/www (newbie's
// too, an account no policy names yet), www-data's /var/www itself, an
// operator's in /home.
var testHomes = map[int]string{1000: "/var/www/tenanta", 1001: "/var/www/tenantb", 995: "/var/vmail", 33: "/var/www", 105: "/var/spool/postfix", 1500: "/var/www/newbie", 1600: "/home/operator"}

func startServer(t *testing.T, withPolicy bool, tweak ...func(*Server)) *testServer {
	t.Helper()
	dir := t.TempDir()
	ts := &testServer{sock: filepath.Join(dir, "policy.sock"), policy: filepath.Join(dir, "policy.json"), checks: filepath.Join(dir, "checks")}
	writeFile(t, ts.checks, HeaderChecksContent)
	if withPolicy {
		writeFile(t, ts.policy, policyJSON(t, testPolicy()))
	}
	ts.srv = &Server{
		Loader:     NewLoader(ts.policy, ts.checks, nil),
		Root:       fakeProc(t, procTCP, procTCP6),
		TrustPeers: true,
		LookupUID: func(uid int) (string, string, error) {
			if l, ok := testLogins[uid]; ok {
				return l, testHomes[uid], nil
			}
			return "", "", fmt.Errorf("uid %d unknown", uid)
		},
		HostAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil },
	}
	for _, f := range tweak {
		f(ts.srv)
	}
	ln, err := Listen(Listener{Network: "unix", Address: ts.sock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ts
}

// conn is how postfix met the client: an SMTP session (addr, port, the
// server's address and port, daemon name) or a local submission.
type conn struct {
	daemon     string
	addr       string
	port       uint16
	serverAddr string
	serverPort string
	hostname   string
}

var (
	pickup     = conn{daemon: PickupDaemonName, addr: "127.0.0.1", hostname: "localhost"}
	loopback   = conn{daemon: "mail.host.test", addr: "127.0.0.1", port: 37494, serverAddr: "127.0.0.1", serverPort: "25", hostname: "localhost"}
	submission = conn{daemon: "ORIGINATING", addr: "203.0.113.5", port: 50000, serverAddr: "192.0.2.10", serverPort: "587", hostname: "client.example"}
	inbound    = conn{daemon: "mail.host.test", addr: "203.0.113.5", port: 50001, serverAddr: "192.0.2.10", serverPort: "25", hostname: "mx.example"}
	hostAddr   = conn{daemon: "mail.host.test", addr: "192.0.2.10", port: 41000, serverAddr: "192.0.2.10", serverPort: "587", hostname: "mail.host.test"}
)

// message is one message of a session.
type message struct {
	auth    string
	from    string
	headers [][2]string
}

// outcome is how the milter answered: "mail" or "eoh" with the reply of a
// refusal or a tempfail, "accept-mail" for inbound mail it let go
// unchecked, "eom" for a message it let go.
type outcome struct {
	stage string
	reply string
}

func (o outcome) String() string { return o.stage + " " + o.reply }

// session runs messages through one milter connection, as postfix does.
func (ts *testServer) session(t *testing.T, c conn, msgs ...message) []outcome {
	t.Helper()
	bag := milter.NewMacroBag()
	bag.Set(milter.MacroMTAFQDN, "mail.host.test")
	bag.Set(milter.MacroDaemonName, c.daemon)
	if c.serverAddr != "" {
		bag.Set(milter.MacroDaemonAddr, c.serverAddr)
		bag.Set(milter.MacroDaemonPort, c.serverPort)
	}
	client := milter.NewClient("unix", ts.sock, milter.WithReadTimeout(5*time.Second), milter.WithWriteTimeout(5*time.Second))
	s, err := client.Session(bag)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test
	family := milter.FamilyInet
	if strings.Contains(c.addr, ":") {
		family = milter.FamilyInet6
	}
	must := func(a *milter.Action, err error) *milter.Action {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	must(s.Conn(c.hostname, family, c.port, c.addr))
	must(s.Helo(c.hostname))
	var out []outcome
	for i, m := range msgs {
		if i > 0 {
			if err := s.Abort(nil); err != nil {
				t.Fatal(err)
			}
		}
		// Postfix leaves the macro out of a session without AUTH; an empty
		// value is the same to the milter.
		bag.Set(milter.MacroAuthAuthen, m.auth)
		bag.Set(milter.MacroQueueId, fmt.Sprintf("Q%d", i))
		a := must(s.Mail(m.from, ""))
		if a.Type == milter.ActionAccept {
			out = append(out, outcome{stage: "accept-mail"})
			continue
		}
		if a.StopProcessing() {
			out = append(out, outcome{stage: "mail", reply: a.SMTPReply})
			continue
		}
		must(s.Rcpt("someone@example.org", ""))
		must(s.DataStart())
		for _, h := range m.headers {
			must(s.HeaderField(h[0], h[1], nil))
		}
		a = must(s.HeaderEnd())
		if a.StopProcessing() {
			out = append(out, outcome{stage: "eoh", reply: a.SMTPReply})
			continue
		}
		must(s.BodyChunk([]byte("body\r\n")))
		_, a, err = s.End()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, outcome{stage: "eom", reply: a.String()})
	}
	return out
}

func received(uid int) [2]string {
	return [2]string{"Received", fmt.Sprintf("by mail.host.test (Postfix, from userid %d)", uid)}
}

func from(v string) [2]string { return [2]string{"From", v} }

const refusedB = "550 5.7.1 <b-corp.test>: this server does not send mail as a domain of another account"

func wantOutcomes(t *testing.T, name string, got []outcome, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d outcomes %v, want %d", name, len(got), got, len(want))
	}
	for i := range want {
		g := got[i].String()
		if !strings.HasPrefix(g, want[i]) {
			t.Errorf("%s, message %d: %q, want %q", name, i+1, g, want[i])
		}
	}
}

func TestMilterSubmission(t *testing.T) {
	ts := startServer(t, true)
	got := ts.session(t, submission,
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("CEO <ceo@b-corp.test>")}},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("Info <info@a-corp.test>"), {"Subject", "hi"}}},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("me@gmail.com")}},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("a@a-corp.test"), from("ceo@b-corp.test")}},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("Info")}},
		message{auth: "nobody@a-corp.test", from: "nobody@a-corp.test", headers: [][2]string{from("nobody@a-corp.test")}},
	)
	wantOutcomes(t, "submission", got,
		"eoh "+refusedB,
		"eom Accept",
		"eom Accept",
		"eoh "+refusedB,
		"eoh 550 5.7.1 the From header cannot be read",
		"mail 550 5.7.1 <a-corp.test>: this server does not send mail as a domain of another account",
	)
}

func TestMilterPickup(t *testing.T) {
	ts := startServer(t, true)
	for _, tc := range []struct {
		name    string
		m       message
		outcome string
	}{
		{"account as another's domain", message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("CEO <ceo@b-corp.test>")}}, "eoh " + refusedB},
		{"account as its own domain", message{from: "noreply@site-a.test", headers: [][2]string{received(1000), from("Shop <noreply@site-a.test>")}}, "eom Accept"},
		{"account, another's envelope", message{from: "ceo@b-corp.test", headers: [][2]string{received(1000), from("info@a-corp.test")}}, "eoh " + refusedB},
		{"account's cron mail", message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("tenanta@mail.host.test (Cron Daemon)")}}, "eom Accept"},
		{"account's cron mail, Debian's", message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("root@mail.host.test (Cron Daemon)")}}, "eom Accept"},
		{"account as another login at the host name", message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("tenantb@mail.host.test")}}, "eoh 550 5.7.1 <tenantb@mail.host.test>"},
		{"root as anything", message{from: "ceo@b-corp.test", headers: [][2]string{received(0), from("ceo@b-corp.test")}}, "eom Accept"},
		{"system user as anything", message{from: "www-data@mail.host.test", headers: [][2]string{received(33), from("ceo@b-corp.test")}}, "eom Accept"},
		{"forged first header", message{from: "x@gmail.com", headers: [][2]string{from("info@a-corp.test"), received(0)}}, "eoh 550 5.7.1 <a-corp.test>"},
		{"another host's header", message{from: "x@gmail.com", headers: [][2]string{{"Received", "by other.test (Postfix, from userid 0)"}, from("ceo@b-corp.test")}}, "eoh " + refusedB},
		{"a uid without a login", message{from: "x@gmail.com", headers: [][2]string{received(4242), from("info@a-corp.test")}}, "eoh 550 5.7.1 <a-corp.test>"},
		{"unknown uid, foreign domain", message{from: "x@gmail.com", headers: [][2]string{received(4242), from("x@gmail.com")}}, "eom Accept"},
	} {
		got := ts.session(t, pickup, tc.m)
		wantOutcomes(t, tc.name, got, tc.outcome)
	}
}

// Sieve's mail is dovecot's mail user's: the account whose script sent it.
func TestMilterSieve(t *testing.T) {
	ts := startServer(t, true)
	xsieve := [2]string{"X-Sieve", "Pigeonhole Sieve 0.5.16 (09c29328)"}
	for _, tc := range []struct {
		name    string
		m       message
		outcome string
	}{
		{"redirect of another account's mail", message{from: "ceo@b-corp.test", headers: [][2]string{received(995), xsieve, {"X-Sieve-Redirected-From", "info@a-corp.test"}, from("ceo@b-corp.test")}}, "eoh " + refusedB},
		{"redirect of foreign mail", message{from: "x@gmail.com", headers: [][2]string{received(995), xsieve, {"X-Sieve-Redirected-From", "info@a-corp.test"}, from("x@gmail.com")}}, "eom Accept"},
		{"redirect of own mail", message{from: "boss@a-corp.test", headers: [][2]string{received(995), xsieve, {"X-Sieve-Redirected-From", "info@a-corp.test"}, from("boss@a-corp.test")}}, "eom Accept"},
		{"a redirect header further down", message{from: "x@gmail.com", headers: [][2]string{received(995), xsieve, {"Message-ID", "<x@y>"}, {"X-Sieve-Redirected-From", "ceo@b-corp.test"}, from("ceo@b-corp.test")}}, "eoh " + refusedB},
		{"vacation as another account", message{from: "info@a-corp.test", headers: [][2]string{received(995), xsieve, {"Message-ID", "<x@y>"}, from("ceo@b-corp.test"), {"Auto-Submitted", "auto-replied (vacation)"}}}, "eoh " + refusedB},
		{"vacation as itself", message{from: "info@a-corp.test", headers: [][2]string{received(995), xsieve, {"Message-ID", "<x@y>"}, from("info@a-corp.test"), {"Auto-Submitted", "auto-replied (vacation)"}}}, "eom Accept"},
		{"vacation, null sender", message{from: "", headers: [][2]string{received(995), xsieve, {"Message-ID", "<x@y>"}, from("info@a-corp.test"), {"Auto-Submitted", "auto-replied (vacation)"}}}, "eoh 550 5.7.1 <a-corp.test>"},
		{"notification as another account", message{from: "ceo@b-corp.test", headers: [][2]string{received(995), xsieve, {"Message-ID", "<x@y>"}, from("ceo@b-corp.test"), {"Auto-Submitted", `auto-notified; owner-email="info@a-corp.test"`}}}, "eoh " + refusedB},
		{"reject report", message{from: "", headers: [][2]string{received(995), xsieve, from("Mail Delivery Subsystem <postmaster@mail.host.test>"), {"Auto-Submitted", "auto-replied (rejected)"}}}, "eom Accept"},
		// A redirect of the bounce of a redirect the policy refused: the
		// bounce goes to the forwarder, whose script forwards it again
		// (seen on mp-debian13 and mp-rocky9: without this the bounce was
		// refused and lost to a double bounce).
		{"redirect of this server's bounce", message{from: "", headers: [][2]string{received(995), xsieve, {"X-Sieve-Redirected-From", "info@a-corp.test"}, from("MAILER-DAEMON@mail.host.test (Mail Delivery System)")}}, "eom Accept"},
		{"redirect as another login at the host name", message{from: "info@a-corp.test", headers: [][2]string{received(995), xsieve, {"X-Sieve-Redirected-From", "info@a-corp.test"}, from("tenantb@mail.host.test")}}, "eoh 550 5.7.1 <tenantb@mail.host.test>"},
	} {
		got := ts.session(t, pickup, tc.m)
		wantOutcomes(t, tc.name, got, tc.outcome)
	}
}

func TestMilterLocalSMTP(t *testing.T) {
	ts := startServer(t, true)
	// procTCP: 127.0.0.1:37494 → 127.0.0.1:25 is uid 1000's (tenanta).
	got := ts.session(t, loopback,
		message{from: "ceo@b-corp.test", headers: [][2]string{from("ceo@b-corp.test")}},
		message{from: "info@a-corp.test", headers: [][2]string{from("ceo@b-corp.test")}},
		message{from: "info@a-corp.test", headers: [][2]string{from("info@a-corp.test")}},
		message{from: "tenanta", headers: [][2]string{from("tenanta@mail.host.test")}},
		message{from: "postmaster", headers: [][2]string{from("x@gmail.com")}},
	)
	wantOutcomes(t, "loopback", got,
		"mail "+refusedB,
		"eoh "+refusedB,
		"eom Accept",
		"eom Accept",
		"mail 550 5.7.1 <postmaster@mail.host.test>",
	)
	// One of the host's addresses: 192.0.2.10:41000 → :587 is uid 1001's
	// (tenantb), which may use its own domain.
	got = ts.session(t, hostAddr, message{from: "ceo@b-corp.test", headers: [][2]string{from("ceo@b-corp.test")}},
		message{from: "info@a-corp.test", headers: [][2]string{from("info@a-corp.test")}})
	wantOutcomes(t, "host address", got, "eom Accept", "mail 550 5.7.1 <a-corp.test>")
	// A local client whose socket is not in /proc/net owns nothing.
	gone := loopback
	gone.port = 40000
	got = ts.session(t, gone, message{from: "info@a-corp.test", headers: [][2]string{from("info@a-corp.test")}},
		message{from: "x@gmail.com", headers: [][2]string{from("x@gmail.com")}})
	wantOutcomes(t, "socket not found", got, "mail 550 5.7.1 <a-corp.test>", "eom Accept")
}

func TestMilterInbound(t *testing.T) {
	ts := startServer(t, true)
	got := ts.session(t, inbound, message{from: "ceo@b-corp.test", headers: [][2]string{from("ceo@b-corp.test")}})
	wantOutcomes(t, "inbound", got, "accept-mail")
}

func TestMilterWithoutPolicy(t *testing.T) {
	ts := startServer(t, false)
	got := ts.session(t, pickup,
		message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("info@a-corp.test")}},
		message{from: "root@mail.host.test", headers: [][2]string{received(0), from("ceo@b-corp.test")}})
	wantOutcomes(t, "no policy", got, "eoh 451 4.7.1", "eom Accept")
	got = ts.session(t, submission, message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: [][2]string{from("info@a-corp.test")}})
	wantOutcomes(t, "no policy, submission", got, "mail 451 4.7.1")
	got = ts.session(t, inbound, message{from: "x@gmail.com", headers: [][2]string{from("x@gmail.com")}})
	wantOutcomes(t, "no policy, inbound", got, "accept-mail")
	// The policy arrives: the next message is checked.
	writeFile(t, ts.policy, policyJSON(t, testPolicy()))
	got = ts.session(t, pickup, message{from: "tenanta@mail.host.test", headers: [][2]string{received(1000), from("info@a-corp.test")}})
	wantOutcomes(t, "policy arrived", got, "eom Accept")
}

// While pickup's header table is not the panel's, the first header of a
// local submission proves nothing.
func TestMilterUntrustedHeaderChecks(t *testing.T) {
	ts := startServer(t, true)
	writeFile(t, ts.checks, "# emptied\n")
	got := ts.session(t, pickup, message{from: "x@gmail.com", headers: [][2]string{received(0), from("ceo@b-corp.test")}})
	wantOutcomes(t, "untrusted table", got, "eoh "+refusedB)
}

// A packet over maxFrame ends the connection before it is read: after the
// negotiation, a 16 MB one, which the library itself would wait for (it
// takes up to 512 MB, and waits as long as the session may stay silent).
func TestMilterFrameLimit(t *testing.T) {
	ts := startServer(t, true)
	c, err := net.Dial("unix", ts.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // test
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	optneg := []byte{0, 0, 0, 13, 'O', 0, 0, 0, 6, 0, 0, 0x01, 0xff, 0, 0x1f, 0xff, 0xff}
	if _, err := c.Write(optneg); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("no answer to the negotiation: %v", err)
	}
	if _, err := io.ReadFull(c, make([]byte, int(head[0])<<24|int(head[1])<<16|int(head[2])<<8|int(head[3]))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0x01, 0x00, 0x00, 0x00, 'C'}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 16)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection stayed open: %v", err)
	}
}

func TestProbe(t *testing.T) {
	ts := startServer(t, true)
	if err := Probe(context.Background(), Listener{Network: "unix", Address: ts.sock}, 3*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := Probe(context.Background(), Listener{Network: "unix", Address: filepath.Join(t.TempDir(), "none.sock")}, time.Second); err == nil {
		t.Fatal("probe of no milter passed")
	}
}

// Only root and postfix talk to the policy: a connection of anybody else is
// closed before a word (on EL, where the port is open to every account).
func TestMilterPeerCheck(t *testing.T) {
	me := os.Getuid()
	if me == 0 {
		t.Skip("runs as root, who may always talk to the policy")
	}
	asPostfix := startServer(t, true, func(s *Server) {
		s.TrustPeers = false
		s.LookupLogin = func(string) (int, error) { return me, nil }
	})
	if err := Probe(context.Background(), Listener{Network: "unix", Address: asPostfix.sock}, 3*time.Second); err != nil {
		t.Fatalf("postfix's connection: %v", err)
	}
	stranger := startServer(t, true, func(s *Server) {
		s.TrustPeers = false
		s.LookupLogin = func(string) (int, error) { return me + 1, nil }
	})
	if err := Probe(context.Background(), Listener{Network: "unix", Address: stranger.sock}, 3*time.Second); err == nil {
		t.Fatal("a connection of neither root nor postfix was answered")
	}
}

// A message of many From headers — postfix limits the size of a header,
// not their number — cannot be read past MaxFromHeaders headers or
// MaxFromAddresses addresses, and the milter keeps no more of it: one
// message of 300 From headers of 100 kB each killed the milter on
// mp-rocky9 (MemoryMax=256M), and on EL its port was free then (review r1
// F3, r3 F5).
func TestMilterFromHeadersBounded(t *testing.T) {
	ts := startServer(t, true)
	many := make([][2]string, 0, MaxFromHeaders+1)
	for i := 0; i <= MaxFromHeaders; i++ {
		many = append(many, from("info@a-corp.test"))
	}
	var distinct strings.Builder
	for i := 0; i < MaxHeaderAddresses; i++ {
		fmt.Fprintf(&distinct, "x@d%d.test, ", i)
	}
	spread := [][2]string{from(distinct.String()), from(strings.ReplaceAll(distinct.String(), "x@d", "y@e"))}
	got := ts.session(t, submission,
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: many},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: spread},
		message{auth: "info@a-corp.test", from: "info@a-corp.test", headers: many[:MaxFromHeaders]},
	)
	wantOutcomes(t, "many From headers", got,
		"eoh 550 5.7.1 the From header cannot be read",
		"eoh 550 5.7.1 the From header cannot be read",
		"eom Accept")
	// root's mail is not read at all: it goes whatever its From headers.
	got = ts.session(t, pickup, message{from: "root@mail.host.test", headers: append([][2]string{received(0)}, many...)})
	wantOutcomes(t, "root's many From headers", got, "eom Accept")

	// What one session keeps of 300 From headers of 100 kB.
	value := strings.Repeat("@a.b ", 100*1024/5)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := &session{srv: &Server{}}
	for i := 0; i < 300; i++ {
		s.addFrom(value)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 1<<20 || len(s.from) > MaxFromAddresses || !s.fromBad {
		t.Errorf("300 From headers of 100 kB: %d bytes kept, %d addresses, unreadable %v", grown, len(s.from), s.fromBad)
	}
	runtime.KeepAlive(s)
}

// A user whose home lies below the accounts' homes is an account even
// before a policy names it — a new one, or one whose write failed — and
// owns nothing then; any other login is a system user, free (review r1
// F10, r3 F6).
func TestMilterAccountNotNamedYet(t *testing.T) {
	ts := startServer(t, true)
	got := ts.session(t, pickup,
		message{from: "newbie@mail.host.test", headers: [][2]string{received(1500), from("info@a-corp.test")}},
		message{from: "newbie@mail.host.test", headers: [][2]string{received(1500), from("ceo@b-corp.test")}},
		message{from: "newbie@mail.host.test", headers: [][2]string{received(1500), from("x@gmail.com")}},
		message{from: "newbie@mail.host.test", headers: [][2]string{received(1500), from("newbie@mail.host.test")}},
		message{from: "operator@mail.host.test", headers: [][2]string{received(1600), from("ceo@b-corp.test")}},
	)
	wantOutcomes(t, "an account no policy names", got,
		"eoh 550 5.7.1 <a-corp.test>",
		"eoh "+refusedB,
		"eom Accept",
		"eom Accept", // its own login at the host name: its cron's mail
		"eom Accept")
}

// On EL the policy listens on a loopback port: the peer of a connection is
// told by /proc/net/tcp, and only root's and postfix's are answered — an
// account's, or an orphaned socket's (uid 0, inode 0), is reset before a
// word (review r3 F11: the TCP branch had no test).
func TestMilterTCPPeerCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := netip.MustParseAddrPort(ln.Addr().String())
	dir := t.TempDir()
	policy, checks := filepath.Join(dir, "policy.json"), filepath.Join(dir, "checks")
	writeFile(t, checks, HeaderChecksContent)
	writeFile(t, policy, policyJSON(t, testPolicy()))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Loader:      NewLoader(policy, checks, nil),
		Root:        root,
		LookupLogin: func(string) (int, error) { return 105, nil },
		LookupUID:   func(int) (string, string, error) { return "", "", errors.New("none") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	defer func() {
		cancel()
		<-done
	}()
	hexAddr := func(ap netip.AddrPort) string {
		b := ap.Addr().As4()
		return fmt.Sprintf("%02X%02X%02X%02X:%04X", b[3], b[2], b[1], b[0], ap.Port())
	}
	for _, tc := range []struct {
		name       string
		uid, inode int
		answered   bool
	}{
		{"postfix", 105, 4711, true},
		{"root", 0, 4712, true},
		{"an account", 1000, 4713, false},
		{"an orphaned socket", 0, 0, false},
	} {
		// The client's port is known before it connects, so that its line
		// is in the fake /proc/net/tcp when the milter looks.
		free, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client := netip.MustParseAddrPort(free.Addr().String())
		_ = free.Close()
		line := fmt.Sprintf("   1: %s %s 01 00000000:00000000 00:00000000 00000000 %5d        0 %d 1 0000000000000000 20 4 30 10 -1\n", hexAddr(client), hexAddr(server), tc.uid, tc.inode)
		if err := os.WriteFile(filepath.Join(root, "proc", "net", "tcp"), []byte(procTCPHead+line), 0o644); err != nil {
			t.Fatal(err)
		}
		dialer := &net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(client), Timeout: 3 * time.Second}
		c := milter.NewClient("tcp", ln.Addr().String(), milter.WithDialer(dialer), milter.WithReadTimeout(3*time.Second), milter.WithWriteTimeout(3*time.Second))
		s, err := c.Session(nil)
		if err == nil {
			_ = s.Close()
		}
		if (err == nil) != tc.answered {
			t.Errorf("%s (uid %d, inode %d): answered %v, want %v", tc.name, tc.uid, tc.inode, err == nil, tc.answered)
		}
	}
}

// A peer that connects and says nothing is closed after the negotiation's
// time, also with the peer check on: the check must not clear the deadline
// the library set for that first read.
func TestMilterSilentPeerTimesOut(t *testing.T) {
	me := os.Getuid()
	ts := startServer(t, true, func(s *Server) {
		s.TrustPeers = false
		s.LookupLogin = func(string) (int, error) { return me, nil }
	})
	c, err := net.Dial("unix", ts.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // test
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	if errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > 8*time.Second {
		t.Fatalf("a silent peer is still connected after %v: %v", time.Since(start), err)
	}
}
