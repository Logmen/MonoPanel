package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Roundcube ships a "complete" tarball with its PHP dependencies inside, so
// the panel needs neither composer nor npm on the server. The version is
// pinned here and its digest checked: the panel installs code that will read
// everybody's mail.
const (
	roundcubeVersion = "1.7.4"
	roundcubeSHA256  = "2c6c878f0093f1bf7fb6086781d2dd9269d652c016b86939c157c5f1729139a2"
)

func roundcubeURL() string {
	return fmt.Sprintf("https://github.com/roundcube/roundcubemail/releases/download/%s/roundcubemail-%s-complete.tar.gz", roundcubeVersion, roundcubeVersion)
}

type webmailPayload struct {
	Domain     string `json:"domain"`
	User       string `json:"user"`
	PHPVersion string `json:"php_version,omitempty"`
}

// jobWebmailInstall unpacks Roundcube into a site of the panel, creates its
// database and writes a configuration pointing at the local dovecot/postfix.
func (s *Server) jobWebmailInstall(ctx context.Context, jc *jobs.Context) error {
	var p webmailPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	c := s.loadMailConfig(ctx)
	if !c.Installed {
		return errors.New("почтовый сервер не установлен")
	}
	if !mailDomainRe.MatchString(p.Domain) {
		return fmt.Errorf("имя сайта вебпочты должно быть доменом: %q", p.Domain)
	}
	owner, err := s.db.GetUserByLogin(ctx, p.User)
	if err != nil {
		return err
	}
	if owner.UnixUID == nil {
		return errors.New("у владельца нет unix-аккаунта")
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
		return errors.New("не установлено ни одной ветки PHP (mp php install 8.4)")
	}

	site, err := s.db.GetSiteByDomain(ctx, p.Domain)
	if errors.Is(err, store.ErrNotFound) {
		site = &store.Site{
			UserID: owner.ID, Domain: p.Domain, Aliases: []string{}, Mode: store.ModeFPM, PHPVersion: phpVersion,
			Docroot: "public_html", SSL: "auto", HTTP2: true, RedirectHTTPS: true, StaticByNginx: true,
			PHPIni: map[string]string{"upload_max_filesize": "32M", "post_max_size": "32M", "memory_limit": "256M"},
		}
		if err := s.db.CreateSite(ctx, site); err != nil {
			return err
		}
		jc.Logf("сайт %s создан (PHP %s, docroot public_html)", site.Domain, phpVersion)
	} else if err != nil {
		return err
	} else {
		jc.Logf("сайт %s уже существует, ставлю вебпочту в него", site.Domain)
	}
	l := s.layoutFor(site, owner)

	jc.Progress(10, "загрузка Roundcube")
	archive, err := s.fetchRoundcube(ctx, jc)
	if err != nil {
		return err
	}

	jc.Progress(35, "распаковка")
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
		jc.Logf("предупреждение: каталог installer остался на месте: %v", err)
	}

	jc.Progress(60, "база данных")
	dbName, dbPass, err := s.webmailDatabase(ctx, jc, owner)
	if err != nil {
		return err
	}
	initial, err := roundcubeInitialSQL(archive)
	if err != nil {
		return err
	}
	if _, err := s.mysqlExec(ctx, "USE `"+dbName+"`;\n"+initial); err != nil {
		return fmt.Errorf("схема Roundcube: %w", err)
	}
	jc.Logf("схема Roundcube загружена в %s", dbName)

	jc.Progress(80, "конфигурация")
	inst, err := s.dbInstance(ctx)
	if err != nil {
		return err
	}
	certKind := ""
	if _, _, kind, _ := s.mailTLS(ctx, c); kind != "" {
		certKind = kind
	}
	verified := certKind == store.CertKindACME || certKind == store.CertKindCustom
	host := c.Hostname
	if !verified {
		host = "localhost"
	}
	desKey, err := auth.NewToken(18)
	if err != nil {
		return err
	}
	conf, err := s.render.Render("mail/roundcube.inc.php.tmpl", render.Roundcube{
		DSN:         fmt.Sprintf("mysql://%s:%s@unix(%s)/%s", dbName, dbPass, inst.Socket, dbName),
		IMAPHost:    fmt.Sprintf("tls://%s:143", host),
		SMTPHost:    fmt.Sprintf("tls://%s:587", host),
		SieveHost:   fmt.Sprintf("tls://%s:4190", host),
		VerifyPeer:  verified,
		SupportURL:  "",
		ProductName: "Почта " + p.Domain,
		DESKey:      desKey[:24],
		Domain:      firstMailDomain(ctx, s),
		TempDir:     path.Join(l.siteRoot, "temp"),
		Plugins:     []string{"archive", "zipdownload", "managesieve", "newmail_notifier"},
	})
	if err != nil {
		return err
	}
	if _, err := s.agent.EnsureFile(ctx, &agent.EnsureFileRequest{
		Path: path.Join(l.siteRoot, "config", "config.inc.php"), Content: conf, Mode: 0o640, Owner: owner.Login, Group: owner.Login,
	}); err != nil {
		return err
	}

	c.Webmail, c.WebmailVersion = p.Domain, roundcubeVersion
	if err := s.saveMailConfig(ctx, c); err != nil {
		return err
	}
	job, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: site.ID}, jobs.WithLockKey(fmt.Sprintf("site:%d", site.ID)), jobs.WithRequestedBy(jc.RequestedBy))
	if err != nil {
		return err
	}
	jc.Logf("сайт применяется задачей #%d; после неё вебпочта откроется на https://%s/", job.ID, p.Domain)
	jc.Progress(100, "Roundcube "+roundcubeVersion+" установлен")
	return nil
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
		jc.Logf("архив уже загружен: %s", file)
		return file, nil
	}
	body, err := fetchLarge(ctx, roundcubeURL())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != roundcubeSHA256 {
		return "", fmt.Errorf("контрольная сумма архива Roundcube не совпала: %x", sum)
	}
	if err := os.WriteFile(file, body, 0o640); err != nil {
		return "", err
	}
	jc.Logf("Roundcube %s загружен (%d КБ), sha256 совпала", roundcubeVersion, len(body)/1024)
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

