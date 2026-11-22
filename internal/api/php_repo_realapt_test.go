package api

// What real apt prints, fed to the readers of ops_php.go (and, at the end,
// the forms a mirror of Sury takes in apt-cache policy). testdata/realapt
// holds the output of the agent's own argv and environment (osprofile
// aptManager: apt-get -q -o DPkg::Lock::Timeout=600 -o
// Acquire::AllowReleaseInfoChange::Label=true update, apt-cache policy
// php-common, LANG=C.UTF-8 LC_ALL=C.UTF-8) on the testbed's VMs on
// 2026-11-22: mp-ubuntu2204 (jammy, apt 2.4.14), mp-ubuntu2604 (resolute,
// apt 3.2.0), mp-debian12 (bookworm, apt 2.6.1), with Sury's source written
// exactly as suryRepoFiles writes it (apt.gpg as downloaded, sha256
// b486fd54…); the *-ru files with LANGUAGE=ru as well, which the agent's
// environment now empties. update-{resolute,bookworm}-fails-elsewhere and
// update-resolute-lists-lock are the third review's, on the same VMs with
// the panel deployed: an update that failed for a repository without a
// Release file, and one that met the lists' lock another process held.

import (
	"slices"
	"testing"

	"monopanel/internal/agent"
)

// suryIndexed on real apt-cache policy output: the host's suite counts,
// another suite, the PPA, a failed apt-cache (Signed-By conflict) do not;
// after a transient failure apt keeps the old lists and they count.
func TestRealAptPolicySuryIndexed(t *testing.T) {
	withOSRelease(t, ubuntuNoble)
	f := newSiteFixture(t)
	for _, c := range []struct {
		file, codename string
		want           bool
	}{
		{"policy-jammy-sury.txt", "jammy", true},
		{"policy-jammy-sury.txt", "noble", false},
		{"policy-jammy-sury.txt", "resolute", false},
		{"policy-resolute-sury.txt", "resolute", true},
		{"policy-resolute-sury.txt", "noble", false},
		{"policy-bookworm-sury.txt", "bookworm", true},
		{"policy-bookworm-sury.txt", "trixie", false},
		// The panel's source written, no apt-get update yet: no lists.
		{"policy-jammy-before-update.txt", "jammy", false},
		// Sury for noble written by hand on jammy.
		{"policy-jammy-noble-suite.txt", "jammy", false},
		{"policy-jammy-noble-suite.txt", "noble", true},
		// 0.8.19's PPA alone, and with Sury beside it (the move's write).
		{"policy-jammy-ppa-only.txt", "jammy", false},
		{"policy-jammy-ppa-and-sury.txt", "jammy", true},
		// Two sources of one repository with different Signed-By: apt-cache
		// policy fails as apt-get update does (exit 100, E: only).
		{"policy-jammy-conflict.txt", "jammy", false},
		{"policy-resolute-conflict.txt", "resolute", false},
		{"policy-bookworm-conflict.txt", "bookworm", false},
		{"policy-jammy-ppa-conflict-inline.txt", "jammy", false},
		// A transient failure of Sury (exit 0, W: Failed to fetch): with the
		// lists of an earlier update apt uses them, without them nothing.
		{"policy-jammy-transient-old-lists.txt", "jammy", true},
		{"policy-jammy-transient-no-lists.txt", "jammy", false},
		{"policy-resolute-transient-old-lists.txt", "resolute", true},
		{"policy-resolute-transient-no-lists.txt", "resolute", false},
		{"policy-bookworm-transient-old-lists.txt", "bookworm", true},
		// The update failed for another source (the PPA's Label, plain
		// apt-get update): Sury's lists were committed all the same.
		{"policy-jammy-fails-elsewhere.txt", "jammy", true},
		// apt translated by LANGUAGE=ru despite LC_ALL=C.UTF-8: the origin
		// lines are not translated, suryIndexed holds.
		{"policy-bookworm-ru.txt", "bookworm", true},
	} {
		f.agent.PkgOutput = map[string]string{"available": realApt(t, c.file)}
		if got := f.s.suryIndexed(f.ctx, c.codename); got != c.want {
			t.Errorf("%s for %s: %v, want %v", c.file, c.codename, got, c.want)
		}
	}
}

