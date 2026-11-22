package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

const (
	ubuntuNoble = "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n"
	// ppaSource and ppaKey are ppa:ondrej/php as panel 0.8.19 set it up.
	ppaSource = "deb [signed-by=/etc/apt/keyrings/ondrej-php.asc] https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble main\n"
	ppaKey    = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nondrej 2012\n-----END PGP PUBLIC KEY BLOCK-----\n"
	// adminPPA is what add-apt-repository ppa:ondrej/php makes on noble:
	// the administrator's, not the panel's.
	adminPPA = "/etc/apt/sources.list.d/ondrej-ubuntu-php-noble.sources"
	// adminSury is where Sury's README has an administrator put its source,
	// signed by the keyring its debsuryorg-archive-keyring package installs.
	adminSury = "/etc/apt/sources.list.d/php.list"
	// suryConflict is what apt says of the panel's source of Sury beside an
	// administrator's, which it reads first (php.list): it reads no source
	// at all then. The fake agent says the same (aptConflict).
	suryConflict = "E: Conflicting values set for option Signed-By regarding source https://packages.sury.org/php/ noble: /usr/share/keyrings/deb.sury.org-php.gpg != /etc/apt/keyrings/sury-php.gpg"
)

// adminSuryList is the administrator's own source of Sury for a codename.
func adminSuryList(codename string) string {
	return "deb [signed-by=/usr/share/keyrings/deb.sury.org-php.gpg] https://packages.sury.org/php/ " + codename + " main\n"
}

// policyWithSury and policyWithoutSury are what apt-cache policy php-common
// prints on noble after an index update that read packages.sury.org, and
// after one that did not.
const (
	policyWithSury = `php-common:
  Installed: 2:101~+ubuntu24.04.1+deb.sury.org+1
  Candidate: 2:102+debsuryorg24.04.1+3
  Version table:
     2:102+debsuryorg24.04.1+3 500
        500 https://packages.sury.org/php noble/main amd64 Packages
 *** 2:101~+ubuntu24.04.1+deb.sury.org+1 500
        500 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble/main amd64 Packages
        100 /var/lib/dpkg/status
     2:93ubuntu2 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
`
	policyWithoutSury = `php-common:
  Installed: 2:101~+ubuntu24.04.1+deb.sury.org+1
  Candidate: 2:101~+ubuntu24.04.1+deb.sury.org+1
  Version table:
 *** 2:101~+ubuntu24.04.1+deb.sury.org+1 500
        500 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble/main amd64 Packages
        100 /var/lib/dpkg/status
     2:93ubuntu2 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
`
)

// seedFiles puts files on the fake host as an older panel or an
// administrator left them; the fake takes them without the agent's checks.
func seedFiles(t *testing.T, f *siteFixture, files map[string]string) {
	t.Helper()
	f.agent.StatWritten = true
	var specs []agent.FileSpec
	for p, c := range files {
		specs = append(specs, agent.FileSpec{Path: p, Content: c, Mode: 0o644})
	}
	if _, err := f.agent.Client().ApplyConfigSet(f.ctx, &agent.ApplyConfigSetRequest{Files: specs, Origin: "test"}); err != nil {
		t.Fatal(err)
	}
}

// seedPPA is the panel's ppa:ondrej/php of 0.8.19.
func seedPPA(t *testing.T, f *siteFixture) {
	t.Helper()
	seedFiles(t, f, map[string]string{ondrejSource: ppaSource, ondrejKeyring: ppaKey})
}

// repoCalls are the agent calls since the one at index from, in order, as
// "apply <paths>", "pkg <action> <packages>" and "remove <paths>"; a stat
// changes nothing and is left out.
func repoCalls(f *siteFixture, from int) []string {
	var out []string
	for _, c := range f.agent.Calls()[from:] {
		switch c.Path {
		case "/v1/stat":
		case "/v1/config/apply":
			var req agent.ApplyConfigSetRequest
			_ = json.Unmarshal(c.Body, &req)
			var paths []string
			for _, file := range req.Files {
				paths = append(paths, file.Path)
			}
			out = append(out, "apply "+strings.Join(paths, " "))
		case "/v1/pkg":
			var req agent.PkgRequest
			_ = json.Unmarshal(c.Body, &req)
			out = append(out, strings.TrimSpace("pkg "+req.Action+" "+strings.Join(req.Packages, " ")))
		case "/v1/paths/remove":
			var req agent.RemovePathsRequest
			_ = json.Unmarshal(c.Body, &req)
			out = append(out, "remove "+strings.Join(req.Paths, " "))
		default:
			out = append(out, c.Path)
		}
	}
	return out
}

