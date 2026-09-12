package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// BitrixVM («1С-Битрикс: Веб-окружение», bitrix-env) — не панель, а
// преднастроенный сервер: один unix-пользователь bitrix, сайты в
// /home/bitrix/www (основной) и /home/bitrix/ext_www/<домен>, nginx-конфиги в
// /etc/nginx/bx/site_enabled, PHP одной ветки на всех, MySQL с паролем root в
// /root/.my.cnf. Реквизиты базы каждый сайт хранит сам в bitrix/.settings.php
// (и в старом bitrix/php_interface/dbconn.php), поэтому пароли баз известны
// открытым текстом и на этой стороне заводятся заново, а не хешами.
//
// Сайты типа link делят ядро основного сайта через абсолютные симлинки
// (/home/bitrix/www/bitrix); tar переписывает их цели под новый домашний
// каталог, а settle правит те же пути в dbconn.php и .settings*.php.

const bitrixHome = "/home/bitrix"

type bitrixVMSource struct {
	foreignSource
	sites []*bxSite
	php   string
}

// bxSite is one nginx server on the old machine.
type bxSite struct {
	root    string
	domain  string
	aliases []string
	conf    []string // nginx files that describe it
	cert    string   // ssl_certificate path
	key     string
	link    bool // bitrix/ is a symlink into another site (a link site)
	db      bxDB
}

type bxDB struct {
	host, name, login, password string
}

func (s *Server) openBitrixVM(ctx context.Context, req apitypes.MigrationSourceRequest) (migrateSource, error) {
	login := "bitrix"
	if req.Scope != "" && scopeLogin(req.Scope) != login {
		return nil, huma.Error422UnprocessableEntity("у BitrixVM один аккаунт — bitrix; область переноса user:bitrix (другой логин здесь: --as)")
	}
	if req.As != "" {
		login = req.As
	}
	conn, err := dialSSH(ctx, req.Source, req.Password, req.Key)
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	src := &bitrixVMSource{foreignSource: foreignSource{s: s, conn: conn, req: req, login: login, home: path.Join(s.cfg.WWWRoot, login)}}
	if !conn.exists(ctx, "/etc/nginx/bx") || !conn.exists(ctx, bitrixHome) {
		conn.close()
		return nil, huma.Error422UnprocessableEntity("на " + req.Source + " не видно BitrixVM: нет /etc/nginx/bx и /home/bitrix")
	}
	return src, nil
}

// inventory reads the sites once; bundle and files both need it.
func (b *bitrixVMSource) inventory(ctx context.Context) error {
	if b.sites != nil {
		return nil
	}
	confs := b.conn.glob(ctx, "/etc/nginx/bx/site_enabled/*.conf")
	if len(confs) == 0 {
		return errors.New("в /etc/nginx/bx/site_enabled нет ни одного сайта")
	}
	byRoot := map[string]*bxSite{}
	var order []string
	for _, c := range confs {
		text, err := b.conn.readFile(ctx, c)
		if err != nil {
			continue
		}
		root, names, cert, key := parseNginxServer(text)
		if root == "" || !strings.HasPrefix(root, bitrixHome+"/") {
			continue
		}
		site := byRoot[root]
		if site == nil {
			site = &bxSite{root: root}
			byRoot[root] = site
			order = append(order, root)
		}
		site.conf = append(site.conf, c)
		site.aliases = append(site.aliases, names...)
		if cert != "" && key != "" {
			site.cert, site.key = cert, key
		}
	}
	if len(order) == 0 {
		return errors.New("в /etc/nginx/bx/site_enabled нет сайтов с root внутри /home/bitrix")
	}
	sort.Strings(order)
	mainRoot := bitrixHome + "/www"
	for _, root := range order {
		site := byRoot[root]
		site.domain, site.aliases = hostDomains(site.aliases)
		if root == mainRoot && b.req.Domain != "" {
			// The default site answers to server_name _ ; the person names it.
			if site.domain != "" && site.domain != b.req.Domain {
				site.aliases = append([]string{site.domain}, site.aliases...)
			}
			site.domain = strings.ToLower(b.req.Domain)
		}
		if site.domain == "" {
			if root == mainRoot {
				return errors.New("у основного сайта BitrixVM нет доменного имени (server_name _): задайте его через --domain")
			}
			site.domain = path.Base(root)
		}
		site.link = b.isSymlink(ctx, root+"/bitrix")
		site.db = b.readDBSettings(ctx, root)
		b.sites = append(b.sites, site)
	}
	// Main site first: link sites point into it.
	sort.SliceStable(b.sites, func(i, j int) bool { return b.sites[i].root == mainRoot && b.sites[j].root != mainRoot })
	b.php = b.phpVersion(ctx, "php")
	// Old → new paths, longest first.
	for _, site := range b.sites {
		b.rewrites = append(b.rewrites, [2]string{site.root + "/", b.home + "/data/www/" + site.domain + "/"})
	}
	b.rewrites = append(b.rewrites, [2]string{bitrixHome + "/ext_www/", b.home + "/data/www/"}, [2]string{bitrixHome + "/", b.home + "/"})
	for _, site := range b.sites {
		if !site.link {
			b.configs = append(b.configs, cmsConfigs("data/www/"+site.domain)...)
		}
	}
	return nil
}

