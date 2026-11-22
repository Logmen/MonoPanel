package osprofile

import (
	"net/url"
	"strings"
)

// The vendor packages the panel installs keep their vendors' repositories
// (nginx.org, Sury or ppa:ondrej/php, Remi, Percona, MySQL) but nothing on
// the host upgrades from them: unattended-upgrades takes only the
// distribution's origins, and EL has no dnf-automatic. mp stack upgrade and
// its daily run do, for these sets. The agent upgrades a name only when it is
// in one of them and installed (UpgradeComponent), so the API cannot bring a
// package onto the host, nor upgrade one of the distribution's packages the
// panel does not manage.

// The components of mp stack upgrade.
const (
	UpgradeNginx  = "nginx"
	UpgradePHP    = "php"
	UpgradeApache = "apache"
	UpgradeDB     = "db"
)

// Candidate is the newer version a repository offers for an installed
// package, and where from: the repository's host (apt) or id (dnf).
type Candidate struct {
	Version string `json:"version"`
	Origin  string `json:"origin,omitempty"`
}

// NginxPackages are nginx's packages (nginx.org builds one package; the
// panel installs none of its modules).
func NginxPackages(Profile) []string { return []string{"nginx"} }

// ApachePackages are the packages of the Apache the accounts' sites in Apache
// mode run on: Debian and Ubuntu only, where the panel installs it.
func ApachePackages(p Profile) []string {
	if p.Family() != FamilyDebian {
		return nil
	}
	return []string{"apache2", "apache2-bin", "apache2-data", "apache2-utils"}
}

// PHPPackages are a branch's packages: the ones the panel installs (core and
// optional extensions) and the common ones they pin to their own version,
// which have to move with them. An extension installed by hand moves along as
// a dependency.
func PHPPackages(p Profile, version string) []string {
	l := PHP(p, version)
	if l == nil {
		return nil
	}
	out := append(append([]string{}, l.CorePackages...), l.ExtraPackages...)
	switch l.Source {
	case PHPSourceSury:
		out = append(out, "php"+version+"-common")
		if versionLess(version, "8.0") && !versionLess(version, "7.0") {
			// json is built in from 8.0 on; before, Sury packages it apart
			out = append(out, "php"+version+"-json")
		}
	case PHPSourceRemi:
		short := "php" + strings.ReplaceAll(version, ".", "")
		out = append(out, short+"-php-common", short+"-runtime")
	}
	return out
}

// PHPSharedPackages are what every branch depends on: Sury's php-common
// (phpenmod, the session cleaner).
func PHPSharedPackages(p Profile) []string {
	if p.Family() == FamilyDebian {
		return []string{"php-common"}
	}
	return nil
}

// DBPackages are a database server's packages for an engine ("percona" or
// "mysql"): the server, its client and the libraries they pin to the same
// version (DB names the one the panel installs).
func DBPackages(p Profile, engine string) []string {
	switch {
	case p.Family() == FamilyRHEL && engine == "percona":
		return []string{"percona-server-server", "percona-server-client", "percona-server-shared", "percona-server-shared-compat", "percona-icu-data-files"}
	case p.Family() == FamilyRHEL:
		return []string{"mysql-community-server", "mysql-community-client", "mysql-community-common", "mysql-community-libs", "mysql-community-libs-compat", "mysql-community-client-plugins", "mysql-community-icu-data-files"}
	case p.Family() == FamilyDebian && engine == "percona":
		return []string{"percona-server-server", "percona-server-client", "percona-server-common", "libperconaserverclient24", "libperconaserverclient22", "libperconaserverclient21"}
	case p.Family() == FamilyDebian:
		return []string{"mysql-community-server", "mysql-community-server-core", "mysql-community-client", "mysql-community-client-core", "mysql-community-client-plugins", "mysql-common", "mysql-client", "mysql-server", "libmysqlclient24", "libmysqlclient21"}
	}
	return nil
}

// UpgradeComponent names the component of mp stack upgrade a package belongs
// to, "" when it is none of the panel's packages: the agent upgrades nothing
// else. Every PHP branch the panel knows counts, installed or not (the agent
// checks that a package is installed on its own).
func UpgradeComponent(p Profile, pkg string) string {
	if p.Family() != FamilyDebian && p.Family() != FamilyRHEL {
		return ""
	}
	sets := []struct {
		component string
		pkgs      []string
	}{
		{UpgradeNginx, NginxPackages(p)},
		{UpgradeApache, ApachePackages(p)},
		{UpgradePHP, PHPSharedPackages(p)},
		{UpgradeDB, DBPackages(p, "percona")},
		{UpgradeDB, DBPackages(p, "mysql")},
	}
	for _, v := range phpAllVersions {
		sets = append(sets, struct {
			component string
			pkgs      []string
		}{UpgradePHP, PHPPackages(p, v)})
	}
	for _, s := range sets {
		for _, name := range s.pkgs {
			if name == pkg {
				return s.component
			}
		}
	}
	return ""
}

// UpgradeArgv is InstallArgv with --only-upgrade: a name that is not
// installed stays so. Changes to a package's configuration files (the
// administrator's, the panel's nginx.conf) are kept, as on install, and an
// upgrade that would remove a package (a conflict, a dependency gone) is
// refused instead of carried out unattended (--no-remove).
func (aptManager) UpgradeArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "-o", aptLockWait, "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", "--no-remove", "--only-upgrade", "install"}, pkgs...)
}

