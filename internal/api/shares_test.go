package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A shared folder: the guest's home stays its own, the agent binds the
// owner's folder into it — no mount unit, which systemd would mount by path —
// the guest gets ACLs, PHP in the folder is denied on the site when asked,
// and everything is undone on removal.
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
	if st.Share.Name != "kaspy" || st.Where != "/var/www/guest/kaspy" || st.MountPoint != st.Where || st.Target != "/var/www/alex/data/www/shop.example.com/upload/kaspy" || !st.Mounted {
		t.Fatalf("share: %+v where=%s target=%s", st.Share, st.Where, st.Target)
	}
	if what, ok := f.agent.Share("/var/www/guest/kaspy"); !ok || what != "/var/www/alex/data/www/shop.example.com/upload/kaspy" || !f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatalf("the agent's bind: %q %v", what, ok)
	}
	noMountUnits(t, f)
	if st.JobID == 0 {
		t.Fatal("no_php must re-apply the site")
	}
	if job := f.waitJob(st.JobID); job.Status != store.JobDone {
		t.Fatalf("site apply: %s %s", job.Status, job.Error)
	}
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/shop.example.com.conf")
	if !strings.Contains(conf, `location ~ ^/upload/kaspy/.*\.(?:php|phtml|phar)(?:/|$) { return 403; }`) {
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
	if _, ok := f.agent.Share("/var/www/guest/kaspy"); ok || f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatal("still bound after removal")
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

// The agent's refusals of a change of a shared folder reach the client
// with their own status: a folder in use stays 409 and the guard an
// in-place restore holds stays 503 (try again later), where every agent
// error used to be a bad gateway; the agent down is still 502.
func TestShareChangesKeepTheAgentsStatus(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "upload/kaspy"}, http.StatusCreated, nil)

	f.agent.SetBusy("var-www-guest-kaspy.mount", true)
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusConflict, nil)
	f.agent.SetBusy("var-www-guest-kaspy.mount", false)

	f.agent.FailStatus = map[string]int{}
	f.agent.Fail["/v1/shares/unbind"] = "a shared folder cannot be bound or unbound now: a restore below the web root is under way; try again when it is done"
	f.agent.FailStatus["/v1/shares/unbind"] = http.StatusServiceUnavailable
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusServiceUnavailable, nil)
	f.agent.FailStatus["/v1/shares/unbind"] = http.StatusBadGateway
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusBadGateway, nil)
	delete(f.agent.Fail, "/v1/shares/unbind")
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusOK, nil)

	f.agent.Fail["/v1/shares/bind"] = "a shared folder cannot be bound or unbound now: a restore below the web root is under way; try again when it is done"
	f.agent.FailStatus["/v1/shares/bind"] = http.StatusServiceUnavailable
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "upload/kaspy"}, http.StatusServiceUnavailable, nil)
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("a share the agent did not bind is kept: %+v", list)
	}
}

// noMountUnits fails the test when the panel asked for a mount unit.
func noMountUnits(t *testing.T, f *siteFixture) {
	t.Helper()
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/units/apply" && strings.Contains(string(c.Body), `.mount"`) {
			t.Fatalf("a mount unit was written: %s", c.Body)
		}
		if c.Path == "/v1/service" && strings.Contains(string(c.Body), `.mount"`) && !strings.Contains(string(c.Body), `"action":"status"`) {
			t.Fatalf("systemd was asked to act on a mount: %s", c.Body)
		}
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

