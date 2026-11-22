package mailpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	milter "github.com/d--j/go-milter"

	"monopanel/internal/peercred"
)

// Server is the milter.
//
// Who sends a message (its Identity), in this order:
//
//   - SMTP AUTH: {auth_authen}, the mailbox's login, is that mailbox's
//     account.
//   - Local submission (sendmail, postdrop → pickup → cleanup): the uid of
//     pickup's "Received: by <host> (Postfix, from userid <uid>)", as the
//     copy HeaderChecksRule puts first shows it, on a message whose
//     {daemon_name} is PickupDaemonName. dovecot's mail user (sieve's mail)
//     is the account of the script's owner: the redirecting mailbox dovecot
//     names right after its X-Sieve header, or the envelope sender of a
//     vacation reply (sieve_vacation_send_from_recipient).
//   - SMTP from an address of this host without AUTH: the uid of the
//     client's socket in /proc/net/tcp{,6}.
//   - SMTP from elsewhere without AUTH: inbound mail, no sender of this
//     server; it passes unchecked.
//
// A uid is a panel account by its login (os/user, /etc/passwd), and so is a
// user whose home lies below the accounts' homes that no policy names yet;
// root and any other login may send as anything. What cannot be told owns
// nothing (Nobody) and is logged.
type Server struct {
	Loader *Loader
	Log    *slog.Logger
	// Root is where proc/net is: "/" but in tests.
	Root string
	// LookupUID and LookupLogin resolve uids (to a login and its home)
	// and logins (os/user by default: /etc/passwd, no cgo).
	LookupUID   func(uid int) (login, home string, err error)
	LookupLogin func(login string) (int, error)
	// HostAddrs lists this host's addresses (net.InterfaceAddrs by
	// default): a client on one of them, or on loopback, is local.
	HostAddrs func() ([]netip.Addr, error)
	// MaxConns caps the milter connections at once (DefaultMaxConns), and
	// Idle is how long one may stay silent (DefaultIdle).
	MaxConns int
	Idle     time.Duration
	// TrustPeers turns the peer check off (tests): otherwise only root
	// and postfix may talk to the milter.
	TrustPeers bool

	addrMu  sync.Mutex
	addrs   []netip.Addr
	addrsAt time.Time
}

const (
	// DefaultMaxConns is the most milter connections at once: postfix
	// opens one per SMTP session and per local submission, and its own
	// process limit is 100 per service.
	DefaultMaxConns = 512
	// DefaultIdle outlasts smtpd_timeout (300 s), the longest a client may
	// leave postfix between two commands of one session.
	DefaultIdle = 10 * time.Minute
	// maxFrame is the largest milter packet taken: postfix sends a header
	// per packet (header_size_limit is 100 kB), and no body (OptNoBody).
	maxFrame = 1 << 20
	// MaxFromHeaders and MaxFromAddresses bound what one message's From
	// headers may name: RFC 5322 allows one From header, of a mailbox or a
	// few. A message past either cannot be read — the refusal of a sender
	// the policy checks — and the milter keeps no more of it: postfix
	// limits the size of a header, not their number, and a message of many
	// From headers would otherwise hold the milter's memory until the
	// kernel killed it.
	MaxFromHeaders   = 8
	MaxFromAddresses = 64
)

var logOnce sync.Once

// Serve answers postfix on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.defaults()
	logOnce.Do(func() {
		log := s.Log
		milter.LogWarning = func(format string, v ...any) { log.Warn("milter protocol: " + fmt.Sprintf(format, v...)) }
	})
	ms := milter.NewServer(
		milter.WithMilter(func() milter.Milter { return &session{srv: s} }),
		milter.WithProtocols(milter.OptNoHelo|milter.OptNoRcptTo|milter.OptNoData|milter.OptNoBody|milter.OptNoUnknown|
			milter.OptNoConnReply|milter.OptNoHeaderReply),
		milter.WithActions(0),
		milter.WithMacroRequest(milter.StageConnect, []milter.MacroName{milter.MacroMTAFQDN, milter.MacroDaemonName, milter.MacroDaemonAddr, milter.MacroDaemonPort}),
		milter.WithMacroRequest(milter.StageMail, []milter.MacroName{milter.MacroAuthAuthen}),
		milter.WithMacroRequest(milter.StageEOH, []milter.MacroName{milter.MacroQueueId}),
		milter.WithReadTimeout(s.Idle),
		milter.WithWriteTimeout(30*time.Second),
	)
	g := &guard{Listener: ln, srv: s, slots: make(chan struct{}, s.MaxConns)}
	errc := make(chan error, 1)
	go func() { errc <- ms.Serve(g) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ms.Shutdown(sctx); err != nil {
			_ = ms.Close()
		}
		return nil
	case err := <-errc:
		return err
	}
}

