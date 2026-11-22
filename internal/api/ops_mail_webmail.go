package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Roundcube ships a "complete" tarball with its PHP dependencies inside, so
// the panel needs neither composer nor npm on the server. The version is
// pinned here and its digest checked: the panel installs code that will read
// everybody's mail.
const roundcubeVersion = "1.7.4"

// roundcubeSHA256 is the digest of that tarball — a variable only so that the
// tests can hand the job a small archive of their own.
var roundcubeSHA256 = "2c6c878f0093f1bf7fb6086781d2dd9269d652c016b86939c157c5f1729139a2"

// roundcubePlugins are the plugins the panel switches on. A reinstall keeps
// the ones an administrator added to config.inc.php by hand after them.
var roundcubePlugins = []string{"archive", "zipdownload", "managesieve", "newmail_notifier"}

func roundcubeURL() string {
	return fmt.Sprintf("https://github.com/roundcube/roundcubemail/releases/download/%s/roundcubemail-%s-complete.tar.gz", roundcubeVersion, roundcubeVersion)
}

type webmailPayload struct {
	Domain     string `json:"domain"`
	User       string `json:"user"`
	PHPVersion string `json:"php_version,omitempty"`
	Port       int    `json:"port,omitempty"`
}

// jobWebmailInstall unpacks Roundcube into a site of the panel, creates its
// database and writes a configuration pointing at the local dovecot/postfix.
//
// The same job reinstalls, updates (a panel that ships a newer Roundcube) and
// moves the webmail, so a run over an existing install must leave it
// working: an existing site keeps its owner; the schema goes only into an
// empty database, an older one is brought up to date by Roundcube's own
// bin/updatedb.sh, and a newer Roundcube's stops the run before any file is
// touched; the database password, the DES key and the added plugins
// come from the config.inc.php being replaced. A fresh password that never
// reached the configuration cut the webmail off its database, and a fresh
// DES key signs everybody out.
func (s *Server) jobWebmailInstall(ctx context.Context, jc *jobs.Context) error {
	var p webmailPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	c := s.loadMailConfig(ctx)
	if !c.Installed {
		return errors.New("mail server is not installed")
	}
	if !mailDomainRe.MatchString(p.Domain) {
		return fmt.Errorf("webmail site name must be a domain: %q", p.Domain)
	}
	requested, err := s.db.GetUserByLogin(ctx, p.User)
	if err != nil {
		return err
	}
	// Before anything is made or downloaded: without a database server the
	// run could only leave a half-made site behind.
	inst, err := s.dbInstance(ctx)
	if err != nil {
		return err
	}
	site, owner, err := s.webmailSite(ctx, jc, c, p, requested)
	if err != nil {
		return err
	}
	l := s.layoutFor(site, owner)
	port := c.WebmailPort
	if p.Port != 0 {
		port = p.Port
	}
	// Read before anything changes: a configuration that is there but cannot
	// be read stops the run instead of being replaced by a new password and key.
	kept, err := s.keptRoundcubeConfig(ctx, c, site, owner, l)
	if err != nil {
		return err
	}
	if kept.from != "" {
		jc.Logf("existing configuration: %s", kept.from)
	}

	jc.Progress(10, "downloading Roundcube")
	archive, err := s.fetchRoundcube(ctx, jc)
	if err != nil {
		return err
	}
	schema, err := readRoundcubeSchema(archive)
	if err != nil {
		return err
	}
	// The database is looked at before the files: tables that are not
	// Roundcube's, or a newer Roundcube's schema, stop the run while the
	// webmail still runs as it is — an older release unpacked over a newer
	// one would no longer fit its database.
	dbName := webmailDBName(owner)
	version, err := s.roundcubeSchemaVersion(ctx, dbName)
	if err != nil {
		return err
	}
	if schema.newest > 0 && version > schema.newest {
		return fmt.Errorf("database %s holds the schema of a newer Roundcube (%d) than %s, the one this panel installs (%d): the panel does not put an older Roundcube over it", dbName, version, roundcubeVersion, schema.newest)
	}

	// From here on the run changes the site's tree, the owner's database,
	// the site's docroot and the mail settings, under the site's lock — the
	// one the removal of the site takes, site.delete's and user.delete's for
	// each of the account's sites (removeSiteConfig) — with the site and its
	// owner read again under it. A removal that came first left them being
	// removed, or gone: the run stops before it leaves a root-owned tree in
	// the home, a database after the account or a webmail on a site that is
	// not there. One that comes later waits for the run and takes away what
	// it made: the tree with the site or the home, the database recorded as
	// the owner's with the account's (user.delete lists the databases once
	// it is past the sites), the webmail with its site. A site this run made
	// is never missed: user.delete marks the account before it lists the
	// account's sites, so a site made after that listing finds the account
	// being removed here. The download stays outside: it takes minutes, and
	// the lock holds back the site's own jobs.
	defer s.lockSite(site.ID)()
	if err := s.webmailSiteStays(ctx, site.ID); err != nil {
		return err
	}

	jc.Progress(35, "unpacking")
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: l.siteRoot, Mode: 0o750, Owner: owner.Login, Group: s.cfg.WebGroup},
	}}); err != nil {
		return err
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "tar", Args: []string{
		"-xzf", archive, "-C", l.siteRoot, "--strip-components=1", "--no-same-owner",
	}, TimeoutSeconds: 300})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar: %s", strings.TrimSpace(res.Output))
	}
	// Код принадлежит root и владельцу не пишется — это ровно то, что нужно:
	// правки в веб-морде не должны переписывать саму вебпочту. Писать можно
	// только в temp и logs.
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: path.Join(l.siteRoot, "temp"), Mode: 0o750, Owner: owner.Login, Group: owner.Login},
		{Path: path.Join(l.siteRoot, "logs"), Mode: 0o750, Owner: owner.Login, Group: owner.Login},
		{Path: path.Join(l.siteRoot, "public_html"), Mode: 0o755},
	}}); err != nil {
		return err
	}
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{path.Join(l.siteRoot, "installer")}, Recursive: true}); err != nil {
		jc.Logf("warning: the installer directory was not removed: %v", err)
	}

	jc.Progress(60, "database")
	db, err := s.webmailDatabase(ctx, jc, owner, dbName, version, schema, kept)
	if err != nil {
		return err
	}

	jc.Progress(80, "configuration")
	certKind := ""
	if _, _, kind, _ := s.mailTLS(ctx, c); kind != "" {
		certKind = kind
	}
	verified := certKind == store.CertKindACME || certKind == store.CertKindCustom
	host := c.Hostname
	if !verified {
		host = "localhost"
	}
	desKey := kept.desKey
	if desKey == "" {
		token, err := auth.NewToken(18)
		if err != nil {
			return err
		}
		desKey = token[:24]
	} else {
		jc.Logf("the DES key stays: sessions and the settings encrypted with it remain readable")
	}
	plugins := slices.Clone(roundcubePlugins)
	for _, name := range kept.plugins {
		if !slices.Contains(plugins, name) {
			plugins = append(plugins, name)
		}
	}
	if extra := plugins[len(roundcubePlugins):]; len(extra) > 0 {
		jc.Logf("plugins added by hand stay on: %s", strings.Join(extra, ", "))
	}
	conf, err := s.render.Render("mail/roundcube.inc.php.tmpl", render.Roundcube{
		DSN:         roundcubeDSN(db.name, db.password, inst.Socket, db.name),
		IMAPHost:    fmt.Sprintf("tls://%s:143", host),
		SMTPHost:    fmt.Sprintf("tls://%s:587", host),
		SieveHost:   fmt.Sprintf("tls://%s:4190", host),
		VerifyPeer:  verified,
		SupportURL:  "",
		ProductName: webmailName(c, site.Domain, port) + " Webmail",
		DESKey:      desKey,
		Domain:      firstMailDomain(ctx, s),
		TempDir:     path.Join(l.siteRoot, "temp"),
		Plugins:     plugins,
	})
	if err != nil {
		return err
	}
	// The account changes only now, right before the file that names its
	// password: nothing that can fail sits between the two any more. Should
	// the write still fail, the next run finds the old file and sets the
	// account back to the password it names.
	if err := s.webmailAccount(ctx, owner, db); err != nil {
		return err
	}
	if _, err := s.agent.EnsureFile(ctx, &agent.EnsureFileRequest{
		Path: path.Join(l.siteRoot, "config", "config.inc.php"), Content: conf, Mode: 0o640, Owner: owner.Login, Group: owner.Login,
	}); err != nil {
		return err
	}
	if db.behind {
		if err := s.roundcubeUpdateDB(ctx, jc, owner, l); err != nil {
			return err
		}
	}
	// A site that becomes the webmail now serves Roundcube's web root: with
	// the site root as its docroot, nginx would hand out logs/ and temp/ as
	// plain files. Only now that Roundcube is in place, and on the record as
	// it is at this moment (the run took minutes).
	if c.Webmail != site.Domain && site.Docroot != "public_html" {
		cur, err := s.db.GetSite(ctx, site.ID)
		if err != nil {
			return err
		}
		was := cur.Clone()
		cur.Docroot = "public_html"
		if err := s.db.UpdateSiteChanges(ctx, was, cur); err != nil {
			return err
		}
		jc.Logf("site %s now serves public_html, Roundcube's web root: its logs and temporary files stay out of reach", site.Domain)
	}

	if c.Webmail != "" && c.Webmail != site.Domain {
		jc.Logf("the webmail moves here from %s: that site keeps its files and its copy of Roundcube; delete it (mp site rm %s) when it is no longer needed", c.Webmail, c.Webmail)
	}
	if c.WebmailVersion != "" && c.WebmailVersion != roundcubeVersion {
		jc.Logf("Roundcube %s → %s", c.WebmailVersion, roundcubeVersion)
	}
	// The settings are read again: the download takes minutes, and a change
	// made meanwhile (mp mail settings, the webmail port included) must not
	// be written back over (changeMailConfig). Only a port the request names
	// is the run's. The removal of the webmail's site — of the one it moves
	// from, too — takes the site out of them under the same lock
	// (forgetWebmail).
	c, err = s.changeMailConfig(ctx, func(cur *mailConfig) {
		cur.Webmail, cur.WebmailVersion = site.Domain, roundcubeVersion
		if p.Port != 0 {
			cur.WebmailPort = p.Port
		}
	})
	if err != nil {
		return err
	}
	// Under the site's own lock key, site:<domain>, as every job of the site
	// takes it: another apply or the delete of the site waits for this one.
	applyJob, err := s.enqueueSiteApply(ctx, site, jc.RequestedBy)
	if err != nil {
		return err
	}
	jc.Logf("the site is applied by job #%d; once it finishes, the webmail opens at https://%s/", applyJob, site.Domain)
	if c.WebmailPort != 0 {
		// Серверный блок на порту ссылается на сокет пула, который создаст
		// задача сайта, — поэтому он пишется после неё.
		if _, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(jc.RequestedBy)); err != nil {
			return err
		}
		jc.Logf("and at https://%s:%d/, with the mail server's name and certificate: no separate DNS record is needed", c.Hostname, c.WebmailPort)
	}
	jc.Progress(100, "Roundcube "+roundcubeVersion+" installed")
	return nil
}

