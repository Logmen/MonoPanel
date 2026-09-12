package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// A CMS is installed the way its vendor documents it, only without the
// clicking: the distribution comes from the vendor, lands in the docroot as
// the client, and the CMS's own installer (wp-cli for WordPress, the CLI
// installers of Joomla and OpenCart, the web wizard of Bitrix) sets it up
// against a database made for it. The site gets the matching preset first.

const (
	wpCLIPhar = "/usr/local/lib/monopanel/wp-cli.phar"
	wpCLIURL  = "https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar"
)

type cmsDef struct {
	ID, Name, Preset, Source, Notes, AdminPath string
	Editions                                   []string
}

var cmsCatalog = []cmsDef{
	{ID: "wordpress", Name: "WordPress", Preset: presetWordPress, Source: "wordpress.org (latest), wp-cli с wp-cli.org", AdminPath: "wp-admin/", Notes: "ЧПУ-ссылки включены пресетом; письмо администратору не отправляется."},
	{ID: "joomla", Name: "Joomla", Preset: presetJoomla, Source: "github.com/joomla/joomla-cms, последний релиз", AdminPath: "administrator/", Notes: "Каталог installation/ удаляется после установки."},
	{ID: "opencart", Name: "OpenCart", Preset: presetOpenCart, Source: "github.com/opencart/opencart, последний релиз", AdminPath: "admin/", Notes: "Каталог install/ удаляется после установки; storage/ закрыт пресетом."},
	{ID: "bitrix", Name: "1С-Битрикс", Preset: presetBitrix, Source: "1c-bitrix.ru, пробная редакция: start, standard, small_business или business", AdminPath: "bitrix/admin/", Editions: []string{"start", "standard", "small_business", "business"},
		Notes: "Пробная версия с регистрацией на 1c-bitrix.ru от имени администратора сайта. По умолчанию ставится «Чистая установка» из Маркетплейса (без демо-сайта); demo — демо-сайт из дистрибутива; можно указать id решения из Маркетплейса. Лицензионный ключ вводится потом в настройках Битрикса, редакция должна совпадать с ключом."},
}

func cmsByID(id string) *cmsDef {
	for i := range cmsCatalog {
		if cmsCatalog[i].ID == id {
			return &cmsCatalog[i]
		}
	}
	return nil
}

// cmsPayload is the job's brief; the passwords travel encrypted, the job
// table is not the place for them in the clear.
type cmsPayload struct {
	SiteID           int64  `json:"site_id"`
	CMS              string `json:"cms"`
	Title            string `json:"title"`
	AdminLogin       string `json:"admin_login"`
	AdminEmail       string `json:"admin_email"`
	AdminPasswordEnc string `json:"admin_password_enc"`
	Edition          string `json:"edition,omitempty"`
	Solution         string `json:"solution,omitempty"`
	Database         string `json:"database"`
	DBPasswordEnc    string `json:"db_password_enc"`
	Force            bool   `json:"force,omitempty"`
	ApplyPreset      bool   `json:"apply_preset,omitempty"`
}

// Test hooks: the downloads and the release lookup.
var (
	cmsFetch      = fetchStream
	cmsFetchBytes = fetchBytesN
	cmsLatestTag  = githubLatestTag
)

type cmsListOutput struct {
	Body []apitypes.CMSInfo
}

type cmsInstallInput struct {
	Domain string `path:"domain"`
	Body   apitypes.CMSInstallRequest
}

type cmsInstallOutput struct {
	Status int
	Body   apitypes.CMSInstallResult
}

