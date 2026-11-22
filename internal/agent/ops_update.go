package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"monopanel/internal/buildinfo"
	"monopanel/internal/systemd"
	"monopanel/internal/updater"
)

// updateUnit is the transient unit that installs a new panel version. It is
// started by systemd rather than forked here, so restarting the agent (which
// installing the package does) cannot kill the update halfway through.
const updateUnit = "monopanel-update.service"

// maxPackageSize bounds the copy of a staged package: a panel package is a
// few tens of megabytes.
const maxPackageSize = 512 << 20

var (
	versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~-]{0,63}$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// openUpdatesDir opens the agent's update directory (root's, 0700). Tests,
// which cannot make directories of root's, replace it.
var openUpdatesDir = func(p string) (int, error) { return ensureRootDir(p, 0o700) }

// installPanel installs a release the API downloaded into its downloads
// directory. That directory is the API's: whatever is checked there could be
// swapped before the package manager reads it. So the package is first copied
// into the agent's own directory (root's alone), without following a symlink,
// and the copy is what gets checked and installed.
//
// What the copy must hash to is settled before anything is copied: the
// request's digest, and with a release key the one the signed checksums give
// the release's package. A copy that does not hash to it never takes the
// staged name — the name an update-run started by an earlier request is about
// to hand to the package manager.
func (s *Server) installPanel(ctx context.Context, req *InstallPanelRequest) (*InstallPanelResponse, error) {
	staged, sum, signed, err := s.stageRelease(req)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, &Error{Message: "locate the panel binary", Output: err.Error()}
	}
	argv := []string{exe, "update-run", "--package", staged, "--sha256", sum, "--version", req.Version, "--from", buildinfo.Version}
	if p := s.cfg.Path(); p != "" {
		argv = append(argv, "--config", p)
	}
	// Not while the package manager runs: the update's own run of it, the
	// install of the panel's package, would meet that run's lock (apt-get
	// gives up after ten minutes), and the restarts the update ends in make
	// the API's jobs start over. The agent's runs hold pkgMu, those an
	// earlier agent left going are waited for; the runs after it wait for
	// the update in turn (pkg).
	s.pkgMu.Lock()
	defer s.pkgMu.Unlock()
	s.waitPkgRuns(ctx, pkgUnitPattern)
	if err := ctx.Err(); err != nil {
		return nil, &Error{Message: "the update is not started: the request ended while it waited for the package manager", Output: err.Error()}
	}
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
	}
	defer conn.Close()
	if err := conn.RunDetached(ctx, updateUnit, "MonoPanel update to "+req.Version, argv); err != nil {
		return nil, &Error{Message: "start the update", Output: err.Error()}
	}
	s.log.Info("panel update started", "unit", updateUnit, "version", req.Version, "signed", signed)
	return &InstallPanelResponse{Unit: updateUnit, Started: true, Signed: signed}, nil
}

// stageRelease checks an install request and stages its package: the copy
// in the agent's update directory, its SHA-256, and whether a signature
// vouched for it.
func (s *Server) stageRelease(req *InstallPanelRequest) (staged, sum string, signed bool, err error) {
	if !versionRe.MatchString(req.Version) {
		return "", "", false, &Error{Status: http.StatusBadRequest, Message: "invalid version"}
	}
	pkg := filepath.Clean(req.Package)
	// The name is the release's own (updater.AssetName): the signed checksum
	// list names it, and with it the version being installed.
	name := updater.AssetName(updater.Kind(string(s.profile.Family())), runtime.GOARCH, req.Version)
	if filepath.Dir(pkg) != s.cfg.DownloadsDir() || filepath.Base(pkg) != name {
		return "", "", false, &Error{Status: http.StatusBadRequest, Message: "package must be " + name + " staged in " + s.cfg.DownloadsDir()}
	}
	// An older release would bring back what later ones fixed, the agent's
	// own checks included: the API alone does not get to go back.
	if updater.Downgrade(buildinfo.Version, req.Version) {
		return "", "", false, &Error{Status: http.StatusForbidden, Message: downgradeRefusal(req.Version, buildinfo.Version)}
	}
	if !sha256Re.MatchString(req.SHA256) {
		return "", "", false, &Error{Status: http.StatusBadRequest, Message: "the package digest is not a SHA-256"}
	}
	want := strings.ToLower(req.SHA256)
	if key, _ := updater.ReleaseKey(s.cfg.Update.PublicKey); key != "" {
		if req.Sums == "" || req.Sig == "" {
			return "", "", false, &Error{Status: http.StatusBadRequest, Message: "release is unsigned but this host requires a signature"}
		}
		if err := updater.VerifySums(key, []byte(req.Sums), []byte(req.Sig)); err != nil {
			return "", "", false, &Error{Status: http.StatusBadRequest, Message: "release signature: " + err.Error()}
		}
		if listed := updater.ParseSums([]byte(req.Sums))[name]; !strings.EqualFold(listed, want) {
			return "", "", false, &Error{Status: http.StatusBadRequest, Message: "package is not the one the signed checksums describe"}
		}
		signed = true
	} else {
		s.log.Warn("installing a panel package without a signature check: update.public_key is none", "package", pkg)
	}
	staged, err = s.stagePackage(pkg, want)
	if err != nil {
		return "", "", false, err
	}
	return staged, want, signed, nil
}

