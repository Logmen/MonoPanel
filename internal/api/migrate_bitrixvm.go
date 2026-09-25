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
	// skipped explains the nginx servers that are not taken over.
	skipped []string
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
	// cache is the cache engine the site's settings name ("" for files).
	cache string
	// hardcoded lists files (old absolute paths) that name /home/bitrix.
	hardcoded []string
}

type bxDB struct {
	host, name, login, password string
}

func (s *Server) openBitrixVM(ctx context.Context, req apitypes.MigrationSourceRequest) (migrateSource, error) {
	login := "bitrix"
	if req.Scope != "" && scopeLogin(req.Scope) != login {
		return nil, huma.Error422UnprocessableEntity("BitrixVM has one account, bitrix: the scope is user:bitrix (another login here: --as)")
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
		return nil, huma.Error422UnprocessableEntity("BitrixVM not found on " + req.Source + ": /etc/nginx/bx or /home/bitrix is missing")
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
		return errors.New("no sites in /etc/nginx/bx/site_enabled")
	}
	byRoot := map[string]*bxSite{}
	var order []string
	for _, c := range confs {
		text, err := b.conn.readFile(ctx, c)
		if err != nil {
			continue
		}
		root, names, cert, key := parseNginxServer(text)
		if root == "" {
			continue // push server, status pages: nothing to serve from disk
		}
		if !strings.HasPrefix(root, bitrixHome+"/") {
			b.skipped = append(b.skipped, fmt.Sprintf("%s skipped: root %s is outside %s", path.Base(c), root, bitrixHome))
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
		msg := "no site in /etc/nginx/bx/site_enabled has its root inside /home/bitrix"
		if len(b.skipped) > 0 {
			msg += ": " + strings.Join(b.skipped, "; ")
		}
		return errors.New(msg)
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
				return errors.New("the main BitrixVM site has no domain name (server_name _): set one with --domain")
			}
			site.domain = path.Base(root)
		}
		site.link = b.isSymlink(ctx, root+"/bitrix")
		site.db = b.readDBSettings(ctx, root)
		if !site.link {
			site.cache = b.cacheEngine(ctx, root)
		}
		site.hardcoded = b.hardcodedPaths(ctx, site)
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
	seen := map[string]bool{}
	for _, site := range b.sites {
		rel := "data/www/" + site.domain
		var files []string
		if !site.link {
			files = cmsConfigs(rel)
		}
		// Scripts that name the old paths themselves (exports writing to
		// /home/bitrix/www/..., logs in /home/bitrix): the old paths do not
		// exist here, so they get the same rewrite as the configs.
		for _, f := range site.hardcoded {
			files = append(files, rel+"/"+strings.TrimPrefix(f, site.root+"/"))
		}
		for _, f := range files {
			if !seen[f] {
				seen[f] = true
				b.configs = append(b.configs, f)
			}
		}
	}
	return nil
}

// hardcodedPaths finds the site's own files that name /home/bitrix: the
// kernel (bitrix/) and upload/ are left out, bitrix/php_interface is the
// site's code and is looked at. Files over 1 MB are logs and dumps, not
// scripts, and the rewrite reads each file whole.
func (b *bitrixVMSource) hardcodedPaths(ctx context.Context, site *bxSite) []string {
	grep := " -type f -size -1024k -print0 2>/dev/null | xargs -0 -r grep -lI -e " + shq(bitrixHome) + " -- 2>/dev/null"
	cmd := "cd " + shq(site.root) + " && { find . \\( -path ./bitrix -o -path ./upload -o -name .git \\) -prune -o" + grep
	if !site.link {
		cmd += "; find ./bitrix/php_interface" + grep
	}
	out, _, _ := b.conn.exec(ctx, cmd+"; } | head -n "+fmt.Sprint(maxHardcoded))
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "./") {
			files = append(files, site.root+"/"+strings.TrimPrefix(l, "./"))
		}
	}
	return files
}

