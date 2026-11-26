package api

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os/user"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A source is another server: a FASTPANEL or BitrixVM host read over ssh, or
// a MonoPanel whose administrator is not ours. What it says about an account
// is input, not configuration, and the receiving panel holds every value of a
// bundle to the rule the API applies to the same value typed by a person:
// a site's names and docroot, a php_value, a cron line, a unit file field, a
// mailbox address and its password hash all end up in a configuration file,
// a command, a path or the database here. The plan shows a value that breaks
// a rule as a conflict; the run checks the bundle again (the source may have
// changed meanwhile), leaves such items out with a line in the job log and
// ends failed, so nothing is dropped silently.

// migrateVet is the verdict on one bundle.
type migrateVet struct {
	conflicts []apitypes.MigrationIssue
	warnings  []apitypes.MigrationIssue
	// refused holds the items the import does not take because a value in
	// them breaks a rule. Keys are the bundle's own item pointers, or
	// "kind:name" strings for secrets (secretPanel, "dkim:<domain>" …).
	refused map[any]string
	// skipped holds items left behind on purpose — a database account for a
	// remote host: a warning in the plan, a line in the log, no failure.
	skipped map[any]string
	// account says the account itself is refused: nothing is imported.
	account bool
	// auth is the authentication of each database account (name@host),
	// parsed when the bundle carries the secrets.
	auth map[string]dbAuth
	// rebase moves the old home of an account from a MonoPanel source to its
	// home here, in app paths and cron commands.
	rebase [][2]string
}

// Keys of the refused secrets that are not items of their own.
const (
	secretPanel = "secret:panel"
	secretUnix  = "secret:unix"
)

func secretDKIM(domain string) string     { return "dkim:" + domain }
func secretMailbox(address string) string { return "mailbox:" + address }
func secretDBUser(account string) string  { return "dbuser:" + account }

// issue builds a conflict or a warning. Targets and texts quote values of the
// source, some through the errors of the rules they broke: they reach the
// plan, the 409 of migrate-run and the job log in one clean line each.
func issue(kind, target, text, fix string) apitypes.MigrationIssue {
	return apitypes.MigrationIssue{Kind: kind, Target: shown(target), Text: cleanText(text, false), Fix: cleanText(fix, false)}
}

func (v *migrateVet) conflict(kind, target, text string) {
	v.conflicts = append(v.conflicts, issue(kind, target, text, ""))
}

func (v *migrateVet) warn(kind, target, text, fix string) {
	v.warnings = append(v.warnings, issue(kind, target, text, fix))
}

// refuse records an item the import will not take, and why.
func (v *migrateVet) refuse(item any, kind, target, text string) {
	v.refuseFix(item, kind, target, text, "")
}

// refuseFix is refuse with a hint at what makes the item acceptable.
func (v *migrateVet) refuseFix(item any, kind, target, text, fix string) {
	if _, done := v.refused[item]; !done {
		v.refused[item] = text
	}
	v.conflicts = append(v.conflicts, issue(kind, target, text, fix))
}

// skip records an item left behind on purpose.
func (v *migrateVet) skip(item any, kind, target, text, fix string) {
	v.skipped[item] = text
	v.warn(kind, target, text, fix)
}

// why says whether an item stays behind: the reason, or "".
func (v *migrateVet) why(item any) string {
	if r, ok := v.refused[item]; ok {
		return r
	}
	return v.skipped[item]
}

// isRefused tells a refused item from one skipped on purpose.
func (v *migrateVet) isRefused(item any) bool {
	_, ok := v.refused[item]
	return ok
}

// issueLine is an issue as one line of the job log or of an error.
func issueLine(i apitypes.MigrationIssue) string {
	s := i.Kind
	if i.Target != "" {
		s += " " + i.Target
	}
	return s + ": " + i.Text
}

// vetBundle checks a bundle item by item and normalizes in place what it
// accepts (aliases sorted, the addresses of this host, paths of the old home
// moved to the new one), so the plan shows what will arrive and the run
// imports exactly that. The secrets are checked when the bundle carries them:
// the plan has none, the run has them.
func (s *Server) vetBundle(ctx context.Context, req apitypes.MigrationSourceRequest, b *apitypes.MigrationBundle, login string) *migrateVet {
	v := &migrateVet{refused: map[any]string{}, skipped: map[any]string{}, auth: map[string]dbAuth{}}
	if b.User == nil {
		v.account = true
		// Kind "user" is what another login (--as) fixes; this is not.
		v.conflict("account", shown(login), "the source sent no account")
		return v
	}
	cleanBundleText(b)
	b.Sites = nonNil(v, b.Sites, "site", "site")
	b.Databases = nonNil(v, b.Databases, "database", "database")
	b.Cron = nonNil(v, b.Cron, "cron", "cron job")
	b.Apps = nonNil(v, b.Apps, "app", "app service")
	b.Valkey = nonNil(v, b.Valkey, "valkey", "Valkey instance")
	b.MailDomains = nonNil(v, b.MailDomains, "mail", "mail domain")
	b.Mailboxes = nonNil(v, b.Mailboxes, "mail", "mailbox")
	b.MailAliases = nonNil(v, b.MailAliases, "mail", "mail alias")

	s.vetAccount(ctx, v, req, b, login)
	var ownerID int64 // an account the bundle merges into keeps its own names
	if u, err := s.db.GetUserByLogin(ctx, login); err == nil {
		ownerID = u.ID
	}
	names := s.vetSites(ctx, v, b, ownerID)
	s.vetCustomNginx(v, b)
	s.vetDatabases(ctx, v, b, login)
	v.rebase = s.migrateRebase(req, b, login)
	s.vetCron(v, b)
	s.vetApps(v, b, path.Join(s.cfg.WWWRoot, login))
	s.vetValkey(v, b)
	s.vetMail(ctx, v, b, ownerID)
	s.vetCerts(ctx, v, b, names)
	return v
}

// nonNil drops the null entries a malformed bundle may carry.
func nonNil[T any](v *migrateVet, list []*T, kind, what string) []*T {
	out := make([]*T, 0, len(list))
	for _, it := range list {
		if it == nil {
			v.conflict(kind, "", "the source sent an empty "+what+" entry")
			continue
		}
		out = append(out, it)
	}
	return out
}

