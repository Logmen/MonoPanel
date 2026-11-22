// Package mailpolicy is the panel's mail policy: a milter postfix calls
// before opendkim, which refuses a message whose envelope sender or header
// From is in a domain another account of the panel owns — whoever sends it:
// a mailbox over submission, a local process through sendmail, a local
// process over SMTP to a local address. opendkim signs whatever local mail
// says it is from a domain of its SigningTable, and SPF passes for any
// domain whose record names this server: without the policy every account
// could send as every other account's domains, DKIM and DMARC passing.
//
// The API writes the policy (Policy, PolicyFile) whenever accounts, sites,
// aliases, mail domains or mailboxes change; the milter runs as the panel's
// own system user (User) in monopanel-mailpolicy.service and reads it.
package mailpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/net/idna"
)

// The names the API, the agent, mp setup and the packaging share.
const (
	// Unit is the milter's systemd unit (packaging/systemd).
	Unit = "monopanel-mailpolicy.service"
	// User is the system user the milter runs as, and its group: never
	// root, never the API's user (the milter parses mail from untrusted
	// senders), never opendkim's (which holds the DKIM keys).
	User = "monopanel-mail"
	// PolicyFile is the policy the API writes through the agent, root's,
	// of User's group, 0640. /etc/monopanel is root:monopanel 0750, which
	// User cannot pass; the panel's postfix maps directory can be passed by
	// anyone, and the file's group keeps it to the milter (and postfix).
	PolicyFile = "/etc/postfix/monopanel/mail-policy.json"
	// PolicyMode is its mode.
	PolicyMode = 0o640
	// MaxPolicySize bounds what is read of the file and what the agent
	// accepts: a host with tens of thousands of mailboxes writes a few MB.
	MaxPolicySize = 64 << 20

	// PickupCleanup is the cleanup service of master.cf that postfix's
	// pickup hands local submissions (sendmail, postdrop) to.
	PickupCleanup = "pickup-cleanup"
	// PickupDaemonName is that service's milter_macro_daemon_name: the
	// {daemon_name} macro of every message that came in by sendmail. An
	// SMTP session's {daemon_name} is the smtpd's ($myhostname,
	// ORIGINATING), and nothing a sender writes reaches the macro.
	PickupDaemonName = "MONOPANEL-PICKUP"
	// HeaderChecksFile is that service's header_checks table.
	HeaderChecksFile = "/etc/postfix/monopanel/pickup_header_checks"
	// HeaderChecksRule copies the first line of the Received: header
	// pickup puts on top of every local submission — "by <host> (Postfix,
	// from userid <uid>)", the uid of the file the sender's sendmail left
	// in the maildrop — in front of it. Postfix hides its own first
	// Received: header from milters (milter8.c, "Sendmail compatibility"):
	// the text it hides is that header's own, and the copy, which cleanup
	// writes past its header callback, reaches the milter as the first
	// header of the message. A sender's own Received: headers, copied or
	// not, come after it.
	HeaderChecksRule = `/^(Received: by [^ ]+ \([^ ,]+, from userid [0-9]+\))/ PREPEND $1`

	// SocketPath is where the milter listens on Debian and Ubuntu: in
	// postfix's own private directory (postfix:root 0700, which no account
	// can pass), the socket postfix's own, 0600 — only postfix and root
	// may open it, and postfix needs no group for it. On EL, init_t may
	// not make a socket there (postfix_private_t), and postfix may not
	// connect to a socket of another place: EL takes TCPAddress.
	SocketPath = "/var/spool/postfix/private/monopanel-mailpolicy"
	// TCPAddress is where it listens on EL: a loopback port of the
	// policy's milter_port_t (8890, 8891, 8893; opendkim has 8891,
	// opendmarc's default is 8893), which postfix_smtpd_t and
	// postfix_cleanup_t may connect to. Any local account could connect
	// to a loopback port, or bind it while nothing holds it: PID 1 holds
	// it (SocketUnitInet), and the guard (GuardUnit) lets only root and
	// postfix connect to it.
	TCPAddress = "127.0.0.1:8890"
	// DKIMPort is opendkim's loopback port on EL (the API's dkimSocket),
	// which the guard keeps the accounts from too: opendkim signs whatever
	// a milter client that claims to be postfix hands it.
	DKIMPort = 8891

	// SocketUnitUnix and SocketUnitInet hold the milter's listener on
	// Debian and Ubuntu and on EL — systemd's socket activation: PID 1
	// binds it at boot and keeps it bound while the service restarts, is
	// down or is killed, so that no local account can take the address in
	// between, and postfix's connections wait in its backlog then rather
	// than fail. The service takes it from systemd (LISTEN_FDS).
	SocketUnitUnix = "monopanel-mailpolicy-unix.socket"
	SocketUnitInet = "monopanel-mailpolicy-inet.socket"
	// GuardUnit loads the guard on EL (`monopanel mail-guard`, root): an
	// nftables table that lets only root and postfix connect to the
	// policy's port and to opendkim's.
	GuardUnit = "monopanel-mailguard.service"
)

