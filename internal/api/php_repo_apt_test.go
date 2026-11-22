package api

// What apt on the host does with the panel's source of packages.sury.org:
// an index it can no longer refresh, the panel's own source left from
// before, another repository's Signed-By conflict, a mirror of Sury, an
// administrator's source apt has not read yet.

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

// aptUpdate is an index update the host ran before — an administrator's
// apt-get update, apt-daily —: apt reads the sources there are.
func aptUpdate(t *testing.T, f *siteFixture) {
	t.Helper()
	if _, err := f.agent.Client().Pkg(f.ctx, "update-index"); err != nil {
		t.Fatal(err)
	}
}

func realApt(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "realapt", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// realAptFailure is the error the real agent returns for an apt run that
// exited 100 (ops_system.go pkg: Message names the unit, Output is apt's).
func realAptFailure(out string) error {
	return &agent.Error{Message: "apt update-index failed: exit status 100 (monopanel-pkg-r2.service)", Output: out}
}

// expiredSury is apt-get update of a source of Sury whose keyring expired:
// apt keeps the previous index files and warns; the run may end well.
const expiredSury = "Err:3 https://packages.sury.org/php noble InRelease\n  The following signatures were invalid: EXPKEYSIG B188E2B695BD4743 DEB.SURY.ORG Automatic Signing Key <deb@sury.org>\n" +
	"Reading package lists...\n" +
	"W: An error occurred during the signature verification. The repository is not updated and the previous index files will be used. GPG error: https://packages.sury.org/php noble InRelease: The following signatures were invalid: EXPKEYSIG B188E2B695BD4743 DEB.SURY.ORG Automatic Signing Key <deb@sury.org>\n" +
	"W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease  The following signatures were invalid: EXPKEYSIG B188E2B695BD4743 DEB.SURY.ORG Automatic Signing Key <deb@sury.org>\n" +
	"W: Some index files failed to download. They have been ignored, or old ones used instead.\n"

// An index of Sury that apt has from a source of the host's own may be one
// it can no longer refresh: the PPA stays until apt reads Sury anew, and the
// doctor says why; then the next start moves the host.
func TestSwitchPHPRepoKeepsThePPAWhileSuryIsStale(t *testing.T) {
	for _, c := range []struct {
		name    string
		exit100 bool
	}{{"apt ends well", false}, {"apt fails", true}} {
		t.Run(c.name, func(t *testing.T) {
			withOSRelease(t, ubuntuNoble)
			stubSury(t, true)
			f := newSiteFixture(t)
			seedPPA(t, f)
			seedFiles(t, f, map[string]string{adminSury: adminSuryList("noble")})
			aptUpdate(t, f) // when the administrator's keyring was good
			if c.exit100 {
				f.agent.PkgFails = map[string]string{"update-index": expiredSury}
			} else {
				f.agent.PkgOutput = map[string]string{"update-index": expiredSury}
			}
			from := len(f.agent.Calls())
			f.s.switchPHPRepoToSury(f.ctx)
			if got := repoCalls(f, from); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "remove "+ondrejSource) }) {
				t.Fatalf("the PPA went while apt could not refresh Sury: %v", got)
			}
			if src, _ := f.agent.File(ondrejSource); src != ppaSource {
				t.Fatalf("the PPA's source: %q", src)
			}
			if src, _ := f.agent.File(adminSury); src != adminSuryList("noble") {
				t.Fatalf("the administrator's source: %q", src)
			}
			if _, ok := f.agent.File(surySource); ok {
				t.Fatal("the panel wrote its source of Sury beside the administrator's")
			}
			why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch)
			if !strings.Contains(why, "EXPKEYSIG") {
				t.Fatalf("reason: %q", why)
			}
			if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, "EXPKEYSIG") {
				t.Fatalf("doctor: %+v", line)
			}
			// The keyring renewed, apt reads Sury again: the next start moves.
			f.agent.PkgFails, f.agent.PkgOutput = nil, nil
			f.s.switchPHPRepoToSury(f.ctx)
			if _, ok := f.agent.File(ondrejSource); ok {
				t.Fatal("the PPA stays once apt reads Sury")
			}
			if line := repoLine(f); line != nil {
				t.Fatalf("doctor after the move: %+v", line)
			}
		})
	}
}

