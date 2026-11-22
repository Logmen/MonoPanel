package api

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/jobs"
	"monopanel/internal/mailpolicy"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// The panel's mail policy (internal/mailpolicy): a milter postfix calls
// before opendkim, which refuses a message from a domain another account
// owns. The API keeps its file up to date — at every mail change
// (applyMailConfig) and after the jobs that change accounts and sites
// (withMailPolicy) — and its unit running while the mail server is
// installed.

// mailPolicyListener is where the policy listens on this host, as postfix
// names it in main.cf.
func (s *Server) mailPolicyListener() mailpolicy.Listener {
	return mailpolicy.ListenerFor(s.profile.Family() == osprofile.FamilyDebian)
}

// mailPolicy is the policy as the database has it: every panel account that
// has a unix user by login with its mail domains, sites and site aliases,
// every mailbox with its account, the mail server's host name, dovecot's
// mail user and the directory the accounts' homes are made in. A name the
// policy cannot hold (a single label, a wildcard's star) is left out:
// "*.shop.example" of a site stands for shop.example and what lies under
// it. A mail domain is its owner's alone: a site or an alias of another
// account with its very name — which only an administrator can give —
// makes the name no second account's, since opendkim signs it with its
// owner's key. A panel user without a unix user sends no mail of this
// host's: a unix login of the same name is no account of the panel.
func (s *Server) mailPolicy(ctx context.Context, c mailConfig) (*mailpolicy.Policy, error) {
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return nil, err
	}
	domains, err := s.db.ListMailDomains(ctx, 0)
	if err != nil {
		return nil, err
	}
	boxes, err := s.db.ListMailboxes(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	p := &mailpolicy.Policy{
		Version:   mailpolicy.Version,
		Delivery:  vmailUser,
		Accounts:  map[string]mailpolicy.Account{},
		Mailboxes: map[string]string{},
	}
	if homes := path.Clean(s.cfg.WWWRoot); path.IsAbs(homes) && homes != "/" {
		p.AccountHomes = homes
	}
	if h, ok := mailpolicy.NormalizeDomain(c.Hostname); ok && mailpolicy.ValidDomain(h) {
		p.Hostname = h
	}
	logins := map[int64]string{}
	for _, u := range users {
		if u.UnixUID == nil {
			continue
		}
		if !mailpolicy.ValidLogin(u.Login) {
			s.log.Warn("mail policy: an account whose login the policy cannot hold is left out", "login", u.Login)
			continue
		}
		logins[u.ID] = u.Login
		p.Accounts[u.Login] = mailpolicy.Account{Domains: []string{}}
	}
	mailOwner := map[string]int64{}
	for _, d := range domains {
		if n, ok := mailpolicy.NormalizeDomain(d.Name); ok {
			mailOwner[n] = d.UserID
		}
	}
	add := func(userID int64, name string, site bool) {
		login := logins[userID]
		if login == "" {
			return
		}
		d, ok := mailpolicy.NormalizeDomain(strings.TrimPrefix(strings.TrimSpace(name), "*."))
		if !ok || !mailpolicy.ValidDomain(d) {
			return
		}
		if owner, mail := mailOwner[d]; site && mail && owner != userID {
			s.log.Warn("mail policy: a site's name is another account's mail domain; the name stays the mail domain's owner's", "name", d, "site_owner", login)
			return
		}
		a := p.Accounts[login]
		a.Domains = append(a.Domains, d)
		p.Accounts[login] = a
	}
	for _, site := range sites {
		add(site.UserID, site.Domain, true)
		for _, alias := range site.Aliases {
			add(site.UserID, alias, true)
		}
	}
	byID := map[int64]*store.MailDomain{}
	for _, d := range domains {
		byID[d.ID] = d
		add(d.UserID, d.Name, false)
	}
	for _, b := range boxes {
		d := byID[b.DomainID]
		if d == nil {
			continue
		}
		addr := strings.ToLower(b.Address)
		if login := logins[d.UserID]; login != "" && mailpolicy.ValidMailbox(addr) {
			p.Mailboxes[addr] = login
		}
	}
	return p, nil
}