func (s *Server) defaults() {
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.Root == "" {
		s.Root = "/"
	}
	if s.LookupUID == nil {
		s.LookupUID = func(uid int) (string, string, error) {
			u, err := user.LookupId(strconv.Itoa(uid))
			if err != nil {
				return "", "", err
			}
			return u.Username, u.HomeDir, nil
		}
	}
	if s.LookupLogin == nil {
		s.LookupLogin = func(login string) (int, error) {
			u, err := user.Lookup(login)
			if err != nil {
				return -1, err
			}
			return strconv.Atoi(u.Uid)
		}
	}
	if s.HostAddrs == nil {
		s.HostAddrs = interfaceAddrs
	}
	if s.MaxConns <= 0 {
		s.MaxConns = DefaultMaxConns
	}
	if s.Idle <= 0 {
		s.Idle = DefaultIdle
	}
}

func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(as))
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap().WithZone(""))
		}
	}
	return out, nil
}

// isLocal tells an address of this host: loopback, or one of its
// interfaces' (asked again every few seconds, and on a miss after one).
func (s *Server) isLocal(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if a.IsLoopback() {
		return true
	}
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	for pass := 0; pass < 2; pass++ {
		age := time.Since(s.addrsAt)
		if s.addrs == nil || age > 5*time.Second || (pass == 1 && age > time.Second) {
			if as, err := s.HostAddrs(); err == nil {
				s.addrs, s.addrsAt = as, time.Now()
			} else {
				s.Log.Warn("mail policy: the host's addresses cannot be read", "err", err)
			}
		}
		for _, h := range s.addrs {
			if h == a {
				return true
			}
		}
	}
	return false
}

// peerAllowed lets root (the agent's probe) and postfix talk to the milter
// and nobody else. Nobody else can reach it — the socket of Debian and
// Ubuntu is postfix's own, and on EL the guard resets the connection of
// anyone else to its port —; this is the check behind those.
func (s *Server) peerAllowed(c net.Conn) bool {
	if s.TrustPeers {
		return true
	}
	uid := -1
	switch c.RemoteAddr().(type) {
	case *net.UnixAddr:
		if cred, err := peercred.Get(c); err == nil {
			uid = int(cred.UID)
		}
	case *net.TCPAddr:
		peer, perr := netip.ParseAddrPort(c.RemoteAddr().String())
		local, lerr := netip.ParseAddrPort(c.LocalAddr().String())
		if perr == nil && lerr == nil {
			uid, _ = SocketUID(s.Root, unmapPort(peer), unmapPort(local))
		}
	}
	if uid == 0 {
		return true
	}
	if pf, err := s.LookupLogin("postfix"); err == nil && uid >= 0 && uid == pf {
		return true
	}
	s.Log.Warn("mail policy: a connection that is neither postfix's nor root's was closed", "peer", c.RemoteAddr().String(), "uid", uid)
	return false
}

func unmapPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}

// guard caps the connections, has the peer of each checked, and limits the
// size of a packet.
type guard struct {
	net.Listener
	srv   *Server
	slots chan struct{}
}

// Accept hands the library each connection at once: its peer is checked
// in the connection's own goroutine, before its first packet is read
// (frameConn.allowed) — a look-up in /proc/net/tcp takes a while on a host
// with many connections, and the next connection, postfix's maybe, must
// not wait for it.
func (g *guard) Accept() (net.Conn, error) {
	g.slots <- struct{}{}
	c, err := g.Listener.Accept()
	if err != nil {
		<-g.slots
		return nil, err
	}
	return &frameConn{Conn: c, srv: g.srv, release: func() { <-g.slots }, needHeader: 4}, nil
}

// frameConn follows the milter framing (a 4-byte length, then the packet)
// in what it reads, and fails a packet over maxFrame before the library
// grows a buffer for it.
type frameConn struct {
	net.Conn
	srv        *Server
	release    func()
	once       sync.Once
	checked    sync.Once
	refused    bool
	needHeader int
	header     [4]byte
	needBody   int
}

var (
	errFrame   = errors.New("milter packet larger than the mail policy takes")
	errRefused = errors.New("a connection that is neither postfix's nor root's")
)