func (s *Server) registerCMS() {
	huma.Register(s.api, huma.Operation{
		OperationID: "cms-list", Method: http.MethodGet, Path: "/cms", Summary: "CMS the panel can install into a site", Tags: []string{"sites"}, Security: secured,
	}, func(_ context.Context, _ *struct{}) (*cmsListOutput, error) {
		out := make([]apitypes.CMSInfo, 0, len(cmsCatalog))
		for _, d := range cmsCatalog {
			out = append(out, apitypes.CMSInfo{ID: d.ID, Name: d.Name, Preset: d.Preset, Source: d.Source, Editions: d.Editions, Notes: d.Notes})
		}
		return &cmsListOutput{Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-cms-install", Method: http.MethodPost, Path: "/sites/{domain}/cms", Summary: "Install a CMS into the site (async): preset, database, files, the CMS's own installer", Tags: []string{"sites"}, Security: secured, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *cmsInstallInput) (*cmsInstallOutput, error) {
		p := principalFrom(ctx)
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		def := cmsByID(in.Body.CMS)
		if def == nil {
			return nil, huma.Error422UnprocessableEntity("unknown CMS: " + in.Body.CMS)
		}
		if site.Mode == store.ModeProxy {
			return nil, huma.Error422UnprocessableEntity("a proxy site has no PHP to run a CMS")
		}
		if site.Status == store.SiteSuspended {
			return nil, huma.Error422UnprocessableEntity("the site is suspended")
		}
		if err := s.checkPHPInstalled(ctx, site.PHPVersion); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		owner, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, err
		}
		if owner.UnixUID == nil {
			return nil, huma.Error422UnprocessableEntity("the owner has no unix account yet")
		}
		if _, err := s.dbInstance(ctx); err != nil {
			return nil, huma.Error422UnprocessableEntity("a database server is required: " + err.Error())
		}
		if s.secrets == nil {
			return nil, huma.Error500InternalServerError("the encryption key is unavailable")
		}
		if def.Editions == nil && (in.Body.Edition != "" || in.Body.Solution != "") {
			return nil, huma.Error422UnprocessableEntity(def.Name + " has no editions or solutions to choose")
		}
		if in.Body.Solution != "" && !cmsSolutionRe.MatchString(in.Body.Solution) {
			return nil, huma.Error422UnprocessableEntity("solution: clean, demo or a marketplace id like vendor.solution")
		}
		login := in.Body.AdminLogin
		if login == "" {
			login = "admin"
		}
		password, generated := in.Body.AdminPassword, false
		if password == "" {
			// 16 characters satisfy every CMS at once (Joomla wants 12+, OpenCart at most 20)
			password, _ = auth.NewPassword(16)
			generated = true
		}
		email := in.Body.AdminEmail
		if email == "" {
			email = owner.Email
		}
		if email == "" {
			email = "admin@" + site.Domain
		}
		title := in.Body.Title
		if title == "" {
			title = site.Domain
		}
		dbName, err := s.freeDatabaseName(ctx, owner, def.ID, in.Body.Force)
		if err != nil {
			return nil, err
		}
		dbPassword, _ := auth.NewPassword(20)
		pwEnc, err := s.secrets.Encrypt(password)
		if err != nil {
			return nil, err
		}
		dbEnc, err := s.secrets.Encrypt(dbPassword)
		if err != nil {
			return nil, err
		}
		payload := cmsPayload{SiteID: site.ID, CMS: def.ID, Title: title, AdminLogin: login, AdminEmail: email, AdminPasswordEnc: pwEnc, Edition: in.Body.Edition, Solution: in.Body.Solution, Database: dbName, DBPasswordEnc: dbEnc, Force: in.Body.Force}
		if site.Preset != def.Preset {
			site.Preset = def.Preset
			if err := s.db.UpdateSite(ctx, site); err != nil {
				return nil, err
			}
			payload.ApplyPreset = true
		}
		job, err := s.jobs.Enqueue(ctx, "site.cms", payload, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.cms", Target: site.Domain, IP: requestInfo(ctx).IP, Details: map[string]any{"cms": def.ID, "database": dbName}})
		out := apitypes.CMSInstallResult{JobID: job.ID, CMS: def.ID, AdminURL: s.siteURL(ctx, site) + "/" + def.AdminPath, AdminLogin: login, AdminEmail: email, Database: dbName}
		if generated {
			out.AdminPassword = password
		}
		return &cmsInstallOutput{Status: http.StatusAccepted, Body: out}, nil
	})
}

// siteURL is how the site is reached: https once it has a certificate.
func (s *Server) siteURL(ctx context.Context, site *store.Site) string {
	if s.certForSite(ctx, site) != nil {
		return "https://" + site.Domain
	}
	return "http://" + site.Domain
}

// freeDatabaseName is <login>_<cms>, with a number when that is taken. A
// forced reinstall takes the plain name back: the job empties it first.
func (s *Server) freeDatabaseName(ctx context.Context, owner *store.User, cms string, reuse bool) (string, error) {
	base := owner.Login + "_" + cms
	if len(base) > 32 {
		return "", huma.Error422UnprocessableEntity("login_cms exceeds MySQL's 32-character account limit")
	}
	if reuse {
		return base, nil
	}
	for i := 0; i < 20; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s%d", base, i+1)
		}
		if _, err := s.db.GetDatabaseByName(ctx, name); errors.Is(err, store.ErrNotFound) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", huma.Error422UnprocessableEntity("too many " + cms + " databases already; reinstall with force to reuse " + base)
}

