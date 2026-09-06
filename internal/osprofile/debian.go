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

func (p *debianProfile) Web() WebLayout {
	return WebLayout{
		NginxUser:       "nginx",
		NginxService:    "nginx.service",
		NginxConfDir:    "/etc/nginx",
		NginxCheckArgv:  []string{"/usr/sbin/nginx", "-t", "-q"},
		ApacheUser:      "www-data",
		ApacheService:   "apache2.service",
		ApacheConfDir:   "/etc/apache2",
		ApacheCheckArgv: []string{"/usr/sbin/apache2ctl", "-t"},
	}
}

type aptManager struct{}

func (aptManager) Name() string { return "apt" }
func (aptManager) Env() []string {
	return []string{"DEBIAN_FRONTEND=noninteractive", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
}
func (aptManager) UpdateIndexArgv() []string { return []string{"apt-get", "-q", "update"} }
func (aptManager) InstallArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", "install"}, pkgs...)
}
func (aptManager) RemoveArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "remove"}, pkgs...)
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
