package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	suryKeyURL = "https://packages.sury.org/php/apt.gpg"
	// ondrejKeyFingerprint is the key Launchpad made for ppa:ondrej/php in
	// 2024 (rsa4096, the PPA's signing_key_fingerprint). The PPA's InRelease
	// still carries a signature by its 2012 key as well (rsa1024,
	// 14AA40EC0831756756D7F66C4F4EA0AAE5267A6C), which apt 3 on Ubuntu 26.04
	// refuses ("untrusted public key algorithm"): with only that key in the
	// keyring every apt-get update there fails, and older apt versions
	// accept the new key just as well.
	ondrejKeyFingerprint = "B8DC7E53946656EFBCE4C1DD71DAEAAB4AD4CAB6"
	ondrejKeyURL         = "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x" + ondrejKeyFingerprint
	ondrejKeyring        = "/etc/apt/keyrings/ondrej-php.asc"
	ondrejSource         = "/etc/apt/sources.list.d/ondrej-php.list"
	remiReleaseURL       = "https://rpms.remirepo.net/enterprise/remi-release-%s.rpm"
	settingPHPRepo       = "php.repo"
	settingTZ            = "php.timezone"
	// settingOndrejKey holds the fingerprint of the key the panel last put
	// into the PPA's keyring.
	settingOndrejKey = "php.repo.ondrej_key"
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

// ppaClient asks Launchpad and packages.sury.org; SetOutbound replaces it.
var ppaClient = &http.Client{Timeout: 20 * time.Second}

// ppaHasRelease reports whether ppa:ondrej/php publishes packages for an
// Ubuntu codename. Tests replace it.
var ppaHasRelease = func(ctx context.Context, codename string) (bool, error) {
	return repoHasRelease(ctx, "https://ppa.launchpadcontent.net/ondrej/php/ubuntu/dists/"+codename+"/Release")
}

// suryHasRelease reports whether packages.sury.org/php, the same maintainer's
// own repository, publishes a codename. For a new Ubuntu it usually does long
// before the PPA (Ubuntu 26.04: all branches 5.6–8.6 while the PPA had none),
// and it is signed with the key the panel already pins for Debian. Tests
// replace it.
var suryHasRelease = func(ctx context.Context, codename string) (bool, error) {
	return repoHasRelease(ctx, "https://packages.sury.org/php/dists/"+codename+"/Release")
}