// A guest with a shell owns its home, no place for a mount point: its folder
// is mounted in .shares, root's, and a symlink in the home leads there. The
// guest's directory in .shares goes with its last folder.
func TestSharedFolderOfAShellGuest(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "dev", "password": "dev-password-1", "role": "user", "shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)

	const mount, link = "/var/www/.shares/dev/kaspy", "/var/www/dev/kaspy"
	var st apitypes.ShareStatus
	f.call(http.MethodPost, "/users/dev/shares", map[string]any{"site": "shop.example.com", "path": "upload/kaspy", "no_php": true}, http.StatusCreated, &st)
	if st.Where != link || st.MountPoint != mount || !st.Mounted {
		t.Fatalf("share: where=%s mount_point=%s mounted=%v", st.Where, st.MountPoint, st.Mounted)
	}
	if what, ok := f.agent.Share(mount); !ok || what != "/var/www/alex/data/www/shop.example.com/upload/kaspy" {
		t.Fatalf("the agent's bind: %q %v", what, ok)
	}
	if _, ok := f.agent.Share("/var/www/dev/kaspy"); ok || f.agent.Mounted("var-www-dev-kaspy.mount") {
		t.Fatal("a mount point in the home of a guest with a shell")
	}
	noMountUnits(t, f)
	for _, want := range []agent.DirSpec{
		{Path: "/var/www/.shares", Mode: 0o711, Owner: "root", Group: "root"},
		{Path: "/var/www/.shares/dev", Mode: 0o750, Owner: "root", Group: "dev"},
		{Path: mount, Mode: 0o755, Owner: "root", Group: "root"},
	} {
		if got, ok := f.agent.DirSpec(want.Path); !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", want.Path, got, ok, want)
		}
	}
	if target, ok := f.agent.Symlink(link); !ok || target != mount {
		t.Fatalf("the guest's symlink: %q %v", target, ok)
	}
	if home, _ := f.agent.DirSpec("/var/www/dev"); home.Owner != "dev" {
		t.Fatalf("the home of a guest with a shell is its own: %+v", home)
	}
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/dev/shares", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Where != link || list[0].MountPoint != mount || !list[0].Mounted {
		t.Fatalf("list: %+v", list)
	}
	// An entry folder is for SFTP-only accounts still.
	f.call(http.MethodPatch, "/users/dev/shares/kaspy", map[string]any{"entry": true}, http.StatusUnprocessableEntity, nil)

	f.call(http.MethodDelete, "/users/dev/shares/kaspy", nil, http.StatusOK, &st)
	if _, ok := f.agent.Share(mount); ok || f.agent.Mounted("var-www-.shares-dev-kaspy.mount") {
		t.Fatal("still bound after removal")
	}
	if _, ok := f.agent.Symlink(link); ok {
		t.Fatal("the guest's symlink stayed")
	}
	removed := strings.Join(f.agent.Removed(), " ")
	for _, p := range []string{mount, link, "/var/www/.shares/dev"} {
		if !strings.Contains(removed+" ", p+" ") {
			t.Errorf("%s not removed: %s", p, removed)
		}
	}
	f.waitJob(st.JobID)
}

// dirsCall tells whether a dirs/ensure call gave path to owner.
func dirsCall(c agenttest.Call, path, owner string) bool {
	if c.Path != "/v1/dirs/ensure" {
		return false
	}
	var req agent.EnsureDirsRequest
	if json.Unmarshal(c.Body, &req) != nil {
		return false
	}
	for _, d := range req.Dirs {
		if d.Path == path && d.Owner == owner {
			return true
		}
	}
	return false
}

// shareCall tells whether a shares/bind or shares/unbind call is for the
// mount point where.
func shareCall(c agenttest.Call, endpoint, where string) bool {
	return c.Path == endpoint && strings.Contains(string(c.Body), `"where":"`+where+`"`)
}

// firstCall is the index of the first call that matches, -1 for none.
func firstCall(calls []agenttest.Call, match func(agenttest.Call) bool) int {
	for i, c := range calls {
		if match(c) {
			return i
		}
	}
	return -1
}