// roundcubeInitialSQL pulls the schema out of the archive: the API process can
// read the file it downloaded, and nothing has to read the unpacked tree.
func roundcubeInitialSQL(archive string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close() //nolint:errcheck // чтение архива
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(h.Name, "SQL/mysql.initial.sql") {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, io.LimitReader(tr, 4<<20)); err != nil {
				return "", err
			}
			return buf.String(), nil
		}
	}
	return "", errors.New("в архиве нет SQL/mysql.initial.sql")
}

// webmailDatabase creates (or reuses) the MySQL database Roundcube stores its
// sessions, contacts and preferences in.
func (s *Server) webmailDatabase(ctx context.Context, jc *jobs.Context, owner *store.User) (name, password string, err error) {
	if _, err := s.dbInstance(ctx); err != nil {
		return "", "", err
	}
	name = owner.Login + "_roundcube"
	if len(name) > 32 {
		name = owner.Login + "_rc"
	}
	password, err = auth.NewToken(15)
	if err != nil {
		return "", "", err
	}
	sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;\n"+
		"CREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s';\n"+
		"ALTER USER '%s'@'localhost' IDENTIFIED WITH caching_sha2_password BY '%s';\n"+
		"GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'localhost';\nFLUSH PRIVILEGES;\n",
		name, name, sqlEscaper.Replace(password), name, sqlEscaper.Replace(password), name, name)
	if _, err := s.mysqlExec(ctx, sql); err != nil {
		return "", "", err
	}
	db := &store.Database{UserID: owner.ID, Name: name}
	if err := s.db.CreateDatabase(ctx, db); err == nil {
		s.db.CreateDBUser(ctx, &store.DBUser{UserID: owner.ID, DatabaseID: &db.ID, Name: name, Host: "localhost", AuthPlugin: "caching_sha2_password"}) //nolint:errcheck // строка учётки — украшение списка баз
	} else if !errors.Is(err, store.ErrExists) {
		return "", "", err
	}
	jc.Logf("база %s готова", name)
	return name, password, nil
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
