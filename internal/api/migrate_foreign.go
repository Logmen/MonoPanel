package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// Адаптеры чужих панелей (docs/07 §6). Каждый читает свой источник по ssh и
// отдаёт тот же MigrationBundle, что и MonoPanel: дальше импорт общий, и
// проверка конфликтов работает одинаково. Здесь — то, что у адаптеров общее:
// MySQL старого сервера, crontab, shadow, сертификаты, tar и правка путей.

// foreignSource is the part the ssh adapters share.
type foreignSource struct {
	s    *Server
	conn *sshConn
	req  apitypes.MigrationSourceRequest
	// login and home are what the account gets on this server; the adapters
	// need them to rewrite absolute paths of the old one.
	login string
	home  string
	// mysql is the client invocation that works on the old server (found once).
	mysql string
	// rewrites maps old absolute paths to new ones, longest prefix first.
	rewrites [][2]string
	// configs lists files (relative to home) whose old paths get rewritten
	// once they landed here.
	configs []string
}

func (f *foreignSource) close() { f.conn.close() }

// note on the fingerprint: the plan shows it so the administrator can
// compare it with what the old server prints.
func (f *foreignSource) fingerprintNote() string {
	return "отпечаток ключа ssh источника: " + f.conn.fingerprint
}

// ------------------------------------------------------------- MySQL ----

// findMySQL picks a mysql client invocation that authenticates on the old
// server: root's ~/.my.cnf (BitrixVM, FASTPANEL write it), Debian's
// maintenance account, or nothing at all with unix_socket auth.
func (f *foreignSource) findMySQL(ctx context.Context) error {
	for _, c := range []string{"mysql", "mysql --defaults-extra-file=/etc/mysql/debian.cnf", "mariadb"} {
		if _, code, err := f.conn.exec(ctx, c+" -N -B -e 'SELECT 1' 2>/dev/null"); err == nil && code == 0 {
			f.mysql = c
			return nil
		}
	}
	return errors.New("на источнике нет доступа к MySQL от root: нужен /root/.my.cnf с паролем root")
}

// query runs SQL through the old server's client and returns rows of
// tab-separated fields.
func (f *foreignSource) query(ctx context.Context, sql string) ([][]string, error) {
	if f.mysql == "" {
		if err := f.findMySQL(ctx); err != nil {
			return nil, err
		}
	}
	out, _, err := f.conn.exec(ctx, f.mysql+" -N -B --raw -e "+shq(sql))
	if err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
	}
	var rows [][]string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		rows = append(rows, strings.Split(l, "\t"))
	}
	return rows, nil
}

// databaseRow describes a database the way the bundle carries it, with the
// size read from information_schema.
func (f *foreignSource) databaseRow(ctx context.Context, name string) (*store.Database, error) {
	d := &store.Database{Name: name, Charset: "utf8mb4"}
	rows, err := f.query(ctx, fmt.Sprintf("SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='%s'", sqlEscaper.Replace(name)))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("базы %s на источнике нет", name)
	}
	if len(rows[0]) == 2 {
		d.Charset, d.Collation = rows[0][0], rows[0][1]
	}
	if rows, err := f.query(ctx, fmt.Sprintf("SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.TABLES WHERE table_schema='%s'", sqlEscaper.Replace(name))); err == nil && len(rows) == 1 {
		d.SizeBytes, _ = strconv.ParseInt(rows[0][0], 10, 64)
	}
	return d, nil
}

// dbAccounts lists 'user'@'host' pairs of one login on the old server.
func (f *foreignSource) dbAccounts(ctx context.Context, login string) ([]*store.DBUser, error) {
	rows, err := f.query(ctx, fmt.Sprintf("SELECT host, plugin FROM mysql.user WHERE user='%s' ORDER BY host", sqlEscaper.Replace(login)))
	if err != nil {
		return nil, err
	}
	var accs []*store.DBUser
	for _, r := range rows {
		if len(r) < 1 {
			continue
		}
		acc := &store.DBUser{Name: login, Host: r[0]}
		if len(r) > 1 {
			acc.AuthPlugin = r[1]
		}
		accs = append(accs, acc)
	}
	return accs, nil
}

