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

func writeState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := StateFile(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, StateFile(dir))
}

// Installer replaces the running panel with a staged package. It runs as root
// in a transient unit of its own, because installing the package restarts both
// the API and the agent — whoever asked for the update is gone by step three.
type Installer struct {
	Package string   // staged .deb or .rpm
	SHA256  string   // expected digest of Package
	To      string   // version being installed
	From    string   // version being replaced
	Dir     string   // state and binary backups
	Binary  string   // /usr/bin/monopanel
	Argv    []string // package manager command installing Package
	Units   []string // units to restart, the API last
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
	if err := writeState(i.Dir, st); err != nil {
		return err
	}
	finish := func(status string, cause error, output string) error {
		st.Status, st.Finished, st.Log = status, time.Now(), tail(output, 4000)
		if cause != nil {
			st.Error = cause.Error()
		}
		if werr := writeState(i.Dir, st); werr != nil {
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

	i.log().Info("installing panel package", "package", i.Package, "version", i.To)
	out, err := i.run(ctx, i.Timeout, i.Argv...)
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
	if err := os.MkdirAll(i.Dir, 0o755); err != nil {
		return "", err
	}
	name := "monopanel-" + Version(i.From)
	if i.From == "" {
		name = "monopanel-previous"
	}
	dst := filepath.Join(i.Dir, name)
	if err := os.WriteFile(dst, src, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

// pruneBackups keeps only the binary we can still roll back to; each one is
// tens of megabytes and older ones are of no use once an update succeeded.
func (i *Installer) pruneBackups(keep string) {
	entries, err := os.ReadDir(i.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(i.Dir, e.Name())
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
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C.UTF-8")
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
