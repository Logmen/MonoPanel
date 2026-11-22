package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

const stubOndrejKeyPEM = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nondrej 2024\n-----END PGP PUBLIC KEY BLOCK-----\n"

// stubOndrejKey serves a stand-in for the PPA's key, so the tests never
// reach keyserver.ubuntu.com, and counts the downloads.
func stubOndrejKey(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := ondrejKey
	ondrejKey = func(context.Context) (string, error) { n++; return stubOndrejKeyPEM, nil }
	t.Cleanup(func() { ondrejKey = prev })
	return &n
}

// The keyring gets the PPA's 2024 key: apt on Ubuntu 26.04 refuses the
// 2012 rsa1024 key the panel used to fetch, and the PPA signs with both.
func TestOndrejKeyIsTheCurrentOne(t *testing.T) {
	if !strings.HasSuffix(ondrejKeyURL, "0x"+ondrejKeyFingerprint) || strings.Contains(ondrejKeyURL, "14AA40EC0831756756D7F66C4F4EA0AAE5267A6C") {
		t.Fatalf("the PPA key: %s", ondrejKeyURL)
	}
}

// A host that took the PPA before gets the current key once at start; one
// without the PPA, or not on Ubuntu, downloads nothing.
func TestRefreshOndrejKey(t *testing.T) {
	t.Run("ubuntu with the PPA", func(t *testing.T) {
		withOSRelease(t, "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n")
		f := newSiteFixture(t)
		n := stubOndrejKey(t)
		f.agent.Stat[ondrejSource] = false
		f.s.refreshOndrejKey(f.ctx)
		if got, ok := f.agent.File(ondrejKeyring); !ok || got != stubOndrejKeyPEM || *n != 1 {
			t.Fatalf("keyring %q (written %v), %d downloads", got, ok, *n)
		}
		if v, _ := f.db.GetSetting(f.ctx, settingOndrejKey); v != ondrejKeyFingerprint {
			t.Fatalf("setting %q", v)
		}
		writes := len(f.agent.Calls())
		f.s.refreshOndrejKey(f.ctx)
		if *n != 1 || len(f.agent.Calls()) != writes {
			t.Fatalf("a second start must do nothing: %d downloads, %d calls after %d", *n, len(f.agent.Calls()), writes)
		}
	})
	t.Run("ubuntu without the PPA", func(t *testing.T) {
		withOSRelease(t, "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n")
		f := newSiteFixture(t)
		n := stubOndrejKey(t)
		f.s.refreshOndrejKey(f.ctx)
		if _, ok := f.agent.File(ondrejKeyring); ok || *n != 0 {
			t.Fatalf("no PPA, no key: written %v, %d downloads", ok, *n)
		}
	})
	t.Run("debian", func(t *testing.T) {
		f := newSiteFixture(t)
		n := stubOndrejKey(t)
		f.agent.Stat[ondrejSource] = false
		f.s.refreshOndrejKey(f.ctx)
		if *n != 0 {
			t.Fatalf("Debian takes packages.sury.org: %d downloads", *n)
		}
	})
}

// A repository apt cannot read must not stay behind: every later index
// update on the host would fail with it.
func TestPHPRepoSourceGoesWhenAptCannotReadIt(t *testing.T) {
	withOSRelease(t, "ID=ubuntu\nVERSION_ID=26.04\nVERSION_CODENAME=resolute\n")
	prev := ppaHasRelease
	ppaHasRelease = func(context.Context, string) (bool, error) { return true, nil }
	t.Cleanup(func() { ppaHasRelease = prev })
	stubOndrejKey(t)
	f := newSiteFixture(t)
	f.agent.Fail["/v1/pkg"] = "E: The repository 'https://ppa.launchpadcontent.net/ondrej/php/ubuntu resolute InRelease' is not signed."
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": "8.3"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobFailed {
		t.Fatalf("the install must fail: %s %s", job.Status, job.Error)
	}
	removed := strings.Join(f.agent.Removed(), " ")
	if !strings.Contains(removed, ondrejSource) {
		t.Fatalf("the unreadable source must go again (removed: %s)", removed)
	}
}