// apt's Signed-By conflicts as apt 2.4, 2.6 and 3.2 print them: the panel's
// source of Sury beside an administrator's (the advice of the move and the
// install), the panel's source of the PPA beside add-apt-repository's, and
// no conflict at all.
func TestRealAptSignedByConflict(t *testing.T) {
	for _, c := range []struct {
		file      string
		sury, ppa bool
	}{
		{"update-jammy-conflict.txt", true, false},
		{"update-resolute-conflict.txt", true, false},
		{"update-bookworm-conflict.txt", true, false},
		// apt 2.4/2.6: an unsigned source of Sury read after the panel's.
		{"update-bookworm-conflict-unsigned-after.txt", true, false},
		{"update-jammy-ppa-conflict-inline.txt", false, true},
		{"update-jammy-ppa-conflict-unsigned.txt", false, true},
		{"update-jammy-transient.txt", false, false},
		{"update-jammy-lists-lock.txt", false, false},
		{"update-resolute-lists-lock.txt", false, false},
		{"update-jammy-label-plain.txt", false, false},
		{"update-resolute-fails-elsewhere.txt", false, false},
		{"update-bookworm-fails-elsewhere.txt", false, false},
		// apt translated by LANGUAGE=ru, which the agent's environment
		// empties (TestRealAptLanguageIsEmptied): not read.
		{"update-bookworm-conflict-ru.txt", false, false},
	} {
		err := realAptFailure(realApt(t, c.file))
		if got := surySignedBy(err); got != c.sury {
			t.Errorf("%s: Sury's conflict %v, want %v", c.file, got, c.sury)
		}
		if got := ppaSignedBy(err); got != c.ppa {
			t.Errorf("%s: the PPA's conflict %v, want %v", c.file, got, c.ppa)
		}
	}
	// The agent's message is no output of apt's.
	if surySignedBy(&agent.Error{Message: realApt(t, "update-jammy-conflict.txt")}) {
		t.Error("a conflict read in the agent's message")
	}
	// Two sources of Sury, neither of them the panel's: not the panel's
	// doing, and no advice of its.
	if surySignedBy(realAptFailure("E: Conflicting values set for option Signed-By regarding source https://packages.sury.org/php/ noble: /usr/share/keyrings/deb.sury.org-php.gpg != /etc/apt/trusted.gpg.d/php.gpg\nE: The list of sources could not be read.")) {
		t.Error("an administrator's two sources of Sury taken for the panel's")
	}
	// Two sources of the PPA, neither of them the panel's — one an
	// administrator signed with add-apt-repository's key, and that one's
	// unsigned source; add-apt-repository's key inline, and a source without
	// one —: no advice of the panel's either.
	for _, out := range []string{
		"E: Conflicting values set for option Signed-By regarding source https://ppa.launchpadcontent.net/ondrej/php/ubuntu/ jammy: /etc/apt/trusted.gpg.d/ondrej-ubuntu-php.gpg != \nE: The list of sources could not be read.",
		"E: Conflicting values set for option Signed-By regarding source https://ppa.launchpadcontent.net/ondrej/php/ubuntu/ noble: -----BEGIN PGP PUBLIC KEY BLOCK-----\n   xo0ESX35nAEEALKDCUDVXvmW9n+T\n   -----END PGP PUBLIC KEY BLOCK----- != \nE: The list of sources could not be read.",
	} {
		if ppaSignedBy(realAptFailure(out)) {
			t.Errorf("an administrator's two sources of the PPA taken for the panel's: %q", out)
		}
	}
}

// aptSaidAbout and suryUnread on real transient failures: apt exits 0 and
// names packages.sury.org in its W: line (apt 3.2 words it differently).
func TestRealAptSaidAbout(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"update-jammy-transient.txt", "W: Failed to fetch https://packages.sury.org/php/dists/jammy/InRelease Could not connect to packages.sury.org:443 (127.0.0.2). - connect (111: Connection refused)"},
		{"update-jammy-transient-no-lists.txt", "W: Failed to fetch https://packages.sury.org/php/dists/jammy/InRelease Could not connect to packages.sury.org:443 (127.0.0.2). - connect (111: Connection refused)"},
		{"update-resolute-transient.txt", "W: Failed to fetch https://packages.sury.org/php/dists/resolute/InRelease Unable to connect to packages.sury.org:https:"},
		{"update-bookworm-transient.txt", "W: Failed to fetch https://packages.sury.org/php/dists/bookworm/InRelease Could not connect to packages.sury.org:443 (127.0.0.2). - connect (111: Connection refused)"},
		{"update-jammy-sury.txt", ""},
		// "W: Target … is configured multiple times in …php.list:1 and
		// …sury-php.list:1" names the files, not the host.
		{"update-bookworm-duplicate-unsigned-before.txt", ""},
	} {
		if got := aptSaidAbout(realApt(t, c.file), "packages.sury.org"); got != c.want {
			t.Errorf("%s: %q, want %q", c.file, got, c.want)
		}
	}
	if got, want := suryUnread(&agent.PkgResponse{Output: realApt(t, "update-jammy-transient-no-lists.txt")}, nil), "apt could not read packages.sury.org/php: W: Failed to fetch https://packages.sury.org/php/dists/jammy/InRelease Could not connect to packages.sury.org:443 (127.0.0.2). - connect (111: Connection refused)"; got != want {
		t.Errorf("suryUnread: %q, want %q", got, want)
	}
}

