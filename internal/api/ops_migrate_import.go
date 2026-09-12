package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// The receiving side. Everything that changes state happens here: the source
// only ever reads, so a botched migration cannot damage the server people are
// still working on.

// migrateSource is where an account comes from. Another MonoPanel answers
// over its API; a foreign panel is read over ssh by an adapter. The import
// itself does not care which: it asks for the state, the file streams and
// the dumps and builds everything here from those.
type migrateSource interface {
	// bundle renders the declarative state; withSecrets adds the hashes,
	// keys and authentication strings the plan must not carry.
	bundle(ctx context.Context, scope string, withSecrets bool) (*apitypes.MigrationBundle, error)
	// files streams a tar whose members are relative to the destination
	// directory of the part (home | mail).
	files(ctx context.Context, req migrateFilesRequest) (io.ReadCloser, error)
	// dump streams mysqldump --databases of one database.
	dump(ctx context.Context, scope, db string) (io.ReadCloser, error)
	// settle runs once the files are here and the sites exist: чужие панели
	// правят в конфигах сайтов пути старого сервера.
	settle(ctx context.Context, jc *jobs.Context, u *store.User) error
	close()
}

type migrateFilesRequest struct {
	scope, part, domain, since string
	// home is the account's home on this server; adapters rewrite absolute
	// paths (symlink targets) of the old server to it.
	home string
}

// migrateHTTP talks to the MonoPanel we are taking an account from.
type migrateHTTP struct {
	base  string
	token string
	http  *http.Client
}

// openMigrateSource picks the adapter for the request and connects to it.
func (s *Server) openMigrateSource(ctx context.Context, req apitypes.MigrationSourceRequest) (migrateSource, error) {
	switch req.Panel {
	case "", "monopanel":
		return newMigrateHTTP(req)
	case "fastpanel":
		return s.openFastpanel(ctx, req)
	case "bitrixvm":
		return s.openBitrixVM(ctx, req)
	}
	return nil, huma.Error422UnprocessableEntity("неизвестная панель-источник " + req.Panel + ": monopanel, fastpanel или bitrixvm")
}

func newMigrateHTTP(req apitypes.MigrationSourceRequest) (*migrateHTTP, error) {
	if strings.TrimSpace(req.Token) == "" {
		return nil, huma.Error422UnprocessableEntity("нужен токен источника: mp migrate grant на старой панели")
	}
	if !strings.HasPrefix(req.Scope, "user:") {
		return nil, huma.Error422UnprocessableEntity("область переноса: user:<логин>")
	}
	raw := strings.TrimSpace(req.Source)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, huma.Error422UnprocessableEntity("адрес источника должен быть вида https://panel.example.com:8443")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: req.Insecure, MinVersion: tls.VersionTLS12}} //nolint:gosec // явный флаг: у переезжающей панели сертификата может ещё не быть
	return &migrateHTTP{base: u.String() + "/api/v1", token: strings.TrimSpace(req.Token), http: &http.Client{Transport: tr}}, nil
}

func (m *migrateHTTP) close() {}

func (m *migrateHTTP) settle(context.Context, *jobs.Context, *store.User) error { return nil }

func (m *migrateHTTP) get(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	res, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("источник недоступен: %w", err)
	}
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		msg := strings.TrimSpace(string(body))
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("источник не принял токен (%s): выдайте новый — mp migrate grant", res.Status)
		}
		return nil, fmt.Errorf("источник ответил %s: %s", res.Status, msg)
	}
	return res, nil
}

// bundle fetches the declarative state; withSecrets picks the endpoint that
// also carries hashes and keys.
func (m *migrateHTTP) bundle(ctx context.Context, scope string, withSecrets bool) (*apitypes.MigrationBundle, error) {
	path := "/migrate/plan"
	if withSecrets {
		path = "/migrate/state"
	}
	res, err := m.get(ctx, path, url.Values{"scope": {scope}})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var b apitypes.MigrationBundle
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("ответ источника не разобрать: %w", err)
	}
	if b.User == nil {
		return nil, errors.New("источник не отдал аккаунт")
	}
	if b.Panel == "" || (b.Panel[0] >= '0' && b.Panel[0] <= '9') {
		b.Panel = strings.TrimSpace("MonoPanel " + b.Panel)
	}
	return &b, nil
}

