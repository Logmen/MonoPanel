package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

// mailTLSLabelCalls are the agent's semanage and restorecon calls on the
// mail server's self-signed pair, in order: "fcontext" for the rule,
// "restorecon" for a relabel of the two files.
func mailTLSLabelCalls(f *siteFixture) []string {
	dir := filepath.Join(f.s.cfg.DataDir, "mail")
	rule := []string{"fcontext", "-a", "-t", "cert_t", dir + `/tls\.(crt|key)`}
	relabel := []string{"-F", "-i", filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")}
	var out []string
	for _, tool := range f.agent.Tools() {
		switch {
		case tool.Name == "semanage" && slices.Equal(tool.Args, rule):
			out = append(out, "fcontext")
		case tool.Name == "restorecon" && slices.Equal(tool.Args, relabel):
			out = append(out, "restorecon")
		}
	}
	return out
}

// On EL smtpd may not read the self-signed pair in <data>/mail (var_lib_t):
// STARTTLS was "not available" and the webmail could not send while the
// mail name had no certificate. The install gives the two files the file
// context cert_t and labels them; a pair made later is labelled when it is
// made; a host whose mail server an older panel installed gets both once at
// the next start.
func TestSelfSignedMailCertificateIsLabelledOnEL(t *testing.T) {
	old := fixtureOSRelease
	fixtureOSRelease = "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"
	defer func() { fixtureOSRelease = old }()

	f := newSiteFixture(t)
	f.s.SetPortProbe(func(int, bool) (bool, string) { return true, "220 mx.example.com ESMTP" })
	var out struct {
		JobID int64 `json:"job_id"`
	}
	// No certificate for the name: the daemons get the self-signed pair.
	f.call(http.MethodPost, "/mail/install", map[string]any{"hostname": "mx.example.com"}, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("mail install: %s %s", job.Status, job.Error)
	}
	got := mailTLSLabelCalls(f)
	if i := slices.Index(got, "fcontext"); i < 0 || !slices.Contains(got[i:], "restorecon") {
		t.Fatalf("the install: rule and relabel of the self-signed pair %v", got)
	}

	// The pair is made again (expired, removed): it is labelled at once.
	cp := filepath.Join(f.s.cfg.DataDir, "mail", "tls.crt")
	if err := os.Remove(cp); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	if p, _, kind, _ := f.s.mailTLS(f.ctx, f.s.loadMailConfig(f.ctx)); p != cp || kind != store.CertKindSelfSigned {
		t.Fatalf("mailTLS: %s %s", p, kind)
	}
	if got := mailTLSLabelCalls(f); !slices.Equal(got, []string{"restorecon"}) {
		t.Fatalf("a new pair: %v", got)
	}
	// One that is there already is left alone.
	f.agent.Reset()
	f.s.mailTLS(f.ctx, f.s.loadMailConfig(f.ctx))
	if got := mailTLSLabelCalls(f); len(got) != 0 {
		t.Fatalf("the pair as it was: %v", got)
	}

	// The start of the API after the upgrade: once.
	f.agent.Reset()
	f.s.refreshSELinuxMailTLS(f.ctx)
	if v, _ := f.db.GetSetting(f.ctx, settingSELinuxMailTLS); v != "1" {
		t.Fatalf("flag %q", v)
	}
	if got := mailTLSLabelCalls(f); !slices.Equal(got, []string{"fcontext", "restorecon"}) {
		t.Fatalf("the first start: %v", got)
	}
	f.agent.Reset()
	f.s.refreshSELinuxMailTLS(f.ctx)
	if got := mailTLSLabelCalls(f); len(got) != 0 {
		t.Fatalf("the next start: %v", got)
	}

	// A semanage that fails leaves the flag unset: the next start retries.
	if err := f.db.SetSetting(f.ctx, settingSELinuxMailTLS, ""); err != nil {
		t.Fatal(err)
	}
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "semanage" {
			return &agent.ToolResponse{ExitCode: 1, Output: "ValueError: SELinux policy is not managed or store cannot be accessed."}
		}
		return nil
	}
	f.s.refreshSELinuxMailTLS(f.ctx)
	if v, _ := f.db.GetSetting(f.ctx, settingSELinuxMailTLS); v != "" {
		t.Fatalf("failed, but the flag is %q", v)
	}
}

// Elsewhere (no SELinux) nothing of it runs.
func TestSelfSignedMailCertificateNoLabelsOffEL(t *testing.T) {
	f := newSiteFixture(t)
	if err := f.s.saveMailConfig(f.ctx, mailConfig{Installed: true, Hostname: "mx.example.com"}); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	f.s.refreshSELinuxMailTLS(f.ctx)
	f.s.mailTLS(f.ctx, f.s.loadMailConfig(f.ctx))
	for _, tool := range f.agent.Tools() {
		if tool.Name == "semanage" || tool.Name == "restorecon" || strings.Contains(strings.Join(tool.Args, " "), "tls.crt") {
			t.Fatalf("on Debian: %s %v", tool.Name, tool.Args)
		}
	}
}
