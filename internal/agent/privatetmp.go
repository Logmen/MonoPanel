package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A unit's own /tmp and /var/tmp (PrivateTmp=) are systemd's directories
// <dir>/systemd-private-<boot id>-<unit>-XXXXXX/tmp, bind mounted on /tmp and
// /var/tmp in the unit's mount namespace, and systemd removes them once the
// unit is over: PID 1 itself (EL9) or a child of its own (EL10), in init_t
// on EL with SELinux. The policy does not let init_t remove what package
// scripts leave there: percona-server-server's %post makes a directory with
// mktemp -d /tmp/XXXXXXXX for its telemetry script and removes the script,
// not the directory, which the script's domain (rpm_script_t) labels
// rpm_script_tmp_t. The rmdir is denied — an AVC after every install and
// upgrade of it — and the directory stays until the next boot. So the units
// whose processes run package scripts — a run of the package manager
// (pkgCleanup), and the agent itself, which ran them as its children up to
// 0.8.19 (its unit) — empty their /tmp and /var/tmp first, in an
// ExecStopPost: `monopanel clean-private-tmp <unit>`. systemd starts it from
// bin_t into unconfined_service_t — neither unit has NoNewPrivileges, which
// would keep it in init_t —, and that domain may remove whatever is there.

// PrivateTmpCleanWait is how long `clean-private-tmp` may take: past it the
// command gives up (internal/cli), and what is left stays for systemd. A
// command after a unit's run that is still going at the unit's
// TimeoutStopSec is killed and fails the unit (Result=timeout), its "-"
// notwithstanding — a package run that did what it was to would be reported
// as failed —, so this is well inside the stop timeout of the units it runs
// in: a run's (pkgStopTimeout), and the agent's (systemd's default, 90
// seconds). 100000 entries go in about two seconds.
const PrivateTmpCleanWait = 30 * time.Second

// privateTmpDirs are the directories PrivateTmp= gives a unit of its own:
// rpm writes its scriptlets into /var/tmp, and a %post may leave its
// directory there as well as in /tmp.
var privateTmpDirs = []string{"/tmp", "/var/tmp"}

// mountEntry is a line of /proc/self/mountinfo: the directory of the mounted
// file system that the mount shows (root), and where (point).
type mountEntry struct{ root, point string }

// parseMounts reads /proc/self/mountinfo, in its order.
func parseMounts(b []byte) []mountEntry {
	var mounts []mountEntry
	for _, line := range strings.Split(string(b), "\n") {
		// 814 794 8:4 /tmp/systemd-private-…-x.service-pta2CB/tmp /tmp rw,relatime shared:449 master:1 - xfs /dev/sda4 rw
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mounts = append(mounts, mountEntry{root: unescapeMountinfo(f[3]), point: unescapeMountinfo(f[4])})
	}
	return mounts
}

// privateTmpRoot matches the root of a mount that is unit's own /tmp or
// /var/tmp: systemd's directory for it, relative to the file system it is
// on (/tmp/systemd-private-… on the root file system, /systemd-private-… on a
// tmpfs at /tmp).
func privateTmpRoot(unit string) *regexp.Regexp {
	return regexp.MustCompile(`(^|/)systemd-private-[0-9a-f]{32}-` + regexp.QuoteMeta(unit) + `-[0-9A-Za-z]{6}/tmp$`)
}

// CleanPrivateTmp empties the /tmp and /var/tmp the calling process sees,
// each only where it is unit's own (cleanPrivateTmp): the ExecStopPost of a
// unit with PrivateTmp=, ahead of systemd's removal of them.
func CleanPrivateTmp(unit string) error {
	if !unitRe.MatchString(unit) || !strings.HasSuffix(unit, ".service") {
		return fmt.Errorf("%q is not the name of a service unit", unit)
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	return cleanPrivateTmp(parseMounts(b), unit, privateTmpDirs)
}

// cleanPrivateTmp removes what is in each of dirs whose mount — the last one
// at the directory, which is what it shows — is unit's own (privateTmpRoot).
// Any other is left as it is: a drop-in (<unit>.d, monopanel-.service.d,
// service.d) can start the unit without PrivateTmp=, and the directory is
// the host's then; one line says so for all of them (notOwn). Nothing below
// a directory is followed out of it: a symlink goes, not what it points to,
// and an entry with a mount at or below it stays.
func cleanPrivateTmp(mounts []mountEntry, unit string, dirs []string) error {
	own := privateTmpRoot(unit)
	var errs []error
	var refused []string
	for _, dir := range dirs {
		root, mounted := "", false
		for _, m := range mounts {
			if m.point == dir {
				root, mounted = m.root, true
			}
		}
		if !mounted || !own.MatchString(root) {
			refused = append(refused, dir)
			continue
		}
		f, err := os.Open(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		names, err := f.Readdirnames(-1)
		f.Close()
		if err != nil {
			errs = append(errs, err)
		}
		for _, name := range names {
			p := filepath.Join(dir, name)
			if m, ok := mountAtOrBelow(mounts, p); ok {
				errs = append(errs, fmt.Errorf("%s: a mount at %s, left as it is", p, m))
				continue
			}
			// os.RemoveAll follows no symlink it meets (openat with
			// O_NOFOLLOW below the start)
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(refused) > 0 {
		errs = append([]error{notOwn(refused, unit)}, errs...)
	}
	return errors.Join(errs...)
}

// notOwn says that dirs are not unit's own and stay as they are, in one
// line: what the cleanup says ends the run's output, of which a job's log
// shows the last lines.
func notOwn(dirs []string, unit string) error {
	if len(dirs) == 1 {
		return fmt.Errorf("%s is not %s's own (PrivateTmp=): left as it is", dirs[0], unit)
	}
	return fmt.Errorf("%s and %s are not %s's own (PrivateTmp=): left as they are", strings.Join(dirs[:len(dirs)-1], ", "), dirs[len(dirs)-1], unit)
}

// mountAtOrBelow is a mount point at p or below it.
func mountAtOrBelow(mounts []mountEntry, p string) (string, bool) {
	for _, m := range mounts {
		if atOrBelow(m.point, p) {
			return m.point, true
		}
	}
	return "", false
}