func (m *migrateHTTP) files(ctx context.Context, req migrateFilesRequest) (io.ReadCloser, error) {
	q := url.Values{"scope": {req.scope}, "part": {req.part}}
	if req.domain != "" {
		q.Set("domain", req.domain)
	}
	if req.since != "" {
		q.Set("since", req.since)
	}
	res, err := m.get(ctx, "/migrate/files", q)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (m *migrateHTTP) dump(ctx context.Context, scope, db string) (io.ReadCloser, error) {
	res, err := m.get(ctx, "/migrate/dump", url.Values{"scope": {scope}, "db": {db}})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

type migratePlanInput struct {
	Body apitypes.MigrationSourceRequest
}

type migratePlanOutput struct {
	Body *apitypes.MigrationPlan
}

// migrationPlan compares what the source offers with what is already here.
// Ничего не создаётся: это ответ на вопрос «что будет, если запустить».
func (s *Server) migrationPlan(ctx context.Context, req apitypes.MigrationSourceRequest, b *apitypes.MigrationBundle) *apitypes.MigrationPlan {
	login := b.User.Login
	if req.As != "" {
		login = req.As
	}
	plan := &apitypes.MigrationPlan{Bundle: b, Login: login, Conflicts: []apitypes.MigrationIssue{}, Warnings: []apitypes.MigrationIssue{}}
	block := func(kind, target, text, fix string) {
		plan.Conflicts = append(plan.Conflicts, apitypes.MigrationIssue{Kind: kind, Target: target, Text: text, Fix: fix})
	}
	warn := func(kind, target, text, fix string) {
		plan.Warnings = append(plan.Warnings, apitypes.MigrationIssue{Kind: kind, Target: target, Text: text, Fix: fix})
	}

	// Another OS family is fine: every configuration is rendered here from
	// the state, with this server's PHP layout and service names. Only what
	// the user wrote by hand (nginx directives, cron commands) may still
	// point at the old paths.
	if b.Family != "" && b.Family != string(s.profile.Family()) {
		warn("user", login, "источник на "+b.Family+", здесь "+string(s.profile.Family())+": конфигурация будет перегенерирована под этот сервер", "проверьте свои директивы nginx и команды cron — в них могут быть пути старой ОС")
	}
	if _, err := s.db.GetUserByLogin(ctx, login); err == nil {
		block("user", login, "аккаунт с таким логином уже есть", "примите под другим логином: --as <логин>")
	}
	for _, site := range b.Sites {
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			if existing, err := s.db.GetSiteByDomain(ctx, name); err == nil {
				block("site", name, "сайт уже обслуживается панелью (владелец "+existing.Login+")", "удалите его здесь или исключите из переноса")
			}
		}
		if site.PHPVersion != "" {
			if err := s.checkPHPInstalled(ctx, site.PHPVersion); err != nil {
				block("php", site.PHPVersion, "ветка PHP "+site.PHPVersion+" не установлена, сайту "+site.Domain+" не на чем работать", "mp php install "+site.PHPVersion)
			}
		}
	}
	if len(b.Databases) > 0 {
		if _, err := s.dbInstance(ctx); err != nil {
			block("database", "", "приезжают базы, а сервер БД здесь не установлен", "mp stack install percona")
		}
		for _, d := range b.Databases {
			if _, err := s.db.GetDatabaseByName(ctx, d.Name); err == nil {
				block("database", d.Name, "база с таким именем уже есть", "переименуйте или удалите её здесь")
			}
		}
	}
	if len(b.MailDomains) > 0 {
		c := s.loadMailConfig(ctx)
		if !c.Installed {
			block("mail", "", "приезжают почтовые домены, а почтовый сервер здесь не установлен", "mp mail install")
		}
		for _, d := range b.MailDomains {
			if _, err := s.db.GetMailDomain(ctx, d.Name); err == nil {
				block("mail", d.Name, "почтовый домен уже обслуживается здесь", "удалите его здесь или исключите из переноса")
			}
		}
	}
	for _, cert := range b.Certificates {
		if cert.Cert == "" {
			warn("cert", cert.Name, "сертификат приедет без файлов: до выпуска нового сайт будет работать по HTTP", "после переключения DNS: mp ssl issue "+cert.Name)
		}
	}
	if len(b.Apps) > 0 {
		warn("user", login, fmt.Sprintf("app-сервисов: %d — они приедут выключенными", len(b.Apps)), "проверьте окружение и порты, затем mp app start")
	}

	// Место: файлы и почта распакуются целиком, дампы ещё и развернутся.
	need := b.Sizes.FilesBytes + b.Sizes.MailBytes
	for _, n := range b.Sizes.Databases {
		need += n * 2
	}
	if info, err := s.agent.SystemInfo(ctx); err == nil && need > 0 {
		for _, d := range info.Disks {
			if d.Mount != "/" {
				continue
			}
			if int64(d.FreeBytes) < need {
				block("disk", d.Mount, fmt.Sprintf("нужно около %d МБ, свободно %d МБ", need>>20, d.FreeBytes>>20), "освободите место или перенесите по частям")
			} else if int64(d.FreeBytes) < need*2 {
				warn("disk", d.Mount, fmt.Sprintf("после переноса останется меньше половины диска (нужно %d МБ, свободно %d МБ)", need>>20, d.FreeBytes>>20), "")
			}
		}
	}
	plan.OK = len(plan.Conflicts) == 0
	return plan
}

type migratePayload struct {
	apitypes.MigrationSourceRequest
	Login string `json:"login"`
	// Учётные данные ssh лежат в задаче только зашифрованными.
	PasswordEnc string `json:"password_enc,omitempty"`
	KeyEnc      string `json:"key_enc,omitempty"`
}

// sealMigratePayload moves the ssh credentials of a foreign source into the
// encrypted fields: the jobs table must never hold a root password in the open.
func (s *Server) sealMigratePayload(req apitypes.MigrationSourceRequest, login string) (migratePayload, error) {
	p := migratePayload{MigrationSourceRequest: req, Login: login}
	if req.Password == "" && req.Key == "" {
		return p, nil
	}
	if s.secrets == nil {
		return p, huma.Error422UnprocessableEntity("хранилище секретов панели не открыто: пароль ssh сохранить некуда")
	}
	var err error
	if req.Password != "" {
		if p.PasswordEnc, err = s.secrets.Encrypt(req.Password); err != nil {
			return p, err
		}
	}
	if req.Key != "" {
		if p.KeyEnc, err = s.secrets.Encrypt(req.Key); err != nil {
			return p, err
		}
	}
	p.Password, p.Key = "", ""
	return p, nil
}

func (s *Server) unsealMigratePayload(p *migratePayload) error {
	var err error
	if p.PasswordEnc != "" {
		if p.Password, err = s.secrets.Decrypt(p.PasswordEnc); err != nil {
			return err
		}
	}
	if p.KeyEnc != "" {
		if p.Key, err = s.secrets.Decrypt(p.KeyEnc); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) registerMigrateImport() {
	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-plan", Method: http.MethodPost, Path: "/migrate/plan", Summary: "Dry run: what would arrive and what stands in the way", Tags: []string{"migrate"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *migratePlanInput) (*migratePlanOutput, error) {
		src, err := s.openMigrateSource(ctx, in.Body)
		if err != nil {
			return nil, err
		}
		defer src.close()
		b, err := src.bundle(ctx, in.Body.Scope, false)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		return &migratePlanOutput{Body: s.migrationPlan(ctx, in.Body, b)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "migrate-run", Method: http.MethodPost, Path: "/migrate/run", Summary: "Take the account over (async)", Tags: []string{"migrate"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *migratePlanInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		src, err := s.openMigrateSource(ctx, in.Body)
		if err != nil {
			return nil, err
		}
		defer src.close()
		b, err := src.bundle(ctx, in.Body.Scope, false)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		plan := s.migrationPlan(ctx, in.Body, b)
		if !plan.OK {
			return nil, huma.Error409Conflict("перенос не запущен: " + plan.Conflicts[0].Text)
		}
		payload, err := s.sealMigratePayload(in.Body, plan.Login)
		if err != nil {
			return nil, err
		}
		if payload.Scope == "" {
			payload.Scope = b.Scope
		}
		job, err := s.jobs.Enqueue(ctx, "migrate.run", payload, jobs.WithLockKey("user:"+plan.Login), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "migrate.run", Target: plan.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"source": in.Body.Source, "scope": in.Body.Scope}})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

// jobMigrateRun takes an account over: account, files, databases, state.
func (s *Server) jobMigrateRun(ctx context.Context, jc *jobs.Context) error {
	var p migratePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	if err := s.unsealMigratePayload(&p); err != nil {
		return err
	}
	src, err := s.openMigrateSource(ctx, p.MigrationSourceRequest)
	if err != nil {
		return err
	}
	defer src.close()
	jc.Progress(2, "состояние источника")
	b, err := src.bundle(ctx, p.Scope, true)
	if err != nil {
		return err
	}
	if b.Secrets == nil {
		return errors.New("источник не отдал секреты: токен выдан только на просмотр")
	}
	if p.Scope == "" {
		p.Scope = b.Scope
	}
	jc.Logf("источник: %s (%s), аккаунт %s → %s", b.Hostname, b.Panel, b.User.Login, p.Login)
	for _, n := range b.Notes {
		jc.Logf("· %s", n)
	}

	jc.Progress(5, "аккаунт")
	u := &store.User{
		Login: p.Login, Role: store.RoleUser, Email: b.User.Email, Shell: b.User.Shell,
		QuotaMB: b.User.QuotaMB, PasswordHash: b.Secrets.PanelPassword, Status: store.UserPending,
	}
	if err := s.db.CreateUser(ctx, u); err != nil {
		return fmt.Errorf("аккаунт %s: %w", p.Login, err)
	}
	if err := s.provisionUser(ctx, userProvisionPayload{UserID: u.ID, Login: u.Login, Shell: u.Shell}, jc.Logf, func(int, string) {}); err != nil {
		return err
	}
	if b.Secrets.UnixShadow != "" {
		if err := s.agent.SetUnixPasswordHash(ctx, u.Login, b.Secrets.UnixShadow); err != nil {
			jc.Logf("предупреждение: пароль SFTP не перенесён: %v", err)
		} else {
			jc.Logf("пароли панели и SFTP перенесены хешами — пользователь входит теми же")
		}
	}
	u, err = s.db.GetUserByLogin(ctx, p.Login)
	if err != nil {
		return err
	}

	jc.Progress(15, "файлы")
	if err := s.migrateFiles(ctx, jc, src, p, u, "home", "", ""); err != nil {
		return err
	}

	jc.Progress(45, "базы данных")
	for _, d := range b.Databases {
		if err := s.migrateDatabase(ctx, jc, src, p, u, d, b.Secrets.DBUsers); err != nil {
			return err
		}
	}

	jc.Progress(65, "сайты")
	for _, site := range b.Sites {
		if err := s.migrateSite(ctx, jc, u, site, b.SiteNginx[site.Domain]); err != nil {
			return err
		}
	}

	jc.Progress(80, "сертификаты")
	for _, cert := range b.Certificates {
		if cert.Cert == "" || cert.Key == "" {
			continue
		}
		if err := s.importCertificate(ctx, u, cert); err != nil {
			jc.Logf("сертификат %s: %v", cert.Name, err)
			continue
		}
		jc.Logf("сертификат %s перенесён (до %s)", cert.Name, cert.NotAfter.Format("2006-01-02"))
	}
	if err := src.settle(ctx, jc, u); err != nil {
		jc.Logf("предупреждение: %v", err)
	}
	for _, site := range b.Sites {
		if _, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: siteID(ctx, s, site.Domain)}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(jc.RequestedBy)); err != nil {
			jc.Logf("сайт %s: %v", site.Domain, err)
		}
	}

	jc.Progress(88, "cron и app-сервисы")
	for _, j := range b.Cron {
		if err := s.db.CreateCronJob(ctx, &store.CronJob{UserID: u.ID, Schedule: j.Schedule, Command: j.Command, Comment: j.Comment, Enabled: j.Enabled}); err != nil {
			jc.Logf("cron: %v", err)
		}
	}
	if len(b.Cron) > 0 {
		if err := s.applyCrontab(ctx, u); err != nil {
			jc.Logf("crontab: %v", err)
		} else {
			jc.Logf("заданий cron перенесено: %d", len(b.Cron))
		}
	}
	for _, a := range b.Apps {
		app := *a
		app.ID, app.UserID, app.Enabled, app.Status = 0, u.ID, false, "stopped"
		if err := s.db.CreateApp(ctx, &app); err != nil {
			jc.Logf("app %s: %v", a.Name, err)
		}
	}
	if len(b.Apps) > 0 {
		jc.Logf("app-сервисов перенесено: %d — они выключены, проверьте окружение и запустите", len(b.Apps))
	}

	jc.Progress(92, "почта")
	if err := s.migrateMail(ctx, jc, src, p, u, b); err != nil {
		jc.Logf("почта: %v", err)
	}

	if err := s.db.SetUserStatus(ctx, u.ID, store.UserActive); err != nil {
		return err
	}
	jc.Progress(100, fmt.Sprintf("аккаунт %s перенесён: сайтов %d, баз %d, ящиков %d", p.Login, len(b.Sites), len(b.Databases), len(b.Mailboxes)))
	jc.Logf("DNS ещё смотрит на источник. Проверьте сайты по IP этого сервера и переключайте записи.")
	return nil
}

// siteID looks a freshly created site up; the bundle carries the source ids.
func siteID(ctx context.Context, s *Server, domain string) int64 {
	if site, err := s.db.GetSiteByDomain(ctx, domain); err == nil {
		return site.ID
	}
	return 0
}

// migrateFiles streams a tar from the source straight into tar -x here: файлы
// нигде не складываются целиком, поток идёт сквозь обе панели.
func (s *Server) migrateFiles(ctx context.Context, jc *jobs.Context, src migrateSource, p migratePayload, u *store.User, part, domain, since string) error {
	body, err := src.files(ctx, migrateFilesRequest{scope: p.Scope, part: part, domain: domain, since: since, home: s.homeOf(u)})
	if err != nil {
		return err
	}
	defer body.Close()
	dest := s.homeOf(u)
	owner, group := u.Login, u.Login
	if part == "mail" {
		dest, owner, group = mailBase+"/"+domain, vmailUser, vmailUser
		if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: dest, Mode: 0o750, Owner: owner, Group: group}}}); err != nil {
			return err
		}
	}
	args := []string{"--extract", "--file", "-", "--directory", dest, "--no-same-owner", "--warning=no-timestamp"}
	out, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "tar", Args: args}, body)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("tar: %s", strings.TrimSpace(out.Output))
	}
	// Распаковка идёт от root, поэтому владельца возвращаем отдельно.
	ch, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: dest, Owner: owner, Group: group, Recursive: true})
	if err != nil {
		return err
	}
	jc.Logf("%s: файлы распакованы в %s, владелец исправлен у %d объектов", part, dest, ch.Changed)
	s.relabel(ctx, jc, dest, true)
	return nil
}

