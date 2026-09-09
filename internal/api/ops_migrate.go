package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/buildinfo"
	"monopanel/internal/store"
)

// The source side of a migration. The panel that gives an account away only
// ever reads: it renders its own state as a bundle and streams files and
// dumps. Everything that changes anything happens on the receiving side.

const migrateScopePrefix = "migrate:"

// migrateExcludes are paths inside a home that make no sense to carry over:
// logs belong to the old server, caches rebuild themselves.
var migrateExcludes = []string{"./data/logs", "./data/tmp", "./data/.cache"}

type migrateScopeInput struct {
	Scope string `query:"scope" required:"true" doc:"user:<login>"`
}

type migrateFilesInput struct {
	Scope  string `query:"scope" required:"true"`
	Part   string `query:"part" enum:"home,mail" default:"home"`
	Domain string `query:"domain,omitempty" doc:"Почтовый домен для part=mail"`
	Since  string `query:"since,omitempty" doc:"RFC3339: только файлы новее (досинхронизация)"`
}

type migrateDumpInput struct {
	Scope string `query:"scope" required:"true"`
	DB    string `query:"db" required:"true" pattern:"^[a-z0-9_]{1,64}$"`
}

type migrateBundleOutput struct {
	Body *apitypes.MigrationBundle
}

type migrateGrantInput struct {
	Body apitypes.MigrationGrantRequest
}

type migrateGrantOutput struct {
	Body apitypes.MigrationGrantResponse
}

// migrateLogin resolves the scope and checks that the caller may have it: a
// grant token carries exactly one scope and is good for nothing else.
func (s *Server) migrateLogin(ctx context.Context, scope string) (*store.User, error) {
	kind, login, ok := strings.Cut(strings.TrimSpace(scope), ":")
	if !ok || kind != "user" || login == "" {
		return nil, huma.Error422UnprocessableEntity("пока переносится только область вида user:<логин>")
	}
	p := principalFrom(ctx)
	if len(p.Scopes) > 0 {
		allowed := false
		for _, sc := range p.Scopes {
			if sc == migrateScopePrefix+"user:"+login {
				allowed = true
			}
		}
		if !allowed {
			return nil, huma.Error403Forbidden("токен выдан для другой области")
		}
	} else if p.Role != store.RoleAdmin {
		return nil, huma.Error403Forbidden("нужна роль администратора")
	}
	u, err := s.db.GetUserByLogin(ctx, login)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("аккаунт " + login + " не найден")
	}
	return u, err
}

// duBytes asks the host how much a directory weighs; a failure is not fatal,
// the number is only shown to a human before the copy starts.
func (s *Server) duBytes(ctx context.Context, dir string) int64 {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "du", Args: []string{"-sb", dir}, TimeoutSeconds: 120})
	if err != nil || res.ExitCode != 0 {
		return 0
	}
	fields := strings.Fields(res.Output)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}

