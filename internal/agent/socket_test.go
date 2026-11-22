package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

// The agent's socket is made with its mode and handed to the service group
// without following a symlink: the run directory is writable by that group,
// and a chmod or chown by name could be pointed at another file.
func TestAgentSocketIsNotChangedThroughALink(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	grp, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Skip(err)
	}
	// A short path: unix sockets are capped near 100 bytes.
	run, err := os.MkdirTemp("", "mpsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RunDir, cfg.ServiceUser, cfg.ServiceGroup = run, me.Username, grp.Name
	sock := cfg.AgentSocket()
	if err := os.Symlink(victim, sock); err != nil {
		t.Fatal(err)
	}
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	p, _ := osprofile.FromRelease(rel)
	s := NewServer(cfg, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	var st os.FileInfo
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st, err = os.Lstat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
			break
		}
	}
	if st == nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no socket: %v %v", st, err)
	}
	if st.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %v, want 0660", st.Mode().Perm())
	}
	if vst, _ := os.Stat(victim); vst.Mode().Perm() != 0o600 {
		t.Errorf("the planted link's target changed to %v", vst.Mode().Perm())
	}
}