// migrateDatabase recreates a database, its accounts (with the original
// password hashes) and streams the dump into mysql.
func (s *Server) migrateDatabase(ctx context.Context, jc *jobs.Context, src migrateSource, p migratePayload, u *store.User, d *store.Database, auths map[string]string) error {
	if d.Charset == "" {
		d.Charset, d.Collation = "utf8mb4", "utf8mb4_0900_ai_ci"
	}
	if d.Collation == "" {
		d.Collation = d.Charset + "_general_ci"
	}
	if _, err := s.mysqlExec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET %s COLLATE %s;", d.Name, d.Charset, d.Collation)); err != nil {
		return err
	}
	row := &store.Database{UserID: u.ID, Name: d.Name, Charset: d.Charset, Collation: d.Collation}
	if err := s.db.CreateDatabase(ctx, row); err != nil && !errors.Is(err, store.ErrExists) {
		return err
	}
	for _, acc := range d.Users {
		create := auths[acc.Name+"@"+acc.Host]
		if create == "" {
			jc.Logf("аккаунт %s@%s приедет без пароля: источник не отдал строку аутентификации", acc.Name, acc.Host)
			continue
		}
		// SHOW CREATE USER отдаёт готовый CREATE USER с хешем пароля; от
		// чужих панелей приходит то же самое, иногда со старым хешем
		// mysql_native_password, который сервер здесь не принимает без
		// включённого плагина.
		create = portableCreateUser(create)
		if strings.Contains(create, "mysql_native_password") {
			if inst, err := s.dbInstance(ctx); err == nil {
				if err := s.ensureNativePassword(ctx, inst); err != nil {
					jc.Logf("mysql_native_password для %s@%s: %v", acc.Name, acc.Host, err)
				}
			}
		}
		sql := strings.TrimSuffix(strings.TrimSpace(create), ";") + ";\n"
		sql += fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%s';\nFLUSH PRIVILEGES;\n", d.Name, acc.Name, acc.Host)
		if _, err := s.mysqlExec(ctx, sql); err != nil {
			return fmt.Errorf("аккаунт %s@%s: %w", acc.Name, acc.Host, err)
		}
		s.db.CreateDBUser(ctx, &store.DBUser{UserID: u.ID, DatabaseID: &row.ID, Name: acc.Name, Host: acc.Host, AuthPlugin: acc.AuthPlugin}) //nolint:errcheck // строка учётки — украшение списка баз
	}
	body, err := src.dump(ctx, p.Scope, d.Name)
	if err != nil {
		return err
	}
	defer body.Close()
	out, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "mysql", Args: []string{"--protocol=socket"}}, body)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("mysql: %s", strings.TrimSpace(out.Output))
	}
	jc.Logf("база %s перенесена вместе с аккаунтами (пароли прежние)", d.Name)
	return nil
}