// allowed checks the peer once (no I/O on the connection: the deadline the
// library set for the read that asks stays). A connection refused is reset,
// not closed: a TCP one leaves no TIME_WAIT behind for the next look-up to
// read.
func (c *frameConn) allowed() bool {
	c.checked.Do(func() {
		if !c.srv.peerAllowed(c.Conn) {
			c.refused = true
			if tc, ok := c.Conn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
		}
	})
	return !c.refused
}

func (c *frameConn) Write(p []byte) (int, error) {
	if !c.allowed() {
		return 0, errRefused
	}
	return c.Conn.Write(p)
}

func (c *frameConn) Read(p []byte) (int, error) {
	if !c.allowed() {
		return 0, errRefused
	}
	n, err := c.Conn.Read(p)
	for b := p[:n]; len(b) > 0; {
		if c.needHeader > 0 {
			k := min(c.needHeader, len(b))
			copy(c.header[4-c.needHeader:], b[:k])
			c.needHeader -= k
			b = b[k:]
			if c.needHeader == 0 {
				size := int(c.header[0])<<24 | int(c.header[1])<<16 | int(c.header[2])<<8 | int(c.header[3])
				if size > maxFrame {
					return 0, errFrame
				}
				c.needBody = size
				if size == 0 {
					c.needHeader = 4
				}
			}
			continue
		}
		k := min(c.needBody, len(b))
		c.needBody -= k
		b = b[k:]
		if c.needBody == 0 {
			c.needHeader = 4
		}
	}
	return n, err
}

