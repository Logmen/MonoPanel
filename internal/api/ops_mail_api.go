package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	Domain string `query:"domain" doc:"Показать ящики одного домена"`
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
	Purge   bool   `query:"purge" doc:"Удалить и письма на диске"`
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
		return nil, huma.Error422UnprocessableEntity("укажите владельца (user)")
	}
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("аккаунт не найден")
	}
	if owner.Role != store.RoleUser {
		return nil, huma.Error422UnprocessableEntity("владельцем может быть только пользовательский аккаунт")
	}
	return owner, nil
}

func (s *Server) registerMail() {
	huma.Register(s.api, huma.Operation{
		OperationID: "mail-status", Method: http.MethodGet, Path: "/mail", Summary: "Mail server status", Tags: []string{"mail"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*mailStatusOutput, error) {
		return &mailStatusOutput{Body: s.mailStatus(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "mail-install", Method: http.MethodPost, Path: "/mail/install", Summary: "Install postfix, dovecot and opendkim (async)", Tags: []string{"mail"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *mailInstallInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
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
		c := s.loadMailConfig(ctx)
		if in.Body.Hostname != "" {
			if !strings.Contains(in.Body.Hostname, ".") {
				return nil, huma.Error422UnprocessableEntity("имя почтового сервера должно быть полным, например mail.example.com")
			}
			c.Hostname = strings.ToLower(in.Body.Hostname)
		}
		if in.Body.MaxSizeMB > 0 {
			c.MaxSizeMB = in.Body.MaxSizeMB
		}
		if in.Body.POP3 != nil {
			c.POP3 = *in.Body.POP3
		}
		if in.Body.DKIM != nil {
			c.DKIM = *in.Body.DKIM
		}
		if in.Body.Port25 != nil {
			c.Port25 = *in.Body.Port25
		}
		if in.Body.WebmailPort != nil {
			port := *in.Body.WebmailPort
			if port != 0 && (port < 1024 || port == s.panelPort() || port == 80 || port == 443) {
				return nil, huma.Error422UnprocessableEntity("для вебпочты возьмите свободный порт выше 1024, например 2096")
			}
			c.WebmailPort = port
		}
		if in.Body.RBL != nil {
			list := []string{}
			for _, r := range *in.Body.RBL {
				if r = strings.TrimSpace(strings.ToLower(r)); r != "" {
					if !mailDomainRe.MatchString(strings.TrimSuffix(r, ".")) {
						return nil, huma.Error422UnprocessableEntity("чёрный список должен быть доменом, например zen.spamhaus.org")
					}
					list = append(list, r)
				}
			}
			c.RBL = list
		}
		if err := s.saveMailConfig(ctx, c); err != nil {
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
			return nil, huma.Error422UnprocessableEntity("почтовый сервер не установлен")
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Body.Name), "."))
		if !mailDomainRe.MatchString(name) {
			return nil, huma.Error422UnprocessableEntity("домен должен быть вида example.com")
		}
		owner, err := s.ownerFor(ctx, in.Body.User)
		if err != nil {
			return nil, err
		}
		d := &store.MailDomain{UserID: owner.ID, Name: name, Active: true, Lenient: in.Body.Lenient}
		wantDKIM := c.DKIM && (in.Body.DKIM == nil || *in.Body.DKIM)
		if wantDKIM {
			if s.secrets == nil {
				return nil, huma.Error422UnprocessableEntity("ключ шифрования недоступен, DKIM включить нельзя")
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
				return nil, huma.Error409Conflict("домен уже добавлен")
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
		if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{mailBase + "/" + d.Name, dkimDir + "/keys/" + d.Name}, Recursive: true}); err != nil {
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
		if err := s.db.UpdateMailDomain(ctx, d); err != nil {
			return nil, err
		}
		if err := s.applyMail(ctx, nil); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "mail.domain.update", Target: d.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"active": d.Active, "lenient": d.Lenient}})
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
			return nil, huma.Error422UnprocessableEntity("ключ шифрования недоступен")
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
		if old != "" {
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
			return nil, huma.Error422UnprocessableEntity("@домен — это catch-all, он создаётся как алиас")
		}
		d, err := s.mailDomainFor(ctx, domain)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
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
		quota := in.Body.QuotaMB
		if quota == 0 && in.Body.QuotaMB == 0 {
			quota = 1024
		}
		b := &store.Mailbox{DomainID: d.ID, LocalPart: local, Address: local + "@" + domain, Name: in.Body.Name, PasswordHash: hash, QuotaMB: quota, Active: true}
		if err := s.db.CreateMailbox(ctx, b); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("такой ящик уже есть")
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
			IMAP:    fmt.Sprintf("%s:993 (SSL/TLS), логин %s", c.Hostname, b.Address),
			SMTP:    fmt.Sprintf("%s:465 (SSL/TLS) или 587 (STARTTLS)", c.Hostname),
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
			b.Name = *in.Body.Name
		}
		if in.Body.QuotaMB != nil {
			b.QuotaMB = *in.Body.QuotaMB
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
		if in.Purge {
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
		dests := []string{}
		for _, to := range in.Body.Destinations {
			to = strings.ToLower(strings.TrimSpace(to))
			if to == "" {
				continue
			}
			if _, _, err := splitAddress(to); err != nil {
				return nil, huma.Error422UnprocessableEntity("получатель " + to + ": " + err.Error())
			}
			dests = append(dests, to)
		}
		if len(dests) == 0 {
			return nil, huma.Error422UnprocessableEntity("нужен хотя бы один получатель")
		}
		if _, err := s.db.GetMailbox(ctx, source+"@"+domain); err == nil {
			return nil, huma.Error409Conflict("такой ящик уже существует; алиас с тем же адресом перехватил бы его почту")
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
			return nil, huma.Error404NotFound("алиас не найден")
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
			return nil, huma.Error422UnprocessableEntity("сначала установите почтовый сервер")
		}
		owner, err := s.ownerFor(ctx, in.Body.User)
		if err != nil {
			return nil, err
		}
		if in.Body.Port != 0 && (in.Body.Port < 1024 || in.Body.Port == s.panelPort() || in.Body.Port == 80 || in.Body.Port == 443) {
			return nil, huma.Error422UnprocessableEntity("для вебпочты возьмите свободный порт выше 1024, например 2096")
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
		return nil, errors.New("ящик не найден")
	}
	return b, err
}
