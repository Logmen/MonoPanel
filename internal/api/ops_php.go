package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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
	suryKeyURL  = "https://packages.sury.org/php/apt.gpg"
	suryKeyring = "/etc/apt/keyrings/sury-php.gpg"
	surySource  = "/etc/apt/sources.list.d/sury-php.list"
	// ondrejSource and ondrejKeyring are ppa:ondrej/php as the panel set it
	// up on Ubuntu up to 0.8.19. It writes neither again: a host that has
	// them moves to packages.sury.org at the start of the API
	// (switchPHPRepoToSury).
	ondrejSource   = "/etc/apt/sources.list.d/ondrej-php.list"
	ondrejKeyring  = "/etc/apt/keyrings/ondrej-php.asc"
	remiReleaseURL = "https://rpms.remirepo.net/enterprise/remi-release-%s.rpm"
	settingPHPRepo = "php.repo"
	settingTZ      = "php.timezone"
	// settingPHPRepoSwitch holds why the last start of the API did not move
	// the host from ppa:ondrej/php to packages.sury.org, for the doctor.
	settingPHPRepoSwitch = "php.repo.switch"
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
		s.livePHPPackages(ctx, installed)
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

// phpRepoDistro is the setting value for an Ubuntu whose release
// packages.sury.org does not publish yet: only Ubuntu's own PHP packages are
// there, and Sury is probed again at the next installation.
const phpRepoDistro = "distro"

// repoProbeClient asks packages.sury.org which releases it publishes;
// SetOutbound replaces it.
var repoProbeClient = &http.Client{Timeout: 20 * time.Second}

// suryHasRelease reports whether packages.sury.org/php publishes a codename:
// every Debian the panel supports, and an Ubuntu release once its maintainer
// builds for it (22.04, 24.04 and 26.04 in November 2026, none of the
// interim releases). Ubuntu used to take the same maintainer's
// ppa:ondrej/php, which he marked deprecated then: its suite for 26.04 stayed
// empty while Sury carried every branch. Sury signs with the key the panel
// pins for Debian. Tests replace it.
var suryHasRelease = func(ctx context.Context, codename string) (bool, error) {
	return repoHasRelease(ctx, "https://packages.sury.org/php/dists/"+codename+"/Release")
}