// MariaDB spells a native hash as IDENTIFIED BY PASSWORD '*…' (before 10.4)
// or IDENTIFIED VIA mysql_native_password USING '*…'; MySQL 8 takes neither.
var (
	mariaDBPasswordRe = regexp.MustCompile(`(?i)IDENTIFIED\s+BY\s+PASSWORD\s+'(\*[0-9A-F]{40})'`)
	mariaDBViaRe      = regexp.MustCompile(`(?i)IDENTIFIED\s+VIA\s+mysql_native_password\s+USING\s+'(\*[0-9A-F]{40})'`)
)

// portableCreateUser rewrites a CREATE USER from another server into the
// form MySQL 8 accepts.
func portableCreateUser(create string) string {
	create = strings.TrimSpace(create)
	create = mariaDBPasswordRe.ReplaceAllString(create, "IDENTIFIED WITH mysql_native_password AS '$1'")
	create = mariaDBViaRe.ReplaceAllString(create, "IDENTIFIED WITH mysql_native_password AS '$1'")
	return create
}

// migrateSite recreates a site row and its custom nginx directives.
func (s *Server) migrateSite(ctx context.Context, jc *jobs.Context, u *store.User, src *store.Site, custom string) error {
	site := *src
	site.ID, site.UserID, site.CertificateID = 0, u.ID, nil
	site.Status = store.SitePending
	site.IP = ""
	if err := s.db.CreateSite(ctx, &site); err != nil {
		return fmt.Errorf("сайт %s: %w", src.Domain, err)
	}
	if strings.TrimSpace(custom) != "" {
		if err := s.writeSiteCustomNginx(ctx, &site, custom); err != nil {
			jc.Logf("сайт %s: свои директивы nginx не перенесены: %v", site.Domain, err)
		}
	}
	jc.Logf("сайт %s (PHP %s, режим %s)", site.Domain, site.PHPVersion, site.Mode)
	return nil
}