// createUser returns the CREATE USER statement with the password hash, the
// way SHOW CREATE USER prints it; plain says the password is known and the
// statement is built from it instead — that works on any server.
func (f *foreignSource) createUser(ctx context.Context, acc *store.DBUser, plain string) string {
	if plain != "" {
		return fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED BY '%s'", acc.Name, acc.Host, sqlEscaper.Replace(plain))
	}
	show := fmt.Sprintf("SHOW CREATE USER '%s'@'%s'", acc.Name, acc.Host)
	rows, err := f.query(ctx, "SET SESSION print_identified_with_as_hex = ON; "+show)
	if err != nil {
		rows, err = f.query(ctx, show)
	}
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return ""
	}
	return strings.Replace(rows[0][0], "CREATE USER ", "CREATE USER IF NOT EXISTS ", 1)
}

func (f *foreignSource) dump(ctx context.Context, _, db string) (io.ReadCloser, error) {
	if f.mysql == "" {
		if err := f.findMySQL(ctx); err != nil {
			return nil, err
		}
	}
	cmd := strings.Replace(strings.Replace(f.mysql, "mysql", "mysqldump", 1), "mariadb", "mariadb-dump", 1)
	return f.conn.stream(ctx, cmd+" --single-transaction --quick --routines --triggers --events --databases "+shq(db))
}

// ------------------------------------------------------------- files ----

// tarStream starts tar on the old server for the given members (relative
// to dir), rewriting names and symlink targets with the sed expressions of
// transforms. Exit status 1 ("file changed as we read it") is a live site,
// not a failure.
func (f *foreignSource) tarStream(ctx context.Context, dir string, members, transforms, excludes []string) (io.ReadCloser, error) {
	cmd := "tar -C " + shq(dir) + " --warning=no-file-changed --warning=no-file-removed"
	for _, e := range excludes {
		cmd += " --exclude=" + shq(e)
	}
	for _, t := range transforms {
		cmd += " --transform=" + shq(t)
	}
	cmd += " -cf -"
	for _, m := range members {
		cmd += " " + shq(m)
	}
	return f.conn.stream(ctx, cmd+" || [ $? -eq 1 ]")
}

// rewriteText replaces every old path inside a text (a config file, a cron
// command).
func (f *foreignSource) rewriteText(text string) string {
	for _, r := range f.rewrites {
		old := strings.TrimSuffix(r[0], "/")
		to := strings.TrimSuffix(r[1], "/")
		text = strings.ReplaceAll(text, old+"/", to+"/")
		text = regexp.MustCompile(regexp.QuoteMeta(old)+`(["'\s;:])`).ReplaceAllString(text, to+"$1")
	}
	return text
}

// symlinkTransforms turns the rewrite map into tar --transform expressions
// for symlink targets only (flag R): member names are placed by the adapter
// itself.
func (f *foreignSource) symlinkTransforms() []string {
	var out []string
	for _, r := range f.rewrites {
		out = append(out, tarRename(strings.TrimSuffix(r[0], "/"), strings.TrimSuffix(r[1], "/"), "R"))
	}
	return out
}

// tarRename builds one sed expression for tar --transform that maps the
// path from (and everything under it) onto to; flags is R for symlink
// targets only, S for member names only.
func tarRename(from, to, flags string) string {
	return "s|^" + sedPattern(from) + "\\(/\\|$\\)|" + sedReplacement(to) + "\\1|" + flags
}

// sedPattern quotes a literal path for the left side of s|||.
func sedPattern(s string) string {
	return strings.NewReplacer("\\", "\\\\", "|", "\\|", ".", "\\.", "*", "\\*", "[", "\\[", "]", "\\]", "^", "\\^", "$", "\\$").Replace(s)
}