func repoHasRelease(ctx context.Context, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	res, err := repoProbeClient.Do(req)
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

// ensurePHPRepo configures packages.sury.org (Debian and Ubuntu) or Remi
// (EL) once and remembers it in settings. A fresh Ubuntu release is not on
// Sury for a while; then the panel stays with Ubuntu's own packages and
// looks again next time instead of writing a source that apt cannot read.
func (s *Server) ensurePHPRepo(ctx context.Context, jc *jobs.Context) error {
	rel := s.profile.Release()
	key := settingPHPRepo + "." + string(s.profile.Family())
	if v, _ := s.db.GetSetting(ctx, key); v == "ready" {
		return nil
	}
	jc.Progress(5, "PHP repository")
	switch s.profile.Family() {
	case osprofile.FamilyDebian:
		if s.onUbuntu() {
			if ok, err := suryHasRelease(ctx, rel.Codename); err != nil {
				jc.Logf("cannot check packages.sury.org/php for %s (%v), trying it anyway", rel.Codename, err)
			} else if !ok {
				jc.Logf("packages.sury.org/php has no packages for Ubuntu %s (%s) yet: using Ubuntu's own PHP", rel.VersionID, rel.Codename)
				return s.db.SetSetting(ctx, key, phpRepoDistro)
			}
		}
		own, err := s.agent.Stat(ctx, surySource)
		if err != nil {
			return err
		}
		if len(own.Entries) != 1 || !own.Entries[0].Exists {
			// An administrator who set Sury up himself, as its README says,
			// has a source of his own signed by a keyring of his own
			// (/etc/apt/sources.list.d/php.list,
			// /usr/share/keyrings/deb.sury.org-php.gpg), and a host may take
			// Sury from a mirror of it. When apt reads Sury from such a
			// source, the panel adds none: its source beside an
			// administrator's would make apt refuse both ("Conflicting
			// values set for option Signed-By"), and with them every other
			// source — no index update would work on the host, nor this
			// install. The panel's own sury-php.list left from before (the
			// panel installed again, its database new) is no such source: it
			// is written again below, with its keyring.
			if hosts := s.suryOrigins(ctx, rel.Codename); len(hosts) > 0 {
				jc.Logf("repository: packages.sury.org/php (%s), which apt reads already from a source on the host (%s): the panel adds none", rel.Codename, strings.Join(hosts, ", "))
				// What apt has of it may be as old as the host's last index
				// update, and Sury drops superseded builds from its pool: it
				// is read once more. Should that fail, the install goes on
				// with the index apt has.
				res, err := s.agent.Pkg(ctx, "update-index")
				if said := aptSaidOf(res, err, hosts...); said != "" {
					jc.Logf("warning: apt did not refresh it, the install goes on with the index apt had: %s", said)
				} else if err != nil {
					jc.Logf("warning: the index update failed, the install goes on with the index apt had: %s", pkgFailure(err))
				}
				return s.db.SetSetting(ctx, key, "ready")
			}
		}
		files, err := suryRepoFiles(ctx, rel.Codename)
		if err != nil {
			return err
		}
		jc.Logf("repository: packages.sury.org/php (%s)", rel.Codename)
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "php:repo"}); err != nil {
			return err
		}
		res, err := s.agent.Pkg(ctx, "update-index")
		if err == nil && !s.suryIndexed(ctx, rel.Codename) {
			// The update ended well and apt has no index of Sury all the
			// same: apt takes a name that did not resolve or a timeout for
			// passing trouble, warns and leaves the source out. Marked
			// ready then, the host would install the branches its
			// distribution ships from there and call every other one not
			// available, until some later index update.
			err = errors.New(suryUnread(res, nil))
		}
		if err != nil {
			if surySignedBy(err) {
				// An administrator's source of Sury that apt had not read
				// yet: the look before the write could not find it.
				err = fmt.Errorf("%w; another source of packages.sury.org names another keyring (yours?): apt-get update has apt read it, and the next PHP install keeps it; or remove it, and the next install puts the panel's in its place", err)
			}
			// The source just written is the likely cause of a failure (a
			// key apt refuses, a repository that went away): left behind,
			// it would fail every later update-index on the host, a mail or
			// nginx install included. The next PHP install writes it again.
			if _, rerr := s.agent.RemovePaths(context.WithoutCancel(ctx), &agent.RemovePathsRequest{Paths: []string{surySource}}); rerr != nil {
				jc.Logf("cannot remove the repository source after the failed index update: %v", rerr)
			} else {
				jc.Logf("repository source %s removed again: apt could not read it", surySource)
			}
			return err
		}
		if said := aptSaidOf(res, nil, "packages.sury.org"); said != "" {
			// Lists of Sury from an earlier update, which apt keeps when it
			// cannot fetch new ones: genuine, and apt installs from them.
			jc.Logf("warning: apt did not refresh packages.sury.org/php this time and goes on with the index it had: %s", said)
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

// suryRepoFiles are packages.sury.org's keyring, downloaded, and its source
// for a codename.
func suryRepoFiles(ctx context.Context, codename string) ([]agent.FileSpec, error) {
	keyBin, err := fetchBytes(ctx, suryKeyURL)
	if err != nil {
		return nil, fmt.Errorf("download packages.sury.org key: %w", err)
	}
	return []agent.FileSpec{
		{Path: suryKeyring, ContentBase64: base64.StdEncoding.EncodeToString(keyBin), Mode: 0o644},
		{Path: surySource, Content: fmt.Sprintf("deb [signed-by=%s] https://packages.sury.org/php/ %s main\n", suryKeyring, codename), Mode: 0o644},
	}, nil
}

// onUbuntu tells an Ubuntu from the other distributions of the Debian
// family: its PHP took ppa:ondrej/php up to panel 0.8.19, and a release of
// it may not be on packages.sury.org yet.
func (s *Server) onUbuntu() bool {
	return s.profile.Family() == osprofile.FamilyDebian && strings.EqualFold(s.profile.Release().ID, "ubuntu")
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

// phpUnavailableNote explains a missing branch: on an Ubuntu that
// packages.sury.org does not publish yet it names what Ubuntu itself ships.
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
		note := "packages.sury.org/php has no packages for Ubuntu " + rel.VersionID + " (" + rel.Codename + ") yet"
		if native != "" {
			note += "; Ubuntu itself ships only PHP " + native
		}
		return note
	}
	return "no configured repository carries php" + version + " packages"
}

// phpAvailable is the availability matrix for this host. On an Ubuntu that
// runs without packages.sury.org it asks apt which branches actually exist.
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

// livePHPPackages puts on the installed branches the version of their FPM
// package as it is installed now, one query for them all, as the stack list
// shows it (annotateUpgrades): the record is that of the install or of the
// panel's last upgrade, and an apt-get upgrade by hand, or the
// distribution's, leaves it behind — the PHP page and the stack would
// disagree. The record follows, its version alone; a query that fails
// leaves the recorded versions.
func (s *Server) livePHPPackages(ctx context.Context, versions []*store.PHPVersion) {
	branch := map[string]*store.PHPVersion{}
	var pkgs []string
	for _, v := range versions {
		if l := phpLayout(s, v.Version); l != nil && l.FPMPackage != "" && v.Status == store.PHPInstalled {
			branch[l.FPMPackage] = v
			pkgs = append(pkgs, l.FPMPackage)
		}
	}
	if len(pkgs) == 0 {
		return
	}
	q, err := s.agent.Pkg(ctx, "query", pkgs...)
	if err != nil {
		s.log.Debug("php list: the installed packages", "err", err)
		return
	}
	for pkg, v := range branch {
		live := q.Installed[pkg]
		if live == "" || live == v.PackageVersion {
			continue
		}
		v.PackageVersion = live
		if err := s.db.SetPHPPackageVersion(context.WithoutCancel(ctx), v.Version, live); err != nil {
			s.log.Warn("the PHP branch's package version", "version", v.Version, "err", err)
		}
	}
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

// switchPHPRepoToSury moves an Ubuntu that takes PHP from ppa:ondrej/php, as
// panels up to 0.8.19 set it up, to packages.sury.org/php, where the same
// maintainer builds first (he marked the PPA deprecated in November 2026).
// It runs at every start of the API until the panel's source of the PPA is
// gone:
//
//   - only when Sury publishes the release; until then the PPA stays, and
//     the doctor says why (phpRepoCheck);
//   - Sury's keyring and source go in beside the PPA's, and the index is
//     read with both; the PPA's source and keyring go only once apt has
//     Sury's index. The agent no longer writes the PPA, so a source taken
//     away first could not be put back: this way a move that fails leaves
//     the host with the repository it had, and Sury's files go again;
//   - when apt reads Sury already, from a source of it the host has (an
//     administrator's, a mirror's), the PPA alone goes, once apt has read
//     that source anew: a source of the panel's beside an administrator's
//     would make apt refuse both for their different keyrings;
//   - the PPA goes only after an index update in which apt read Sury and
//     said nothing against it: an index of Sury apt kept from before, when
//     it could not fetch the new one, is no repository the host can rely
//     on;
//   - nothing is upgraded or reinstalled (php-fpm would restart): Sury's
//     builds sort above the PPA's, and the next upgrade of the branches —
//     the daily one, or mp stack upgrade php — takes them.
//
// A source of the PPA that an administrator added (add-apt-repository
// writes ondrej-ubuntu-php-<codename>.sources or .list) is not the panel's
// and stays.
func (s *Server) switchPHPRepoToSury(ctx context.Context) {
	if !s.onUbuntu() {
		return
	}
	st, err := s.agent.Stat(ctx, ondrejSource, ondrejKeyring, surySource)
	if err != nil || len(st.Entries) != 3 {
		s.log.Warn("PHP repository: cannot look for the panel's source of ppa:ondrej/php", "err", err)
		return
	}
	if !st.Entries[0].Exists {
		if st.Entries[1].Exists {
			// What 0.8.19 left on Ubuntu 26.04, where apt refused the
			// PPA's key and the source went again: nothing reads it.
			if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{ondrejKeyring}}); err != nil {
				s.log.Warn("PHP repository: the keyring of ppa:ondrej/php without its source", "err", err)
			}
		}
		return
	}
	why := s.moveToSury(ctx, st.Entries[2].Exists)
	if why == "" {
		if err := s.db.DeleteSetting(context.WithoutCancel(ctx), settingPHPRepoSwitch); err != nil {
			s.log.Warn("PHP repository", "err", err)
		}
		return
	}
	s.log.Warn("PHP repository: ppa:ondrej/php stays; the move to packages.sury.org/php is tried again at the next start of the API", "why", why)
	if err := s.db.SetSetting(context.WithoutCancel(ctx), settingPHPRepoSwitch, why); err != nil {
		s.log.Warn("PHP repository", "err", err)
	}
}