// A PHP install that keeps the host's own source of Sury reads the index
// anew first: what apt has may be as old as the administrator's last apt-get
// update, and Sury drops superseded builds from its pool. A refresh that
// fails fails no install: apt still has the index.
func TestPHPRepoRefreshesTheHostsOwnSury(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
	stubSury(t, true)
	f := newSiteFixture(t)
	seedFiles(t, f, map[string]string{adminSury: adminSuryList("trixie")})
	aptUpdate(t, f)
	f.agent.PkgFails = map[string]string{"update-index": "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4242 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/"}
	from := len(f.agent.Calls())
	job := phpInstallJob(t, f, "8.3")
	if job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	calls := repoCalls(f, from)
	update := slices.Index(calls, "pkg update-index")
	install := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "pkg install ") })
	if update < 0 || install < 0 || update > install {
		t.Fatalf("no index update before the install:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(job.Log, "Could not get lock /var/lib/apt/lists/lock") {
		t.Fatalf("the failed refresh is not in the job's log:\n%s", job.Log)
	}
	if files, _ := applied(f, from); slices.ContainsFunc(files, func(file agent.FileSpec) bool { return strings.HasPrefix(file.Path, "/etc/apt/") }) {
		t.Fatalf("the panel wrote apt files beside the administrator's source: %+v", files)
	}
}

// The panel's own sury-php.list left from before — the panel installed
// again, its database new — is no source of the host's own: the install
// writes it again, with its keyring, which may be gone or expired though apt
// still lists Sury from the index it read earlier.
func TestPHPRepoRewritesThePanelsOwnSource(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
	stubSury(t, true)
	f := newSiteFixture(t)
	seedFiles(t, f, map[string]string{surySource: "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ trixie main\n"})
	aptUpdate(t, f)
	from := len(f.agent.Calls())
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if calls := repoCalls(f, from); !slices.Contains(calls, "apply "+suryKeyring+" "+surySource) {
		t.Fatalf("the panel's own source was not written again:\n%s", strings.Join(calls, "\n"))
	}
	if key, _ := f.agent.File(suryKeyring); key != "sury-key" {
		t.Fatalf("Sury's keyring: %q", key)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingPHPRepo+".debian"); v != "ready" {
		t.Fatalf("repository state: %q", v)
	}
}

// A move an API restart cut short left the panel's own sury-php.list beside
// the PPA, and apt has read it: the next start writes it again, keyring and
// all, and the journal names it as what the host moved to.
func TestSwitchPHPRepoRewritesThePanelsOwnSource(t *testing.T) {
	withOSRelease(t, ubuntuNoble)
	stubSury(t, true)
	f := newSiteFixture(t)
	seedPPA(t, f)
	seedFiles(t, f, map[string]string{surySource: "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ noble main\n"})
	aptUpdate(t, f)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	from := len(f.agent.Calls())
	f.s.switchPHPRepoToSury(f.ctx)
	// No look for a source of the host's own: the panel's is there.
	want := []string{
		"apply " + suryKeyring + " " + surySource,
		"pkg update-index",
		"pkg available php-common",
		"remove " + ondrejSource + " " + ondrejKeyring,
		"pkg update-index",
	}
	if got := repoCalls(f, from); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if key, _ := f.agent.File(suryKeyring); key != "sury-key" {
		t.Fatalf("Sury's keyring: %q", key)
	}
	if l := logs.String(); !strings.Contains(l, "PHP repository: moved from ppa:ondrej/php") || !strings.Contains(l, "source="+surySource) {
		t.Fatalf("the journal:\n%s", l)
	}
}