// sedReplacement quotes a literal path for the right side: only the
// delimiter, & and backslash mean anything there.
func sedReplacement(s string) string {
	return strings.NewReplacer("\\", "\\\\", "|", "\\|", "&", "\\&").Replace(s)
}

// settle rewrites the old server's paths in the config files that came
// with the sites. Runs as the client through fsop, so nothing outside the
// home can be touched.
func (f *foreignSource) settle(ctx context.Context, jc *jobs.Context, u *store.User) error {
	if len(f.rewrites) == 0 || len(f.configs) == 0 {
		return nil
	}
	var fixed []string
	for _, rel := range f.configs {
		raw, err := f.s.fsop(ctx, u, nil, "read", rel)
		if err != nil {
			continue
		}
		text := f.rewriteText(string(raw))
		if text == string(raw) {
			continue
		}
		if _, err := f.s.fsop(ctx, u, []byte(text), "write", rel); err != nil {
			jc.Logf("%s: пути не переписаны: %v", rel, err)
			continue
		}
		fixed = append(fixed, rel)
	}
	if len(fixed) > 0 {
		jc.Logf("пути старого сервера переписаны в: %s", strings.Join(fixed, ", "))
	}
	return nil
}

// ------------------------------------------------------------ system ----

// unixAccount reads the passwd and shadow entries of a login.
func (f *foreignSource) unixAccount(ctx context.Context, login string) (home, shell, hash string, err error) {
	out, _, err := f.conn.exec(ctx, "getent passwd "+shq(login))
	if err != nil {
		return "", "", "", fmt.Errorf("unix-пользователя %s на источнике нет", login)
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) >= 7 {
		home, shell = fields[5], fields[6]
	}
	if out, _, err := f.conn.exec(ctx, "getent shadow "+shq(login)); err == nil {
		if sf := strings.Split(strings.TrimSpace(out), ":"); len(sf) >= 2 && len(sf[1]) > 2 {
			hash = sf[1]
		}
	}
	return home, shell, hash, nil
}

// crontab reads a user's crontab and turns it into jobs; commands get their
// paths rewritten.
func (f *foreignSource) crontab(ctx context.Context, login string) []*store.CronJob {
	out, _, err := f.conn.exec(ctx, "crontab -l -u "+shq(login)+" 2>/dev/null")
	if err != nil {
		return nil
	}
	return f.parseCrontab(out)
}

var cronLineRe = regexp.MustCompile(`^(@\w+|(?:\S+\s+){4}\S+)\s+(.+)$`)

func (f *foreignSource) parseCrontab(text string) []*store.CronJob {
	var jobs []*store.CronJob
	comment := ""
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
			comment = ""
			continue
		case strings.HasPrefix(l, "#"):
			comment = strings.TrimSpace(strings.TrimPrefix(l, "#"))
			continue
		case strings.Contains(l, "=") && !strings.ContainsAny(strings.SplitN(l, "=", 2)[0], " \t*"):
			continue // PATH=… and friends
		}
		m := cronLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		jobs = append(jobs, &store.CronJob{Schedule: m[1], Command: f.rewriteText(m[2]), Comment: comment, Enabled: true})
		comment = ""
	}
	return jobs
}