// cleanText drops control characters from a text the panel only shows — a
// terminal escape in a note of the source would otherwise reach the screen of
// whoever runs mp migrate plan. keepLines keeps line breaks and tabs: the
// notes are multi-line by design.
func cleanText(s string, keepLines bool) string {
	return strings.Map(func(r rune) rune {
		if keepLines && (r == '\n' || r == '\t') {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// toolOutput is what tar or mysql printed about the source's data, as one
// entry of the job log: mysql quotes the statement it choked on, text of the
// dump, and a line break in it would start a line of its own in the log.
func toolOutput(s string) string {
	var lines []string
	for _, l := range strings.Split(cleanText(s, true), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " / ")
}

// shown is a value of the source as a message quotes it: one line, short.
func shown(s string) string {
	s = cleanText(s, false)
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

func cleanBundleText(b *apitypes.MigrationBundle) {
	b.Panel, b.Hostname = shown(b.Panel), shown(b.Hostname)
	if b.Family != "debian" && b.Family != "rhel" {
		b.Family = ""
	}
	for i, n := range b.Notes {
		b.Notes[i] = cleanText(n, true)
	}
}

// displayBundle readies the bundle a plan hands to people. mp migrate plan
// prints its sites, cron lines and login as they are, refused ones included —
// those are the very values that break a rule, a terminal escape among them.
// Every string loses its control characters (tabs stay: a cron command may
// hold one); the notes and the custom nginx directives keep their lines.
// Only the plan's copy is touched: the run fetches and checks its own.
func displayBundle(b *apitypes.MigrationBundle) {
	notes, nginx := b.Notes, b.SiteNginx
	b.Notes, b.SiteNginx = nil, nil
	cleanDisplay(reflect.ValueOf(b))
	for i, n := range notes {
		notes[i] = cleanText(n, true)
	}
	if nginx != nil {
		kept := make(map[string]string, len(nginx))
		for domain, text := range nginx {
			kept[shown(domain)] = cleanText(text, true)
		}
		nginx = kept
	}
	b.Notes, b.SiteNginx = notes, nginx
}

// cleanDisplay drops the control characters but tabs from every string
// reachable from v, in place.
func cleanDisplay(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(strings.Map(func(r rune) rune {
				if r != '\t' && unicode.IsControl(r) {
					return -1
				}
				return r
			}, v.String()))
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			cleanDisplay(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				cleanDisplay(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			cleanDisplay(v.Index(i))
		}
	case reflect.Map:
		// Entries of a map are not addressable: it is rebuilt, keys included.
		if v.IsNil() || !v.CanSet() {
			return
		}
		m := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			k.Set(it.Key())
			e.Set(it.Value())
			cleanDisplay(k)
			cleanDisplay(e)
			m.SetMapIndex(k, e)
		}
		v.Set(m)
	}
}

// fieldProblem is what a request rule says about one field.
type fieldProblem struct{ field, msg string }

func (p fieldProblem) String() string {
	if p.field == "" {
		return p.msg
	}
	return p.field + ": " + p.msg
}

func joinProblems(ps []fieldProblem) error {
	if len(ps) == 0 {
		return nil
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return errors.New(strings.Join(parts, "; "))
}

// vetRequestTypes are the request types vetBundle holds values to.
var vetRequestTypes = []any{
	apitypes.CreateUserRequest{}, apitypes.SiteRequest{}, apitypes.CronRequest{}, apitypes.AppRequest{},
	apitypes.ValkeyRequest{}, apitypes.MailboxRequest{}, apitypes.MailAliasRequest{},
}

// registerVetSchemas makes sure the schemas of vetRequestTypes are in the
// registry while the server starts. Their operations put them there, but a
// type no operation used would be added by the first plan, and huma's
// registry is a plain map that requests read concurrently.
func (s *Server) registerVetSchemas() {
	reg := s.api.OpenAPI().Components.Schemas
	for _, t := range vetRequestTypes {
		reg.Schema(reflect.TypeOf(t), true, "")
	}
}

// requestProblems checks v against the OpenAPI schema of its type the way
// huma checks a request body: the enum, pattern, length and range rules a
// person meets in the Web UI, mp and the TUI. The schema is the one the
// operations registered, so a rule changed there applies here as well.
func (s *Server) requestProblems(v any) []fieldProblem {
	reg := s.api.OpenAPI().Components.Schemas
	schema := reg.Schema(reflect.TypeOf(v), true, "")
	raw, err := json.Marshal(v)
	if err != nil {
		return []fieldProblem{{msg: err.Error()}}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []fieldProblem{{msg: err.Error()}}
	}
	res := &huma.ValidateResult{}
	huma.Validate(reg, schema, huma.NewPathBuffer([]byte{}, 0), huma.ModeWriteToServer, doc, res)
	out := make([]fieldProblem, 0, len(res.Errors))
	for _, e := range res.Errors {
		var d *huma.ErrorDetail
		if errors.As(e, &d) {
			out = append(out, fieldProblem{field: d.Location, msg: d.Message})
			continue
		}
		out = append(out, fieldProblem{msg: e.Error()})
	}
	return out
}

// unixAccountExists answers whether this host already has a unix user or
// group of the name. A variable, so tests do not depend on the machine they
// run on.
var unixAccountExists = lookupUnixName

// lookupUnixName looks for a user or a group of the name: an existing user
// would be adopted, and useradd -U cannot make one whose group name is taken.
func lookupUnixName(name string) bool {
	if _, err := user.Lookup(name); err == nil {
		return true
	}
	_, err := user.LookupGroup(name)
	return err == nil
}

// serviceLogins are the system accounts and groups the panel creates or takes
// over when it installs a service, or that its own services run as. One that
// does not exist yet is no less taken: an account imported under the name
// would be adopted later by the installer of nginx (the web group with it),
// of mail (every Maildir belongs to vmail, the DKIM keys to opendkim) or of
// MySQL, with the password hash its source chose.
func (s *Server) serviceLogins() map[string]bool {
	web := s.profile.Web()
	out := map[string]bool{
		"root": true, vmailUser: true, "postfix": true, "opendkim": true, "dovecot": true, "dovenull": true,
		"mysql": true, "nginx": true, "apache": true, "www-data": true, "memcache": true, "memcached": true,
		"sphinx": true, "sphinxsearch": true, "manticore": true, "redis": true, "valkey": true, "nobody": true,
		web.NginxUser: true, web.ApacheUser: true, sftpGroup: true,
		s.cfg.ServiceUser: true, s.cfg.ServiceGroup: true, s.cfg.WebGroup: true, s.cfg.CLIGroup: true,
	}
	delete(out, "")
	return out
}

// ------------------------------------------------------------ account ----

func (s *Server) vetAccount(ctx context.Context, v *migrateVet, req apitypes.MigrationSourceRequest, b *apitypes.MigrationBundle, login string) {
	u := b.User
	refuse := func(kind, text string) {
		v.account = true
		v.conflict(kind, shown(login), text)
	}
	loginOK := true
	for _, p := range s.requestProblems(apitypes.CreateUserRequest{Login: login, Email: u.Email, Role: store.RoleUser, Shell: u.Shell, QuotaMB: u.QuotaMB}) {
		if p.field == "login" {
			loginOK = false
			v.account = true
			v.conflicts = append(v.conflicts, issue("user", login, "the login does not suit a unix account: "+p.msg, "take it under another login: --as <login>"))
			continue
		}
		refuse("account", p.String())
	}
	// A MonoPanel source is asked for one account by the scope; one that
	// answers with another hands over something nobody asked for.
	if req.Panel == "" || req.Panel == "monopanel" {
		if want := scopeLogin(req.Scope); want != "" && u.Login != want {
			refuse("account", fmt.Sprintf("the source answered with the account %q instead of the requested %s", shown(u.Login), want))
		}
	}
	// provisionUser adopts a unix account that already exists: a login of a
	// system user (mysql, vmail, www-data) would hand its shell and password
	// to whoever controls the source. The source's grant command names the
	// login, so a service account that is not there yet is refused as well.
	if loginOK {
		_, err := s.db.GetUserByLogin(ctx, login)
		switch {
		case !errors.Is(err, store.ErrNotFound):
		case s.serviceLogins()[login]:
			v.account = true
			v.conflicts = append(v.conflicts, issue("user", login, "the name of a system account or group the panel's services use: the import would hand it to the source", "take it under another login: --as <login>"))
		case panelActors[login]:
			// The panel signs its own jobs and audit entries so: the
			// account would see them as its own.
			v.account = true
			v.conflicts = append(v.conflicts, issue("user", login, "a name the panel signs its own jobs and audit entries with", "take it under another login: --as <login>"))
		case unixAccountExists(login):
			v.account = true
			v.conflicts = append(v.conflicts, issue("user", login, "a unix user or group with this name already exists on this server and is not a panel account: the import would take it over", "take it under another login: --as <login>"))
		}
	}
	if u.Shell {
		v.warn("user", shown(login), "the account arrives with SSH shell access, as it has on the source", "keep it to SFTP: mp user set "+shown(login)+" --shell=false")
	}
	if sec := b.Secrets; sec != nil {
		if sec.PanelPassword != "" {
			if err := checkPanelHash(sec.PanelPassword); err != nil {
				v.refuse(secretPanel, "account", login, "panel password hash: "+err.Error())
			}
		}
		if sec.UnixShadow != "" {
			if err := checkShadowHash(sec.UnixShadow); err != nil {
				v.refuse(secretUnix, "account", login, "unix password hash: "+err.Error())
			}
		}
	}
}

var (
	// The panel's own argon2id strings (auth.HashPassword), with parameters a
	// sign-in can afford: VerifyPassword spends what the hash asks for, and a
	// hash asking for gigabytes would take the panel down at every attempt.
	panelHashRe = regexp.MustCompile(`^\$argon2id\$v=19\$m=([0-9]{1,8}),t=([0-9]{1,4}),p=([0-9]{1,3})\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)
	// crypt(3) hashes of /etc/shadow, as chpasswd writes them on the systems
	// the panel runs on: yescrypt, gost-yescrypt, SHA-512, SHA-256, bcrypt.
	// Nothing with a colon or a line break, which would add a line to what
	// chpasswd -e reads.
	shadowHashRe = regexp.MustCompile(`^\$(?:y|gy|6|5|2[aby])\$[./0-9A-Za-z$=,]{8,250}$`)
	// What dovecot gets from the panel (hashMailPassword): {BLF-CRYPT} and a
	// bcrypt hash. A colon or a line break in it would add a field or an
	// entry for somebody else to the passwd-file.
	mailHashRe = regexp.MustCompile(`^\{BLF-CRYPT\}\$2[aby]\$([0-9]{2})\$[./A-Za-z0-9]{53}$`)
)

func checkPanelHash(h string) error {
	m := panelHashRe.FindStringSubmatch(h)
	if m == nil {
		return errors.New("not an argon2id hash of the panel")
	}
	mem, _ := strconv.Atoi(m[1])
	iter, _ := strconv.Atoi(m[2])
	par, _ := strconv.Atoi(m[3])
	salt, serr := base64.RawStdEncoding.DecodeString(m[4])
	key, kerr := base64.RawStdEncoding.DecodeString(m[5])
	switch {
	case mem < 8*1024 || mem > 64*1024 || iter < 1 || iter > 10 || par < 1 || par > 8:
		return fmt.Errorf("argon2id parameters m=%d,t=%d,p=%d are outside what the panel uses", mem, iter, par)
	case serr != nil || len(salt) < 8 || len(salt) > 64, kerr != nil || len(key) < 16 || len(key) > 64:
		return errors.New("malformed argon2id salt or key")
	}
	return nil
}

func checkShadowHash(h string) error {
	if !shadowHashRe.MatchString(h) {
		return errors.New("not a crypt(3) hash the panel takes (yescrypt, SHA-512, SHA-256, bcrypt)")
	}
	return shadowCost(h)
}

// Costs of a shadow hash a sign-in can afford. sshd verifies the password of
// every attempt, by anyone who knows the login, with what the hash asks for:
// yescrypt's memory, SHA-crypt's rounds, bcrypt's cost. The source writes the
// hash, so the parameters stay near the defaults: 64 MiB like the panel's own
// argon2id, a million rounds (the default is 5000), bcrypt cost 13.
const (
	shadowMaxMemory = 64 << 20
	shadowMaxRounds = 1_000_000
	shadowMaxBcrypt = 13
)

// cryptAlphabet is the base-64 alphabet of crypt(3); yescrypt encodes its
// parameters with it.
const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// shadowCost reads the cost parameters of a hash shadowHashRe let through.
func shadowCost(h string) error {
	scheme, rest, _ := strings.Cut(h[1:], "$")
	params, _, _ := strings.Cut(rest, "$")
	switch scheme {
	case "y", "gy":
		// libxcrypt writes the flavour (j), log2 N and r, one character each
		// (N=4096, r=32 is "j9T"); memory is 128·r·N bytes.
		if len(params) != 3 || params[0] != 'j' {
			return errors.New("yescrypt parameters other than the ones chpasswd writes")
		}
		logN, r := strings.IndexByte(cryptAlphabet[:48], params[1])+1, strings.IndexByte(cryptAlphabet[:48], params[2])+1
		if logN < 1 || r < 1 || logN > 32 || int64(128*r)<<logN > shadowMaxMemory {
			return fmt.Errorf("yescrypt parameters %q ask for more memory than a sign-in can afford", params)
		}
	case "5", "6":
		if v, ok := strings.CutPrefix(params, "rounds="); ok {
			if n, err := strconv.Atoi(v); err != nil || n > shadowMaxRounds {
				return fmt.Errorf("SHA-crypt rounds %q: at most %d", v, shadowMaxRounds)
			}
		}
	default: // 2a, 2b, 2y
		if cost, err := strconv.Atoi(params); err != nil || cost < 4 || cost > shadowMaxBcrypt {
			return fmt.Errorf("bcrypt cost %q: 4 to %d", params, shadowMaxBcrypt)
		}
	}
	return nil
}

func checkMailHash(h string) error {
	m := mailHashRe.FindStringSubmatch(h)
	if m == nil {
		return errors.New("not a {BLF-CRYPT} hash of the panel")
	}
	// dovecot's auth workers spend what the cost asks for at every sign-in.
	if cost, _ := strconv.Atoi(m[1]); cost < 4 || cost > 13 {
		return fmt.Errorf("bcrypt cost %d is outside what the panel uses", cost)
	}
	return nil
}

// --------------------------------------------------------------- sites ----

// panelNames are the hostnames the panel answers to itself: the one it runs
// with, the one config.yaml names for its next start, its mail server's. The
// panel loads its own certificate from the certificate store by its name
// (certHolder.Reload), so a certificate of an account under that name — with a
// key its source knows — would be what the panel serves to its administrators.
func (s *Server) panelNames(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if n := s.panelCertName(); n != "" {
		out[n] = true
	}
	if file, err := s.panelConfigFile(); err == nil {
		if h := acme.NormalizeName(file.Web.Hostname); h != "" {
			out[h] = true
		}
	}
	if c := s.loadMailConfig(ctx); c.Hostname != "" {
		out[acme.NormalizeName(c.Hostname)] = true
	}
	return out
}

// vetSites checks every site the way sites-create does and returns the names
// the accepted sites bring, which their certificates may carry.
func (s *Server) vetSites(ctx context.Context, v *migrateVet, b *apitypes.MigrationBundle, ownerID int64) map[string]bool {
	names := map[string]bool{}
	taken := map[string]string{} // a name → the bundle site that has it
	own := s.panelNames(ctx)
	for _, site := range b.Sites {
		target := shown(site.Domain)
		if err := s.vetSite(site); err != nil {
			v.refuse(site, "site", target, err.Error())
			continue
		}
		mine := ""
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			if own[acme.NormalizeName(name)] {
				mine = acme.NormalizeName(name)
			}
		}
		if mine != "" {
			v.refuse(site, "site", target, mine+" is a hostname of this panel itself (the panel or its mail server)")
			continue
		}
		served := false
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			others, err := s.db.FindSitesByName(ctx, acme.NormalizeName(name))
			if err != nil || len(others) == 0 {
				continue
			}
			served = true
			v.refuseFix(site, "site", acme.NormalizeName(name), "the site is already served by this panel (owner "+others[0].Login+")", "delete it here or exclude it from the migration")
		}
		if served {
			continue
		}
		// validateNames is the sites-create rule for the names: canonical
		// form, a valid hostname each, no name of another site.
		aliases, err := s.validateNames(ctx, site.Domain, site.Aliases, 0, ownerID)
		if err != nil {
			v.refuse(site, "site", target, err.Error())
			continue
		}
		site.Aliases = aliases
		dup := ""
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			if other, ok := taken[name]; ok {
				dup = fmt.Sprintf("%s is also a name of the site %s in the same bundle", name, other)
				break
			}
		}
		if dup != "" {
			v.refuse(site, "site", target, dup)
			continue
		}
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			taken[name], names[name] = site.Domain, true
		}
	}
	return names
}

// vetSite holds one site to the sites-create rules and puts this host's
// addresses in place of the source's: the old server's IPs mean nothing here,
// and nginx would not start with them.
func (s *Server) vetSite(site *store.Site) error {
	if site.Domain != acme.NormalizeName(site.Domain) {
		return fmt.Errorf("domain %q is not in the form the panel keeps names in (lower case, no trailing dot)", shown(site.Domain))
	}
	if err := acme.ValidateName(site.Domain); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	if site.PHPVersion != "" && !slices.Contains(osprofileVersions(), site.PHPVersion) {
		return fmt.Errorf("php_version %q is not a PHP branch the panel knows", shown(site.PHPVersion))
	}
	if site.Mode == store.ModeProxy {
		site.PHPVersion = ""
	}
	// sites-create checks the backend in the proxy mode only, but a backend
	// kept with another mode is rendered the moment the mode changes.
	if (site.Mode == store.ModeProxy || site.Backend != "") && !backendRe.MatchString(site.Backend) {
		return fmt.Errorf("backend %q: the proxy mode needs a local backend like http://127.0.0.1:3000 or http://unix:/run/app.sock:", shown(site.Backend))
	}
	site.Docroot = strings.Trim(site.Docroot, "/")
	if site.Docroot != "" && (!docrootRe.MatchString(site.Docroot) || strings.Contains(site.Docroot, "..")) {
		return fmt.Errorf("docroot %q: invalid docroot", shown(site.Docroot))
	}
	if err := validateIni(site.PHPIni); err != nil {
		return err
	}
	var err error
	if site.AllowFrom, err = normalizeAllowFrom(site.AllowFrom); err != nil {
		return err
	}
	if site.Preset, err = normalizePreset(site.Preset); err != nil {
		return err
	}
	if site.SessionStore != "" && site.SessionStore != store.SessionStoreValkey {
		return fmt.Errorf("session_store %q: files or valkey", shown(site.SessionStore))
	}
	rate := site.RateLimit
	if err := joinProblems(s.requestProblems(apitypes.SiteRequest{
		Domain: site.Domain, Aliases: site.Aliases, Mode: site.Mode, Backend: site.Backend, PHPVersion: site.PHPVersion, Docroot: site.Docroot,
		SSL: site.SSL, RedirectWWW: site.RedirectWWW, FPMPM: site.FPMPM, FPMMaxChildren: site.FPMMaxChildren, PHPIni: site.PHPIni,
		ClientMaxBody: site.ClientMaxBody, RateLimit: &rate, AllowFrom: site.AllowFrom, Preset: site.Preset,
	})); err != nil {
		return err
	}
	for _, f := range [][2]string{{"cms", site.CMS}, {"cms_version", site.CMSVersion}, {"cms_at", site.CMSAt}, {"cms_database", site.CMSDatabase}} {
		if err := unitValue(f[0], f[1]); err != nil {
			return err
		}
	}
	// IPv4 is chosen when the site is applied; IPv6 like sites-create does
	// for a site that wants it: the host's first global address, if any.
	site.IP = ""
	if site.IPv6 != "" {
		site.IPv6 = ""
		if v6 := s.hostIPv6s(); len(v6) > 0 {
			site.IPv6 = v6[0]
		}
	}
	return nil
}

// migratedNginxName is where the custom nginx directives of a site from a
// MonoPanel source wait for a person: next to custom.conf, but not *.conf,
// so the server block does not include them.
const migratedNginxName = customConfName + ".migrated"

// maxMigratedNginx caps the custom directives kept for review.
const maxMigratedNginx = 1 << 20

// vetCustomNginx says what happens to the custom nginx directives of the
// sites. Writing them is administrator-only (sites-nginx-set): they run with
// the web server's privileges — an access_log line alone writes a file
// anywhere as root. A source is not an administrator here, so the directives
// are kept for review instead of being applied.
func (s *Server) vetCustomNginx(v *migrateVet, b *apitypes.MigrationBundle) {
	for _, site := range b.Sites {
		custom := b.SiteNginx[site.Domain]
		if strings.TrimSpace(custom) == "" || v.why(site) != "" {
			continue
		}
		file := path.Join(s.profile.Web().NginxConfDir, "monopanel", "sites", site.Domain+".d", migratedNginxName)
		if len(custom) > maxMigratedNginx {
			v.warn("site", site.Domain, fmt.Sprintf("custom nginx directives of %d bytes are not moved: more than the panel keeps for review", len(custom)), "")
			continue
		}
		v.warn("site", site.Domain, "custom nginx directives are not applied: they run with the web server's privileges, so they are saved as "+file+" for review", "after checking them: mp site nginx "+site.Domain+" --set "+file)
	}
}

// ----------------------------------------------------------- databases ----

var (
	// The pattern the API addresses databases by (databases-delete, -password,
	// migrate-dump): a database it cannot name could not be managed after the
	// move.
	migrateDBNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	// The account names the panel makes (<login>_<name>, at most 32
	// characters, the MySQL limit).
	migrateDBUserRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// The host of a MySQL account: a name, an address, a pattern or a
	// netmask. Nothing that ends the quoted literal it goes into.
	migrateDBHostRe = regexp.MustCompile(`^[A-Za-z0-9._%:/-]{1,255}$`)
	// Character set and collation names (utf8mb4, utf8mb4_0900_ai_ci).
	mysqlCharsetRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// System schemas and accounts of MySQL: nothing of an account may land in
// them.
var (
	mysqlSystemDBs   = map[string]bool{"mysql": true, "sys": true, "information_schema": true, "performance_schema": true}
	mysqlSystemUsers = map[string]bool{"root": true, "debian-sys-maint": true, "mysqlxsys": true}
	// The accounts a site of this host uses: over the socket (localhost) or
	// the loopback. The panel offers nothing else (databases-create refuses
	// remote access).
	localDBHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
)

// importAccountPrefix names the account a database that arrives without one
// of its own is loaded as, and keeps (keepImportAccount).
const importAccountPrefix = "mpimp_"

// grantDBEscaper keeps a database name literal in a database-level GRANT.
// There _ and % are wildcards: GRANT … ON `shop_db`.* also opens shopXdb, and
// a name of underscores alone — a valid name, and the source picks it — opens
// every database of that length: another account's, or `mysql` itself (five
// characters), whose privilege tables make the account MySQL's root. Escaped,
// the grant names this one database, with partial_revokes on or off.
var grantDBEscaper = strings.NewReplacer("\\", "\\\\", "_", "\\_", "%", "\\%", "`", "``")

// grantDB is a database as the ON part of a GRANT names it: quoted, and
// matching itself only.
func grantDB(name string) string { return "`" + grantDBEscaper.Replace(name) + "`" }

// dbPrefixOwner names the account here under whose prefix a database or
// MySQL account name of the source sits. databases-create names an account's
// databases and their accounts <login>_<name> and goes on with one of that
// name that is already there (CREATE … IF NOT EXISTS): the source's database
// under alex_ would become what alex gets for mp db create — with the
// source's tables and triggers in it, and its record and accounts the
// source's, whose owner can reset the password. Only the account taking the
// bundle may bring names under its own prefix. An honest move meets this too:
// an account taken under another login (--as) because its own is in use here
// keeps the old prefix on its databases, and the two accounts would share
// them.
func dbPrefixOwner(name, self string, logins []string) string {
	for _, l := range logins {
		if l != self && strings.HasPrefix(name, l+"_") {
			return l
		}
	}
	return ""
}

func (s *Server) vetDatabases(ctx context.Context, v *migrateVet, b *apitypes.MigrationBundle, login string) {
	if len(b.Databases) == 0 {
		return
	}
	haveDB, haveUser := s.mysqlInventory(ctx)
	// The accounts that own databases here (databases-create refuses
	// administrators as owners).
	var logins []string
	if users, err := s.db.ListUsers(ctx); err == nil {
		for _, u := range users {
			if u.Role == store.RoleUser {
				logins = append(logins, u.Login)
			}
		}
	}
	seen := map[string]bool{}
	accounts := map[string]bool{} // name@host the bundle creates
	var native []string           // accounts the source says use mysql_native_password
	for _, d := range b.Databases {
		target := shown(d.Name)
		_, lookupErr := s.db.GetDatabaseByName(ctx, d.Name)
		switch {
		case !migrateDBNameRe.MatchString(d.Name):
			v.refuse(d, "database", target, "the name must match "+migrateDBNameRe.String()+" for the panel to manage the database")
		case mysqlSystemDBs[d.Name]:
			v.refuse(d, "database", target, "a system schema of MySQL")
		case seen[d.Name]:
			v.refuse(d, "database", target, "the bundle carries this database twice")
		case lookupErr == nil:
			v.refuseFix(d, "database", target, "a database with this name already exists", "rename or delete it here")
		case haveDB[d.Name]:
			v.refuseFix(d, "database", target, "the database server here already has a database with this name", "rename or delete it here")
		case dbPrefixOwner(d.Name, login, logins) != "":
			other := dbPrefixOwner(d.Name, login, logins)
			v.refuseFix(d, "database", target, fmt.Sprintf("names starting with %s_ are the databases of the account %s here, and its mp db create would take this one over", other, other), "rename the database on the source")
		case d.Charset != "" && !mysqlCharsetRe.MatchString(d.Charset), d.Collation != "" && !mysqlCharsetRe.MatchString(d.Collation):
			v.refuse(d, "database", target, fmt.Sprintf("character set %q / collation %q are not MySQL names", shown(d.Charset), shown(d.Collation)))
		}
		if v.why(d) != "" {
			continue
		}
		seen[d.Name] = true
		d.Users = nonNil(v, d.Users, "database", "database account")
		for _, acc := range d.Users {
			key := acc.Name + "@" + acc.Host
			who := shown(key)
			switch {
			case !migrateDBUserRe.MatchString(acc.Name):
				v.refuse(acc, "database", d.Name, fmt.Sprintf("account %s: the name must match %s", who, migrateDBUserRe))
			case mysqlSystemUsers[acc.Name] || strings.HasPrefix(acc.Name, importAccountPrefix):
				v.refuse(acc, "database", d.Name, "account "+who+": a system account of MySQL or the panel")
			case !migrateDBHostRe.MatchString(acc.Host):
				v.refuse(acc, "database", d.Name, fmt.Sprintf("account %s: the host must match %s", who, migrateDBHostRe))
			case !localDBHosts[acc.Host]:
				v.skip(acc, "database", d.Name, "account "+who+" is not moved: the panel keeps database accounts on this host only (localhost)", "")
			case haveUser[key] && !accounts[key]:
				v.refuse(acc, "database", d.Name, "account "+who+": the database server here already has this account")
			case dbPrefixOwner(acc.Name, login, logins) != "":
				other := dbPrefixOwner(acc.Name, login, logins)
				v.refuse(acc, "database", d.Name, fmt.Sprintf("account %s: names starting with %s_ are the database accounts of the account %s here, and its mp db create would take this one over", who, other, other))
			}
			if v.why(acc) != "" {
				continue
			}
			accounts[key] = true
			if acc.AuthPlugin == "mysql_native_password" {
				native = append(native, key)
			}
			if b.Secrets == nil {
				continue
			}
			create := b.Secrets.DBUsers[key]
			if create == "" {
				v.skip(acc, "database", d.Name, "account "+who+" is not moved: the source gave no authentication string for it", "a database left without accounts gets one of its own: mp db passwd "+d.Name+" sets its password")
				continue
			}
			a, err := parseCreateUser(create)
			if err != nil {
				v.refuse(secretDBUser(key), "database", d.Name, "account "+who+": "+err.Error())
				v.refused[acc] = err.Error()
				continue
			}
			v.auth[key] = a
		}
	}
	// Such a hash needs the plugin, which MySQL 8.4 keeps off: switching it on
	// restarts the server for every account on it.
	if inst, err := s.dbInstance(ctx); err == nil && !inst.NativePassword && len(native) > 0 {
		v.warn("database", "", "database accounts with mysql_native_password hashes are coming ("+strings.Join(native, ", ")+"): the plugin is switched on here during the move, which restarts MySQL once", "")
	}
}

// mysqlInventory lists the databases and accounts the server here already
// has: the plan names a collision before the run fails on it. Best effort —
// without a database server there is nothing to collide with.
func (s *Server) mysqlInventory(ctx context.Context) (dbs, accounts map[string]bool) {
	dbs, accounts = map[string]bool{}, map[string]bool{}
	if _, err := s.dbInstance(ctx); err != nil {
		return dbs, accounts
	}
	out, err := s.mysqlExec(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA;\nSELECT CONCAT('@', user, '@', host) FROM mysql.user;\n")
	if err != nil {
		return dbs, accounts
	}
	for _, l := range strings.Split(out, "\n") {
		switch l = strings.TrimSpace(l); {
		case l == "":
		case strings.HasPrefix(l, "@"):
			accounts[l[1:]] = true
		default:
			dbs[l] = true
		}
	}
	return dbs, accounts
}

// dbAuth is how a MySQL account authenticates, as the import recreates it:
// the plugin and its stored hash, or a password known in plain text (the
// BitrixVM adapter reads it from the site's settings). The CREATE USER the
// source sends is never run as it is — as root, it could be anything — but
// taken apart into these and built again.
type dbAuth struct {
	plugin string // mysql_native_password | caching_sha2_password; "" with plain
	hash   []byte
	plain  string
	locked bool
}

var (
	identifiedRe = regexp.MustCompile(`(?is)\bIDENTIFIED\s+(.*)$`)
	authWithRe   = regexp.MustCompile(`(?is)^WITH\s+['"` + "`" + `]?([a-z0-9_]+)['"` + "`" + `]?\s+(AS|BY)\s+(.*)$`)
	authByPassRe = regexp.MustCompile(`(?is)^BY\s+PASSWORD\s+(.*)$`)
	authViaRe    = regexp.MustCompile(`(?is)^VIA\s+['"]?mysql_native_password['"]?\s+USING\s+(.*)$`)
	authByRe     = regexp.MustCompile(`(?is)^BY\s+(.*)$`)
	nativeHashRe = regexp.MustCompile(`^\*[0-9A-F]{40}$`)
	accountLock  = regexp.MustCompile(`(?i)\bACCOUNT\s+LOCK\b`)
)

// parseCreateUser takes the authentication out of a CREATE USER: SHOW CREATE
// USER of MySQL 5.7/8 (the hash quoted or as a hex literal), MariaDB's
// IDENTIFIED BY PASSWORD and VIA … USING, and the IDENTIFIED BY '<plain>' the
// BitrixVM adapter builds. Only the plugins the panel itself sets up pass.
func parseCreateUser(stmt string) (dbAuth, error) {
	m := identifiedRe.FindStringSubmatch(stmt)
	if m == nil {
		return dbAuth{}, errors.New("the authentication string has no IDENTIFIED clause")
	}
	clause := strings.TrimSpace(m[1])
	var a dbAuth
	var lit string
	isHash := true
	switch {
	case authWithRe.MatchString(clause):
		w := authWithRe.FindStringSubmatch(clause)
		a.plugin, lit = strings.ToLower(w[1]), w[3]
		isHash = strings.EqualFold(w[2], "AS")
	case authByPassRe.MatchString(clause):
		a.plugin, lit = "mysql_native_password", authByPassRe.FindStringSubmatch(clause)[1]
	case authViaRe.MatchString(clause):
		a.plugin, lit = "mysql_native_password", authViaRe.FindStringSubmatch(clause)[1]
	case authByRe.MatchString(clause):
		lit, isHash = authByRe.FindStringSubmatch(clause)[1], false
	default:
		return dbAuth{}, errors.New("unsupported authentication in the CREATE USER statement")
	}
	val, rest, err := readSQLValue(lit)
	if err != nil {
		return dbAuth{}, err
	}
	a.locked = accountLock.MatchString(rest)
	if !isHash {
		if a.plugin != "" && a.plugin != "mysql_native_password" && a.plugin != "caching_sha2_password" {
			return dbAuth{}, fmt.Errorf("unsupported authentication plugin %q", shown(a.plugin))
		}
		a.plugin = ""
		if len(val) == 0 || len(val) > 1024 || strings.ContainsFunc(string(val), unicode.IsControl) {
			return dbAuth{}, errors.New("the password is empty, too long or has control characters")
		}
		a.plain = string(val)
		return a, nil
	}
	switch a.plugin {
	case "mysql_native_password":
		h := strings.ToUpper(string(val))
		if !nativeHashRe.MatchString(h) {
			return dbAuth{}, errors.New("not a mysql_native_password hash")
		}
		a.hash = []byte(h)
	case "caching_sha2_password":
		if !sha2HashOK(val) {
			return dbAuth{}, errors.New("not a caching_sha2_password hash")
		}
		a.hash = val
	default:
		return dbAuth{}, fmt.Errorf("unsupported authentication plugin %q (the panel sets up mysql_native_password and caching_sha2_password)", shown(a.plugin))
	}
	return a, nil
}

// sha2HashOK checks the stored form of caching_sha2_password: $A$, three hex
// digits of rounds, $, a 20-byte salt and 43 characters of digest.
func sha2HashOK(h []byte) bool {
	if len(h) != 70 || string(h[:3]) != "$A$" || h[6] != '$' {
		return false
	}
	if _, err := strconv.ParseUint(string(h[3:6]), 16, 16); err != nil {
		return false
	}
	for _, c := range h[27:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}

// readSQLValue reads a quoted string or a hex literal at the start of s and
// returns its bytes and what follows it.
func readSQLValue(s string) ([]byte, string, error) {
	s = strings.TrimLeft(s, " \t")
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		end := 2
		for end < len(s) && strings.IndexByte("0123456789abcdefABCDEF", s[end]) >= 0 {
			end++
		}
		b, err := hex.DecodeString(s[2:end])
		if err != nil {
			return nil, "", errors.New("malformed hex literal in the authentication string")
		}
		return b, s[end:], nil
	}
	if s == "" || (s[0] != '\'' && s[0] != '"') {
		return nil, "", errors.New("the authentication string is not a quoted literal")
	}
	q := s[0]
	var out []byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case '0':
				out = append(out, 0)
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'Z':
				out = append(out, 26)
			default:
				out = append(out, s[i])
			}
		case c == q && i+1 < len(s) && s[i+1] == q:
			out = append(out, q)
			i++
		case c == q:
			return out, s[i+1:], nil
		default:
			out = append(out, c)
		}
	}
	return nil, "", errors.New("unterminated literal in the authentication string")
}

// effectivePlugin is the plugin the account ends up with: a plain password
// takes the one PHP below 7.4 can use when such a site comes along.
func (a dbAuth) effectivePlugin(legacy bool) string {
	switch {
	case a.plain == "":
		return a.plugin
	case legacy:
		return "mysql_native_password"
	}
	return "caching_sha2_password"
}

// sqlLiteralEscaper quotes a value of the source inside '…' of a statement
// run as root. A quote is doubled, not backslashed: with NO_BACKSLASH_ESCAPES
// in the server's sql_mode (mp db config set sql_mode=…) \' ends the string
// and the rest of the password would run as SQL. A backslash is still
// doubled for the default mode; under that sql_mode the value then differs
// from the source's, which costs a password, not the server.
var sqlLiteralEscaper = strings.NewReplacer(`\`, `\\`, `'`, `''`)

// clause renders the IDENTIFIED part of the statement the import runs. The
// name and host are vetted separately; a stored caching_sha2_password hash
// travels as a hex literal, so no byte of its binary salt can end the string.
func (a dbAuth) clause(legacy bool) string {
	var c string
	switch {
	case a.plain != "":
		c = fmt.Sprintf("IDENTIFIED WITH %s BY '%s'", a.effectivePlugin(legacy), sqlLiteralEscaper.Replace(a.plain))
	case a.plugin == "mysql_native_password":
		c = fmt.Sprintf("IDENTIFIED WITH mysql_native_password AS '%s'", a.hash)
	default:
		c = fmt.Sprintf("IDENTIFIED WITH caching_sha2_password AS 0x%X", a.hash)
	}
	if a.locked {
		c += " ACCOUNT LOCK"
	}
	return c
}

// hashedClause is clause for a password known in plain text, given as the
// hash the server stores instead (mysql_hash.go): the server's
// validate_password judges IDENTIFIED BY only, and refuses a password the
// source took (error 1819). ok is false when there is no plain password.
func (a dbAuth) hashedClause(legacy bool) (string, bool, error) {
	if a.plain == "" {
		return "", false, nil
	}
	var c string
	if a.effectivePlugin(legacy) == "mysql_native_password" {
		c = fmt.Sprintf("IDENTIFIED WITH mysql_native_password AS '%s'", mysqlNativeHash(a.plain))
	} else {
		h, err := mysqlCachingSHA2Hash(a.plain)
		if err != nil {
			return "", false, err
		}
		c = fmt.Sprintf("IDENTIFIED WITH caching_sha2_password AS 0x%X", h)
	}
	if a.locked {
		c += " ACCOUNT LOCK"
	}
	return c, true, nil
}

// passwordPolicyRefused says whether MySQL refused a password by its
// validate_password policy.
func passwordPolicyRefused(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "ERROR 1819") || strings.Contains(err.Error(), "does not satisfy the current policy requirements"))
}

// ------------------------------------------------------ cron, apps, valkey ----

// migrateRebase maps the home of an account on a MonoPanel source to its home
// here when the two differ (the account arrives under another login, or the
// source keeps homes elsewhere): app paths and cron commands then point at the
// moved files, and the app paths pass the rule that keeps them inside the
// home. The foreign adapters rewrite their own paths.
func (s *Server) migrateRebase(req apitypes.MigrationSourceRequest, b *apitypes.MigrationBundle, login string) [][2]string {
	if req.Panel != "" && req.Panel != "monopanel" {
		return nil
	}
	old := b.User.Home
	if old == "" {
		old = path.Join(s.cfg.WWWRoot, b.User.Login)
	}
	old = path.Clean(old)
	home := path.Join(s.cfg.WWWRoot, login)
	if !path.IsAbs(old) || strings.Count(old, "/") < 2 || old == home || strings.ContainsFunc(old, unicode.IsControl) {
		return nil
	}
	return [][2]string{{old + "/", home + "/"}}
}

// vetCron holds the jobs to the cron-create rules: validateCron for the
// schedule and the command, one-line values (a line break in a command or a
// comment adds a line to the crontab), the lengths of CronRequest.
func (s *Server) vetCron(v *migrateVet, b *apitypes.MigrationBundle) {
	for _, j := range b.Cron {
		j.Command = rewritePaths(j.Command, v.rebase)
		target := shown(j.Schedule + " " + j.Command)
		err := validateCron(j.Schedule, j.Command)
		if err == nil {
			err = unitValue("command", j.Command)
		}
		if err == nil {
			err = unitValue("comment", j.Comment)
		}
		if err == nil {
			enabled := j.Enabled
			err = joinProblems(s.requestProblems(apitypes.CronRequest{Schedule: j.Schedule, Command: j.Command, Comment: j.Comment, Enabled: &enabled}))
		}
		if err != nil {
			v.refuse(j, "cron", target, err.Error())
		}
	}
}

func (s *Server) vetApps(v *migrateVet, b *apitypes.MigrationBundle, home string) {
	seen := map[string]bool{}
	for _, a := range b.Apps {
		a.Command, a.WorkDir, a.EnvFile = rewritePaths(a.Command, v.rebase), rewritePaths(a.WorkDir, v.rebase), rewritePaths(a.EnvFile, v.rebase)
		for i, e := range a.Env {
			a.Env[i] = rewritePaths(e, v.rebase)
		}
		target := shown(a.Name)
		if err := s.vetApp(home, a); err != nil {
			v.refuse(a, "app", target, err.Error())
			continue
		}
		if seen[a.Name] {
			v.refuse(a, "app", target, "the bundle carries this app service twice")
			continue
		}
		seen[a.Name] = true
	}
}

// vetApp is validateApp without the look at the files, which are still on the
// source when the plan runs: the AppRequest rules, values that cannot reshape
// the unit file (unitValue — ExecStartPre=+… on a line of its own would run as
// root), an absolute executable, the working directory and the environment
// file inside the home (systemd reads EnvironmentFile as root and hands its
// lines to the app), KEY=value pairs.
func (s *Server) vetApp(home string, a *store.App) error {
	if err := joinProblems(s.requestProblems(apitypes.AppRequest{Name: a.Name, Description: a.Description, Command: a.Command, WorkDir: a.WorkDir, EnvFile: a.EnvFile, Env: a.Env, Restart: a.Restart})); err != nil {
		return err
	}
	if !appNameRe.MatchString(a.Name) {
		return errors.New("name must match " + appNameRe.String())
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" || len(a.Command) > 1000 || strings.ContainsAny(a.Command, "\n\r") {
		return errors.New("command must be one line up to 1000 characters")
	}
	for _, f := range [][2]string{{"description", a.Description}, {"command", a.Command}, {"workdir", a.WorkDir}, {"env_file", a.EnvFile}} {
		if err := unitValue(f[0], f[1]); err != nil {
			return err
		}
	}
	if exe := strings.Fields(a.Command)[0]; !path.IsAbs(exe) {
		return errors.New("command must start with an absolute path")
	}
	if a.WorkDir == "" {
		a.WorkDir = path.Join(home, "data")
	}
	a.WorkDir = path.Clean(a.WorkDir)
	if !path.IsAbs(a.WorkDir) || !insideHome(home, a.WorkDir) {
		return errors.New("workdir must be an absolute path inside " + home)
	}
	if a.EnvFile != "" {
		a.EnvFile = path.Clean(a.EnvFile)
		if !path.IsAbs(a.EnvFile) || !insideHome(home, a.EnvFile) {
			return errors.New("env_file must be an absolute path inside " + home)
		}
	}
	for _, e := range a.Env {
		if !envPairRe.MatchString(e) {
			return fmt.Errorf("env: %q must look like KEY=value (no double quotes)", shown(e))
		}
		if err := unitValue("env", e); err != nil {
			return err
		}
	}
	if a.Restart == "" {
		a.Restart = "always"
	}
	return nil
}

// vetValkey holds the instances to valkey-put: a purpose of the two the
// panel runs (it names the unit and its directories) and memory in range.
func (s *Server) vetValkey(v *migrateVet, b *apitypes.MigrationBundle) {
	seen := map[string]bool{}
	for _, inst := range b.Valkey {
		target := shown(inst.Purpose)
		switch {
		case inst.Purpose != store.ValkeyCache && inst.Purpose != store.ValkeySessions:
			v.refuse(inst, "valkey", target, "the purpose must be cache or sessions")
			continue
		case seen[inst.Purpose]:
			v.refuse(inst, "valkey", target, "the bundle carries this instance twice")
			continue
		}
		seen[inst.Purpose] = true
		if inst.MemoryMB == 0 {
			inst.MemoryMB = valkeyDefaultMemory[inst.Purpose]
		}
		if err := joinProblems(s.requestProblems(apitypes.ValkeyRequest{MemoryMB: inst.MemoryMB})); err != nil {
			v.refuse(inst, "valkey", target, err.Error())
		}
	}
}

// ---------------------------------------------------------------- mail ----

// A DKIM selector is one DNS label, and it names the key file under
// /etc/opendkim/keys/<domain>/ as well: "../" in it would put the key — text
// of the source's choosing — anywhere the agent writes.
var dkimSelectorRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (s *Server) vetMail(ctx context.Context, v *migrateVet, b *apitypes.MigrationBundle, ownerID int64) {
	own := s.panelNames(ctx)
	byID := map[int64]*store.MailDomain{}
	seen := map[string]bool{}
	for _, d := range b.MailDomains {
		target := shown(d.Name)
		_, lookupErr := s.db.GetMailDomain(ctx, d.Name)
		switch {
		case d.Name != strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Name), ".")) || !mailDomainRe.MatchString(d.Name):
			v.refuse(d, "mail", target, "domain must look like example.com")
		case seen[d.Name]:
			v.refuse(d, "mail", target, "the bundle carries this domain twice")
		case lookupErr == nil:
			v.refuseFix(d, "mail", target, "the mail domain is already served here", "delete it here or exclude it from the migration")
		case byID[d.ID] != nil:
			v.refuse(d, "mail", target, "two domains of the bundle share an id, so their mailboxes cannot be told apart")
		case d.SendOnly && d.Lenient:
			v.refuse(d, "mail", target, "a send-only domain receives no mail here, so it cannot be lenient")
		case d.DKIMSelector != "" && !dkimSelectorRe.MatchString(d.DKIMSelector):
			v.refuse(d, "mail", target, fmt.Sprintf("DKIM selector %q must be one DNS label", shown(d.DKIMSelector)))
		case d.DKIMPublic != "" && dkimPublicOK(d.DKIMPublic) != nil:
			v.refuse(d, "mail", target, "DKIM public key: "+dkimPublicOK(d.DKIMPublic).Error())
		case own[d.Name]:
			v.refuse(d, "mail", target, "a hostname of this panel itself (the panel or its mail server)")
		}
		if v.why(d) != "" {
			continue
		}
		// The server delivers mail for its domains itself: one under another
		// account's name would take mail this server sends there.
		if other, err := s.foreignDomain(ctx, ownerID, d.Name); err == nil && other != "" {
			v.refuse(d, "mail", target, "the domain is, or is under, "+other+", which belongs to another account here")
			continue
		}
		seen[d.Name], byID[d.ID] = true, d
		if b.Secrets == nil {
			continue
		}
		if key := b.Secrets.DKIM[d.Name]; key != "" {
			pub, err := dkimPublicKey(key)
			switch {
			case err != nil:
				v.refuse(secretDKIM(d.Name), "mail", d.Name, "DKIM key: "+err.Error())
			case d.DKIMSelector == "":
				v.refuse(secretDKIM(d.Name), "mail", d.Name, "DKIM key without a selector")
			case d.DKIMPublic != "" && d.DKIMPublic != pub:
				v.refuse(secretDKIM(d.Name), "mail", d.Name, "the DKIM public key does not match the private key")
			default:
				d.DKIMPublic = pub
			}
		}
	}
	addresses := map[string]bool{}
	for _, box := range b.Mailboxes {
		d := byID[box.DomainID]
		target := shown(box.Address)
		if d == nil {
			v.refuse(box, "mail", target, "the mailbox belongs to no accepted mail domain of the bundle")
			continue
		}
		if d.SendOnly {
			v.refuse(box, "mail", target, "the domain is send-only (another server receives its mail), so it has no mailboxes")
			continue
		}
		// The address is what postfix delivers to and dovecot signs in with:
		// it is built here from the local part and the domain it belongs
		// to, never taken as the source spells it.
		local, domain, err := splitAddress(box.LocalPart + "@" + d.Name)
		addr := local + "@" + domain
		switch {
		case err != nil:
			v.refuse(box, "mail", target, err.Error())
		case local == "@":
			v.refuse(box, "mail", target, "@domain is a catch-all; it can only be an alias")
		case local != box.LocalPart:
			v.refuse(box, "mail", target, fmt.Sprintf("local part %q is not in canonical form (lower case)", shown(box.LocalPart)))
		case box.Address != "" && box.Address != addr:
			v.refuse(box, "mail", target, "the address does not match its local part and domain ("+addr+")")
		case addresses[addr]:
			v.refuse(box, "mail", target, "the bundle carries this mailbox twice")
		}
		if v.why(box) != "" {
			continue
		}
		quota := box.QuotaMB
		if err := joinProblems(s.requestProblems(apitypes.MailboxRequest{Address: addr, Name: box.Name, QuotaMB: &quota})); err != nil {
			v.refuse(box, "mail", addr, err.Error())
			continue
		}
		if err := unitValue("name", box.Name); err != nil {
			v.refuse(box, "mail", addr, err.Error())
			continue
		}
		box.Address, addresses[addr] = addr, true
		if b.Secrets != nil {
			if h := b.Secrets.Mailboxes[addr]; h != "" {
				if err := checkMailHash(h); err != nil {
					v.refuse(secretMailbox(addr), "mail", addr, "password hash: "+err.Error())
				}
			}
		}
	}
	aliases := map[string]bool{}
	for _, a := range b.MailAliases {
		d := byID[a.DomainID]
		target := shown(a.Address)
		if d == nil {
			v.refuse(a, "mail", target, "the alias belongs to no accepted mail domain of the bundle")
			continue
		}
		if d.SendOnly {
			v.refuse(a, "mail", target, "the domain is send-only (another server receives its mail), so it has no aliases")
			continue
		}
		addr := a.Source + "@" + d.Name
		if a.Source == "@" {
			addr = "@" + d.Name
		}
		local, _, err := splitAddress(addr)
		dests := []string{}
		if err == nil {
			for _, to := range strings.Split(a.Destination, ",") {
				if to = strings.ToLower(strings.TrimSpace(to)); to == "" {
					continue
				}
				if _, _, derr := splitAddress(to); derr != nil {
					err = fmt.Errorf("recipient %s: %w", shown(to), derr)
					break
				}
				dests = append(dests, to)
			}
		}
		switch {
		case err != nil:
			v.refuse(a, "mail", target, err.Error())
		case local != a.Source:
			v.refuse(a, "mail", target, fmt.Sprintf("source %q is not in canonical form (lower case)", shown(a.Source)))
		case a.Address != "" && a.Address != addr:
			v.refuse(a, "mail", target, "the address does not match its source and domain ("+addr+")")
		case addresses[addr]:
			v.refuse(a, "mail", addr, "a mailbox of the bundle has the same address; the alias would intercept its mail")
		case aliases[addr]:
			v.refuse(a, "mail", addr, "the bundle carries this alias twice")
		}
		if v.why(a) != "" {
			continue
		}
		if err := joinProblems(s.requestProblems(apitypes.MailAliasRequest{Address: addr, Destinations: dests})); err != nil {
			v.refuse(a, "mail", addr, err.Error())
			continue
		}
		a.Address, a.Destination, aliases[addr] = addr, strings.Join(dests, ","), true
	}
}

// dkimPublicKey reads a DKIM private key — the PEM opendkim gets — and
// returns its public half the way the TXT record carries it.
func dkimPublicKey(key string) (string, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(key)))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("not a single PEM block")
	}
	var priv *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		priv = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("not an RSA key")
		}
		priv = rk
	default:
		return "", fmt.Errorf("unexpected PEM block %q", shown(block.Type))
	}
	if bits := priv.N.BitLen(); bits < 1024 || bits > 8192 {
		return "", fmt.Errorf("an RSA key of %d bits", bits)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// dkimPublicOK checks the public key the plan shows and the DNS record names.
func dkimPublicOK(pub string) error {
	der, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		return errors.New("not base64")
	}
	if _, err := x509.ParsePKIXPublicKey(der); err != nil {
		return errors.New("not a public key")
	}
	return nil
}