// HeaderChecksContent is the table HeaderChecksFile holds, and all it may
// hold: the milter trusts the first header of local submissions only while
// the file is exactly this.
const HeaderChecksContent = "# Generated by MonoPanel — do not edit: changes will be overwritten.\n" +
	"# The mail policy reads the sender's uid from this copy of the header pickup adds.\n" +
	HeaderChecksRule + "\n"

// Listener is where the milter listens and how postfix names it.
type Listener struct {
	// Network and Address are net.Listen's.
	Network, Address string
	// Postfix is the milter in main.cf's notation.
	Postfix string
	// Socket is the socket unit that holds it.
	Socket string
}

// ListenerFor is the listener of an OS family: a unix socket on Debian
// (debian is true), a loopback port elsewhere — as opendkim's (the API's
// dkimSocket).
func ListenerFor(debian bool) Listener {
	if debian {
		return Listener{Network: "unix", Address: SocketPath, Postfix: "unix:" + SocketPath, Socket: SocketUnitUnix}
	}
	return Listener{Network: "tcp", Address: TCPAddress, Postfix: "inet:" + TCPAddress, Socket: SocketUnitInet}
}

// MilterToken is the policy's entry in main.cf's milter lists: first, with
// a default action of its own — tempfail: while it is down mail waits, it
// never leaves unchecked.
func (l Listener) MilterToken() string { return "{ " + l.Postfix + ", default_action=tempfail }" }

// Version is the policy format the milter reads.
const Version = 1

// Policy is the file the API writes: every panel account by login with the
// domains it owns (its mail domains, sites and site aliases), every mailbox
// with its account, the mail server's host name and the user dovecot
// delivers (and sends sieve's mail) as. Nothing in it is secret.
type Policy struct {
	Version  int    `json:"version"`
	Hostname string `json:"hostname,omitempty"`
	// Delivery is dovecot's mail user (vmail): its mail — sieve's
	// redirects, vacation replies, notifications — carries what the
	// accounts' scripts and their correspondents put in it, so it is no
	// system sender (Identify).
	Delivery string `json:"delivery_user,omitempty"`
	// AccountHomes is the directory the accounts' homes are made in (the
	// panel's www_root): a user whose home lies below it is an account,
	// also before a policy names it (a new account, a write that failed),
	// and owns nothing then — never a system user, free to send as
	// anything.
	AccountHomes string             `json:"account_homes,omitempty"`
	Accounts     map[string]Account `json:"accounts"`
	Mailboxes    map[string]string  `json:"mailboxes"`
}

// Account is one panel account's part of the policy.
type Account struct {
	Domains []string `json:"domains"`
}

var (
	loginRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?$`)
	localRe = regexp.MustCompile(`^[a-z0-9!#$%&'*+/=?^_{|}~.-]{1,64}$`)
)

// ValidLogin reports whether s is a login the policy may name.
func ValidLogin(s string) bool { return loginRe.MatchString(s) }

// ValidDomain reports whether s is a domain the policy may name: ASCII
// (IDNs in their xn-- form), lower case, no trailing dot, two labels or
// more — a single label would make every name under it the account's.
func ValidDomain(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if !labelRe.MatchString(l) {
			return false
		}
	}
	return true
}

// ValidMailbox reports whether s is a mailbox address the policy may name.
func ValidMailbox(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && localRe.MatchString(local) && ValidDomain(domain)
}

// Parse reads a policy, strictly: unknown fields, a second JSON value,
// names that are not logins, domains or addresses, a mailbox of an account
// the policy does not list, a file over MaxPolicySize — all refused.
func Parse(b []byte) (*Policy, error) {
	if len(b) > MaxPolicySize {
		return nil, fmt.Errorf("the policy is larger than %d bytes", MaxPolicySize)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("the policy is not the panel's JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the policy has more than one JSON value")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks every name of the policy.
func (p *Policy) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("policy version %d, not %d", p.Version, Version)
	}
	if p.Hostname != "" && !ValidDomain(p.Hostname) {
		return fmt.Errorf("the host name %q is not a domain name", p.Hostname)
	}
	if p.Delivery != "" && !ValidLogin(p.Delivery) {
		return fmt.Errorf("the delivery user %q is not a login", p.Delivery)
	}
	if h := p.AccountHomes; h != "" && (!path.IsAbs(h) || path.Clean(h) != h || h == "/" || strings.ContainsAny(h, "\x00\n")) {
		return fmt.Errorf("the accounts' homes %q are not an absolute, clean path below /", h)
	}
	if p.Accounts == nil || p.Mailboxes == nil {
		return errors.New("the policy has no accounts or no mailboxes (empty ones are {})")
	}
	for login, a := range p.Accounts {
		if !ValidLogin(login) {
			return fmt.Errorf("the account %q is not a login", login)
		}
		if a.Domains == nil {
			return fmt.Errorf("the account %q has no domains list (an empty one is [])", login)
		}
		for _, d := range a.Domains {
			if !ValidDomain(d) {
				return fmt.Errorf("the domain %q of %s is not a domain name in lower case, xn-- for IDNs", d, login)
			}
		}
	}
	for box, login := range p.Mailboxes {
		if !ValidMailbox(box) {
			return fmt.Errorf("the mailbox %q is not an address in lower case", box)
		}
		if _, ok := p.Accounts[login]; !ok {
			return fmt.Errorf("the mailbox %s belongs to %q, which is no account of the policy", box, login)
		}
	}
	return nil
}

