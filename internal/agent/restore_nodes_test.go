package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lsModes is restic ls --json of a snapshot holding the nodes (name, type,
// path) with their os.FileMode as restic prints it.
func lsModes(nodes ...lsMode) string {
	var b strings.Builder
	b.WriteString(`{"time":"2026-11-21T10:00:00Z","tree":"ab","paths":["/var/www"],"hostname":"h","id":"` + snapID + `","short_id":"1a2b3c4d","struct_type":"snapshot"}` + "\n")
	for _, n := range nodes {
		fmt.Fprintf(&b, `{"name":%q,"type":%q,"path":%q,"uid":0,"gid":0,"mode":%d,"struct_type":"node"}`+"\n", filepath.Base(n.path), n.typ, n.path, uint32(n.mode))
	}
	return b.String()
}

type lsMode struct {
	typ, path string
	mode      os.FileMode
}

// A restore brings back files, directories and symlinks: a snapshot whose
// selection holds a device node, a FIFO, a socket, a node of a type restic
// may know one day, or a set-user-ID or set-group-ID file is refused before
// restic writes anything — whoever reached its staging before sanitize
// could open the node or run the file — and the refusal names it. A
// set-group-ID directory (the panel's shared folders are 2770) and what
// lies outside the selection pass.
func TestVetSnapshotRefusesNodes(t *testing.T) {
	s := policyServer(t, debian13)
	base := []lsMode{{"dir", "/var", os.ModeDir | 0o755}, {"dir", "/var/www", os.ModeDir | 0o711}, {"dir", "/var/www/alex", os.ModeDir | 0o710}}
	selected := func(p string) bool { return atOrBelow(p, "/var/www/alex") }
	for _, c := range []struct {
		node lsMode
		why  string
	}{
		{lsMode{"dev", "/var/www/alex/sda", os.ModeDevice | 0o666}, "a block device node"},
		{lsMode{"chardev", "/var/www/alex/mem", os.ModeDevice | os.ModeCharDevice | 0o666}, "a character device node"},
		{lsMode{"fifo", "/var/www/alex/pipe", os.ModeNamedPipe | 0o666}, "a FIFO"},
		{lsMode{"socket", "/var/www/alex/s", os.ModeSocket | 0o777}, "a socket"},
		{lsMode{"irregular", "/var/www/alex/x", os.ModeIrregular | 0o644}, `a node of the type "irregular"`},
		{lsMode{"file", "/var/www/alex/su", os.ModeSetuid | 0o755}, "a set-user-ID file"},
		{lsMode{"file", "/var/www/alex/sg", os.ModeSetgid | 0o755}, "a set-group-ID file"},
	} {
		var calls [][]string
		listing := lsModes(append(base, c.node)...)
		_, _, err := s.vetSnapshot(fakeRestic(listing, &calls, nil), "latest", selected, nil)
		if agentStatus(err) != 403 || !strings.Contains(err.Error(), c.node.path+", "+c.why) {
			t.Errorf("%s: %v, want 403 naming it", c.node.path, err)
		}
		// outside what the restore brings it is none of its business
		outside := c.node
		outside.path = "/var/www/bob"
		if _, _, err := s.vetSnapshot(fakeRestic(lsModes(append(base, outside)...), &calls, nil), "latest", selected, nil); err != nil {
			t.Errorf("%s outside the selection: %v", c.why, err)
		}
		// a restore of everything: everything counts
		if _, _, err := s.vetSnapshot(fakeRestic(listing, &calls, nil), "latest", nil, nil); agentStatus(err) != 403 {
			t.Errorf("%s in a restore of everything: %v", c.why, err)
		}
	}
	fine := lsModes(append(base,
		lsMode{"dir", "/var/www/alex/shared", os.ModeDir | os.ModeSetgid | 0o770},
		lsMode{"dir", "/var/www/alex/odd", os.ModeDir | os.ModeSetuid | 0o755},
		lsMode{"file", "/var/www/alex/index.php", 0o644},
		lsMode{"symlink", "/var/www/alex/current", os.ModeSymlink | 0o777},
	)...)
	var calls [][]string
	if id, _, err := s.vetSnapshot(fakeRestic(fine, &calls, nil), "latest", nil, nil); err != nil || id != snapID {
		t.Fatalf("files, directories and symlinks: %q %v", id, err)
	}
}

// The restores refuse such a snapshot before restic restores anything: in
// place by the includes, aside by its includes — every node when there are
// none.
func TestRestoresRefuseNodesBeforeRestic(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	www := strings.Split(strings.TrimPrefix(home, "/"), "/")
	var nodes []lsMode
	for i := range www {
		nodes = append(nodes, lsMode{"dir", "/" + strings.Join(www[:i+1], "/"), os.ModeDir | 0o755})
	}
	nodes = append(nodes, lsMode{"chardev", home + "/mem", os.ModeDevice | os.ModeCharDevice | 0o666})
	listing := lsModes(nodes...)
	var calls [][]string
	var out bytes.Buffer
	if _, err := s.restoreInPlace(context.Background(), fakeRestic(listing, &calls, nil), "latest", []string{home}, &out); agentStatus(err) != 403 || len(calls) != 1 {
		t.Fatalf("in place: %v, restic calls %q", err, calls)
	}
	calls = nil
	target := filepath.Join(t.TempDir(), "latest")
	if _, err := s.restoreAside(context.Background(), fakeRestic(listing, &calls, nil), "latest", target, nil, &out); agentStatus(err) != 403 || len(calls) != 1 {
		t.Fatalf("aside: %v, restic calls %q", err, calls)
	}
	calls = nil
	if _, err := s.restoreAside(context.Background(), fakeRestic(listing, &calls, nil), "latest", target, []string{filepath.Join(s.cfg.WWWRoot, "bob")}, &out); err != nil || len(calls) != 2 {
		t.Fatalf("aside, the node left out: %v, restic calls %q", err, calls)
	}
}

// No shared folder is bound while an in-place restore lies staged: the
// share guard is held from the first restic run to the end of the merge —
// before, its staging below the web root was guarded during the merge
// alone — and the merge, which is under it, does not take it again.
func TestRestoreInPlaceHoldsTheShareGuard(t *testing.T) {
	s := stagingServer(t)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	listing := lsNodes(map[string]uint64{home + "/data/www/index.php": 5})
	var calls [][]string
	held := false
	run := fakeRestic(listing, &calls, func(args []string) {
		if s.shareMu.TryLock() {
			s.shareMu.Unlock()
		} else {
			held = true
		}
		stage(t, args[3], map[string]string{home + "/data/www/index.php": "<?php"})
	})
	var out bytes.Buffer
	if code, err := s.restoreInPlace(context.Background(), run, "latest", []string{home}, &out); err != nil || code != 0 {
		t.Fatalf("restore: %d %v\n%s", code, err, out.String())
	}
	if !held {
		t.Fatal("a shared folder could have been bound while restic restored into the staging")
	}
	if readFileT(t, filepath.Join(home, "data", "www", "index.php")) != "<?php" {
		t.Fatal("not moved into place")
	}
	if !s.shareMu.TryLock() {
		t.Fatal("the guard is still held after the restore")
	}
	s.shareMu.Unlock()
}