// moveToSury is switchPHPRepoToSury's move; own tells that the panel's own
// source of Sury is there already, left by a move an API restart cut short.
// It tells why the move did not happen, "" when it did.
func (s *Server) moveToSury(ctx context.Context, own bool) string {
	rel := s.profile.Release()
	switch ok, err := suryHasRelease(ctx, rel.Codename); {
	case err != nil:
		return fmt.Sprintf("cannot ask packages.sury.org whether it publishes %s: %v", rel.Codename, err)
	case !ok:
		return fmt.Sprintf("packages.sury.org/php does not publish Ubuntu %s (%s) yet", rel.VersionID, rel.Codename)
	}
	// apt may read Sury already from a source the host has of it: an
	// administrator's, set up as Sury's README says
	// (/etc/apt/sources.list.d/php.list, signed by
	// /usr/share/keyrings/deb.sury.org-php.gpg), or a mirror's. The panel's
	// source beside an administrator's would make apt refuse both
	// ("Conflicting values set for option Signed-By"), and with them every
	// other source: the move would fail at every start. The panel's own
	// sury-php.list is no such source: it is written again, keyring and all.
	if !own {
		if hosts := s.suryOrigins(ctx, rel.Codename); len(hosts) > 0 {
			return s.moveToHostsSury(ctx, rel.Codename, hosts)
		}
	}
	files, err := suryRepoFiles(ctx, rel.Codename)
	if err != nil {
		return err.Error()
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "php:repo"}); err != nil {
		return "writing the source of packages.sury.org: " + err.Error()
	}
	// Whether apt read Sury in this update decides, not how the update
	// ended: it fails when any source fails (the PPA itself, or an
	// administrator's), and it ends well when Sury failed for what apt takes
	// for a passing network trouble — a name that did not resolve, a
	// timeout —, a warning, Sury left out or its lists from before kept. The
	// PPA taken away then would leave the host with no repository of PHP it
	// can refresh.
	upd, uerr := s.agent.Pkg(ctx, "update-index")
	if !s.suryIndexed(ctx, rel.Codename) || !suryRead(upd, uerr, "packages.sury.org") {
		why := suryUnread(upd, uerr)
		switch {
		case surySignedBy(uerr):
			// Another source of Sury, one apt had not read yet: the look
			// before the write could not find it.
			why += "; another source of packages.sury.org names another keyring (yours?): apt-get update has apt read it, and the next start of the API keeps it, removing the PPA alone; or remove it, and the next start puts the panel's in its place"
		case ppaSignedBy(uerr):
			// An administrator's add-apt-repository beside the panel's own
			// source of the PPA: apt has read no source since.
			why += "; another source of ppa:ondrej/php names another keyring (yours, from add-apt-repository?), and apt reads no source while both are there: remove it, and the next start of the API moves PHP to packages.sury.org; or remove the panel's " + ondrejSource + " and " + ondrejKeyring + ", and PHP keeps coming from yours"
		}
		if _, rerr := s.agent.RemovePaths(context.WithoutCancel(ctx), &agent.RemovePathsRequest{Paths: []string{surySource, suryKeyring}}); rerr != nil {
			// Left beside the PPA's, a source apt cannot read fails or
			// holds up every index update.
			why += "; its source could not be removed again (" + rerr.Error() + "): remove " + surySource
		}
		return why
	}
	if uerr != nil {
		s.log.Warn("PHP repository: the index update failed, but apt has the index of packages.sury.org/php", "err", pkgFailure(uerr))
	}
	return s.dropPPA(ctx, rel.Codename, surySource)
}

