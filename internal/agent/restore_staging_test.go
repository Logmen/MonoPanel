package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// lsNodes is restic ls --json of a snapshot holding files (path → size)
// and the directories that lead to them, depth first as restic lists them.
func lsNodes(files map[string]uint64) string {
	var b strings.Builder
	b.WriteString(`{"time":"2026-11-21T10:00:00Z","tree":"ab","paths":["/"],"hostname":"h","id":"` + snapID + `","short_id":"1a2b3c4d","struct_type":"snapshot"}` + "\n")
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	listed := map[string]bool{}
	for _, p := range paths {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for i := range parts[:len(parts)-1] {
			d := "/" + strings.Join(parts[:i+1], "/")
			if !listed[d] {
				listed[d] = true
				fmt.Fprintf(&b, `{"name":%q,"type":"dir","path":%q,"struct_type":"node"}`+"\n", parts[i], d)
			}
		}
		fmt.Fprintf(&b, `{"name":%q,"type":"file","path":%q,"size":%d,"struct_type":"node"}`+"\n", parts[len(parts)-1], p, files[p])
	}
	return b.String()
}

// stagingServer is a policy server whose RestoreDir is the test's.
func stagingServer(t *testing.T) *Server {
	t.Helper()
	s := policyServer(t, debian13)
	withServiceUserOfTheTest(t, s)
	s.restoreRoot = filepath.Join(t.TempDir(), "restore")
	return s
}

// An in-place restore stages what goes into the web root in the web root's
// restore area and the rest in RestoreDir, each in a fresh directory of
// root's alone, has restic restore each part into its own, and moves the
// result into place: a home's files are renamed on the file system they
// stay on, and nothing is left staged afterwards.
func TestInPlaceRestoreStagesOnTheTreesFileSystem(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	dump := filepath.Join(s.backupTmpDir(), "run-3", "alex_wp.sql")
	mkdirs(t, home)
	listing := lsNodes(map[string]uint64{home + "/data/www/index.php": 5, dump: 7})
	var www unix.Stat_t
	if err := unix.Stat(s.cfg.WWWRoot, &www); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	run := fakeRestic(listing, &calls, func(args []string) {
		target := args[3]
		var st unix.Stat_t
		if err := unix.Stat(target, &st); err != nil || st.Mode&0o7777 != 0o700 || int(st.Uid) != s.rootUID {
			t.Errorf("staging %s: mode %#o uid %d (%v)", target, st.Mode&0o7777, st.Uid, err)
		}
		switch filepath.Dir(target) {
		case s.restoreArea():
			if st.Dev != www.Dev {
				t.Errorf("the home is staged on another file system than the web root's")
			}
			stage(t, target, map[string]string{home + "/data/www/index.php": "<?php"})
		case s.restoreRoot:
			stage(t, target, map[string]string{dump: "-- dump"})
		default:
			t.Errorf("restored into %s", target)
		}
	})
	var out bytes.Buffer
	code, err := s.restoreInPlace(context.Background(), run, "latest", []string{home, dump}, &out)
	if err != nil || code != 0 {
		t.Fatalf("restore: %d %v\n%s", code, err, out.String())
	}
	if len(calls) != 3 || strings.Join(calls[0], " ") != "ls --json latest" {
		t.Fatalf("restic calls %q", calls)
	}
	byStage := map[string][]string{}
	for _, c := range calls[1:] {
		if c[0] != "restore" || c[1] != snapID || c[2] != "--target" || !stagingNameRe.MatchString(filepath.Base(c[3])) {
			t.Fatalf("restore argv %q", c)
		}
		byStage[filepath.Dir(c[3])] = c[4:]
	}
	if !slices.Equal(byStage[s.restoreArea()], []string{"--include", home}) || !slices.Equal(byStage[s.restoreRoot], []string{"--include", dump}) {
		t.Fatalf("the includes by stage: %q", byStage)
	}
	if readFileT(t, filepath.Join(home, "data", "www", "index.php")) != "<?php" || readFileT(t, dump) != "-- dump" {
		t.Fatal("not moved into place")
	}
	for _, d := range []string{s.restoreArea(), s.restoreRoot} {
		if names, err := os.ReadDir(d); err != nil || len(names) != 0 {
			t.Errorf("%s still holds %v (%v)", d, names, err)
		}
	}
	if st, err := os.Lstat(s.restoreArea()); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("the restore area: %v %v", st, err)
	}
	// an include in the restore area itself is refused before anything runs
	call := withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", filepath.Join(s.restoreArea(), "x")), "RESTIC_REPOSITORY=s3:https://s3.example.com/b", "RESTIC_PASSWORD=pw")
	if _, err := s.planTool(&call); err == nil || !strings.Contains(err.Error(), "stages in-place restores") {
		t.Fatalf("restoring into the restore area: %v", err)
	}
}

