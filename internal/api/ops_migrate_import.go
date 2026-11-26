package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
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

// maxBundleBytes caps the state a MonoPanel source sends: certificates, keys
// and settings of one account, far below it even with custom nginx files. A
// variable, so a test need not send megabytes.
var maxBundleBytes int64 = 64 << 20

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
	return nil, huma.Error422UnprocessableEntity("unknown source panel " + req.Panel + ": monopanel, fastpanel or bitrixvm")
}

func newMigrateHTTP(req apitypes.MigrationSourceRequest) (*migrateHTTP, error) {
	if strings.TrimSpace(req.Token) == "" {
		return nil, huma.Error422UnprocessableEntity("a token from the source is required: mp migrate grant on the old panel")
	}
	if !strings.HasPrefix(req.Scope, "user:") {
		return nil, huma.Error422UnprocessableEntity("the scope must be user:<login>")
	}
	raw := strings.TrimSpace(req.Source)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, huma.Error422UnprocessableEntity("the source address must look like https://panel.example.com:8443")
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
		return nil, fmt.Errorf("the source is unreachable: %w", err)
	}
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		msg := strings.TrimSpace(string(body))
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("the source rejected the token (%s): issue a new one with mp migrate grant", res.Status)
		}
		return nil, fmt.Errorf("the source answered %s: %s", res.Status, msg)
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
	// The source decides how long its answer is: a limit keeps it from
	// taking this panel's memory along.
	body := &io.LimitedReader{R: res.Body, N: maxBundleBytes + 1}
	if err := json.NewDecoder(body).Decode(&b); err != nil {
		if body.N <= 0 {
			return nil, fmt.Errorf("the source's answer is larger than %d MB: not an account's state", maxBundleBytes>>20)
		}
		return nil, fmt.Errorf("cannot parse the source's answer: %w", err)
	}
	if b.User == nil {
		return nil, errors.New("the source did not return the account")
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
	login := req.As
	if login == "" && b.User != nil {
		login = b.User.Login
	}
	// Every item first, against the rules the API applies to the same values
	// typed by a person: what breaks one is a conflict below.
	v := s.vetBundle(ctx, req, b, login)
	plan := &apitypes.MigrationPlan{Bundle: b, Login: shown(login), Conflicts: []apitypes.MigrationIssue{}, Warnings: []apitypes.MigrationIssue{}}
	block := func(kind, target, text, fix string) {
		plan.Conflicts = append(plan.Conflicts, apitypes.MigrationIssue{Kind: kind, Target: target, Text: text, Fix: fix})
	}
	warn := func(kind, target, text, fix string) {
		plan.Warnings = append(plan.Warnings, apitypes.MigrationIssue{Kind: kind, Target: target, Text: text, Fix: fix})
	}
	sites := make([]*store.Site, 0, len(b.Sites))
	for _, site := range b.Sites {
		if v.why(site) == "" {
			sites = append(sites, site)
		}
	}

	// Another OS family is fine: every configuration is rendered here from
	// the state, with this server's PHP layout and service names. Only what
	// the user wrote by hand (nginx directives, cron commands) may still
	// point at the old paths.
	if b.Family != "" && b.Family != string(s.profile.Family()) {
		warn("user", plan.Login, "the source is on "+b.Family+", this server on "+string(s.profile.Family())+": the configuration will be regenerated for this server", "check your own nginx directives and cron commands: they may contain paths of the old OS")
	}
	if _, err := s.db.GetUserByLogin(ctx, login); err == nil {
		block("user", plan.Login, "an account with this login already exists", "take it under another login: --as <login>")
	}
	plan.Conflicts = append(plan.Conflicts, v.conflicts...)
	plan.Warnings = append(plan.Warnings, v.warnings...)
	// Sites need the web server: without nginx the account would arrive and
	// every site.apply would fail, leaving the sites down until someone
	// installed it by hand.
	if len(sites) > 0 {
		if q, err := s.agent.Pkg(ctx, "query", "nginx"); err == nil && q.Installed["nginx"] == "" {
			block("site", "", "sites are coming but nginx is not installed here", "mp stack install nginx")
		}
		apache := false
		for _, site := range sites {
			apache = apache || site.Mode == store.ModeApache
		}
		if v, _ := s.db.GetSetting(ctx, settingApache); apache && v != "installed" {
			block("site", "", "sites in the nginx + Apache mode are coming but Apache is not installed here", "mp stack install apache")
		}
	}
	for _, site := range sites {
		if site.PHPVersion != "" {
			if err := s.checkPHPInstalled(ctx, site.PHPVersion); err != nil {
				block("php", site.PHPVersion, "PHP "+site.PHPVersion+" is not installed, so site "+site.Domain+" has nothing to run on", "mp php install "+site.PHPVersion)
			}
		}
	}
	if len(b.Databases) > 0 {
		if _, err := s.dbInstance(ctx); err != nil {
			block("database", "", "databases are coming but no database server is installed here", "mp stack install percona")
		}
		if v := legacyPHP(sites); v != "" {
			warn("database", "", "PHP "+v+": database accounts will use mysql_native_password, as this branch cannot handle caching_sha2_password", "")
		}
	}
	if len(b.MailDomains) > 0 {
		if c := s.loadMailConfig(ctx); !c.Installed {
			block("mail", "", "mail domains are coming but no mail server is installed here", "mp mail install")
		}
	}
	for _, cert := range b.Certificates {
		if !cert.Files && cert.Cert == "" {
			warn("cert", shown(cert.Name), "the certificate will arrive without its files: the site will work over HTTP until a new one is issued", "after the DNS switch: mp ssl issue "+shown(cert.Name))
		}
	}
	if len(b.Apps) > 0 {
		warn("user", plan.Login, fmt.Sprintf("app services: %d; they will arrive switched off", len(b.Apps)), "check their environment and ports, then mp app start")
	}
	if len(b.Valkey) > 0 && s.valkeyLayout(ctx) == nil {
		warn("user", plan.Login, fmt.Sprintf("Valkey instances: %d, but Valkey is not installed here, so they will not be created", len(b.Valkey)), "mp stack install valkey before the migration")
	}

	// Место: файлы и почта распакуются целиком, дампы ещё и развернутся.
	// Отрицательный размер от источника — неправда, а не скидка на место.
	need := max(b.Sizes.FilesBytes, 0) + max(b.Sizes.MailBytes, 0)
	for _, n := range b.Sizes.Databases {
		need += max(n, 0) * 2
	}
	if info, err := s.agent.SystemInfo(ctx); err == nil && need > 0 {
		for _, d := range info.Disks {
			if d.Mount != "/" {
				continue
			}
			if int64(d.FreeBytes) < need {
				block("disk", d.Mount, fmt.Sprintf("about %d MB needed, %d MB free", need>>20, d.FreeBytes>>20), "free up space or migrate in parts")
			} else if int64(d.FreeBytes) < need*2 {
				warn("disk", d.Mount, fmt.Sprintf("less than half of the free space will be left after the migration (%d MB needed, %d MB free)", need>>20, d.FreeBytes>>20), "")
			}
		}
	}
	plan.OK = len(plan.Conflicts) == 0
	displayBundle(b)
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
		return p, huma.Error422UnprocessableEntity("the panel's secret store is not open: there is nowhere to keep the ssh password")
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
	s.registerVetSchemas()
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
			first := plan.Conflicts[0]
			if first.Target != "" {
				return nil, huma.Error409Conflict("migration not started: " + first.Target + ": " + first.Text)
			}
			return nil, huma.Error409Conflict("migration not started: " + first.Text)
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
	jc.Progress(2, "source state")
	b, err := src.bundle(ctx, p.Scope, true)
	if err != nil {
		return err
	}
	if b.Secrets == nil {
		return errors.New("the source returned no secrets: the token is view-only")
	}
	if p.Scope == "" {
		p.Scope = b.Scope
	}
	// The plan checked a bundle without secrets, and the source may have
	// changed since: the one the import works from is checked again, and
	// what it refuses stays behind with a line here.
	v := s.vetBundle(ctx, p.MigrationSourceRequest, b, p.Login)
	if b.User == nil {
		return errors.New("the source sent no account")
	}
	jc.Logf("source: %s (%s), account %s → %s", b.Hostname, b.Panel, shown(b.User.Login), p.Login)
	// A note may span lines; each gets the mark, so a note cannot pass for
	// a line of the import itself ("refused: …", "database … moved").
	for _, n := range b.Notes {
		for _, line := range strings.Split(n, "\n") {
			jc.Logf("· %s", line)
		}
	}
	for _, c := range v.conflicts {
		jc.Logf("refused: %s", issueLine(c))
	}
	// Warnings say what stays behind on purpose (an account for a remote
	// host, a certificate of another account's name) or what to check.
	for _, w := range v.warnings {
		jc.Logf("warning: %s", issueLine(w))
	}
	if v.account {
		return errors.New("the account itself is refused, nothing was imported: see the refused lines above")
	}
	failed := 0

	jc.Progress(5, "account")
	hash := b.Secrets.PanelPassword
	if v.isRefused(secretPanel) {
		hash = ""
	}
	u := &store.User{
		Login: p.Login, Role: store.RoleUser, Email: b.User.Email, Shell: b.User.Shell,
		QuotaMB: b.User.QuotaMB, PasswordHash: hash, Status: store.UserPending,
	}
	if err := s.db.CreateUser(ctx, u); err != nil {
		return fmt.Errorf("account %s: %w", p.Login, err)
	}
	if err := s.provisionUser(ctx, userProvisionPayload{UserID: u.ID, Login: u.Login, Shell: u.Shell}, jc.Logf, func(int, string) {}); err != nil {
		return err
	}
	if v.isRefused(secretPanel) {
		jc.Logf("the panel password did not move: mp user set %s --generate", u.Login)
	}
	switch {
	case b.Secrets.UnixShadow == "":
	case v.isRefused(secretUnix):
		jc.Logf("the SFTP password did not move: mp user set %s --generate", u.Login)
	default:
		if err := s.agent.SetUnixPasswordHash(ctx, u.Login, b.Secrets.UnixShadow); err != nil {
			jc.Logf("warning: the SFTP password was not moved: %v", err)
		} else {
			jc.Logf("panel and SFTP passwords moved as hashes: the user signs in with the same ones")
		}
	}
	u, err = s.db.GetUserByLogin(ctx, p.Login)
	if err != nil {
		return err
	}

	jc.Progress(15, "files")
	if err := s.migrateFiles(ctx, jc, src, p, u, "home", "", ""); err != nil {
		return err
	}

	jc.Progress(45, "databases")
	var sites []*store.Site
	for _, site := range b.Sites {
		if v.why(site) == "" {
			sites = append(sites, site)
		}
	}
	legacy := legacyPHP(sites) != ""
	created := map[string]bool{}
	databases := 0
	for _, d := range b.Databases {
		if v.why(d) != "" {
			continue
		}
		if err := s.migrateDatabase(ctx, jc, src, p, u, d, v, legacy, created); err != nil {
			jc.Logf("database %s: %v", d.Name, err)
			failed++
			continue
		}
		databases++
	}

	jc.Progress(65, "sites")
	var moved []*store.Site
	for _, site := range sites {
		if err := s.migrateSite(ctx, jc, u, site, b.SiteNginx[site.Domain]); err != nil {
			jc.Logf("site %s: %v", site.Domain, err)
			failed++
			continue
		}
		moved = append(moved, site)
	}

	jc.Progress(80, "certificates")
	for i := range b.Certificates {
		cert := b.Certificates[i]
		if v.why(&b.Certificates[i]) != "" || cert.Cert == "" || cert.Key == "" {
			continue
		}
		if err := s.importCertificate(ctx, u, cert); err != nil {
			jc.Logf("certificate %s: %v", cert.Name, err)
			continue
		}
		jc.Logf("certificate %s moved (valid until %s)", cert.Name, cert.NotAfter.Format("2006-01-02"))
	}
	if err := src.settle(ctx, jc, u); err != nil {
		jc.Logf("warning: %v", err)
	}
	for _, site := range moved {
		job, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: siteID(ctx, s, site.Domain)}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(jc.RequestedBy))
		if err != nil {
			jc.Logf("site %s: %v", site.Domain, err)
			continue
		}
		jc.Logf("site %s: job #%d applies the configuration", site.Domain, job.ID)
	}

	jc.Progress(88, "cron and app services")
	cron := 0
	for _, j := range b.Cron {
		if v.why(j) != "" {
			continue
		}
		if err := s.db.CreateCronJob(ctx, &store.CronJob{UserID: u.ID, Schedule: j.Schedule, Command: j.Command, Comment: j.Comment, Enabled: j.Enabled}); err != nil {
			jc.Logf("cron: %v", err)
			failed++
			continue
		}
		cron++
	}
	if cron > 0 {
		if err := s.applyCrontab(ctx, u); err != nil {
			jc.Logf("crontab: %v", err)
		} else {
			jc.Logf("cron jobs moved: %d", cron)
		}
	}
	apps := 0
	for _, a := range b.Apps {
		if v.why(a) != "" {
			continue
		}
		app := *a
		app.ID, app.UserID, app.Login, app.Enabled, app.Status, app.LastError = 0, u.ID, u.Login, false, "stopped", ""
		if other, err := s.unitTaken(ctx, &app); err != nil || other != "" {
			jc.Logf("refused: app %s: its unit %s belongs to the app %s here already", a.Name, app.Unit(), other)
			failed++
			continue
		}
		if err := s.db.CreateApp(ctx, &app); err != nil {
			jc.Logf("app %s: %v", a.Name, err)
			failed++
			continue
		}
		apps++
	}
	if apps > 0 {
		jc.Logf("app services moved: %d, switched off; check their environment and start them", apps)
	}
	if len(b.Valkey) > 0 {
		if l := s.valkeyLayout(ctx); l == nil {
			jc.Logf("Valkey instances not created: Valkey is not installed here (mp stack install valkey, then mp valkey add)")
		} else {
			made := 0
			for _, src := range b.Valkey {
				if v.why(src) != "" {
					continue
				}
				inst := &store.ValkeyInstance{UserID: u.ID, Login: u.Login, Purpose: src.Purpose, MemoryMB: src.MemoryMB}
				if err := s.db.CreateValkey(ctx, inst); err != nil {
					jc.Logf("valkey %s: %v", src.Purpose, err)
					failed++
					continue
				}
				made++
				if _, err := s.applyValkey(ctx, inst, l); err != nil {
					jc.Logf("valkey %s: %v", inst.Name(), err)
				}
			}
			jc.Logf("Valkey instances created: %d; the cache and sessions start from scratch", made)
		}
	}

	jc.Progress(92, "mail")
	mailFailed, err := s.migrateMail(ctx, jc, src, p, u, b, v)
	if err != nil {
		jc.Logf("mail: %v", err)
	}
	failed += mailFailed

	// The account came in pending and its provision above made it active,
	// as for an account made here: a move that breaks off halfway leaves an
	// account its administrator can take on from. This activates only one
	// still pending (ActivateUser) and so changes nothing of what was asked
	// for during the move: a suspension stays, and so does a removal, which
	// waits under the account's key and must find the account being removed.
	switch status, err := s.db.ActivateUser(ctx, u.ID); {
	case errors.Is(err, store.ErrBeingRemoved):
		jc.Logf("the account %s is being removed: it stays so, and its removal takes what moved", u.Login)
	case err != nil:
		return err
	case status == store.UserSuspended:
		jc.Logf("the account %s was suspended during the move: it stays suspended", u.Login)
	}
	mailboxes := 0
	for _, box := range b.Mailboxes {
		if v.why(box) == "" {
			mailboxes++
		}
	}
	jc.Progress(100, fmt.Sprintf("account %s moved: %d sites, %d databases, %d mailboxes", p.Login, len(moved), databases, mailboxes))
	jc.Logf("DNS still points at the source. Check the sites at this server's IP, then switch the records.")
	if n := len(v.conflicts) + failed; n > 0 {
		return fmt.Errorf("account %s moved incompletely: %d problem(s) — values of the source this panel refuses, or items that failed to import; see the log", p.Login, n)
	}
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
		// The domain was vetted (mailDomainRe): the path stays a directory of
		// the mail store.
		dest, owner, group = mailBase+"/"+domain, vmailUser, vmailUser
		if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: dest, Mode: 0o750, Owner: owner, Group: group}}}); err != nil {
			return err
		}
	}
	args := []string{"--extract", "--file", "-", "--directory", dest, "--no-same-owner", "--warning=no-timestamp"}
	// tar -x runs as root: only files, directories and links reach it, and
	// in the mail store, where every Maildir is vmail's, no link leads out of
	// its directory (see migrate_tar.go). An absolute link target the source
	// rewrote to this home stays as a relative one.
	clean, st, done := migrateTar(body, part == "mail", dest)
	out, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "tar", Args: args}, clean)
	clean.Close()
	body.Close()
	<-done
	if err == nil && out.ExitCode != 0 {
		err = fmt.Errorf("tar: %s", toolOutput(out.Output))
	}
	if err != nil || st.err != nil && !errors.Is(st.err, io.ErrClosedPipe) {
		// What tar unpacked before the stream broke off is root's
		// (--no-same-owner): hand it over as a finished unpack is, or the
		// account cannot remove or replace it.
		s.handOverTree(ctx, jc, dest, owner, group)
	}
	// The filter's own reason comes first: the agent only sees the stream
	// break off. A closed pipe is the filter outliving tar, not a cause.
	if st.err != nil && !errors.Is(st.err, io.ErrClosedPipe) {
		return st.err
	}
	if err != nil {
		return err
	}
	if st.dropped > 0 {
		jc.Logf("%s: %d members of the source's archive left out (only files, directories and links inside the tree are taken), the first: %s", part, st.dropped, st.first)
	}
	// Распаковка идёт от root, поэтому владельца возвращаем отдельно.
	ch, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: dest, Owner: owner, Group: group, Recursive: true})
	if err != nil {
		return err
	}
	jc.Logf("%s: files unpacked into %s, ownership fixed on %d objects", part, dest, ch.Changed)
	s.relabel(ctx, jc, dest, true)
	return nil
}

