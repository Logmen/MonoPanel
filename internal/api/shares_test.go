package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A shared folder: the guest's home stays its own, the owner's folder is
// bind-mounted into it by a unit, the guest gets ACLs, PHP in the folder is
// denied on the site when asked, and everything is undone on removal.
func TestSharedFolderLifecycle(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)

	var st apitypes.ShareStatus
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "upload/kaspy", "no_php": true}, http.StatusCreated, &st)
	if st.Share.Name != "kaspy" || st.Where != "/var/www/guest/kaspy" || st.Target != "/var/www/alex/data/www/shop.example.com/upload/kaspy" || !st.Mounted {
		t.Fatalf("share: %+v where=%s target=%s", st.Share, st.Where, st.Target)
	}
	unit, ok := f.agent.File("/etc/systemd/system/var-www-guest-kaspy.mount")
	if !ok || !strings.Contains(unit, "What=/var/www/alex/data/www/shop.example.com/upload/kaspy\nWhere=/var/www/guest/kaspy\nType=none\nOptions=bind") {
		t.Fatalf("mount unit: %v %q", ok, unit)
	}
	if st.JobID == 0 {
		t.Fatal("no_php must re-apply the site")
	}
	if job := f.waitJob(st.JobID); job.Status != store.JobDone {
		t.Fatalf("site apply: %s %s", job.Status, job.Error)
	}
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/shop.example.com.conf")
	if !strings.Contains(conf, `location ~ ^/upload/kaspy/.*\.(?:php|phtml|phar)$ { return 403; }`) {
		t.Fatalf("nginx config lacks the PHP denial:\n%s", conf)
	}

	// A second folder of the same name, a folder of the guest's own site and a
	// path that leaves the docroot are refused.
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "other/kaspy"}, http.StatusConflict, nil)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "../secrets", "name": "x"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/users/alex/shares", map[string]any{"site": "shop.example.com", "path": "mine"}, http.StatusUnprocessableEntity, nil)

	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Share.OwnerLogin != "alex" || list[0].Share.Domain != "shop.example.com" {
		t.Fatalf("list: %+v", list)
	}

	// The guest sees its own folders, not the administrator's endpoint.
	f.loginAs("guest", "guest-password-1")
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusForbidden, nil)
	f.login()

	var gone apitypes.ShareStatus
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusOK, &gone)
	if _, ok := f.agent.File("/etc/systemd/system/var-www-guest-kaspy.mount"); ok {
		t.Fatal("mount unit still there after removal")
	}
	if job := f.waitJob(gone.JobID); job.Status != store.JobDone {
		t.Fatalf("site apply after removal: %s %s", job.Status, job.Error)
	}
	if conf, _ := f.agent.File("/etc/nginx/monopanel/sites/shop.example.com.conf"); strings.Contains(conf, "upload/kaspy") {
		t.Fatal("nginx config still denies PHP in a folder that is no longer shared")
	}
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("share not forgotten: %+v", list)
	}
}

func TestMountUnitName(t *testing.T) {
	for where, want := range map[string]string{"/var/www/guest/kaspy": "var-www-guest-kaspy.mount", "/var/www/my-shop/x_1.2": `var-www-my\x2dshop-x_1.2.mount`} {
		if got := store.MountUnit(where); got != want {
			t.Errorf("MountUnit(%q) = %q, want %q", where, got, want)
		}
	}
}