// writeMailPolicy writes the policy file, root's, of the milter's group,
// 0640; the milter reads it again with the next message. One write at a
// time, each from the database as it is then — the mail settings too: the
// last one written is the newest. A write that fails is remembered for the
// doctor and done again within the hour (retryMoves).
func (s *Server) writeMailPolicy(ctx context.Context) (err error) {
	s.mailPolicyMu.Lock()
	defer s.mailPolicyMu.Unlock()
	defer func() {
		if err != nil {
			s.mailPolicyErr.Store(err.Error())
		} else {
			s.mailPolicyErr.Store("")
		}
	}()
	c := s.loadMailConfig(ctx)
	p, err := s.mailPolicy(ctx, c)
	if err != nil {
		return fmt.Errorf("mail policy: %w", err)
	}
	b, err := p.Marshal()
	if err != nil {
		return fmt.Errorf("mail policy: %w", err)
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:  []agent.FileSpec{{Path: mailpolicy.PolicyFile, Content: string(b), Mode: mailpolicy.PolicyMode, Owner: "root", Group: mailpolicy.User}},
		Origin: "mail:policy",
	}); err != nil {
		return fmt.Errorf("mail policy: %w", err)
	}
	return nil
}

// refreshMailPolicy writes the policy again after a change of accounts or
// sites, and once an hour (retryMoves), where the mail server is installed.
// Until a write names a new account, the milter takes it by its home for
// one that owns nothing.
func (s *Server) refreshMailPolicy(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if !s.loadMailConfig(ctx).Installed {
		return
	}
	if err := s.writeMailPolicy(ctx); err != nil {
		s.log.Warn("mail policy: not written; the milter keeps the policy it has until the next write (within the hour) or mp mail apply", "err", err)
	}
}

// withMailPolicy is a job that changes accounts, sites or their aliases,
// followed by a write of the mail policy — whether the job succeeded or
// not, the policy follows what the database has.
func (s *Server) withMailPolicy(h jobs.Handler) jobs.Handler {
	return func(ctx context.Context, jc *jobs.Context) error {
		err := h(ctx, jc)
		s.refreshMailPolicy(ctx)
		return err
	}
}

// ensureMailPolicyAccount makes the milter's system user where the package
// (sysusers.d) or mp setup did not. postfix needs no group of it: the
// socket is postfix's own (mailpolicy.SocketPath), or a port on EL.
func (s *Server) ensureMailPolicyAccount(ctx context.Context) error {
	if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: mailpolicy.User, System: true}); err != nil {
		return fmt.Errorf("mail policy group: %w", err)
	}
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: mailpolicy.User, System: true, PrimaryGroup: mailpolicy.User, Comment: "MonoPanel mail policy"}); err != nil {
		return fmt.Errorf("mail policy user: %w", err)
	}
	return nil
}

// startMailPolicy puts the policy in place before postfix is told to send no
// mail past it: on EL the guard first (an nftables table that lets only
// root and postfix connect to the policy's port and opendkim's), then the
// socket unit that holds the listener, then the service — restarted when it
// runs a binary an upgrade replaced —, and waits a few seconds for it to
// answer. A unit that does not start fails the change, and postfix keeps
// the configuration it has. So does a policy that does not answer while
// main.cf does not name it yet — an install, the first apply after an
// upgrade: a mail server that worked must not start keeping every message
// waiting for a policy that cannot run; the move tries again later. Once
// main.cf names it, one that does not answer yet is said in the log, and
// mail waits for it.
func (s *Server) startMailPolicy(ctx context.Context, logf func(string, ...any)) error {
	l := s.mailPolicyListener()
	if l.Network == "tcp" {
		if err := s.ensurePackages(ctx, nil, "nftables"); err != nil {
			return fmt.Errorf("mail guard: nftables: %w", err)
		}
		for _, action := range []string{"enable", "start"} {
			if _, err := s.agent.Service(ctx, mailpolicy.GuardUnit, action); err != nil {
				return fmt.Errorf("mail guard: %w", err)
			}
		}
	}
	for _, action := range []string{"enable", "start"} {
		if _, err := s.agent.Service(ctx, l.Socket, action); err != nil {
			return fmt.Errorf("mail policy: %w", err)
		}
	}
	if _, err := s.agent.Service(ctx, mailpolicy.Unit, "enable"); err != nil {
		return fmt.Errorf("mail policy: %w", err)
	}
	action := "start"
	if st, err := s.agent.Service(ctx, mailpolicy.Unit, "status"); err == nil && st.Status.ActiveState == "active" && st.Status.BinaryReplaced {
		action = "restart"
	}
	if _, err := s.agent.Service(ctx, mailpolicy.Unit, action); err != nil {
		return fmt.Errorf("mail policy: %w", err)
	}
	var last string
	// Whether main.cf names the policy already: the agent reads it with
	// each probe; when no probe got through, an apply of this version that
	// went through before says so.
	configured, told := false, false
	for i := 0; i < 25; i++ {
		res, err := s.agent.MailPolicyProbe(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case res.OK:
			return nil
		default:
			last, configured, told = res.Error, res.Configured, true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.mailPolicyWait):
		}
	}
	if !told {
		configured = s.mailPolicyMoveState(ctx).Done >= mailPolicyGeneration
	}
	if !configured {
		return fmt.Errorf("the mail policy does not answer (%s): postfix keeps the configuration it has, without the policy; systemctl status %s %s", last, l.Socket, mailpolicy.Unit)
	}
	logf("warning: the mail policy does not answer yet (%s): mail waits until it does; systemctl status %s %s", last, l.Socket, mailpolicy.Unit)
	return nil
}