// phpVersion asks the old server's CLI PHP for its branch.
func (f *foreignSource) phpVersion(ctx context.Context, bin string) string {
	if bin == "" {
		bin = "php"
	}
	out, _, err := f.conn.exec(ctx, bin+" -r 'echo PHP_MAJOR_VERSION.\".\".PHP_MINOR_VERSION;' 2>/dev/null")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// detectCMS looks at the files of a docroot: the preset gives the site the
// nginx rules it had on the old server without anyone writing them again.
func (f *foreignSource) detectCMS(ctx context.Context, docroot string) (preset, cms string) {
	checks := []struct{ file, preset, cms string }{
		{"bitrix/modules/main/classes", presetBitrix, "bitrix"},
		{"wp-includes/version.php", presetWordPress, "wordpress"},
		{"administrator/manifests/files/joomla.xml", presetJoomla, "joomla"},
		{"system/startup.php", presetOpenCart, "opencart"},
	}
	for _, c := range checks {
		if f.conn.exists(ctx, docroot+"/"+c.file) {
			return c.preset, c.cms
		}
	}
	return "", ""
}

// cmsConfigs lists the config files under a site dir (relative to home)
// worth a path rewrite.
func cmsConfigs(siteRel string) []string {
	var out []string
	for _, c := range []string{"wp-config.php", "configuration.php", "config.php", "admin/config.php", "bitrix/.settings.php", "bitrix/.settings_extra.php", "bitrix/php_interface/dbconn.php", ".htaccess"} {
		out = append(out, siteRel+"/"+c)
	}
	return out
}

// duBytes sizes a directory on the old server.
func (f *foreignSource) duBytes(ctx context.Context, dir string) int64 {
	out, _, err := f.conn.exec(ctx, "du -sb "+shq(dir)+" 2>/dev/null")
	if err != nil {
		return 0
	}
	if fields := strings.Fields(out); len(fields) > 0 {
		n, _ := strconv.ParseInt(fields[0], 10, 64)
		return n
	}
	return 0
}

// ------------------------------------------------------ certificates ----

// certificateFromPEM validates a certificate that came with a site and
// builds the bundle entry; self-signed and expired ones are left behind
// (the target issues its own after the DNS switch).
func certificateFromPEM(name, certPEM, keyPEM string, withSecrets bool) (apitypes.MigrationCert, bool) {
	certPEM, keyPEM = strings.TrimSpace(certPEM), strings.TrimSpace(keyPEM)
	if certPEM == "" || keyPEM == "" {
		return apitypes.MigrationCert{}, false
	}
	info, err := acme.ParseCertificatePEM([]byte(certPEM + "\n"))
	if err != nil || info.SelfSigned || time.Now().After(info.NotAfter) {
		return apitypes.MigrationCert{}, false
	}
	na := info.NotAfter
	mc := apitypes.MigrationCert{Name: name, Names: info.Names, Kind: store.CertKindCustom, NotAfter: &na}
	if strings.Contains(strings.ToLower(info.Issuer+" "+info.IssuerOrg), "let's encrypt") {
		mc.Kind, mc.AutoRenew = store.CertKindACME, true
	}
	if withSecrets {
		mc.Cert, mc.Key = certPEM+"\n", keyPEM+"\n"
	}
	return mc, true
}

// ----------------------------------------------------------- helpers ----

// hostDomains splits an nginx server_name into the main name and aliases;
// www. of the main name is folded into the site (the panel serves it itself).
func hostDomains(names []string) (string, []string) {
	var clean []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || n == "_" || strings.ContainsAny(n, "*~$") || seen[n] {
			continue
		}
		seen[n] = true
		clean = append(clean, n)
	}
	if len(clean) == 0 {
		return "", nil
	}
	main := clean[0]
	for _, n := range clean {
		if !strings.HasPrefix(n, "www.") {
			main = n
			break
		}
	}
	var aliases []string
	for _, n := range clean {
		if n != main && n != "www."+main {
			aliases = append(aliases, n)
		}
	}
	sort.Strings(aliases)
	return main, aliases
}

// newForeignSite fills the defaults a site created through the API gets.
func newForeignSite(domain string, aliases []string, php string) *store.Site {
	return &store.Site{
		Domain: domain, Aliases: aliases, Mode: store.ModeFPM, PHPVersion: php, SSL: "auto",
		HTTP2: true, RedirectHTTPS: true, StaticByNginx: true, Status: store.SitePending, PHPIni: map[string]string{},
	}
}

// scopeLogin returns the login of user:<login>.
func scopeLogin(scope string) string {
	return strings.TrimPrefix(strings.TrimSpace(scope), "user:")
}