// applied are the files the API had the agent write since the call at
// index from, with the origin of their set.
func applied(f *siteFixture, from int) (files []agent.FileSpec, origins []string) {
	for _, c := range f.agent.Calls()[from:] {
		if c.Path != "/v1/config/apply" {
			continue
		}
		var req agent.ApplyConfigSetRequest
		_ = json.Unmarshal(c.Body, &req)
		files = append(files, req.Files...)
		origins = append(origins, req.Origin)
	}
	return files, origins
}

// ppaWrites names what of ppa:ondrej/php the API had the agent write since
// the call at index from: a path of the PPA's, or a source naming it.
func ppaWrites(f *siteFixture, from int) []string {
	var out []string
	files, _ := applied(f, from)
	for _, file := range files {
		if strings.Contains(file.Path, "ondrej") || strings.Contains(file.Content, "launchpad") {
			out = append(out, file.Path)
		}
	}
	return out
}

// aptWritesPass checks the apt files the API had the agent write since the
// call at index from against the real agent's rules: the fake takes any.
func aptWritesPass(t *testing.T, f *siteFixture, from int) {
	t.Helper()
	files, _ := applied(f, from)
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "/etc/apt/") {
			continue
		}
		content := []byte(file.Content)
		if file.ContentBase64 != "" {
			content, _ = base64.StdEncoding.DecodeString(file.ContentBase64)
		}
		if err := confcheck.Check(file.Path, content); err != nil {
			t.Errorf("the agent refuses %s: %v", file.Path, err)
		}
	}
}

// repoLine is the doctor's "php repository" line, nil when it has none.
func repoLine(f *siteFixture) *apitypes.Check {
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	for _, c := range doc.Checks {
		if c.Name == "php repository" {
			return &c
		}
	}
	return nil
}

// countSuryProbes counts the asks whether packages.sury.org publishes a
// release (after stubSury, whose answer they get).
func countSuryProbes(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := suryHasRelease
	suryHasRelease = func(ctx context.Context, codename string) (bool, error) { n.Add(1); return prev(ctx, codename) }
	t.Cleanup(func() { suryHasRelease = prev })
	return &n
}

