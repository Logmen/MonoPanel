package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	_ "modernc.org/sqlite" // FASTPANEL's own database is SQLite; read with the same driver the panel uses

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// FASTPANEL 2 держит всё состояние в SQLite (/usr/local/fastpanel2/app/db/
// fastpanel2.db): аккаунты (panel_account), сайты (site — index_dir там
// абсолютный docroot, virtualhost_aliases, website_backends — какой PHP их
// обслуживает), базы (db, database_user, datbases_users) и сертификаты
// (certificate: загруженные лежат текстом в базе, Let's Encrypt — файлами в
// /var/www/httpd-cert/<имя>.crt|.key). Cron панель пишет в crontab
// пользователя. Раскладка на диске совпадает с нашей:
// /var/www/<login>/data/www/<домен>, так что файлы едут без переименований.
// Пароли баз панель хранит зашифрованными — вместо них берём хеши из SHOW
// CREATE USER, как и между двумя MonoPanel. Пароль самой панели у FASTPANEL —
// unix-пароль (PAM), он приезжает хешем shadow и работает для SFTP; пароль
// веб-панели задают заново.

const (
	fastpanelDB    = "/usr/local/fastpanel2/app/db/fastpanel2.db"
	fastpanelCerts = "/var/www/httpd-cert"
)

type fastpanelSource struct {
	foreignSource
	db      *sql.DB
	dbFile  string
	account *fpAccount
	sites   []*fpSite
}

type fpAccount struct {
	id       int64
	login    string
	home     string
	shell    bool
	quotaMB  *int
	dataRoot string // /var/www/<login>
}

type fpSite struct {
	id        int64
	domain    string
	aliases   []string
	indexDir  string // docroot relative to the site dir ("" = the site dir)
	php       string
	mode      string
	backend   string
	https     bool
	http2     bool
	certID    int64
	status    string
	manual    bool // configs edited by hand in the panel
	allowFrom []string
	siteDir   string // /var/www/<login>/data/www/<domain> on the old server
	docroot   string // absolute, on the old server
}

func (s *Server) openFastpanel(ctx context.Context, req apitypes.MigrationSourceRequest) (migrateSource, error) {
	conn, err := dialSSH(ctx, req.Source, req.Password, req.Key)
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	if !conn.exists(ctx, fastpanelDB) {
		conn.close()
		return nil, huma.Error422UnprocessableEntity("на " + req.Source + " не видно FASTPANEL: нет " + fastpanelDB)
	}
	src := &fastpanelSource{foreignSource: foreignSource{s: s, conn: conn, req: req}}
	if err := src.fetchDB(ctx); err != nil {
		conn.close()
		return nil, huma.Error502BadGateway(err.Error())
	}
	if req.Scope == "" || scopeLogin(req.Scope) == "" {
		logins, _ := src.accounts(ctx)
		src.close()
		return nil, huma.Error422UnprocessableEntity("укажите, чей аккаунт переносим: --scope user:<логин>; на источнике есть: " + strings.Join(logins, ", "))
	}
	src.login = scopeLogin(req.Scope)
	if req.As != "" {
		src.login = req.As
	}
	src.home = path.Join(s.cfg.WWWRoot, src.login)
	return src, nil
}

// fetchDB takes a consistent copy of the panel database (python's backup
// API replays the WAL), so the rest reads locally.
func (f *fastpanelSource) fetchDB(ctx context.Context) error {
	script := `import sqlite3,sys,os,tempfile
src=sqlite3.connect("file:` + fastpanelDB + `?mode=ro",uri=True)
fd,p=tempfile.mkstemp()
os.close(fd)
dst=sqlite3.connect(p)
src.backup(dst)
dst.close()
sys.stdout.buffer.write(open(p,"rb").read())
os.unlink(p)`
	out, _, err := f.conn.exec(ctx, "python3 -c "+shq(script))
	if err != nil {
		// No python: take the raw file; the WAL may hold the newest rows,
		// so copy it alongside where it exists.
		raw, rerr := f.conn.readFile(ctx, fastpanelDB)
		if rerr != nil {
			return fmt.Errorf("база FASTPANEL не читается: %w", err)
		}
		out = raw
	}
	tmp, err := os.CreateTemp("", "fastpanel-*.db")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	f.dbFile = tmp.Name()
	db, err := sql.Open("sqlite", "file:"+f.dbFile+"?mode=ro")
	if err != nil {
		os.Remove(f.dbFile)
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		os.Remove(f.dbFile)
		return fmt.Errorf("база FASTPANEL: %w", err)
	}
	f.db = db
	return nil
}

func (f *fastpanelSource) close() {
	if f.db != nil {
		f.db.Close()
	}
	if f.dbFile != "" {
		os.Remove(f.dbFile)
	}
	f.foreignSource.close()
}