// settingMailPolicy remembers that this version's mail configuration — the
// policy's unit and file, main.cf and master.cf with the policy before
// opendkim, TrustedHosts without the mail domains — is in place:
// mailPolicyGeneration, once a mail.apply went through.
const (
	settingMailPolicy    = "mail.policy"
	mailPolicyGeneration = 1
)

// mailPolicyMove is that bookkeeping: the generation in place and the
// mail.apply queued to put this one in place, retried until it succeeds.
type mailPolicyMove struct {
	Done int `json:"done,omitempty"`
	retry
}

func (s *Server) mailPolicyMoveState(ctx context.Context) mailPolicyMove {
	var m mailPolicyMove
	if raw, err := s.db.GetSetting(ctx, settingMailPolicy); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func (s *Server) saveMailPolicyMove(ctx context.Context, m mailPolicyMove) {
	b, err := json.Marshal(m)
	if err == nil {
		err = s.db.SetSetting(context.WithoutCancel(ctx), settingMailPolicy, string(b))
	}
	if err != nil {
		s.log.Warn("mail policy: the state of its move is not stored", "err", err)
	}
}

// mailPolicyInPlace records that a mail.apply (or any change of the mail
// configuration) put this version's configuration in place.
func (s *Server) mailPolicyInPlace(ctx context.Context) {
	s.movesMu.Lock()
	defer s.movesMu.Unlock()
	m := s.mailPolicyMoveState(ctx)
	if m.Done >= mailPolicyGeneration && m.Job == 0 {
		return
	}
	s.saveMailPolicyMove(ctx, mailPolicyMove{Done: mailPolicyGeneration})
}

// moveMailPolicy brings a host that had the mail server before this version
// to its mail configuration at the first start of the API after the
// upgrade, without an administrator: a mail.apply, queued again at every
// start and once an hour, each time later than the last (retry), until one
// goes through. It goes by main.cf too, not by its bookkeeping alone: a
// main.cf that does not have postfix ask the policy first — the package
// removed and installed again, an older version's mp mail apply after a
// downgrade and an upgrade again — gets the same mail.apply. While the
// updater has not confirmed this version yet (it rolls back to the binary
// it replaced when the version does not answer, which knows no policy), the
// move waits for its verdict.
func (s *Server) moveMailPolicy(ctx context.Context) {
	if !s.loadMailConfig(ctx).Installed {
		return
	}
	if s.updateUnconfirmed() {
		s.moveMailPolicyAfterUpdate(ctx)
		return
	}
	s.movesMu.Lock()
	defer s.movesMu.Unlock()
	m := s.mailPolicyMoveState(ctx)
	if m.Done >= mailPolicyGeneration {
		res, err := s.agent.MailPolicyProbe(ctx)
		if err != nil || res.Configured {
			return
		}
		s.log.Warn("mail policy: main.cf does not have postfix ask the policy; the mail configuration is applied again", "why", res.ConfigError)
	}
	if m.settle(ctx, s.db) || !m.due(time.Now()) {
		s.saveMailPolicyMove(ctx, m)
		return
	}
	j, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy("system"))
	if err != nil {
		s.log.Warn("mail policy: cannot queue the mail configuration of this version; the next start tries again", "err", err)
		return
	}
	m.Job = j.ID
	s.saveMailPolicyMove(ctx, m)
	s.log.Info("mail policy: the mail configuration of this version is queued (mail.apply)", "job", j.ID)
}

