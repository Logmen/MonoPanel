package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func (f *siteFixture) installFail2ban(t *testing.T) {
	t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "fail2ban"}, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("fail2ban install: %s %s", job.Status, job.Error)
	}
}

// The sites write their own nginx logs, so the nginx jails list them — each by
// name, never the PHP error logs — and follow the sites as they come and go.
func TestFail2banReadsSiteLogs(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.installFail2ban(t)

	jail, ok := f.agent.File(fail2banJailPath)
	if !ok {
		t.Fatal("jail file not written")
	}
	logs := "/var/www/alex/data/logs/"
	for _, want := range []string{
		"logpath = /var/log/nginx/access.log\n          " + logs + "shop.example.com.access.log\n",
		"logpath = /var/log/nginx/error.log\n          " + logs + "shop.example.com.error.log\n",
		"ignoreip = 127.0.0.1/8 ::1\n",
		"/etc/fail2ban/jail.d/zz-local.local",
	} {
		if !strings.Contains(jail, want) {
			t.Errorf("jail lacks %q:\n%s", want, jail)
		}
	}
	for _, line := range strings.Split(jail, "\n") {
		if !strings.HasPrefix(line, "#") && (strings.Contains(line, "php.error.log") || strings.Contains(line, "*")) {
			t.Errorf("jail reads PHP logs or uses a mask: %q", line)
		}
	}

	// A new site joins, a deleted one leaves.
	f.createSite(map[string]any{"domain": "blog.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if jail, _ = f.agent.File(fail2banJailPath); !strings.Contains(jail, logs+"blog.example.com.access.log") || !strings.Contains(jail, logs+"blog.example.com.error.log") {
		t.Errorf("the new site is not in the jails:\n%s", jail)
	}
	var del struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/sites/shop.example.com", nil, http.StatusAccepted, &del)
	f.waitJob(del.JobID)
	if jail, _ = f.agent.File(fail2banJailPath); strings.Contains(jail, "shop.example.com") || !strings.Contains(jail, "blog.example.com") {
		t.Errorf("jails after a site was deleted:\n%s", jail)
	}
}

// Trusted addresses go to ignoreip; they are kept even while fail2ban is not
// installed and written when it is.
func TestFail2banTrustedAddresses(t *testing.T) {
	f := newSiteFixture(t)
	var st apitypes.FirewallStatus
	f.call(http.MethodPost, "/firewall/trust", map[string]any{"ip": "198.51.100.7"}, http.StatusOK, &st)
	f.call(http.MethodPost, "/firewall/trust", map[string]any{"ip": "not-an-address"}, http.StatusUnprocessableEntity, nil)
	f.installFail2ban(t)
	jail, _ := f.agent.File(fail2banJailPath)
	if !strings.Contains(jail, "ignoreip = 127.0.0.1/8 ::1 198.51.100.7\n") {
		t.Fatalf("ignoreip after install:\n%s", jail)
	}

	f.call(http.MethodPost, "/firewall/trust", map[string]any{"ip": "203.0.113.9/24"}, http.StatusOK, &st)
	if st.Fail2ban == nil || strings.Join(st.Fail2ban.Trusted, " ") != "198.51.100.7 203.0.113.0/24" {
		t.Fatalf("trusted list: %+v", st.Fail2ban)
	}
	if jail, _ = f.agent.File(fail2banJailPath); !strings.Contains(jail, "ignoreip = 127.0.0.1/8 ::1 198.51.100.7 203.0.113.0/24\n") {
		t.Fatalf("ignoreip:\n%s", jail)
	}
	unbanned := false
	for _, tool := range f.agent.Tools() {
		if tool.Name == "fail2ban-client" && strings.Join(tool.Args, " ") == "unban 203.0.113.0/24" {
			unbanned = true
		}
	}
	if !unbanned {
		t.Error("a trusted address keeps the ban it already had")
	}

	f.call(http.MethodPost, "/firewall/untrust", map[string]any{"ip": "198.51.100.7"}, http.StatusOK, &st)
	f.call(http.MethodPost, "/firewall/untrust", map[string]any{"ip": "198.51.100.7"}, http.StatusNotFound, nil)
	if jail, _ = f.agent.File(fail2banJailPath); !strings.Contains(jail, "ignoreip = 127.0.0.1/8 ::1 203.0.113.0/24\n") {
		t.Fatalf("ignoreip after untrust:\n%s", jail)
	}
}

// On EL the meta package drags in a sendmail provider (exim on a host without
// postfix) and firewalld; the server package alone is installed.
func TestFail2banInstallsServerOnlyOnEL(t *testing.T) {
	withOSRelease(t, "ID=ol\nID_LIKE=fedora\nVERSION_ID=10.2\n")
	f := newSiteFixture(t)
	f.installFail2ban(t)
	var pkgs []string
	for _, call := range f.agent.Calls() {
		var req struct {
			Action   string   `json:"action"`
			Packages []string `json:"packages"`
		}
		if call.Path == "/v1/pkg" && json.Unmarshal(call.Body, &req) == nil && req.Action == "install" {
			pkgs = append(pkgs, req.Packages...)
		}
	}
	got := " " + strings.Join(pkgs, " ") + " "
	if !strings.Contains(got, " fail2ban-server ") || strings.Contains(got, " fail2ban ") {
		t.Fatalf("installed on EL: %v", pkgs)
	}
}

// The doctor names what an older panel left behind and a newer one does not
// undo on its own: the fail2ban meta package on EL, and CodeReady Builder
// that 0.8.15 switched on — recognised by the panel's own log, so a
// repository the administrator enabled on purpose is left alone.
func TestDoctorNamesLeftovers(t *testing.T) {
	withOSRelease(t, "ID=ol\nID_LIKE=fedora\nVERSION_ID=10.2\n")
	f := newSiteFixture(t)
	f.installFail2ban(t)
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "dnf" && len(req.Args) > 2 && req.Args[2] == "repolist" {
			return &agentRes{Output: "repo id      repo name      status\nol10_appstream   AppStream   enabled\nol10_codeready_builder   CodeReady Builder   enabled\n"}
		}
		return nil
	}
	find := func(name string) string {
		for _, c := range f.s.doctor(f.ctx).Checks {
			if c.Name == name {
				return c.Status + ": " + c.Detail
			}
		}
		return ""
	}
	if got := find("fail2ban packages"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "dnf mark install fail2ban-server fail2ban-selinux && dnf remove fail2ban fail2ban-sendmail fail2ban-firewalld") {
		t.Errorf("fail2ban meta package: %q", got)
	}
	if got := find("repositories"); got != "" {
		t.Errorf("a repository the panel never touched is reported: %q", got)
	}

	// The line 0.8.15 left in the log of its mail install.
	jobs, err := f.db.ListJobs(f.ctx, 1, store.JobDone)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("no finished job to carry the mark: %v", err)
	}
	if err := f.db.AppendJobLog(f.ctx, jobs[0].ID, "repository ol10_codeready_builder enabled: opendkim needs libmilter and libmemcached from it\n"); err != nil {
		t.Fatal(err)
	}
	if got := find("repositories"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "dnf config-manager --set-disabled ol10_codeready_builder") {
		t.Errorf("CodeReady Builder left on by 0.8.15: %q", got)
	}

	// Cleaned up: both hints go.
	f.agent.MissingPackages = map[string]bool{"fail2ban": true, "fail2ban-sendmail": true, "fail2ban-firewalld": true, "exim": true}
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "dnf" {
			return &agentRes{Output: "repo id      repo name      status\nol10_codeready_builder   CodeReady Builder   disabled\n"}
		}
		return nil
	}
	if a, b := find("fail2ban packages"), find("repositories"); a != "" || b != "" {
		t.Errorf("hints stay after the cleanup: %q %q", a, b)
	}
}