// A Signed-By conflict that apt names for another repository is not
// packages.sury.org's: the reason quotes apt's own line about it and gives
// the advice for that repository only — none for nginx.org, the PPA's for
// two sources of the PPA (the panel's and an administrator's
// add-apt-repository).
func TestSwitchPHPRepoNamesTheConflictingSource(t *testing.T) {
	const suryAdvice = "another source of packages.sury.org names another keyring"
	t.Run("nginx.org", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		const conflict = "E: Conflicting values set for option Signed-By regarding source https://nginx.org/packages/ubuntu/ noble: /usr/share/keyrings/nginx-archive-keyring.gpg != /etc/apt/keyrings/nginx.asc\nE: The list of sources could not be read."
		f.agent.PkgFails = map[string]string{"update-index": conflict}
		f.agent.PkgOutput = map[string]string{"available": conflict + "\n"}
		f.s.switchPHPRepoToSury(f.ctx)
		why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch)
		if strings.Contains(why, suryAdvice) || !strings.Contains(why, "regarding source https://nginx.org/packages/ubuntu/ noble") {
			t.Fatalf("reason: %q", why)
		}
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
	})
	t.Run("two sources of the PPA", func(t *testing.T) {
		withOSRelease(t, "ID=ubuntu\nVERSION_ID=22.04\nVERSION_CODENAME=jammy\n")
		stubSury(t, true)
		f := newSiteFixture(t)
		const admin = "/etc/apt/sources.list.d/ondrej-ubuntu-php-jammy.sources"
		seedFiles(t, f, map[string]string{
			ondrejSource:  "deb [signed-by=/etc/apt/keyrings/ondrej-php.asc] https://ppa.launchpadcontent.net/ondrej/php/ubuntu jammy main\n",
			ondrejKeyring: ppaKey,
			admin:         "Types: deb\nURIs: https://ppa.launchpadcontent.net/ondrej/php/ubuntu/\nSuites: jammy\nComponents: main\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n xo0E\n -----END PGP PUBLIC KEY BLOCK-----\n",
		})
		f.agent.PkgFails = map[string]string{"update-index": realApt(t, "update-jammy-ppa-conflict-inline.txt")}
		f.agent.PkgOutput = map[string]string{"available": realApt(t, "policy-jammy-ppa-conflict-inline.txt")}
		f.s.switchPHPRepoToSury(f.ctx)
		why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch)
		if strings.Contains(why, suryAdvice) || !strings.Contains(why, "regarding source https://ppa.launchpadcontent.net/ondrej/php/ubuntu/ jammy") || !strings.Contains(why, "another source of ppa:ondrej/php") {
			t.Fatalf("reason: %q", why)
		}
		for _, p := range []string{ondrejSource, ondrejKeyring, admin} {
			if _, ok := f.agent.File(p); !ok {
				t.Errorf("%s went", p)
			}
		}
	})
	t.Run("the real agent's error", func(t *testing.T) {
		// The agent's message names the run, apt's text is the output: the
		// conflict line comes first, the key's lines and "E: The list of
		// sources could not be read." after it.
		why := suryUnread(nil, realAptFailure(realApt(t, "update-jammy-ppa-conflict-inline.txt")))
		if !strings.Contains(why, "regarding source https://ppa.launchpadcontent.net/ondrej/php/ubuntu/ jammy: /etc/apt/keyrings/ondrej-php.asc") || !strings.Contains(why, "apt update-index failed: exit status 100") || strings.Contains(why, "END PGP") {
			t.Fatalf("reason: %q", why)
		}
	})
}

// A host that takes Sury from a mirror of its own, packages.sury.org out of
// apt's reach: apt has Sury's builds already (their versions say
// "+debsuryorg"), and the panel writes no source of its own.
func TestPHPRepoTakesSuryFromTheHostsMirror(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
	stubSury(t, true)
	f := newSiteFixture(t)
	const mirror = "/etc/apt/sources.list.d/php-mirror.list"
	seedFiles(t, f, map[string]string{mirror: "deb [signed-by=/usr/share/keyrings/deb.sury.org-php.gpg] http://mirror.internal/sury/php/ trixie main\n"})
	f.agent.PkgOutput = map[string]string{
		"update-index": "Hit:1 http://mirror.internal/sury/php trixie InRelease\nErr:2 https://packages.sury.org/php trixie InRelease\n  Could not connect to packages.sury.org:443 (203.0.113.5), connection timed out\nReading package lists...\n" +
			"W: Failed to fetch https://packages.sury.org/php/dists/trixie/InRelease  Could not connect to packages.sury.org:443 (203.0.113.5), connection timed out\n" +
			"W: Some index files failed to download. They have been ignored, or old ones used instead.\n",
		"available": "php-common:\n  Installed: (none)\n  Candidate: 2:102+debsuryorg13.1+3\n  Version table:\n     2:102+debsuryorg13.1+3 500\n        500 http://mirror.internal/sury/php trixie/main amd64 Packages\n",
	}
	from := len(f.agent.Calls())
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
		t.Fatalf("PHP install with Sury's builds from the host's mirror: %s %q", job.Status, job.Error)
	}
	if _, ok := f.agent.File(surySource); ok {
		t.Fatal("the panel wrote its source of packages.sury.org beside the mirror")
	}
	if src, _ := f.agent.File(mirror); src == "" || slices.Contains(f.agent.Removed(), mirror) {
		t.Fatalf("the mirror's source: %q (removed: %v)", src, f.agent.Removed())
	}
	if files, _ := applied(f, from); slices.ContainsFunc(files, func(file agent.FileSpec) bool { return strings.HasPrefix(file.Path, "/etc/apt/") }) {
		t.Fatalf("the panel wrote apt files beside the mirror: %+v", files)
	}
}