// moveToHostsSury is the move of a host whose own source of Sury apt reads,
// from hosts: what apt has of it may be an index it can no longer refresh
// (a keyring that expired: apt keeps the previous index files and warns),
// so it is read anew, the PPA still there, and only then does the PPA go.
// It tells why it did not, "" when it did.
func (s *Server) moveToHostsSury(ctx context.Context, codename string, hosts []string) string {
	upd, uerr := s.agent.Pkg(ctx, "update-index")
	now := s.suryOrigins(ctx, codename)
	if !suryRead(upd, uerr, now...) {
		return "apt could not refresh packages.sury.org/php from the host's own source of it (" + strings.Join(hosts, ", ") + "): " + aptUnread(upd, uerr, hosts...)
	}
	if uerr != nil {
		s.log.Warn("PHP repository: the index update failed, but apt has read packages.sury.org/php from the host's own source of it", "err", pkgFailure(uerr))
	}
	return s.dropPPA(ctx, codename, "the host's own source of packages.sury.org/php ("+strings.Join(now, ", ")+")")
}

// dropPPA ends the move once apt has the index of packages.sury.org from
// source: the panel's source and keyring of ppa:ondrej/php go, and apt
// drops the PPA's index. It tells why it did not, "" when it did.
func (s *Server) dropPPA(ctx context.Context, codename, source string) string {
	res, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{ondrejSource, ondrejKeyring}})
	if err != nil {
		return "packages.sury.org/php is in place, but the source of the PPA could not be removed: " + err.Error()
	}
	// Once more without the PPA: apt drops its index.
	if _, err := s.agent.Pkg(ctx, "update-index"); err != nil {
		s.log.Warn("PHP repository: the index update without ppa:ondrej/php", "err", pkgFailure(err))
	}
	s.log.Info("PHP repository: moved from ppa:ondrej/php to packages.sury.org/php; the installed branches take its builds at their next upgrade (the daily one, or mp stack upgrade php)", "codename", codename, "source", source, "removed", res.Removed)
	return ""
}