func (b *bitrixVMSource) isSymlink(ctx context.Context, p string) bool {
	_, code, err := b.conn.exec(ctx, "test -L "+shq(p))
	return err == nil && code == 0
}

var (
	nginxRootRe   = regexp.MustCompile(`(?m)^\s*root\s+([^;\s]+)\s*;`)
	nginxNamesRe  = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	nginxCertRe   = regexp.MustCompile(`(?m)^\s*ssl_certificate\s+([^;\s]+)\s*;`)
	nginxKeyRe    = regexp.MustCompile(`(?m)^\s*ssl_certificate_key\s+([^;\s]+)\s*;`)
	phpSettingRe  = regexp.MustCompile(`'(host|database|login|password)'\s*=>\s*'((?:[^'\\]|\\.)*)'`)
	phpDBVarRe    = regexp.MustCompile(`\$(DBHost|DBName|DBLogin|DBPassword)\s*=\s*["']((?:[^"'\\]|\\.)*)["']`)
	phpUnescapeRe = regexp.MustCompile(`\\(.)`)
)

// parseNginxServer pulls root, server_name and the certificate paths out of
// a server block; the first root and every server_name count.
func parseNginxServer(text string) (root string, names []string, cert, key string) {
	// Comments would otherwise contribute directives.
	var kept []string
	for _, l := range strings.Split(text, "\n") {
		if i := strings.Index(l, "#"); i >= 0 {
			l = l[:i]
		}
		kept = append(kept, l)
	}
	text = strings.Join(kept, "\n")
	if m := nginxRootRe.FindStringSubmatch(text); m != nil {
		root = strings.TrimSuffix(m[1], "/")
	}
	for _, m := range nginxNamesRe.FindAllStringSubmatch(text, -1) {
		names = append(names, strings.Fields(m[1])...)
	}
	if m := nginxCertRe.FindStringSubmatch(text); m != nil {
		cert = m[1]
	}
	if m := nginxKeyRe.FindStringSubmatch(text); m != nil {
		key = m[1]
	}
	return root, names, cert, key
}

// readDBSettings takes the connection parameters from bitrix/.settings.php
// or, failing that, bitrix/php_interface/dbconn.php.
func (b *bitrixVMSource) readDBSettings(ctx context.Context, root string) bxDB {
	var db bxDB
	if text, err := b.conn.readFile(ctx, root+"/bitrix/.settings.php"); err == nil {
		if i := strings.Index(text, "'connections'"); i >= 0 {
			text = text[i:]
		}
		for _, m := range phpSettingRe.FindAllStringSubmatch(text, -1) {
			v := phpUnescapeRe.ReplaceAllString(m[2], "$1")
			switch m[1] {
			case "host":
				if db.host == "" {
					db.host = v
				}
			case "database":
				if db.name == "" {
					db.name = v
				}
			case "login":
				if db.login == "" {
					db.login = v
				}
			case "password":
				if db.password == "" {
					db.password = v
				}
			}
		}
	}
	if db.name == "" {
		if text, err := b.conn.readFile(ctx, root+"/bitrix/php_interface/dbconn.php"); err == nil {
			for _, m := range phpDBVarRe.FindAllStringSubmatch(text, -1) {
				v := phpUnescapeRe.ReplaceAllString(m[2], "$1")
				switch m[1] {
				case "DBHost":
					db.host = v
				case "DBName":
					db.name = v
				case "DBLogin":
					db.login = v
				case "DBPassword":
					db.password = v
				}
			}
		}
	}
	return db
}