// migrateMail moves domains, mailboxes with their password hashes, aliases and
// the Maildirs themselves.
func (s *Server) migrateMail(ctx context.Context, jc *jobs.Context, src migrateSource, p migratePayload, u *store.User, b *apitypes.MigrationBundle) error {
	if len(b.MailDomains) == 0 {
		return nil
	}
	if c := s.loadMailConfig(ctx); !c.Installed {
		return errors.New("почтовый сервер здесь не установлен, домены пропущены")
	}
	byID := map[int64]string{}
	for _, d := range b.MailDomains {
		row := &store.MailDomain{UserID: u.ID, Name: d.Name, Active: d.Active, Lenient: d.Lenient, DKIMSelector: d.DKIMSelector, DKIMPublic: d.DKIMPublic}
		if pem := b.Secrets.DKIM[d.Name]; pem != "" && s.secrets != nil {
			enc, err := s.secrets.Encrypt(pem)
			if err != nil {
				return err
			}
			row.DKIMKeyEnc = enc
		}
		if err := s.db.CreateMailDomain(ctx, row); err != nil {
			return fmt.Errorf("домен %s: %w", d.Name, err)
		}
		byID[d.ID] = d.Name
		if err := s.migrateFiles(ctx, jc, src, p, u, "mail", d.Name, ""); err != nil {
			jc.Logf("почта %s: письма не перенесены: %v", d.Name, err)
		}
	}
	for _, box := range b.Mailboxes {
		domain := byID[box.DomainID]
		if domain == "" {
			continue
		}
		row, err := s.db.GetMailDomain(ctx, domain)
		if err != nil {
			continue
		}
		hash := b.Secrets.Mailboxes[box.Address]
		if hash == "" {
			jc.Logf("ящик %s приедет без пароля", box.Address)
		}
		if err := s.db.CreateMailbox(ctx, &store.Mailbox{
			DomainID: row.ID, LocalPart: box.LocalPart, Address: box.Address, Name: box.Name,
			PasswordHash: hash, QuotaMB: box.QuotaMB, Active: box.Active,
		}); err != nil {
			jc.Logf("ящик %s: %v", box.Address, err)
		}
	}
	for _, a := range b.MailAliases {
		domain := byID[a.DomainID]
		if domain == "" {
			continue
		}
		row, err := s.db.GetMailDomain(ctx, domain)
		if err != nil {
			continue
		}
		if err := s.db.CreateMailAlias(ctx, &store.MailAlias{DomainID: row.ID, Source: a.Source, Destination: a.Destination, Active: a.Active}); err != nil {
			jc.Logf("алиас %s: %v", a.Address, err)
		}
	}
	if err := s.applyMail(ctx, jc.Logf); err != nil {
		return err
	}
	jc.Logf("почта перенесена: доменов %d, ящиков %d, алиасов %d (пароли прежние)", len(b.MailDomains), len(b.Mailboxes), len(b.MailAliases))
	return nil
}

