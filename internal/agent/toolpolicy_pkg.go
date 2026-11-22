package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
)

// The repository packages the panel installs from their vendors' sites, the
// only packages /v1/pkg takes other than names: Remi's and EPEL's release
// RPMs (internal/api/ops_php.go remiReleaseURL, osprofile EPELPackage on
// Oracle Linux 10) and percona-release (internal/api/ops_db.go).
const (
	remiReleaseURL       = "https://rpms.remirepo.net/enterprise/remi-release-%s.rpm"
	perconaReleaseRPMURL = "https://repo.percona.com/yum/percona-release-latest.noarch.rpm"
	// PerconaReleaseDebURL is percona-release for Debian and Ubuntu. apt takes
	// no URL, so the agent downloads it itself: a package file the API
	// staged would be a package of the API's making, installed as root.
	PerconaReleaseDebURL = "https://repo.percona.com/apt/percona-release_latest.generic_all.deb"
)

// releasePackages maps the URLs this host may install to the package each
// one must be.
func (s *Server) releasePackages() map[string]string {
	switch s.profile.Family() {
	case osprofile.FamilyRHEL:
		m := map[string]string{
			fmt.Sprintf(remiReleaseURL, s.profile.Release().MajorVersion()): "remi-release",
			perconaReleaseRPMURL: "percona-release",
		}
		if e := s.profile.EPELPackage(); strings.HasPrefix(e, "https://") {
			m[e] = "epel-release"
		}
		return m
	case osprofile.FamilyDebian:
		return map[string]string{PerconaReleaseDebURL: "percona-release"}
	}
	return nil
}

// pkgArgs vets the packages of /v1/pkg: names, and for install the release
// packages above. A path, another URL or a package file would be a package
// of anybody's making installed as root (its maintainer scripts run as root).
// For a Debian release package it returns the downloaded file instead of the
// URL, and a cleanup.
func (s *Server) pkgArgs(ctx context.Context, action string, pkgs []string) ([]string, func(), error) {
	out := make([]string, 0, len(pkgs))
	var tmp []string
	cleanup := func() {
		for _, d := range tmp {
			os.RemoveAll(d) //nolint:errcheck // a temporary download
		}
	}
	release := s.releasePackages()
	for _, p := range pkgs {
		if validPkgName(p) {
			out = append(out, p)
			continue
		}
		name, ok := release[p]
		if !ok || action != "install" {
			cleanup()
			if strings.Contains(p, "://") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".deb") || strings.HasSuffix(p, ".rpm") {
				return nil, nil, notAllowed("the package "+p, "packages are installed by name (or the vendor's release package the panel knows)")
			}
			return nil, nil, &Error{Status: http.StatusBadRequest, Message: "invalid package name: " + p}
		}
		if s.profile.Family() != osprofile.FamilyDebian {
			out = append(out, p) // dnf installs the URL itself
			continue
		}
		// apt-get runs in a unit of its own (pkg_unit.go), which does not
		// see the agent's private /tmp: the download goes into the agent's
		// directory of the package manager's runs.
		pdir, err := s.openPkgDir()
		if err != nil {
			cleanup()
			return nil, nil, &Error{Message: "download " + p, Output: err.Error()}
		}
		unix.Close(pdir) //nolint:errcheck // a directory opened for reading
		file, dir, err := fetchReleasePackage(ctx, s.pkgDir(), p, name)
		if dir != "" {
			tmp = append(tmp, dir)
		}
		if err != nil {
			cleanup()
			return nil, nil, &Error{Message: "download " + p, Output: err.Error()}
		}
		out = append(out, file)
	}
	return out, cleanup, nil
}

// releaseDirPrefix starts the name of a directory a release package is
// downloaded into, in pkgDir; the agent's start removes those an earlier
// agent left once no run of theirs may still install from them
// (reapPkgRuns).
const releaseDirPrefix = "release-"

// fetchReleasePackage downloads a release package into a new directory of
// the agent's own below parent (pkgDir: root's alone, which the package
// manager's run reads too) and checks that it is the package it should be.
// Tests replace it.
var fetchReleasePackage = func(ctx context.Context, parent, url, want string) (file, dir string, err error) {
	dir, err = os.MkdirTemp(parent, releaseDirPrefix)
	if err != nil {
		return "", "", err
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return "", dir, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", dir, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", dir, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	file = path.Join(dir, path.Base(url))
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", dir, err
	}
	const limit = 32 << 20
	n, err := io.Copy(f, io.LimitReader(res.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", dir, err
	}
	if n > limit {
		return "", dir, fmt.Errorf("%s is larger than %d MB", url, limit>>20)
	}
	got, err := exec.CommandContext(rctx, "dpkg-deb", "--field", file, "Package").Output()
	if err != nil {
		return "", dir, fmt.Errorf("dpkg-deb: %w", err)
	}
	if strings.TrimSpace(string(got)) != want {
		return "", dir, fmt.Errorf("%s is the package %q, not %s", url, strings.TrimSpace(string(got)), want)
	}
	return file, dir, nil
}
