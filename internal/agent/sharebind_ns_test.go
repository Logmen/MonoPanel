package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

// The test binary is the host and the agent of the test below as well:
// shareNSHostEnv makes it the host, a process with a user and a mount
// namespace of its own whose test directory is a shared mount, and the host
// starts it again as the agent (shareNSAgentEnv), in a mount namespace of its
// own that is a slave of the host's — as systemd gives the agent one. The
// agent binds through the host's namespace (hostMountNS → the host's), and
// looks into it through /proc/<host>/root.
const (
	shareNSHostEnv  = "MP_TEST_SHARE_NS_HOST"
	shareNSAgentEnv = "MP_TEST_SHARE_NS_AGENT"
	shareNSDirEnv   = "MP_TEST_SHARE_NS_DIR"
)

func init() {
	switch {
	case os.Getenv(shareNSHostEnv) != "":
		os.Exit(shareNSHost())
	case os.Getenv(shareNSAgentEnv) != "":
		os.Exit(shareNSAgent())
	}
}

// shareNSHost makes the test directory a shared mount of its own and runs
// the agent in a slave copy of its namespace.
func shareNSHost() int {
	dir := os.Getenv(shareNSDirEnv)
	for _, m := range []struct {
		src, target string
		flags       uintptr
	}{
		{"none", "/", unix.MS_REC | unix.MS_PRIVATE},
		{dir, dir, unix.MS_BIND | unix.MS_REC},
		{"none", dir, unix.MS_SHARED},
	} {
		if err := unix.Mount(m.src, m.target, "", m.flags, ""); err != nil {
			fmt.Println("host: mount:", m.target, err)
			return 98
		}
	}
	cmd := exec.Command("/proc/self/exe")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, shareNSHostEnv+"=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, shareNSAgentEnv+"="+strconv.Itoa(os.Getpid()))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if err := cmd.Run(); err != nil {
		fmt.Println("host: the agent:", err)
		return 97
	}
	return 0
}