// buildMigrationBundle renders everything the panel knows about an account.
func (s *Server) buildMigrationBundle(ctx context.Context, u *store.User, withSecrets bool) (*apitypes.MigrationBundle, error) {
	b := &apitypes.MigrationBundle{
		Panel: buildinfo.Version, Family: string(s.profile.Family()), Scope: "user:" + u.Login,
		Generated: time.Now().UTC(), User: u, SiteNginx: map[string]string{},
		Sizes: apitypes.MigrationSizes{Databases: map[string]int64{}},
	}
	if info, err := s.agent.SystemInfo(ctx); err == nil {
		b.Hostname = info.Hostname
	}
	var err error
	if b.Sites, err = s.db.ListSites(ctx, u.ID); err != nil {
		return nil, err
	}
	for _, site := range b.Sites {
		if n, err := s.siteNginx(ctx, site); err == nil && strings.TrimSpace(n.Custom) != "" {
			b.SiteNginx[site.Domain] = n.Custom
		}
	}
	if b.Databases, err = s.db.ListDatabases(ctx, u.ID); err != nil {
		return nil, err
	}
	if b.Cron, err = s.db.ListCronJobs(ctx, u.ID); err != nil {
		return nil, err
	}
	if b.Apps, err = s.db.ListApps(ctx, u.ID); err != nil {
		return nil, err
	}
	if b.MailDomains, err = s.db.ListMailDomains(ctx, u.ID); err != nil {
		return nil, err
	}
	if b.Mailboxes, err = s.db.ListMailboxes(ctx, 0, u.ID); err != nil {
		return nil, err
	}
	if b.MailAliases, err = s.db.ListMailAliases(ctx, 0, u.ID); err != nil {
		return nil, err
	}

	// Сертификаты едут файлами: пока DNS смотрит на старый сервер, ACME на
	// новом выпустить ничего не сможет, а HTTPS рваться не должен.
	b.Certificates = []apitypes.MigrationCert{}
	seen := map[string]bool{}
	for _, site := range b.Sites {
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			c, err := s.db.GetCertificateByName(ctx, name)
			if err != nil || seen[c.Name] || c.Status != store.CertValid || c.CertPath == "" {
				continue
			}
			seen[c.Name] = true
			mc := apitypes.MigrationCert{Name: c.Name, Names: c.Names, Kind: c.Kind, AutoRenew: c.AutoRenew, NotAfter: c.NotAfter}
			if withSecrets {
				cert, cerr := os.ReadFile(c.CertPath)
				key, kerr := os.ReadFile(c.KeyPath)
				if cerr != nil || kerr != nil {
					b.Notes = append(b.Notes, "сертификат "+c.Name+" не читается, приедет без файлов")
				} else {
					mc.Cert, mc.Key = string(cert), string(key)
				}
			}
			b.Certificates = append(b.Certificates, mc)
		}
	}

	home := s.homeOf(u)
	b.Sizes.FilesBytes = s.duBytes(ctx, home)
	for _, d := range b.Databases {
		b.Sizes.Databases[d.Name] = d.SizeBytes
	}
	for _, d := range b.MailDomains {
		b.Sizes.MailBytes += s.duBytes(ctx, mailBase+"/"+d.Name)
	}
	if len(b.Apps) > 0 {
		b.Notes = append(b.Notes, "app-сервисы приедут выключенными: их окружение и порты нужно проверить руками")
	}
	b.Notes = append(b.Notes, "логи сайтов и каталог data/tmp не переносятся")

	if withSecrets {
		sec := &apitypes.MigrationSecrets{
			PanelPassword: u.PasswordHash,
			Mailboxes:     map[string]string{},
			DKIM:          map[string]string{},
			DBUsers:       map[string]string{},
		}
		if sh, err := s.agent.UnixShadow(ctx, u.Login); err == nil && sh.Hash != "" {
			sec.UnixShadow = sh.Hash
		} else if u.UnixUID != nil {
			b.Notes = append(b.Notes, "пароль unix-аккаунта прочитать не удалось: SFTP-пароль придётся задать заново")
		}
		for _, box := range b.Mailboxes {
			full, err := s.db.GetMailbox(ctx, box.Address)
			if err == nil {
				sec.Mailboxes[box.Address] = full.PasswordHash
			}
		}
		for _, d := range b.MailDomains {
			if d.DKIMKeyEnc == "" || s.secrets == nil {
				continue
			}
			if pem, err := s.secrets.Decrypt(d.DKIMKeyEnc); err == nil {
				sec.DKIM[d.Name] = pem
			}
		}
		for _, d := range b.Databases {
			for _, acc := range d.Users {
				out, err := s.mysqlExec(ctx, fmt.Sprintf("SHOW CREATE USER '%s'@'%s';", acc.Name, acc.Host))
				if err == nil {
					sec.DBUsers[acc.Name+"@"+acc.Host] = strings.TrimSpace(out)
				}
			}
		}
		b.Secrets = sec
	}
	return b, nil
}