// maxHardcoded caps the files rewritten per site: more than that is a copy
// of something (a backup, a vendor tree) rather than the site's own scripts.
const maxHardcoded = 200

var (
	bxCacheBlockRe = regexp.MustCompile(`'cache'\s*=>`)
	bxCacheTypeRe  = regexp.MustCompile(`'(?:class_name|type)'\s*=>\s*'([^']+)'`)
	bxCacheDefRe   = regexp.MustCompile(`define\(\s*["'](?:BX_CACHE_TYPE|BX_CACHE_CLASS_FILE)["']\s*,\s*["']([^"']+)["']`)
)

// cacheEngine reads which cache the site is set up for: the 'cache' block
// of bitrix/.settings.php, then BX_CACHE_TYPE of dbconn.php. "files" (the
// default) comes back as "".
func (b *bitrixVMSource) cacheEngine(ctx context.Context, root string) string {
	engine := ""
	if text, err := b.conn.readFile(ctx, root+"/bitrix/.settings.php"); err == nil {
		if loc := bxCacheBlockRe.FindStringIndex(text); loc != nil {
			block := text[loc[1]:]
			if len(block) > 1000 {
				block = block[:1000]
			}
			if m := bxCacheTypeRe.FindStringSubmatch(block); m != nil {
				engine = m[1]
			}
		}
	}
	if engine == "" {
		if text, err := b.conn.readFile(ctx, root+"/bitrix/php_interface/dbconn.php"); err == nil {
			if m := bxCacheDefRe.FindStringSubmatch(text); m != nil {
				engine = m[1]
			}
		}
	}
	if strings.EqualFold(engine, "files") {
		return ""
	}
	return strings.ReplaceAll(engine, `\\`, `\`)
}

func (b *bitrixVMSource) isSymlink(ctx context.Context, p string) bool {
	_, code, err := b.conn.exec(ctx, "test -L "+shq(p))
	return err == nil && code == 0
}

var (
	nginxRootRe   = regexp.MustCompile(`(?m)^\s*root\s+("[^"]*"|'[^']*'|[^;\s]+)\s*;`)
	nginxNamesRe  = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	nginxCertRe   = regexp.MustCompile(`(?m)^\s*ssl_certificate\s+("[^"]*"|'[^']*'|[^;\s]+)\s*;`)
	nginxKeyRe    = regexp.MustCompile(`(?m)^\s*ssl_certificate_key\s+("[^"]*"|'[^']*'|[^;\s]+)\s*;`)
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
		root = strings.TrimSuffix(nginxUnquote(m[1]), "/")
	}
	for _, m := range nginxNamesRe.FindAllStringSubmatch(text, -1) {
		for _, n := range strings.Fields(m[1]) {
			names = append(names, nginxUnquote(n))
		}
	}
	if m := nginxCertRe.FindStringSubmatch(text); m != nil {
		cert = nginxUnquote(m[1])
	}
	if m := nginxKeyRe.FindStringSubmatch(text); m != nil {
		key = nginxUnquote(m[1])
	}
	return root, names, cert, key
}

