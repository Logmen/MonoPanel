package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// asRoot makes the test's own uid stand for root in trustedSymlink: the
// files the agent makes are root's in production, and the test's here.
func asRoot(t *testing.T) {
	t.Helper()
	old := symlinkTrustUID
	symlinkTrustUID = uint32(os.Getuid())
	t.Cleanup(func() { symlinkTrustUID = old })
}

// withServiceUserOfTheTest is the standard account database with the
// service user being the test's own uid: what is restored into the data
// directory is handed to it, and a test can only hand files to itself.
func withServiceUserOfTheTest(t *testing.T, s *Server) {
	t.Helper()
	var accounts []testAccount
	for _, a := range standardAccounts(s) {
		if a.login != s.cfg.ServiceUser {
			accounts = append(accounts, a)
		}
	}
	accounts = append(accounts, testAccount{s.cfg.ServiceUser, strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()), s.cfg.DataDir, nil})
	withAccounts(t, accounts...)
}

// The rule that follows root's symlinks in root's directories holds for the
// system's directories only: below one of the panel's trees no symlink is
// followed, whoever owns it.
func TestOpenDirBelowFollowsNothingBelowTheTree(t *testing.T) {
	asRoot(t)
	dir := t.TempDir()
	tree := filepath.Join(dir, "www")
	outside := filepath.Join(dir, "outside")
	mkdirs(t, filepath.Join(tree, "alex", "d"), filepath.Join(outside, "sub"))
	if err := os.Symlink(outside, filepath.Join(tree, "alex", "d", "evil")); err != nil {
		t.Fatal(err)
	}
	// a symlink "of root's in a directory of root's" passes the old rule…
	fd, err := openDirNoFollow(filepath.Join(tree, "alex", "d", "evil", "sub"))
	if err != nil {
		t.Fatalf("the system rule: %v", err)
	}
	unix.Close(fd)
	// …and nothing below the tree
	for _, p := range []string{filepath.Join(tree, "alex", "d", "evil"), filepath.Join(tree, "alex", "d", "evil", "sub")} {
		if fd, err := openDirBelow(tree, p, false, 0); err == nil {
			unix.Close(fd)
			t.Fatalf("%s: a symlink below the tree was followed", p)
		} else if !errors.Is(err, errSymlinkComponent) {
			t.Fatalf("%s: want errSymlinkComponent, got %v", p, err)
		}
	}
	if fd, err := openDirBelow(tree, filepath.Join(tree, "alex", "d", "evil", "new"), true, 0o755); err == nil {
		unix.Close(fd)
		t.Fatal("made a directory through the symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("a directory appeared outside")
	}
	// the tree itself may be reached through a symlink of root's (a web root
	// an administrator moved elsewhere), and the walk below it is strict
	if err := os.Symlink(tree, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	fd, err = openDirBelow(filepath.Join(dir, "moved"), filepath.Join(dir, "moved", "alex", "d"), false, 0)
	if err != nil {
		t.Fatalf("a moved tree: %v", err)
	}
	unix.Close(fd)
	// paths outside the tree or with ".." are not walked at all
	for _, p := range []string{outside, tree + "/alex/../../outside", dir} {
		if fd, err := openDirBelow(tree, p, false, 0); err == nil {
			unix.Close(fd)
			t.Fatalf("%s: walked", p)
		}
	}
}

// The attack the strict walk closes: an archive the agent unpacked (as root)
// leaves a directory and a symlink in it to /etc, both root's; the next call
// names a path through that symlink. Extraction, chown, tar --create, du,
// restic backup and an in-place restore all refuse it, and nothing outside
// changes.
func TestRootsOwnSymlinkInAHomeLeadsNowhere(t *testing.T) {
	asRoot(t)
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	outside := t.TempDir()
	mkdirs(t, home, filepath.Join(outside, "cron.d"))
	if err := os.WriteFile(filepath.Join(outside, "shadow"), []byte("root:hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := makeTar(t, false, tarEntry{name: "d/", typ: '5', mode: 0o755}, tarEntry{name: "d/evil", typ: '2', link: outside})
	if code, out := extractInto(t, home, untarOpts{}, first); code != 0 {
		t.Fatalf("first archive: %d %s", code, out)
	}
	via := filepath.Join(home, "d", "evil")
	var ae *Error
	cases := []policyCase{
		{"untar through it", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", via+"/cron.d", "--no-same-owner")},
		{"untar into it", tc(modeStreamIn, "tar", "-xzf", "-", "-C", via, "--no-same-owner")},
		{"tar --create of it", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", via, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"du through it", tc(modeTool, "du", "-sb", via+"/cron.d")},
		{"backup through it", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", via+"/shadow"), "RESTIC_REPOSITORY=/var/backups/x", "RESTIC_PASSWORD=p")},
	}
	mustRefuse(t, s, cases)
	if _, err := s.chown(context.Background(), &ChownRequest{Path: via + "/shadow", Owner: "alex"}); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("chown through it: %v", err)
	}
	// an in-place restore through it fails, and writes nothing outside
	staging := t.TempDir()
	stage(t, staging, map[string]string{via + "/cron.d/job": "* * * * * root id"})
	m, out := runMerge(t, s, staging, via+"/cron.d/job")
	if m.failed != 1 || !strings.Contains(out, "refusing") {
		t.Fatalf("in place: failed %d: %s", m.failed, out)
	}
	if _, err := os.Stat(filepath.Join(outside, "cron.d", "job")); err == nil {
		t.Fatal("the restore wrote through the symlink")
	}
	if readFileT(t, filepath.Join(outside, "shadow")) != "root:hash" {
		t.Fatal("changed outside")
	}
}

// Without the planted symlink the same calls pass: the rule takes nothing
// from the panel.
func TestHomesStillWalk(t *testing.T) {
	asRoot(t)
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	site := filepath.Join(home, "data", "www", "shop.example.com")
	mkdirs(t, site)
	mustAccept(t, s, []policyCase{
		{"untar", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--no-same-owner", "--warning=no-timestamp")},
		{"tar --create", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"du", tc(modeTool, "du", "-sb", home)},
		{"backup site", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=site:shop.example.com", site), "RESTIC_REPOSITORY=/var/backups/x", "RESTIC_PASSWORD=p")},
	})
}

// The mail store is often reached through /var/mail -> spool/mail: du of a
// mail domain passes the policy, and on a host without the domain it is a
// directory that is not there, not a refusal.
func TestDuOfAMailDomain(t *testing.T) {
	s := policyServer(t, debian13)
	plan, err := s.planTool(&toolCall{name: "du", args: []string{"-sb", mailStore + "/example.com"}, mode: modeTool})
	var ae *Error
	switch {
	case err == nil:
		if strings.Join(plan.args, " ") != "-sb --dereference-args /proc/self/fd/3" || len(plan.files) != 1 {
			t.Fatalf("du argv %q", plan.args)
		}
		plan.close()
	case errors.As(err, &ae) && ae.Status == 404:
	default:
		t.Fatalf("du of a mail domain: %v", err)
	}
}

// A restore in place never replaces one of the trees themselves: not the web
// root with a symlink of the snapshot's.
func TestRestoreInPlaceKeepsTheTrees(t *testing.T) {
	s := policyServer(t, debian13)
	www := s.cfg.WWWRoot
	outside := t.TempDir()
	staging := t.TempDir()
	mkdirs(t, filepath.Join(staging, filepath.Dir(www)))
	if err := os.Symlink(outside, filepath.Join(staging, www)); err != nil {
		t.Fatal(err)
	}
	m, out := runMerge(t, s, staging, www)
	if m.failed != 1 {
		t.Fatalf("failed %d: %s", m.failed, out)
	}
	if st, err := os.Lstat(www); err != nil || !st.IsDir() {
		t.Fatalf("the web root was replaced: %v %v", st, err)
	}
	// a staged web root that is a directory is merged into the live one
	staging = t.TempDir()
	stage(t, staging, map[string]string{www + "/alex/index.html": "restored"})
	if m, out := runMerge(t, s, staging, www); m.failed != 0 {
		t.Fatalf("merge: %s", out)
	}
	if readFileT(t, filepath.Join(www, "alex", "index.html")) != "restored" {
		t.Fatal("not merged")
	}
}

// Below the configuration directory a restore brings no symlink, nothing
// writable by others, and nothing the content checks of config/apply refuse.
func TestRestoreInPlaceConfiguration(t *testing.T) {
	s := policyServer(t, debian13)
	withServiceUserOfTheTest(t, s)
	old := restoreConfCheck
	restoreConfCheck = func(p string, content []byte) error {
		if bytes.Contains(content, []byte("load_module")) {
			return errors.New("load_module is not allowed")
		}
		return nil
	}
	t.Cleanup(func() { restoreConfCheck = old })
	conf := s.cfg.ConfigDir
	staging := t.TempDir()
	stage(t, staging, map[string]string{
		conf + "/templates/site.tmpl": "server {}",
		conf + "/php/8.3/evil.conf":   "load_module /tmp/x.so;",
	})
	if err := os.Chmod(filepath.Join(staging, conf, "templates", "site.tmpl"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(staging, conf, "templates"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/systemd/system", filepath.Join(staging, conf, "apps")); err != nil {
		t.Fatal(err)
	}
	m, out := runMerge(t, s, staging, conf)
	if m.failed != 1 || !strings.Contains(out, "load_module") {
		t.Fatalf("failed %d: %s", m.failed, out)
	}
	if _, err := os.Lstat(filepath.Join(conf, "apps")); err == nil {
		t.Fatal("a symlink arrived in the configuration directory")
	}
	if _, err := os.Lstat(filepath.Join(conf, "php", "8.3", "evil.conf")); err == nil {
		t.Fatal("content the checks refuse arrived")
	}
	for _, p := range []string{filepath.Join(conf, "templates"), filepath.Join(conf, "templates", "site.tmpl")} {
		if st, err := os.Lstat(p); err != nil || st.Mode().Perm()&0o022 != 0 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if !strings.Contains(m.skippedWhy(), "symlinks") {
		t.Fatalf("skipped: %s", m.skippedWhy())
	}
}

// Into the data directory a restore brings the service user's files, and
// the directories it makes on the way are the service user's.
func TestRestoreInPlaceData(t *testing.T) {
	s := policyServer(t, debian13)
	withServiceUserOfTheTest(t, s)
	dump := filepath.Join(s.backupTmpDir(), "run-7", "alex_wp.sql")
	certs := filepath.Join(s.cfg.DataDir, "certs")
	staging := t.TempDir()
	stage(t, staging, map[string]string{dump: "-- dump", certs + "/example.com/fullchain.pem": "PEM"})
	m, out := runMerge(t, s, staging, dump, certs)
	if m.failed != 0 {
		t.Fatalf("failed: %s", out)
	}
	if readFileT(t, dump) != "-- dump" || readFileT(t, certs+"/example.com/fullchain.pem") != "PEM" {
		t.Fatal("content")
	}
	if st, err := os.Stat(filepath.Dir(dump)); err != nil || !st.IsDir() {
		t.Fatalf("run directory: %v", err)
	}
}