// The staging is checked for room before restic writes anything: a file
// system without the blocks or the inodes for what the snapshot brings —
// the listing says how much — refuses the restore with what it takes and
// what is free, and nothing is staged or moved.
func TestInPlaceRestoreChecksSpace(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	files := map[string]uint64{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("%s/data/www/f%d", home, i)] = 10 << 20
	}
	listing := lsNodes(files)
	old := stagingStatfs
	t.Cleanup(func() { stagingStatfs = old })
	for _, c := range []struct {
		name               string
		blocks, freeInodes uint64
		says               string
	}{
		{"no room", (100 << 20) / 4096, 1 << 20, "MB are free there"},
		{"no inodes", 1 << 30, 100, "free inodes"},
		{"room", 1 << 30, 1 << 20, ""},
	} {
		stagingStatfs = func(fd int, st *unix.Statfs_t) error {
			if err := unix.Fstatfs(fd, st); err != nil {
				return err
			}
			st.Bsize, st.Frsize, st.Bavail, st.Files, st.Ffree = 4096, 4096, c.blocks, 1<<20, c.freeInodes
			return nil
		}
		var calls [][]string
		run := fakeRestic(listing, &calls, func(args []string) {
			stage(t, args[3], map[string]string{home + "/data/www/f0": "x"})
		})
		var out bytes.Buffer
		code, err := s.restoreInPlace(context.Background(), run, "latest", []string{home}, &out)
		if c.says == "" {
			if err != nil || code != 0 || len(calls) != 2 {
				t.Errorf("%s: %d %v %q", c.name, code, err, calls)
			}
			continue
		}
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != http.StatusInsufficientStorage || !strings.Contains(ae.Message, c.says) || !strings.Contains(ae.Message, s.restoreArea()) {
			t.Errorf("%s: %v", c.name, err)
		}
		if len(calls) != 1 {
			t.Errorf("%s: restic restored after all: %q", c.name, calls)
		}
		if names, _ := os.ReadDir(s.restoreArea()); len(names) != 0 {
			t.Errorf("%s: the staging stayed: %v", c.name, names)
		}
	}
}

// The restore area is made root's and 0700; one somebody else may write to,
// or a symlink under its name, is refused rather than taken over — and so is
// a web root others may rearrange.
func TestRestoreAreaIsTheAgents(t *testing.T) {
	s := stagingServer(t)
	// a default ACL of the web root gives the area made in it nothing
	acl := exec.Command("setfacl", "-d", "-m", "g:"+strconv.Itoa(os.Getgid())+":rwx", s.cfg.WWWRoot)
	if out, err := acl.CombinedOutput(); err != nil {
		t.Logf("no default ACL for the web root (%v %s): the area is made without one to inherit", err, out)
	}
	fd, err := s.openRestoreArea()
	if err != nil {
		t.Fatal(err)
	}
	if hasAccessACL(fd) {
		t.Error("the area kept the entries of the web root's default ACL")
	}
	unix.Close(fd)
	if st, err := os.Lstat(s.restoreArea()); err != nil || st.Mode()&os.ModeType != os.ModeDir || st.Mode().Perm() != 0o700 {
		t.Fatalf("made %v %v", st, err)
	}
	// and once made it opens again as it is
	if fd, err = s.openRestoreArea(); err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	if err := os.Chmod(s.restoreArea(), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openRestoreArea(); err == nil || !strings.Contains(err.Error(), "not the agent's alone") {
		t.Fatalf("a restore area others may write to: %v", err)
	}
	if st, _ := os.Stat(s.restoreArea()); st.Mode().Perm() != 0o777 {
		t.Fatal("taken over")
	}
	os.Remove(s.restoreArea())
	if err := os.Symlink(t.TempDir(), s.restoreArea()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openRestoreArea(); err == nil || !errors.Is(err, errSymlinkComponent) {
		t.Fatalf("a symlink for the restore area: %v", err)
	}
	os.Remove(s.restoreArea())
	if err := os.Chmod(s.cfg.WWWRoot, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.cfg.WWWRoot, 0o755) })
	if _, err := s.openRestoreArea(); err == nil || !strings.Contains(err.Error(), "not root's alone") {
		t.Fatalf("a web root others may write to: %v", err)
	}
}