// suryUnread tells why apt has no index of packages.sury.org from an index
// update, or one it did not refresh (aptUnread).
func suryUnread(upd *agent.PkgResponse, uerr error) string {
	return "apt could not read packages.sury.org/php: " + aptUnread(upd, uerr, "packages.sury.org")
}

// aptUnread tells why an index update did not read the repositories at
// hosts: what apt said against them; else, when the update failed, apt's
// first error, which names the cause (a Signed-By conflict, a lock), with
// the agent's word on the run; else that it ended without them.
func aptUnread(upd *agent.PkgResponse, uerr error, hosts ...string) string {
	reason := aptSaidOf(upd, uerr, hosts...)
	if uerr == nil {
		if reason == "" {
			reason = "the index update ended without it"
		}
		return reason
	}
	var ae *agent.Error
	if !errors.As(uerr, &ae) {
		return pkgFailure(uerr)
	}
	if reason == "" {
		reason = aptFirstError(ae.Output)
	}
	if reason == "" {
		return pkgFailure(uerr)
	}
	return oneLine(ae.Message+": "+reason, 400)
}

// aptOutput is what apt printed in a run of the package manager that ended
// well (upd) or failed (uerr, the agent's error).
func aptOutput(upd *agent.PkgResponse, uerr error) string {
	var ae *agent.Error
	switch {
	case errors.As(uerr, &ae):
		return ae.Output
	case uerr == nil && upd != nil:
		return upd.Output
	}
	return ""
}