func repoHasRelease(ctx context.Context, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	res, err := ppaClient.Do(req)
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
		useSury := !strings.EqualFold(rel.ID, "ubuntu")
		if !useSury {
			if ok, err := ppaHasRelease(ctx, rel.Codename); err != nil {
				jc.Logf("cannot check ppa:ondrej/php for %s (%v), trying it anyway", rel.Codename, err)
			} else if !ok {
				// An earlier attempt may have left the source behind, and
				// with it every apt-get update fails.
				if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{"/etc/apt/sources.list.d/ondrej-php.list"}}); err != nil {
					jc.Logf("cannot remove the stale PPA source: %v", err)
				}
				if has, err := suryHasRelease(ctx, rel.Codename); err == nil && has {
					jc.Logf("ppa:ondrej/php has no packages for Ubuntu %s (%s) yet; packages.sury.org/php does: using it", rel.VersionID, rel.Codename)
					useSury = true
				} else {
					if err != nil {
						jc.Logf("cannot check packages.sury.org/php for %s: %v", rel.Codename, err)
					}
					jc.Logf("neither ppa:ondrej/php nor packages.sury.org/php has packages for Ubuntu %s (%s) yet: using Ubuntu's own PHP", rel.VersionID, rel.Codename)
					return s.db.SetSetting(ctx, key, phpRepoDistro)
				}
			}
		}
		if !useSury {
			keyPEM, err := ondrejKey(ctx)
			if err != nil {
				return err
			}
			files = []agent.FileSpec{
				{Path: ondrejKeyring, Content: keyPEM, Mode: 0o644},
				{Path: ondrejSource, Content: fmt.Sprintf("deb [signed-by=%s] https://ppa.launchpadcontent.net/ondrej/php/ubuntu %s main\n", ondrejKeyring, rel.Codename), Mode: 0o644},
			}
			jc.Logf("repository: ppa:ondrej/php (%s)", rel.Codename)
		}
		if useSury {
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
		if !useSury {
			// Spares the start-up refresh a download it does not need.
			s.db.SetSetting(ctx, settingOndrejKey, ondrejKeyFingerprint) //nolint:errcheck // the refresh only downloads the key once more
		}
		res, err := s.agent.Pkg(ctx, "update-index")
		if err != nil {
			// The source just written is the likely cause (a key apt
			// refuses, a repository that went away): left behind, it would
			// fail every later update-index on the host, a mail or nginx
			// install included. The next PHP install writes it again.
			var srcs []string
			for _, f := range files {
				if strings.HasPrefix(f.Path, "/etc/apt/sources.list.d/") {
					srcs = append(srcs, f.Path)
				}
			}
			if _, rerr := s.agent.RemovePaths(context.WithoutCancel(ctx), &agent.RemovePathsRequest{Paths: srcs}); rerr != nil {
				jc.Logf("cannot remove the repository source after the failed index update: %v", rerr)
			} else {
				jc.Logf("repository source %s removed again: apt could not read it", strings.Join(srcs, ", "))
			}
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
	return &phpUnavailableError{fmt.Sprintf("PHP %s is not available on this OS: %s", layout.Version, s.phpUnavailableNote(ctx, layout.Version))}
}

// phpUnavailableError marks a branch no repository of this OS carries: the
// job fails, and the row is removed instead of staying in error — there is
// nothing to retry.
type phpUnavailableError struct{ msg string }

func (e *phpUnavailableError) Error() string { return e.msg }

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
		note := "neither ppa:ondrej/php nor packages.sury.org/php has packages for Ubuntu " + rel.VersionID + " (" + rel.Codename + ") yet"
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
	// Once Sury publishes this Ubuntu the whole matrix is installable again:
	// the next installation re-probes and switches the host to it.
	if s.suryCovers(ctx) {
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

// suryCovers remembers for an hour whether packages.sury.org/php publishes
// this host's codename: the PHP page asks on every load, the answer changes
// once in a release's lifetime.
func (s *Server) suryCovers(ctx context.Context) bool {
	codename := s.profile.Release().Codename
	suryProbe.mu.Lock()
	defer suryProbe.mu.Unlock()
	if suryProbe.codename == codename && time.Since(suryProbe.at) < time.Hour {
		return suryProbe.ok
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ok, err := suryHasRelease(pctx, codename)
	if err != nil {
		return false // unknown: keep what apt says, ask again next time
	}
	suryProbe.codename, suryProbe.ok, suryProbe.at = codename, ok, time.Now()
	return ok
}

var suryProbe struct {
	mu       sync.Mutex
	codename string
	ok       bool
	at       time.Time
}

func (s *Server) phpFail(ctx context.Context, version string, err error) error {
	var unavailable *phpUnavailableError
	if errors.As(err, &unavailable) {
		_ = s.db.DeletePHPVersion(context.WithoutCancel(ctx), version)
		return err
	}
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
	s.installPHPExtras(ctx, jc, layout)

	jc.Progress(75, "configuration")
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: path.Join(s.cfg.RunDir, "php"), Mode: 0o755, Owner: "root", Group: "root"}}}); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	ini, err := s.phpIniFiles(ctx, layout, false)
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	// The distribution's own pool goes idle before php-fpm runs it again, on
	// EL with the master's log in syslog (php_pools.go).
	idle, err := s.idlePHPPool(layout)
	if err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	// The socket directory is made at boot by the package's and mp setup's
	// own tmpfiles file, and by the agent at its start: the API writes no
	// tmpfiles line (/etc/tmpfiles.d is not on the agent's allow-list).
	files := append([]agent.FileSpec{idle}, ini...)
	jc.Logf("the distribution's pool %s: idle (nobody, a socket of root's alone)", idle.Path)
	syslog := true
	if conf, err := s.fpmSyslogConf(ctx, layout); err != nil {
		jc.Logf("php-fpm.conf: the master keeps its log file (the panel's next start tries again): %v", err)
		syslog = false
	} else if conf != nil {
		files = append(files, *conf)
		jc.Logf("php-fpm master: log to syslog (%s)", layout.FPMConf)
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{layout.FPMCheckArgv}, Reload: []string{layout.FPMService}, Force: true, Origin: "php:" + p.Version}); err != nil {
		return s.phpFail(ctx, p.Version, err)
	}
	// Made so once: from now on the files are the administrator's to change
	// (php_pools.go). Without the syslog line the next start tries again.
	if syslog {
		s.markPHPDistroIdle(ctx, p.Version)
	} else if err := s.db.SetSetting(context.WithoutCancel(ctx), settingPHPDistroIdle(p.Version), ""); err != nil {
		jc.Logf("warning: %v", err)
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

// installPHPExtras installs a branch's optional extensions in one run of the
// package manager. A run that installs packages of a branch restarts its
// php-fpm (on Debian the fpm package's trigger, once at the end of the run):
// with a run per package a fresh Debian 12 restarted php8.4-fpm about every
// 1.5 s, more often than systemd's start limit lets a unit start (5 in
// 10 s), and systemd refused the panel's own reload right after. The
// repositories are asked first which of the packages they carry (old
// branches lack some); only when the one run fails are the packages
// installed one by one, so that one broken package does not keep the others
// out.
func (s *Server) installPHPExtras(ctx context.Context, jc *jobs.Context, layout *osprofile.PHPLayout) {
	extras := layout.ExtraPackages
	if len(extras) == 0 {
		return
	}
	var carried, other []string
	if res, err := s.agent.Pkg(ctx, "available", extras...); err != nil {
		// Not known: the one run says, or the runs one by one after it.
		jc.Logf("cannot ask the repositories for the optional extensions (%v): trying all of them", err)
		carried = extras
	} else {
		for _, pkg := range extras {
			if _, ok := res.Available[pkg]; ok {
				carried = append(carried, pkg)
			} else {
				other = append(other, pkg)
			}
		}
	}
	var failed []string
	if len(carried) > 0 {
		if _, err := s.agent.Pkg(ctx, "install", carried...); err == nil {
			jc.Logf("optional extensions: %s", strings.Join(carried, " "))
		} else {
			jc.Logf("the optional extensions did not install in one run (%s): installing them one by one", pkgFailure(err))
			var installed []string
			installed, failed = s.installEach(ctx, jc.Logf, carried)
			if len(installed) > 0 {
				jc.Logf("optional extensions: %s", strings.Join(installed, " "))
			}
		}
	}
	if len(other) > 0 {
		// A name no repository carries as a package of its own may be one
		// that another package provides, and apt and dnf install it by that
		// name: Ubuntu's own PHP ships php-imagick as php8.3-imagick,
		// Debian's php8.4-xml brings php8.4-xsl. Those go in one more run;
		// should a name nothing provides fail it, one by one, quietly.
		provided, missing := other, []string(nil)
		if _, err := s.agent.Pkg(ctx, "install", other...); err != nil {
			provided, missing = s.installEach(ctx, nil, other)
		}
		// An extension that is there all the same — built into a package
		// the branch has already — is no gap either.
		if len(missing) > 0 {
			have := s.phpModuleNames(ctx, layout)
			var skipped []string
			for _, pkg := range missing {
				if name := extensionName(pkg, layout.Version); name != "" && have[name] {
					provided = append(provided, pkg)
				} else {
					skipped = append(skipped, pkg)
				}
			}
			missing = skipped
		}
		if len(provided) > 0 {
			jc.Logf("provided by other packages of the branch: %s", strings.Join(provided, " "))
		}
		if len(missing) > 0 {
			jc.Logf("not available for this branch, skipped: %s", strings.Join(missing, " "))
		}
	}
	if len(failed) > 0 {
		jc.Logf("did not install, skipped: %s", strings.Join(failed, " "))
	}
}

// installEach installs packages one run each and tells which went in; logf,
// when set, gets why each of the others did not.
func (s *Server) installEach(ctx context.Context, logf func(string, ...any), pkgs []string) (installed, failed []string) {
	for _, pkg := range pkgs {
		if _, err := s.agent.Pkg(ctx, "install", pkg); err != nil {
			if logf != nil {
				logf("%s: %s", pkg, pkgFailure(err))
			}
			failed = append(failed, pkg)
			continue
		}
		installed = append(installed, pkg)
	}
	return installed, failed
}

// pkgFailure is a failed run of the package manager in one line: the
// agent's message and the last lines of the output, where apt and dnf say
// what went wrong.
func pkgFailure(err error) string {
	var ae *agent.Error
	if errors.As(err, &ae) && ae.Output != "" {
		return oneLine(ae.Message+": "+lastLinesJoin(ae.Output, 3), 400)
	}
	return oneLine(err.Error(), 400)
}

// phpModuleNames are the extensions a branch has an ini file for, installed
// with it or with another package: Debian's mods-available, the branch's
// php.d of Remi ("20-xsl.ini").
func (s *Server) phpModuleNames(ctx context.Context, layout *osprofile.PHPLayout) map[string]bool {
	el := s.profile.Family() == osprofile.FamilyRHEL
	dir := "/etc/php/" + layout.Version + "/mods-available"
	if el {
		if len(layout.IniDirs) == 0 {
			return nil
		}
		dir = layout.IniDirs[0]
	}
	res, err := s.agent.ListDir(ctx, dir)
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, e := range res.Entries {
		name, ok := strings.CutSuffix(e.Name, ".ini")
		if !ok || e.IsDir {
			continue
		}
		if _, rest, cut := strings.Cut(name, "-"); el && cut {
			name = rest
		}
		names[name] = true
	}
	return names
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

// phpIniFiles renders 99-monopanel.ini of a branch for its ini directories
// (only the CLI one with cliOnly). The copy the CLI reads opens short tags
// while a 1C-Bitrix site runs on the branch: its cron scripts and the prolog
// start with <?, and PHP would print them as text and exit 0. The FPM pools
// set short_open_tag themselves, so the shared directory of Remi is fine.
// The server-wide layer goes into these files too: the pools get it as
// php_value, but PHP from the command line — and with it every cron job,
// Bitrix agents included — reads nothing but the ini files. A timezone set
// with mp php ini set used to reach the sites and leave cron in UTC.
func (s *Server) phpIniFiles(ctx context.Context, layout *osprofile.PHPLayout, cliOnly bool) ([]agent.FileSpec, error) {
	tz, _ := s.db.GetSetting(ctx, settingTZ)
	if tz == "" {
		tz = "UTC"
	}
	global := s.globalIni(ctx)
	if v := global["date.timezone"]; v != "" {
		tz = v
	}
	data := render.PHPIni{Version: layout.Version, Timezone: tz, OpcacheMemory: 128}
	if n, err := strconv.Atoi(global["opcache.memory_consumption"]); err == nil && n > 0 {
		data.OpcacheMemory = n
	}
	globalShort := iniOn(global["short_open_tag"])
	for _, k := range slices.Sorted(maps.Keys(global)) {
		switch k {
		case "date.timezone", "opcache.memory_consumption", "short_open_tag":
			continue // written by the template's own lines
		}
		data.Global = append(data.Global, render.KV{Key: k, Value: global[k]})
	}
	var files []agent.FileSpec
	for _, dir := range layout.IniDirs {
		cli := dir == layout.CLIIniDir
		if cliOnly && !cli {
			continue
		}
		// 1C-Bitrix needs short tags from the command line too, whatever the
		// server-wide value says — as its preset wins over it in the pools.
		data.ShortOpenTag = globalShort || (cli && s.branchRunsBitrix(ctx, layout.Version))
		ini, err := s.render.Render("php/monopanel.ini.tmpl", data)
		if err != nil {
			return nil, err
		}
		files = append(files, agent.FileSpec{Path: path.Join(dir, "99-monopanel.ini"), Content: ini, Mode: 0o644})
	}
	return files, nil
}

// iniOn reads an ini boolean the way PHP does.
func iniOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// shortOpenTagRe finds the short_open_tag line of a written ini file.
var shortOpenTagRe = regexp.MustCompile(`(?m)^short_open_tag\s*=\s*(\S+)`)

// refreshPHPInis writes a branch's ini files (all of them, or the CLI copy
// only). A file FPM reads as well — Remi's shared php.d, Debian's
// fpm/conf.d — is left for the next site apply when its short_open_tag
// would change: short tags opened there for 1C-Bitrix's command line reach
// every pool of the branch that does not close them itself, and only pools
// rendered by 0.8.6 or later do. reload asks FPM to re-read what changed.
func (s *Server) refreshPHPInis(ctx context.Context, layout *osprofile.PHPLayout, cliOnly, reload bool) ([]string, error) {
	files, err := s.phpIniFiles(ctx, layout, cliOnly)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	keep := files[:0]
	var deferred []string
	for _, f := range files {
		fpmReads := path.Dir(f.Path) != layout.CLIIniDir || len(layout.IniDirs) == 1
		if fpmReads {
			if cur, err := s.agent.ReadFile(ctx, f.Path, 64<<10); err == nil && cur.Content != "" {
				was, now := shortOpenTagRe.FindStringSubmatch(cur.Content), shortOpenTagRe.FindStringSubmatch(f.Content)
				if was != nil && now != nil && iniOn(was[1]) != iniOn(now[1]) {
					deferred = append(deferred, f.Path)
					continue
				}
			}
		}
		keep = append(keep, f)
	}
	if len(deferred) > 0 {
		s.log.Info("php ini: short_open_tag changes at the next site apply", "files", deferred)
	}
	if len(keep) == 0 {
		return nil, nil
	}
	req := &agent.ApplyConfigSetRequest{Files: keep, Origin: "php:" + layout.Version}
	if reload {
		req.Validate, req.Reload = [][]string{layout.FPMCheckArgv}, []string{layout.FPMService}
	}
	res, err := s.agent.ApplyConfigSet(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Written, nil
}

func (s *Server) branchRunsBitrix(ctx context.Context, version string) bool {
	sites, _ := s.db.ListSites(ctx, 0)
	for _, site := range sites {
		if site.PHPVersion == version && site.Preset == presetBitrix && site.Mode != store.ModeProxy {
			return true
		}
	}
	return false
}

// refreshPHPCLIInis brings the CLI copies of every branch up to date at
// startup: a panel update that changed them, the server-wide layer an older
// panel did not write there. Remi's shared directory is FPM's too, so a
// change of its short_open_tag waits for a site apply (refreshPHPInis).
func (s *Server) refreshPHPCLIInis(ctx context.Context) {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return
	}
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if v.Status != store.PHPInstalled || l == nil {
			continue
		}
		if _, err := s.refreshPHPInis(ctx, l, true, false); err != nil {
			s.log.Warn("php cli ini", "version", v.Version, "err", err)
		}
	}
}

// refreshPHPAllInis rewrites every ini file of every installed branch after
// the server-wide layer changed, and reloads FPM where one changed: the
// opcache sizes are read at FPM start only, a pool cannot set them.
func (s *Server) refreshPHPAllInis(ctx context.Context) []string {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return nil
	}
	var written []string
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if v.Status != store.PHPInstalled || l == nil {
			continue
		}
		w, err := s.refreshPHPInis(ctx, l, false, true)
		if err != nil {
			s.log.Warn("php ini", "version", v.Version, "err", err)
			continue
		}
		written = append(written, w...)
	}
	return written
}