// webmailSite finds the site the webmail goes into, or makes it. An existing
// site keeps its owner — its files and the webmail's database belong to that
// account, whoever the request named — and one that is not the webmail yet
// must hold nothing the archive would overwrite.
func (s *Server) webmailSite(ctx context.Context, jc *jobs.Context, c mailConfig, p webmailPayload, requested *store.User) (*store.Site, *store.User, error) {
	site, err := s.db.GetSiteByDomain(ctx, p.Domain)
	if errors.Is(err, store.ErrNotFound) {
		if requested.UnixUID == nil {
			return nil, nil, errors.New("the owner has no unix account")
		}
		// Asked before anything is made or downloaded; the run asks again
		// under the site's lock (webmailSiteStays).
		if err := webmailOwnerStays(requested); err != nil {
			return nil, nil, err
		}
		if err := s.webmailDBFree(ctx, requested); err != nil {
			return nil, nil, err
		}
		phpVersion := p.PHPVersion
		if phpVersion == "" {
			versions, _ := s.db.ListPHPVersions(ctx)
			for _, v := range versions {
				if v.Status == store.PHPInstalled {
					phpVersion = v.Version
				}
			}
		}
		if phpVersion == "" {
			return nil, nil, errors.New("no PHP branch installed (mp php install 8.4)")
		}
		if err := s.checkPHPInstalled(ctx, phpVersion); err != nil {
			return nil, nil, err
		}
		site = &store.Site{
			UserID: requested.ID, Domain: p.Domain, Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: phpVersion,
			Docroot: "public_html", SSL: "auto", HTTP2: true, RedirectHTTPS: true, StaticByNginx: true,
			PHPIni: map[string]string{"upload_max_filesize": "32M", "post_max_size": "32M", "memory_limit": "256M"},
		}
		if err := s.db.CreateSite(ctx, site); err != nil {
			return nil, nil, err
		}
		jc.Logf("site %s created (PHP %s, docroot public_html)", site.Domain, phpVersion)
		return site, requested, nil
	}
	if err != nil {
		return nil, nil, err
	}
	owner := requested
	if site.UserID != requested.ID {
		if owner, err = s.db.GetUserByID(ctx, site.UserID); err != nil {
			return nil, nil, err
		}
		jc.Logf("site %s belongs to %s: the webmail is installed for %s (the request named %s)", site.Domain, owner.Login, owner.Login, requested.Login)
	}
	if owner.UnixUID == nil {
		return nil, nil, errors.New("the owner has no unix account")
	}
	if site.Status == store.SiteDeleting {
		return nil, nil, fmt.Errorf("site %s is being removed: the webmail is not installed into it", site.Domain)
	}
	if err := webmailOwnerStays(owner); err != nil {
		return nil, nil, err
	}
	if site.Mode == store.ModeProxy || site.PHPVersion == "" {
		return nil, nil, fmt.Errorf("site %s does not run PHP, and the webmail needs it", site.Domain)
	}
	if err := s.webmailDBFree(ctx, owner); err != nil {
		return nil, nil, err
	}
	if c.Webmail == site.Domain {
		jc.Logf("site %s is the webmail: reinstalling it", site.Domain)
	} else {
		if err := s.webmailSiteFree(ctx, owner, site, s.layoutFor(site, owner)); err != nil {
			return nil, nil, err
		}
		jc.Logf("site %s already exists; installing the webmail into it", site.Domain)
	}
	if p.PHPVersion != "" && p.PHPVersion != site.PHPVersion {
		jc.Logf("site %s keeps PHP %s (mp site set %s --php %s changes it)", site.Domain, site.PHPVersion, site.Domain, p.PHPVersion)
	}
	return site, owner, nil
}