// aptSaidOf is the first line apt printed against the repository at one of
// hosts in a run (aptSaidAbout), "" when it said nothing against them.
func aptSaidOf(upd *agent.PkgResponse, uerr error, hosts ...string) string {
	out := aptOutput(upd, uerr)
	for _, h := range hosts {
		if line := aptSaidAbout(out, h); line != "" {
			return line
		}
	}
	return ""
}

// suryRead tells whether an index update read Sury's PHP from hosts: apt
// said nothing against them, and, should the update have failed, apt shows
// it fetched from them ("Hit:7 https://packages.sury.org/php noble
// InRelease", "Get:…"). A failed update may have fetched nothing at all —
// apt stops at once on the lock of the lists another apt-get update holds,
// and on sources it cannot read —, and lists of Sury kept from before would
// pass for read.
func suryRead(upd *agent.PkgResponse, uerr error, hosts ...string) bool {
	if len(hosts) == 0 || aptSaidOf(upd, uerr, hosts...) != "" {
		return false
	}
	if uerr == nil {
		return true
	}
	for _, line := range strings.Split(aptOutput(upd, uerr), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Hit:") && !strings.HasPrefix(line, "Get:") {
			continue
		}
		for _, h := range hosts {
			if strings.Contains(line, h) {
				return true
			}
		}
	}
	return false
}

// aptFirstError is apt's first error ("E: Could not get lock
// /var/lib/apt/lists/lock …", "E: Conflicting values set for option
// Signed-By regarding source …"), which says why it stopped; "" when it
// printed none. Its last lines may say less: after a conflict over a key
// inline in a .sources file come the key's lines and "E: The list of
// sources could not be read.".
func aptFirstError(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "E:") {
			return oneLine(line, 300)
		}
	}
	return ""
}

// signedByConflict is apt's refusal of two sources of one repository that
// name different keyrings, after which apt reads no source at all: the
// repository's URI and apt's line ("E: Conflicting values set for option
// Signed-By regarding source https://packages.sury.org/php/ noble:
// /usr/share/keyrings/deb.sury.org-php.gpg != /etc/apt/keyrings/sury-php.gpg");
// both "" when apt refused none.
func signedByConflict(err error) (uri, line string) {
	var ae *agent.Error
	if !errors.As(err, &ae) {
		return "", ""
	}
	for _, l := range strings.Split(ae.Output, "\n") {
		if _, rest, ok := strings.Cut(l, "Conflicting values set for option Signed-By regarding source "); ok {
			uri, _, _ = strings.Cut(rest, " ")
			return uri, strings.TrimSpace(l)
		}
	}
	return "", ""
}

// surySignedBy tells apt's refusal of the panel's source of
// packages.sury.org/php beside another one that names another keyring.
func surySignedBy(err error) bool {
	uri, line := signedByConflict(err)
	return repoOf(uri, "packages.sury.org", "/php/") && strings.Contains(line, suryKeyring)
}

// ppaSignedBy tells apt's refusal of the panel's source of ppa:ondrej/php,
// as panels up to 0.8.19 wrote it, beside another one that names another
// keyring or none: add-apt-repository's.
func ppaSignedBy(err error) bool {
	uri, line := signedByConflict(err)
	return repoOf(uri, "ppa.launchpadcontent.net", "/ondrej/php/") && strings.Contains(line, ondrejKeyring)
}

// repoOf tells whether uri is of the repository at host whose paths start
// with dir ("/php/": packages.sury.org/php and what is below it, not
// /php-qa or /apache2).
func repoOf(uri, host, dir string) bool {
	u, err := url.Parse(uri)
	return err == nil && u.Hostname() == host && strings.HasPrefix(path.Clean(u.Path)+"/", dir)
}

