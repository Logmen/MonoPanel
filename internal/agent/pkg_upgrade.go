package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"monopanel/internal/osprofile"
	"monopanel/internal/systemd"
)

// Upgrades of the panel's vendor packages (mp stack upgrade): nginx.org's
// nginx, the PHP branches from Sury, ppa:ondrej/php or Remi, Apache and the
// database server. Nothing else upgrades them — unattended-upgrades takes the
// distribution's origins only, EL has no dnf-automatic — and the API must
// not write a rule into /etc/apt/apt.conf.d (apt runs the commands of those
// files as root), so the panel's own job asks the agent.

// queryInstalled is what of pkgs is installed, by version. Tests replace it.
var queryInstalled = func(ctx context.Context, pm osprofile.PackageManager, pkgs []string) map[string]string {
	out, _ := runArgv(ctx, pm.Env(), pm.QueryInstalledArgv(pkgs)...)
	return pm.ParseQuery(out)
}

// heldPackages are the packages the administrator holds back (apt-mark
// hold); none where the package manager leaves them out of the candidates
// itself (dnf's versionlock). Tests replace it.
var heldPackages = func(ctx context.Context, pm osprofile.PackageManager) map[string]bool {
	argv := pm.HeldArgv()
	if argv == nil {
		return map[string]bool{}
	}
	out, _ := runArgv(ctx, pm.Env(), argv...)
	return pm.ParseHeld(out)
}

