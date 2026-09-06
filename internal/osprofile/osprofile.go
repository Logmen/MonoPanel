// Package osprofile localises every difference between the Debian and RHEL
// families: package manager, paths, service names, users, MAC and shells.
package osprofile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// Family is the distribution family.
type Family string

const (
	FamilyDebian  Family = "debian"
	FamilyRHEL    Family = "rhel"
	FamilyUnknown Family = "unknown"
)

// Release describes the running distribution (from /etc/os-release).
type Release struct {
	ID         string   `json:"id"`
	IDLike     []string `json:"id_like,omitempty"`
	VersionID  string   `json:"version_id"`
	Codename   string   `json:"codename,omitempty"`
	PrettyName string   `json:"pretty_name"`
	Arch       string   `json:"arch"`
}

// Family maps ID / ID_LIKE to a supported family.
func (r Release) Family() Family {
	ids := append([]string{r.ID}, r.IDLike...)
	for _, id := range ids {
		switch strings.ToLower(id) {
		case "debian", "ubuntu":
			return FamilyDebian
		case "rhel", "fedora", "centos", "almalinux", "rocky", "ol":
			return FamilyRHEL
		}
	}
	return FamilyUnknown
}

// MajorVersion returns the leading integer of VERSION_ID ("24.04" -> "24").
func (r Release) MajorVersion() string {
	v, _, _ := strings.Cut(r.VersionID, ".")
	return v
}

// DebArch returns the Debian architecture name ("amd64", "arm64").
func (r Release) DebArch() string { return r.Arch }

// RPMArch returns the RPM architecture name ("x86_64", "aarch64").
func (r Release) RPMArch() string {
	switch r.Arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return r.Arch
}

// PackageManager builds argv for the native package manager. Nothing here
// executes; the agent runs the argv without a shell.
type PackageManager interface {
	Name() string
	Env() []string
	UpdateIndexArgv() []string
	InstallArgv(pkgs []string) []string
	RemoveArgv(pkgs []string) []string
	QueryInstalledArgv(pkgs []string) []string
	// ParseQuery turns QueryInstalledArgv output into package -> version.
	ParseQuery(output string) map[string]string
}

// WebLayout holds web-server users, services, directories and check commands.
type WebLayout struct {
	NginxUser       string   `json:"nginx_user"`
	NginxService    string   `json:"nginx_service"`
	NginxConfDir    string   `json:"nginx_conf_dir"`
	NginxCheckArgv  []string `json:"nginx_check_argv"`
	ApacheUser      string   `json:"apache_user"`
	ApacheService   string   `json:"apache_service"`
	ApacheConfDir   string   `json:"apache_conf_dir"`
	ApacheCheckArgv []string `json:"apache_check_argv"`
}

// Profile is the per-family behaviour used everywhere else in the panel.
type Profile interface {
	Family() Family
	Release() Release
	Packages() PackageManager
	Web() WebLayout
	// MAC returns "selinux", "apparmor" or "none".
	MAC() string
	NologinShell() string
	CronPackage() string
	SSHService() string
}

// Detect reads /etc/os-release and returns the matching profile.
func Detect() (Profile, error) {
	rel, err := ReadOSRelease()
	if err != nil {
		return nil, err
	}
	return FromRelease(rel)
}

// DetectOrGeneric returns the matching profile, or the Generic profile for
// unsupported distributions (second result true). Daemons use it so a
// developer workstation can run the panel; package operations are refused.
func DetectOrGeneric() (Profile, bool, error) {
	rel, err := ReadOSRelease()
	if err != nil {
		return nil, false, err
	}
	p, err := FromRelease(rel)
	if errors.Is(err, ErrUnsupported) {
		return Generic(rel), true, nil
	}
	return p, false, err
}

// ReadOSRelease parses /etc/os-release of the running host.
func ReadOSRelease() (Release, error) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return Release{}, fmt.Errorf("open /etc/os-release: %w", err)
	}
	defer f.Close()
	rel, err := ParseOSRelease(f)
	if err != nil {
		return rel, err
	}
	rel.Arch = runtime.GOARCH
	return rel, nil
}

// FromRelease returns the profile for an already-parsed release.
func FromRelease(rel Release) (Profile, error) {
	if rel.Arch == "" {
		rel.Arch = runtime.GOARCH
	}
	switch rel.Family() {
	case FamilyDebian:
		return &debianProfile{rel: rel}, nil
	case FamilyRHEL:
		return &rhelProfile{rel: rel}, nil
	}
	return nil, fmt.Errorf("%w: %s (ID=%s ID_LIKE=%s)", ErrUnsupported, rel.PrettyName, rel.ID, strings.Join(rel.IDLike, " "))
}

// ParseOSRelease parses the os-release(5) format.
func ParseOSRelease(r io.Reader) (Release, error) {
	var rel Release
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			rel.ID = v
		case "ID_LIKE":
			rel.IDLike = strings.Fields(v)
		case "VERSION_ID":
			rel.VersionID = v
		case "VERSION_CODENAME":
			rel.Codename = v
		case "PRETTY_NAME":
			rel.PrettyName = v
		}
	}
	if err := sc.Err(); err != nil {
		return rel, err
	}
	if rel.ID == "" {
		return rel, fmt.Errorf("os-release: missing ID")
	}
	return rel, nil
}