// jobSiteCMS does the install: the preset (when it changed), an empty
// docroot, the database, the files, the installer, the record.
func (s *Server) jobSiteCMS(ctx context.Context, jc *jobs.Context) error {
	var p cmsPayload
	if err := json.Unmarshal(jc.Payload, &p); err != nil {
		return err
	}
	site, err := s.db.GetSite(ctx, p.SiteID)
	if err != nil {
		return err
	}
	owner, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	def := cmsByID(p.CMS)
	if def == nil {
		return errors.New("unknown CMS " + p.CMS)
	}
	l := s.layoutFor(site, owner)
	if l.php == nil {
		return errors.New("no PHP layout for this OS")
	}
	adminPassword, err := s.secrets.Decrypt(p.AdminPasswordEnc)
	if err != nil {
		return fmt.Errorf("decrypt the administrator password: %w", err)
	}
	dbPassword, err := s.secrets.Decrypt(p.DBPasswordEnc)
	if err != nil {
		return fmt.Errorf("decrypt the database password: %w", err)
	}
	if p.ApplyPreset {
		jc.Progress(2, "applying the "+def.Name+" preset")
		if err := s.jobSiteApply(ctx, jc); err != nil {
			return fmt.Errorf("apply the preset: %w", err)
		}
		site, err = s.db.GetSite(ctx, p.SiteID)
		if err != nil {
			return err
		}
	}

	jc.Progress(10, "checking the docroot")
	rel := strings.TrimPrefix(l.docroot, l.home+"/")
	out, err := s.fsop(ctx, owner, nil, "list", rel)
	if err != nil {
		return fmt.Errorf("list the docroot: %w", err)
	}
	var entries []apitypes.FileEntry
	_ = json.Unmarshal(out, &entries)
	if len(entries) > 0 {
		if !p.Force {
			return fmt.Errorf("the docroot %s is not empty (%d entries); install with force to replace its files", l.docroot, len(entries))
		}
		args := []string{"rm"}
		for _, e := range entries {
			args = append(args, path.Join(rel, e.Name))
		}
		// a site still serving requests keeps writing cache and session files
		// under the tree being removed: try again rather than fail on the race
		var rmErr error
		for attempt := 0; attempt < 3; attempt++ {
			if _, rmErr = s.fsop(ctx, owner, nil, args...); rmErr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if rmErr != nil {
			return fmt.Errorf("empty the docroot: %w", rmErr)
		}
		jc.Logf("docroot emptied: %d entries removed", len(entries))
	}

	jc.Progress(15, "database "+p.Database)
	inst, err := s.dbInstance(ctx)
	if err != nil {
		return err
	}
	if p.Force {
		// a forced reinstall starts from an empty database, as it starts from an empty docroot
		if _, err := s.mysqlExec(ctx, "DROP DATABASE IF EXISTS `"+p.Database+"`;\n"); err != nil {
			return fmt.Errorf("drop database %s: %w", p.Database, err)
		}
	}
	if _, _, err := s.createDatabase(ctx, inst, owner, p.Database, dbPassword, nil); err != nil {
		return fmt.Errorf("database %s: %w", p.Database, err)
	}
	jc.Logf("database %s and its account are ready", p.Database)

	jc.Progress(25, "downloading "+def.Name)
	version, err := s.cmsDeliver(ctx, jc, def, p.Edition, l, owner.Login)
	if err != nil {
		return err
	}

	jc.Progress(60, "running the "+def.Name+" installer")
	siteURL := s.siteURL(ctx, site)
	ins := cmsInstall{def: def, site: site, owner: owner, layout: l, rel: rel, url: siteURL, title: p.Title, login: p.AdminLogin, password: adminPassword, email: p.AdminEmail, db: p.Database, dbPassword: dbPassword, edition: p.Edition, solution: p.Solution}
	switch def.ID {
	case "wordpress":
		version, err = s.cmsInstallWordPress(ctx, jc, ins)
	case "joomla":
		err = s.cmsInstallJoomla(ctx, jc, ins)
	case "opencart":
		err = s.cmsInstallOpenCart(ctx, jc, ins)
	case "bitrix":
		err = s.cmsInstallBitrix(ctx, jc, ins)
	}
	if err != nil {
		return err
	}
	s.relabel(ctx, nil, l.docroot, true)

	site.CMS, site.CMSVersion, site.CMSAt = def.ID, version, time.Now().UTC().Format(time.RFC3339)
	if err := s.db.UpdateSite(ctx, site); err != nil {
		return err
	}
	admin := siteURL + "/" + def.AdminPath
	jc.Logf("administrator %s signs in at %s", p.AdminLogin, admin)
	jc.Progress(100, fmt.Sprintf("%s %s installed: %s", def.Name, version, admin))
	return nil
}

// cmsDeliver streams the distribution from its vendor into the docroot:
// tar on the agent unpacks it as root (no memory for a 300 MB Bitrix), the
// client then owns every file and SELinux labels are put right.
func (s *Server) cmsDeliver(ctx context.Context, jc *jobs.Context, def *cmsDef, edition string, l *siteLayout, login string) (string, error) {
	var (
		src, version string
		strip        int
		isZip        bool
	)
	switch def.ID {
	case "wordpress":
		src, strip = "https://wordpress.org/latest.tar.gz", 1
	case "joomla":
		tag, err := cmsLatestTag(ctx, "joomla/joomla-cms")
		if err != nil {
			return "", err
		}
		src, version = fmt.Sprintf("https://github.com/joomla/joomla-cms/releases/download/%s/Joomla_%s-Stable-Full_Package.tar.gz", tag, tag), tag
	case "opencart":
		tag, err := cmsLatestTag(ctx, "opencart/opencart")
		if err != nil {
			return "", err
		}
		// the release zip has no top folder: the shop is its upload/ directory
		src, version, strip, isZip = fmt.Sprintf("https://github.com/opencart/opencart/releases/download/%s/opencart-%s.zip", tag, tag), tag, 1, true
	case "bitrix":
		if edition == "" {
			edition = "start"
		}
		src, version = "https://www.1c-bitrix.ru/download/files/"+edition+"_encode.tar.gz", edition
	}
	jc.Logf("source: %s", src)
	body, err := cmsFetch(ctx, src)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", def.Name, err)
	}
	defer body.Close()
	var rd io.Reader = body
	args := []string{"-xzf", "-", "-C", l.docroot, "--no-same-owner", "--warning=no-timestamp"}
	members := []string{}
	if def.ID == "opencart" {
		members = []string{"upload"}
	}
	if isZip {
		// tar cannot read a zip from a pipe: the zip is transcoded to a tar stream on the fly
		data, err := io.ReadAll(io.LimitReader(body, 256<<20))
		if err != nil {
			return "", fmt.Errorf("download %s: %w", def.Name, err)
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", fmt.Errorf("%s: not a zip archive: %w", def.Name, err)
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(zipToTar(zr, pw)) }()
		rd = pr
		args = []string{"-xf", "-", "-C", l.docroot, "--no-same-owner", "--warning=no-timestamp"}
	}
	if strip > 0 {
		args = append(args, fmt.Sprintf("--strip-components=%d", strip))
	}
	args = append(args, members...) // only these members, when named
	res, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "tar", Args: args, TimeoutSeconds: 3600}, rd)
	if err != nil {
		return "", fmt.Errorf("unpack %s: %w", def.Name, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("unpack %s: tar exit %d: %s", def.Name, res.ExitCode, strings.TrimSpace(res.Output))
	}
	ch, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: l.docroot, Owner: login, Group: login, Recursive: true})
	if err != nil {
		return "", err
	}
	jc.Logf("%s unpacked into %s, %d objects handed to %s", def.Name, l.docroot, ch.Changed, login)
	s.relabel(ctx, jc, l.docroot, true)
	return version, nil
}