// layoutRelease is the release that changed the host in ways no earlier one
// can run: sites in Apache mode on their accounts' own Apaches, which the
// earlier ones render for the shared one, retired by then; apps whose env
// file mp app-exec reads, a command the binaries before it lack; the site
// logs in the log tree, which the logrotate and fail2ban configuration of
// the earlier ones do not watch. Going back below it takes the steps the
// documentation lists, which the package's preremove prints too.
const layoutRelease = "0.9.0"

// downgradeDoc is where the documentation describes going back.
const downgradeDoc = `"Going back to an older version" in the documentation (https://monopanel.app/en/docs/update/)`

// downgradeRefusal is why installing version to over from is refused, and
// where the way back is described for whoever has to take it. It does not
// suggest installing the older package by hand: below layoutRelease that
// leaves sites and apps down until they are applied again.
func downgradeRefusal(to, from string) string {
	msg := fmt.Sprintf("installing %s over %s is a downgrade, which the panel does not do: an older release brings back what later ones fixed", to, from)
	if updater.Predates(to, layoutRelease) {
		msg += fmt.Sprintf(", and one older than %s cannot run what %s changed on this host: a site in Apache mode breaks once the older panel applies it, an app with an env file at its next start, and nothing rotates or watches the site logs, until Apache, the sites and the apps are applied again by hand", layoutRelease, layoutRelease)
	}
	return msg + "; should going back be unavoidable, read " + downgradeDoc + " before installing an older package as root"
}

// stagePackage copies the package pkg (in the downloads directory) into the
// agent's update directory, where it takes pkg's name once its SHA-256 is
// want, and returns the copy. The downloads directory is reached without
// following a symlink below the data directory, the package is opened without
// following one either, and it must be a plain file.
func (s *Server) stagePackage(pkg, want string) (string, error) {
	src, err := openDirBelow(s.cfg.DataDir, filepath.Dir(pkg), false, 0)
	if err != nil {
		return "", &Error{Status: http.StatusBadRequest, Message: "no such package: " + pkg, Output: err.Error()}
	}
	defer unix.Close(src) //nolint:errcheck // a directory opened for reading
	dir := s.cfg.AgentUpdatesDir()
	dst, err := openUpdatesDir(dir)
	if err != nil {
		return "", &Error{Message: "prepare " + dir, Output: err.Error()}
	}
	defer unix.Close(dst) //nolint:errcheck // a directory opened for reading
	name := filepath.Base(pkg)
	err = copyPackage(src, dst, name, want)
	switch {
	case errors.Is(err, errDigest):
		return "", &Error{Status: http.StatusBadRequest, Message: "package digest does not match the request"}
	case errors.Is(err, errNotPackage):
		return "", &Error{Status: http.StatusBadRequest, Message: pkg + ": " + err.Error()}
	case err != nil:
		return "", &Error{Message: "stage the package", Output: err.Error()}
	}
	pruneStaged(dst, name)
	s.pruneLegacyBackups()
	return filepath.Join(dir, name), nil
}

// pruneLegacyBackups removes the binaries updates up to 0.8.19 kept in the
// API's data directory (UpdatesDir/monopanel-<version>): nothing reads them
// any more, and they were the API's to rewrite.
func (s *Server) pruneLegacyBackups() {
	dfd, err := openDirBelow(s.cfg.DataDir, s.cfg.UpdatesDir(), false, 0)
	if err != nil {
		return
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	for _, n := range dirNames(dfd) {
		if strings.HasPrefix(n, "monopanel-") {
			_ = unix.Unlinkat(dfd, n, 0)
		}
	}
}

// dirNames lists the directory dfd, read through a descriptor of its own.
func dirNames(dfd int) []string {
	dup, err := unix.Dup(dfd)
	if err != nil {
		return nil
	}
	d := os.NewFile(uintptr(dup), "")
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	return names
}

var (
	errNotPackage = errors.New("not a package file")
	errDigest     = errors.New("the package digest does not match")
)

// copyPackage copies name from the directory src into the directory dst
// under a name of its own, and renames the copy to name once its SHA-256 is
// want: whatever an earlier request staged there stays unless this copy is
// the package both were checked to be.
func copyPackage(src, dst int, name, want string) error {
	fd, err := unix.Openat(src, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%w: %v", errNotPackage, err)
		}
		return err
	}
	in := os.NewFile(uintptr(fd), name)
	defer in.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: not a plain file", errNotPackage)
	}
	if st.Size > maxPackageSize {
		return fmt.Errorf("%w: %d bytes, more than a panel package has", errNotPackage, st.Size)
	}
	// A name no other request uses: two requests at once each write their
	// own copy, and neither renames the other's.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := "." + hex.EncodeToString(nonce[:]) + ".part"
	wfd, err := unix.Openat(dst, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(wfd), tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, maxPackageSize+1))
	if err == nil && n > maxPackageSize {
		err = fmt.Errorf("%w: it grew past %d bytes while being copied", errNotPackage, int64(maxPackageSize))
	}
	if err == nil && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
		err = errDigest
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(dst, tmp, dst, name)
	}
	if err != nil {
		_ = unix.Unlinkat(dst, tmp, 0)
		return err
	}
	return nil
}

// pruneStaged removes packages staged by earlier updates and copies left by
// requests that failed: only the one being installed is of any use. The
// binary backups of update-run stay.
func pruneStaged(dfd int, keep string) {
	for _, n := range dirNames(dfd) {
		if n == keep || !(strings.HasSuffix(n, ".deb") || strings.HasSuffix(n, ".rpm") || strings.HasSuffix(n, ".part")) {
			continue
		}
		_ = unix.Unlinkat(dfd, n, 0) // a directory stays: unlinkat without AT_REMOVEDIR
	}
}
