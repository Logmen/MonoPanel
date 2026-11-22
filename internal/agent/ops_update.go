package agent

import (
	"context"
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

var versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~-]{0,63}$`)

// installPanel installs a release the API downloaded into its downloads
// directory. That directory is the API's: whatever is checked there could be
// swapped before the package manager reads it. So the package is first copied
// into the agent's own directory (root's alone), without following a symlink,
// and the copy is what gets checked and installed.
func (s *Server) installPanel(ctx context.Context, req *InstallPanelRequest) (*InstallPanelResponse, error) {
	if !versionRe.MatchString(req.Version) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid version"}
	}
	pkg := filepath.Clean(req.Package)
	// The name is the release's own (updater.AssetName): the signed checksum
	// list names it, and with it the version being installed.
	name := updater.AssetName(updater.Kind(string(s.profile.Family())), runtime.GOARCH, req.Version)
	if filepath.Dir(pkg) != s.cfg.DownloadsDir() || filepath.Base(pkg) != name {
		return nil, &Error{Status: http.StatusBadRequest, Message: "package must be " + name + " staged in " + s.cfg.DownloadsDir()}
	}
	// An older release would bring back what later ones fixed, the agent's
	// own checks included: the API alone does not get to go back.
	if updater.Newer(req.Version, buildinfo.Version) {
		return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("installing %s over %s is a downgrade, which the panel does not do: install the older package by hand as root", req.Version, buildinfo.Version)}
	}
	staged, sum, err := s.stagePackage(pkg)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(sum, req.SHA256) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "package digest does not match the request"}
	}

	signed := false
	if key, _ := updater.ReleaseKey(s.cfg.Update.PublicKey); key != "" {
		if req.Sums == "" || req.Sig == "" {
			return nil, &Error{Status: http.StatusBadRequest, Message: "release is unsigned but this host requires a signature"}
		}
		if err := updater.VerifySums(key, []byte(req.Sums), []byte(req.Sig)); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "release signature: " + err.Error()}
		}
		if want := updater.ParseSums([]byte(req.Sums))[name]; !strings.EqualFold(want, sum) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "package is not the one the signed checksums describe"}
		}
		signed = true
	} else {
		s.log.Warn("installing a panel package without a signature check: update.public_key is none", "package", staged)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, &Error{Message: "locate the panel binary", Output: err.Error()}
	}
	argv := []string{exe, "update-run", "--package", staged, "--sha256", sum, "--version", req.Version, "--from", buildinfo.Version}
	if p := s.cfg.Path(); p != "" {
		argv = append(argv, "--config", p)
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

// stagePackage copies the package pkg (in the downloads directory) into the
// agent's update directory and returns the copy and its SHA-256. The
// downloads directory is reached without following a symlink below the data
// directory, the package is opened without following one either, and it
// must be a plain file.
func (s *Server) stagePackage(pkg string) (string, string, error) {
	src, err := openDirBelow(s.cfg.DataDir, filepath.Dir(pkg), false, 0)
	if err != nil {
		return "", "", &Error{Status: http.StatusBadRequest, Message: "no such package: " + pkg, Output: err.Error()}
	}
	defer unix.Close(src) //nolint:errcheck // a directory opened for reading
	dir := s.cfg.AgentUpdatesDir()
	dst, err := ensureRootDir(dir, 0o700)
	if err != nil {
		return "", "", &Error{Message: "prepare " + dir, Output: err.Error()}
	}
	defer unix.Close(dst) //nolint:errcheck // a directory opened for reading
	name := filepath.Base(pkg)
	sum, err := copyPackage(src, dst, name)
	switch {
	case errors.Is(err, errNotPackage):
		return "", "", &Error{Status: http.StatusBadRequest, Message: pkg + ": " + err.Error()}
	case err != nil:
		return "", "", &Error{Message: "stage the package", Output: err.Error()}
	}
	pruneStaged(dst, dir, name)
	s.pruneLegacyBackups()
	return filepath.Join(dir, name), sum, nil
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
	dup, err := unix.Dup(dfd)
	if err != nil {
		return
	}
	d := os.NewFile(uintptr(dup), s.cfg.UpdatesDir())
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	for _, n := range names {
		if strings.HasPrefix(n, "monopanel-") {
			_ = unix.Unlinkat(dfd, n, 0)
		}
	}
}

var errNotPackage = errors.New("not a package file")

// copyPackage copies name from the directory src into the directory dst
// (made next to the name and renamed over it) and returns its SHA-256.
func copyPackage(src, dst int, name string) (string, error) {
	fd, err := unix.Openat(src, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("%w: %v", errNotPackage, err)
		}
		return "", err
	}
	in := os.NewFile(uintptr(fd), name)
	defer in.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("%w: not a plain file", errNotPackage)
	}
	if st.Size > maxPackageSize {
		return "", fmt.Errorf("%w: %d bytes, more than a panel package has", errNotPackage, st.Size)
	}
	tmp := "." + name + ".part"
	_ = unix.Unlinkat(dst, tmp, 0)
	wfd, err := unix.Openat(dst, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", err
	}
	out := os.NewFile(uintptr(wfd), tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, maxPackageSize+1))
	if err == nil && n > maxPackageSize {
		err = fmt.Errorf("%w: it grew past %d bytes while being copied", errNotPackage, int64(maxPackageSize))
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
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pruneStaged removes packages staged by earlier updates: only the one being
// installed is of any use. The binary backups of update-run stay.
func pruneStaged(dfd int, dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if n == keep || e.IsDir() || !(strings.HasSuffix(n, ".deb") || strings.HasSuffix(n, ".rpm") || strings.HasSuffix(n, ".part")) {
			continue
		}
		_ = unix.Unlinkat(dfd, n, 0)
	}
}
