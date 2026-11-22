package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"monopanel/internal/systemd"
)

// Update states written to state.json. The panel restarts in the middle of an
// update, so the file is the only thing that survives to tell what happened.
const (
	StatusInstalling = "installing"
	StatusDone       = "done"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled-back"
)

// State is the outcome of the last update attempt.
type State struct {
	Status   string    `json:"status"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	Error    string    `json:"error,omitempty"`
	Log      string    `json:"log,omitempty"`
}

// StateFile is where an in-flight update records itself.
func StateFile(dir string) string { return filepath.Join(dir, "state.json") }

// ReadState returns the last update state; a missing file is not an error.
func ReadState(dir string) (*State, error) {
	b, err := os.ReadFile(StateFile(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// writeStateFile is the plain writer of the state, for tests and for a Dir
// nobody but root may write to.
func writeStateFile(dir string, b []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := StateFile(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, StateFile(dir))
}

func (i *Installer) writeState(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	write := i.WriteState
	if write == nil {
		write = writeStateFile
	}
	return write(i.Dir, append(b, '\n'))
}

func (i *Installer) backupDir() string {
	if i.BackupDir != "" {
		return i.BackupDir
	}
	return i.Dir
}

// Packages is what the installer needs of the host's package manager; the
// panel's (osprofile.PackageManager) has it all.
type Packages interface {
	// Env is the package manager's environment: apt's noninteractive
	// frontend, and NEEDRESTART_MODE=l, without which Ubuntu's needrestart
	// restarts every service an upgrade left on an old library — the
	// database server too, and none after the configuration check the panel
	// runs before restarts of its own.
	Env() []string
	InstallArgv(pkgs []string) []string
	// ReinstallArgv installs the package again over its own version, and
	// PackageFileArgv prints a package file's "<name> <version>" in the form
	// QueryInstalledArgv and ParseQuery give the installed version.
	ReinstallArgv(pkgs []string) []string
	PackageFileArgv(file string) []string
	QueryInstalledArgv(pkgs []string) []string
	ParseQuery(out string) map[string]string
}

// defaultEnv is the package manager's environment when Packages gives none.
var defaultEnv = []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "LC_ALL=C.UTF-8"}

// Installer replaces the running panel with a staged package. It runs as root
// in a transient unit of its own, because installing the package restarts both
// the API and the agent — whoever asked for the update is gone by step three.
type Installer struct {
	Package string // staged .deb or .rpm
	SHA256  string // expected digest of Package
	To      string // version being installed
	From    string // version being replaced
	Dir     string // state of the update, which the API reads
	// BackupDir keeps the binary being replaced (Dir when empty). A rollback
	// copies it over Binary as root, so nobody but root may write there.
	BackupDir string
	// WriteState writes the state file into Dir; nil writes it by path. The
	// panel's Dir belongs to the API, and root must not follow a symlink the
	// API left there.
	WriteState func(dir string, data []byte) error
	Binary     string   // /usr/bin/monopanel
	Packages   Packages // the package manager that installs Package
	Units      []string // units to restart, the API last
	// Ready reports the version the panel answers with, and is polled until it
	// matches To. Without it an update is called done as soon as it installs.
	Ready func(context.Context) (string, error)
	// Timeout bounds the package manager; Settle bounds the readiness wait.
	Timeout, Settle time.Duration
	Log             *slog.Logger
}

func (i *Installer) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

// Run installs the package and restarts the panel, rolling back to the
// previous binary if the new one fails to install or fails to answer.
func (i *Installer) Run(ctx context.Context) (err error) {
	if i.Timeout <= 0 {
		i.Timeout = 10 * time.Minute
	}
	if i.Settle <= 0 {
		i.Settle = 90 * time.Second
	}
	st := &State{Status: StatusInstalling, From: i.From, To: i.To, Started: time.Now()}
	if err := i.writeState(st); err != nil {
		return err
	}
	finish := func(status string, cause error, output string) error {
		st.Status, st.Finished, st.Log = status, time.Now(), tail(output, 4000)
		if cause != nil {
			st.Error = cause.Error()
		}
		if werr := i.writeState(st); werr != nil {
			i.log().Error("update state", "err", werr)
		}
		return cause
	}

	sum, err := FileSHA256(i.Package)
	if err != nil {
		return finish(StatusFailed, err, "")
	}
	if !strings.EqualFold(sum, i.SHA256) {
		return finish(StatusFailed, fmt.Errorf("package digest %s does not match the release", sum), "")
	}

	backup, err := i.backupBinary()
	if err != nil {
		return finish(StatusFailed, err, "")
	}

	out, err := i.install(ctx)
	if err != nil {
		i.restore(ctx, backup)
		return finish(StatusFailed, fmt.Errorf("package install failed: %w", err), out)
	}

	if err := i.restart(ctx); err != nil {
		i.restore(ctx, backup)
		_ = i.restart(ctx)
		return finish(StatusRolledBack, err, out)
	}
	if i.Ready != nil {
		if err := i.wait(ctx); err != nil {
			i.log().Error("new version did not answer; rolling back", "err", err)
			i.restore(ctx, backup)
			if rerr := i.restart(ctx); rerr != nil {
				i.log().Error("restart after rollback", "err", rerr)
			}
			return finish(StatusRolledBack, err, out)
		}
	}
	i.log().Info("panel updated", "from", i.From, "to", i.To)
	i.pruneBackups(backup)
	return finish(StatusDone, nil, out)
}

// install has the package manager install Package — again, when the package
// database holds its version already: an update that rolled back put the
// previous binary back, while the package database kept the version it
// rolled back from, its scripts having run, so that an install of the same
// file would change nothing ("already the newest version", "Nothing to do")
// and the retry would roll back once more. The output says so at its end,
// which the state keeps. What keeps that from being told — the file's query
// fails — leaves the install as it was.
func (i *Installer) install(ctx context.Context) (string, error) {
	if i.Packages == nil {
		return "", errors.New("no install command for this system")
	}
	argv, note := i.Packages.InstallArgv([]string{i.Package}), ""
	version, err := i.installedAlready(ctx)
	switch {
	case err != nil:
		i.log().Warn("cannot tell whether the package database holds this version already; installing as usual", "package", i.Package, "err", err)
	case version != "":
		if again := i.Packages.ReinstallArgv([]string{i.Package}); len(again) > 0 {
			argv = again
			note = "the package database held " + version + " already, so it was installed again"
		}
	}
	i.log().Info("installing panel package", "package", i.Package, "version", i.To, "reinstall", note != "")
	out, err := i.run(ctx, i.Timeout, argv...)
	if note != "" {
		out = strings.TrimSpace(out + "\n" + note)
	}
	return out, err
}

// installedAlready is Package's version when the package database holds
// that very version of it, "" when it holds another one or none. A package
// not installed at all — a panel set up from the binary alone taking the
// package now — fails the query (dpkg-query, rpm -q), which says just that.
func (i *Installer) installedAlready(ctx context.Context) (string, error) {
	q := i.Packages.PackageFileArgv(i.Package)
	if len(q) == 0 {
		return "", nil
	}
	out, err := i.run(ctx, time.Minute, q...)
	if err != nil {
		return "", err
	}
	name, version, ok := nameVersion(out)
	if !ok {
		return "", fmt.Errorf("%s: no package name and version in %q", i.Package, tail(out, 200))
	}
	installed, _ := i.run(ctx, time.Minute, i.Packages.QueryInstalledArgv([]string{name})...)
	if v := i.Packages.ParseQuery(installed)[name]; v != version {
		return "", nil
	}
	return version, nil
}

// nameVersion reads the first "<name> <version>" line of a package file's
// query; a warning the tool prints on the way (rpm's about a signature key)
// is none.
func nameVersion(out string) (name, version string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && !strings.HasSuffix(f[0], ":") {
			return f[0], f[1], true
		}
	}
	return "", "", false
}

// backupBinary keeps the running executable so a bad release can be undone
// without network access.
func (i *Installer) backupBinary() (string, error) {
	if i.Binary == "" {
		return "", nil
	}
	src, err := os.ReadFile(i.Binary)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", i.Binary, err)
	}
	dir := i.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "monopanel-" + Version(i.From)
	if i.From == "" {
		name = "monopanel-previous"
	}
	dst := filepath.Join(dir, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, src, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// pruneBackups keeps only the binary we can still roll back to; each one is
// tens of megabytes and older ones are of no use once an update succeeded.
func (i *Installer) pruneBackups(keep string) {
	dir := i.backupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() || path == keep || !strings.HasPrefix(e.Name(), "monopanel-") {
			continue
		}
		if err := os.Remove(path); err != nil {
			i.log().Warn("remove old backup", "path", path, "err", err)
		}
	}
}

func (i *Installer) restore(ctx context.Context, backup string) {
	if backup == "" || i.Binary == "" {
		return
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		i.log().Error("rollback: backup unreadable", "path", backup, "err", err)
		return
	}
	// Write through a temporary file: the old binary may still be executing.
	tmp := i.Binary + ".rollback"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		i.log().Error("rollback: write", "err", err)
		return
	}
	if err := os.Rename(tmp, i.Binary); err != nil {
		i.log().Error("rollback: rename", "err", err)
		return
	}
	i.log().Warn("rolled back to the previous binary", "version", i.From)
	_ = ctx
}

func (i *Installer) restart(ctx context.Context) error {
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return fmt.Errorf("systemd unreachable: %w", err)
	}
	defer conn.Close()
	if err := conn.DaemonReload(ctx); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	for _, unit := range i.Units {
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := conn.Restart(rctx, unit)
		cancel()
		if err != nil {
			return fmt.Errorf("restart %s: %w", unit, err)
		}
	}
	return nil
}

// wait polls the panel until it answers with the version just installed.
func (i *Installer) wait(ctx context.Context) error {
	deadline := time.Now().Add(i.Settle)
	var last error
	for time.Now().Before(deadline) {
		v, err := i.Ready(ctx)
		switch {
		case err != nil:
			last = err
		case Version(v) == Version(i.To):
			return nil
		default:
			last = fmt.Errorf("panel answers %s, expected %s", v, i.To)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("panel did not come up after the update: %w", last)
}

func (i *Installer) run(ctx context.Context, timeout time.Duration, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no install command for this system")
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	env := defaultEnv
	if i.Packages != nil && len(i.Packages.Env()) > 0 {
		env = i.Packages.Env()
	}
	cmd.Env = append(os.Environ(), env...)
	b, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(b))
	if err != nil {
		if out != "" {
			return out, fmt.Errorf("%s: %s", err, tail(out, 400))
		}
		return out, err
	}
	return out, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