// pkgCandidates answers "candidates": what of the packages is installed, and
// for each installed one the other version a repository offers, from the
// index as the host last fetched it (apt-get update, dnf makecache). It
// reads only, like "query".
func (s *Server) pkgCandidates(ctx context.Context, pm osprofile.PackageManager, pkgs []string) *PkgResponse {
	resp := &PkgResponse{Installed: map[string]string{}, Upgrades: map[string]osprofile.Candidate{}}
	if len(pkgs) == 0 {
		return resp
	}
	resp.Installed = queryInstalled(ctx, pm, pkgs)
	seen := map[string]bool{}
	var names []string
	for _, p := range pkgs {
		if _, ok := resp.Installed[p]; ok && !seen[p] {
			seen[p] = true
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return resp
	}
	// apt-cache and dnf repoquery exit non-zero for a name they do not know
	// while still answering for the others: the exit code is no error.
	out, _ := runArgv(ctx, pm.Env(), pm.CandidatesArgv(names)...)
	resp.Output = out
	resp.Upgrades = upgradesOf(resp.Installed, pm.ParseCandidates(out))
	resp.Upgrades, resp.Held = splitHeld(resp.Upgrades, heldPackages(ctx, pm))
	return resp
}

// splitHeld takes what the administrator holds out of the upgrades: no
// upgrade the panel makes, it is reported apart.
func splitHeld(ups map[string]osprofile.Candidate, held map[string]bool) (map[string]osprofile.Candidate, map[string]osprofile.Candidate) {
	var holds map[string]osprofile.Candidate
	for name, c := range ups {
		if held[name] {
			if holds == nil {
				holds = map[string]osprofile.Candidate{}
			}
			holds[name] = c
			delete(ups, name)
		}
	}
	return ups, holds
}

// upgradeArgv vets an upgrade (upgradable) and leaves out what the
// administrator holds: apt-get would fail the whole run on it.
func (s *Server) upgradeArgv(ctx context.Context, pm osprofile.PackageManager, pkgs []string) ([]string, error) {
	if err := s.upgradable(ctx, pm, pkgs); err != nil {
		return nil, err
	}
	held := heldPackages(ctx, pm)
	var kept, holds []string
	for _, p := range pkgs {
		if held[p] {
			holds = append(holds, p)
		} else {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil, &Error{Status: http.StatusConflict, Message: "the administrator holds " + strings.Join(holds, ", ") + " (apt-mark hold): nothing to upgrade"}
	}
	return pm.UpgradeArgv(kept), nil
}

// upgradesOf keeps the candidates that differ from what is installed. apt's
// candidate is what apt-get install would put there; the panel pins no
// vendor at 1000 or more, so for its packages another candidate is a newer
// one. dnf's --upgrades lists newer ones only.
func upgradesOf(installed map[string]string, cands map[string]osprofile.Candidate) map[string]osprofile.Candidate {
	out := map[string]osprofile.Candidate{}
	for name, c := range cands {
		if v, ok := installed[name]; ok && c.Version != "" && c.Version != v {
			out[name] = c
		}
	}
	return out
}

// upgradable vets an upgrade: the panel's own vendor packages only
// (osprofile.UpgradeComponent), and of them only installed ones. With names
// alone (pkgArgs refused paths, URLs and options) an upgrade takes the version
// a configured repository offers and installs nothing new — apt-get
// --only-upgrade and dnf upgrade skip a name that is not installed, and this
// refuses it outright — so the API can neither bring a package onto the host
// through it nor move one of the distribution's packages the panel does not
// manage.
func (s *Server) upgradable(ctx context.Context, pm osprofile.PackageManager, pkgs []string) error {
	for _, p := range pkgs {
		if osprofile.UpgradeComponent(s.profile, p) == "" {
			return notAllowed("upgrading "+p, "the panel upgrades its own packages only: nginx, PHP, Apache and the database server")
		}
	}
	installed := queryInstalled(ctx, pm, pkgs)
	var missing []string
	for _, p := range pkgs {
		if _, ok := installed[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return notAllowed("upgrading "+strings.Join(missing, ", "), "not installed, and an upgrade installs nothing new")
	}
	return nil
}

// procRoot is where the agent looks at processes; tests point it elsewhere.
var procRoot = "/proc"

// binaryReplaced tells whether a running service's main process runs a
// program file an upgrade has replaced: it runs the old code until it
// restarts, as a reload re-reads the configuration without executing the new
// binary (nginx's HUP, Apache's graceful restart). The kernel shows such a
// file as "<path> (deleted)".
func binaryReplaced(ctx context.Context, conn systemdConn, unit string, st systemd.Status) bool {
	if !strings.HasSuffix(unit, ".service") || (st.ActiveState != "active" && st.ActiveState != "reloading") {
		return false
	}
	pid, err := conn.MainPID(ctx, unit)
	if err != nil || pid == 0 {
		return false
	}
	if !exeDeleted(pid) {
		return false
	}
	pidFile, _ := conn.PIDFile(ctx, unit)
	return !binaryUpgraded(pid, pidFile)
}

// binaryUpgraded tells that a service whose main process runs a replaced
// binary already serves on the new one: nginx's binary upgrade (USR2, which
// nginx.org's packages send on an upgrade) starts a new master from the new
// file, which writes the PID file, and the old master — the main process to
// systemd until it exits — only finishes the requests it holds (downloads,
// websockets, for as long as they last). New connections reach the new code,
// and a restart would just cut those requests. The process the PID file
// names has to run the same program, from a file still there.
func binaryUpgraded(mainPID uint32, pidFile string) bool {
	if pidFile == "" {
		return false
	}
	next := readPIDFile(pidFile)
	if next == 0 || next == mainPID {
		return false
	}
	old, oldGone := exeOf(mainPID)
	cur, curGone := exeOf(next)
	return oldGone && !curGone && cur != "" && cur == old
}

// readPIDFile is the process a PID file names, 0 when it names none. A
// unit's PIDFile= is no file the agent chose: a FIFO there must not hang
// the status, a link not be followed.
func readPIDFile(p string) uint32 {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck // read-only
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return 0
	}
	b := make([]byte, 32)
	n, _ := f.Read(b)
	pid, err := strconv.ParseUint(strings.TrimSpace(string(b[:n])), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(pid)
}

// exeOf is the program file a process runs and whether it is gone (the
// kernel shows such a file as "<path> (deleted)"); "" when the process is
// none to see.
func exeOf(pid uint32) (string, bool) {
	target, err := os.Readlink(filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "exe"))
	if err != nil {
		return "", false
	}
	if p, ok := strings.CutSuffix(target, " (deleted)"); ok {
		return p, true
	}
	return target, false
}

// exeDeleted tells whether the program file of a process is gone.
func exeDeleted(pid uint32) bool {
	_, gone := exeOf(pid)
	return gone
}

// runPackageManager runs a change of the package manager's (pkg: update,
// install, remove, upgrade). Tests replace it.
var runPackageManager = runArgv

// pkgRunLimit bounds a package manager run that the API no longer waits
// for: generous, as one waits up to ten minutes for dpkg's lock
// (aptLockWait) and the downloads of every PHP branch can be slow, but a
// maintainer script that hangs must not hold the lock for good.
const pkgRunLimit = 2 * time.Hour
