package agent

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A recursive chown with a mode reaches the files of the tree through
// descriptors: a symlink in the tree is re-owned itself, its target keeps
// its mode.
func TestChownWalkDoesNotFollow(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	me := withMeAsClient(t, s)
	site := filepath.Join(s.cfg.WWWRoot, "alex", "data", "www", "site")
	mkdirs(t, filepath.Join(site, "sub"))
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(site, "index.php"), filepath.Join(site, "sub", "a.php")} {
		if err := os.WriteFile(f, []byte("<?php"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(site, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	res, err := s.chown(ctx, &ChownRequest{Path: site, Owner: me, Recursive: true, Mode: 0o640})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 2 {
		t.Fatalf("changed %d, want the two files", res.Changed)
	}
	for _, f := range []string{filepath.Join(site, "index.php"), filepath.Join(site, "sub", "a.php")} {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o640 {
			t.Errorf("%s: %v", f, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() != 0o600 {
		t.Fatalf("the symlink's target changed to %v", st.Mode().Perm())
	}
	// a symlink in place of the tree itself is refused
	if err := os.Symlink(t.TempDir(), filepath.Join(s.cfg.WWWRoot, "alex", "data", "www", "swapped")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.chown(ctx, &ChownRequest{Path: filepath.Join(s.cfg.WWWRoot, "alex", "data", "www", "swapped"), Owner: me, Recursive: true}); err == nil {
		t.Fatal("a symlinked tree must be refused")
	}
	if _, err := s.chown(ctx, &ChownRequest{Path: filepath.Join(s.cfg.WWWRoot, "alex", "data", "www", "swapped", "x"), Owner: me}); err == nil {
		t.Fatal("a path through a symlink must be refused")
	}
}

// Without fs.protected_hardlinks a file with a second name may be a file of
// root's the account linked into its tree: it stays as it is.
func TestChownLeavesForeignHardLinks(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	me := withMeAsClient(t, s)
	site := filepath.Join(s.cfg.WWWRoot, "alex", "data")
	mkdirs(t, site)
	outside := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(site, "shadow")); err != nil {
		t.Skipf("hard links: %v", err)
	}
	old := hardlinksProtected
	hardlinksProtected = func() bool { return false }
	t.Cleanup(func() { hardlinksProtected = old })
	res, err := s.chown(ctx, &ChownRequest{Path: site, Owner: me, Recursive: true, Mode: 0o666})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped %d", res.Skipped)
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() != 0o600 {
		t.Fatalf("the linked file changed to %v", st.Mode().Perm())
	}
}

// Owners: the panel's accounts below the web root, vmail in the mail store;
// never root, a system account or a privileged group; nowhere else.
func TestChownRefusesOwnersAndPlaces(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	site := filepath.Join(s.cfg.WWWRoot, "alex", "data")
	mkdirs(t, site)
	for name, req := range map[string]ChownRequest{
		"outside":          {Path: "/etc/shadow", Owner: currentUserName(t)},
		"the web root":     {Path: s.cfg.WWWRoot, Owner: currentUserName(t), Recursive: true},
		"relative":         {Path: "var/www/alex", Owner: currentUserName(t)},
		"privileged group": {Path: site, Owner: currentUserName(t), Group: "root"},
	} {
		req := req
		if _, err := s.chown(ctx, &req); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	withAccounts(t, standardAccounts(s)...)
	for name, req := range map[string]ChownRequest{
		"root":          {Path: site, Owner: "root"},
		"system":        {Path: site, Owner: "nginx"},
		"service user":  {Path: site, Owner: "monopanel"},
		"vmail in home": {Path: site, Owner: "vmail"},
		"mail to alex":  {Path: mailStore + "/example.com", Owner: "alex"},
		"unknown":       {Path: site, Owner: "nobody-here"},
		// ordinary uids that are not the panel's clients
		"an administrator": {Path: site, Owner: "admin"},
		"a docker member":  {Path: site, Owner: "sneaky"},
	} {
		req := req
		if _, err := s.chown(ctx, &req); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// withMeAsClient registers the test's own user as one of the panel's
// clients, its home <www_root>/<login>: chown hands files to a client, and a
// test can hand files to itself only.
func withMeAsClient(t *testing.T, s *Server) string {
	t.Helper()
	me := currentUserName(t)
	withAccounts(t, testAccount{me, strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()), filepath.Join(s.cfg.WWWRoot, me), []string{me}})
	return me
}

// A client's tree goes to the client and the client's own group, no other.
func TestChownKeepsToTheClientsGroup(t *testing.T) {
	ctx := context.Background()
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	site := filepath.Join(s.cfg.WWWRoot, "bob", "data")
	mkdirs(t, site)
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil || g.Name == "bob" {
		t.Skip("no group of the test's own to try")
	}
	_, err = s.chown(ctx, &ChownRequest{Path: site, Owner: "bob", Group: g.Name, Recursive: true})
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 403 || !strings.Contains(ae.Message, "own group") {
		t.Fatalf("chown to bob:%s: %v", g.Name, err)
	}
}
