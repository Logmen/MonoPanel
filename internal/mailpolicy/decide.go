package mailpolicy

import (
	"fmt"
	"slices"
	"strings"
)

// Kind is what a sender is to the policy.
type Kind int

const (
	// KindNobody is a sender whose account could not be told — a local
	// submission without pickup's header, a local SMTP client without its
	// socket in /proc/net, a login no mailbox has — or dovecot's mail user
	// without a script owner: it owns nothing, and may use no domain of the
	// panel.
	KindNobody Kind = iota
	// KindAccount is a panel account (Login).
	KindAccount
	// KindFree is root or a system user — no panel account (the panel's own
	// users, system daemons, an administrator's login): it may send as
	// anything.
	KindFree
)

// Identity is who sends a message.
type Identity struct {
	Kind Kind
	// Login is the account's login; for Nobody the unix login when there
	// is one (dovecot's mail user's included).
	Login string
	// Local: a process of this host (sendmail, or SMTP from one of the
	// host's addresses) — the mail of its cron and of its local programs
	// carries <login>@<host name>, which is its own, or root@<host name>,
	// which Debian's cron writes for every account.
	Local bool
	// Delivery: dovecot's mail user — owning nothing (a sieve reject or a
	// notification), or relaying a redirect of an account's script:
	// postmaster@ and mailer-daemon@ the host name are its.
	Delivery bool
	// Via says how the identity was found, for the log.
	Via string
}

func (id Identity) String() string {
	switch id.Kind {
	case KindFree:
		return "free (" + id.Via + ")"
	case KindAccount:
		return "account " + id.Login + " (" + id.Via + ")"
	}
	if id.Login != "" {
		return "no account, " + id.Login + " (" + id.Via + ")"
	}
	return "no account (" + id.Via + ")"
}

// Refusals. They name the domain or the address, never the account that
// owns it.
const (
	reasonDomain   = "this server does not send mail as a domain of another account"
	reasonHostname = "this server sends mail from its own host name only as the sender's own login"
	// ReasonUnreadable refuses a From header with no domain in it at all.
	ReasonUnreadable = "the From header cannot be read"
)

// Refusal is a refused message: what to answer, 550 5.7.1 <Text>.
type Refusal struct {
	// Address is the refused address ("" for an unreadable From).
	Address Address
	Text    string
}

func (r *Refusal) Error() string { return "5.7.1 " + r.Text }

// Check decides a message of id: its envelope sender (nil for the null
// sender, or when it was checked already), the addresses of its From
// headers, and whether every From header could be read. nil: the message
// may go.
func Check(ix *Index, id Identity, envelope *Address, from []Address, fromReadable bool) *Refusal {
	if id.Kind == KindFree {
		return nil
	}
	if !fromReadable {
		return &Refusal{Text: ReasonUnreadable}
	}
	check := from
	if envelope != nil {
		check = append([]Address{*envelope}, from...)
	}
	for _, a := range check {
		if r := checkAddress(ix, id, a); r != nil {
			return r
		}
	}
	return nil
}

// checkAddress decides one address.
func checkAddress(ix *Index, id Identity, a Address) *Refusal {
	owners := ix.Owners(a.Domain)
	if h := ix.Hostname(); h != "" && a.Domain == h {
		// The mail server's own host name (myhostname and myorigin): the
		// <login>@<host> of a local sender's own login, and root@<host>,
		// which Debian's and Ubuntu's cron writes into the From of every
		// account's cron mail (it sends as the account, "From: root (Cron
		// Daemon)") — while no account owns the name: root@ an account's
		// domain is that account's to send, and opendkim signs it with
		// that account's key —; the delivery user's postmaster and
		// mailer-daemon. Should the name be an account's domain as well,
		// that account's own; nobody else's, even where no account has it.
		local := strings.ToLower(a.Local)
		switch {
		case id.Local && id.Login != "" && local == id.Login:
			return nil
		case id.Local && id.Login != "" && local == "root" && len(owners) == 0:
			return nil
		case id.Delivery && (local == "postmaster" || local == "mailer-daemon"):
			return nil
		case id.Kind == KindAccount && slices.Contains(owners, id.Login):
			return nil
		}
		return &Refusal{Address: a, Text: fmt.Sprintf("<%s>: %s", a, reasonHostname)}
	}
	if len(owners) == 0 || (id.Kind == KindAccount && slices.Contains(owners, id.Login)) {
		return nil
	}
	return &Refusal{Address: a, Text: fmt.Sprintf("<%s>: %s", a.Domain, reasonDomain)}
}