// shareNSAgent is the agent: it binds, binds again, unbinds, is raced and
// refuses, takes an older panel's unit over — and prints "ok <step>" for
// each step that went as it must, "FAIL <step>: …" for one that did not.
func shareNSAgent() int {
	failed := false
	fail := func(step, format string, args ...any) {
		failed = true
		fmt.Printf("FAIL %s: %s\n", step, fmt.Sprintf(format, args...))
	}
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		fmt.Println("FAIL setup: a slave namespace:", err)
		return 1
	}
	dir := os.Getenv(shareNSDirEnv)
	hostPID := os.Getenv(shareNSAgentEnv)
	ownNS, _ := os.Readlink("/proc/thread-self/ns/mnt")
	hostMountNS = "/proc/" + hostPID + "/ns/mnt"
	hostView := func(p string) string { return "/proc/" + hostPID + "/root" + p }
	inode := func(p string) uint64 {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			return 0
		}
		return st.Ino
	}

	cfg := config.Default()
	cfg.WWWRoot = filepath.Join(dir, "www")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ConfigDir = filepath.Join(dir, "etc")
	cfg.Agent.StateDir = filepath.Join(dir, "agent")
	rel, _ := osprofile.ParseOSRelease(strings.NewReader(debian13))
	p, err := osprofile.FromRelease(rel)
	if err != nil {
		fmt.Println("FAIL setup:", err)
		return 1
	}
	var logs bytes.Buffer
	s := NewServer(cfg, p, slog.New(slog.NewTextHandler(&logs, nil)))
	s.unitDir = filepath.Join(dir, "system")
	www := cfg.WWWRoot
	lookupUnitAccount = func(_ *Server, login string) (*UnitAccount, error) {
		switch login {
		case "alex", "bob", "guest":
			// the test's user is root here, and the folders' owner too
			return &UnitAccount{Login: login, Home: filepath.Join(www, login), Group: login, UID: 0}, nil
		}
		return nil, errors.New("not a panel account")
	}
	disableUnit = func(context.Context, string) error { return nil }
	reloadSystemd = func(context.Context) error { return nil }
	ctx := context.Background()
	site := filepath.Join(www, "alex", "data", "www", "shop.example.com", "kaspy")
	mount := filepath.Join(www, "guest", "kaspy")
	bob := filepath.Join(www, "bob", "private")
	bind := func(what, where string) (*ShareBindResponse, error) {
		return s.shareBind(ctx, &ShareBindRequest{What: what, Where: where})
	}

	// 1. the folder is bound in the host's namespace, and reaches the
	// agent's own as the host's mounts do
	folder, point := inode(site), inode(mount)
	if res, err := bind(site, mount); err != nil || !res.Changed {
		fail("bind", "%+v %v", res, err)
	} else if got := inode(hostView(mount)); got != folder {
		fail("bind", "the host sees inode %d at the mount point, the folder is %d", got, folder)
	} else if got := inode(mount); got != folder {
		fail("bind", "the agent's own namespace sees inode %d, the folder is %d", got, folder)
	} else {
		fmt.Println("ok bind")
	}
	if st, err := s.shareStatus(ctx, &ShareStatusRequest{Paths: []string{mount}}); err != nil || !st.Entries[0].Mounted || !st.Entries[0].Recorded {
		fail("status", "%+v %v", st, err)
	} else {
		fmt.Println("ok status")
	}
	// 2. applied again: the folder stays, nothing is stacked on it
	if res, err := bind(site, mount); err != nil || res.Changed {
		fail("again", "%+v %v", res, err)
	} else {
		fmt.Println("ok again")
	}
	// 2b. a mount the guest makes inside its folder — fusermount on a
	// directory of its own there — stays in the guest's view: it does not
	// show inside the owner's site, where the owner's processes and the
	// agent's walks of the site would meet it, in every namespace
	dev := func(p string) uint64 {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			return 0
		}
		return st.Dev
	}
	sub := filepath.Join(site, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		fail("guest's mount", "%v", err)
	}
	if err := inHostMountNS(func() error { return unix.Mount("tmpfs", filepath.Join(mount, "sub"), "tmpfs", 0, "size=1m") }); err != nil {
		fail("guest's mount", "mount: %v", err)
	}
	switch {
	case dev(hostView(filepath.Join(mount, "sub"))) == dev(hostView(site)):
		fail("guest's mount", "the guest's own mount is not there")
	case dev(hostView(sub)) != dev(hostView(site)):
		fail("guest's mount", "the guest's mount shows inside the owner's site in the host's namespace")
	case dev(sub) != dev(site):
		fail("guest's mount", "the guest's mount shows inside the owner's site in the agent's namespace")
	default:
		fmt.Println("ok guest's mount")
	}
	inHostMountNS(func() error { //nolint:errcheck // what the step mounted, taken off again
		unix.Unmount(filepath.Join(mount, "sub"), unix.MNT_DETACH) //nolint:errcheck // as above
		unix.Unmount(sub, unix.MNT_DETACH)                         //nolint:errcheck // there, should it have shown
		return nil
	})
	// 3. the agent unmounts it too, in the host's namespace, and the
	// unmount reaches the agent's own as well
	if res, err := s.shareUnbind(ctx, &ShareUnbindRequest{Where: mount}); err != nil || !res.Unmounted {
		fail("unbind", "%+v %v", res, err)
	} else if got := inode(hostView(mount)); got != point {
		fail("unbind", "the host sees inode %d at the mount point, its own is %d", got, point)
	} else if got := inode(mount); got != point {
		fail("unbind", "the agent's own namespace sees inode %d at the mount point, its own is %d", got, point)
	} else {
		fmt.Println("ok unbind")
	}
	// 4. the owner swaps the folder for a symlink to bob's home while the
	// agent binds: the guest sees the folder that was checked
	real := bindTree
	bindTree = func(src, dst int) error {
		if err := os.Rename(site, site+".old"); err != nil {
			return err
		}
		if err := os.Symlink(bob, site); err != nil {
			return err
		}
		return real(src, dst)
	}
	if _, err := bind(site, mount); err != nil {
		fail("race", "%v", err)
	} else if got := inode(hostView(mount)); got != folder || got == inode(bob) {
		fail("race", "the host sees inode %d, the folder checked is %d, bob's %d", got, folder, inode(bob))
	} else {
		fmt.Println("ok race")
	}
	bindTree = real
	if _, err := s.shareUnbind(ctx, &ShareUnbindRequest{Where: mount}); err != nil {
		fail("race", "unbind: %v", err)
	}
	// 5. the folder a symlink now, or a symlink above it: refused, nothing
	// bound
	if _, err := bind(site, mount); agentStatus(err) != 403 || inode(hostView(mount)) != point {
		fail("symlink", "%v, the host sees %d", err, inode(hostView(mount)))
	} else {
		fmt.Println("ok symlink")
	}
	other := filepath.Join(www, "alex", "data", "www", "other.example.com")
	if err := os.Symlink(filepath.Join(www, "bob"), other); err != nil {
		fail("symlink above", "%v", err)
	}
	if _, err := bind(filepath.Join(other, "private"), mount); agentStatus(err) != 403 || inode(hostView(mount)) != point {
		fail("symlink above", "%v, the host sees %d", err, inode(hostView(mount)))
	} else {
		fmt.Println("ok symlink above")
	}

	// 6. a panel up to 0.8.20 left units, and systemd mounted them by path
	// at boot: the one whose folder was a symlink by then shows bob's home,
	// the other its folder. The agent took them over (recorded; the unit
	// files' removal walks from /, which a user namespace shows as nobody's:
	// TestRetireUnsafeShareUnits has it) and, as it starts, takes off the
	// mount that is not the folder.
	if err := os.Remove(site); err != nil {
		fail("legacy", "%v", err)
	}
	if err := os.Mkdir(site, 0o2770); err != nil {
		fail("legacy", "%v", err)
	}
	good := filepath.Join(www, "guest", "good")
	byPath := func(what, where string) error {
		return inHostMountNS(func() error { return unix.Mount(what, where, "", unix.MS_BIND, "") })
	}
	swapped := filepath.Join(other, "private")
	for _, u := range []shareRecord{{What: swapped, Where: mount}, {What: site, Where: good}} {
		if err := s.writeShareRecord(u); err != nil {
			fail("legacy", "%v", err)
		}
		if err := byPath(u.What, u.Where); err != nil {
			fail("legacy", "mount by path: %v", err)
		}
	}
	if inode(hostView(mount)) != inode(bob) || inode(hostView(good)) != inode(site) {
		fail("legacy", "the mounts by path: %d %d", inode(hostView(mount)), inode(hostView(good)))
	}
	s.verifyShareMounts(ctx)
	switch {
	case inode(hostView(mount)) != point:
		fail("legacy", "the host still sees inode %d at %s, bob's is %d\n%s", inode(hostView(mount)), mount, inode(bob), logs.String())
	case inode(hostView(good)) != inode(site):
		fail("legacy", "the folder mounted by path went: %d\n%s", inode(hostView(good)), logs.String())
	default:
		fmt.Println("ok legacy")
	}
	// 7. a folder the agent bound stays bound as the agent starts, though
	// its owner swapped the path for a symlink since: the agent checks its
	// own mounts by the directory it bound, not by the path
	kept := filepath.Join(www, "guest", "kept")
	folder = inode(site)
	if _, err := bind(site, kept); err != nil {
		fail("kept", "%v", err)
	}
	if err := os.Rename(site, site+".kept"); err != nil {
		fail("kept", "%v", err)
	}
	if err := os.Symlink(bob, site); err != nil {
		fail("kept", "%v", err)
	}
	s.verifyShareMounts(ctx)
	if got := inode(hostView(kept)); got != folder {
		fail("kept", "the host sees inode %d at %s, the folder bound was %d\n%s", got, kept, folder, logs.String())
	} else {
		fmt.Println("ok kept")
	}
	// 8. the goroutine that would join the host's namespace runs on the
	// process's main thread: Go parks that thread for good once a goroutine
	// locked to it ends, in the state it left it — the host's namespace,
	// which /proc/<pid>/ns/mnt and nsenter -t <pid> would show for the
	// agent from then on. Another thread does the work, and the main one
	// stays in the agent's namespace.
	mainTID, workTID := 0, 0
	oldMain := onMainThread
	onMainThread = func() bool {
		if mainTID == 0 {
			mainTID = unix.Gettid()
			return true
		}
		return false
	}
	err = inHostMountNS(func() error {
		workTID = unix.Gettid()
		return nil
	})
	onMainThread = oldMain
	mainNS, _ := os.Readlink(fmt.Sprintf("/proc/self/task/%d/ns/mnt", mainTID))
	switch {
	case err != nil || mainTID == 0 || workTID == 0:
		fail("main thread", "%v: main %d, work %d", err, mainTID, workTID)
	case workTID == mainTID:
		fail("main thread", "the main thread %d joined the host's namespace itself", mainTID)
	case mainNS != ownNS:
		fail("main thread", "the main thread %d is in %s, the agent's namespace is %s", mainTID, mainNS, ownNS)
	default:
		fmt.Println("ok main thread")
	}
	if failed {
		return 1
	}
	return 0
}