// --------------------------------------------------------- certificates ----

// certSANRe is what a name a certificate covers may look like: a hostname, a
// wildcard, an address (custom certificates carry IP addresses and localhost
// as well). The certificate's own name is stricter: it is a path.
var certSANRe = regexp.MustCompile(`^[a-z0-9*][a-z0-9*.:-]{0,252}$`)

// vetCerts lets in the certificates of the sites that arrive. The name is a
// path under the certificate store and every name it carries is a site name
// here: a certificate with a name of another account's site, or one that
// would replace a certificate this panel has, stays behind.
func (s *Server) vetCerts(ctx context.Context, v *migrateVet, b *apitypes.MigrationBundle, siteNames map[string]bool) {
	seen := map[string]bool{}
	own := s.panelNames(ctx)
	// The custom certificates let in, by fingerprint: a source serves one
	// file (a wildcard) on several sites, and it arrives once (sameCerts).
	prints := map[string][]*apitypes.MigrationCert{}
	var order []string
	for i := range b.Certificates {
		mc := &b.Certificates[i]
		fp := ""
		target := shown(mc.Name)
		if mc.Name != acme.NormalizeName(mc.Name) || acme.ValidateNameDNS(mc.Name) != nil {
			v.refuse(mc, "cert", target, "the certificate name is not a valid hostname")
			continue
		}
		if own[mc.Name] {
			v.refuse(mc, "cert", target, "a hostname of this panel itself: its certificate is the panel's own")
			continue
		}
		if seen[mc.Name] {
			v.refuse(mc, "cert", target, "the bundle carries this certificate twice")
			continue
		}
		seen[mc.Name] = true
		if mc.Kind != store.CertKindACME {
			mc.Kind = store.CertKindCustom
		}
		if b.Secrets != nil && mc.Cert != "" {
			info, err := checkCertPair(mc.Cert, mc.Key)
			if err != nil {
				v.refuse(mc, "cert", target, err.Error())
				continue
			}
			// What the certificate covers is read from it, not from the
			// source's word.
			mc.Names = info.Names
			na := info.NotAfter
			mc.NotAfter = &na
			fp = info.Fingerprint
			if !strings.Contains(strings.ToLower(info.IssuerOrg), "let's encrypt") {
				mc.Kind, mc.AutoRenew = store.CertKindCustom, false
			}
		}
		bad := ""
		for _, n := range mc.Names {
			if !certSANRe.MatchString(n) {
				bad = n
				break
			}
		}
		if bad != "" {
			v.refuse(mc, "cert", target, fmt.Sprintf("the certificate carries %q, which is neither a hostname nor an address", shown(bad)))
			continue
		}
		if !siteNames[mc.Name] {
			v.skip(mc, "cert", mc.Name, "the certificate is not installed: it is not for a site that arrives", "")
			continue
		}
		if !certCovers(mc.Names, mc.Name) && len(mc.Names) > 0 {
			v.skip(mc, "cert", mc.Name, "the certificate is not installed: it does not cover its own name", "")
			continue
		}
		foreign := ""
		for _, n := range mc.Names {
			n = acme.NormalizeName(n)
			if siteNames[n] || strings.HasPrefix(n, "*.") {
				continue
			}
			if others, err := s.db.FindSitesByName(ctx, n); err == nil && len(others) > 0 {
				foreign = n + " (site " + others[0].Domain + " of " + others[0].Login + ")"
				break
			}
		}
		if foreign != "" {
			v.skip(mc, "cert", mc.Name, "the certificate is not installed: it also names "+foreign+" here", "after the DNS switch: mp ssl issue "+mc.Name)
			continue
		}
		if _, err := s.db.GetCertificateByName(ctx, mc.Name); err == nil {
			v.skip(mc, "cert", mc.Name, "the certificate is not installed: this panel already has a certificate with this name", "")
			continue
		}
		if mc.Kind == store.CertKindACME {
			mc.Names = acmeNames(mc.Names, func(n string) bool { return siteNames[n] })
			if wildcardName(mc.Names) && mc.AutoRenew {
				v.warn("cert", mc.Name, "a Let's Encrypt wildcard renews through DNS-01 only: before it expires add a DNS provider here and order it again with it",
					"mp dns-provider add …; "+reorderHint(mc.Name, mc.Names))
			}
		} else if fp != "" {
			if prints[fp] == nil {
				order = append(order, fp)
			}
			prints[fp] = append(prints[fp], mc)
		}
	}
	for _, fp := range order {
		sameCerts(v, prints[fp])
	}
}