// Switching a guest between SFTP-only and a shell moves its folders: out of
// the home before the home becomes the account's, back in once it is root's
// again. The fake refuses, as the agent does, a home holding a mount point to
// its account and a mount point below a home the account owns: the wrong
// order fails the switch.
func TestSwitchingShellMovesSharedFolders(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	var st apitypes.ShareStatus
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, &st)
	const home, inHome, inShares = "/var/www/guest", "var-www-guest-kaspy.mount", `var-www-.shares-guest-kaspy.mount`
	if st.MountPoint != home+"/kaspy" || !f.agent.Mounted(inHome) {
		t.Fatalf("an SFTP-only guest has the folder in its home: %+v", st)
	}

	n := len(f.agent.Calls())
	f.call(http.MethodPatch, "/users/guest", map[string]any{"shell": true}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("switch to a shell: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.Share(home + "/kaspy"); ok || f.agent.Mounted(inHome) {
		t.Fatal("the folder is still mounted in the home")
	}
	if !f.agent.Mounted(inShares) {
		t.Fatal("the folder is not mounted in .shares")
	}
	if target, ok := f.agent.Symlink(home + "/kaspy"); !ok || target != "/var/www/.shares/guest/kaspy" {
		t.Fatalf("the guest's symlink: %q %v", target, ok)
	}
	calls := f.agent.Calls()[n:]
	gone := firstCall(calls, func(c agenttest.Call) bool { return shareCall(c, "/v1/shares/unbind", home+"/kaspy") })
	moved := firstCall(calls, func(c agenttest.Call) bool { return shareCall(c, "/v1/shares/bind", "/var/www/.shares/guest/kaspy") })
	handed := firstCall(calls, func(c agenttest.Call) bool { return dirsCall(c, home, "guest") })
	if gone < 0 || moved < gone || handed < moved {
		t.Fatalf("order: unbound from the home at %d, bound in .shares at %d, home handed over at %d", gone, moved, handed)
	}
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Where != home+"/kaspy" || list[0].MountPoint != "/var/www/.shares/guest/kaspy" || !list[0].Mounted {
		t.Fatalf("list after the switch: %+v", list)
	}

	n = len(f.agent.Calls())
	f.call(http.MethodPatch, "/users/guest", map[string]any{"shell": false}, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone {
		t.Fatalf("switch to SFTP-only: %s %s", job.Status, job.Error)
	}
	if _, ok := f.agent.Share("/var/www/.shares/guest/kaspy"); ok || f.agent.Mounted(inShares) {
		t.Fatal("the folder is still mounted in .shares")
	}
	if !f.agent.Mounted(inHome) {
		t.Fatal("the folder is not mounted in the home")
	}
	if _, ok := f.agent.Symlink(home + "/kaspy"); ok {
		t.Fatal("the symlink stayed where the mount point belongs")
	}
	calls = f.agent.Calls()[n:]
	rooted := firstCall(calls, func(c agenttest.Call) bool { return dirsCall(c, home, "root") })
	gone = firstCall(calls, func(c agenttest.Call) bool { return shareCall(c, "/v1/shares/unbind", "/var/www/.shares/guest/kaspy") })
	moved = firstCall(calls, func(c agenttest.Call) bool { return shareCall(c, "/v1/shares/bind", home+"/kaspy") })
	if rooted < 0 || gone < rooted || moved < gone {
		t.Fatalf("order: home root's at %d, unbound from .shares at %d, bound in the home at %d", rooted, gone, moved)
	}
	noMountUnits(t, f)
}

// Up to 0.8.19 every folder was mounted in the guest's home, also in the home
// a guest with a shell owns: the agent took the unit over as it started
// (recorded, the unit gone, the folder still mounted), and at its start the
// panel moves such a folder to .shares, once, and leaves an SFTP-only
// guest's alone.
func TestRefreshShareMountsMovesFoldersOutOfShellHomes(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	f.call(http.MethodPost, "/users", map[string]any{"login": "dev", "password": "dev-password-1", "role": "user", "shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)

	// What an older panel left: the share, its unit in dev's own home,
	// mounted, and the mount point there.
	dev, err := f.db.GetUserByLogin(f.ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateShare(f.ctx, &store.Share{UserID: dev.ID, SiteID: site.ID, Path: "upload", Name: "upload"}); err != nil {
		t.Fatal(err)
	}
	const legacy, moved = "var-www-dev-upload.mount", "var-www-.shares-dev-upload.mount"
	f.agent.SetShare("/var/www/dev/upload", "/var/www/alex/data/www/shop.example.com/upload", true)
	f.agent.SetDir(agent.DirSpec{Path: "/var/www/dev/upload", Mode: 0o755, Owner: "root", Group: "root"})

	f.s.refreshShareMounts(f.ctx)
	if _, ok := f.agent.Share("/var/www/dev/upload"); ok || f.agent.Mounted(legacy) {
		t.Fatal("the folder is still mounted in the home of a guest with a shell")
	}
	if !f.agent.Mounted(moved) {
		t.Fatal("the folder is not mounted in .shares")
	}
	if target, ok := f.agent.Symlink("/var/www/dev/upload"); !ok || target != "/var/www/.shares/dev/upload" {
		t.Fatalf("the guest's symlink: %q %v", target, ok)
	}
	if !f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatal("the SFTP-only guest's folder went")
	}
	sh, err := f.db.GetShare(f.ctx, dev.ID, "upload")
	if err != nil || sh.Status != "active" {
		t.Fatalf("share status: %+v %v", sh, err)
	}

	// Once: nothing is at the other place any more.
	n := len(f.agent.Calls())
	f.s.refreshShareMounts(f.ctx)
	for _, c := range f.agent.Calls()[n:] {
		if c.Path != "/v1/shares/status" && c.Path != "/v1/shares/records" {
			t.Fatalf("the second start changed something: %s %s", c.Path, c.Body)
		}
	}
}

// A shared folder the agent keeps that no share names any more — a restore
// of the panel's database lost its row, say — would keep the way to its
// mount point root's for good, and nothing else in the panel removes it: the
// panel's start unbinds it, its empty mount point goes, and the log says so.
// One in use stays bound and recorded: the doctor names it, as in use, and
// the next start unbinds it once it is free — the guest's directory in
// .shares going with its last folder. The folders the shares name stay, and
// before the start's sweep the doctor names the strays as not unbound yet.
func TestStraySharesAreUnboundAtStart(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	shares := func() apitypes.Check {
		t.Helper()
		for _, c := range f.s.doctor(f.ctx).Checks {
			if c.Name == "shared folders" {
				return c
			}
		}
		t.Fatal("the doctor has no line on the shared folders")
		return apitypes.Check{}
	}
	const lost, busy = "/var/www/guest/old", "/var/www/.shares/dev/gone"
	f.agent.SetShare(lost, "/var/www/alex/data/www/shop.example.com/old", true)
	f.agent.SetShare(busy, "/var/www/alex/data/www/shop.example.com/gone", true)
	f.agent.SetBusy(store.MountUnit(busy), true)
	if c := shares(); c.Status != "warn" || !strings.HasPrefix(c.Detail, "2 kept by the agent that no shared folder of the panel names") || !strings.Contains(c.Detail, lost+": not unbound yet") || !strings.Contains(c.Detail, busy+": not unbound yet") {
		t.Fatalf("doctor before the start: %+v", c)
	}
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	var logs bytes.Buffer
	f.s.log = slog.New(slog.NewTextHandler(&logs, nil))

	f.s.refreshShareMounts(f.ctx)
	if _, ok := f.agent.Share(lost); ok || f.agent.Mounted(store.MountUnit(lost)) {
		t.Fatal("a folder no share names stayed bound")
	}
	if removed := strings.Join(f.agent.Removed(), " ") + " "; !strings.Contains(removed, lost+" ") {
		t.Errorf("its mount point stayed: %s", removed)
	}
	if out := logs.String(); !strings.Contains(out, `msg="a shared folder the panel no longer knows was unbound" mount_point=`+lost) || !strings.Contains(out, `msg="a shared folder the panel no longer knows was not unbound: the panel tries again at its next start" mount_point=`+busy) {
		t.Errorf("log:\n%s", out)
	}
	if _, ok := f.agent.Share(busy); !ok || !f.agent.Mounted(store.MountUnit(busy)) {
		t.Fatal("a folder in use was unbound or forgotten")
	}
	if what, ok := f.agent.Share("/var/www/guest/kaspy"); !ok || what != "/var/www/alex/data/www/shop.example.com/kaspy" || !f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatal("the folder of a share went")
	}
	if c := shares(); c.Status != "warn" || !strings.HasPrefix(c.Detail, "1 mounted; 1 kept by the agent") || !strings.Contains(c.Detail, busy+": in use") || strings.Contains(c.Detail, lost) {
		t.Fatalf("doctor after the start: %+v", c)
	}

	f.agent.SetBusy(store.MountUnit(busy), false)
	f.s.refreshShareMounts(f.ctx)
	if _, ok := f.agent.Share(busy); ok || f.agent.Mounted(store.MountUnit(busy)) {
		t.Fatal("a folder no share names stayed bound once free")
	}
	if removed := strings.Join(f.agent.Removed(), " ") + " "; !strings.Contains(removed, busy+" ") || !strings.Contains(removed, "/var/www/.shares/dev ") {
		t.Errorf("its mount point or the guest's directory in .shares stayed: %s", removed)
	}
	if c := shares(); c.Status != "ok" || c.Detail != "1 mounted" {
		t.Fatalf("doctor once the strays are gone: %+v", c)
	}
}

// A guest the agent no longer counts as a panel account — one added to a
// group of the system's since its folders were given — can still lose every
// one of them, and go itself: a folder the agent keeps a record of is
// unbound by it, and a share the agent keeps nothing of — a restore of the
// panel's database brought back one deleted since, whose bind the agent now
// refuses; a removal that stopped half way unbound it already — is not
// unbound at all: the agent, which unbinds for such a guest only where it
// knows a folder, is not asked where it has nothing. A share given to such a
// guest fails, and leaves no mount point behind.
func TestRefusedGuestLosesItsSharedFolders(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	for _, p := range []string{"kaspy", "gone"} {
		f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": p}, http.StatusCreated, nil)
	}
	if _, err := f.s.agent.ShareUnbind(f.ctx, &agent.ShareUnbindRequest{Where: "/var/www/guest/gone"}); err != nil {
		t.Fatal(err)
	}
	f.agent.SetAccount(func(login string) (*agent.UnitAccount, error) {
		if login == "guest" {
			return nil, errors.New("a member of the group users (gid 100): the panel runs services only for its own accounts")
		}
		return &agent.UnitAccount{Login: login, Home: "/var/www/" + login, Group: login}, nil
	})

	// the agent's refusal keeps its status: not allowed, not a bad gateway
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "new"}, http.StatusForbidden, nil)
	if removed := strings.Join(f.agent.Removed(), " ") + " "; !strings.Contains(removed, "/var/www/guest/new ") {
		t.Errorf("the mount point of a share the agent refused stayed: %s", removed)
	}
	f.call(http.MethodDelete, "/users/guest/shares/gone", nil, http.StatusOK, nil)
	f.call(http.MethodDelete, "/users/guest", nil, http.StatusAccepted, &created)
	if job := f.waitJob(created.JobID); job.Status != store.JobDone || !strings.Contains(job.Log, "shared folder kaspy (shop.example.com/kaspy) removed") {
		t.Fatalf("removing the guest: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if _, ok := f.agent.Share("/var/www/guest/kaspy"); ok || f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatal("the guest's folder stayed bound")
	}
}

// A restore of the panel's database may bring back a share whose name its
// guest has since been given again, for another folder: the agent keeps that
// other folder at the mount point the share names, and the guest goes on
// seeing it. The doctor says so, and the panel's start binds the share's own
// folder there.
func TestShareShowingAnotherFolderIsBoundAgainAtStart(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	const mount, own, other = "/var/www/guest/kaspy", "/var/www/alex/data/www/shop.example.com/kaspy", "/var/www/alex/data/www/shop.example.com/private"
	f.agent.SetShare(mount, other, true)
	shares := func() apitypes.Check {
		t.Helper()
		for _, c := range f.s.doctor(f.ctx).Checks {
			if c.Name == "shared folders" {
				return c
			}
		}
		t.Fatal("the doctor has no line on the shared folders")
		return apitypes.Check{}
	}
	if c := shares(); c.Status != "warn" || !strings.Contains(c.Detail, "kaspy of guest (shop.example.com/kaspy): another folder is mounted there, "+other) {
		t.Fatalf("doctor before the start: %+v", c)
	}

	f.s.refreshShareMounts(f.ctx)
	if what, ok := f.agent.Share(mount); !ok || what != own || !f.agent.Mounted(store.MountUnit(mount)) {
		t.Fatalf("the agent keeps %q at %s, the share's folder is %s", what, mount, own)
	}
	if c := shares(); c.Status != "ok" || c.Detail != "1 mounted" {
		t.Fatalf("doctor after the start: %+v", c)
	}
}

// A mount point with its folder mounted opens as the folder: applying a
// share again — mp site fix does, for every share of the site — must not ask
// for the mount point as a directory of root's 0755, which would land on the
// site's folder. The directories of .shares on the way are made as before.
func TestReapplyingASharedFolderLeavesTheMountedFolderAlone(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users", map[string]any{"login": "dev", "password": "dev-password-1", "role": "user", "shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	for login, mount := range map[string]string{"guest": "/var/www/guest/kaspy", "dev": "/var/www/.shares/dev/kaspy"} {
		var st apitypes.ShareStatus
		f.call(http.MethodPost, "/users/"+login+"/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, &st)
		if st.MountPoint != mount || !st.Mounted {
			t.Fatalf("%s: %+v", login, st)
		}
		u, err := f.db.GetUserByLogin(f.ctx, login)
		if err != nil {
			t.Fatal(err)
		}
		sh, err := f.db.GetShare(f.ctx, u.ID, "kaspy")
		if err != nil {
			t.Fatal(err)
		}
		n := len(f.agent.Calls())
		if _, err := f.s.applyShare(f.ctx, sh); err != nil {
			t.Fatalf("%s: apply again: %v", login, err)
		}
		calls := f.agent.Calls()[n:]
		if i := firstCall(calls, func(c agenttest.Call) bool { return dirsCall(c, mount, "root") }); i >= 0 {
			t.Fatalf("%s: the mounted folder was asked for as the mount point: %s", login, calls[i].Body)
		}
		if login == "dev" && firstCall(calls, func(c agenttest.Call) bool { return dirsCall(c, "/var/www/.shares/dev", "root") }) < 0 {
			t.Fatalf("dev: the guest's directory in .shares is not ensured any more")
		}
		if !f.agent.Mounted(store.MountUnit(mount)) {
			t.Fatalf("%s: not mounted after the apply", login)
		}
	}
}

// legacyShare is what a panel up to 0.8.19 left for the guest dev, who has a
// shell: a share whose unit mounts the folder in dev's own home — as the
// agent leaves it when it could not take the unit over (systemd did not
// reload, say).
func legacyShare(t *testing.T, f *siteFixture) (dev *store.User, unit string) {
	t.Helper()
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "dev", "password": "dev-password-1", "role": "user", "shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	dev, err := f.db.GetUserByLogin(f.ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateShare(f.ctx, &store.Share{UserID: dev.ID, SiteID: site.ID, Path: "upload", Name: "upload"}); err != nil {
		t.Fatal(err)
	}
	unit = "var-www-dev-upload.mount"
	f.agent.SetUnit(unit, "[Unit]\nDescription=MonoPanel shared folder shop.example.com/upload for dev\nRequiresMountsFor=/var/www/alex/data/www/shop.example.com\n\n[Mount]\nWhat=/var/www/alex/data/www/shop.example.com/upload\nWhere=/var/www/dev/upload\nType=none\nOptions=bind\n\n[Install]\nWantedBy=multi-user.target\n", true)
	f.agent.SetDir(agent.DirSpec{Path: "/var/www/dev/upload", Mode: 0o755, Owner: "root", Group: "root"})
	return dev, unit
}

// The guest decides how long its folder stays in use. A unit in the home of
// a guest with a shell must not stay because the folder could not be
// unmounted: systemd would mount it there again at boot, by path. So the
// agent's unbind removes it first and the share is marked failed; the
// folder stays mounted where it was until it is free, and the next start
// finishes the move — after the session ended, or after a reboot that took
// the old mount away and left its empty mount point in the home.
func TestMovingAFolderInUseOutOfAShellHome(t *testing.T) {
	for _, then := range []string{"session ended", "reboot"} {
		t.Run(then, func(t *testing.T) {
			f := newSiteFixture(t)
			dev, legacy := legacyShare(t, f)
			const moved = "var-www-.shares-dev-upload.mount"
			f.agent.SetBusy(legacy, true)

			f.s.refreshShareMounts(f.ctx)
			if _, ok := f.agent.File("/etc/systemd/system/" + legacy); ok {
				t.Fatal("the unit in the guest's own home stayed because the folder was in use")
			}
			if !f.agent.Mounted(legacy) {
				t.Fatal("a folder in use was unmounted")
			}
			sh, err := f.db.GetShare(f.ctx, dev.ID, "upload")
			if err != nil || sh.Status != "failed" || !strings.Contains(sh.LastError, "no longer in use") {
				t.Fatalf("share after the first try: %+v %v", sh, err)
			}

			if then == "session ended" {
				f.agent.SetBusy(legacy, false)
			} else {
				f.agent.Unmount(legacy)
			}
			f.s.refreshShareMounts(f.ctx)
			if f.agent.Mounted(legacy) {
				t.Fatal("the folder is still mounted in the home")
			}
			if !f.agent.Mounted(moved) {
				t.Fatal("the folder is not mounted in .shares")
			}
			if target, ok := f.agent.Symlink("/var/www/dev/upload"); !ok || target != "/var/www/.shares/dev/upload" {
				t.Fatalf("the guest's symlink: %q %v", target, ok)
			}
			if sh, err := f.db.GetShare(f.ctx, dev.ID, "upload"); err != nil || sh.Status != "active" {
				t.Fatalf("share after the move: %+v %v", sh, err)
			}
		})
	}
}

// Removing a folder in use fails, and the admin tries again once it is
// free: an SFTP-only guest's folder stays bound and recorded meanwhile (its
// home is root's), the unit an older panel left in the home of a guest with
// a shell does not stay.
func TestRemovingASharedFolderInUse(t *testing.T) {
	f := newSiteFixture(t)
	_, legacy := legacyShare(t, f)
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	const sftp = "var-www-guest-kaspy.mount"
	f.agent.SetBusy(sftp, true)
	f.agent.SetBusy(legacy, true)

	// in use: 409, as the agent says, so a client can tell it from the agent down
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusConflict, nil)
	if _, ok := f.agent.Share("/var/www/guest/kaspy"); !ok || !f.agent.Mounted(sftp) {
		t.Fatal("a folder in use in an SFTP-only home was forgotten")
	}
	f.call(http.MethodDelete, "/users/dev/shares/upload", nil, http.StatusConflict, nil)
	if _, ok := f.agent.File("/etc/systemd/system/" + legacy); ok {
		t.Fatal("the unit in the home of a guest with a shell stayed")
	}
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/dev/shares", nil, http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("the share went though its folder is still mounted: %+v", list)
	}

	f.agent.SetBusy(sftp, false)
	f.agent.SetBusy(legacy, false)
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusOK, nil)
	f.call(http.MethodDelete, "/users/dev/shares/upload", nil, http.StatusOK, nil)
	if f.agent.Mounted(sftp) || f.agent.Mounted(legacy) {
		t.Fatal("still mounted after the removal")
	}
}