// Before the panel writes its source, a mirror of Sury stands in for it:
// Sury's own builds, "+debsuryorg", from another host. The PPA's builds are
// no Sury's, and a mirror is not packages.sury.org.
func TestSuryOrigins(t *testing.T) {
	withOSRelease(t, ubuntuNoble)
	f := newSiteFixture(t)
	for _, c := range []struct {
		policy, codename string
		want             []string
	}{
		{policyWithSury, "noble", []string{"packages.sury.org"}},
		{policyWithoutSury, "noble", nil},
		{"php-common:\n  Installed: (none)\n  Candidate: 2:102+debsuryorg13.1+3\n  Version table:\n     2:102+debsuryorg13.1+3 500\n        500 http://mirror.internal/sury/php trixie/main amd64 Packages\n     2:93 500\n        500 http://deb.debian.org/debian trixie/main amd64 Packages\n", "trixie", []string{"mirror.internal"}},
		{"php-common:\n  Installed: 2:102+debsuryorg13.1+3\n  Candidate: 2:102+debsuryorg13.1+3\n  Version table:\n *** 2:102+debsuryorg13.1+3 500\n        500 http://mirror.internal:3142/sury/php trixie/main amd64 Packages\n        100 /var/lib/dpkg/status\n", "trixie", []string{"mirror.internal"}},
		{"php-common:\n  Installed: (none)\n  Candidate: 2:102+debsuryorg13.1+3\n  Version table:\n     2:102+debsuryorg13.1+3 500\n        500 http://mirror.internal/sury/php bookworm/main amd64 Packages\n", "trixie", nil},
		{"php-common:\n  Installed: (none)\n  Candidate: 2:102+debsuryorg13.1+3\n  Version table:\n     2:102+debsuryorg13.1+3 500\n        500 https://packages.sury.org/php-qa trixie/main amd64 Packages\n", "trixie", nil},
	} {
		f.agent.PkgOutput = map[string]string{"available": c.policy}
		if got := f.s.suryOrigins(f.ctx, c.codename); !slices.Equal(got, c.want) {
			t.Errorf("%s in %q: %v, want %v", c.codename, c.policy, got, c.want)
		}
		if got := f.s.suryIndexed(f.ctx, c.codename); got != slices.Contains(c.want, "packages.sury.org") {
			t.Errorf("%s in %q: indexed %v", c.codename, c.policy, got)
		}
	}
}

// suryRead on real index updates of apt 2.4, 2.6 and 3.2: one that read
// Sury, ones that failed elsewhere but fetched Sury (the PPA's relabel, a
// repository without a Release file), transient failures of Sury (exit 0,
// "W: Failed to fetch"), and ones that stopped at once on the lists' lock,
// which fetched nothing at all. What the reason then quotes is apt's first
// error, which names the cause.
func TestRealAptSuryRead(t *testing.T) {
	for _, c := range []struct {
		file   string
		failed bool
		want   bool
	}{
		{"update-jammy-sury.txt", false, true},
		{"update-jammy-fails-elsewhere.txt", true, true},
		{"update-resolute-fails-elsewhere.txt", true, true},
		{"update-bookworm-fails-elsewhere.txt", true, true},
		{"update-jammy-label-plain.txt", true, false}, // no Sury on that host
		{"update-jammy-transient.txt", false, false},
		{"update-resolute-transient-no-lists.txt", false, false},
		{"update-jammy-lists-lock.txt", true, false},
		{"update-resolute-lists-lock.txt", true, false},
		{"update-jammy-conflict.txt", true, false},
	} {
		out := realApt(t, c.file)
		var got bool
		if c.failed {
			got = suryRead(nil, realAptFailure(out), "packages.sury.org")
		} else {
			got = suryRead(&agent.PkgResponse{Output: out}, nil, "packages.sury.org")
		}
		if got != c.want {
			t.Errorf("%s: read %v, want %v", c.file, got, c.want)
		}
	}
	if suryRead(&agent.PkgResponse{Output: realApt(t, "update-jammy-sury.txt")}, nil) {
		t.Error("read from no host at all")
	}
	for file, want := range map[string]string{
		"update-resolute-fails-elsewhere.txt": "apt update-index failed: exit status 100 (monopanel-pkg-r2.service): E: The repository 'http://deb.debian.org/debian-r3a-missing resolute Release' does not have a Release file.",
		"update-bookworm-fails-elsewhere.txt": "apt update-index failed: exit status 100 (monopanel-pkg-r2.service): E: The repository 'http://deb.debian.org/debian-r3a-missing bookworm Release' does not have a Release file.",
		"update-resolute-lists-lock.txt":      "apt update-index failed: exit status 100 (monopanel-pkg-r2.service): E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1742 (python3)",
	} {
		if got := aptUnread(nil, realAptFailure(realApt(t, file)), "packages.sury.org"); got != want {
			t.Errorf("%s: %q, want %q", file, got, want)
		}
	}
}