// webmailOwnerStays refuses an account on the way out: the webmail would
// make a site, a tree and a database for it that its removal may not see.
func webmailOwnerStays(owner *store.User) error {
	if owner.Status == store.UserDeleting {
		return fmt.Errorf("the account %s is being removed: the webmail is not installed for it", owner.Login)
	}
	return nil
}

// webmailSiteStays reads the webmail's site and its owner again, under the
// site's lock (the caller holds it): a site or an account that is being
// removed, or gone, stops the run before it changes anything of theirs.
func (s *Server) webmailSiteStays(ctx context.Context, id int64) error {
	site, err := s.db.GetSite(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("the webmail's site was removed meanwhile: nothing was installed")
	}
	if err != nil {
		return err
	}
	if site.Status == store.SiteDeleting {
		return fmt.Errorf("site %s is being removed: the webmail is not installed into it", site.Domain)
	}
	owner, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	return webmailOwnerStays(owner)
}

// webmailSiteFree makes sure the archive lands on nothing that belongs to the
// owner: an existing site that is not the webmail yet may hold the panel's
// placeholder page or an earlier Roundcube (an install that did not finish,
// or a webmail that moved away and back) — anything else is a site of its
// own, which unpacking Roundcube over it would wreck.
func (s *Server) webmailSiteFree(ctx context.Context, owner *store.User, site *store.Site, l *siteLayout) error {
	st, err := s.agent.Stat(ctx, l.siteRoot)
	if err != nil {
		return fmt.Errorf("look at %s: %w", l.siteRoot, err)
	}
	if len(st.Entries) != 1 || !st.Entries[0].Exists {
		return nil
	}
	rel := strings.TrimPrefix(l.siteRoot, l.home+"/")
	entries, err := s.listAsOwner(ctx, owner, rel)
	if err != nil {
		return err
	}
	types := map[string]string{}
	for _, e := range entries {
		types[e.Name] = e.Type
	}
	switch {
	case len(entries) == 0:
		return nil
	case types["program"] == "dir" && types["SQL"] == "dir" && types["index.php"] == "file":
		return nil // Roundcube's own layout
	case l.docroot == l.siteRoot:
		if s.onlyPlaceholder(ctx, owner, rel, site.Domain, entries) {
			return nil
		}
	default:
		top, _, _ := strings.Cut(strings.TrimPrefix(l.docroot, l.siteRoot+"/"), "/")
		if len(entries) == 1 && entries[0].Name == top && entries[0].Type == "dir" {
			docRel := strings.TrimPrefix(l.docroot, l.home+"/")
			doc, err := s.listAsOwner(ctx, owner, docRel)
			if err != nil {
				return err
			}
			if len(doc) == 0 || s.onlyPlaceholder(ctx, owner, docRel, site.Domain, doc) {
				return nil
			}
		}
	}
	names := make([]string, 0, 4)
	for _, e := range entries {
		if len(names) == 3 {
			names = append(names, "…")
			break
		}
		names = append(names, e.Name)
	}
	return fmt.Errorf("site %s already holds files of its own (%s in %s): the webmail goes into a new site, an empty one or the one it runs in now — pick another domain or empty the site first", site.Domain, strings.Join(names, ", "), l.siteRoot)
}