// Marshal is the policy as the API writes it: indented, keys and domains
// sorted, so that an unchanged policy is the same file.
func (p *Policy) Marshal() ([]byte, error) {
	for login, a := range p.Accounts {
		ds := slices.Clone(a.Domains)
		sort.Strings(ds)
		a.Domains = slices.Compact(ds)
		p.Accounts[login] = a
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// NormalizeDomain is a domain as the policy compares it: lower case, no
// trailing dot, an IDN in its xn-- form. ok is false when nothing is left
// or an IDN does not convert; the lower-case name is returned then, which
// matches none of the policy's (all ASCII) names.
func NormalizeDomain(s string) (string, bool) {
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	if s == "" {
		return "", false
	}
	lower := strings.ToLower(s)
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return lower, true
	}
	a, err := idna.Lookup.ToASCII(s)
	if err != nil || a == "" {
		return lower, false
	}
	return strings.ToLower(a), true
}

// Index is a policy made ready for lookups.
type Index struct {
	hostname string
	delivery string
	homes    string
	accounts map[string]bool
	// owners are the accounts of each domain; one name may be several
	// accounts' (an administrator can give a site of one account the name
	// of another's mail domain).
	owners    map[string][]string
	mailboxes map[string]string
}

// NewIndex indexes a validated policy.
func NewIndex(p *Policy) *Index {
	ix := &Index{
		hostname:  p.Hostname,
		delivery:  p.Delivery,
		homes:     p.AccountHomes,
		accounts:  make(map[string]bool, len(p.Accounts)),
		owners:    map[string][]string{},
		mailboxes: make(map[string]string, len(p.Mailboxes)),
	}
	for login, a := range p.Accounts {
		ix.accounts[login] = true
		for _, d := range a.Domains {
			if !slices.Contains(ix.owners[d], login) {
				ix.owners[d] = append(ix.owners[d], login)
			}
		}
	}
	for d := range ix.owners {
		sort.Strings(ix.owners[d])
	}
	for box, login := range p.Mailboxes {
		ix.mailboxes[box] = login
	}
	return ix
}

// Hostname is the mail server's host name (myhostname and myorigin).
func (ix *Index) Hostname() string { return ix.hostname }

// Delivery is dovecot's mail user.
func (ix *Index) Delivery() string { return ix.delivery }

// IsAccount reports whether login is a panel account.
func (ix *Index) IsAccount(login string) bool { return ix.accounts[login] }

// AccountHome reports whether home lies below the accounts' homes: the home
// of an account, named by the policy or not yet.
func (ix *Index) AccountHome(home string) bool {
	if ix.homes == "" || home == "" {
		return false
	}
	h := path.Clean(home)
	return strings.HasPrefix(h, ix.homes+"/")
}

// MailboxAccount is the account of a mailbox address ("" when none).
func (ix *Index) MailboxAccount(address string) string {
	local, domain, ok := strings.Cut(strings.TrimSpace(address), "@")
	if !ok {
		return ""
	}
	d, _ := NormalizeDomain(domain)
	return ix.mailboxes[strings.ToLower(local)+"@"+d]
}

// Owners are the accounts that own domain: those of the longest of the
// policy's domains that domain is, or lies under (a.example.com of one
// account is its own; x.example.com is the owner of example.com's when no
// account has it). None: the domain is foreign to the panel, anybody's.
func (ix *Index) Owners(domain string) []string {
	d, _ := NormalizeDomain(domain)
	for d != "" {
		if o, ok := ix.owners[d]; ok {
			return o
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			break
		}
		d = d[i+1:]
	}
	return nil
}

// AddressAccount is the account an address of the panel's names: its
// mailbox's, else the one owner of its domain; "" when there is none or
// several.
func (ix *Index) AddressAccount(address string) string {
	if a := ix.MailboxAccount(address); a != "" {
		return a
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok {
		return ""
	}
	if o := ix.Owners(domain); len(o) == 1 {
		return o[0]
	}
	return ""
}