// suryIndexed tells whether apt has the index of packages.sury.org/php for
// a release (suryOrigins), after the index update that follows the panel's
// write of its source, however that ended (ensurePHPRepo, moveToSury).
func (s *Server) suryIndexed(ctx context.Context, codename string) bool {
	return slices.Contains(s.suryOrigins(ctx, codename), "packages.sury.org")
}

// suryOrigins are the hosts apt has Sury's PHP for a release from, as
// apt-cache policy php-common lists them: each version, then its sources in
// lines like "500 https://packages.sury.org/php noble/main amd64 Packages".
// They are packages.sury.org/php itself, and any other host whose
// php-common is one of Sury's own builds — "2:102+debsuryorg24.04.1+3", a
// mirror of Sury; the PPA's read "+deb.sury.org+1". Sury has php-common for
// every release. Before the panel writes a source of Sury, any of them
// stands in for it (an administrator may have one of his own).
func (s *Server) suryOrigins(ctx context.Context, codename string) []string {
	res, err := s.agent.Pkg(ctx, "available", "php-common")
	if err != nil {
		return nil
	}
	var hosts []string
	version := ""
	for _, line := range strings.Split(res.Output, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "***" {
			f = f[1:] // the installed version
		}
		switch {
		case len(f) == 2 && isNumber(f[1]):
			version = f[0]
		case len(f) >= 3 && isNumber(f[0]) && strings.HasPrefix(f[2], codename+"/"):
			u, err := url.Parse(f[1])
			if err != nil || u.Hostname() == "" || slices.Contains(hosts, u.Hostname()) {
				continue
			}
			if u.Hostname() == "packages.sury.org" && repoOf(f[1], "packages.sury.org", "/php/") || u.Hostname() != "packages.sury.org" && strings.Contains(version, "+debsuryorg") {
				hosts = append(hosts, u.Hostname())
			}
		}
	}
	return hosts
}

// isNumber tells a priority of apt-cache policy's ("500").
func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// aptSaidAbout is the first warning or error apt-get update printed about a
// host ("W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease
// Temporary failure resolving 'packages.sury.org'"), "" when there is none.
// A warning that starts with the URL of a file is a notice on a file apt
// read all the same ("W: https://…/InRelease: Key is stored in legacy
// trusted.gpg keyring …"); it does not count.
func aptSaidAbout(out, host string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		warning := strings.HasPrefix(line, "W:")
		if !warning && !strings.HasPrefix(line, "E:") || !strings.Contains(line, host) {
			continue
		}
		if msg := strings.TrimSpace(line[2:]); warning && (strings.HasPrefix(msg, "https://") || strings.HasPrefix(msg, "http://")) {
			continue
		}
		return oneLine(line, 300)
	}
	return ""
}

// phpRepoCheck is the doctor's line while an Ubuntu still takes PHP from
// ppa:ondrej/php as panels up to 0.8.19 set it up: the start of the API moves
// it to packages.sury.org once it can, and the line says why that has not
// happened. Nothing once the panel's source of the PPA is gone.
func (s *Server) phpRepoCheck(ctx context.Context) *apitypes.Check {
	if !s.onUbuntu() {
		return nil
	}
	st, err := s.agent.Stat(ctx, ondrejSource)
	if err != nil || len(st.Entries) != 1 || !st.Entries[0].Exists {
		return nil
	}
	why, _ := s.db.GetSetting(ctx, settingPHPRepoSwitch)
	if why = strings.TrimRight(why, ". "); why == "" {
		why = "the start of the API has not got to it yet"
	}
	c := check("php repository", "warn", "PHP still comes from ppa:ondrej/php ("+ondrejSource+"), which the panel no longer uses: the move to packages.sury.org/php did not happen — "+why+". The panel tries again at every start of the API: systemctl restart monopanel-api (journalctl -u monopanel-api says how it went)")
	return &c
}