func (c *frameConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// session is one milter connection: one SMTP session, or one local
// submission.
type session struct {
	milter.NoOpMilter
	srv *Server

	// connection
	pickup  bool
	host    string
	client  netip.AddrPort
	server  netip.AddrPort
	local   bool
	sockUID int
	sockErr error

	// message
	ix           *Index
	checksOK     bool
	envelope     string
	null         bool
	auth         string
	id           *Identity
	queue        string
	headers      int
	pickupUID    int
	pickupSeen   bool
	sieve        bool
	redirectFrom string
	autoSeen     bool
	autoValue    string
	fromHeaders  int
	from         []Address
	fromSeen     map[Address]bool
	fromBad      bool
}

func (s *session) NewConnection(milter.Modifier) error {
	*s = session{srv: s.srv}
	return nil
}

func (s *session) Connect(host string, family string, port uint16, addr string, m milter.Modifier) (*milter.Response, error) {
	*s = session{srv: s.srv}
	s.host = m.Get(milter.MacroMTAFQDN)
	s.pickup = m.Get(milter.MacroDaemonName) == PickupDaemonName
	if s.pickup {
		return milter.RespContinue, nil
	}
	ca, perr := netip.ParseAddr(addr)
	known := perr == nil && (family == "tcp4" || family == "tcp6")
	if !known {
		// No address to tell where the client is: nobody's, should it
		// send without AUTH.
		s.local, s.sockErr = true, fmt.Errorf("an SMTP client of the %s family without an address postfix names (%q)", family, addr)
		return milter.RespContinue, nil
	}
	s.client = netip.AddrPortFrom(ca.Unmap().WithZone(""), port)
	if sa, err := netip.ParseAddr(m.Get(milter.MacroDaemonAddr)); err == nil {
		if sp, err := strconv.ParseUint(m.Get(milter.MacroDaemonPort), 10, 16); err == nil {
			s.server = netip.AddrPortFrom(sa.Unmap().WithZone(""), uint16(sp))
		}
	}
	if s.local = s.srv.isLocal(ca); s.local {
		s.sockUID, s.sockErr = SocketUID(s.srv.Root, s.client, s.server)
	}
	return milter.RespContinue, nil
}

func (s *session) resetMessage() {
	s.ix, s.checksOK, s.envelope, s.null, s.auth, s.id, s.queue = nil, false, "", false, "", nil, ""
	s.headers, s.pickupUID, s.pickupSeen, s.sieve, s.redirectFrom = 0, 0, false, false, ""
	s.autoSeen, s.autoValue, s.fromHeaders, s.from, s.fromSeen, s.fromBad = false, "", 0, nil, nil, false
}

func (s *session) MailFrom(from string, _ string, m milter.Modifier) (*milter.Response, error) {
	s.resetMessage()
	s.envelope = strings.TrimSpace(from)
	s.null = s.envelope == "" || s.envelope == "<>"
	s.auth = strings.TrimSpace(m.Get(milter.MacroAuthAuthen))
	if !s.pickup && !s.local && s.auth == "" {
		// Inbound: no sender of this server.
		return milter.RespAccept, nil
	}
	s.ix, s.checksOK = s.srv.Loader.Current()
	if s.pickup {
		return milter.RespContinue, nil
	}
	id := s.smtpIdentity()
	s.id = &id
	return s.decide(id, s.envelopeAddress(), nil, true)
}

func (s *session) Header(name, value string, _ milter.Modifier) (*milter.Response, error) {
	s.headers++
	if s.pickup {
		switch {
		case s.headers == 1:
			s.pickupUID, s.pickupSeen = pickupHeader(name, value, s.host)
		case s.headers == 2 && strings.EqualFold(name, "X-Sieve") && strings.HasPrefix(strings.TrimSpace(value), "Pigeonhole"):
			s.sieve = true
		case s.headers == 3 && s.sieve && strings.EqualFold(name, "X-Sieve-Redirected-From"):
			s.redirectFrom = strings.Trim(strings.TrimSpace(value), "<>")
		}
	}
	if !s.autoSeen && strings.EqualFold(name, "Auto-Submitted") {
		s.autoSeen, s.autoValue = true, strings.ToLower(strings.TrimSpace(value))
	}
	if strings.EqualFold(name, "From") {
		s.addFrom(value)
	}
	return milter.RespContinue, nil
}

// addFrom takes the addresses of a From header: the distinct ones, as many
// as MaxFromAddresses of MaxFromHeaders headers. Past that, or once a
// header cannot be read, the From cannot be read, and no more is kept.
func (s *session) addFrom(value string) {
	if s.fromBad {
		return
	}
	s.fromHeaders++
	addrs, ok := FromAddresses(value)
	if !ok || s.fromHeaders > MaxFromHeaders {
		s.fromBad, s.from, s.fromSeen = true, nil, nil
		return
	}
	if s.fromSeen == nil {
		s.fromSeen = map[Address]bool{}
	}
	for _, a := range addrs {
		if s.fromSeen[a] {
			continue
		}
		if len(s.from) >= MaxFromAddresses {
			s.fromBad, s.from, s.fromSeen = true, nil, nil
			return
		}
		s.fromSeen[a] = true
		s.from = append(s.from, a)
	}
}

func (s *session) Headers(m milter.Modifier) (*milter.Response, error) {
	s.queue = m.Get(milter.MacroQueueId)
	if !s.pickup {
		if s.id == nil {
			return milter.RespContinue, nil
		}
		return s.decide(*s.id, nil, s.from, !s.fromBad)
	}
	return s.decide(s.pickupIdentity(), s.envelopeAddress(), s.from, !s.fromBad)
}

func (s *session) EndOfMessage(milter.Modifier) (*milter.Response, error) {
	return milter.RespAccept, nil
}

func (s *session) Abort(milter.Modifier) error {
	s.resetMessage()
	return nil
}

// envelopeAddress is the envelope sender to check, nil for the null sender.
func (s *session) envelopeAddress() *Address {
	if s.null {
		return nil
	}
	host := ""
	if s.ix != nil {
		host = s.ix.Hostname()
	}
	a := EnvelopeAddress(s.envelope, host)
	if a.Domain == "" {
		return nil
	}
	return &a
}

// decide checks a message of id, and answers postfix: a refusal names the
// address, a sender that is not root waits (tempfail) while no policy was
// ever read.
func (s *session) decide(id Identity, envelope *Address, from []Address, readable bool) (*milter.Response, error) {
	if id.Kind == KindFree {
		return milter.RespContinue, nil
	}
	if s.ix == nil {
		return s.tempfail()
	}
	if r := Check(s.ix, id, envelope, from, readable); r != nil {
		return s.refuse(id, r)
	}
	return milter.RespContinue, nil
}

// smtpIdentity is the sender of an SMTP session: the mailbox it
// authenticated as, or the account of its socket.
func (s *session) smtpIdentity() Identity {
	if s.auth != "" {
		via := "SMTP AUTH " + s.auth
		if s.ix == nil {
			return Identity{Kind: KindNobody, Via: via}
		}
		if login := s.ix.MailboxAccount(s.auth); login != "" {
			return Identity{Kind: KindAccount, Login: login, Via: via}
		}
		s.srv.Log.Warn("mail policy: a login that is no mailbox of the policy; it may use no domain of the panel", "login", s.auth)
		return Identity{Kind: KindNobody, Via: via + ", no mailbox of the policy"}
	}
	via := "SMTP from " + s.client.String()
	if s.sockErr != nil {
		s.srv.Log.Warn("mail policy: the local SMTP client's socket cannot be found; it may use no domain of the panel", "client", s.client.String(), "server", s.server.String(), "err", s.sockErr)
		return Identity{Kind: KindNobody, Local: true, Via: via + ": " + s.sockErr.Error()}
	}
	return s.uidIdentity(s.sockUID, via)
}

// pickupIdentity is the sender of a local submission.
func (s *session) pickupIdentity() Identity {
	if !s.checksOK {
		return Identity{Kind: KindNobody, Local: true, Via: "sendmail; " + HeaderChecksFile + " is not the panel's table"}
	}
	if !s.pickupSeen {
		s.srv.Log.Warn("mail policy: a local submission without pickup's Received header first; it may use no domain of the panel", "queue", s.queue)
		return Identity{Kind: KindNobody, Local: true, Via: "sendmail without pickup's Received header"}
	}
	id := s.uidIdentity(s.pickupUID, "sendmail")
	if !id.Delivery {
		return id
	}
	// dovecot's mail user: the account whose sieve script sent it. A
	// redirect relays what came in, this server's own bounces among it —
	// those of a redirect the policy refused, which go to the forwarder
	// (sieve_redirect_envelope_from = recipient) and are redirected again
	// by a script that forwards everything: postmaster@ and mailer-daemon@
	// the host name are its as they are the delivery user's, or the bounce
	// would be lost.
	switch {
	case s.redirectFrom != "":
		if login := s.ix.AddressAccount(s.redirectFrom); login != "" {
			return Identity{Kind: KindAccount, Login: login, Delivery: true, Via: "sieve redirect of " + s.redirectFrom}
		}
		return Identity{Kind: KindNobody, Login: id.Login, Via: "sieve redirect of " + s.redirectFrom + ", no account's address"}
	case strings.HasPrefix(s.autoValue, "auto-replied") && !s.null:
		if login := s.ix.AddressAccount(s.envelope); login != "" {
			return Identity{Kind: KindAccount, Login: login, Via: "sieve auto-reply of " + s.envelope}
		}
	}
	return id
}

// uidIdentity is the sender a uid is. Without a policy only root can be
// told.
func (s *session) uidIdentity(uid int, via string) Identity {
	via = fmt.Sprintf("%s, uid %d", via, uid)
	if uid == 0 {
		return Identity{Kind: KindFree, Login: "root", Local: true, Via: via}
	}
	if s.ix == nil {
		return Identity{Kind: KindNobody, Local: true, Via: via}
	}
	login, home, err := s.srv.LookupUID(uid)
	if err != nil {
		s.srv.Log.Warn("mail policy: a uid without a login; it may use no domain of the panel", "uid", uid, "err", err)
		return Identity{Kind: KindNobody, Local: true, Via: via + ", no login"}
	}
	via += " (" + login + ")"
	switch {
	case s.ix.Delivery() != "" && login == s.ix.Delivery():
		return Identity{Kind: KindNobody, Login: login, Local: true, Delivery: true, Via: via}
	case s.ix.IsAccount(login):
		return Identity{Kind: KindAccount, Login: login, Local: true, Via: via}
	case s.ix.AccountHome(home):
		// An account the policy does not name (yet): a new one, before the
		// write that names it, or one whose write failed. It owns nothing
		// until it is named — never free.
		s.srv.Log.Warn("mail policy: an account the policy does not name yet; it may use no domain of the panel", "login", login, "home", home)
		return Identity{Kind: KindNobody, Login: login, Local: true, Via: via + ", an account the policy does not name"}
	}
	return Identity{Kind: KindFree, Login: login, Local: true, Via: via}
}

var pickupRe = regexp.MustCompile(`^by (\S+) \((\S+), from userid ([0-9]+)\)$`)

// pickupHeader reads the copy of pickup's Received header: "by <host>
// (Postfix, from userid <uid>)", host the mail server's own (the j macro).
func pickupHeader(name, value, host string) (int, bool) {
	if !strings.EqualFold(name, "Received") {
		return 0, false
	}
	m := pickupRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil || (host != "" && !strings.EqualFold(m[1], host)) {
		return 0, false
	}
	uid, err := strconv.Atoi(m[3])
	if err != nil || uid < 0 {
		return 0, false
	}
	return uid, true
}

func (s *session) refuse(id Identity, r *Refusal) (*milter.Response, error) {
	s.srv.Log.Info("mail policy: refused", "queue", s.queue, "sender", id.String(), "address", r.Address.String(), "envelope", s.envelope, "reason", r.Text)
	return milter.RejectWithCodeAndReason(550, r.Error())
}

func (s *session) tempfail() (*milter.Response, error) {
	return milter.RejectWithCodeAndReason(451, "4.7.1 the mail policy is not loaded yet, try again later")
}