// listAsOwner lists a directory of the owner's home as the owner.
func (s *Server) listAsOwner(ctx context.Context, owner *store.User, rel string) ([]apitypes.FileEntry, error) {
	out, err := s.fsop(ctx, owner, nil, "list", rel)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", rel, err)
	}
	var entries []apitypes.FileEntry
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &entries); err != nil {
			return nil, fmt.Errorf("list %s: %w", rel, err)
		}
	}
	return entries, nil
}

// roundcubeKept is what a run takes over from the config.inc.php it replaces.
type roundcubeKept struct {
	from                       string // the file ("" — no install to take over)
	dbUser, dbPassword, dbName string
	desKey                     string
	plugins                    []string
}

var (
	rcDSNRe     = regexp.MustCompile(`\$config\[\s*'db_dsnw'\s*\]\s*=\s*'([^'\\]*)'\s*;`)
	rcDESKeyRe  = regexp.MustCompile(`\$config\[\s*'des_key'\s*\]\s*=\s*'([^'\\]*)'\s*;`)
	rcPluginsRe = regexp.MustCompile(`(?s)\$config\[\s*'plugins'\s*\]\s*=\s*(?:\[|array\s*\()(.*?)(?:\]|\))\s*;`)
	rcPluginRe  = regexp.MustCompile(`'([A-Za-z0-9_]{1,64})'`)
	rcCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// parseRoundcubeConfig reads the values a reinstall keeps from config.inc.php
// as the panel writes it (the last assignment wins, as in PHP). The file is
// the owner's to edit, so a value is taken only when it can go back into the
// file and into MySQL as it is; anything else is left for the panel to make
// anew.
func parseRoundcubeConfig(php string) roundcubeKept {
	var k roundcubeKept
	last := func(re *regexp.Regexp) (string, bool) {
		m := re.FindAllStringSubmatch(php, -1)
		if len(m) == 0 {
			return "", false
		}
		return m[len(m)-1][1], true
	}
	if dsn, ok := last(rcDSNRe); ok {
		if user, password, db, ok := parseRoundcubeDSN(dsn); ok && printableASCII(password) && len(password) >= 8 && len(password) <= 128 {
			k.dbUser, k.dbPassword, k.dbName = user, password, db
		}
	}
	// Roundcube's default cipher wants exactly 24 characters; the capture
	// holds no quote or backslash, so the value goes back into the file as is.
	if key, ok := last(rcDESKeyRe); ok && len(key) == 24 && printableASCII(key) {
		k.desKey = key
	}
	if block, ok := last(rcPluginsRe); ok {
		block = rcCommentRe.ReplaceAllString(block, "")
		for _, line := range strings.Split(block, "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, m := range rcPluginRe.FindAllStringSubmatch(line, -1) {
				if !slices.Contains(k.plugins, m[1]) && len(k.plugins) < 64 {
					k.plugins = append(k.plugins, m[1])
				}
			}
		}
	}
	return k
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// roundcubeDSN writes the database DSN the way Roundcube reads it:
// rcube_db::parse_dsn rawurldecodes the user and the password, so they are
// rawurlencoded here — a generated password may hold "%" and "+".
func roundcubeDSN(user, password, socket, db string) string {
	return fmt.Sprintf("mysql://%s:%s@unix(%s)/%s", rawURLEncode(user), rawURLEncode(password), socket, db)
}

// parseRoundcubeDSN is rcube_db::parse_dsn for the panel's mysql://user:pass@unix(socket)/db:
// the credentials end at the last "@", and the password is everything after
// the first ":" of them.
func parseRoundcubeDSN(dsn string) (user, password, db string, ok bool) {
	rest, found := strings.CutPrefix(dsn, "mysql://")
	at := strings.LastIndex(rest, "@")
	if !found || at < 0 {
		return "", "", "", false
	}
	cred, target := rest[:at], rest[at+1:]
	u, pw, found := strings.Cut(cred, ":")
	slash := strings.LastIndex(target, "/")
	if !found || slash < 0 {
		return "", "", "", false
	}
	db, _, _ = strings.Cut(target[slash+1:], "?")
	return rawURLDecode(u), rawURLDecode(pw), rawURLDecode(db), true
}

// rawURLEncode is PHP's rawurlencode: everything but letters, digits and
// "-_.~" becomes %XX.
func rawURLEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// rawURLDecode is PHP's rawurldecode: %XX with two hex digits becomes the
// byte, any other "%" stays as it is — which is what Roundcube made of a
// password an older panel wrote into the DSN unencoded.
func rawURLDecode(s string) string {
	unhex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// keptRoundcubeConfig reads config.inc.php of the install a run replaces.
// When the webmail moves to another site of the same account, which shares
// its database, that is the current webmail site's file — the one in use,
// whatever an earlier install left in the target; otherwise, or when that
// file is gone, the target site's. The file is the owner's, so it is read as
// the owner; a file that is not there is no error, one that is there and
// cannot be read is.
func (s *Server) keptRoundcubeConfig(ctx context.Context, c mailConfig, site *store.Site, owner *store.User, l *siteLayout) (roundcubeKept, error) {
	files := []string{path.Join(l.siteRoot, "config", "config.inc.php")}
	if c.Webmail != "" && c.Webmail != site.Domain {
		if cur, err := s.db.GetSiteByDomain(ctx, c.Webmail); err == nil && cur.UserID == owner.ID {
			files = append([]string{path.Join(s.layoutFor(cur, owner).siteRoot, "config", "config.inc.php")}, files...)
		}
	}
	for _, file := range files {
		st, err := s.agent.Stat(ctx, file)
		if err != nil {
			return roundcubeKept{}, fmt.Errorf("look for %s: %w", file, err)
		}
		if len(st.Entries) != 1 || !st.Entries[0].Exists || st.Entries[0].IsDir {
			continue
		}
		raw, err := s.fsop(ctx, owner, nil, "read", strings.TrimPrefix(file, l.home+"/"))
		if err != nil {
			return roundcubeKept{}, fmt.Errorf("read %s: %w", file, err)
		}
		k := parseRoundcubeConfig(string(raw))
		k.from = file
		return k, nil
	}
	return roundcubeKept{}, nil
}

// fetchRoundcube downloads the release into the panel's download directory and
// checks its digest; an already downloaded file is reused.
func (s *Server) fetchRoundcube(ctx context.Context, jc *jobs.Context) (string, error) {
	dir := filepath.Join(s.cfg.DataDir, "downloads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	file := filepath.Join(dir, fmt.Sprintf("roundcubemail-%s-complete.tar.gz", roundcubeVersion))
	if sum, err := fileSHA256(file); err == nil && sum == roundcubeSHA256 {
		jc.Logf("archive already downloaded: %s", file)
		return file, nil
	}
	body, err := fetchLarge(ctx, roundcubeURL())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != roundcubeSHA256 {
		return "", fmt.Errorf("Roundcube archive checksum mismatch: %x", sum)
	}
	if err := os.WriteFile(file, body, 0o640); err != nil {
		return "", err
	}
	jc.Logf("Roundcube %s downloaded (%d KB), sha256 matches", roundcubeVersion, len(body)/1024)
	return file, nil
}

// fetchLarge downloads a release archive; fetchBytes is capped at a megabyte,
// which is right for keys and repository files and far too small here.
func fetchLarge(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 64<<20))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// roundcubeSchema is the database half of a release: the schema an empty
// database is loaded with, and the newest of the updates in SQL/mysql/ —
// bin/updatedb.sh applies those newer than the version a database records.
// newest is the newest version the release knows at all, the one
// mysql.initial.sql records included: a database past it is a newer
// Roundcube's.
type roundcubeSchema struct {
	initial        string
	latest, newest int64
}

var (
	rcUpdateRe         = regexp.MustCompile(`SQL/mysql/([0-9]{1,12})\.sql$`)
	rcInitialVersionRe = regexp.MustCompile(`'roundcube-version'\s*,\s*'([0-9]{1,12})'`)
)

// readRoundcubeSchema pulls the schema out of the archive: the API process can
// read the file it downloaded, and nothing has to read the unpacked tree.
func readRoundcubeSchema(archive string) (roundcubeSchema, error) {
	var out roundcubeSchema
	f, err := os.Open(archive)
	if err != nil {
		return out, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return out, err
	}
	defer gz.Close() //nolint:errcheck // чтение архива
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		if m := rcUpdateRe.FindStringSubmatch(h.Name); m != nil {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil && v > out.latest {
				out.latest = v
			}
			continue
		}
		if strings.HasSuffix(h.Name, "SQL/mysql.initial.sql") {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, io.LimitReader(tr, 4<<20)); err != nil {
				return out, err
			}
			out.initial, found = buf.String(), true
		}
	}
	if !found {
		return out, errors.New("the archive has no SQL/mysql.initial.sql")
	}
	out.newest = out.latest
	if m := rcInitialVersionRe.FindStringSubmatch(out.initial); m != nil {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil && v > out.newest {
			out.newest = v
		}
	}
	return out, nil
}