// importCertificate installs a certificate that came with the account, so the
// site keeps answering by HTTPS from the first second after the DNS switch —
// ACME can only issue its own once the name points here.
func (s *Server) importCertificate(ctx context.Context, u *store.User, mc apitypes.MigrationCert) error {
	certPEM := []byte(strings.TrimSpace(mc.Cert) + "\n")
	keyPEM := []byte(strings.TrimSpace(mc.Key) + "\n")
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("сертификат и ключ не сходятся: %w", err)
	}
	info, err := acme.ParseCertificatePEM(certPEM)
	if err != nil {
		return err
	}
	if time.Now().After(info.NotAfter) {
		return fmt.Errorf("истёк %s", info.NotAfter.Format("2006-01-02"))
	}
	leaf, chain := splitChain(certPEM)
	fullchain := append(append([]byte{}, leaf...), chain...)
	certPath, keyPath, chainPath, err := s.acme.WriteFiles(mc.Name, fullchain, keyPEM, chain)
	if err != nil {
		return err
	}
	c, err := s.db.GetCertificateByName(ctx, mc.Name)
	if errors.Is(err, store.ErrNotFound) {
		c = &store.Certificate{Name: mc.Name}
	} else if err != nil {
		return err
	}
	uid := u.ID
	now := time.Now()
	nb, na := info.NotBefore, info.NotAfter
	c.UserID, c.Names, c.Kind, c.AutoRenew = &uid, mc.Names, mc.Kind, mc.AutoRenew
	if c.Kind == store.CertKindACME {
		c.DirectoryURL = acme.LetsEncrypt
		c.Email, _ = s.db.GetSetting(ctx, settingACMEEmail)
	}
	c.CertPath, c.KeyPath, c.ChainPath = certPath, keyPath, chainPath
	c.Issuer, c.Serial = info.Issuer, info.Serial
	c.NotBefore, c.NotAfter, c.LastAttempt = &nb, &na, &now
	c.Status, c.LastError = store.CertValid, ""
	return s.db.UpsertCertificate(ctx, c)
}

// writeSiteCustomNginx puts the directives an administrator wrote by hand on
// the old server back into sites/<domain>.d/custom.conf.
func (s *Server) writeSiteCustomNginx(ctx context.Context, site *store.Site, custom string) error {
	u, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	l := s.layoutFor(site, u)
	content := strings.TrimRight(custom, "\n") + "\n"
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:  []agent.FileSpec{{Path: path.Join(l.nginxDir, customConfName), Content: content, Mode: 0o644}},
		Origin: "migrate:" + site.Domain,
	})
	return err
}