// updateUnconfirmed reports whether this binary is an update the updater has
// not confirmed yet: its state says it installs this very version.
func (s *Server) updateUnconfirmed() bool {
	st, err := updater.ReadState(s.cfg.UpdatesDir())
	return err == nil && st != nil && st.Status == updater.StatusInstalling && updater.Version(st.To) == updater.Version(buildinfo.Version)
}

// moveMailPolicyAfterUpdate has the move wait for the updater's verdict on
// this version, once: it looks again every mailPolicyUpdateStep, for as long
// as the updater waits for the version to answer and a little more, and
// moves once the update is through. After a rollback the binary that runs is
// the replaced one, and this process is gone.
func (s *Server) moveMailPolicyAfterUpdate(ctx context.Context) {
	if !s.mailPolicyAwaitsUpdate.CompareAndSwap(false, true) {
		return
	}
	s.log.Info("mail policy: the update to this version is not confirmed yet; the mail configuration waits for it")
	go func() {
		defer s.mailPolicyAwaitsUpdate.Store(false)
		ctx := context.WithoutCancel(ctx)
		for deadline := time.Now().Add(mailPolicyUpdateWait); time.Now().Before(deadline); {
			time.Sleep(s.mailPolicyUpdateStep)
			if !s.updateUnconfirmed() {
				s.mailPolicyAwaitsUpdate.Store(false)
				s.moveMailPolicy(ctx)
				return
			}
		}
		s.log.Warn("mail policy: the update to this version is still not confirmed; the hourly retry moves the mail configuration")
	}()
}

// mailPolicyUpdateWait is how long the move waits for the updater's verdict:
// the updater's own wait for the version to answer (90 s), and more.
const mailPolicyUpdateWait = 5 * time.Minute

// mailPolicyCheck is the doctor's "mail policy" line while the mail server
// is installed: postfix asks the policy (main.cf), and the policy runs on its
// socket and answers the milter protocol there — otherwise either an account
// can send as another account's domain, or postfix keeps every message
// waiting. On EL the guard runs too: without it any local account can have
// opendkim sign any domain.
func (s *Server) mailPolicyCheck(ctx context.Context) *apitypes.Check {
	if !s.loadMailConfig(ctx).Installed {
		return nil
	}
	const name = "mail policy"
	l := s.mailPolicyListener()
	hint := "mail waits until it is back: systemctl status " + strings.TrimSuffix(l.Socket, ".socket") + " " + strings.TrimSuffix(mailpolicy.Unit, ".service")
	fail := func(status, detail string) *apitypes.Check {
		c := check(name, status, detail)
		return &c
	}
	res, err := s.agent.MailPolicyProbe(ctx)
	if err != nil {
		return fail("fail", "it cannot be asked: "+err.Error())
	}
	if !res.Configured {
		if m := s.mailPolicyMoveState(ctx); m.Done < mailPolicyGeneration {
			return fail("warn", "the mail configuration of this version is not in place yet ("+m.describe(ctx, s.db)+"): until it is, postfix does not ask the policy, and an account can send as another account's domain; mp mail apply")
		}
		return fail("fail", "postfix does not ask the mail policy ("+res.ConfigError+"): an account can send as another account's domain; mp mail apply")
	}
	units := []string{l.Socket, mailpolicy.Unit}
	if l.Network == "tcp" {
		units = append([]string{mailpolicy.GuardUnit}, units...)
	}
	for _, u := range units {
		st, err := s.agent.Service(ctx, u, "status")
		switch {
		case err != nil:
			return fail("fail", u+": "+err.Error()+"; "+hint)
		case st.Status.ActiveState == "active":
		case u == mailpolicy.GuardUnit:
			return fail("fail", u+" is "+st.Status.ActiveState+" ("+st.Status.SubState+"): any local account can have opendkim sign any domain and reach the policy's port; systemctl start "+strings.TrimSuffix(u, ".service"))
		default:
			return fail("fail", u+" is "+st.Status.ActiveState+" ("+st.Status.SubState+"); "+hint)
		}
	}
	if !res.OK {
		return fail("fail", "it does not answer at "+res.Listener+": "+res.Error+"; "+hint)
	}
	if e, _ := s.mailPolicyErr.Load().(string); e != "" {
		return fail("warn", "answers postfix at "+res.Listener+", but its file was not written at the last change ("+e+"): it keeps the policy it had, the hourly retry writes it again; mp mail apply")
	}
	return fail("ok", "answers postfix at "+res.Listener)
}