// phpInstallJob installs a branch through the API and waits for the job.
func phpInstallJob(t *testing.T, f *siteFixture, version string) *store.Job {
	t.Helper()
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": version}, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

// A new install on Ubuntu takes packages.sury.org, as on Debian, signed by
// the key the panel pins for it; the panel writes ppa:ondrej/php no more.
func TestPHPRepoUbuntuTakesSury(t *testing.T) {
	withOSRelease(t, ubuntuNoble)
	stubSury(t, true)
	probes := countSuryProbes(t)
	f := newSiteFixture(t)
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if probes.Load() == 0 {
		t.Error("Ubuntu took Sury without asking whether it publishes noble")
	}
	if src, _ := f.agent.File(surySource); src != "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ noble main\n" {
		t.Fatalf("Sury's source: %q", src)
	}
	if key, _ := f.agent.File(suryKeyring); key != "sury-key" {
		t.Fatalf("Sury's keyring: %q", key)
	}
	if w := ppaWrites(f, 0); len(w) > 0 {
		t.Fatalf("ppa:ondrej/php was written: %v", w)
	}
	aptWritesPass(t, f, 0)
	if _, origins := applied(f, 0); !slices.Contains(origins, "php:repo") {
		t.Fatalf("origins of the sets: %v", origins)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingPHPRepo+".debian"); v != "ready" {
		t.Fatalf("repository state: %q", v)
	}
}

// Debian takes Sury as it always did: no probe, the source with its codename.
func TestPHPRepoDebianUnchanged(t *testing.T) {
	withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
	stubSury(t, false) // whatever the probe would say, Debian does not ask
	probes := countSuryProbes(t)
	f := newSiteFixture(t)
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if n := probes.Load(); n != 0 {
		t.Errorf("Debian asked packages.sury.org %d times", n)
	}
	if src, _ := f.agent.File(surySource); src != "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ trixie main\n" {
		t.Fatalf("Sury's source: %q", src)
	}
	aptWritesPass(t, f, 0)
	if w := ppaWrites(f, 0); len(w) > 0 {
		t.Fatalf("ppa:ondrej/php was written: %v", w)
	}
}

// A repository apt cannot read must not stay behind: every later index
// update on the host would fail with it.
func TestPHPRepoSourceGoesWhenAptCannotReadIt(t *testing.T) {
	withOSRelease(t, "ID=ubuntu\nVERSION_ID=26.04\nVERSION_CODENAME=resolute\n")
	stubSury(t, true)
	f := newSiteFixture(t)
	f.agent.Fail["/v1/pkg"] = "E: The repository 'https://packages.sury.org/php resolute InRelease' is not signed."
	if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobFailed || !strings.Contains(job.Error, "is not signed") {
		t.Fatalf("the install must fail with apt's word: %s %s", job.Status, job.Error)
	}
	if removed := f.agent.Removed(); !slices.Contains(removed, surySource) {
		t.Fatalf("the unreadable source must go again (removed: %v)", removed)
	}
}

// An index update that ends well proves nothing about Sury: apt takes a name
// that did not resolve or a timeout for passing trouble, warns and leaves the
// source out (exit 0). Without Sury's index the repository is not ready: the
// install fails with apt's warning, the source goes again, and the next
// install writes it once more.
func TestPHPRepoNeedsSurysIndex(t *testing.T) {
	for _, c := range []struct{ name, release, codename, update, want string }{
		{"debian", "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n", "trixie", "", "apt could not read packages.sury.org/php: W: Failed to fetch https://packages.sury.org/php/dists/trixie/InRelease Temporary failure resolving"},
		{"ubuntu", ubuntuNoble, "noble", "", "apt could not read packages.sury.org/php: W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease Temporary failure resolving"},
		// Not a word of Sury in apt's output, and no index of it either.
		{"silent", ubuntuNoble, "noble", "Hit:1 http://deb.example.org/os noble InRelease\nReading package lists...\n", "apt could not read packages.sury.org/php: the index update ended without it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withOSRelease(t, c.release)
			stubSury(t, true)
			f := newSiteFixture(t)
			update := c.update
			if update == "" {
				update = "Err:3 https://packages.sury.org/php " + c.codename + " InRelease\n  Temporary failure resolving 'packages.sury.org'\n" +
					"Reading package lists...\n" +
					"W: Failed to fetch https://packages.sury.org/php/dists/" + c.codename + "/InRelease  Temporary failure resolving 'packages.sury.org'\n" +
					"W: Some index files failed to download. They have been ignored, or old ones used instead.\n"
			}
			f.agent.PkgOutput = map[string]string{"update-index": update}
			if c.update != "" {
				// The fake reads a source apt said nothing against; here apt
				// has none of Sury all the same.
				f.agent.PkgOutput["available"] = "php-common:\n  Installed: (none)\n  Candidate: 2:93\n  Version table:\n     2:93 500\n        500 http://deb.example.org/os " + c.codename + "/main amd64 Packages\n"
			}
			job := phpInstallJob(t, f, "8.3")
			if job.Status != store.JobFailed || !strings.Contains(job.Error, c.want) {
				t.Fatalf("install without Sury's index: %s %q", job.Status, job.Error)
			}
			if v, _ := f.db.GetSetting(f.ctx, settingPHPRepo+".debian"); v == "ready" {
				t.Fatal("the repository is marked ready without Sury's index")
			}
			if !slices.Contains(f.agent.Removed(), surySource) {
				t.Fatalf("the source apt left out stays (removed: %v)", f.agent.Removed())
			}
			// The next install, apt reaching Sury, writes it again.
			f.agent.PkgOutput = nil
			if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
				t.Fatalf("install with Sury readable: %s %s", job.Status, job.Error)
			}
			if src, _ := f.agent.File(surySource); src != "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ "+c.codename+" main\n" {
				t.Fatalf("Sury's source: %q", src)
			}
			if v, _ := f.db.GetSetting(f.ctx, settingPHPRepo+".debian"); v != "ready" {
				t.Fatalf("repository state: %q", v)
			}
		})
	}
}

// An administrator who set Sury up himself, as its README says, keeps his
// source once apt has read it: the panel's source beside it, signed by
// another keyring, would make apt refuse both and every other source.
func TestPHPRepoKeepsAnAdministratorsSury(t *testing.T) {
	for _, c := range []struct{ name, release, codename string }{
		{"debian", "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n", "trixie"},
		{"ubuntu", ubuntuNoble, "noble"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withOSRelease(t, c.release)
			stubSury(t, true)
			f := newSiteFixture(t)
			seedFiles(t, f, map[string]string{adminSury: adminSuryList(c.codename)})
			aptUpdate(t, f)
			from := len(f.agent.Calls())
			if job := phpInstallJob(t, f, "8.3"); job.Status != store.JobDone {
				t.Fatalf("install: %s %s", job.Status, job.Error)
			}
			if files, _ := applied(f, from); slices.ContainsFunc(files, func(file agent.FileSpec) bool { return strings.HasPrefix(file.Path, "/etc/apt/") }) {
				t.Fatalf("the panel wrote apt files beside the administrator's source: %+v", files)
			}
			if _, ok := f.agent.File(surySource); ok {
				t.Fatal("the panel's source of Sury was written beside the administrator's")
			}
			if src, _ := f.agent.File(adminSury); src != adminSuryList(c.codename) || slices.Contains(f.agent.Removed(), adminSury) {
				t.Fatalf("the administrator's source: %q (removed: %v)", src, f.agent.Removed())
			}
			if v, _ := f.db.GetSetting(f.ctx, settingPHPRepo+".debian"); v != "ready" {
				t.Fatalf("repository state: %q", v)
			}
		})
	}
}

