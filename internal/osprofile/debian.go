package osprofile

import "strings"

type debianProfile struct{ rel Release }

func (p *debianProfile) Family() Family           { return FamilyDebian }
func (p *debianProfile) Release() Release         { return p.rel }
func (p *debianProfile) Packages() PackageManager { return aptManager{} }
func (p *debianProfile) MAC() string              { return "apparmor" }
func (p *debianProfile) NologinShell() string     { return "/usr/sbin/nologin" }
func (p *debianProfile) CronPackage() string      { return "cron" }
func (p *debianProfile) SSHService() string       { return "ssh.service" }
func (p *debianProfile) EPELPackage() string      { return "" }

func (p *debianProfile) Web() WebLayout {
	return WebLayout{
		NginxUser:      "nginx",
		NginxService:   "nginx.service",
		NginxConfDir:   "/etc/nginx",
		NginxCheckArgv: []string{"/usr/sbin/nginx", "-t", "-q"},
		ApacheUser:     "www-data",
		ApacheService:  "apache2.service",
		ApacheConfDir:  "/etc/apache2",
	}
}

type aptManager struct{}

func (aptManager) Name() string { return "apt" }

// Env: NEEDRESTART_MODE=l keeps Ubuntu's needrestart, which apt runs after
// dpkg, to listing the services that run old code. Its default asks which
// to restart, and with DEBIAN_FRONTEND=noninteractive the default answer is
// all of them: the database server too, which mp stack upgrade restarts only
// when asked for by name, and every other one without the configuration
// check the panel runs before its own restarts.
func (aptManager) Env() []string {
	return []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
}

// aptLockWait makes apt wait for the dpkg lock instead of failing at once:
// on a fresh Ubuntu unattended-upgrades holds it for minutes after boot, and
// a job that dies on "Could not get lock" looks like a broken installer.
const aptLockWait = "DPkg::Lock::Timeout=600"

// aptLabelChange lets apt-get update accept a repository whose Release file
// changed its Label. apt refuses that by default until someone confirms it
// interactively, and then every index update fails — as on every Ubuntu with
// ppa:ondrej/php when the PPA was relabelled "PPA for PHP (deprecated)" in
// November 2026. The panel's own source of the PPA goes when the host moves
// to packages.sury.org, but an administrator's (add-apt-repository) stays,
// and with it the relabel. The label is cosmetic (unlike Origin, Suite or
// Codename, which apt keeps guarding).
const aptLabelChange = "Acquire::AllowReleaseInfoChange::Label=true"

func (aptManager) UpdateIndexArgv() []string {
	return []string{"apt-get", "-q", "-o", aptLockWait, "-o", aptLabelChange, "update"}
}
func (aptManager) InstallArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "-o", aptLockWait, "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", "install"}, pkgs...)
}
func (aptManager) RemoveArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "-o", aptLockWait, "remove"}, pkgs...)
}
func (aptManager) QueryInstalledArgv(pkgs []string) []string {
	return append([]string{"dpkg-query", "-W", "-f=${Package} ${Version} ${db:Status-Status}\n"}, pkgs...)
}
func (aptManager) ParseQuery(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[2] == "installed" {
			res[f[0]] = f[1]
		}
	}
	return res
}
func (aptManager) AvailableArgv(pkgs []string) []string {
	return append([]string{"apt-cache", "policy"}, pkgs...)
}

// ParseAvailable reads `apt-cache policy` blocks: a "pkg:" line followed by
// indented "Installed:" and "Candidate:" lines; "(none)" means no repository
// has the package.
func (aptManager) ParseAvailable(out string) map[string]string {
	res := map[string]string{}
	pkg := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
			continue
		case line[0] != ' ' && strings.HasSuffix(line, ":"):
			pkg = strings.TrimSuffix(line, ":")
		case pkg != "" && strings.HasPrefix(strings.TrimSpace(line), "Candidate:"):
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Candidate:"))
			if v != "" && v != "(none)" {
				res[pkg] = v
			}
		}
	}
	return res
}
