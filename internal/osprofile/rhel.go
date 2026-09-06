package osprofile

import "strings"

type rhelProfile struct{ rel Release }

func (p *rhelProfile) Family() Family           { return FamilyRHEL }
func (p *rhelProfile) Release() Release         { return p.rel }
func (p *rhelProfile) Packages() PackageManager { return dnfManager{} }
func (p *rhelProfile) MAC() string              { return "selinux" }
func (p *rhelProfile) NologinShell() string     { return "/sbin/nologin" }
func (p *rhelProfile) CronPackage() string      { return "cronie" }
func (p *rhelProfile) SSHService() string       { return "sshd.service" }

func (p *rhelProfile) Web() WebLayout {
	return WebLayout{
		NginxUser:       "nginx",
		NginxService:    "nginx.service",
		NginxConfDir:    "/etc/nginx",
		NginxCheckArgv:  []string{"/usr/sbin/nginx", "-t", "-q"},
		ApacheUser:      "apache",
		ApacheService:   "httpd.service",
		ApacheConfDir:   "/etc/httpd",
		ApacheCheckArgv: []string{"/usr/sbin/apachectl", "-t"},
	}
}

type dnfManager struct{}

func (dnfManager) Name() string              { return "dnf" }
func (dnfManager) Env() []string             { return []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8"} }
func (dnfManager) UpdateIndexArgv() []string { return []string{"dnf", "-q", "-y", "makecache"} }
func (dnfManager) InstallArgv(pkgs []string) []string {
	return append([]string{"dnf", "-q", "-y", "install"}, pkgs...)
}
func (dnfManager) RemoveArgv(pkgs []string) []string {
	return append([]string{"dnf", "-q", "-y", "remove"}, pkgs...)
}
func (dnfManager) QueryInstalledArgv(pkgs []string) []string {
	return append([]string{"rpm", "-q", "--qf", "%{NAME} %{VERSION}-%{RELEASE}\n"}, pkgs...)
}
func (dnfManager) ParseQuery(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "not installed") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 2 {
			res[f[0]] = f[1]
		}
	}
	return res
}
