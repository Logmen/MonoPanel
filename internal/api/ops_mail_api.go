package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

type mailStatusOutput struct {
	Body apitypes.MailStatus
}

type mailSettingsInput struct {
	Body apitypes.MailSettingsRequest
}

type mailInstallInput struct {
	Body apitypes.MailInstallRequest
}

type mailDomainsOutput struct {
	Body []*store.MailDomain
}

type mailDomainInput struct {
	Body apitypes.MailDomainRequest
}

type mailDomainOutput struct {
	Status int
	Body   *store.MailDomain
}

type mailNameInput struct {
	Name string `path:"name" maxLength:"253"`
}

type mailDomainPatchInput struct {
	Name string `path:"name" maxLength:"253"`
	Body apitypes.MailDomainUpdateRequest
}

type mailboxesInput struct {
	Domain string `query:"domain" doc:"Show the mailboxes of one domain"`
}

type mailboxesOutput struct {
	Body []*store.Mailbox
}

type mailboxCreateInput struct {
	Body apitypes.MailboxRequest
}

type mailboxOutput struct {
	Status int
	Body   apitypes.MailboxResponse
}

type mailboxPatchInput struct {
	Address string `path:"address" maxLength:"320"`
	Body    apitypes.MailboxUpdateRequest
}

type mailboxDeleteInput struct {
	Address string `path:"address" maxLength:"320"`
	Purge   bool   `query:"purge" doc:"Also delete the messages on disk"`
}

type aliasesOutput struct {
	Body []*store.MailAlias
}

type aliasInput struct {
	Body apitypes.MailAliasRequest
}

type aliasOutput struct {
	Status int
	Body   *store.MailAlias
}

type aliasDeleteInput struct {
	Address string `path:"address" maxLength:"320"`
}

type mailDNSOutput struct {
	Body apitypes.MailDNS
}

type webmailInput struct {
	Body apitypes.WebmailRequest
}

// ownerFor resolves who a new object belongs to: an administrator names the
// account, a user always gets their own.
func (s *Server) ownerFor(ctx context.Context, login string) (*store.User, error) {
	p := principalFrom(ctx)
	var owner *store.User
	var err error
	switch {
	case p.Role == store.RoleAdmin && login != "":
		owner, err = s.db.GetUserByLogin(ctx, login)
	case p.UserID != 0:
		owner, err = s.db.GetUserByID(ctx, p.UserID)
	default:
		return nil, huma.Error422UnprocessableEntity("specify the owner (user)")
	}
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("account not found")
	}
	if owner.Role != store.RoleUser {
		return nil, huma.Error422UnprocessableEntity("only a user account can be the owner")
	}
	return owner, nil
}