// webmailDB is the database side of an install: the database, the password
// the configuration names for its account, and what the run still has to do.
type webmailDB struct {
	name, password string
	// rec is the panel's record of the database, the owner's.
	rec *store.Database
	// account brings the MySQL account in line with password; empty when
	// the account already signs in with it.
	account string
	// behind: the database holds an older schema than the release, and
	// bin/updatedb.sh brings it up to date once the configuration is written.
	behind bool
}

// webmailDBName is the database of the owner's webmail, and its account.
func webmailDBName(owner *store.User) string {
	name := owner.Login + "_roundcube"
	if len(name) > 32 {
		name = owner.Login + "_rc"
	}
	return name
}

// webmailDBFree makes sure the owner's webmail database can be the owner's:
// a name the panel has recorded for another account would be taken over
// (see dbNameTaken), and MySQL refuses account names over 32 characters.
func (s *Server) webmailDBFree(ctx context.Context, owner *store.User) error {
	name := webmailDBName(owner)
	if len(name) > 32 {
		return fmt.Errorf("the login %s is too long for the webmail's database account (MySQL allows 32 characters): install the webmail into a site of an account with a shorter login", owner.Login)
	}
	taken, err := s.dbNameTaken(ctx, owner, name)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("the webmail of %s keeps its data in the database %s, and that name belongs to a database of another account: install the webmail into a site of another account, or remove that database first if nobody needs it (mp db rm %s)", owner.Login, name, name)
	}
	return nil
}

