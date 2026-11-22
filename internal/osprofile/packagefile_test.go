package osprofile

import (
	"slices"
	"strings"
	"testing"
)

// The reinstall is the install with --reinstall on apt — the same lock wait,
// configuration files kept — and dnf reinstall; a package file is asked for
// its name and version in the form the installed one is queried in, so the
// two compare as they are.
func TestReinstallAndPackageFileArgv(t *testing.T) {
	apt, dnf := aptManager{}, dnfManager{}
	const deb, rpm = "/var/lib/monopanel-agent/updates/monopanel_0.9.1_amd64.deb", "/var/lib/monopanel-agent/updates/monopanel-0.9.1.x86_64.rpm"
	if got := strings.Join(apt.ReinstallArgv([]string{deb}), " "); got != strings.Replace(strings.Join(apt.InstallArgv([]string{deb}), " "), " install ", " --reinstall install ", 1) {
		t.Errorf("apt reinstall: %s", got)
	}
	if got := strings.Join(dnf.ReinstallArgv([]string{rpm}), " "); got != "dnf -q -y reinstall "+rpm {
		t.Errorf("dnf reinstall: %s", got)
	}
	if got := apt.PackageFileArgv(deb); !slices.Equal(got, []string{"dpkg-deb", "--show", "--showformat=${Package} ${Version}\n", deb}) {
		t.Errorf("apt package file: %q", got)
	}
	// rpm's query of a file and of the installed package print alike.
	q := dnf.QueryInstalledArgv([]string{"monopanel"})
	if got := dnf.PackageFileArgv(rpm); !slices.Equal(got, []string{"rpm", "-q", "--qf", q[3], "-p", rpm}) {
		t.Errorf("dnf package file: %q, the installed one is queried with %q", got, q)
	}
	// dpkg-query names the version as dpkg-deb does: ParseQuery reads it.
	if v := apt.ParseQuery("monopanel 0.9.1 installed\n")["monopanel"]; v != "0.9.1" {
		t.Errorf("apt installed version: %q", v)
	}
	var none noPackages
	if none.ReinstallArgv([]string{deb}) != nil || none.PackageFileArgv(deb) != nil {
		t.Error("a host without a package manager reinstalls")
	}
}