func (s *Server) registerMail() {
	huma.Register(s.api, huma.Operation{
		OperationID: "mail-status", Method: http.MethodGet, Path: "/mail", Summary: "Mail server status", Tags: []string{"mail"}, Security: secured,
		Description: "For an ordinary account, domains, mailboxes and aliases count its own, and the warnings are about its own mail only: what the server-wide ones say is the administrator's to fix.",
	}, func(ctx context.Context, _ *struct{}) (*mailStatusOutput, error) {
		return &mailStatusOutput{Body: s.mailStatusFor(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-install", Method: http.MethodPost, Path: "/mail/install", Summary: "Install postfix, dovecot and opendkim (async)", Tags: []string{"mail"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *mailInstallInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		// Checked here, not only in the job: the name goes into main.cf as
		// it is, the temporary one of the install included.
		if h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Body.Hostname), ".")); h != "" && !mailDomainOK(h) {
			return nil, huma.Error422UnprocessableEntity("mail server hostname must be fully qualified, e.g. mail.example.com")
		}
		job, err := s.jobs.Enqueue(ctx, "mail.install", mailInstallPayload{Hostname: in.Body.Hostname, POP3: in.Body.POP3}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.install", Target: in.Body.Hostname, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-apply", Method: http.MethodPost, Path: "/mail/apply", Summary: "Regenerate the mail configuration (async)", Tags: []string{"mail"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, _ *struct{}) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		job, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-settings", Method: http.MethodPut, Path: "/mail/settings", Summary: "Change mail settings", Tags: []string{"mail"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *mailSettingsInput) (*mailStatusOutput, error) {
		p := principalFrom(ctx)
		b := in.Body
		// Every value is checked first; the change then goes in over the
		// settings as they are by then (changeMailConfig says why).
		hostname := ""
		if b.Hostname != "" {
			// The name goes into main.cf, dovecot.conf and opendkim's
			// TrustedHosts as it is: a host name and nothing else.
			hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b.Hostname), "."))
			if !mailDomainOK(hostname) {
				return nil, huma.Error422UnprocessableEntity("mail server hostname must be fully qualified, e.g. mail.example.com")
			}
		}
		if b.WebmailPort != nil {
			if port := *b.WebmailPort; port != 0 && (port < 1024 || port == s.panelPort() || port == 80 || port == 443) {
				return nil, huma.Error422UnprocessableEntity("choose a free port above 1024 for the webmail, e.g. 2096")
			}
		}
		rbl := []string{}
		if b.RBL != nil {
			for _, r := range *b.RBL {
				if r = strings.TrimSpace(strings.ToLower(r)); r != "" {
					if !mailDomainOK(strings.TrimSuffix(r, ".")) {
						return nil, huma.Error422UnprocessableEntity("a blocklist must be a domain, e.g. zen.spamhaus.org")
					}
					rbl = append(rbl, r)
				}
			}
		}
		c, err := s.changeMailConfig(ctx, func(cur *mailConfig) {
			if hostname != "" {
				cur.Hostname = hostname
			}
			if b.MaxSizeMB > 0 {
				cur.MaxSizeMB = b.MaxSizeMB
			}
			if b.POP3 != nil {
				cur.POP3 = *b.POP3
			}
			if b.DKIM != nil {
				cur.DKIM = *b.DKIM
			}
			if b.Port25 != nil {
				cur.Port25 = *b.Port25
			}
			if b.WebmailPort != nil {
				cur.WebmailPort = *b.WebmailPort
			}
			if b.RBL != nil {
				cur.RBL = rbl
			}
		})
		if err != nil {
			return nil, err
		}
		if c.Installed {
			if err := s.applyMail(ctx, nil); err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			if enabled, err := s.db.GetSetting(ctx, settingFirewall); err == nil && enabled == "yes" {
				s.applyFirewall(ctx) //nolint:errcheck // порты — не повод отказать в сохранении настроек
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.settings", Target: c.Hostname, IP: requestInfo(ctx).IP})
		return &mailStatusOutput{Body: s.mailStatus(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domains-list", Method: http.MethodGet, Path: "/mail/domains", Summary: "Mail domains (admins: all, users: own)", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*mailDomainsOutput, error) {
		p := principalFrom(ctx)
		var uid int64
		if p.Role != store.RoleAdmin {
			uid = p.UserID
		}
		list, err := s.db.ListMailDomains(ctx, uid)
		if err != nil {
			return nil, err
		}
		return &mailDomainsOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domains-create", Method: http.MethodPost, Path: "/mail/domains", Summary: "Add a mail domain (generates a DKIM key)", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *mailDomainInput) (*mailDomainOutput, error) {
		p := principalFrom(ctx)
		c := s.loadMailConfig(ctx)
		if !c.Installed {
			return nil, huma.Error422UnprocessableEntity("mail server is not installed")
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Body.Name), "."))
		if !mailDomainOK(name) {
			return nil, huma.Error422UnprocessableEntity("domain must look like example.com")
		}
		owner, err := s.ownerFor(ctx, in.Body.User)
		if err != nil {
			return nil, err
		}
		if p.Role != store.RoleAdmin {
			// The server delivers mail for its domains itself: a domain of
			// the panel or under another account's name would hand that mail
			// to this account.
			if s.panelNames(ctx)[name] {
				return nil, huma.Error422UnprocessableEntity(name + " is a hostname of this panel itself (the panel or its mail server)")
			}
			other, err := s.foreignDomain(ctx, owner.ID, name)
			if err != nil {
				return nil, err
			}
			if other != "" {
				return nil, huma.Error403Forbidden(name + " is under " + other + ", which belongs to another account")
			}
		}
		if in.Body.SendOnly && in.Body.Lenient {
			return nil, huma.Error422UnprocessableEntity("a send-only domain receives no mail here, so it cannot be lenient")
		}
		d := &store.MailDomain{UserID: owner.ID, Name: name, Active: true, Lenient: in.Body.Lenient, SendOnly: in.Body.SendOnly}
		wantDKIM := c.DKIM && (in.Body.DKIM == nil || *in.Body.DKIM)
		if wantDKIM {
			if s.secrets == nil {
				return nil, huma.Error422UnprocessableEntity("the encryption key is unavailable, so DKIM cannot be enabled")
			}
			priv, pub, err := newDKIMKey()
			if err != nil {
				return nil, err
			}
			enc, err := s.secrets.Encrypt(priv)
			if err != nil {
				return nil, err
			}
			d.DKIMSelector, d.DKIMPublic, d.DKIMKeyEnc = "mp"+timeSelector(), pub, enc
		}
		if err := s.db.CreateMailDomain(ctx, d); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("domain already added")
			}
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		d.Login = owner.Login
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.domain.create", Target: name, IP: requestInfo(ctx).IP})
		return &mailDomainOutput{Status: http.StatusCreated, Body: d}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domains-delete", Method: http.MethodDelete, Path: "/mail/domains/{name}", Summary: "Remove a mail domain with its mailboxes", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *mailNameInput) (*struct{}, error) {
		p := principalFrom(ctx)
		d, err := s.mailDomainFor(ctx, strings.ToLower(in.Name))
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err := s.db.DeleteMailDomain(ctx, d.ID); err != nil {
			return nil, err
		}
		// The name is a path component here: one that came in another way
		// (see mapToken) could lead into another tenant's directory.
		if !mapToken(d.Name) {
			s.log.Warn("mail: the domain's name is not a directory name; its directories are left alone", "domain", d.Name)
		} else if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{mailBase + "/" + d.Name, dkimDir + "/keys/" + d.Name}, Recursive: true}); err != nil {
			s.log.Warn("mail: removing the domain directory", "domain", d.Name, "err", err)
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.domain.delete", Target: d.Name, IP: requestInfo(ctx).IP})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domains-update", Method: http.MethodPatch, Path: "/mail/domains/{name}", Summary: "Change a mail domain (state, lenient checks)", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailDomainPatchInput) (*mailDomainOutput, error) {
		p := principalFrom(ctx)
		d, err := s.mailDomainFor(ctx, strings.ToLower(in.Name))
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if in.Body.Active != nil {
			d.Active = *in.Body.Active
		}
		if in.Body.Lenient != nil {
			d.Lenient = *in.Body.Lenient
		}
		if in.Body.SendOnly != nil && *in.Body.SendOnly && !d.SendOnly {
			// Mail that the domain's mailboxes hold would be stranded: the
			// server stops accepting it the moment the domain turns send-only.
			boxes, err := s.db.ListMailboxes(ctx, d.ID, 0)
			if err != nil {
				return nil, err
			}
			aliases, err := s.db.ListMailAliases(ctx, d.ID, 0)
			if err != nil {
				return nil, err
			}
			if len(boxes)+len(aliases) > 0 {
				return nil, huma.Error409Conflict(fmt.Sprintf("the domain has %d mailboxes and %d aliases here; delete them before it becomes send-only", len(boxes), len(aliases)))
			}
		}
		if in.Body.SendOnly != nil {
			d.SendOnly = *in.Body.SendOnly
		}
		if d.SendOnly && d.Lenient {
			return nil, huma.Error422UnprocessableEntity("a send-only domain receives no mail here, so it cannot be lenient")
		}
		if err := s.db.UpdateMailDomain(ctx, d); err != nil {
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.domain.update", Target: d.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"active": d.Active, "lenient": d.Lenient, "send_only": d.SendOnly}})
		return &mailDomainOutput{Status: http.StatusOK, Body: d}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domain-dkim", Method: http.MethodPost, Path: "/mail/domains/{name}/dkim", Summary: "Generate a new DKIM key for the domain", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailNameInput) (*mailDomainOutput, error) {
		p := principalFrom(ctx)
		d, err := s.mailDomainFor(ctx, strings.ToLower(in.Name))
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if s.secrets == nil {
			return nil, huma.Error422UnprocessableEntity("the encryption key is unavailable")
		}
		priv, pub, err := newDKIMKey()
		if err != nil {
			return nil, err
		}
		enc, err := s.secrets.Encrypt(priv)
		if err != nil {
			return nil, err
		}
		old := d.DKIMSelector
		d.DKIMSelector, d.DKIMPublic, d.DKIMKeyEnc = "mp"+timeSelector(), pub, enc
		if err := s.db.UpdateMailDomain(ctx, d); err != nil {
			return nil, err
		}
		// A selector that is no file name (see mapToken) names no key the
		// panel wrote: a new key replaces it, and nothing is removed for it.
		if old != "" && mapToken(old) && mapToken(d.Name) {
			s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{fmt.Sprintf("%s/keys/%s/%s.private", dkimDir, d.Name, old)}}) //nolint:errcheck // старый ключ уже не нужен
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.domain.dkim", Target: d.Name, IP: requestInfo(ctx).IP})
		return &mailDomainOutput{Status: http.StatusOK, Body: d}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-domain-dns", Method: http.MethodGet, Path: "/mail/domains/{name}/dns", Summary: "DNS records the domain needs, checked live", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailNameInput) (*mailDNSOutput, error) {
		d, err := s.mailDomainFor(ctx, strings.ToLower(in.Name))
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		return &mailDNSOutput{Body: s.mailDNS(ctx, d)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mailboxes-list", Method: http.MethodGet, Path: "/mail/mailboxes", Summary: "Mailboxes", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailboxesInput) (*mailboxesOutput, error) {
		p := principalFrom(ctx)
		var domainID, uid int64
		if in.Domain != "" {
			d, err := s.mailDomainFor(ctx, strings.ToLower(in.Domain))
			if err != nil {
				return nil, huma.Error404NotFound(err.Error())
			}
			domainID = d.ID
		} else if p.Role != store.RoleAdmin {
			uid = p.UserID
		}
		list, err := s.db.ListMailboxes(ctx, domainID, uid)
		if err != nil {
			return nil, err
		}
		return &mailboxesOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mailboxes-create", Method: http.MethodPost, Path: "/mail/mailboxes", Summary: "Create a mailbox", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *mailboxCreateInput) (*mailboxOutput, error) {
		p := principalFrom(ctx)
		local, domain, err := splitAddress(in.Body.Address)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if local == "@" {
			return nil, huma.Error422UnprocessableEntity("@domain is a catch-all; create it as an alias")
		}
		d, err := s.mailDomainFor(ctx, domain)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if d.SendOnly {
			return nil, huma.Error422UnprocessableEntity(sendOnlyRefusal(d.Name))
		}
		if !mailboxNameOK(in.Body.Name) {
			return nil, huma.Error422UnprocessableEntity(badMailboxName)
		}
		password, generated := in.Body.Password, false
		if password == "" {
			password, _ = auth.NewPassword(16)
			generated = true
		}
		hash, err := hashMailPassword(password)
		if err != nil {
			return nil, err
		}
		quota := 1024
		if in.Body.QuotaMB != nil {
			quota = *in.Body.QuotaMB // 0: no limit
		}
		b := &store.Mailbox{DomainID: d.ID, LocalPart: local, Address: local + "@" + domain, Name: in.Body.Name, PasswordHash: hash, QuotaMB: quota, Active: true}
		if err := s.db.CreateMailbox(ctx, b); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("mailbox already exists")
			}
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		b.Domain = d.Name
		c := s.loadMailConfig(ctx)
		out := &mailboxOutput{Status: http.StatusCreated, Body: apitypes.MailboxResponse{
			Mailbox: b,
			IMAP:    fmt.Sprintf("%s:993 (SSL/TLS), username %s", c.Hostname, b.Address),
			SMTP:    fmt.Sprintf("%s:465 (SSL/TLS) or 587 (STARTTLS)", c.Hostname),
		}}
		if generated {
			out.Body.Password = password
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.mailbox.create", Target: b.Address, IP: requestInfo(ctx).IP})
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mailboxes-update", Method: http.MethodPatch, Path: "/mail/mailboxes/{address}", Summary: "Change a mailbox (password, quota, state)", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailboxPatchInput) (*mailboxOutput, error) {
		p := principalFrom(ctx)
		b, err := s.mailboxFor(ctx, in.Address)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		password := in.Body.Password
		generated := false
		if in.Body.Password == "" && in.Body.Name == nil && in.Body.QuotaMB == nil && in.Body.Active == nil {
			password, _ = auth.NewPassword(16)
			generated = true
		}
		if password != "" {
			hash, err := hashMailPassword(password)
			if err != nil {
				return nil, err
			}
			b.PasswordHash = hash
		}
		if in.Body.Name != nil {
			if !mailboxNameOK(*in.Body.Name) {
				return nil, huma.Error422UnprocessableEntity(badMailboxName)
			}
			b.Name = *in.Body.Name
		}
		if in.Body.QuotaMB != nil {
			b.QuotaMB = *in.Body.QuotaMB // 0: no limit
		}
		if in.Body.Active != nil {
			b.Active = *in.Body.Active
		}
		if err := s.db.UpdateMailbox(ctx, b); err != nil {
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		out := &mailboxOutput{Status: http.StatusOK, Body: apitypes.MailboxResponse{Mailbox: b}}
		if generated {
			out.Body.Password = password
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.mailbox.update", Target: b.Address, IP: requestInfo(ctx).IP})
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mailboxes-delete", Method: http.MethodDelete, Path: "/mail/mailboxes/{address}", Summary: "Delete a mailbox", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *mailboxDeleteInput) (*struct{}, error) {
		p := principalFrom(ctx)
		b, err := s.mailboxFor(ctx, in.Address)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err := s.db.DeleteMailbox(ctx, b.ID); err != nil {
			return nil, err
		}
		// The maildir is the domain and the local part as path components:
		// a local part that came in another way (see mapToken) could name
		// another tenant's maildir.
		if in.Purge && (!mapToken(b.Domain) || !mapToken(b.LocalPart) || b.Address != b.LocalPart+"@"+b.Domain) {
			s.log.Warn("mail: the mailbox's local part is not its address; its maildir is left alone", "address", b.Address, "local_part", b.LocalPart)
		} else if in.Purge {
			if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{fmt.Sprintf("%s/%s/%s", mailBase, b.Domain, b.LocalPart)}, Recursive: true}); err != nil {
				s.log.Warn("mail: removing the maildir", "address", b.Address, "err", err)
			}
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.mailbox.delete", Target: b.Address, IP: requestInfo(ctx).IP, Details: map[string]any{"purge": in.Purge}})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-aliases-list", Method: http.MethodGet, Path: "/mail/aliases", Summary: "Aliases", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, in *mailboxesInput) (*aliasesOutput, error) {
		p := principalFrom(ctx)
		var domainID, uid int64
		if in.Domain != "" {
			d, err := s.mailDomainFor(ctx, strings.ToLower(in.Domain))
			if err != nil {
				return nil, huma.Error404NotFound(err.Error())
			}
			domainID = d.ID
		} else if p.Role != store.RoleAdmin {
			uid = p.UserID
		}
		list, err := s.db.ListMailAliases(ctx, domainID, uid)
		if err != nil {
			return nil, err
		}
		return &aliasesOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-aliases-create", Method: http.MethodPost, Path: "/mail/aliases", Summary: "Create or replace an alias", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *aliasInput) (*aliasOutput, error) {
		p := principalFrom(ctx)
		source, domain, err := splitAddress(in.Body.Address)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		d, err := s.mailDomainFor(ctx, domain)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if d.SendOnly {
			return nil, huma.Error422UnprocessableEntity(sendOnlyRefusal(d.Name))
		}
		dests := []string{}
		for _, to := range in.Body.Destinations {
			to = strings.ToLower(strings.TrimSpace(to))
			if to == "" {
				continue
			}
			if _, _, err := splitAddress(to); err != nil {
				return nil, huma.Error422UnprocessableEntity("recipient " + to + ": " + err.Error())
			}
			dests = append(dests, to)
		}
		if len(dests) == 0 {
			return nil, huma.Error422UnprocessableEntity("at least one recipient is required")
		}
		if _, err := s.db.GetMailbox(ctx, source+"@"+domain); err == nil {
			return nil, huma.Error409Conflict("this mailbox already exists; an alias with the same address would intercept its mail")
		}
		a := &store.MailAlias{DomainID: d.ID, Source: source, Destination: strings.Join(dests, ","), Active: true}
		if existing, err := s.db.GetMailAlias(ctx, d.ID, source); err == nil {
			existing.Destination, existing.Active = a.Destination, true
			if err := s.db.UpdateMailAlias(ctx, existing); err != nil {
				return nil, err
			}
			a = existing
		} else if err := s.db.CreateMailAlias(ctx, a); err != nil {
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		a.Domain, a.Address = d.Name, source+"@"+d.Name
		if source == "@" {
			a.Address = "@" + d.Name
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.alias.create", Target: a.Address, IP: requestInfo(ctx).IP})
		return &aliasOutput{Status: http.StatusCreated, Body: a}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-aliases-delete", Method: http.MethodDelete, Path: "/mail/aliases/{address}", Summary: "Delete an alias", Tags: []string{"mail"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *aliasDeleteInput) (*struct{}, error) {
		p := principalFrom(ctx)
		source, domain, err := splitAddress(in.Address)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		d, err := s.mailDomainFor(ctx, domain)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		a, err := s.db.GetMailAlias(ctx, d.ID, source)
		if err != nil {
			return nil, huma.Error404NotFound("alias not found")
		}
		if err := s.db.DeleteMailAlias(ctx, a.ID); err != nil {
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.alias.delete", Target: a.Address, IP: requestInfo(ctx).IP})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-webmail", Method: http.MethodPost, Path: "/mail/webmail", Summary: "Install Roundcube as a panel site (async)", Tags: []string{"mail"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *webmailInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		if c := s.loadMailConfig(ctx); !c.Installed {
			return nil, huma.Error422UnprocessableEntity("install the mail server first")
		}
		owner, err := s.ownerFor(ctx, in.Body.User)
		if err != nil {
			return nil, err
		}
		if in.Body.Port != 0 && (in.Body.Port < 1024 || in.Body.Port == s.panelPort() || in.Body.Port == 80 || in.Body.Port == 443) {
			return nil, huma.Error422UnprocessableEntity("choose a free port above 1024 for the webmail, e.g. 2096")
		}
		job, err := s.jobs.Enqueue(ctx, "mail.webmail", webmailPayload{Domain: strings.ToLower(in.Body.Domain), User: owner.Login, PHPVersion: in.Body.PHPVersion, Port: in.Body.Port}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.webmail", Target: in.Body.Domain, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

// mailboxFor resolves an address and checks the caller may touch it.
func (s *Server) mailboxFor(ctx context.Context, address string) (*store.Mailbox, error) {
	local, domain, err := splitAddress(address)
	if err != nil {
		return nil, err
	}
	if _, err := s.mailDomainFor(ctx, domain); err != nil {
		return nil, err
	}
	b, err := s.db.GetMailbox(ctx, local+"@"+domain)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("mailbox not found")
	}
	return b, err
}

// mailboxNameOK: the display name of a mailbox is one line of text. It is in
// no map, but every listing shows it, mp mail box list in the administrator's
// terminal among them.
func mailboxNameOK(name string) bool {
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

const badMailboxName = "name must be one line, without control characters"

// mailStatusFor is GET /mail as the caller may see it. An ordinary account
// learns nothing about the other tenants of the server: the counts are of its
// own domains, mailboxes and aliases, and of the warnings only the one about
// its own mail stays — certificates, ports and the encryption key are the
// administrator's to fix, and say how the whole server is set up.
func (s *Server) mailStatusFor(ctx context.Context) apitypes.MailStatus {
	st := s.mailStatus(ctx)
	p := principalFrom(ctx)
	if p.Role == store.RoleAdmin {
		return st
	}
	st.Domains, st.Mailboxes, st.Aliases, st.Warnings = 0, 0, 0, nil
	if p.UserID == 0 {
		return st // no account, nothing of its own
	}
	domains, err := s.db.ListMailDomains(ctx, p.UserID)
	if err != nil {
		return st
	}
	st.Domains = len(domains)
	for _, d := range domains {
		st.Mailboxes += d.Mailboxes
		st.Aliases += d.Aliases
	}
	if st.Installed && len(domains) == 0 {
		st.Warnings = []string{"no domains added yet"}
	}
	return st
}

// sendOnlyRefusal explains why a send-only domain takes no mailboxes.
func sendOnlyRefusal(domain string) string {
	return domain + " is send-only: its mail is received by another server, so it has no mailboxes or aliases here (mp mail domain set " + domain + " --send-only=false to receive here)"
}
