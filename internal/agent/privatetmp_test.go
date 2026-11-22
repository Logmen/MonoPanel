package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testBoot = "c843b57562264276be6595de996aa7eb"

// What the cleanup reads of /proc/self/mountinfo: each line's root and mount
// point, escapes undone, in the kernel's order.
func TestParseMounts(t *testing.T) {
	mi := "73 1 8:4 / / rw,relatime shared:1 - xfs /dev/sda4 rw,seclabel\n" +
		"814 794 8:4 /tmp/systemd-private-" + testBoot + "-monopanel-pkg-3a463a0b41f25d56.service-iPSzPZ/tmp /tmp rw,relatime shared:449 master:1 - xfs /dev/sda4 rw\n" +
		"90 73 0:40 /a\\040b /mnt/x\\040y rw - tmpfs tmpfs rw\n" +
		"short line\n"
	got := parseMounts([]byte(mi))
	want := []mountEntry{
		{root: "/", point: "/"},
		{root: "/tmp/systemd-private-" + testBoot + "-monopanel-pkg-3a463a0b41f25d56.service-iPSzPZ/tmp", point: "/tmp"},
		{root: "/a b", point: "/mnt/x y"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
}

// The cleanup empties a unit's own /tmp and /var/tmp, and nothing else: the
// directory Percona's %post leaves (empty, rpm_script_tmp_t on EL) and
// whatever else is there goes; a symlink goes, not what it points to; an
// entry with a mount at or below it stays. A directory that is not the
// unit's own stays as it is — the host's /tmp of a unit a drop-in started
// without PrivateTmp=, another unit's private /tmp, a directory with no
// mount of its own —, and the cleanup says so.
func TestCleanPrivateTmpEmptiesOnlyTheUnitsOwn(t *testing.T) {
	const unit = "monopanel-pkg-3a463a0b41f25d56.service"
	base := t.TempDir()
	dir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tmp, vartmp, outside := dir("tmp"), dir("var-tmp"), dir("outside")
	dir("tmp/HQGFadnM")
	write(filepath.Join(tmp, "rpm-tmp.Ab12Cd/deep/file"))
	write(filepath.Join(tmp, ".hidden"))
	write(filepath.Join(outside, "keep"))
	if err := os.Symlink(outside, filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	dir("tmp/bound/inner")
	write(filepath.Join(tmp, "bound", "inner", "f"))
	write(filepath.Join(vartmp, "dnf-cache.lock"))
	host, other, bare := dir("host-tmp"), dir("other-tmp"), dir("bare")
	for _, d := range []string{host, other, bare} {
		write(filepath.Join(d, "theirs"))
	}
	mounts := []mountEntry{
		{root: "/", point: "/"},
		// a lower mount at tmp, which the unit's own covers
		{root: "/", point: tmp},
		{root: "/tmp/systemd-private-" + testBoot + "-" + unit + "-iPSzPZ/tmp", point: tmp},
		{root: "/var/tmp/systemd-private-" + testBoot + "-" + unit + "-6ylspz/tmp", point: vartmp},
		{root: "/", point: filepath.Join(tmp, "bound", "inner")},
		// the host's tmpfs, another unit's own /tmp
		{root: "/", point: host},
		{root: "/tmp/systemd-private-" + testBoot + "-monopanel-agent.service-1R7SPj/tmp", point: other},
	}
	err := cleanPrivateTmp(mounts, unit, []string{tmp, vartmp})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(tmp, "bound")+": a mount at "+filepath.Join(tmp, "bound", "inner")+", left as it is") {
		t.Fatalf("the mount below: %v", err)
	}
	for p, stays := range map[string]bool{
		filepath.Join(tmp, "HQGFadnM"): false, filepath.Join(tmp, "rpm-tmp.Ab12Cd"): false, filepath.Join(tmp, ".hidden"): false,
		filepath.Join(tmp, "link"): false, filepath.Join(vartmp, "dnf-cache.lock"): false,
		filepath.Join(outside, "keep"): true, filepath.Join(tmp, "bound", "inner", "f"): true, tmp: true, vartmp: true,
	} {
		if _, err := os.Lstat(p); (err == nil) != stays {
			t.Errorf("%s: stays %v, want %v", p, err == nil, stays)
		}
	}
	// what is not the unit's own is said in one line: the cleanup's words
	// end the run's output, of which a job's log shows the last lines
	err = cleanPrivateTmp(mounts, unit, []string{host, other, bare})
	for _, d := range []string{host, other, bare} {
		if _, serr := os.Lstat(filepath.Join(d, "theirs")); serr != nil {
			t.Errorf("%s was emptied: %v", d, serr)
		}
	}
	if want := host + ", " + other + " and " + bare + " are not " + unit + "'s own (PrivateTmp=): left as they are"; err == nil || err.Error() != want {
		t.Errorf("what is not the unit's own: %v, want %s", err, want)
	}
	if err, want := cleanPrivateTmp(mounts, unit, []string{host}), host+" is not "+unit+"'s own (PrivateTmp=): left as it is"; err == nil || err.Error() != want {
		t.Errorf("a directory that is not the unit's own: %v, want %s", err, want)
	}
	// the unit's own once more: nothing left to remove but what stays
	if err := cleanPrivateTmp(mounts[:4], unit, []string{vartmp}); err != nil {
		t.Errorf("an empty /var/tmp: %v", err)
	}
}

// The root of a mount that is the unit's own /tmp or /var/tmp is systemd's
// directory for it and nothing else: systemd-private-, the boot id (32
// hexadecimal digits in lower case, sd_id128's), the unit's name as it is,
// the six letters or digits of mkdtemp, /tmp — on the root file system, on
// a tmpfs at /tmp, on a subvolume. A directory below it, a name that only
// ends or begins like it, another unit's, a malformed boot id or suffix are
// not.
func TestPrivateTmpRoot(t *testing.T) {
	const unit = "monopanel-pkg-3a463a0b41f25d56.service"
	own := privateTmpRoot(unit)
	dir := func(boot, unit, suffix string) string {
		return "systemd-private-" + boot + "-" + unit + "-" + suffix + "/tmp"
	}
	for root, want := range map[string]bool{
		"/tmp/" + dir(testBoot, unit, "iPSzPZ"):  true,
		"/" + dir(testBoot, unit, "iPSzPZ"):      true,
		"/@tmp/" + dir(testBoot, unit, "6ylspz"): true,
		"/tmp/" + dir(testBoot, unit, "123456"):  true,

		"/tmp/" + dir(testBoot[1:], unit, "iPSzPZ"):                           false,
		"/tmp/" + dir(testBoot+"0", unit, "iPSzPZ"):                           false,
		"/tmp/" + dir("", unit, "iPSzPZ"):                                     false,
		"/tmp/" + dir(strings.ToUpper(testBoot), unit, "iPSzPZ"):              false,
		"/tmp/" + dir(strings.Repeat("g", 32), unit, "iPSzPZ"):                false,
		"/tmp/" + dir(testBoot, unit, "iPSzP"):                                false,
		"/tmp/" + dir(testBoot, unit, "iPSzPZZ"):                              false,
		"/tmp/" + dir(testBoot, unit, "iPS_PZ"):                               false,
		"/tmp/" + dir(testBoot, unit, "iPSzPZ") + "/sub":                      false,
		"/tmp/" + dir(testBoot, unit, "iPSzPZ") + "x":                         false,
		"/x/z" + dir(testBoot, unit, "iPSzPZ"):                                false,
		"/tmp/" + dir(testBoot, "monopanel-agent.service", "1R7SPj"):          false,
		"/tmp/" + dir(testBoot, "x"+unit, "iPSzPZ"):                           false,
		"/tmp/" + dir(testBoot, strings.Replace(unit, ".", "x", 1), "iPSzPZ"): false,
		"/": false, "": false,
	} {
		if got := own.MatchString(root); got != want {
			t.Errorf("%q: the unit's own %v, want %v", root, got, want)
		}
	}
}

// PrivateTmp= gives a unit both, and both are emptied: rpm writes its
// scriptlets into /var/tmp, and a %post may leave its directory there as well
// as in /tmp. (CleanPrivateTmp works on the calling process's own, which no
// test may run on.)
func TestPrivateTmpDirs(t *testing.T) {
	if !slices.Equal(privateTmpDirs, []string{"/tmp", "/var/tmp"}) {
		t.Fatalf("the cleanup empties %q", privateTmpDirs)
	}
}

// Only a service unit's name is taken: it becomes part of what the mount's
// root must match. (No test calls it with a good one: that would read the
// test's own /tmp.)
func TestCleanPrivateTmpWantsAUnitName(t *testing.T) {
	for _, bad := range []string{"", "../tmp", "monopanel-agent", "a b.service", "x.service/..", "monopanel-agent.socket"} {
		if err := CleanPrivateTmp(bad); err == nil || !strings.Contains(err.Error(), "not the name of a service unit") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
