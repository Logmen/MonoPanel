package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"

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

// modprobeHistory is every text of the rule a release wrote, as the
// package's file or as mp setup's copy of it in /etc/modprobe.d, oldest
// first: each is a generation of its own, since a change of the rule changes
// what it keeps from loading. It is how a copy in /etc is told for the
// panel's own and whether it lags the package's rule (pruneModprobeCopy). A
// release that changes osprofile.ModprobeConf adds the new text at the end,
// the former one staying here word for word:
// TestModprobeHistoryKeepsEveryRelease pins them.
func modprobeHistory() []string {
	return []string{
		// 0.8.20, the first release with the rule.
		osprofile.ModprobeConf,
	}
}

// maxRuleSize bounds what is read of a rule to compare it: the panel's is a
// kilobyte.
const maxRuleSize = 64 << 10

// readRule is a rule file's content, when it is a plain file of a rule's
// size.
func readRule(path string) ([]byte, bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxRuleSize {
		return nil, false
	}
	b, err := os.ReadFile(path)
	return b, err == nil
}

// modprobeCopy is what pruneModprobeCopy did.
type modprobeCopy struct {
	// Removed: the copy was the panel's own and went; the package's rule
	// applies, and whatever a later release changes in it.
	Removed bool
	// Kept: a file there replaces the package's rule all the same — the
	// administrator's (a module let load again, an edit), or one this
	// version cannot tell lags the package's.
	Kept bool
}

// pruneModprobeCopy removes localFile (/etc/modprobe.d/monopanel.conf),
// which mp setup writes on an install without the package, once the
// package's own rule is there as packageFile and the copy is provably the
// panel's and no newer than it: both are texts of modprobeHistory, and the
// copy's is not the later one. A host set up from the binary alone that
// takes the package later (mp update apply) would keep the copy otherwise,
// and a file of that name in /etc/modprobe.d replaces the one in /usr/lib
// for good: a module a later release adds to the rule would load there, and
// the doctor would take that for the administrator's choice. A file the
// administrator wrote or edited stays (Kept), and so does a symlink, which
// is no copy: a link to /dev/null switches the rule off on purpose. history
// is modprobeHistory.
func pruneModprobeCopy(localFile, packageFile string, history []string) (modprobeCopy, error) {
	st, err := os.Lstat(localFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return modprobeCopy{}, nil
	case err != nil:
		return modprobeCopy{}, err
	case !st.Mode().IsRegular():
		return modprobeCopy{}, nil
	}
	// Without the package the copy is the rule.
	if _, err := os.Stat(packageFile); errors.Is(err, os.ErrNotExist) {
		return modprobeCopy{}, nil
	}
	local, okLocal := readRule(localFile)
	pkg, okPkg := readRule(packageFile)
	e, p := slices.Index(history, string(local)), slices.Index(history, string(pkg))
	if !okLocal || !okPkg || e < 0 || p < 0 || e > p {
		return modprobeCopy{Kept: true}, nil
	}
	if err := os.Remove(localFile); err != nil {
		return modprobeCopy{Kept: true}, err
	}
	return modprobeCopy{Removed: true}, nil
}

// PruneModprobeCopy is pruneModprobeCopy on the host's files: monopanel
// prune-unit-copies runs it from the package's postinstall, after the units.
func PruneModprobeCopy() (removed, kept bool, err error) {
	res, err := pruneModprobeCopy(osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile, modprobeHistory())
	return res.Removed, res.Kept, err
}

// modprobeStep is mp setup's line about the rule: a copy of the package's
// rule the panel made goes, and an install without the package gets one.
func modprobeStep(step func(format string, a ...any)) {
	res, err := pruneModprobeCopy(osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile, modprobeHistory())
	switch {
	case err != nil:
		step(T("предупреждение: %s: %v", "warning: %s: %v"), osprofile.ModprobeLocalFile, err)
	case res.Removed:
		step(T("modprobe: удалён %s — копия правила пакета, которую сделала панель; действует %s", "modprobe: removed %s, a copy of the package's rule the panel made: %s applies"), osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile)
	case res.Kept:
		step(T("предупреждение: %s заменяет правило пакета %s, и его изменения в следующих версиях сюда не доходят; если файл не нужен, удалите его",
			"warning: %s replaces the package's rule %s, and what later releases change in it does not reach this host; unless you keep it on purpose, remove it"), osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile)
	}
	wrote, err := ensureModprobe(osprofile.ModprobePackageFile, osprofile.ModprobeLocalFile)
	switch {
	case err != nil:
		step(T("предупреждение: %s: %v; модули ядра sctp, dccp, rds, tipc может загрузить любой локальный аккаунт", "warning: %s: %v; any local account can have the kernel load sctp, dccp, rds and tipc"), osprofile.ModprobeLocalFile, err)
	case wrote:
		step(T("модули ядра sctp, dccp, rds, tipc больше не загружаются: %s", "kernel modules sctp, dccp, rds and tipc no longer load: %s"), osprofile.ModprobeLocalFile)
	}
}
