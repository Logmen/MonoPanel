package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const (
	suryKeyURL     = "https://packages.sury.org/php/apt.gpg"
	ondrejKeyURL   = "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x14AA40EC0831756756D7F66C4F4EA0AAE5267A6C"
	remiReleaseURL = "https://rpms.remirepo.net/enterprise/remi-release-%s.rpm"
	settingPHPRepo = "php.repo"
	settingTZ      = "php.timezone"
)

type phpListOutput struct {
	Body apitypes.PHPVersions
}

type phpInstallInput struct {
	Body apitypes.PHPInstallRequest
}

type phpVersionInput struct {
	Version string `path:"version" pattern:"^[578]\\.[0-9]$"`
}

type phpPayload struct {
	Version string `json:"version"`
}

func (s *Server) registerPHP() {
	huma.Register(s.api, huma.Operation{
		OperationID: "php-list", Method: http.MethodGet, Path: "/php/versions", Summary: "Installed PHP branches and the availability matrix", Tags: []string{"php"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*phpListOutput, error) {
		installed, err := s.db.ListPHPVersions(ctx)
		if err != nil {
			return nil, err
		}
		if installed == nil {
			installed = []*store.PHPVersion{}
		}
		return &phpListOutput{Body: apitypes.PHPVersions{Installed: installed, Available: s.phpAvailable(ctx)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "php-install", Method: http.MethodPost, Path: "/php/versions", Summary: "Install a PHP branch with FPM and the standard extensions (async)", Tags: []string{"php"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *phpInstallInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		v := in.Body.Version
		if !osprofile.ValidPHPVersion(v) {
			return nil, huma.Error422UnprocessableEntity("unknown PHP branch " + v)
		}
		for _, info := range s.phpAvailable(ctx) {
			if info.Version == v && !info.Available {
				return nil, huma.Error422UnprocessableEntity("PHP " + v + " is not available on this OS: " + info.Note)
			}
		}
		layout := osprofile.PHP(s.profile, v)
		if layout == nil {
			return nil, huma.Error422UnprocessableEntity("no PHP package source for this OS")
		}
		if existing, err := s.db.GetPHPVersion(ctx, v); err == nil && existing.Status == store.PHPInstalled {
			return nil, huma.Error409Conflict("PHP " + v + " is already installed")
		}
		row := &store.PHPVersion{Version: v, Source: layout.Source, Status: store.PHPInstalling, FPMService: layout.FPMService, FPMBinary: layout.FPMBinary, CLIBinary: layout.CLIBinary, PoolDir: layout.PoolDir}
		if err := s.db.UpsertPHPVersion(ctx, row); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "php.install", phpPayload{Version: v}, jobs.WithLockKey("php"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "php.install", Target: v, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "php-remove", Method: http.MethodDelete, Path: "/php/versions/{version}", Summary: "Remove a PHP branch (no site may use it)", Tags: []string{"php"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *phpVersionInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		if _, err := s.db.GetPHPVersion(ctx, in.Version); errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("PHP " + in.Version + " is not installed")
		} else if err != nil {
			return nil, err
		}
		if n, _ := s.db.CountSitesByPHP(ctx, in.Version); n > 0 {
			return nil, huma.Error409Conflict(fmt.Sprintf("%d site(s) still use PHP %s", n, in.Version))
		}
		s.db.SetPHPStatus(ctx, in.Version, store.PHPRemoving, "") //nolint:errcheck // best effort; the caller reports the real failure
		job, err := s.jobs.Enqueue(ctx, "php.remove", phpPayload{Version: in.Version}, jobs.WithLockKey("php"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "php.remove", Target: in.Version, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

// phpRepoDistro is the setting value for an Ubuntu whose release the ondrej
// PPA does not build for yet: only Ubuntu's own PHP packages are there, and
// the PPA is probed again at the next installation.
const phpRepoDistro = "distro"

// ppaHasRelease reports whether ppa:ondrej/php publishes packages for an
// Ubuntu codename. Tests replace it.
var ppaHasRelease = func(ctx context.Context, codename string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://ppa.launchpadcontent.net/ondrej/php/ubuntu/dists/"+codename+"/Release", nil)
	if err != nil {
		return false, err
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return false, err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusOK:
		return true, nil
	case res.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: %s", req.URL, res.Status)
	}
}

// ensurePHPRepo configures Sury (Debian), the ondrej PPA (Ubuntu) or Remi (EL)
// once and remembers it in settings. A fresh Ubuntu release has no PPA builds
// for a while; then the panel stays with Ubuntu's own packages and looks
// again next time instead of writing a source that apt cannot read.
func (s *Server) ensurePHPRepo(ctx context.Context, jc *jobs.Context) error {
	rel := s.profile.Release()
	key := settingPHPRepo + "." + string(s.profile.Family())
	if v, _ := s.db.GetSetting(ctx, key); v == "ready" {
		return nil
	}
	jc.Progress(5, "PHP repository")
	switch s.profile.Family() {
	case osprofile.FamilyDebian:
		var files []agent.FileSpec
		if strings.EqualFold(rel.ID, "ubuntu") {
			if ok, err := ppaHasRelease(ctx, rel.Codename); err != nil {
				jc.Logf("cannot check ppa:ondrej/php for %s (%v), trying it anyway", rel.Codename, err)
			} else if !ok {
				jc.Logf("ppa:ondrej/php has no packages for Ubuntu %s (%s) yet: using Ubuntu's own PHP", rel.VersionID, rel.Codename)
				// An earlier attempt may have left the source behind, and
				// with it every apt-get update fails.
				if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{"/etc/apt/sources.list.d/ondrej-php.list"}}); err != nil {
					jc.Logf("cannot remove the stale PPA source: %v", err)
				}
				return s.db.SetSetting(ctx, key, phpRepoDistro)
			}
			keyPEM, err := fetchText(ctx, ondrejKeyURL)
			if err != nil {
				return fmt.Errorf("download ondrej PPA key: %w", err)
			}
			if !strings.Contains(keyPEM, "BEGIN PGP PUBLIC KEY BLOCK") {
				return errors.New("unexpected content from keyserver.ubuntu.com")
			}
			files = []agent.FileSpec{
				{Path: "/etc/apt/keyrings/ondrej-php.asc", Content: keyPEM, Mode: 0o644},
				{Path: "/etc/apt/sources.list.d/ondrej-php.list", Content: fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/ondrej-php.asc] https://ppa.launchpadcontent.net/ondrej/php/ubuntu %s main\n", rel.Codename), Mode: 0o644},
			}
			jc.Logf("repository: ppa:ondrej/php (%s)", rel.Codename)
		} else {
			keyBin, err := fetchBytes(ctx, suryKeyURL)
			if err != nil {
				return fmt.Errorf("download packages.sury.org key: %w", err)
			}
			files = []agent.FileSpec{
				{Path: "/etc/apt/keyrings/sury-php.gpg", ContentBase64: base64.StdEncoding.EncodeToString(keyBin), Mode: 0o644},
				{Path: "/etc/apt/sources.list.d/sury-php.list", Content: fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ %s main\n", rel.Codename), Mode: 0o644},
			}
			jc.Logf("repository: packages.sury.org/php (%s)", rel.Codename)
		}
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "php:repo"}); err != nil {
			return err
		}
		res, err := s.agent.Pkg(ctx, "update-index")
		if err != nil {
			return err
		}
		logTail(jc, res.Output, 2)
	case osprofile.FamilyRHEL:
		if _, err := s.agent.Pkg(ctx, "install", s.profile.EPELPackage(), fmt.Sprintf(remiReleaseURL, rel.MajorVersion())); err != nil {
			return err
		}
		jc.Logf("repository: remi-release-%s + %s", rel.MajorVersion(), s.profile.EPELPackage())
	default:
		return errors.New("no PHP package source for this OS")
	}
	return s.db.SetSetting(ctx, key, "ready")
}

// phpPackagesExist fails with a readable reason when no repository carries
// the branch: apt's own "Unable to locate package" says nothing about why.
func (s *Server) phpPackagesExist(ctx context.Context, layout *osprofile.PHPLayout) error {
	res, err := s.agent.Pkg(ctx, "available", layout.FPMPackage)
	if err != nil {
		return err
	}
	if _, ok := res.Available[layout.FPMPackage]; ok {
		return nil
	}
	return fmt.Errorf("PHP %s is not available on this OS: %s", layout.Version, s.phpUnavailableNote(ctx, layout.Version))
}

// phpUnavailableNote explains a missing branch: on an Ubuntu the PPA does not
// build for yet it names what Ubuntu itself ships.
func (s *Server) phpUnavailableNote(ctx context.Context, version string) string {
	rel := s.profile.Release()
	if v, _ := s.db.GetSetting(ctx, settingPHPRepo+"."+string(s.profile.Family())); v == phpRepoDistro {
		native := ""
		if res, err := s.agent.Pkg(ctx, "available", "php"); err == nil {
			// Ubuntu's meta package is versioned like 2:8.5+99ubuntu1.
			if _, ver, ok := strings.Cut(res.Available["php"], ":"); ok {
				native, _, _ = strings.Cut(ver, "+")
			}
		}
		note := "ppa:ondrej/php has no packages for Ubuntu " + rel.VersionID + " (" + rel.Codename + ") yet"
		if native != "" {
			note += "; Ubuntu itself ships only PHP " + native
		}
		return note
	}
	return "no configured repository carries php" + version + " packages"
}

// phpAvailable is the availability matrix for this host. On an Ubuntu that
// runs without the PPA it asks apt which branches actually exist.
func (s *Server) phpAvailable(ctx context.Context) []osprofile.PHPVersionInfo {
	list := osprofile.PHPVersions(s.profile)
	if v, _ := s.db.GetSetting(ctx, settingPHPRepo+"."+string(s.profile.Family())); v != phpRepoDistro {
		return list
	}
	pkgs := make([]string, 0, len(list))
	for _, info := range list {
		pkgs = append(pkgs, "php"+info.Version+"-fpm")
	}
	res, err := s.agent.Pkg(ctx, "available", pkgs...)
	if err != nil {
		return list
	}
	note := s.phpUnavailableNote(ctx, "")
	for i := range list {
		if _, ok := res.Available["php"+list[i].Version+"-fpm"]; !ok && list[i].Available {
			list[i].Available = false
			list[i].Note = note
		}
	}
	return list
}

func (s *Server) phpFail(ctx context.Context, version string, err error) error {
	_ = s.db.SetPHPStatus(context.WithoutCancel(ctx), version, store.PHPError, err.Error())
	return err
}

func (s *Server) jobPHPInstall(ctx context.Context, jc *jobs.Context) error {
	var p phpPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	layout := osprofile.PHP(s.profile, p.Version)
	if layout == nil {
		return s.phpFail(ctx, p.Version, errors.New("no PHP package source for this OS"))
	}
	if err := s.selinuxHostingPolicy(ctx, jc); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	if err := s.ensurePHPRepo(ctx, jc); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	if err := s.phpPackagesExist(ctx, layout); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	jc.Progress(20, "installing PHP "+p.Version+" packages")
	res, err := s.agent.Pkg(ctx, "install", layout.CorePackages...)
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	logTail(jc, res.Output, 3)
	jc.Logf("core: %s", strings.Join(layout.CorePackages, " "))
	jc.Progress(55, "optional extensions")
	skipped := []string{}
	for _, pkg := range layout.ExtraPackages {
		if _, err := s.agent.Pkg(ctx, "install", pkg); err != nil {
			skipped = append(skipped, pkg)
		}
	}
	if len(skipped) > 0 {
		jc.Logf("not available for this branch, skipped: %s", strings.Join(skipped, " "))
	}

	jc.Progress(75, "configuration")
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: path.Join(s.cfg.RunDir, "php"), Mode: 0o755, Owner: "root", Group: "root"}}}); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	tz, _ := s.db.GetSetting(ctx, settingTZ)
	if tz == "" {
		tz = "UTC"
	}
	ini, err := s.render.Render("php/monopanel.ini.tmpl", render.PHPIni{Version: p.Version, Timezone: tz, OpcacheMemory: 128})
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	files := []agent.FileSpec{{Path: "/etc/tmpfiles.d/monopanel-php.conf", Content: fmt.Sprintf("d %s 0755 root root -\n", path.Join(s.cfg.RunDir, "php")), Mode: 0o644}}
	for _, dir := range layout.IniDirs {
		files = append(files, agent.FileSpec{Path: path.Join(dir, "99-monopanel.ini"), Content: ini, Mode: 0o644})
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{layout.FPMCheckArgv}, Reload: []string{layout.FPMService}, Force: true, Origin: "php:" + p.Version}); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	if _, err := s.agent.Service(ctx, layout.FPMService, "enable"); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	st, err := s.agent.Service(ctx, layout.FPMService, "status")
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	jc.Logf("%s: %s (%s)", layout.FPMService, st.Status.ActiveState, st.Status.SubState)

	jc.Progress(90, "recording")
	all := append(append([]string{}, layout.CorePackages...), layout.ExtraPackages...)
	q, _ := s.agent.Pkg(ctx, "query", all...)
	exts := []string{}
	pkgVersion := ""
	for pkg, ver := range q.Installed {
		if pkg == layout.FPMPackage {
			pkgVersion = ver
		}
		if name := extensionName(pkg, p.Version); name != "" {
			exts = append(exts, name)
		}
	}
	row := &store.PHPVersion{Version: p.Version, Source: layout.Source, Status: store.PHPInstalled, FPMService: layout.FPMService, FPMBinary: layout.FPMBinary, CLIBinary: layout.CLIBinary, PoolDir: layout.PoolDir, PackageVersion: pkgVersion, Extensions: exts}
	if err := s.db.UpsertPHPVersion(context.WithoutCancel(ctx), row); err != nil {
		return err
	}
	jc.Logf("PHP %s (%s): %d extension packages", p.Version, pkgVersion, len(exts))
	jc.Progress(100, "PHP "+p.Version+" ready")
	return nil
}

// extensionName maps a package name to its extension ("php8.4-gd" -> "gd",
// "php84-php-pecl-redis6" -> "redis"). fpm/cli/common are not extensions.
func extensionName(pkg, version string) string {
	short := "php" + strings.ReplaceAll(version, ".", "")
	name := ""
	switch {
	case strings.HasPrefix(pkg, "php"+version+"-"):
		name = strings.TrimPrefix(pkg, "php"+version+"-")
	case strings.HasPrefix(pkg, short+"-php-"):
		name = strings.TrimPrefix(pkg, short+"-php-")
		name = strings.TrimPrefix(name, "pecl-")
		name = strings.TrimRight(name, "0123456789")
	default:
		return ""
	}
	switch name {
	case "fpm", "cli", "common", "process", "pdo", "":
		return ""
	case "mysqlnd":
		return "mysql"
	}
	return name
}

func (s *Server) jobPHPRemove(ctx context.Context, jc *jobs.Context) error {
	var p phpPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	layout := osprofile.PHP(s.profile, p.Version)
	if layout == nil {
		return errors.New("no PHP layout for this OS")
	}
	all := append(append([]string{}, layout.CorePackages...), layout.ExtraPackages...)
	q, err := s.agent.Pkg(ctx, "query", all...)
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	present := make([]string, 0, len(q.Installed))
	for pkg := range q.Installed {
		present = append(present, pkg)
	}
	if len(present) > 0 {
		jc.Progress(20, "removing packages")
		res, err := s.agent.Pkg(ctx, "remove", present...)
		if err != nil {
			return s.phpFail(ctx, p.Version, err)
		}
		logTail(jc, res.Output, 3)
	}
	if err := s.db.DeletePHPVersion(context.WithoutCancel(ctx), p.Version); err != nil {
		return err
	}
	jc.Progress(100, "PHP "+p.Version+" removed")
	return nil
}

func phpLayout(s *Server, version string) *osprofile.PHPLayout {
	return osprofile.PHP(s.profile, version)
}