// zipToTar rewrites a zip's entries as a tar stream.
func zipToTar(zr *zip.Reader, w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, f := range zr.File {
		hdr := &tar.Header{Name: f.Name, ModTime: f.Modified, Mode: int64(f.Mode().Perm())}
		if f.FileInfo().IsDir() {
			hdr.Typeflag = tar.TypeDir
			if hdr.Mode == 0 {
				hdr.Mode = 0o755
			}
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(f.UncompressedSize64)
			if hdr.Mode == 0 {
				hdr.Mode = 0o644
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// cmsInstall carries what the installers need.
type cmsInstall struct {
	def                                                                        *cmsDef
	site                                                                       *store.Site
	owner                                                                      *store.User
	layout                                                                     *siteLayout
	rel, url, title, login, password, email, db, dbPassword, edition, solution string
}

// run executes the CMS's own installer as the client, in the docroot.
func (s *Server) cmsRun(ctx context.Context, in cmsInstall, argv ...string) (string, error) {
	args := append([]string{"run", "--cwd", in.rel, "--", in.layout.php.CLIBinary}, argv...)
	out, err := s.fsop(ctx, in.owner, nil, args...)
	return string(out), err
}

// ensureWPCLI keeps wp-cli where every site can run it, checked against the
// checksum wp-cli publishes next to it.
func (s *Server) ensureWPCLI(ctx context.Context, jc *jobs.Context) error {
	if st, err := s.agent.Stat(ctx, wpCLIPhar); err == nil && len(st.Entries) == 1 && st.Entries[0].Exists && st.Entries[0].Size > 1<<20 {
		return nil
	}
	jc.Logf("downloading wp-cli")
	phar, err := cmsFetchBytes(ctx, wpCLIURL, 32<<20)
	if err != nil {
		return fmt.Errorf("download wp-cli: %w", err)
	}
	sums, err := cmsFetchBytes(ctx, wpCLIURL+".sha512", 4096)
	if err != nil {
		return fmt.Errorf("download the wp-cli checksum: %w", err)
	}
	want := strings.Fields(string(sums))
	sum := sha512.Sum512(phar)
	if len(want) == 0 || !strings.EqualFold(want[0], hex.EncodeToString(sum[:])) {
		return errors.New("wp-cli checksum mismatch: refusing to install it")
	}
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: path.Dir(wpCLIPhar), Mode: 0o755, Owner: "root", Group: "root"}}}); err != nil {
		return err
	}
	// a system file, not a client's: it goes the way composer's phar does
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: wpCLIPhar, ContentBase64: base64.StdEncoding.EncodeToString(phar), Mode: 0o644}}, Origin: "wp-cli"}); err != nil {
		return fmt.Errorf("install wp-cli: %w", err)
	}
	return nil
}