func (b *bitrixVMSource) bundle(ctx context.Context, _ string, withSecrets bool) (*apitypes.MigrationBundle, error) {
	if err := b.inventory(ctx); err != nil {
		return nil, err
	}
	home, shell, hash, err := b.unixAccount(ctx, "bitrix")
	if err != nil {
		return nil, err
	}
	hostname, _, _ := b.conn.exec(ctx, "hostname -f 2>/dev/null || hostname")
	version, _, _ := b.conn.exec(ctx, "rpm -q --qf '%{VERSION}' bitrix-env 2>/dev/null")
	out := &apitypes.MigrationBundle{
		Panel: strings.TrimSpace("BitrixVM " + strings.TrimSpace(version)), Hostname: strings.TrimSpace(hostname), Family: "rhel", Scope: "user:bitrix",
		Generated: time.Now().UTC(), SiteNginx: map[string]string{}, Sizes: apitypes.MigrationSizes{Databases: map[string]int64{}},
		Certificates: []apitypes.MigrationCert{}, Notes: []string{b.fingerprintNote()},
	}
	out.User = &store.User{Login: "bitrix", Role: store.RoleUser, Shell: shell != "" && !strings.HasSuffix(shell, "nologin") && !strings.HasSuffix(shell, "/false"), Home: home}
	seenDB := map[string]bool{}
	var plain map[string]string
	if withSecrets {
		plain = map[string]string{}
	}
	for _, site := range b.sites {
		row := newForeignSite(site.domain, site.aliases, b.php)
		row.Preset, row.CMS = presetBitrix, "bitrix"
		row.ClientMaxBody = "256m"
		if !b.conn.exists(ctx, site.root+"/bitrix") {
			row.Preset, row.CMS = "", ""
		}
		out.Sites = append(out.Sites, row)
		out.Sizes.FilesBytes += b.duBytes(ctx, site.root)
		if site.link {
			out.Notes = append(out.Notes, fmt.Sprintf("сайт %s типа link: ядро общее с основным сайтом, симлинки перепишутся на новый путь", site.domain))
		}
		if site.db.name != "" && !seenDB[site.db.name] {
			seenDB[site.db.name] = true
			d, err := b.databaseRow(ctx, site.db.name)
			if err != nil {
				return nil, err
			}
			if site.db.login != "" {
				accs, err := b.dbAccounts(ctx, site.db.login)
				if err != nil {
					return nil, err
				}
				if len(accs) == 0 {
					accs = []*store.DBUser{{Name: site.db.login, Host: "localhost"}}
				}
				d.Users = accs
				if plain != nil {
					for _, acc := range accs {
						plain[acc.Name+"@"+acc.Host] = b.createUser(ctx, acc, site.db.password)
					}
				}
			}
			out.Databases = append(out.Databases, d)
			out.Sizes.Databases[d.Name] = d.SizeBytes
		} else if site.db.name == "" && row.CMS == "bitrix" {
			out.Notes = append(out.Notes, "сайт "+site.domain+": реквизиты базы в bitrix/.settings.php не найдены, база не переносится")
		}
		if site.cert != "" {
			certPEM, cerr := b.conn.readFile(ctx, site.cert)
			keyPEM, kerr := b.conn.readFile(ctx, site.key)
			if cerr == nil && kerr == nil {
				if mc, ok := certificateFromPEM(site.domain, certPEM, keyPEM, withSecrets); ok {
					out.Certificates = append(out.Certificates, mc)
				}
			}
		}
	}
	out.Cron = b.crontab(ctx, "bitrix")
	out.Notes = append(out.Notes,
		"кеш Битрикса (bitrix/cache, managed_cache, stack_cache) не переносится — соберётся заново",
		"push-сервер, memcached и msmtp окружения BitrixVM не переезжают: настройте их здесь отдельно (mp stack install memcached)",
		"пароль веб-панели у аккаунта не задан: mp user set "+b.login+" --generate")
	if withSecrets {
		out.Secrets = &apitypes.MigrationSecrets{UnixShadow: hash, DBUsers: plain, Mailboxes: map[string]string{}, DKIM: map[string]string{}}
	}
	return out, nil
}

// files streams every site into data/www/<domain>; symlink targets that
// pointed into /home/bitrix are rewritten on the way.
func (b *bitrixVMSource) files(ctx context.Context, req migrateFilesRequest) (io.ReadCloser, error) {
	if req.part != "home" {
		return nil, fmt.Errorf("BitrixVM: часть %s не переносится", req.part)
	}
	if err := b.inventory(ctx); err != nil {
		return nil, err
	}
	var members, transforms []string
	for _, site := range b.sites {
		rel := strings.TrimPrefix(site.root, bitrixHome+"/")
		members = append(members, rel)
		// Names only (flag S): symlink targets get their own rules below.
		transforms = append(transforms, tarRename(rel, "data/www/"+site.domain, "S"))
	}
	transforms = append(transforms, b.symlinkTransforms()...)
	excludes := []string{"*/bitrix/cache/*", "*/bitrix/managed_cache/*", "*/bitrix/stack_cache/*", "*/bitrix/tmp/*", "*/upload/resize_cache/*"}
	return b.tarStream(ctx, bitrixHome, members, transforms, excludes)
}
