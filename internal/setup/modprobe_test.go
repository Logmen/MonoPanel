package setup

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/osprofile"
)

// The package ships the modprobe rule, word for word what mp setup writes on
// an install without it, as /usr/lib/modprobe.d/monopanel.conf.
func TestModprobeRuleIsThePackages(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "modprobe.d", "monopanel.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != osprofile.ModprobeConf {
		t.Errorf("packaging/modprobe.d/monopanel.conf differs from mp setup's:\n%s\n---\n%s", b, osprofile.ModprobeConf)
	}
	nfpm, err := os.ReadFile(filepath.Join("..", "..", "packaging", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nfpm), "src: ./packaging/modprobe.d/monopanel.conf\n    dst: "+osprofile.ModprobePackageFile+"\n") {
		t.Error("the package does not ship the modprobe rule as " + osprofile.ModprobePackageFile)
	}
	// /bin/false, not /usr/bin/false: Debian without the merged /usr has
	// false in /bin only.
	for _, m := range osprofile.BlockedModules {
		if !strings.Contains(osprofile.ModprobeConf, "\ninstall "+m+" /bin/false\n") {
			t.Errorf("the rule does not keep %s from loading", m)
		}
	}
	if len(osprofile.BlockedModules) != 4 {
		t.Errorf("modules: %v", osprofile.BlockedModules)
	}
}

// mp setup writes /etc/modprobe.d/monopanel.conf only where the package's
// file is missing, and never over anything of that name: a file there is
// the administrator's word on which modules may load, a symlink is not
// written through.
func TestModprobeOnlyOnBinaryInstalls(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "usr/lib/modprobe.d/monopanel.conf")
	local := filepath.Join(dir, "etc/modprobe.d/monopanel.conf")

	// From the binary alone: the rule lands in /etc.
	wrote, err := ensureModprobe(pkg, local)
	if err != nil || !wrote {
		t.Fatalf("binary install: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(local); string(b) != osprofile.ModprobeConf {
		t.Fatalf("written:\n%s", b)
	}
	if st, _ := os.Stat(local); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode())
	}
	// Run again, after the administrator allowed sctp: their file stays.
	edited := strings.Replace(osprofile.ModprobeConf, "install sctp /bin/false\n", "", 1)
	if err := os.WriteFile(local, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("again: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(local); string(b) != edited {
		t.Fatal("the administrator's file was replaced")
	}

	// From the package: nothing is written.
	os.Remove(local)
	if err := os.MkdirAll(filepath.Dir(pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte(osprofile.ModprobeConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("package install: wrote=%v err=%v", wrote, err)
	}
	if _, err := os.Lstat(local); err == nil {
		t.Fatal("mp setup wrote the rule next to the package's")
	}

	// A symlink in /etc's place is not written through.
	os.Remove(pkg)
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, local); err != nil {
		t.Fatal(err)
	}
	if wrote, err := ensureModprobe(pkg, local); err != nil || wrote {
		t.Fatalf("symlink: wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("written through the symlink")
	}
}

// mp setup's copy of the rule in /etc/modprobe.d goes once the package's own
// is there and the copy is a text of the panel's, no newer than the
// package's: a host set up from the binary alone that took the package
// later (mp update apply). Without the package the copy is the rule. A file
// the administrator wrote stays, and is said to stay; so does one newer than
// the package's rule, or one beside a rule this version does not know. A
// symlink is no copy and is left alone without a word.
func TestPruneModprobeCopy(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "usr/lib/modprobe.d/monopanel.conf")
	local := filepath.Join(dir, "etc/modprobe.d/monopanel.conf")
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prune := func(want modprobeCopy, history ...string) {
		t.Helper()
		if len(history) == 0 {
			history = modprobeHistory()
		}
		got, err := pruneModprobeCopy(local, pkg, history)
		if err != nil || got != want {
			t.Fatalf("pruneModprobeCopy: %+v %v, want %+v", got, err, want)
		}
	}
	there := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}

	prune(modprobeCopy{})
	// From the binary alone: the copy is the rule, and stays.
	if wrote, err := ensureModprobe(pkg, local); err != nil || !wrote {
		t.Fatalf("binary install: %v %v", wrote, err)
	}
	prune(modprobeCopy{})
	if !there(local) {
		t.Fatal("the rule of an install without the package went")
	}
	// The package arrives: the copy goes, and the next run finds nothing.
	write(pkg, osprofile.ModprobeConf)
	prune(modprobeCopy{Removed: true})
	if there(local) || !there(pkg) {
		t.Fatalf("after the prune: copy %v, package %v", there(local), there(pkg))
	}
	prune(modprobeCopy{})

	// The administrator's: a module let load again, all four, a note added.
	for _, edited := range []string{
		strings.Replace(osprofile.ModprobeConf, "install sctp /bin/false\n", "", 1),
		"",
		osprofile.ModprobeConf + "# kept on purpose\n",
	} {
		write(local, edited)
		prune(modprobeCopy{Kept: true})
		if b, _ := os.ReadFile(local); string(b) != edited {
			t.Fatalf("the administrator's file was changed:\n%s", b)
		}
	}
	// A later release's copy over an older package's rule, and a copy
	// beside a package rule this version does not know: they stay.
	const older, later = "install sctp /bin/false\n", "install sctp /bin/false\ninstall dccp /bin/false\n"
	write(pkg, older)
	write(local, later)
	prune(modprobeCopy{Kept: true}, older, later)
	write(pkg, "install everything /bin/false\n")
	prune(modprobeCopy{Kept: true}, older, later)
	// An older release's copy under a later package's rule goes.
	write(pkg, later)
	write(local, older)
	prune(modprobeCopy{Removed: true}, older, later)

	// A symlink: no word, nothing removed.
	if err := os.Symlink("/dev/null", local); err != nil {
		t.Fatal(err)
	}
	prune(modprobeCopy{})
	if st, err := os.Lstat(local); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink: %v %v", st, err)
	}
}

// The rule's every released text stays in modprobeHistory, word for word,
// and today's is the last of them: a copy some release's mp setup wrote is
// told for the panel's own by it.
func TestModprobeHistoryKeepsEveryRelease(t *testing.T) {
	for text, want := range map[string]string{
		// 0.9.0's, today's.
		osprofile.ModprobeConf: "e5e245a1c6f042d5d9c0a2ff42bfe400a496480f0852eab0561e1d50f9ba4fab",
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(text))); got != want {
			t.Errorf("the rule's text changed (sha256 %s, pinned %s): keep the former one in modprobeHistory and pin the new one\n%s", got, want, text)
		}
	}
	h := modprobeHistory()
	if len(h) == 0 || h[len(h)-1] != osprofile.ModprobeConf {
		t.Error("today's rule is not the last text of modprobeHistory")
	}
	seen := map[string]bool{}
	for _, text := range h {
		if seen[text] {
			t.Errorf("one text twice in modprobeHistory:\n%s", text)
		}
		seen[text] = true
	}
}
