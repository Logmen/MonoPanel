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

// An entry folder puts a per-user block into the sshd drop-in: the guest's
// SFTP session starts inside the folder. The guest has one; it goes with the
// folder, and a shell account cannot have one.
func TestShareEntryPointRendersSSHD(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)

	const dropIn = "/etc/ssh/sshd_config.d/monopanel.conf"
	var st apitypes.ShareStatus
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy", "entry": true}, http.StatusCreated, &st)
	if !st.Share.Entry {
		t.Fatalf("share not an entry: %+v", st.Share)
	}
	conf, _ := f.agent.File(dropIn)
	block := "Match User guest Group monopanel-sftp\n    ForceCommand internal-sftp -u 0027 -d /kaspy\n"
	if !strings.Contains(conf, block) || strings.Index(conf, block) > strings.Index(conf, "Match Group monopanel-sftp\n") {
		t.Fatalf("sshd drop-in lacks the user block before the group block:\n%s", conf)
	}

	// A second entry folder takes over; the guest still has one.
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "upload", "entry": true}, http.StatusCreated, &st)
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	entries := 0
	for _, sh := range list {
		if sh.Share.Entry {
			entries++
			if sh.Share.Name != "upload" {
				t.Fatalf("entry stayed with %s", sh.Share.Name)
			}
		}
	}
	if entries != 1 {
		t.Fatalf("%d entry folders", entries)
	}
	conf, _ = f.agent.File(dropIn)
	if !strings.Contains(conf, "-d /upload\n") || strings.Contains(conf, "-d /kaspy\n") || strings.Count(conf, "Match User guest") != 1 {
		t.Fatalf("sshd drop-in after the switch:\n%s", conf)
	}

	// Switching it off and back on by PATCH; then the folder goes and takes the block with it.
	f.call(http.MethodPatch, "/users/guest/shares/upload", map[string]any{"entry": false}, http.StatusOK, &st)
	if conf, _ = f.agent.File(dropIn); strings.Contains(conf, "Match User") {
		t.Fatalf("sshd drop-in still has a user block:\n%s", conf)
	}
	f.call(http.MethodPatch, "/users/guest/shares/kaspy", map[string]any{"entry": true}, http.StatusOK, &st)
	if conf, _ = f.agent.File(dropIn); !strings.Contains(conf, "-d /kaspy\n") {
		t.Fatalf("sshd drop-in after PATCH:\n%s", conf)
	}
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusOK, &st)
	if conf, _ = f.agent.File(dropIn); strings.Contains(conf, "Match User") {
		t.Fatalf("sshd drop-in keeps the block of a removed folder:\n%s", conf)
	}

	// A shell account signs in wherever it likes.
	f.call(http.MethodPatch, "/users/alex/shares/nothing", map[string]any{"entry": true}, http.StatusNotFound, nil)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "x", "entry": true}, http.StatusCreated, &st)
	f.call(http.MethodPatch, "/users/guest", map[string]any{"shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPatch, "/users/guest/shares/x", map[string]any{"entry": true}, http.StatusUnprocessableEntity, nil)
}