// An Ubuntu that took ppa:ondrej/php under panel 0.8.19 moves to
// packages.sury.org at the start of the API, once: Sury's files go in beside
// the PPA's, the PPA goes once apt has read Sury, nothing is upgraded. A move
// that fails keeps the PPA and tries again at the next start, and the doctor
// says why meanwhile.
func TestSwitchPHPRepoToSury(t *testing.T) {
	// apt-cache policy is asked before the write as well: an administrator's
	// own source of Sury makes the panel's needless, and apt refuses the two
	// together. The fake's answer names the sources an index update read
	// (aptPolicy), so it has Sury after the write and the update, not before.
	moved := []string{
		"pkg available php-common",
		"apply " + suryKeyring + " " + surySource,
		"pkg update-index",
		"pkg available php-common",
		"remove " + ondrejSource + " " + ondrejKeyring,
		"pkg update-index",
	}
	unread := []string{
		"pkg available php-common",
		"apply " + suryKeyring + " " + surySource,
		"pkg update-index",
		"pkg available php-common",
		"remove " + surySource + " " + suryKeyring,
	}
	// The host's own source of Sury, which apt has read: read anew, the PPA
	// still there, and then the PPA alone goes.
	hosts := []string{
		"pkg available php-common",
		"pkg update-index",
		"pkg available php-common",
		"remove " + ondrejSource + " " + ondrejKeyring,
		"pkg update-index",
	}
	// The advice for a Signed-By conflict is the conflict's alone.
	const suryAdvice = "another source of packages.sury.org names another keyring"
	t.Run("moves", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, "has not got to it yet") {
			t.Fatalf("doctor before the start: %+v", line)
		}
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, moved) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(moved, "\n"))
		}
		if src, _ := f.agent.File(surySource); src != "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ noble main\n" {
			t.Fatalf("Sury's source: %q", src)
		}
		for _, p := range []string{ondrejSource, ondrejKeyring} {
			if _, ok := f.agent.File(p); ok {
				t.Errorf("%s is still there", p)
			}
		}
		if _, origins := applied(f, from); !slices.Equal(origins, []string{"php:repo"}) {
			t.Fatalf("origins: %v", origins)
		}
		aptWritesPass(t, f, from)
		if w := ppaWrites(f, from); len(w) > 0 {
			t.Fatalf("ppa:ondrej/php was written: %v", w)
		}
		if _, err := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a reason stays recorded: %v", err)
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor after the move: %+v", line)
		}
		// Done once: the next start only looks.
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 {
			t.Fatalf("a second start: %v", got)
		}
	})
	t.Run("apt cannot read Sury", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		f.agent.PkgFails = map[string]string{"update-index": "E: The repository 'https://packages.sury.org/php noble InRelease' is not signed."}
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, unread) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(unread, "\n"))
		}
		// The PPA was never taken away: the host has the repository it had.
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
		if key, _ := f.agent.File(ondrejKeyring); key != ppaKey {
			t.Fatalf("the PPA's keyring: %q", key)
		}
		if _, ok := f.agent.File(surySource); ok {
			t.Fatal("Sury's source stayed")
		}
		if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "apt could not read packages.sury.org/php: apt update-index failed: exit status 100") || !strings.Contains(why, "is not signed") || strings.Contains(why, suryAdvice) {
			t.Fatalf("reason: %q", why)
		}
		line := repoLine(f)
		if line == nil || line.Status != "warn" || !strings.Contains(line.Detail, "apt could not read packages.sury.org/php") || !strings.Contains(line.Detail, "systemctl restart monopanel-api") || !strings.Contains(line.Detail, ondrejSource) {
			t.Fatalf("doctor: %+v", line)
		}
		// The next start, with Sury readable, moves the host, and the reason
		// goes with the PPA.
		f.agent.PkgFails = nil
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, moved) {
			t.Fatalf("calls at the next start:\n%s", strings.Join(got, "\n"))
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor after the move: %+v", line)
		}
		if why, err := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the reason of the failed start stays recorded: %q %v", why, err)
		}
	})
	t.Run("the index update ends well without Sury", func(t *testing.T) {
		// apt-get update exits 0 when a source failed for what apt takes for
		// a passing network trouble: a warning, the source left out. The
		// exit code proves nothing then; without Sury's index the PPA stays.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		f.agent.PkgOutput = map[string]string{
			"update-index": "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\n" +
				"Err:2 https://packages.sury.org/php noble InRelease\n  Temporary failure resolving 'packages.sury.org'\n" +
				"Reading package lists...\n" +
				"W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease  Temporary failure resolving 'packages.sury.org'\n" +
				"W: Some index files failed to download. They have been ignored, or old ones used instead.\n",
		}
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, unread) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(unread, "\n"))
		}
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
		if key, _ := f.agent.File(ondrejKeyring); key != ppaKey {
			t.Fatalf("the PPA's keyring: %q", key)
		}
		if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "apt could not read packages.sury.org/php: W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease Temporary failure resolving") || strings.Contains(why, suryAdvice) {
			t.Fatalf("reason: %q", why)
		}
	})
	t.Run("an index of Sury apt did not refresh", func(t *testing.T) {
		// Lists of Sury from an earlier fetch, a move an API restart cut
		// short: apt lists Sury, but this update could not fetch it. The
		// PPA stays until an update reads Sury.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		seedFiles(t, f, map[string]string{surySource: "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ noble main\n"})
		aptUpdate(t, f)
		f.agent.PkgOutput = map[string]string{"update-index": "Err:2 https://packages.sury.org/php noble InRelease\n  Could not connect to packages.sury.org:443 (203.0.113.5), connection timed out\n" +
			"Reading package lists...\n" +
			"W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease  Could not connect to packages.sury.org:443 (203.0.113.5), connection timed out\n" +
			"W: Some index files failed to download. They have been ignored, or old ones used instead.\n"}
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		// The panel's own source is written again, no look for another.
		if got := repoCalls(f, from); !slices.Equal(got, unread[1:]) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(unread[1:], "\n"))
		}
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
		if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "apt could not read packages.sury.org/php: W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease Could not connect") {
			t.Fatalf("reason: %q", why)
		}
	})
	t.Run("the index update cannot run", func(t *testing.T) {
		// apt-get update stops at once on the lock of the lists another one
		// holds (the agent waits ten minutes for it first): apt fetched
		// nothing, and lists of Sury from before do not make it read.
		const locked = "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4242 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/"
		for _, c := range []struct {
			name, source, content string
			calls                 []string
		}{
			{"the host's own source", adminSury, adminSuryList("noble"), hosts[:3]},
			{"the panel's own source", surySource, "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://packages.sury.org/php/ noble main\n", unread[1:]},
		} {
			withOSRelease(t, ubuntuNoble)
			stubSury(t, true)
			f := newSiteFixture(t)
			seedPPA(t, f)
			seedFiles(t, f, map[string]string{c.source: c.content})
			aptUpdate(t, f)
			f.agent.PkgFails = map[string]string{"update-index": locked}
			from := len(f.agent.Calls())
			f.s.switchPHPRepoToSury(f.ctx)
			if got := repoCalls(f, from); !slices.Equal(got, c.calls) {
				t.Fatalf("%s: calls:\n%s\nwant:\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.calls, "\n"))
			}
			if src, _ := f.agent.File(ondrejSource); src != ppaSource {
				t.Fatalf("%s: the PPA's source: %q", c.name, src)
			}
			if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "apt update-index failed: exit status 100 (monopanel-pkg-test.service): E: Could not get lock /var/lib/apt/lists/lock") {
				t.Fatalf("%s: reason: %q", c.name, why)
			}
		}
	})
	t.Run("the PPA's files cannot be removed", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		f.agent.Fail["/v1/paths/remove"] = "remove /etc/apt/sources.list.d/ondrej-php.list: read-only file system"
		f.s.switchPHPRepoToSury(f.ctx)
		for _, p := range []string{ondrejSource, ondrejKeyring} {
			if _, ok := f.agent.File(p); !ok {
				t.Errorf("%s went", p)
			}
		}
		const why = "packages.sury.org/php is in place, but the source of the PPA could not be removed: remove /etc/apt/sources.list.d/ondrej-php.list: read-only file system"
		if got, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.HasPrefix(got, why) {
			t.Fatalf("reason: %q", got)
		}
		if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, why) {
			t.Fatalf("doctor: %+v", line)
		}
	})
	t.Run("the index update fails elsewhere", func(t *testing.T) {
		// The PPA's own relabel, say, or an administrator's broken source:
		// apt read Sury all the same, so the move goes on.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		f.agent.PkgFails = map[string]string{"update-index": "Get:2 https://packages.sury.org/php noble InRelease [6063 B]\nGet:3 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble InRelease [24.6 kB]\nGet:5 https://packages.sury.org/php noble/main amd64 Packages [218 kB]\nReading package lists...\n" +
			"E: Repository 'https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble InRelease' changed its 'Label' value from '***** The main PPA for supported PHP versions with many PECL extensions *****' to 'PPA for PHP (deprecated)'"}
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, moved) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(moved, "\n"))
		}
		if _, ok := f.agent.File(surySource); !ok {
			t.Fatal("Sury's source went")
		}
		if _, err := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a reason is recorded: %v", err)
		}
	})
	t.Run("an administrator's own source of Sury", func(t *testing.T) {
		// Set up as Sury's README says, and read by apt: the panel's source
		// beside it, signed by another keyring, would make apt refuse every
		// source, and the move fail at every start. The PPA alone goes.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		seedFiles(t, f, map[string]string{adminSury: adminSuryList("noble")})
		aptUpdate(t, f)
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, hosts) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(hosts, "\n"))
		}
		if _, ok := f.agent.File(surySource); ok {
			t.Fatal("the panel's source of Sury was written beside the administrator's")
		}
		if src, _ := f.agent.File(adminSury); src != adminSuryList("noble") || slices.Contains(f.agent.Removed(), adminSury) {
			t.Fatalf("the administrator's source: %q (removed: %v)", src, f.agent.Removed())
		}
		for _, p := range []string{ondrejSource, ondrejKeyring} {
			if _, ok := f.agent.File(p); ok {
				t.Errorf("%s is still there", p)
			}
		}
		if _, err := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a reason is recorded: %v", err)
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor after the move: %+v", line)
		}
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 {
			t.Fatalf("a second start: %v", got)
		}
	})
	t.Run("an administrator's source of Sury apt has not read", func(t *testing.T) {
		// The look before the write cannot find a source apt never read;
		// with the panel's beside it apt refuses both. The panel's goes
		// again, and the reason says what to do.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		seedFiles(t, f, map[string]string{adminSury: adminSuryList("noble")})
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, unread) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(unread, "\n"))
		}
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
		if src, _ := f.agent.File(adminSury); src != adminSuryList("noble") || slices.Contains(f.agent.Removed(), adminSury) {
			t.Fatalf("the administrator's source: %q (removed: %v)", src, f.agent.Removed())
		}
		const advice = "another source of packages.sury.org names another keyring or none (yours?): apt-get update has apt read it, and the next start of the API keeps it, removing the PPA alone; or remove it, and the next start puts the panel's in its place"
		if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "apt update-index failed: exit status 100 (monopanel-pkg-test.service): "+suryConflict) || !strings.Contains(why, advice) {
			t.Fatalf("reason: %q", why)
		}
		if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, advice) {
			t.Fatalf("doctor: %+v", line)
		}
		// Once apt has read it, the next start keeps it and drops the PPA.
		aptUpdate(t, f)
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, hosts) {
			t.Fatalf("calls at the next start:\n%s", strings.Join(got, "\n"))
		}
		if _, ok := f.agent.File(surySource); ok {
			t.Fatal("the panel's source of Sury was written beside the administrator's")
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor after the move: %+v", line)
		}
	})
	t.Run("the host's own source has no Sury after the refresh", func(t *testing.T) {
		// apt had Sury's builds from the host's mirror; the index update with
		// the PPA still there ends well and says nothing against the mirror,
		// but after it apt has Sury's builds from no host — the mirror serves
		// other builds now. What apt lists after the update decides: the PPA
		// stays.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		const mirror = "/etc/apt/sources.list.d/php-mirror.list"
		seedFiles(t, f, map[string]string{mirror: "deb [signed-by=/usr/share/keyrings/deb.sury.org-php.gpg] http://mirror.internal/sury/php/ noble main\n"})
		before := strings.ReplaceAll(policyWithSury, "https://packages.sury.org/php", "http://mirror.internal/sury/php")
		const after = "php-common:\n  Installed: 2:101~+ubuntu24.04.1+deb.sury.org+1\n  Candidate: 2:101~+ubuntu24.04.1+deb.sury.org+1\n  Version table:\n" +
			" *** 2:101~+ubuntu24.04.1+deb.sury.org+1 500\n        500 http://mirror.internal/sury/php noble/main amd64 Packages\n        500 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble/main amd64 Packages\n        100 /var/lib/dpkg/status\n" +
			"     2:93ubuntu2 500\n        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages\n"
		const update = "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\nHit:2 http://mirror.internal/sury/php noble InRelease\nHit:3 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble InRelease\nReading package lists...\n"
		f.agent.PkgOutput = map[string]string{"update-index": update, "available": before}
		f.agent.SetBefore(func(path string, body []byte) {
			var req agent.PkgRequest
			if path == "/v1/pkg" && json.Unmarshal(body, &req) == nil && req.Action == "update-index" {
				f.agent.PkgOutput = map[string]string{"update-index": update, "available": after}
			}
		})
		t.Cleanup(func() { f.agent.SetBefore(nil) })
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, hosts[:3]) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(hosts[:3], "\n"))
		}
		if src, _ := f.agent.File(ondrejSource); src != ppaSource {
			t.Fatalf("the PPA's source: %q", src)
		}
		if key, _ := f.agent.File(ondrejKeyring); key != ppaKey {
			t.Fatalf("the PPA's keyring: %q", key)
		}
		if _, ok := f.agent.File(surySource); ok {
			t.Fatal("the panel wrote its source of Sury beside the mirror")
		}
		const why = "apt could not refresh packages.sury.org/php from the host's own source of it (mirror.internal): the index update ended without it"
		if got, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); got != why {
			t.Fatalf("reason: %q", got)
		}
		if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, why) {
			t.Fatalf("doctor: %+v", line)
		}
	})
	t.Run("Sury does not publish the release", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, false)
		f := newSiteFixture(t)
		seedPPA(t, f)
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 {
			t.Fatalf("the PPA must stay as it is: %v", got)
		}
		why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch)
		if why != "packages.sury.org/php does not publish Ubuntu 24.04 (noble) yet" {
			t.Fatalf("reason: %q", why)
		}
		if line := repoLine(f); line == nil || line.Status != "warn" || !strings.Contains(line.Detail, why) {
			t.Fatalf("doctor: %+v", line)
		}
	})
	t.Run("Sury cannot be asked", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		prev := suryHasRelease
		suryHasRelease = func(context.Context, string) (bool, error) {
			return false, errors.New("dial tcp: lookup packages.sury.org: i/o timeout")
		}
		t.Cleanup(func() { suryHasRelease = prev })
		f := newSiteFixture(t)
		seedPPA(t, f)
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 {
			t.Fatalf("the PPA must stay as it is: %v", got)
		}
		if why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch); !strings.Contains(why, "cannot ask packages.sury.org whether it publishes noble") || !strings.Contains(why, "i/o timeout") {
			t.Fatalf("reason: %q", why)
		}
	})
	t.Run("no PPA", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		probes := countSuryProbes(t)
		f := newSiteFixture(t)
		f.agent.StatWritten = true
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 || probes.Load() != 0 {
			t.Fatalf("a host without the PPA: %v, %d probes", got, probes.Load())
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor: %+v", line)
		}
	})
	t.Run("an administrator's PPA", func(t *testing.T) {
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		// add-apt-repository of the years before 2022 (Ubuntu 20.04 and the
		// hosts upgraded from it): the PPA's old address, its key in
		// trusted.gpg.d. Another URI than the panel's: apt takes both.
		const old = "/etc/apt/sources.list.d/ondrej-ubuntu-php-noble.list"
		const oldPPA = "deb http://ppa.launchpad.net/ondrej/php/ubuntu noble main\n"
		seedFiles(t, f, map[string]string{old: oldPPA})
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); len(got) > 0 {
			t.Fatalf("the administrator's own source alone: %v", got)
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor: %+v", line)
		}
		// Beside the panel's: the panel's goes, the administrator's stays.
		seedPPA(t, f)
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, moved) {
			t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
		}
		if src, ok := f.agent.File(old); !ok || src != oldPPA || slices.Contains(f.agent.Removed(), old) {
			t.Fatalf("the administrator's source: %q (there %v)", src, ok)
		}
	})
	t.Run("an administrator's PPA apt refuses beside the panel's", func(t *testing.T) {
		// add-apt-repository of today: the PPA's address of the panel's
		// source, the key inline. apt refuses the two together, and with them
		// every source, since the panel 0.8.19 wrote its own beside it: the
		// move cannot happen, and the reason says why and what to do.
		withOSRelease(t, ubuntuNoble)
		stubSury(t, true)
		f := newSiteFixture(t)
		const admin = "Types: deb\nURIs: https://ppa.launchpadcontent.net/ondrej/php/ubuntu/\nSuites: noble\nComponents: main\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n xo0ESX35nAEEALKDCUDVXvmW9n+T\n -----END PGP PUBLIC KEY BLOCK-----\n"
		seedFiles(t, f, map[string]string{adminPPA: admin})
		seedPPA(t, f)
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, unread) {
			t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(unread, "\n"))
		}
		why, _ := f.db.GetSetting(f.ctx, settingPHPRepoSwitch)
		if !strings.Contains(why, "Conflicting values set for option Signed-By regarding source https://ppa.launchpadcontent.net/ondrej/php/ubuntu/ noble: /etc/apt/keyrings/ondrej-php.asc != -----BEGIN PGP PUBLIC KEY BLOCK-----") ||
			!strings.Contains(why, "another source of ppa:ondrej/php names another keyring or none (yours, from add-apt-repository?)") || strings.Contains(why, suryAdvice) {
			t.Fatalf("reason: %q", why)
		}
		for _, p := range []string{ondrejSource, ondrejKeyring, adminPPA} {
			if _, ok := f.agent.File(p); !ok {
				t.Errorf("%s went", p)
			}
		}
		if line := repoLine(f); line == nil || !strings.Contains(line.Detail, "another source of ppa:ondrej/php") {
			t.Fatalf("doctor: %+v", line)
		}
		// The administrator removes his: the next start moves the host.
		if _, err := f.agent.Client().RemovePaths(f.ctx, &agent.RemovePathsRequest{Paths: []string{adminPPA}}); err != nil {
			t.Fatal(err)
		}
		from = len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, moved) {
			t.Fatalf("calls at the next start:\n%s", strings.Join(got, "\n"))
		}
	})
	t.Run("a keyring without its source", func(t *testing.T) {
		// What 0.8.19 left on Ubuntu 26.04 after apt refused the PPA.
		withOSRelease(t, "ID=ubuntu\nVERSION_ID=26.04\nVERSION_CODENAME=resolute\n")
		stubSury(t, true)
		probes := countSuryProbes(t)
		f := newSiteFixture(t)
		seedFiles(t, f, map[string]string{ondrejKeyring: ppaKey})
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := repoCalls(f, from); !slices.Equal(got, []string{"remove " + ondrejKeyring}) || probes.Load() != 0 {
			t.Fatalf("calls: %v, %d probes", got, probes.Load())
		}
	})
	t.Run("not Ubuntu", func(t *testing.T) {
		withOSRelease(t, "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n")
		stubSury(t, true)
		f := newSiteFixture(t)
		seedPPA(t, f)
		from := len(f.agent.Calls())
		f.s.switchPHPRepoToSury(f.ctx)
		if got := f.agent.Calls()[from:]; len(got) > 0 {
			t.Fatalf("Debian: %d agent calls", len(got))
		}
		if line := repoLine(f); line != nil {
			t.Fatalf("doctor: %+v", line)
		}
	})
}

