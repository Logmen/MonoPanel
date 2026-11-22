package osprofile

// A package file installed again: the panel's self-update hands the package
// manager its own package as a file (updater.Installer), and one whose
// install rolled back is still recorded as installed — the rollback puts
// the previous binary back, not the package database — so that installing
// the file once more changes nothing unless it is a reinstall.

// ReinstallArgv is InstallArgv with --reinstall: apt installs the package
// file again although its version is installed, with the same lock wait
// and configuration files kept.
func (aptManager) ReinstallArgv(pkgs []string) []string {
	return append([]string{"apt-get", "-q", "-y", "-o", aptLockWait, "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", "--reinstall", "install"}, pkgs...)
}

// PackageFileArgv reads the file's control fields: ${Version} prints as
// dpkg-query's does, epoch and revision included.
func (aptManager) PackageFileArgv(file string) []string {
	return []string{"dpkg-deb", "--show", "--showformat=${Package} ${Version}\n", file}
}

// ReinstallArgv: dnf reinstall takes package files as well, and refuses a
// version other than the installed one, which the caller rules out first
// (PackageFileArgv).
func (dnfManager) ReinstallArgv(pkgs []string) []string {
	return append([]string{"dnf", "-q", "-y", "reinstall"}, pkgs...)
}

// PackageFileArgv asks rpm about the file in QueryInstalledArgv's format.
func (dnfManager) PackageFileArgv(file string) []string {
	return []string{"rpm", "-q", "--qf", "%{NAME} %{VERSION}-%{RELEASE}\n", "-p", file}
}

func (noPackages) ReinstallArgv([]string) []string { return nil }
func (noPackages) PackageFileArgv(string) []string { return nil }