func (f *fastpanelSource) accounts(ctx context.Context) ([]string, error) {
	return f.strings(ctx, `SELECT username FROM panel_account ORDER BY username`)
}

// strings runs a one-column query against the panel database.
func (f *fastpanelSource) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := f.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// idNames runs an (id, name) query against the panel database.
func (f *fastpanelSource) idNames(ctx context.Context, query string, args ...any) (ids []int64, names []string, err error) {
	rows, err := f.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		ids, names = append(ids, id), append(names, name)
	}
	return ids, names, rows.Err()
}

// inventory reads the account and its sites from the panel database.
func (f *fastpanelSource) inventory(ctx context.Context) error {
	if f.account != nil {
		return nil
	}
	login := scopeLogin(f.req.Scope)
	acc := &fpAccount{login: login}
	var ssh int
	var quota sql.NullInt64
	err := f.db.QueryRowContext(ctx, `SELECT id, COALESCE(home_dir,''), COALESCE(ssh_access,0), COALESCE(quota_value,0) FROM panel_account WHERE username=?`, login).Scan(&acc.id, &acc.home, &ssh, &quota)
	if errors.Is(err, sql.ErrNoRows) {
		logins, _ := f.accounts(ctx)
		return fmt.Errorf("аккаунта %s в FASTPANEL нет; есть: %s", login, strings.Join(logins, ", "))
	}
	if err != nil {
		return err
	}
	acc.shell = ssh != 0
	if quota.Valid && quota.Int64 > 0 {
		mb := int(quota.Int64 >> 20)
		acc.quotaMB = &mb
	}
	acc.dataRoot = strings.TrimSuffix(acc.home, "/data")
	if acc.home == "" {
		acc.home = "/var/www/" + login + "/data"
		acc.dataRoot = "/var/www/" + login
	}
	f.account = acc
	cliPHP := f.phpVersion(ctx, "php")
	rows, err := f.db.QueryContext(ctx, `SELECT id, domain, COALESCE(index_dir,''), COALESCE(https_redirect,0), COALESCE(http2,1), COALESCE(certificate_id,0), COALESCE(status,''), COALESCE(manual_changes,0) FROM site WHERE owner_id=? ORDER BY lower(domain)`, acc.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		site := &fpSite{}
		var https, http2, manual int
		var indexDir string
		if err := rows.Scan(&site.id, &site.domain, &indexDir, &https, &http2, &site.certID, &site.status, &manual); err != nil {
			return err
		}
		site.https, site.http2, site.manual = https != 0, http2 != 0, manual != 0
		site.domain = strings.ToLower(site.domain)
		site.siteDir = path.Join(acc.home, "www", site.domain)
		// index_dir is the absolute docroot; ours is relative to the site dir.
		site.docroot = site.siteDir
		if indexDir = strings.TrimRight(strings.TrimSpace(indexDir), "/"); indexDir != "" {
			site.docroot = indexDir
			if rel := strings.TrimPrefix(indexDir, site.siteDir); rel != indexDir {
				site.indexDir = strings.Trim(rel, "/")
			} else if !strings.HasPrefix(indexDir, "/") {
				site.indexDir = strings.Trim(indexDir, "/")
				site.docroot = path.Join(site.siteDir, site.indexDir)
			}
		}
		f.sites = append(f.sites, site)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, site := range f.sites {
		if names, err := f.strings(ctx, `SELECT name FROM virtualhost_aliases WHERE site_id=? ORDER BY name`, site.id); err == nil {
			site.aliases = append(site.aliases, names...)
		}
		_, site.aliases = hostDomains(append([]string{site.domain}, site.aliases...))
		site.mode, site.php, site.backend = store.ModeFPM, "", ""
		var handler, version, kind, addr string
		var port int
		err := f.db.QueryRowContext(ctx, `SELECT COALESCE(handler,''), COALESCE(handler_version,''), COALESCE(type,''), COALESCE(addr,''), COALESCE(port,0) FROM website_backends WHERE frontend_id=? ORDER BY main DESC, id LIMIT 1`, site.id).Scan(&handler, &version, &kind, &addr, &port)
		if err == nil {
			// php_fpm is nginx → PHP-FPM like ours; fcgi and mod_php mean
			// Apache behind nginx.
			switch {
			case strings.Contains(handler, "apache") || strings.Contains(handler, "mod_php") || handler == "fcgi":
				site.mode = store.ModeApache
			case kind == "proxy" && addr != "" && port > 0:
				site.mode, site.backend = store.ModeProxy, fmt.Sprintf("http://%s:%d", addr, port)
			}
			site.php = normalizePHPBranch(version)
		}
		if site.php == "" && site.mode != store.ModeProxy {
			site.php = cliPHP
		}
		site.allowFrom = allowFromNginx(f.renderedNginx(ctx, site))
	}
	if login != f.login {
		f.rewrites = [][2]string{{acc.home + "/", f.home + "/data/"}, {acc.dataRoot + "/", f.home + "/"}}
		for _, site := range f.sites {
			f.configs = append(f.configs, cmsConfigs("data/www/"+site.domain)...)
		}
	}
	return nil
}

// normalizePHPBranch turns "8.2", "82", "php8.2-fpm" into "8.2".
func normalizePHPBranch(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimPrefix(v, "php")
	v = strings.TrimSuffix(v, "-fpm")
	if len(v) == 2 && !strings.Contains(v, ".") {
		v = v[:1] + "." + v[1:]
	}
	if len(v) == 3 && !strings.Contains(v, ".") {
		v = v[:1] + "." + v[1:]
	}
	if parts := strings.Split(v, "."); len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return ""
}

func (f *fastpanelSource) bundle(ctx context.Context, _ string, withSecrets bool) (*apitypes.MigrationBundle, error) {
	if err := f.inventory(ctx); err != nil {
		return nil, err
	}
	acc := f.account
	_, shell, hash, err := f.unixAccount(ctx, acc.login)
	if err != nil {
		return nil, err
	}
	hostname, _, _ := f.conn.exec(ctx, "hostname -f 2>/dev/null || hostname")
	version, _, _ := f.conn.exec(ctx, "dpkg-query -W -f='${Version}' fastpanel2 2>/dev/null || rpm -q --qf '%{VERSION}' fastpanel2 2>/dev/null")
	family := "debian"
	if f.conn.exists(ctx, "/etc/redhat-release") {
		family = "rhel"
	}
	out := &apitypes.MigrationBundle{
		Panel: strings.TrimSpace("FASTPANEL " + strings.TrimSpace(version)), Hostname: strings.TrimSpace(hostname), Family: family, Scope: "user:" + acc.login,
		Generated: time.Now().UTC(), SiteNginx: map[string]string{}, Sizes: apitypes.MigrationSizes{Databases: map[string]int64{}},
		Certificates: []apitypes.MigrationCert{}, Notes: []string{f.fingerprintNote()},
	}
	out.User = &store.User{Login: acc.login, Role: store.RoleUser, Shell: acc.shell && shell != "" && !strings.HasSuffix(shell, "nologin"), QuotaMB: acc.quotaMB, Home: acc.home}
	var plain map[string]string
	if withSecrets {
		plain = map[string]string{}
	}
	for _, site := range f.sites {
		row := newForeignSite(site.domain, site.aliases, site.php)
		row.Mode, row.Backend, row.Docroot, row.RedirectHTTPS, row.HTTP2 = site.mode, site.backend, site.indexDir, site.https, site.http2
		row.AllowFrom = site.allowFrom
		row.Preset, row.CMS = f.detectCMS(ctx, site.docroot)
		if row.Preset == presetBitrix {
			row.ClientMaxBody = "256m"
		}
		if site.status != "" && site.status != "active" && site.status != "enabled" {
			out.Notes = append(out.Notes, fmt.Sprintf("сайт %s в FASTPANEL со статусом %s", site.domain, site.status))
		}
		if site.manual {
			out.Notes = append(out.Notes, fmt.Sprintf("сайт %s: конфиги nginx правились вручную в FASTPANEL — сверьте их после переезда", site.domain))
		}
		if len(site.allowFrom) > 0 {
			out.Notes = append(out.Notes, fmt.Sprintf("сайт %s: список allow из nginx перенесён как allow_from (%s)", site.domain, strings.Join(site.allowFrom, ", ")))
		}
		out.Sites = append(out.Sites, row)
		out.Sizes.FilesBytes += f.duBytes(ctx, site.siteDir)
		if site.certID > 0 {
			if mc, ok := f.certificate(ctx, site.certID, site.domain, withSecrets); ok {
				out.Certificates = append(out.Certificates, mc)
			}
		}
	}
	dbs, err := f.databases(ctx, acc.id, plain)
	if err != nil {
		return nil, err
	}
	out.Databases = dbs
	for _, d := range dbs {
		out.Sizes.Databases[d.Name] = d.SizeBytes
	}
	out.Cron = f.crontab(ctx, acc.login)
	if n := f.mailboxCount(ctx, acc.id); n > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("почтовых ящиков у аккаунта: %d — почта FASTPANEL не переносится, заведите ящики здесь заново", n))
	}
	out.Notes = append(out.Notes,
		"переносится data/www целиком; логи, tmp и php-bin остаются",
		"пароль веб-панели у аккаунта не задан (FASTPANEL пускает по unix-паролю): mp user set "+f.login+" --generate")
	if withSecrets {
		out.Secrets = &apitypes.MigrationSecrets{UnixShadow: hash, DBUsers: plain, Mailboxes: map[string]string{}, DKIM: map[string]string{}}
	}
	return out, nil
}