// What a restore an agent was killed in the middle of staged is found as the
// next one starts and removed — the staging directories alone: a restore
// aside and whatever else lies there stay, and so does what a restore area
// that is not the agent's alone holds.
func TestStaleStagingRemovedAtStart(t *testing.T) {
	s := stagingServer(t)
	fd, err := s.openRestoreArea()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	staleWWW := filepath.Join(s.restoreArea(), ".in-place-0123456789abcdef")
	staleRoot := filepath.Join(s.restoreRoot, ".in-place-fedcba9876543210")
	aside := filepath.Join(s.restoreRoot, "1a2b3c4d")
	other := filepath.Join(s.restoreArea(), "keep")
	for _, d := range []string{filepath.Join(staleWWW, "var", "www", "alex"), staleRoot, aside, other} {
		mkdirs(t, d)
	}
	writeFileT(t, filepath.Join(staleWWW, "var", "www", "alex", "index.php"), "<?php")
	got := s.staleStaging()
	slices.Sort(got)
	want := []string{staleRoot, staleWWW}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("stale %q, want %q", got, want)
	}
	s.removeStaging(got)
	for _, d := range want {
		if _, err := os.Lstat(d); err == nil {
			t.Errorf("%s stayed", d)
		}
	}
	for _, d := range []string{aside, other} {
		if _, err := os.Lstat(d); err != nil {
			t.Errorf("%s went: %v", d, err)
		}
	}
	mkdirs(t, staleWWW)
	if err := os.Chmod(s.restoreArea(), 0o777); err != nil {
		t.Fatal(err)
	}
	if got := s.staleStaging(); slices.Contains(got, staleWWW) {
		t.Fatalf("a restore area that is not the agent's alone: %q", got)
	}
}