// webmailDatabase makes (or finds) the database Roundcube keeps its sessions,
// contacts and preferences in and settles its schema; version is what
// roundcubeSchemaVersion found there before the files were unpacked. The
// account is only planned here, webmailAccount applies it next to the
// configuration.
//
// A run keeps what works: the schema of a database that has one (Roundcube's
// mysql.initial.sql is plain CREATE TABLE and fails on a loaded database),
// and the password of config.inc.php while the account exists — the same
// password set back when MySQL no longer takes it. Only a new account, or one
// without a configuration to keep a password from, gets a new password.
func (s *Server) webmailDatabase(ctx context.Context, jc *jobs.Context, owner *store.User, name string, version int64, schema roundcubeSchema, kept roundcubeKept) (*webmailDB, error) {
	// The name was free when the run began, minutes ago: it is checked again
	// and claimed in the panel before MySQL is touched, so that a database
	// another account makes meanwhile is neither taken over nor handed the
	// webmail's.
	if err := s.webmailDBFree(ctx, owner); err != nil {
		return nil, err
	}
	rec := &store.Database{UserID: owner.ID, Name: name}
	if err := s.db.CreateDatabase(ctx, rec); errors.Is(err, store.ErrExists) {
		if rec, err = s.db.GetDatabaseByName(ctx, name); err != nil {
			return nil, err
		}
		if rec.UserID != owner.ID {
			return nil, fmt.Errorf("database %s became another account's during the run: the webmail does not use it", name)
		}
	} else if err != nil {
		return nil, err
	}
	db := &webmailDB{name: name, rec: rec}
	if _, err := s.mysqlExec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;\n", name)); err != nil {
		return nil, err
	}
	switch {
	case version == 0:
		if _, err := s.mysqlExec(ctx, "USE `"+name+"`;\n"+schema.initial); err != nil {
			return nil, fmt.Errorf("Roundcube schema: %w", err)
		}
		jc.Logf("Roundcube schema loaded into %s", name)
	case version < schema.latest:
		db.behind = true
		jc.Logf("database %s holds Roundcube schema %d, this release brings %d: it is updated once the configuration is written", name, version, schema.latest)
	default:
		jc.Logf("database %s already holds the Roundcube schema (%d): its data stays as it is", name, version)
	}

	out, err := s.mysqlExec(ctx, fmt.Sprintf("SELECT COUNT(*) FROM mysql.user WHERE user = '%s' AND host = 'localhost';", sqlEscaper.Replace(name)))
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("account %s: unexpected answer %q", name, strings.TrimSpace(out))
	}
	exists := n > 0
	if exists && kept.dbPassword != "" && kept.dbUser == name && kept.dbName == name {
		works, err := s.mysqlSignsIn(ctx, name, kept.dbPassword)
		if err != nil {
			return nil, err
		}
		db.password = kept.dbPassword
		if works {
			jc.Logf("account %s keeps its password", name)
		} else {
			db.account = fmt.Sprintf("ALTER USER '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s';\n", name, sqlEscaper.Replace(db.password))
			jc.Logf("account %s does not take the password config.inc.php names: it is set back to that one", name)
		}
		return db, nil
	}
	if db.password, err = auth.NewPassword(20); err != nil {
		return nil, err
	}
	db.account = fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s';\n"+
		"ALTER USER '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s';\n",
		name, sqlEscaper.Replace(db.password), name, sqlEscaper.Replace(db.password))
	if exists {
		jc.Logf("account %s gets a new password: there is no configuration to keep its password from", name)
	} else {
		jc.Logf("account %s is created", name)
	}
	return db, nil
}

