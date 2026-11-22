package setup

import (
	"errors"
	"os"
	"path/filepath"

	"monopanel/internal/osprofile"
)

// ensureModprobe keeps the kernel modules hosting does not need from loading
// (osprofile.ModprobeConf) on an install without the package: the package
// ships the rule as packageFile itself, and then mp setup writes nothing.
// Nor over localFile when anything is there already: a file of that name is
// the administrator's word on which of the modules may load (it replaces the
// package's), and a symlink is not written through. It reports whether it
// wrote the file.
func ensureModprobe(packageFile, localFile string) (bool, error) {
	for _, p := range []string{packageFile, localFile} {
		if _, err := os.Lstat(p); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(localFile), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(localFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(osprofile.ModprobeConf); err != nil {
		f.Close()            //nolint:errcheck // the write failed already
		os.Remove(localFile) //nolint:errcheck // a half-written rule is no rule
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(localFile) //nolint:errcheck // a half-written rule is no rule
		return false, err
	}
	return true, nil
}

// modprobeStep is mp setup's line about the rule.
func modprobeStep(step func(format string, a ...any)) {
	wrote, err := ensureModprobe(osprofile.ModprobePackageFile, osprofile.ModprobeLocalFile)
	switch {
	case err != nil:
		step(T("предупреждение: %s: %v; модули ядра sctp, dccp, rds, tipc может загрузить любой локальный аккаунт", "warning: %s: %v; any local account can have the kernel load sctp, dccp, rds and tipc"), osprofile.ModprobeLocalFile, err)
	case wrote:
		step(T("модули ядра sctp, dccp, rds, tipc больше не загружаются: %s", "kernel modules sctp, dccp, rds and tipc no longer load: %s"), osprofile.ModprobeLocalFile)
	}
}