// A PHP install beside an administrator's source of Sury that apt has not
// read yet: apt refuses the panel's source beside it, the install fails, the
// panel's source goes again, and the error says what to do. Once apt has
// read the administrator's source, the next install keeps it.
func TestPHPRepoAnAdministratorsSuryAptHasNotRead(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
	stubSury(t, true)
	f := newSiteFixture(t)
	seedFiles(t, f, map[string]string{adminSury: adminSuryList("trixie")})
	job := phpInstallJob(t, f, "8.3")
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "Conflicting values set for option Signed-By") || !strings.Contains(job.Error, "another source of packages.sury.org names another keyring") || !strings.Contains(job.Error, "apt-get update") {
		t.Fatalf("install beside a source apt has not read: %s %q", job.Status, job.Error)
	}
	if _, ok := f.agent.File(surySource); ok {
		t.Fatal("the panel's source stayed beside the administrator's")
	}
	if src, _ := f.agent.File(adminSury); src != adminSuryList("trixie") {
		t.Fatalf("the administrator's source: %q", src)
	}
	aptUpdate(t, f)
	from := len(f.agent.Calls())
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
		t.Fatalf("install once apt read the administrator's source: %s %s", job.Status, job.Error)
	}
	if files, _ := applied(f, from); slices.ContainsFunc(files, func(file agent.FileSpec) bool { return strings.HasPrefix(file.Path, "/etc/apt/") }) {
		t.Fatalf("the panel wrote apt files beside the administrator's source: %+v", files)
	}
}

// What apt says against a repository is a fetch that failed, a signature it
// refused, an error; a warning that starts with the URL of a file is a notice
// on a file apt read all the same — a key in the legacy keyring of an
// administrator's apt-key add, a weak signature — and no reason to keep the
// PPA or to call Sury unread.
func TestAptSaidAboutSkipsNotices(t *testing.T) {
	const notices = "Hit:3 https://packages.sury.org/php noble InRelease\n" +
		"Reading package lists...\n" +
		"W: https://packages.sury.org/php/dists/noble/InRelease: Key is stored in legacy trusted.gpg keyring (/etc/apt/trusted.gpg), see the DEPRECATION section in apt-key(8) for details.\n" +
		"W: http://mirror.internal/sury/php/dists/noble/InRelease: Signature by key 15058500A0235D97F5D10063B188E2B695BD4743 uses weak algorithm (rsa1024)\n"
	for _, host := range []string{"packages.sury.org", "mirror.internal"} {
		if got := aptSaidAbout(notices, host); got != "" {
			t.Errorf("%s: a notice counts against it: %q", host, got)
		}
	}
	if got := aptSaidAbout(notices+"E: The repository 'https://packages.sury.org/php noble InRelease' is not signed.\n", "packages.sury.org"); !strings.HasPrefix(got, "E: The repository") {
		t.Errorf("an error after the notices: %q", got)
	}
}

// LC_ALL=C.UTF-8 does not override LANGUAGE (gettext ignores LANGUAGE only
// for the plain C locale): with LANGUAGE in the systemd manager's environment
// (LANGUAGE=ru_RU:ru in /etc/default/locale on Debian 12 reached a transient
// unit started with LANG=C.UTF-8 LC_ALL=C.UTF-8) apt speaks Russian, and
// neither the conflict nor apt-cache policy's "Candidate:" is read
// (TestRealAptSignedByConflict has the Russian conflict). The agent's
// environment for apt empties LANGUAGE.
func TestRealAptLanguageIsEmptied(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=12\nVERSION_CODENAME=bookworm\n")
	f := newSiteFixture(t)
	pm := f.s.profile.Packages()
	if !slices.Contains(pm.Env(), "LANGUAGE=") {
		t.Errorf("apt's environment %v leaves LANGUAGE as the host has it", pm.Env())
	}
	if got := pm.ParseAvailable(realApt(t, "policy-bookworm-sury.txt")); got["php-common"] != "2:102+debsuryorg12.1+3" {
		t.Errorf("ParseAvailable of apt-cache policy in English: %v", got)
	}
}
