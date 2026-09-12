package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// nginx records name only the file: the label still tells where it came from.
	f.agent.ToolOutput["ausearch"] = `type=AVC msg=audit(1757660002.300:202): avc:  denied  { getattr } for  pid=901 comm="nginx" name="label.php" dev="vda1" ino=2 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:admin_home_t:s0 tclass=file permissive=0` + "\n"
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	for _, c := range d.Checks {
		if c.Name == "selinux" && (c.Status != "warn" || !strings.Contains(c.Detail, "nginx → label.php") || !strings.Contains(c.Detail, "mp site fix <domain>")) {
			t.Fatalf("selinux check for a name-only record: %+v", c)
		}
	}

	// Nothing denied: the mode alone, and the line is green.
	f.agent.ToolOutput["ausearch"] = ""
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &d)
	for _, c := range d.Checks {
		if c.Name == "selinux" && (c.Status != "ok" || !strings.Contains(c.Detail, "enforcing")) {
			t.Fatalf("selinux without denials: %+v", c)
		}
	}
}