// databases lists the account's databases with their users; withSecrets
// (plain != nil) also collects the CREATE USER statements.
func (f *fastpanelSource) databases(ctx context.Context, ownerID int64, plain map[string]string) ([]*store.Database, error) {
	ids, names, err := f.idNames(ctx, `SELECT id, name FROM db WHERE owner_id=? ORDER BY name`, ownerID)
	if err != nil {
		return nil, err
	}
	var out []*store.Database
	for i, name := range names {
		d, err := f.databaseRow(ctx, name)
		if err != nil {
			return nil, err
		}
		logins, err := f.strings(ctx, `SELECT u.login FROM database_user u JOIN datbases_users du ON du.user_id=u.id WHERE du.database_id=? ORDER BY u.login`, ids[i])
		if err == nil {
			for _, l := range logins {
				accs, err := f.dbAccounts(ctx, l)
				if err != nil {
					return nil, err
				}
				d.Users = append(d.Users, accs...)
				if plain != nil {
					for _, acc := range accs {
						plain[acc.Name+"@"+acc.Host] = f.createUser(ctx, acc, "")
					}
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// certificate reads one certificate row. An uploaded certificate ("exists")
// sits as PEM text in the database; a Let's Encrypt one only on disk, as
// /var/www/httpd-cert/<name>.crt and .key.
func (f *fastpanelSource) certificate(ctx context.Context, id int64, domain string, withSecrets bool) (apitypes.MigrationCert, bool) {
	var name, cert, chain, key, kind string
	var enabled int
	err := f.db.QueryRowContext(ctx, `SELECT COALESCE(name,''), COALESCE(certificate,''), COALESCE(chain,''), COALESCE(private_key,''), COALESCE(type,''), COALESCE(enabled,1) FROM certificate WHERE id=?`, id).Scan(&name, &cert, &chain, &key, &kind, &enabled)
	if err != nil || enabled == 0 {
		return apitypes.MigrationCert{}, false
	}
	if strings.TrimSpace(cert) == "" && name != "" {
		cert, _ = f.conn.readFile(ctx, path.Join(fastpanelCerts, name+".crt"))
		key, _ = f.conn.readFile(ctx, path.Join(fastpanelCerts, name+".key"))
		chain = ""
	}
	full := strings.TrimSpace(cert)
	if c := strings.TrimSpace(chain); c != "" && !strings.Contains(full, c) {
		full += "\n" + c
	}
	mc, ok := certificateFromPEM(domain, full, key, withSecrets)
	if ok && strings.Contains(strings.ToLower(kind), "let") {
		mc.Kind, mc.AutoRenew = store.CertKindACME, true
	}
	return mc, ok
}

// renderedNginx returns the server block FASTPANEL generated for the site
// (virtualhost_configuration.frontend): not something to carry over as is,
// but the allow-list inside it is worth keeping.
func (f *fastpanelSource) renderedNginx(ctx context.Context, site *fpSite) string {
	var frontend sql.NullString
	if err := f.db.QueryRowContext(ctx, `SELECT frontend FROM virtualhost_configuration WHERE virtualhost_id=?`, site.id).Scan(&frontend); err != nil || !frontend.Valid {
		return ""
	}
	return frontend.String
}

var nginxAllowRe = regexp.MustCompile(`(?m)^\s*allow\s+([0-9a-fA-F.:/]+)\s*;`)

// allowFromNginx collects the allow directives of a server block that ends
// in deny all: the site was closed to everyone else, keep it that way.
func allowFromNginx(text string) []string {
	if !regexp.MustCompile(`(?m)^\s*deny\s+all\s*;`).MatchString(text) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range nginxAllowRe.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func (f *fastpanelSource) mailboxCount(ctx context.Context, ownerID int64) int {
	var n int
	f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE owner_id=?`, ownerID).Scan(&n) //nolint:errcheck // счётчик только для заметки
	return n
}

// files streams data/www of the account: the layout matches ours.
func (f *fastpanelSource) files(ctx context.Context, req migrateFilesRequest) (io.ReadCloser, error) {
	if req.part != "home" {
		return nil, fmt.Errorf("FASTPANEL: часть %s не переносится", req.part)
	}
	if err := f.inventory(ctx); err != nil {
		return nil, err
	}
	members := []string{"www"}
	transforms := []string{tarRename("www", "data/www", "S")}
	transforms = append(transforms, f.symlinkTransforms()...)
	return f.tarStream(ctx, f.account.home, members, transforms, nil)
}