// HeldArgv: apt-cache policy names a held package's candidate all the same,
// and apt-get fails the whole run on it (-y without
// --allow-change-held-packages).
func (aptManager) HeldArgv() []string { return []string{"apt-mark", "showhold"} }

// ParseHeld reads apt-mark showhold: a package name per line, with its
// architecture for a library installed for more than one ("libfoo:amd64";
// a package name has no colon of its own).
func (aptManager) ParseHeld(out string) map[string]bool {
	res := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 1 {
			name, _, _ := strings.Cut(f[0], ":")
			res[name] = true
		}
	}
	return res
}

// CandidatesArgv reads the index as the last apt-get update left it.
func (aptManager) CandidatesArgv(pkgs []string) []string {
	return append([]string{"apt-cache", "policy"}, pkgs...)
}

// ParseCandidates reads `apt-cache policy`: the Candidate: line and, in the
// version table below it, the first repository that carries the candidate.
// A version line is "<version> <priority>" (" *** " marks the installed
// one), a repository line under it "<priority> <uri> <suite> …" or
// "<priority> /var/lib/dpkg/status". The table goes from the newest version
// down, so a candidate listed under the installed one is older: what a pin
// of 1000 or more asks for (an administrator's way of holding a version),
// which apt-get -y refuses to install — no update, it is left out. A header
// may carry the architecture ("libfoo:amd64:").
func (aptManager) ParseCandidates(out string) map[string]Candidate {
	res := map[string]Candidate{}
	pkg, cand, inCand, below := "", "", false, false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case line[0] != ' ' && strings.HasSuffix(line, ":"):
			pkg, _, _ = strings.Cut(strings.TrimSuffix(line, ":"), ":")
			cand, inCand, below = "", false, false
			continue
		case pkg == "":
			continue
		case strings.HasPrefix(trimmed, "Candidate:"):
			if cand = strings.TrimSpace(strings.TrimPrefix(trimmed, "Candidate:")); cand == "(none)" {
				cand = ""
			}
			if cand != "" {
				res[pkg] = Candidate{Version: cand}
			}
			continue
		}
		installed := strings.HasPrefix(trimmed, "***")
		f := strings.Fields(strings.TrimPrefix(trimmed, "***"))
		switch {
		case len(f) < 2:
		case digits(f[0]) && (strings.HasPrefix(f[1], "/") || strings.Contains(f[1], "://")):
			if c, ok := res[pkg]; ok && inCand && c.Origin == "" {
				c.Origin = repoHost(f[1])
				res[pkg] = c
			}
		case digits(f[1]):
			inCand = cand != "" && f[0] == cand
			if inCand && below && !installed {
				delete(res, pkg)
				inCand = false
			}
			below = below || installed
		}
	}
	return res
}

// UpgradeArgv upgrades installed packages only (dnf upgrade installs no
// name that is not installed). The index before it is fetched afresh:
// dnfManager.UpdateIndexArgv has --refresh, as plain makecache keeps what is
// younger than metadata_expire (48 hours by default) and an upgrade would not
// see a fix published since.
func (dnfManager) UpgradeArgv(pkgs []string) []string {
	return append([]string{"dnf", "-q", "-y", "upgrade"}, pkgs...)
}

// CandidatesArgv lists the upgrades of installed packages, the newest of
// each, from the metadata dnf has cached (-C): dnf-makecache.timer and the
// upgrade job keep it fresh, and a listing must not wait for a download.
func (dnfManager) CandidatesArgv(pkgs []string) []string {
	return append([]string{"dnf", "-q", "-C", "repoquery", "--upgrades", "--latest-limit", "1", "--queryformat", "%{name} %{version}-%{release} %{repoid}\n"}, pkgs...)
}

// ParseCandidates reads "<name> <version>-<release> <repository id>" lines;
// a warning dnf prints on the way ("Warning: …") is none of them.
func (dnfManager) ParseCandidates(out string) map[string]Candidate {
	res := map[string]Candidate{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && !strings.HasSuffix(f[0], ":") && strings.Contains(f[1], "-") {
			res[f[0]] = Candidate{Version: f[1], Origin: f[2]}
		}
	}
	return res
}

// HeldArgv: dnf's versionlock keeps a locked package out of repoquery
// --upgrades and dnf upgrade by itself.
func (dnfManager) HeldArgv() []string               { return nil }
func (dnfManager) ParseHeld(string) map[string]bool { return map[string]bool{} }

func (noPackages) UpgradeArgv([]string) []string    { return nil }
func (noPackages) CandidatesArgv([]string) []string { return nil }
func (noPackages) ParseCandidates(string) map[string]Candidate {
	return map[string]Candidate{}
}
func (noPackages) HeldArgv() []string               { return nil }
func (noPackages) ParseHeld(string) map[string]bool { return map[string]bool{} }

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// repoHost is the host of an apt source's URI ("https://nginx.org/packages/ubuntu"
// → nginx.org), how the panel names where a version comes from; "" for the
// dpkg status file and what does not parse.
func repoHost(uri string) string {
	if !strings.Contains(uri, "://") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