func (s *Server) cmsInstallWordPress(ctx context.Context, jc *jobs.Context, in cmsInstall) (string, error) {
	if err := s.ensureWPCLI(ctx, jc); err != nil {
		return "", err
	}
	wp := func(args ...string) (string, error) {
		return s.cmsRun(ctx, in, append([]string{wpCLIPhar, "--path=" + in.layout.docroot}, args...)...)
	}
	if _, err := wp("config", "create", "--dbname="+in.db, "--dbuser="+in.db, "--dbpass="+in.dbPassword, "--dbhost=localhost", "--skip-check"); err != nil {
		return "", fmt.Errorf("wp config create: %w", err)
	}
	if _, err := wp("core", "install", "--url="+in.url, "--title="+in.title, "--admin_user="+in.login, "--admin_password="+in.password, "--admin_email="+in.email, "--skip-email"); err != nil {
		return "", fmt.Errorf("wp core install: %w", err)
	}
	version, _ := wp("core", "version")
	jc.Logf("WordPress %s installed with wp-cli", strings.TrimSpace(version))
	return strings.TrimSpace(version), nil
}

func (s *Server) cmsInstallJoomla(ctx context.Context, jc *jobs.Context, in cmsInstall) error {
	if _, err := s.cmsRun(ctx, in, "installation/joomla.php", "install", "--site-name="+in.title, "--admin-user=Administrator", "--admin-username="+in.login, "--admin-password="+in.password, "--admin-email="+in.email,
		"--db-type=mysqli", "--db-host=localhost", "--db-user="+in.db, "--db-pass="+in.dbPassword, "--db-name="+in.db, "--db-prefix=j_", "--db-encryption=0", "--no-interaction"); err != nil {
		return fmt.Errorf("joomla installer: %w", err)
	}
	if _, err := s.fsop(ctx, in.owner, nil, "rm", path.Join(in.rel, "installation")); err != nil {
		jc.Logf("warning: installation/ was not removed: %v", err)
	}
	jc.Logf("Joomla installed with its CLI installer, installation/ removed")
	return nil
}