// The unit in the home of a guest with a shell goes before anything else is
// tried: a site folder whose ACLs cannot be set (its owner may well arrange
// that) must not keep it there for systemd to mount at the next boot.
func TestUnitInAShellHomeGoesWhateverFailsAfter(t *testing.T) {
	f := newSiteFixture(t)
	dev, legacy := legacyShare(t, f)
	f.agent.Fail["/v1/acl/set"] = "setfacl: Operation not permitted"
	f.s.refreshShareMounts(f.ctx)
	if _, ok := f.agent.File("/etc/systemd/system/" + legacy); ok || f.agent.Mounted(legacy) {
		t.Fatal("the unit in the guest's own home stayed because the folder's ACLs failed")
	}
	sh, err := f.db.GetShare(f.ctx, dev.ID, "upload")
	if err != nil || sh.Status != "failed" || !strings.Contains(sh.LastError, "acl:") {
		t.Fatalf("share: %+v %v", sh, err)
	}
	delete(f.agent.Fail, "/v1/acl/set")
	f.s.refreshShareMounts(f.ctx)
	if !f.agent.Mounted("var-www-.shares-dev-upload.mount") {
		t.Fatal("the folder is not mounted in .shares at the next start")
	}
	if sh, err := f.db.GetShare(f.ctx, dev.ID, "upload"); err != nil || sh.Status != "active" {
		t.Fatalf("share after the next start: %+v %v", sh, err)
	}
}