// The real thing, as far as a test may: the agent binds a folder in the
// host's mount namespace — not its own, a slave copy, where a mount reaches
// nobody — from the descriptors it checked: the host sees the folder at the
// mount point and the agent's namespace receives it from there; applied
// again nothing is stacked; the agent unmounts it there too; an owner who
// swaps the folder for a symlink to another home during the bind still has
// the guest see the folder that was checked; a symlinked folder or one with
// a symlink above it is refused; and as the agent starts it takes off what
// a unit of an older panel mounted by path through a symlink, leaves the
// folder another one mounted, and leaves a bind of its own though the owner
// swapped the folder's path since.
func TestShareBindInTheHostsMountNamespace(t *testing.T) {
	attr := userNS(t)
	attr.Cloneflags |= syscall.CLONE_NEWNS
	dir := t.TempDir()
	www := filepath.Join(dir, "www")
	for _, d := range []string{
		filepath.Join(www, "alex", "data", "www", "shop.example.com", "kaspy"),
		filepath.Join(www, "guest", "kaspy"), filepath.Join(www, "guest", "good"), filepath.Join(www, "guest", "kept"),
		filepath.Join(www, "bob", "private"), filepath.Join(dir, "system"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(www, "alex", "data", "www", "shop.example.com", "kaspy"), 0o2770); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), shareNSHostEnv+"=1", shareNSDirEnv+"="+dir)
	cmd.SysProcAttr = attr
	out, err := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code == 98 {
		t.Skipf("the host cannot mount: %s", out)
	}
	if err != nil || strings.Contains(string(out), "FAIL") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, step := range []string{"bind", "status", "again", "unbind", "race", "symlink", "symlink above", "legacy", "kept", "main thread", "guest's mount"} {
		if !strings.Contains(string(out), "ok "+step+"\n") {
			t.Errorf("step %s did not run:\n%s", step, out)
		}
	}
}