func (s *Server) cmsInstallOpenCart(ctx context.Context, jc *jobs.Context, in cmsInstall) error {
	for _, f := range []string{"config", "admin/config"} {
		dist, err := s.fsop(ctx, in.owner, nil, "read", path.Join(in.rel, f+"-dist.php"))
		if err != nil {
			return fmt.Errorf("opencart %s-dist.php: %w", f, err)
		}
		if _, err := s.fsop(ctx, in.owner, dist, "write", path.Join(in.rel, f+".php")); err != nil {
			return fmt.Errorf("opencart %s.php: %w", f, err)
		}
	}
	out, err := s.cmsRun(ctx, in, "install/cli_install.php", "install", "--username", in.login, "--email", in.email, "--password", in.password, "--http_server", in.url+"/",
		"--db_driver", "mysqli", "--db_hostname", "localhost", "--db_username", in.db, "--db_password", in.dbPassword, "--db_database", in.db, "--db_port", "3306", "--db_prefix", "oc_")
	if err != nil {
		return fmt.Errorf("opencart installer: %w", err)
	}
	if !strings.Contains(strings.ToLower(out), "success") {
		return fmt.Errorf("opencart installer: %s", snippet(strings.TrimSpace(out), 300))
	}
	if _, err := s.fsop(ctx, in.owner, nil, "rm", path.Join(in.rel, "install")); err != nil {
		jc.Logf("warning: install/ was not removed: %v", err)
	}
	jc.Logf("OpenCart installed with its CLI installer, install/ removed")
	return nil
}

// cmsSolutionRe accepts the two words and a marketplace id (vendor.solution).
var cmsSolutionRe = regexp.MustCompile(`^(clean|demo|[a-z0-9_]+\.[a-z0-9_.]+)$`)

// bitrixCleanSolution is the marketplace's «Чистая установка «1С-Битрикс»»:
// the edition without a demo site, the way most sites start.
const bitrixCleanSolution = "nsandrey.emptyinstall"

func (s *Server) cmsInstallBitrix(ctx context.Context, jc *jobs.Context, in cmsInstall) error {
	overrides := map[string]string{
		"__wiz_agree_license": "Y", "__wiz_lic_key_variant": "Y",
		"__wiz_user_name": "Site", "__wiz_user_surname": "Administrator", "__wiz_email": in.email,
		"__wiz_host": "localhost", "__wiz_create_user": "N", "__wiz_user": in.db, "__wiz_password": in.dbPassword,
		"__wiz_create_database": "N", "__wiz_database": in.db,
		"__wiz_login": in.login, "__wiz_admin_password": in.password, "__wiz_admin_password_confirm": in.password, "__wiz_admin_email": in.email,
		// the solution wizards ask for the site's name and title: the site's own
		"__wiz_siteName": in.title, "__wiz_siteMetaTitle": in.title,
	}
	// The solution: the marketplace's clean install unless the demo site or
	// another marketplace solution was asked for. "@" is the wizard's own
	// name for "load from the marketplace"; the solution id is chosen on the
	// next step.
	solution, what := in.solution, ""
	switch solution {
	case "", "clean":
		solution, what = bitrixCleanSolution, "the marketplace's clean install"
	case "demo":
		solution, what = "", "the demo site bundled with the edition"
	default:
		what = "marketplace solution " + solution
	}
	if solution != "" {
		overrides["__wiz_selected_wizard"] = "@"
		overrides["__wiz_selected_module"] = solution
	}
	w := newBitrixWizard(in.url, in.site.Domain, in.site.IP, overrides, jc)
	if err := w.run(ctx); err != nil {
		return err
	}
	jc.Logf("Bitrix installed through its web wizard: trial licence, %s", what)
	return nil
}