// The agent took over a unit an older panel left in the home of a guest with
// a shell as it started (retireUnsafeShareUnits), and the folder is not
// mounted (a reboot came since). At its own start the panel mounts such a
// folder where it belongs, and the empty mount point left in the home gives
// way to the guest's symlink.
func TestRefreshShareMountsAppliesAFolderWithoutAUnit(t *testing.T) {
	f := newSiteFixture(t)
	dev, legacy := legacyShare(t, f)
	f.agent.Unmount(legacy)
	if _, err := f.s.agent.RemoveUnit(f.ctx, &agent.RemoveUnitRequest{Name: legacy}); err != nil {
		t.Fatal(err)
	}
	f.s.refreshShareMounts(f.ctx)
	if !f.agent.Mounted("var-www-.shares-dev-upload.mount") {
		t.Fatal("the folder is not mounted in .shares")
	}
	if target, ok := f.agent.Symlink("/var/www/dev/upload"); !ok || target != "/var/www/.shares/dev/upload" {
		t.Fatalf("the guest's symlink: %q %v", target, ok)
	}
	if sh, err := f.db.GetShare(f.ctx, dev.ID, "upload"); err != nil || sh.Status != "active" {
		t.Fatalf("share: %+v %v", sh, err)
	}
}

// Nothing mounts a shared folder at boot: after a reboot the panel binds
// every one again at its start, an SFTP-only guest's and one of a guest with
// a shell, through the agent — only the bind, the folder and its ACLs are as
// they were — and marks them active.
func TestRefreshShareMountsAfterAReboot(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users", map[string]any{"login": "dev", "password": "dev-password-1", "role": "user", "shell": true}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	units := map[string]string{"guest": "var-www-guest-kaspy.mount", "dev": "var-www-.shares-dev-kaspy.mount"}
	for login := range units {
		f.call(http.MethodPost, "/users/"+login+"/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	}
	for _, unit := range units {
		f.agent.Unmount(unit) // a reboot: the records stay, the mounts do not
	}
	var list []*apitypes.ShareStatus
	f.call(http.MethodGet, "/users/guest/shares", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Mounted {
		t.Fatalf("after the reboot: %+v", list)
	}

	n := len(f.agent.Calls())
	f.s.refreshShareMounts(f.ctx)
	for login, unit := range units {
		if !f.agent.Mounted(unit) {
			t.Errorf("%s: not mounted again", login)
		}
		u, err := f.db.GetUserByLogin(f.ctx, login)
		if err != nil {
			t.Fatal(err)
		}
		if sh, err := f.db.GetShare(f.ctx, u.ID, "kaspy"); err != nil || sh.Status != "active" {
			t.Errorf("%s: %+v %v", login, sh, err)
		}
	}
	calls := f.agent.Calls()[n:]
	if i := firstCall(calls, func(c agenttest.Call) bool { return c.Path == "/v1/acl/set" }); i >= 0 {
		t.Fatalf("the folders' ACLs were set again: %s", calls[i].Body)
	}
	if target, ok := f.agent.Symlink("/var/www/dev/kaspy"); !ok || target != "/var/www/.shares/dev/kaspy" {
		t.Fatalf("the shell guest's symlink: %q %v", target, ok)
	}
	noMountUnits(t, f)
}

// A folder the agent refuses to bind — its owner swapped it for a symlink —
// stays unmounted after a reboot: the share is marked failed with why, and
// the doctor names it; with every folder mounted the doctor says so.
func TestShareRefusedAfterARebootShowsInTheDoctor(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "kaspy"}, http.StatusCreated, nil)
	shares := func() apitypes.Check {
		t.Helper()
		for _, c := range f.s.doctor(f.ctx).Checks {
			if c.Name == "shared folders" {
				return c
			}
		}
		t.Fatal("the doctor has no line on the shared folders")
		return apitypes.Check{}
	}
	if c := shares(); c.Status != "ok" || c.Detail != "1 mounted" {
		t.Fatalf("doctor: %+v", c)
	}

	const folder = "/var/www/alex/data/www/shop.example.com/kaspy"
	f.agent.SetSymlink(folder, "/var/www/bob")
	f.agent.Unmount("var-www-guest-kaspy.mount")
	f.s.refreshShareMounts(f.ctx)
	if f.agent.Mounted("var-www-guest-kaspy.mount") {
		t.Fatal("a folder swapped for a symlink was bound")
	}
	guest, err := f.db.GetUserByLogin(f.ctx, "guest")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := f.db.GetShare(f.ctx, guest.ID, "kaspy")
	if err != nil || sh.Status != "failed" || !strings.Contains(sh.LastError, "symlink") {
		t.Fatalf("share: %+v %v", sh, err)
	}
	if c := shares(); c.Status != "warn" || !strings.Contains(c.Detail, "1 of 1 not mounted") || !strings.Contains(c.Detail, "kaspy of guest (shop.example.com/kaspy)") || !strings.Contains(c.Detail, "symlink") {
		t.Fatalf("doctor: %+v", c)
	}
}
