package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/mailpolicy"
)

// The probe asks where postfix reaches the policy on this OS family — the
// caller names nothing — and says why it got no answer.
func TestMailPolicyProbe(t *testing.T) {
	for rel, want := range map[string]string{debian13: mailpolicy.SocketPath, alma9: mailpolicy.TCPAddress} {
		s := policyServer(t, rel)
		res, err := s.mailPolicyProbe(context.Background(), &struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Listener != want {
			t.Errorf("%s: asked %s, want %s", rel, res.Listener, want)
		}
		if rel == debian13 && (res.OK || res.Error == "") {
			t.Errorf("%s: no policy runs on this machine, yet: %+v", rel, res)
		}
	}
}

// The probe reads main.cf — postfix asks the policy only where its milter
// lists begin with it (review r2 F1) — and on EL makes sure that what
// listens on the port is systemd's socket, not an account's process that
// took the port (reviews r1 F2, r2 F4/F10).
func TestMailPolicyProbeChecksMainCfAndTheListener(t *testing.T) {
	dir := t.TempDir()
	s := policyServer(t, alma9)
	s.mainCFPath = filepath.Join(dir, "main.cf")
	el := mailpolicy.ListenerFor(false)
	if err := os.WriteFile(s.mainCFPath, []byte("smtpd_milters = "+el.MilterToken()+", inet:127.0.0.1:8891\nnon_smtpd_milters = "+el.MilterToken()+", inet:127.0.0.1:8891\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc", "net")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	s.procRootPath = dir
	squatter := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:22BA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3001 1 0000000000000000 100 0 0 10 0\n"
	if err := os.WriteFile(filepath.Join(proc, "tcp"), []byte(squatter), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.mailPolicyProbe(context.Background(), &struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Configured || res.OK || !strings.Contains(res.Error, "uid 1000 listens on 127.0.0.1:8890") {
		t.Errorf("an account on the policy's port: %+v", res)
	}
	if err := os.WriteFile(s.mainCFPath, []byte("smtpd_milters = inet:127.0.0.1:8891\nnon_smtpd_milters = inet:127.0.0.1:8891\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.mailPolicyProbe(context.Background(), &struct{}{}); res.Configured || !strings.Contains(res.ConfigError, "smtpd_milters does not begin with") {
		t.Errorf("main.cf without the policy: %+v", res)
	}
}
