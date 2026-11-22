package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The folder a shared folder shows is a directory of its site owner's in the
// owner's home, reached from the web root without a symlink: a folder the
// owner swapped for a symlink — to another account's home, to the restore
// area or RestoreDir — or with a symlink above it, one in .shares or the
// restore area, someone else's, a file, a home itself, or one at or below
// another shared folder's mount point (another account's tree under the
// owner's name) is refused, 403, before anything is bound or recorded.
func TestShareSourceMustBeTheOwnersFolder(t *testing.T) {
	type setup func(t *testing.T, h *shareHost) (what string)
	victim := func(t *testing.T, h *shareHost) string {
		bob := filepath.Join(h.www, "bob", "private")
		mkdirs(t, bob)
		return bob
	}
	for _, c := range []struct {
		name  string
		setup setup
		why   string
	}{
		{"the owner's folder", func(t *testing.T, h *shareHost) string { return h.site }, ""},
		{"swapped for a symlink to another home", func(t *testing.T, h *shareHost) string {
			swap(t, h.site, victim(t, h))
			return h.site
		}, "is a symlink or no directory"},
		{"swapped for a symlink to the restore area", func(t *testing.T, h *shareHost) string {
			staging := filepath.Join(h.www, restoreAreaName, ".in-place-0123456789abcdef")
			mkdirs(t, staging)
			swap(t, h.site, staging)
			return h.site
		}, "is a symlink or no directory"},
		{"a symlink above it", func(t *testing.T, h *shareHost) string {
			elsewhere := t.TempDir()
			mkdirs(t, filepath.Join(elsewhere, "kaspy"))
			swap(t, filepath.Dir(h.site), elsewhere)
			return h.site
		}, "is a symlink or no directory"},
		{"a file", func(t *testing.T, h *shareHost) string {
			f := filepath.Join(filepath.Dir(h.site), "file")
			writeFileT(t, f, "x")
			return f
		}, "is a symlink or no directory"},
		{"missing", func(t *testing.T, h *shareHost) string { return filepath.Join(filepath.Dir(h.site), "gone") }, "does not exist"},
		{"someone else's", func(t *testing.T, h *shareHost) string {
			pretendOwners(t, map[string]uint32{h.site: 1502})
			return h.site
		}, "belongs to uid 1502"},
		{"in the restore area", func(t *testing.T, h *shareHost) string {
			p := filepath.Join(h.www, restoreAreaName, "x")
			mkdirs(t, p)
			return p
		}, "the agent's own"},
		{"in .shares", func(t *testing.T, h *shareHost) string {
			p := filepath.Join(h.shares, "bob", "x")
			mkdirs(t, p)
			return p
		}, "the agent's own"},
		{"RestoreDir", func(t *testing.T, h *shareHost) string { return filepath.Join(RestoreDir, "latest") }, "not inside an account's home"},
		{"a home itself", func(t *testing.T, h *shareHost) string { return filepath.Join(h.www, "alex") }, "not inside an account's home"},
		{"below another shared folder", func(t *testing.T, h *shareHost) string {
			// alex is an SFTP-only guest of bob's: bob's folder is mounted in
			// alex's home, and below it lies bob's tree
			mount := filepath.Join(h.www, "alex", "media")
			mkdirs(t, filepath.Join(mount, "x"))
			h.placeShare(t, mount)
			return filepath.Join(mount, "x")
		}, "the mount point of another shared folder"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newShareHost(t)
			where := filepath.Join(h.home, "kaspy")
			mkdirs(t, where)
			what := c.setup(t, h)
			res, err := h.s.shareBind(context.Background(), &ShareBindRequest{What: what, Where: where})
			_, recorded := h.s.recordedOK(t, where)
			if c.why == "" {
				if err != nil || !res.Mounted {
					t.Fatalf("bind: %+v %v", res, err)
				}
				if want := ino(t, what) + " on " + ino(t, where); len(h.binds) != 1 || h.binds[0] != want {
					t.Fatalf("binds %v, want %s", h.binds, want)
				}
				if !recorded {
					t.Fatal("not recorded")
				}
				return
			}
			if agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("bind: %v, want 403 saying %q", err, c.why)
			}
			if len(h.binds) != 0 || recorded {
				t.Fatalf("a refused folder was bound %v or recorded %v", h.binds, recorded)
			}
		})
	}
}