// migrateDatabase recreates a database with its accounts and loads the dump
// into it as one of those accounts. Every statement is built here from vetted
// parts: the CREATE USER of the source is taken apart (parseCreateUser), never
// replayed as root.
func (s *Server) migrateDatabase(ctx context.Context, jc *jobs.Context, src migrateSource, p migratePayload, u *store.User, d *store.Database, v *migrateVet, legacy bool, created map[string]bool) error {
	if d.Charset == "" {
		d.Charset, d.Collation = "utf8mb4", "utf8mb4_0900_ai_ci"
	}
	if d.Collation == "" {
		d.Collation = d.Charset + "_general_ci"
	}
	// No IF NOT EXISTS: a database that appeared since the plan is not
	// written into.
	if _, err := s.mysqlExec(ctx, fmt.Sprintf("CREATE DATABASE `%s` CHARACTER SET %s COLLATE %s;", d.Name, d.Charset, d.Collation)); err != nil {
		return err
	}
	row := &store.Database{UserID: u.ID, Name: d.Name, Charset: d.Charset, Collation: d.Collation}
	if err := s.db.CreateDatabase(ctx, row); errors.Is(err, store.ErrExists) {
		// The plan found no such database: a record of that name now is
		// another account's, and nothing of the source is granted on it or
		// loaded into it.
		return errors.New("the panel has a database of this name now (added while the files were copied): nothing of the source is loaded into it")
	} else if err != nil {
		return err
	}
	var accounts []*store.DBUser
	native := false
	for _, acc := range d.Users {
		a, ok := v.auth[acc.Name+"@"+acc.Host]
		if v.why(acc) != "" || !ok {
			continue
		}
		accounts = append(accounts, acc)
		native = native || a.effectivePlugin(legacy) == "mysql_native_password"
	}
	if native {
		if inst, err := s.dbInstance(ctx); err == nil {
			// The plan warned only if the source said so ahead of the hashes:
			// the restart every account on this server feels gets its line.
			if !inst.NativePassword {
				jc.Logf("database %s: switching mysql_native_password on for its accounts, MySQL restarts once", d.Name)
			}
			if err := s.ensureNativePassword(ctx, inst); err != nil {
				jc.Logf("mysql_native_password for %s: %v", d.Name, err)
			}
		}
	}
	// The dump is loaded as an account of this database, so it can do
	// nothing outside it. Until then that account authenticates with
	// auth_socket for the OS user root — the agent signs in as it over the
	// socket, and no password is made up, stored or put on a command line —
	// and gets its own authentication back right after.
	var loader *store.DBUser
	for _, acc := range accounts {
		if acc.Host == "localhost" {
			loader = acc
			break
		}
	}
	for _, acc := range accounts {
		key := acc.Name + "@" + acc.Host
		a := v.auth[key]
		var sql string
		switch {
		case acc == loader && created[key]:
			sql = fmt.Sprintf("ALTER USER '%s'@'%s' %s;\n", acc.Name, acc.Host, loaderAuth)
		case acc == loader:
			sql = fmt.Sprintf("CREATE USER '%s'@'%s' %s;\n", acc.Name, acc.Host, loaderAuth)
		case !created[key]:
			// No IF NOT EXISTS: an account that is not ours is never changed.
			sql = fmt.Sprintf("CREATE USER '%s'@'%s' %s;\n", acc.Name, acc.Host, a.clause(legacy))
		}
		grant := fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%s';\n", grantDB(d.Name), acc.Name, acc.Host)
		_, err := s.mysqlExec(ctx, sql+grant)
		if passwordPolicyRefused(err) && acc != loader && !created[key] {
			if hashed, ok, herr := a.hashedClause(legacy); herr != nil {
				err = herr
			} else if ok {
				if _, err = s.mysqlExec(ctx, fmt.Sprintf("CREATE USER '%s'@'%s' %s;\n", acc.Name, acc.Host, hashed)+grant); err == nil {
					s.logPolicyBypass(jc, acc.Name, acc.Host)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("account %s@%s: %w", acc.Name, acc.Host, err)
		}
		created[key] = true
		if plugin := a.effectivePlugin(legacy); legacy && plugin != "mysql_native_password" {
			jc.Logf("warning: account %s@%s arrives with a %s hash, which PHP below 7.4 cannot connect with; after the migration: ALTER USER '%s'@'%s' IDENTIFIED WITH mysql_native_password BY '<the password from the site settings>'", acc.Name, acc.Host, plugin, acc.Name, acc.Host)
		}
	}
	name := ""
	if loader != nil {
		name = loader.Name
	} else {
		// A database without an account of this host gets one of the panel's
		// making to load the dump as.
		name = importAccountPrefix + randomHex(4)
		if _, err := s.mysqlExec(ctx, fmt.Sprintf("CREATE USER '%s'@'localhost' %s;\nGRANT ALL PRIVILEGES ON %s.* TO '%s'@'localhost';\n", name, loaderAuth, grantDB(d.Name), name)); err != nil {
			return fmt.Errorf("an account to load the dump as: %w", err)
		}
	}
	filter, loadErr := s.loadDump(ctx, src, p, d.Name, name)
	if loader != nil {
		a := v.auth[loader.Name+"@localhost"]
		_, err := s.mysqlExec(ctx, fmt.Sprintf("ALTER USER '%s'@'localhost' %s;", loader.Name, a.clause(legacy)))
		if passwordPolicyRefused(err) {
			if hashed, ok, herr := a.hashedClause(legacy); herr != nil {
				err = herr
			} else if ok {
				if _, err = s.mysqlExec(ctx, fmt.Sprintf("ALTER USER '%s'@'localhost' %s;", loader.Name, hashed)); err == nil {
					s.logPolicyBypass(jc, loader.Name, "localhost")
				}
			}
		}
		if err != nil {
			return fmt.Errorf("account %s@localhost did not get its password back after the load: %w", loader.Name, err)
		}
	} else if err := s.keepImportAccount(ctx, jc, u, row, name, legacy); err != nil {
		return err
	}
	for _, acc := range accounts {
		s.db.CreateDBUser(ctx, &store.DBUser{UserID: u.ID, DatabaseID: &row.ID, Name: acc.Name, Host: acc.Host, AuthPlugin: v.auth[acc.Name+"@"+acc.Host].effectivePlugin(legacy)}) //nolint:errcheck // строка учётки — украшение списка баз
	}
	if loadErr != nil {
		return loadErr
	}
	if filter.definers > 0 || filter.dropped > 0 {
		jc.Logf("database %s: DEFINER of %d views, routines or triggers set to the database's account, %d binlog/GTID lines of the source left out", d.Name, filter.definers, filter.dropped)
	}
	jc.Logf("database %s moved together with its accounts (passwords unchanged)", d.Name)
	return nil
}

// loaderAuth is the authentication the account a dump is loaded as has while
// it loads: auth_socket for the OS user root, which the agent runs mysql as.
const loaderAuth = "IDENTIFIED WITH auth_socket AS 'root'"

// keepImportAccount makes the account a database without one of its own was
// loaded as its regular account. The views, routines and triggers of the
// dump name it as their definer, and without an account the database could
// not be used or managed here. It gets a password nobody knows, with the
// plugin a plain password of the source gets (PHP below 7.4 needs
// mysql_native_password), and a record: mp db passwd sets its password, and
// mp db rm drops it along with the database — which an auth_socket record
// would break, as passwd renders IDENTIFIED WITH <plugin> BY for each one.
func (s *Server) keepImportAccount(ctx context.Context, jc *jobs.Context, u *store.User, row *store.Database, name string, legacy bool) error {
	pw, err := auth.NewPassword(24)
	if err != nil {
		return err
	}
	a := dbAuth{plain: pw}
	if a.effectivePlugin(legacy) == "mysql_native_password" {
		if inst, err := s.dbInstance(ctx); err == nil {
			if !inst.NativePassword {
				jc.Logf("database %s: switching mysql_native_password on for its account, MySQL restarts once", row.Name)
			}
			if err := s.ensureNativePassword(ctx, inst); err != nil {
				jc.Logf("mysql_native_password for %s: %v", row.Name, err)
			}
		}
	}
	if _, err := s.mysqlExec(ctx, fmt.Sprintf("ALTER USER '%s'@'localhost' %s;", name, a.clause(legacy))); err != nil {
		return fmt.Errorf("account %s@localhost that loaded the dump: %w", name, err)
	}
	s.db.CreateDBUser(ctx, &store.DBUser{UserID: u.ID, DatabaseID: &row.ID, Name: name, Host: "localhost", AuthPlugin: a.effectivePlugin(legacy)}) //nolint:errcheck // строка учётки — украшение списка баз
	jc.Logf("database %s came without an account of its own here: the account %s@localhost it was loaded as stays its account, with a password nobody knows; set one: mp db passwd %s", row.Name, name, row.Name)
	return nil
}

// loadDump streams the source's dump into a database as one of its accounts
// (see migrate_dump.go for what keeps it there).
func (s *Server) loadDump(ctx context.Context, src migrateSource, p migratePayload, db, account string) (*dumpFilter, error) {
	body, err := src.dump(ctx, p.Scope, db)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	f := newDumpFilter(body)
	// --local-infile=0 wins over any option file: LOAD DATA LOCAL INFILE in
	// the dump would have the client, which runs as root, read a file of the
	// dump's choosing (/etc/shadow, the panel's keys) into the database.
	out, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "mysql", Args: []string{"--protocol=socket", "--binary-mode", "--local-infile=0", "--user=" + account, db}}, f)
	if err != nil {
		return f, err
	}
	if out.ExitCode != 0 {
		// mysql quotes the statement it choked on: text of the dump, which is
		// the source's, on its way to the job log and a terminal.
		return f, fmt.Errorf("mysql: %s", toolOutput(out.Output))
	}
	return f, nil
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// legacyPHP returns the oldest PHP branch below 7.4 among the sites, or "".
func legacyPHP(sites []*store.Site) string {
	oldest := ""
	for _, site := range sites {
		if site.PHPVersion != "" && versionLess(site.PHPVersion, "7.4") && (oldest == "" || versionLess(site.PHPVersion, oldest)) {
			oldest = site.PHPVersion
		}
	}
	return oldest
}

// migrateSite recreates a site row (vetted by vetSite). The custom nginx
// directives of a MonoPanel source are saved for review, never applied.
func (s *Server) migrateSite(ctx context.Context, jc *jobs.Context, u *store.User, src *store.Site, custom string) error {
	site := *src
	// The sites-create rule for the names once more, right before the row:
	// another site may have taken one of them while the files were copied.
	names, err := s.validateNames(ctx, site.Domain, site.Aliases, 0, u.ID)
	if err != nil {
		return err
	}
	site.Aliases = names
	site.ID, site.UserID, site.Login, site.CertificateID, site.CertPinned = 0, u.ID, u.Login, nil, false
	site.Status, site.LastError = store.SitePending, ""
	// Sessions stay in Valkey only where PHP can reach it: the instance
	// itself comes later in this job, from the bundle.
	if site.SessionStore == store.SessionStoreValkey {
		if s.valkeyLayout(ctx) == nil {
			site.SessionStore = ""
			jc.Logf("%s: PHP sessions switch to files: there is no Valkey here (mp stack install valkey, mp valkey add sessions, mp site set --sessions valkey)", site.Domain)
		} else if on, _ := s.redisEnabled(ctx, site.PHPVersion); !on {
			site.SessionStore = ""
			jc.Logf("%s: PHP sessions switch to files: PHP %s has the redis extension disabled here (mp php ext enable %s redis, then mp site set %s --sessions valkey)", site.Domain, site.PHPVersion, site.PHPVersion, site.Domain)
		}
	}
	if site.Preset == presetBitrix && site.FPMMaxChildren <= 0 {
		site.FPMMaxChildren = s.bitrixPoolSize(ctx)
	}
	if err := s.db.CreateSite(ctx, &site); err != nil {
		return err
	}
	if strings.TrimSpace(custom) != "" && len(custom) <= maxMigratedNginx {
		if file, err := s.saveMigratedNginx(ctx, &site, u, custom); err != nil {
			jc.Logf("site %s: the custom nginx directives of the source were not saved for review: %v", site.Domain, err)
		} else {
			jc.Logf("site %s: the custom nginx directives of the source are not applied; saved for review as %s (mp site nginx %s --set %s)", site.Domain, file, site.Domain, file)
		}
	}
	jc.Logf("site %s (PHP %s, mode %s)", site.Domain, site.PHPVersion, site.Mode)
	return nil
}

// migrateMail moves domains, mailboxes with their password hashes, aliases and
// the Maildirs themselves; it returns how many items failed to import.
func (s *Server) migrateMail(ctx context.Context, jc *jobs.Context, src migrateSource, p migratePayload, u *store.User, b *apitypes.MigrationBundle, v *migrateVet) (int, error) {
	if len(b.MailDomains) == 0 {
		return 0, nil
	}
	if c := s.loadMailConfig(ctx); !c.Installed {
		return 0, errors.New("no mail server is installed here; the domains were skipped")
	}
	failed := 0
	byID := map[int64]*store.MailDomain{}
	for _, d := range b.MailDomains {
		if v.why(d) != "" {
			continue
		}
		row := &store.MailDomain{UserID: u.ID, Name: d.Name, Active: d.Active, Lenient: d.Lenient, SendOnly: d.SendOnly}
		// Selector and public key only together with the private key they
		// belong to: without it the domain does not sign, and the DNS record
		// the panel shows must not say otherwise.
		if key := b.Secrets.DKIM[d.Name]; key != "" && d.DKIMSelector != "" && !v.isRefused(secretDKIM(d.Name)) && s.secrets != nil {
			enc, err := s.secrets.Encrypt(key)
			if err != nil {
				return failed, err
			}
			row.DKIMSelector, row.DKIMPublic, row.DKIMKeyEnc = d.DKIMSelector, d.DKIMPublic, enc
		} else if d.DKIMSelector != "" {
			jc.Logf("mail %s arrives without its DKIM key, so its mail is not signed: mp mail domain dkim %s", d.Name, d.Name)
		}
		if err := s.db.CreateMailDomain(ctx, row); err != nil {
			jc.Logf("mail domain %s: %v", d.Name, err)
			failed++
			continue
		}
		byID[d.ID] = row
		if d.SendOnly {
			// Another server receives its mail: there is no Maildir to copy.
			jc.Logf("mail %s is send-only: this server signs and sends its mail, another one receives it", d.Name)
			continue
		}
		if err := s.migrateFiles(ctx, jc, src, p, u, "mail", d.Name, ""); err != nil {
			jc.Logf("mail %s: messages not moved: %v", d.Name, err)
		}
	}
	boxes, aliases := 0, 0
	for _, box := range b.Mailboxes {
		row := byID[box.DomainID]
		if row == nil || v.why(box) != "" {
			continue
		}
		hash := b.Secrets.Mailboxes[box.Address]
		if hash == "" || v.isRefused(secretMailbox(box.Address)) {
			// An empty password field never reaches dovecot's passwd-file:
			// the mailbox gets a password nobody knows until its owner sets
			// one.
			pw, err := auth.NewPassword(24)
			if err == nil {
				hash, err = hashMailPassword(pw)
			}
			if err != nil {
				return failed, err
			}
			jc.Logf("mailbox %s arrives without its password: set one with mp mail box set %s --password-stdin", box.Address, box.Address)
		}
		if err := s.db.CreateMailbox(ctx, &store.Mailbox{
			DomainID: row.ID, LocalPart: box.LocalPart, Address: box.Address, Name: box.Name,
			PasswordHash: hash, QuotaMB: box.QuotaMB, Active: box.Active,
		}); err != nil {
			jc.Logf("mailbox %s: %v", box.Address, err)
			failed++
			continue
		}
		boxes++
	}
	for _, a := range b.MailAliases {
		row := byID[a.DomainID]
		if row == nil || v.why(a) != "" {
			continue
		}
		if err := s.db.CreateMailAlias(ctx, &store.MailAlias{DomainID: row.ID, Source: a.Source, Destination: a.Destination, Active: a.Active}); err != nil {
			jc.Logf("alias %s: %v", a.Address, err)
			failed++
			continue
		}
		aliases++
	}
	if err := s.applyMail(ctx, jc.Logf); err != nil {
		return failed, err
	}
	jc.Logf("mail moved: %d domains, %d mailboxes, %d aliases (passwords unchanged)", len(byID), boxes, aliases)
	return failed, nil
}

// importCertificate installs a certificate that came with the account (vetted
// by vetCerts), so the site keeps answering by HTTPS from the first second
// after the DNS switch — ACME can only issue its own once the name points here.
func (s *Server) importCertificate(ctx context.Context, u *store.User, mc apitypes.MigrationCert) error {
	info, err := checkCertPair(mc.Cert, mc.Key)
	if err != nil {
		return err
	}
	// Another account's certificate, or the panel's own, is never replaced:
	// one with this name may have appeared since the job started, and the
	// panel's own files sit under its hostname without a record.
	if s.panelNames(ctx)[mc.Name] {
		return errors.New("a hostname of this panel itself: the certificate is not installed")
	}
	if _, err := s.db.GetCertificateByName(ctx, mc.Name); err == nil {
		return errors.New("this panel already has a certificate with this name; the one from the source is not installed")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// A site takes its certificate by name (certForSite): the site of this
	// name must be the account's now. The plan saw it arrive, but while the
	// files were copied another account may have added a site of that name,
	// and it would be served the source's certificate and key.
	own := func(name string) bool {
		sites, err := s.db.FindSitesByName(ctx, name)
		return err == nil && len(sites) > 0 && sites[0].UserID == u.ID
	}
	if !own(mc.Name) {
		return errors.New("no site of the account has this name here (it did not arrive, or another account has it now): the certificate is not installed")
	}
	names := info.Names
	if mc.Kind == store.CertKindACME {
		names = acmeNames(info.Names, own)
	}
	certPEM := []byte(strings.TrimSpace(mc.Cert) + "\n")
	keyPEM := []byte(strings.TrimSpace(mc.Key) + "\n")
	leaf, chain := splitChain(certPEM)
	fullchain := append(append([]byte{}, leaf...), chain...)
	certPath, keyPath, chainPath, err := s.acme.WriteFiles(mc.Name, fullchain, keyPEM, chain)
	if err != nil {
		return err
	}
	uid := u.ID
	now := time.Now()
	nb, na := info.NotBefore, info.NotAfter
	c := &store.Certificate{Name: mc.Name, UserID: &uid, Names: names, Kind: mc.Kind, AutoRenew: mc.AutoRenew, KeyType: certKeyType(mc.Cert, mc.Key)}
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

// certKeyType names the key the way certificates-import does.
func certKeyType(certPEM, keyPEM string) string {
	pair, err := tls.X509KeyPair([]byte(strings.TrimSpace(certPEM)+"\n"), []byte(strings.TrimSpace(keyPEM)+"\n"))
	if err == nil {
		if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); ok {
			return "ec256"
		}
	}
	return "rsa2048"
}

// saveMigratedNginx puts the directives an administrator wrote by hand on the
// old server next to the site's custom.conf, under a name the server block
// does not include: they wait there for a person (vetCustomNginx).
func (s *Server) saveMigratedNginx(ctx context.Context, site *store.Site, u *store.User, custom string) (string, error) {
	l := s.layoutFor(site, u)
	file := path.Join(l.nginxDir, migratedNginxName)
	content := "# Custom nginx directives of " + site.Domain + " on the old server, NOT applied.\n" +
		"# Check them, then: mp site nginx " + site.Domain + " --set " + file + "\n" +
		strings.TrimRight(custom, "\n") + "\n"
	_, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:  []agent.FileSpec{{Path: file, Content: content, Mode: 0o644}},
		Origin: "migrate:" + site.Domain,
	})
	return file, err
}

// logPolicyBypass tells that an account's password, which the server's
// validate_password policy refuses in plain text, came as its hash.
func (s *Server) logPolicyBypass(jc *jobs.Context, name, host string) {
	jc.Logf("account %s@%s: its password does not pass this server's validate_password policy, so it is set from its hash — unchanged, the site connects as before; a stronger one is advisable (mp db passwd, then the site's settings)", name, host)
}