// nginxUnquote drops the quotes nginx allows around a value: bitrix-env
// writes root "/home/bitrix/ext_www/<site>"; for its ext sites.
func nginxUnquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
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
	out.Notes = append(out.Notes, b.skipped...)
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
			out.Notes = append(out.Notes, fmt.Sprintf("site %s is a link site: it shares the core with the main site, and its symlinks will be rewritten to the new path", site.domain))
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
			out.Notes = append(out.Notes, "site "+site.domain+": no database credentials found in bitrix/.settings.php, so the database is not moved")
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
	rootJobs, rootNotes := b.rootCron(ctx, out.Cron)
	out.Cron = append(out.Cron, rootJobs...)
	for _, j := range out.Cron {
		j.Command = bxCronPHP(j.Command)
	}
	out.Notes = append(out.Notes, rootNotes...)
	out.Notes = append(out.Notes, cronNotes(out.Cron)...)
	for _, site := range b.sites {
		if site.cache != "" {
			out.Notes = append(out.Notes, "site "+site.domain+": the Bitrix cache is "+site.cache+". Check that it works here (memcached: mp stack install memcached; the cluster cache needs the cluster module and its tables) or switch to files in bitrix/.settings.php: with a broken cache the templates recompute everything on every hit and tie up all the php-fpm processes")
		}
		if n := len(site.hardcoded); n > 0 {
			list := site.hardcoded
			if n > 5 {
				list = list[:5]
			}
			more := ""
			if n > 5 {
				more = fmt.Sprintf(" and %d more", n-5)
			}
			if n >= maxHardcoded {
				more += " (not all are listed)"
			}
			out.Notes = append(out.Notes, fmt.Sprintf("site %s: %s is hard-coded in the site's files (%d): %s%s — it will be rewritten to the new path", site.domain, bitrixHome, n, strings.Join(list, ", "), more))
		}
	}
	out.Notes = append(out.Notes,
		"the Bitrix cache (bitrix/cache, managed_cache, stack_cache) is not moved: it will be rebuilt",
		"the push server, memcached and msmtp of the BitrixVM environment do not move: set them up here separately (mp stack install memcached)",
		"the account has no web-panel password: mp user set "+b.login+" --generate")
	if withSecrets {
		out.Secrets = &apitypes.MigrationSecrets{UnixShadow: hash, DBUsers: plain, Mailboxes: map[string]string{}, DKIM: map[string]string{}}
	}
	return out, nil
}

// rootCron picks the jobs of root's crontab that work on the sites: on
// BitrixVM everything is done as root, exports and imports included. They
// move to the account (paths rewritten, marked as root's); the rest stays
// behind and the plan lists it.
func (b *bitrixVMSource) rootCron(ctx context.Context, have []*store.CronJob) ([]*store.CronJob, []string) {
	out, _, err := b.conn.exec(ctx, "crontab -l -u root 2>/dev/null")
	if err != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, j := range have {
		seen[j.Schedule+" "+j.Command] = true
	}
	var jobs []*store.CronJob
	var left []string
	for _, e := range cronEntries(out) {
		if !strings.Contains(e.command, bitrixHome+"/") {
			left = append(left, e.schedule+" "+e.command)
			continue
		}
		j := b.cronJob(e)
		if seen[j.Schedule+" "+j.Command] {
			continue
		}
		seen[j.Schedule+" "+j.Command] = true
		j.Comment = strings.TrimSuffix("from root's crontab; "+j.Comment, "; ")
		jobs = append(jobs, j)
	}
	var notes []string
	if len(jobs) > 0 {
		notes = append(notes, fmt.Sprintf("cron jobs moved from root's crontab: %d; they work with %s and run as the account here", len(jobs), bitrixHome))
	}
	if len(left) > 0 {
		notes = append(notes, fmt.Sprintf("jobs in root's crontab without %s paths are not moved: %s", bitrixHome, strings.Join(left, "; ")))
	}
	return jobs, notes
}

// bxCronPHPRe matches the system PHP a BitrixVM crontab names by path.
var bxCronPHPRe = regexp.MustCompile(`(^|[\s;&|(])/usr(?:/local)?/bin/php(\s)`)

// bxCronPHP points cron's PHP at the account's own: /usr/bin/php here is
// whatever branch the distribution defaults to, while the crontab's PATH
// starts with data/bin, where php is the branch of the sites.
func bxCronPHP(cmd string) string {
	return bxCronPHPRe.ReplaceAllString(cmd, "${1}php$2")
}

// files streams every site into data/www/<domain>; symlink targets that
// pointed into /home/bitrix are rewritten on the way.
func (b *bitrixVMSource) files(ctx context.Context, req migrateFilesRequest) (io.ReadCloser, error) {
	if req.part != "home" {
		return nil, fmt.Errorf("BitrixVM: the %s part is not moved", req.part)
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