// sameCerts keeps one record of a certificate several sites of a source
// serve: the one whose name the certificate carries (the shortest), else the
// first. The others' sites are served by it all the same — it covers their
// names, and an account's certificate serves its own sites (certForSite) —
// and a renewal imported once reaches them all.
func sameCerts(v *migrateVet, certs []*apitypes.MigrationCert) {
	if len(certs) < 2 {
		return
	}
	keep := certs[0]
	for _, mc := range certs[1:] {
		if containsName(mc.Names, mc.Name) && (!containsName(keep.Names, keep.Name) || len(mc.Name) < len(keep.Name)) {
			keep = mc
		}
	}
	for _, mc := range certs {
		if mc != keep {
			v.skip(mc, "cert", mc.Name, "the same certificate arrives as "+keep.Name+", which serves this site too", "")
		}
	}
}

// acmeNames is what an ACME certificate of a source names in its record here:
// the names it covers that are the account's own sites, and a wildcard over
// one of them (*.example.com of the site example.com). The record's names
// are what a renewal orders and which sites the renewed certificate is
// attached to (reapplySitesForCert), and the issuer that makes a certificate
// ACME is only a string in it: the source's choice of names must not reach a
// site another account adds later — a wildcard serves only its account's
// sites (certOwnerFits), and another account cannot add a site under an
// account's domain (foreignDomain).
func acmeNames(names []string, own func(string) bool) []string {
	out := []string{}
	for _, n := range names {
		n = acme.NormalizeName(n)
		base, wild := strings.CutPrefix(n, "*.")
		if (own(n) || (wild && !strings.Contains(base, "*") && own(base))) && !containsName(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// wildcardName says whether a certificate's names include a wildcard: it
// renews through DNS-01 only.
func wildcardName(names []string) bool {
	for _, n := range names {
		if strings.Contains(n, "*") {
			return true
		}
	}
	return false
}

// checkCertPair is what certificates-import checks before it writes files:
// a certificate and a key that belong together, still valid.
func checkCertPair(certPEM, keyPEM string) (*acme.CertInfo, error) {
	cert := []byte(strings.TrimSpace(certPEM) + "\n")
	if _, err := tls.X509KeyPair(cert, []byte(strings.TrimSpace(keyPEM)+"\n")); err != nil {
		return nil, fmt.Errorf("the certificate and key do not match: %w", err)
	}
	info, err := acme.ParseCertificatePEM(cert)
	if err != nil {
		return nil, err
	}
	if time.Now().After(info.NotAfter) {
		return nil, fmt.Errorf("the certificate expired on %s", info.NotAfter.Format("2006-01-02"))
	}
	return info, nil
}