// roundcubeSchemaVersion inspects the webmail's database: 0 for an empty one,
// the schema version Roundcube records in its system table for a loaded one,
// and an error for tables without that version — an interrupted load or
// somebody else's data, which neither the initial schema nor an update fits.
func (s *Server) roundcubeSchemaVersion(ctx context.Context, name string) (int64, error) {
	out, err := s.mysqlExec(ctx, fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s';", sqlEscaper.Replace(name)))
	if err != nil {
		return 0, err
	}
	tables := strings.Fields(out)
	if len(tables) == 0 {
		return 0, nil
	}
	if slices.Contains(tables, "system") {
		out, err := s.mysqlExec(ctx, fmt.Sprintf("SELECT `value` FROM `%s`.`system` WHERE `name` = 'roundcube-version';", name))
		if err != nil {
			return 0, err
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("database %s holds %d tables but no Roundcube schema version: an interrupted install or somebody else's data; drop it (mp db rm %s) or empty it and run the webmail install again", name, len(tables), name)
}

// mysqlSignsIn tries a password the way the webmail uses it: as the account,
// over the socket. The password travels in the environment, not on argv.
func (s *Server) mysqlSignsIn(ctx context.Context, user, password string) (bool, error) {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{
		Name: "mysql", Args: []string{"--protocol=socket", "--batch", "--skip-column-names", "--user=" + user},
		Env: []string{"MYSQL_PWD=" + password}, Stdin: "SELECT 1;", TimeoutSeconds: 30,
	})
	if err != nil {
		return false, err
	}
	if res.ExitCode == 0 {
		return true, nil
	}
	if strings.Contains(res.Output, "Access denied") {
		return false, nil
	}
	return false, fmt.Errorf("mysql as %s: %s", user, strings.TrimSpace(res.Output))
}

// webmailAccount applies the planned account change with the grant, and
// records the account next to the database for the owner's list.
func (s *Server) webmailAccount(ctx context.Context, owner *store.User, db *webmailDB) error {
	sql := db.account + fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'localhost';\nFLUSH PRIVILEGES;\n", db.name, db.name)
	if _, err := s.mysqlExec(ctx, sql); err != nil {
		return err
	}
	if len(db.rec.Users) == 0 {
		s.db.CreateDBUser(ctx, &store.DBUser{UserID: owner.ID, DatabaseID: &db.rec.ID, Name: db.name, Host: "localhost", AuthPlugin: "caching_sha2_password"}) //nolint:errcheck // строка учётки — украшение списка баз
	}
	return nil
}

// roundcubeUpdateDB brings an older schema up to the release with Roundcube's
// own bin/updatedb.sh: it applies the SQL/mysql/ updates newer than the
// version the database records and moves that version on. It runs as the
// site's owner with the site's PHP — the configuration it reads is the
// owner's file, so nothing in it runs with more rights than the webmail.
func (s *Server) roundcubeUpdateDB(ctx context.Context, jc *jobs.Context, owner *store.User, l *siteLayout) error {
	if l.php == nil {
		return errors.New("no PHP layout for this OS: the schema update cannot run")
	}
	rel := strings.TrimPrefix(l.siteRoot, l.home+"/")
	out, err := s.fsop(ctx, owner, nil, "run", "--cwd", rel, "--", l.php.CLIBinary, "bin/updatedb.sh", "--package=roundcube", "--dir="+path.Join(l.siteRoot, "SQL"))
	if err != nil {
		return fmt.Errorf("Roundcube schema update (bin/updatedb.sh): %w", err)
	}
	logTail(jc, string(out), 20)
	jc.Logf("database schema updated")
	return nil
}

// webmailName is what Roundcube calls itself: тем именем, по которому вебпочту
// и будут открывать — на порту это имя почтового сервера, а не сайта. The
// port is the one the webmail ends up on, so that a run without --port over
// a webmail published on a port names it the same as before.
func webmailName(c mailConfig, domain string, port int) string {
	if port != 0 {
		return c.Hostname
	}
	return domain
}

// firstMailDomain is what Roundcube appends when someone logs in without a
// domain part.
func firstMailDomain(ctx context.Context, s *Server) string {
	domains, err := s.db.ListMailDomains(ctx, 0)
	if err != nil || len(domains) == 0 {
		return ""
	}
	return domains[0].Name
}