// apt has Sury's index for the release when apt-cache policy names Sury's
// PHP repository with its suite.
func TestSuryIndexed(t *testing.T) {
	withOSRelease(t, ubuntuNoble)
	f := newSiteFixture(t)
	for _, c := range []struct {
		policy, codename string
		want             bool
	}{
		{policyWithSury, "noble", true},
		{policyWithSury, "jammy", false},
		{policyWithoutSury, "noble", false},
		{"", "noble", false},
		{"php-common:\n  Installed: (none)\n  Candidate: (none)\n  Version table:\n", "noble", false},
		{"        500 https://packages.sury.org.evil.example/php noble/main amd64 Packages\n", "noble", false},
		// Sury's other repositories are not its PHP.
		{"        500 https://packages.sury.org/apache2 noble/main amd64 Packages\n", "noble", false},
		{"        500 https://packages.sury.org/php-qa noble/main amd64 Packages\n", "noble", false},
		{"        500 https://packages.sury.org/php/ noble/main amd64 Packages\n", "noble", true},
	} {
		f.agent.PkgOutput = map[string]string{"available": c.policy}
		if got := f.s.suryIndexed(f.ctx, c.codename); got != c.want {
			t.Errorf("%s in %q: %v, want %v", c.codename, c.policy, got, c.want)
		}
	}
	f.agent.Fail["/v1/pkg"] = "apt-cache: broken"
	f.agent.PkgOutput = map[string]string{"available": policyWithSury}
	if f.s.suryIndexed(f.ctx, "noble") {
		t.Error("an apt-cache that failed says nothing")
	}
}