// refreshPHPCLIIni re-renders the CLI copy after a site changed; the CLI
// reads it on every start, nothing to reload.
func (s *Server) refreshPHPCLIIni(ctx context.Context, layout *osprofile.PHPLayout) error {
	files, err := s.phpIniFiles(ctx, layout, true)
	if err != nil || len(files) == 0 {
		return err
	}
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "php:" + layout.Version})
	return err
}

// ondrejKey downloads the PPA's signing key from keyserver.ubuntu.com.
var ondrejKey = func(ctx context.Context) (string, error) {
	keyPEM, err := fetchText(ctx, ondrejKeyURL)
	if err != nil {
		return "", fmt.Errorf("download ondrej PPA key: %w", err)
	}
	if !strings.Contains(keyPEM, "BEGIN PGP PUBLIC KEY BLOCK") {
		return "", errors.New("unexpected content from keyserver.ubuntu.com")
	}
	return keyPEM, nil
}

// refreshOndrejKey gives a host that took ppa:ondrej/php before 0.8.20 the
// PPA's current key, once: its keyring holds the 2012 key alone, which apt
// on Ubuntu 26.04 already refuses and the others will in time. Nothing
// happens without the PPA's source; a failed download tries again at the
// next start.
func (s *Server) refreshOndrejKey(ctx context.Context) {
	if s.profile.Family() != osprofile.FamilyDebian || !strings.EqualFold(s.profile.Release().ID, "ubuntu") {
		return
	}
	if v, _ := s.db.GetSetting(ctx, settingOndrejKey); v == ondrejKeyFingerprint {
		return
	}
	st, err := s.agent.Stat(ctx, ondrejSource)
	if err != nil || len(st.Entries) != 1 || !st.Entries[0].Exists {
		return
	}
	keyPEM, err := ondrejKey(ctx)
	if err != nil {
		s.log.Warn("ppa:ondrej/php key", "err", err)
		return
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: ondrejKeyring, Content: keyPEM, Mode: 0o644}}, Origin: "php:repo"}); err != nil {
		s.log.Warn("ppa:ondrej/php key", "err", err)
		return
	}
	if err := s.db.SetSetting(ctx, settingOndrejKey, ondrejKeyFingerprint); err != nil {
		s.log.Warn("ppa:ondrej/php key", "err", err)
		return
	}
	s.log.Info("ppa:ondrej/php key renewed", "fingerprint", ondrejKeyFingerprint)
}