func (s *Server) registerMigrateSource() {
	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-grant", Method: http.MethodPost, Path: "/migrate/grant", Summary: "Allow another panel to take an account", Tags: []string{"migrate"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *migrateGrantInput) (*migrateGrantOutput, error) {
		p := principalFrom(ctx)
		u, err := s.migrateLogin(ctx, in.Body.Scope)
		if err != nil {
			return nil, err
		}
		hours := in.Body.Hours
		if hours == 0 {
			hours = 24
		}
		// root по локальному сокету аккаунта не имеет: токен принадлежит
		// единственному администратору, как и в mp token create.
		owner := p.UserID
		if owner == 0 {
			all, err := s.db.ListUsers(ctx)
			if err != nil {
				return nil, err
			}
			admins := []*store.User{}
			for _, a := range all {
				if a.Role == store.RoleAdmin {
					admins = append(admins, a)
				}
			}
			if len(admins) != 1 {
				return nil, huma.Error422UnprocessableEntity("администраторов несколько: выдайте токен из панели, а не от root")
			}
			owner = admins[0].ID
		}
		plain, err := auth.NewToken(24)
		if err != nil {
			return nil, err
		}
		exp := time.Now().Add(time.Duration(hours) * time.Hour)
		rec := &store.APIToken{
			UserID: owner, Name: "переезд " + u.Login, Hash: auth.HashToken(plain),
			Scopes: []string{migrateScopePrefix + "user:" + u.Login}, ExpiresAt: &exp,
		}
		if err := s.db.CreateAPIToken(ctx, rec); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "migrate.grant", Target: u.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"hours": hours}})
		// В подсказке должен быть адрес, по которому панель действительно
		// отвечает: имя из конфигурации порт не содержит.
		host := s.cfg.Web.Hostname
		if host == "" {
			host = requestInfo(ctx).Host
		}
		if !strings.Contains(host, ":") {
			host = fmt.Sprintf("%s:%d", host, s.panelPort())
		}
		return &migrateGrantOutput{Body: apitypes.MigrationGrantResponse{
			Token: plain, Scope: "user:" + u.Login, ExpiresAt: exp,
			Command: fmt.Sprintf("mp migrate plan --source https://%s --token %s --scope user:%s", host, plain, u.Login),
		}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-plan-source", Method: http.MethodGet, Path: "/migrate/plan", Summary: "What this panel is ready to hand over (no secrets)", Tags: []string{"migrate"}, Security: secured,
	}, func(ctx context.Context, in *migrateScopeInput) (*migrateBundleOutput, error) {
		u, err := s.migrateLogin(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		b, err := s.buildMigrationBundle(ctx, u, false)
		if err != nil {
			return nil, err
		}
		return &migrateBundleOutput{Body: b}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-state", Method: http.MethodGet, Path: "/migrate/state", Summary: "Full state including password hashes and keys", Tags: []string{"migrate"}, Security: secured,
	}, func(ctx context.Context, in *migrateScopeInput) (*migrateBundleOutput, error) {
		u, err := s.migrateLogin(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		b, err := s.buildMigrationBundle(ctx, u, true)
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: principalFrom(ctx).Login, Action: "migrate.state", Target: u.Login, IP: requestInfo(ctx).IP})
		return &migrateBundleOutput{Body: b}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-files", Method: http.MethodGet, Path: "/migrate/files", Summary: "Stream the account files as a tar", Tags: []string{"migrate"}, Security: secured,
	}, func(ctx context.Context, in *migrateFilesInput) (*huma.StreamResponse, error) {
		u, err := s.migrateLogin(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		dir := s.homeOf(u)
		excludes := migrateExcludes
		if in.Part == "mail" {
			domain, derr := s.db.GetMailDomain(ctx, in.Domain)
			if derr != nil || domain.UserID != u.ID {
				return nil, huma.Error404NotFound("почтовый домен не принадлежит этому аккаунту")
			}
			dir, excludes = mailBase+"/"+domain.Name, nil
		}
		args := []string{"--create", "--file", "-", "--directory", dir, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed"}
		for _, e := range excludes {
			args = append(args, "--exclude="+e)
		}
		if in.Since != "" {
			t, perr := time.Parse(time.RFC3339, in.Since)
			if perr != nil {
				return nil, huma.Error422UnprocessableEntity("since: ожидается время в формате RFC3339")
			}
			args = append(args, "--newer-mtime="+t.UTC().Format("2006-01-02 15:04:05"))
		}
		args = append(args, ".")
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-tar")
			if err := s.agent.StreamOut(ctx, &agent.StreamRequest{Name: "tar", Args: args}, hctx.BodyWriter()); err != nil {
				// Поток уже пошёл: сказать об ошибке можно только в журнал,
				// принимающая сторона увидит оборванный архив.
				s.log.Error("migrate: files stream", "scope", in.Scope, "err", err)
			}
		}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-dump", Method: http.MethodGet, Path: "/migrate/dump", Summary: "Stream a database dump", Tags: []string{"migrate"}, Security: secured,
	}, func(ctx context.Context, in *migrateDumpInput) (*huma.StreamResponse, error) {
		u, err := s.migrateLogin(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		db, err := s.db.GetDatabaseByName(ctx, in.DB)
		if err != nil || db.UserID != u.ID {
			return nil, huma.Error404NotFound("база не принадлежит этому аккаунту")
		}
		args := []string{"--protocol=socket", "--single-transaction", "--routines", "--triggers", "--events", "--databases", db.Name}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/sql")
			if err := s.agent.StreamOut(ctx, &agent.StreamRequest{Name: "mysqldump", Args: args}, hctx.BodyWriter()); err != nil {
				s.log.Error("migrate: dump stream", "db", db.Name, "err", err)
			}
		}}, nil
	})
}