// The restore area is the agent's: no call of the API changes, archives,
// measures, backs up or restores into anything there — the staging of a
// restore running meanwhile could otherwise get a symlink where restic
// writes, or a file sanitize never saw. A server backup leaves it out; a
// backup of a home has nothing of it to leave out.
func TestRestoreAreaIsFenced(t *testing.T) {
	s := stagingServer(t)
	ctx := context.Background()
	area := s.restoreArea()
	fd, err := s.openRestoreArea()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	staging := filepath.Join(area, ".in-place-0123456789abcdef")
	mkdirs(t, filepath.Join(staging, "var"), filepath.Join(s.cfg.WWWRoot, "alex"))
	refused := func(what string, err error) {
		t.Helper()
		var ae *Error
		if err == nil || !errors.As(err, &ae) || ae.Status < 400 || ae.Status >= 500 {
			t.Errorf("%s: want a refusal, got %v", what, err)
		}
	}
	for _, p := range []string{area, staging, filepath.Join(staging, "var")} {
		_, err := s.ensureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: filepath.Join(p, "x"), Mode: 0o755, Owner: "alex", Group: "alex"}}})
		refused("dirs/ensure in "+p, err)
		_, err = s.ensureDirs(ctx, &EnsureDirsRequest{Dirs: []DirSpec{{Path: p, Mode: 0o777}}})
		refused("dirs/ensure of "+p, err)
		_, err = s.ensureFile(ctx, &EnsureFileRequest{Path: filepath.Join(p, "f"), Owner: "alex", Content: "x"})
		refused("file/ensure in "+p, err)
		_, err = s.ensureSymlink(ctx, &EnsureSymlinkRequest{Path: filepath.Join(p, "l"), Target: "/etc", Owner: "alex"})
		refused("symlink/ensure in "+p, err)
		_, err = s.setACL(ctx, &SetACLRequest{Path: p, Entries: []string{"u:alex:rwx"}})
		refused("acl/set of "+p, err)
		_, err = s.siteACL(ctx, &SiteACLRequest{Root: p, Group: "alex"})
		refused("acl/site of "+p, err)
		_, err = s.removePaths(ctx, &RemovePathsRequest{Paths: []string{p}, Recursive: true})
		refused("paths/remove of "+p, err)
		_, err = s.removePaths(ctx, &RemovePathsRequest{Paths: []string{p}})
		refused("paths/remove (rmdir) of "+p, err)
		_, err = s.chown(ctx, &ChownRequest{Path: p, Owner: "alex", Recursive: true})
		refused("chown of "+p, err)
	}
	if _, err := os.Lstat(filepath.Join(staging, "var")); err != nil {
		t.Fatalf("the staging was touched: %v", err)
	}
	for _, e := range []string{"x", "f", "l"} {
		if _, err := os.Lstat(filepath.Join(staging, e)); err == nil {
			t.Errorf("%s was made in the staging", e)
		}
	}
	// the same calls of a home pass: the shapes are right, the place is not
	tools := func(dir string) []policyCase {
		return []policyCase{
			{"tar create", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", dir, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
			{"tar extract", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", dir, "--no-same-owner", "--warning=no-timestamp")},
			{"du", tc(modeTool, "du", "-sb", dir)},
			{"backup", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", dir), "RESTIC_REPOSITORY=s3:https://s3.example.com/b", "RESTIC_PASSWORD=pw")},
		}
	}
	mustAccept(t, s, tools(filepath.Join(s.cfg.WWWRoot, "alex")))
	mustRefuse(t, s, tools(staging))
	mustRefuse(t, s, tools(area))
	// a server backup leaves the restore area out, a home's has none of it
	for _, c := range []struct {
		path    string
		exclude bool
	}{{s.cfg.WWWRoot, true}, {filepath.Join(s.cfg.WWWRoot, "alex"), false}} {
		call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", c.path), "RESTIC_REPOSITORY=s3:https://s3.example.com/b", "RESTIC_PASSWORD=pw")
		plan, err := s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		plan.close()
		if got := slices.Contains(plan.args, "--exclude="+area); got != c.exclude || plan.args[6] != c.path {
			t.Errorf("backup of %s: %q", c.path, plan.args)
		}
	}
	// restic matches its patterns with filepath.Match
	if got := resticPattern("/srv/w*w/[x]?\\"); got != `/srv/w\*w/\[x]\?\\` {
		t.Errorf("pattern %q", got)
	}
}

// A snapshot that holds the restore area — one made by hand, without the
// agent's exclusion — never brings it into the live one.
func TestMergeSkipsTheRestoreArea(t *testing.T) {
	s := stagingServer(t)
	www := s.cfg.WWWRoot
	staging := t.TempDir()
	stage(t, staging, map[string]string{
		www + "/alex/index.html": "restored",
		www + "/" + restoreAreaName + "/.in-place-0123456789abcdef/evil": "staged",
	})
	m, out := runMerge(t, s, staging, www)
	if m.failed != 0 {
		t.Fatalf("failed: %s", out)
	}
	if _, err := os.Lstat(s.restoreArea()); err == nil {
		t.Fatal("the snapshot's restore area came into the web root")
	}
	if readFileT(t, filepath.Join(www, "alex", "index.html")) != "restored" || !strings.Contains(m.skippedWhy(), "restore area") {
		t.Fatalf("merge: %s, skipped %s", out, m.skippedWhy())
	}
}

// statfsWith has the staging's file system report free blocks (of 4 KiB)
// and free inodes of the test's choosing.
func statfsWith(t *testing.T, freeBlocks, freeInodes uint64) {
	t.Helper()
	old := stagingStatfs
	t.Cleanup(func() { stagingStatfs = old })
	stagingStatfs = func(fd int, st *unix.Statfs_t) error {
		if err := unix.Fstatfs(fd, st); err != nil {
			return err
		}
		st.Bsize, st.Frsize, st.Bavail, st.Files, st.Ffree = 4096, 4096, freeBlocks, 1<<30, freeInodes
		return nil
	}
}

// The sizes come from the snapshot, which the API composes: a count that
// wrapped around would make two files of 8 EB look like a few bytes, and
// let the restore fill the file system after all.
func TestStagingCountSaturates(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	statfsWith(t, 1<<30, 1<<20)
	listing := lsNodes(map[string]uint64{home + "/data/a": 1 << 63, home + "/data/b": 1 << 63})
	var calls [][]string
	var out bytes.Buffer
	_, err := s.restoreInPlace(context.Background(), fakeRestic(listing, &calls, nil), "latest", []string{home}, &out)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusInsufficientStorage || len(calls) != 1 {
		t.Fatalf("a listing of 16 EB: %v, calls %q", err, calls)
	}
	var n stageNeed
	n.add("file", 1<<64-1)
	n.add("file", 1)
	if n.bytes != 1<<64-1 || n.inodes != 2 {
		t.Fatalf("saturated count: %+v", n)
	}
}

// The stages of one restore that lie on one file system — the restore area
// and RestoreDir of a host with a single file system — are there at the same
// time, until the merge: each is checked against what all of them take.
func TestInPlaceStagesShareTheirFileSystem(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	dump := filepath.Join(s.backupTmpDir(), "run-3", "alex_wp.sql")
	mkdirs(t, home)
	var www, root unix.Stat_t
	if unix.Stat(s.cfg.WWWRoot, &www) != nil || unix.Stat(filepath.Dir(s.restoreRoot), &root) != nil || www.Dev != root.Dev {
		t.Skip("the test's web root and RestoreDir lie on different file systems")
	}
	// 1 GiB free; each part takes 600 MiB
	statfsWith(t, (1<<30)/4096, 1<<20)
	listing := lsNodes(map[string]uint64{home + "/data/www/big": 600 << 20, dump: 600 << 20})
	var calls [][]string
	var out bytes.Buffer
	for _, c := range []struct {
		includes []string
		ok       bool
	}{{[]string{home}, true}, {[]string{dump}, true}, {[]string{home, dump}, false}} {
		calls = nil
		code, err := s.restoreInPlace(context.Background(), fakeRestic(listing, &calls, nil), "latest", c.includes, &out)
		var ae *Error
		switch {
		case c.ok && (err != nil || code != 0):
			t.Errorf("%q alone: %d %v", c.includes, code, err)
		case !c.ok && (!errors.As(err, &ae) || ae.Status != http.StatusInsufficientStorage || len(calls) != 1):
			t.Errorf("%q together: %v, calls %q", c.includes, err, calls)
		case !c.ok && (!strings.Contains(ae.Message, home) || !strings.Contains(ae.Message, dump)):
			t.Errorf("the refusal names what takes the room: %s", ae.Message)
		}
	}
}

// A default ACL the restore area got after the agent made it — an
// administrator's setfacl -R -d over the web root — reaches nothing restic
// stages: the merge carries the snapshot's ACLs into the homes, copying
// those of a staged directory onto the live one, and an inherited entry
// would give whoever it names the restored home.
func TestStagingInheritsNoACL(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	fd, err := s.openRestoreArea()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	if out, err := exec.Command("setfacl", "-d", "-m", "u:"+strconv.Itoa(os.Getuid())+":rwx", s.restoreArea()).CombinedOutput(); err != nil {
		t.Skipf("no default ACL for the restore area here (%v %s)", err, out)
	}
	listing := lsNodes(map[string]uint64{home + "/data/www/index.php": 5})
	var calls [][]string
	run := fakeRestic(listing, &calls, func(args []string) {
		stage(t, args[3], map[string]string{home + "/data/www/index.php": "<?php"})
	})
	var out bytes.Buffer
	if code, err := s.restoreInPlace(context.Background(), run, "latest", []string{home}, &out); err != nil || code != 0 {
		t.Fatalf("restore: %d %v\n%s", code, err, out.String())
	}
	for _, p := range []string{home, filepath.Join(home, "data"), filepath.Join(home, "data", "www", "index.php")} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if hasAccessACL(int(f.Fd())) {
			t.Errorf("%s carries the restore area's default ACL", p)
		}
		f.Close()
	}
}

// A restore aside lands in RestoreDir, on the root file system next to MySQL
// and the mail queue: it is checked for room as the staging of an in-place
// restore is — once the earlier restore of that name is gone — for what its
// includes select; a pattern among them leaves the selection to restic, and
// the restore unchecked.
func TestRestoreAsideChecksSpace(t *testing.T) {
	s := policyServer(t, debian13)
	target := filepath.Join(t.TempDir(), "latest")
	files := map[string]uint64{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("/var/www/alex/f%d", i)] = 10 << 20
	}
	listing := lsNodes(files)
	statfsWith(t, (100<<20)/4096, 1<<20)
	for _, c := range []struct {
		includes []string
		ok       bool
	}{{nil, false}, {[]string{"/var/www/alex"}, false}, {[]string{"/var/www/alex/f1"}, true}, {[]string{"/var/www/*/f1"}, true}} {
		mkdirs(t, filepath.Join(target, "earlier"))
		var calls [][]string
		var out bytes.Buffer
		code, err := s.restoreAside(context.Background(), fakeRestic(listing, &calls, nil), "latest", target, c.includes, &out)
		var ae *Error
		switch {
		case c.ok && (err != nil || code != 0 || len(calls) != 2):
			t.Errorf("%q: %d %v %q", c.includes, code, err, calls)
		case !c.ok && (!errors.As(err, &ae) || ae.Status != http.StatusInsufficientStorage || len(calls) != 1 || !strings.Contains(ae.Message, "MB are free there")):
			t.Errorf("%q: %v, calls %q", c.includes, err, calls)
		}
	}
}
