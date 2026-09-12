package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
)

// The doctor tells the SELinux mode and today's denials of the web domain,
// naming the site to fix, so a 403 caused by a label is not mistaken for one
// caused by permissions.
func TestDoctorReportsSELinuxDenials(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	enforce := filepath.Join(t.TempDir(), "enforce")
	if err := os.WriteFile(enforce, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := selinuxEnforcePath
	selinuxEnforcePath = enforce
	t.Cleanup(func() { selinuxEnforcePath = prev })
	f := newSiteFixture(t)
	f.agent.ToolOutput["ausearch"] = strings.Join([]string{
		`type=AVC msg=audit(1757660000.100:200): avc:  denied  { read } for  pid=812 comm="sshd" name="x" scontext=system_u:system_r:sshd_t:s0 tcontext=unconfined_u:object_r:admin_home_t:s0 tclass=file permissive=0`,
		`type=AVC msg=audit(1757660001.200:201): avc:  denied  { getattr } for  pid=900 comm="php-fpm" path="/var/www/cms/data/www/wp.example.com/index.php" dev="vda1" ino=1 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:admin_home_t:s0 tclass=file permissive=0`,
		"",
	}, "\n")
	var d apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	var sel *apitypes.Check
	for i := range d.Checks {
		if d.Checks[i].Name == "selinux" {
			sel = &d.Checks[i]
		}
	}
	if sel == nil {
		t.Fatalf("no selinux check: %+v", d.Checks)
	}
	if sel.Status != "warn" || !strings.Contains(sel.Detail, "enforcing") || !strings.Contains(sel.Detail, "1 denial") || !strings.Contains(sel.Detail, "php-fpm") || !strings.Contains(sel.Detail, "admin_home_t") || !strings.Contains(sel.Detail, "mp site fix wp.example.com") {
		t.Fatalf("selinux check: %+v", sel)
	}
	if sel.Action != "site.fix" || sel.Target != "wp.example.com" {
		t.Fatalf("the finding must carry the fix for a button: %+v", sel)
	}

	// nginx records name only the file: the label still tells where it came from.
	f.agent.ToolOutput["ausearch"] = `type=AVC msg=audit(1757660002.300:202): avc:  denied  { getattr } for  pid=901 comm="nginx" name="label.php" dev="vda1" ino=2 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:admin_home_t:s0 tclass=file permissive=0` + "\n"
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	for _, c := range d.Checks {
		if c.Name == "selinux" && (c.Status != "warn" || !strings.Contains(c.Detail, "nginx → label.php") || !strings.Contains(c.Detail, "mp site fix <domain>") || c.Action != "site.fix" || c.Target != "*") {
			t.Fatalf("selinux check for a name-only record: %+v", c)
		}
	}

	// After a fix today the doctor counts from the fix, not from midnight.
	if err := f.db.SetSetting(f.ctx, settingSELinuxRelabeledAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	sinceFix := false
	for _, tc := range f.agent.Tools() {
		if tc.Name == "ausearch" && len(tc.Args) >= 5 && tc.Args[2] == "-ts" && tc.Args[3] == time.Now().Format("01/02/06") {
			sinceFix = true
		}
	}
	if !sinceFix {
		t.Fatalf("ausearch must start at the fix time: %+v", f.agent.Tools())
	}
	for _, c := range d.Checks {
		if c.Name == "selinux" && !strings.Contains(c.Detail, "since the fix at") {
			t.Fatalf("the detail must say the count starts at the fix: %+v", c)
		}
	}

	// Nothing denied: ausearch exits 1 and prints nothing in --raw mode; the line is green.
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "ausearch" {
			return &agent.ToolResponse{ExitCode: 1}
		}
		return nil
	}
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	for _, c := range d.Checks {
		if c.Name == "selinux" && (c.Status != "ok" || !strings.Contains(c.Detail, "no denials")) {
			t.Fatalf("selinux without denials: %+v", c)
		}
	}
}

// The switch lives in the panel with its warning: permissive turns the live
// mode and the boot configuration, keeps the file's comments, and the doctor
// says so from then on; enforcing turns it back.
func TestSELinuxSwitch(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	dir := t.TempDir()
	enforce, cfg := filepath.Join(dir, "enforce"), filepath.Join(dir, "config")
	os.WriteFile(enforce, []byte("1\n"), 0o644)
	os.WriteFile(cfg, []byte("# This file controls the state of SELinux on the system.\nSELINUX=enforcing\nSELINUXTYPE=targeted\n"), 0o644)
	prevE, prevC := selinuxEnforcePath, selinuxConfigPath
	selinuxEnforcePath, selinuxConfigPath = enforce, cfg
	t.Cleanup(func() { selinuxEnforcePath, selinuxConfigPath = prevE, prevC })
	f := newSiteFixture(t)
	f.agent.WriteThrough = func(p string) bool { return p == cfg }
	// the fake agent does not run setenforce: mirror it by hand when asked
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "setenforce" {
			os.WriteFile(enforce, []byte(req.Args[0]+"\n"), 0o644)
			return &agent.ToolResponse{}
		}
		return nil
	}

	var st apitypes.SELinuxStatus
	f.call(http.MethodGet, "/system/selinux", nil, http.StatusOK, &st)
	if !st.Supported || st.Mode != "enforcing" || st.Configured != "enforcing" || st.Warning == "" {
		t.Fatalf("status must carry the warning text to show before switching: %+v", st)
	}
	f.call(http.MethodPut, "/system/selinux", map[string]any{"mode": "disabled"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPut, "/system/selinux", map[string]any{"mode": "permissive"}, http.StatusOK, &st)
	if st.Mode != "permissive" || st.Configured != "permissive" || st.Warning == "" {
		t.Fatalf("after switching: %+v", st)
	}
	written, _ := f.agent.File(cfg)
	if !strings.HasPrefix(written, "# This file controls") || !strings.Contains(written, "SELINUX=permissive\n") || !strings.Contains(written, "SELINUXTYPE=targeted") {
		t.Fatalf("config must keep its comments and change one line:\n%s", written)
	}
	var d apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	found := false
	for _, c := range d.Checks {
		if c.Name == "selinux" {
			found = c.Status == "warn" && strings.Contains(c.Detail, "permissive") && c.Action == "selinux.enforcing"
		}
	}
	if !found {
		t.Fatalf("doctor must warn about permissive: %+v", d.Checks)
	}
	st = apitypes.SELinuxStatus{} // omitempty: a stale warning would survive the decode
	f.call(http.MethodPut, "/system/selinux", map[string]any{"mode": "enforcing"}, http.StatusOK, &st)
	if st.Mode != "enforcing" || st.Configured != "enforcing" {
		t.Fatalf("back to enforcing: %+v", st)
	}
}