// swap puts a symlink to target in the place of the directory p, as its
// owner may at any time.
func swap(t *testing.T, p, target string) {
	t.Helper()
	if err := os.Rename(p, p+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// The bind takes the folder the agent opened and checked, not its name: an
// owner who swaps the folder for a symlink to bob's home while the agent is
// at it still has the guest see the folder it checked.
func TestShareBindBindsTheFolderItOpened(t *testing.T) {
	h := newShareHost(t)
	where := filepath.Join(h.home, "kaspy")
	mkdirs(t, where)
	bob := filepath.Join(h.www, "bob", "private")
	mkdirs(t, bob)
	folder := ino(t, h.site)
	stub := bindTree
	bindTree = func(src, dst int) error {
		swap(t, h.site, bob)
		return stub(src, dst)
	}
	if _, err := h.bind(where); err != nil {
		t.Fatal(err)
	}
	if want := folder + " on " + ino(t, where); len(h.binds) != 1 || h.binds[0] != want {
		t.Fatalf("binds %v, want the folder checked: %s (bob's is %s)", h.binds, want, ino(t, bob))
	}
	// the record says which directory was bound
	recs, err := h.s.shareRecords()
	if err != nil || len(recs) != 1 || strconv.FormatUint(recs[0].Ino, 10) != folder || recs[0].Dev == 0 {
		t.Fatalf("records %+v %v, want the folder's inode %s", recs, err, folder)
	}
}

// A mount point that shows another directory than the folder — what a unit
// of an older panel bound by its What= — loses that mount, in use or not,
// before the folder is bound; a failing bind leaves no record.
func TestShareBindTakesOffAnotherMount(t *testing.T) {
	h := newShareHost(t)
	where := filepath.Join(h.home, "kaspy")
	mkdirs(t, where)
	mountedIno := ino(t, where)
	old := mountedOn
	mountedOn = func(fd int) bool { return fdIno(fd) == mountedIno && len(h.unmounts) == 0 }
	t.Cleanup(func() { mountedOn = old })
	if _, err := h.bind(where); err != nil {
		t.Fatal(err)
	}
	if len(h.unmounts) != 1 || h.unmounts[0] != mountedIno+" "+strconv.Itoa(unix.MNT_DETACH) || len(h.binds) != 1 {
		t.Fatalf("unmounts %v, binds %v", h.unmounts, h.binds)
	}

	h2 := newShareHost(t)
	where = filepath.Join(h2.home, "kaspy")
	mkdirs(t, where)
	h2.bindErr = errors.New("move_mount: invalid argument")
	if _, err := h2.bind(where); err == nil || !strings.Contains(err.Error(), "invalid argument") {
		t.Fatalf("a failing bind: %v", err)
	}
	if _, recorded := h2.s.recordedOK(t, where); recorded {
		t.Fatal("a folder that was not bound is recorded")
	}
}

// Unbinding takes a unit an older panel wrote for the mount point away first
// — systemd forgets it — then the mount, then the record. A folder in use
// stays mounted and recorded (409), its unit gone all the same; the agent
// tells what lies at a mount point: mounted, recorded, a unit.
func TestShareUnbindAndStatus(t *testing.T) {
	ctx := context.Background()
	h := newShareHost(t)
	var disabled []string
	reloads := 0
	oldDisable, oldReload := disableUnit, reloadSystemd
	disableUnit = func(_ context.Context, unit string) error { disabled = append(disabled, unit); return nil }
	reloadSystemd = func(context.Context) error { reloads++; return nil }
	t.Cleanup(func() { disableUnit, reloadSystemd = oldDisable, oldReload })
	where := filepath.Join(h.home, "kaspy")
	mkdirs(t, where)
	name := h.placeUnit(t, where)
	h.placeShare(t, where)
	mounted := true
	busy := true
	oldMounted := mountedOn
	whereIno := ino(t, where)
	mountedOn = func(fd int) bool { return mounted && fdIno(fd) == whereIno }
	t.Cleanup(func() { mountedOn = oldMounted })
	unmountAt = func(dir int, name string, flags int) error {
		if busy {
			return unix.EBUSY
		}
		mounted = false
		h.unmounts = append(h.unmounts, entryIno(dir, name)+" "+strconv.Itoa(flags))
		return nil
	}

	st, err := h.s.shareStatus(ctx, &ShareStatusRequest{Paths: []string{where, filepath.Join(h.home, "other")}})
	if err != nil || len(st.Entries) != 2 || !st.Entries[0].Mounted || !st.Entries[0].Recorded || !st.Entries[0].Unit || st.Entries[1].Mounted || st.Entries[1].Recorded || st.Entries[1].Unit {
		t.Fatalf("status: %+v %v", st, err)
	}
	_, err = h.s.shareUnbind(ctx, &ShareUnbindRequest{Where: where})
	if agentStatus(err) != http.StatusConflict || !strings.Contains(err.Error(), "in use") || !strings.Contains(err.Error(), "gone all the same") {
		t.Fatalf("unbind of a folder in use: %v, want 409", err)
	}
	if _, err := os.Stat(filepath.Join(h.s.unitDir, name)); !errors.Is(err, os.ErrNotExist) || len(disabled) != 1 || reloads != 1 {
		t.Fatalf("the unit of an older panel stayed: %v, disabled %v, %d reloads", err, disabled, reloads)
	}
	if _, recorded := h.s.recordedOK(t, where); !recorded {
		t.Fatal("a folder still mounted was forgotten")
	}
	busy = false
	res, err := h.s.shareUnbind(ctx, &ShareUnbindRequest{Where: where})
	if err != nil || !res.Unmounted || res.UnitRemoved || len(h.unmounts) != 1 || h.unmounts[0] != whereIno+" 0" {
		t.Fatalf("unbind: %+v %v, unmounts %v", res, err, h.unmounts)
	}
	if _, recorded := h.s.recordedOK(t, where); recorded {
		t.Fatal("still recorded")
	}
	st, err = h.s.shareStatus(ctx, &ShareStatusRequest{Paths: []string{where}})
	if err != nil || st.Entries[0] != (ShareMountState{Where: where}) {
		t.Fatalf("status after the unbind: %+v %v", st, err)
	}
	// again: nothing there, nothing to do
	if res, err := h.s.shareUnbind(ctx, &ShareUnbindRequest{Where: where}); err != nil || res.Unmounted {
		t.Fatalf("a second unbind: %+v %v", res, err)
	}
	for _, bad := range []string{"/etc/cron.d", filepath.Join(h.www, "nobody", "x"), filepath.Join(h.shares, "guest")} {
		if _, err := h.s.shareUnbind(ctx, &ShareUnbindRequest{Where: bad}); agentStatus(err) != http.StatusForbidden {
			t.Errorf("unbind %s: %v, want 403", bad, err)
		}
	}
	if _, err := h.s.shareStatus(ctx, &ShareStatusRequest{Paths: []string{"/etc"}}); agentStatus(err) != http.StatusBadRequest {
		t.Errorf("status of /etc: %v", err)
	}
}

// As the agent starts it takes over the mount units of the shared folders
// that the panels up to 0.8.20 wrote — systemd would mount them by path at
// the next boot: each one in the panel's form is recorded, disabled and
// removed, wherever its mount point lies (a home the guest owns, an SFTP-only
// guest's, .shares); a unit not in the panel's form stays. What one of them
// mounted is checked against its folder: a folder its owner swapped for a
// symlink loses the mount, in use or not.
func TestRetireUnsafeShareUnits(t *testing.T) {
	ctx := context.Background()
	h := newShareHost(t)
	var disabled []string
	reloads := 0
	oldDisable, oldReload := disableUnit, reloadSystemd
	disableUnit = func(_ context.Context, unit string) error { disabled = append(disabled, unit); return nil }
	reloadSystemd = func(context.Context) error { reloads++; return nil }
	t.Cleanup(func() { disableUnit, reloadSystemd = oldDisable, oldReload })

	legacy := filepath.Join(h.home, "kaspy")     // the guest owns its home
	sftp := filepath.Join(h.www, "bob", "kaspy") // bob's home is root's
	shell := filepath.Join(h.shares, "carl", "kaspy")
	admin := filepath.Join(h.home, "media") // not in the panel's form
	mkdirs(t, legacy, sftp, shell, admin)
	pretendOwners(t, map[string]uint32{h.home: 1500})
	old := lookupUnitAccount
	lookupUnitAccount = func(s *Server, login string) (*UnitAccount, error) {
		if login == "carl" {
			return &UnitAccount{Login: login, Home: filepath.Join(h.www, login), Group: login, UID: 1503}, nil
		}
		return old(s, login)
	}
	for _, where := range []string{legacy, sftp, shell} {
		h.placeUnit(t, where)
	}
	name, content := h.mountUnit(admin)
	adminFile := filepath.Join(h.s.unitDir, name)
	if err := os.WriteFile(adminFile, []byte(strings.Replace(content, "Options=bind", "Options=bind,ro", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// the legacy unit bound its What= by path once its owner had swapped it
	// for a symlink: whatever is mounted there now is not the folder
	swap(t, h.site, filepath.Join(h.www, "bob"))
	legacyIno := ino(t, legacy)
	oldMounted := mountedOn
	mountedOn = func(fd int) bool { return fdIno(fd) == legacyIno && len(h.unmounts) == 0 }
	t.Cleanup(func() { mountedOn = oldMounted })

	h.s.retireUnsafeShareUnits(ctx)
	for _, where := range []string{legacy, sftp, shell} {
		if _, err := os.Stat(filepath.Join(h.s.unitDir, mountUnitName(where))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the unit of %s stayed: %v", where, err)
		}
		if what, ok := h.s.recordedOK(t, where); !ok || what != h.site {
			t.Errorf("%s is not recorded: %q %v", where, what, ok)
		}
	}
	if len(disabled) != 3 || reloads != 3 {
		t.Fatalf("disabled %v, %d reloads", disabled, reloads)
	}
	if _, err := os.Stat(adminFile); err != nil {
		t.Errorf("an administrator's unit went: %v", err)
	}
	if len(h.unmounts) != 1 || h.unmounts[0] != legacyIno+" "+strconv.Itoa(unix.MNT_DETACH) {
		t.Fatalf("unmounts %v: the mount of the swapped folder must go", h.unmounts)
	}
	// the guard keeps every way, the admin's unit's too
	g, err := h.s.loadShareGuard()
	if err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{legacy, sftp, shell, admin} {
		if !g.isMount(where) {
			t.Errorf("the guard does not know %s", where)
		}
	}

	// A reload that fails: the unit is back, recorded all the same, and the
	// agent starts.
	h2 := newShareHost(t)
	reloadSystemd = func(context.Context) error { return errors.New("systemd unreachable") }
	where := filepath.Join(h2.home, "kaspy")
	mkdirs(t, where)
	h2.placeUnit(t, where)
	h2.s.retireUnsafeShareUnits(ctx)
	if _, err := os.Stat(filepath.Join(h2.s.unitDir, mountUnitName(where))); err != nil {
		t.Fatalf("the unit is gone though systemd did not reload: %v", err)
	}
	if _, ok := h2.s.recordedOK(t, where); !ok {
		t.Fatal("not recorded")
	}
}

// A panel up to 0.8.19 wrote the units of the shared folders through
// config/apply, without a look at the accounts: one of them may have a guest
// or an owner the agent no longer counts as a panel account (a supplementary
// group of the system's, a privileged one). Such a unit is the panel's all
// the same — its generated header says so — and systemd mounts it by path at
// every boot: it is taken over like any other. What it mounted stays only
// when it shows the folder as the agent would bind it; the API's bind then
// fails, with why, where the agent refuses the account.
func TestRetireTakesOverThePanelsUnitsWhateverTheAccounts(t *testing.T) {
	ctx := context.Background()
	h := newShareHost(t)
	oldDisable, oldReload := disableUnit, reloadSystemd
	disableUnit = func(context.Context, string) error { return nil }
	reloadSystemd = func(context.Context) error { return nil }
	t.Cleanup(func() { disableUnit, reloadSystemd = oldDisable, oldReload })
	refused := map[string]bool{"carl": true}
	old := lookupUnitAccount
	lookupUnitAccount = func(s *Server, login string) (*UnitAccount, error) {
		if refused[login] {
			return nil, errors.New("a member of the group users (gid 100): the panel runs services only for its own accounts")
		}
		if login == "dave" {
			return &UnitAccount{Login: login, Home: filepath.Join(h.www, login), Group: login, UID: uint32(os.Getuid())}, nil
		}
		return old(s, login)
	}
	t.Cleanup(func() { lookupUnitAccount = old })
	// carl, the guest, is in a group of the system's
	guest := filepath.Join(h.www, "carl", "kaspy")
	// dave's folder is shared with bob; the owner is the one refused below
	owned := filepath.Join(h.www, "bob", "media")
	daveFolder := filepath.Join(h.www, "dave", "data", "www", "shop.example.com", "kaspy")
	mkdirs(t, guest, owned, daveFolder)
	h.placeUnit(t, guest)
	name, content := h.mountUnit(owned)
	content = strings.ReplaceAll(content, filepath.Join(h.www, "alex")+"/", filepath.Join(h.www, "dave")+"/")
	if err := os.WriteFile(filepath.Join(h.s.unitDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// headerless, an administrator's that names an account the agent refuses:
	// not taken for the panel's
	admin := filepath.Join(h.www, "carl", "media")
	mkdirs(t, admin)
	aname, acontent := h.mountUnit(admin)
	acontent = acontent[strings.Index(acontent, "\n")+1:]
	if err := os.WriteFile(filepath.Join(h.s.unitDir, aname), []byte(acontent), 0o644); err != nil {
		t.Fatal(err)
	}
	ownedIno := ino(t, owned)
	oldMounted := mountedOn
	mountedOn = func(fd int) bool { return fdIno(fd) == ownedIno && len(h.unmounts) == 0 }
	t.Cleanup(func() { mountedOn = oldMounted })
	refused["dave"] = true // dave joined a group of the system's since

	h.s.retireUnsafeShareUnits(ctx)
	for _, where := range []string{guest, owned} {
		if _, err := os.Stat(filepath.Join(h.s.unitDir, mountUnitName(where))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the panel's unit of %s stayed, mounting by path at boot: %v", where, err)
		}
		if _, ok := h.s.recordedOK(t, where); !ok {
			t.Errorf("%s is not recorded", where)
		}
	}
	if _, err := os.Stat(filepath.Join(h.s.unitDir, aname)); err != nil {
		t.Errorf("an administrator's unit went: %v", err)
	}
	if len(h.unmounts) != 1 || h.unmounts[0] != ownedIno+" "+strconv.Itoa(unix.MNT_DETACH) {
		t.Fatalf("unmounts %v: the folder of an owner the agent refuses is not bound as the agent binds", h.unmounts)
	}
}

// Removing an older panel's unit reloads systemd — twice, with its disable —
// and a host with many units reloads slowly: the removals may use up the
// agent's start-up budget. What the units mounted is checked before that,
// every one by its record: else a folder its owner had swapped for a
// symlink before the upgrade stayed bound to whatever the link pointed at,
// looking mounted to the panel, which binds only what is not.
func TestRetireChecksTheMountsBeforeTheSlowRemovals(t *testing.T) {
	h := newShareHost(t)
	oldDisable, oldReload := disableUnit, reloadSystemd
	// every reload takes as long as the agent's start may
	disableUnit = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
	reloadSystemd = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	t.Cleanup(func() { disableUnit, reloadSystemd = oldDisable, oldReload })
	var points []string
	for _, name := range []string{"one", "two"} {
		where := filepath.Join(h.www, "bob", name) // bob's home is root's
		mkdirs(t, where)
		h.placeUnit(t, where)
		points = append(points, ino(t, where))
	}
	// systemd mounted both by path once the owner had swapped the folder
	swap(t, h.site, filepath.Join(h.www, "bob"))
	var mu sync.Mutex
	taken := map[string]bool{}
	oldMounted := mountedOn
	mountedOn = func(fd int) bool {
		time.Sleep(20 * time.Millisecond) // a look takes a moment
		mu.Lock()
		defer mu.Unlock()
		i := fdIno(fd)
		return slices.Contains(points, i) && !taken[i]
	}
	t.Cleanup(func() { mountedOn = oldMounted })
	unmountAt = func(dir int, name string, _ int) error {
		mu.Lock()
		defer mu.Unlock()
		taken[entryIno(dir, name)] = true
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.s.retireUnsafeShareUnits(ctx)
	mu.Lock()
	defer mu.Unlock()
	for i, p := range points {
		if !taken[p] {
			t.Errorf("mount point %d: what systemd mounted by path stayed", i+1)
		}
	}
	for _, name := range []string{"one", "two"} {
		if _, ok := h.s.recordedOK(t, filepath.Join(h.www, "bob", name)); !ok {
			t.Errorf("%s: not recorded", name)
		}
	}
}

// A check that does not end — a lookup in the owner's tree that waits on an
// NFS server gone away, or on a FUSE mount of the owner's that never
// answers — keeps only its own mount as it is: the records after it are
// checked all the same. And once it ends, after its time, with the agent
// listening by then, it takes nothing off: the panel may have bound the
// folder there meanwhile.
func TestAStuckShareCheckHoldsBackNoOther(t *testing.T) {
	h := newShareHost(t)
	oldWait := shareCheckWait
	shareCheckWait = 100 * time.Millisecond
	t.Cleanup(func() { shareCheckWait = oldWait })
	stuck, next := filepath.Join(h.www, "bob", "one"), filepath.Join(h.www, "bob", "two")
	mkdirs(t, stuck, next)
	h.placeShare(t, stuck)
	h.placeShare(t, next)
	swap(t, h.site, filepath.Join(h.www, "bob")) // neither shows the folder
	stuckIno, nextIno := ino(t, stuck), ino(t, next)
	release := make(chan struct{})
	var mu sync.Mutex
	taken := map[string]bool{}
	oldMounted := mountedOn
	mountedOn = func(fd int) bool {
		i := fdIno(fd)
		if i == stuckIno {
			<-release
		}
		mu.Lock()
		defer mu.Unlock()
		return (i == stuckIno || i == nextIno) && !taken[i]
	}
	t.Cleanup(func() { mountedOn = oldMounted })
	unmountAt = func(dir int, name string, _ int) error {
		mu.Lock()
		defer mu.Unlock()
		taken[entryIno(dir, name)] = true
		return nil
	}
	var released atomic.Bool
	var once sync.Once
	ended := make(chan struct{})
	base := inHostMountNS
	inHostMountNS = func(fn func() error) error {
		err := base(fn)
		if released.Load() {
			// nothing but the stuck check runs by then
			once.Do(func() { close(ended) })
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.s.verifyShareMounts(ctx)
	mu.Lock()
	gotNext, gotStuck := taken[nextIno], taken[stuckIno]
	mu.Unlock()
	if !gotNext {
		t.Error("the record after a stuck check was not checked")
	}
	if gotStuck {
		t.Error("a stuck check took its mount off")
	}
	released.Store(true)
	close(release)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck check did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	if taken[stuckIno] {
		t.Error("a check that ended after its time took a mount off, without the lock, the agent listening")
	}
}

// The records are the agent's alone and say the mount points by their
// names: one whose content cannot be read still guards its mount point, and
// is left out of the checks at start.
func TestShareRecords(t *testing.T) {
	h := newShareHost(t)
	where := filepath.Join(h.shares, "guest", "kaspy")
	h.placeShare(t, where)
	dir := h.s.shareRecordDir()
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the records' directory: %v %v", st, err)
	}
	f := filepath.Join(dir, shareRecordName(where))
	if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the record: %v %v", st, err)
	}
	if err := os.WriteFile(f, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recs, err := h.s.shareRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("a broken record read as %+v %v", recs, err)
	}
	if g, err := h.s.loadShareGuard(); err != nil || !g.isMount(where) {
		t.Fatalf("a broken record no longer guards: %v", err)
	}
	if err := h.s.removeShareRecord(where); err != nil {
		t.Fatal(err)
	}
	if g, err := h.s.loadShareGuard(); err != nil || g.isMount(where) {
		t.Fatalf("a removed record still guards: %v", err)
	}
}
