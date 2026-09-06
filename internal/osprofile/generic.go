package osprofile

import "errors"

// ErrUnsupported is returned by Detect for distributions outside the
// Debian and RHEL families.
var ErrUnsupported = errors.New("unsupported distribution")

// Generic is a last-resort profile for unsupported distributions (developer
// workstations). It reports host facts but refuses package operations, so the
// panel can start for local development without touching the system.
func Generic(rel Release) Profile { return &genericProfile{rel: rel} }

type genericProfile struct{ rel Release }

func (p *genericProfile) Family() Family           { return FamilyUnknown }
func (p *genericProfile) Release() Release         { return p.rel }
func (p *genericProfile) Packages() PackageManager { return noPackages{} }
func (p *genericProfile) MAC() string              { return "none" }
func (p *genericProfile) NologinShell() string     { return "/usr/sbin/nologin" }
func (p *genericProfile) CronPackage() string      { return "cron" }
func (p *genericProfile) SSHService() string       { return "ssh.service" }
func (p *genericProfile) Web() WebLayout {
	return (&debianProfile{rel: p.rel}).Web()
}

type noPackages struct{}

func (noPackages) Name() string                         { return "none" }
func (noPackages) Env() []string                        { return nil }
func (noPackages) UpdateIndexArgv() []string            { return nil }
func (noPackages) InstallArgv([]string) []string        { return nil }
func (noPackages) RemoveArgv([]string) []string         { return nil }
func (noPackages) QueryInstalledArgv([]string) []string { return nil }
func (noPackages) ParseQuery(string) map[string]string  { return map[string]string{} }
