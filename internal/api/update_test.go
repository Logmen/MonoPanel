package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// release is a published version served by a fake GitHub, the way the update
// job sees one: a package, a checksum list and a signature over it.
type release struct {
	*httptest.Server
	pkgName  string
	pkgBody  []byte
	pkgSum   string
	pub      string
	sums     []byte
	sig      []byte
	assets   map[int64][]byte
	unsigned bool
}

// newRelease publishes version v of the panel for this host's architecture.
func newRelease(t *testing.T, v string, opts ...func(*release)) *release {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("not really a package, but it hashes like one")
	sum := sha256.Sum256(body)
	r := &release{
		pkgName: updater.AssetName("deb", runtime.GOARCH, v),
		pkgBody: body,
		pkgSum:  hex.EncodeToString(sum[:]),
		pub:     base64.StdEncoding.EncodeToString(pub),
		assets:  map[int64][]byte{},
	}
	for _, o := range opts {
		o(r)
	}
	r.sums = []byte(r.pkgSum + "  " + r.pkgName + "\n")
	r.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, r.sums)))

	assets := []map[string]any{
		{"name": r.pkgName, "id": 1, "size": len(r.pkgBody)},
		{"name": updater.SumsFile, "id": 2, "size": len(r.sums)},
	}
	r.assets[1], r.assets[2] = r.pkgBody, r.sums
	if !r.unsigned {
		assets = append(assets, map[string]any{"name": updater.SigFile, "id": 3, "size": len(r.sig)})
		r.assets[3] = r.sig
	}
	rel := []map[string]any{{"tag_name": "v" + v, "name": "MonoPanel " + v, "body": "исправления", "assets": assets}}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/panel/releases", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(rel) //nolint:errcheck // test double
	})
	mux.HandleFunc("/repos/acme/panel/releases/assets/", func(w http.ResponseWriter, req *http.Request) {
		var id int64
		fmt.Sscanf(filepath.Base(req.URL.Path), "%d", &id)
		w.Write(r.assets[id]) //nolint:errcheck // test double
	})
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Close)
	return r
}

// withWrongSums publishes a checksum list that does not describe the package.
func withWrongSums(r *release) { r.pkgSum = strings.Repeat("0", 64) }

// unsigned publishes a release with no signature asset.
func unsigned(r *release) { r.unsigned = true }

// configure points the panel at the fake repository.
func (f *siteFixture) configureUpdates(r *release) apitypes.UpdateStatus {
	f.t.Helper()
	var st apitypes.UpdateStatus
	f.call(http.MethodPut, "/system/update", map[string]any{
		"repo": "acme/panel", "api": r.URL, "token": "secret-token", "channel": "stable",
	}, http.StatusOK, &st)
	return st
}

func TestUpdateInstallsSignedRelease(t *testing.T) {
	rel := newRelease(t, "9.9.9")
	f := newFixture(t, func(c *config.Config) { c.Update.PublicKey = rel.pub })

	st := f.configureUpdates(rel)
	if !st.Settings.HasToken || st.Settings.Repo != "acme/panel" || !st.KeyPinned {
		t.Fatalf("settings not stored: %+v", st.Settings)
	}

	f.call(http.MethodPost, "/system/update/check", nil, http.StatusOK, &st)
	if !st.Available || st.Latest != "9.9.9" || st.Tag != "v9.9.9" {
		t.Fatalf("check found %+v", st)
	}
	if st.CheckedAt == nil || st.LastError != "" {
		t.Fatalf("check should record when it ran and no error: %+v", st)
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("update job failed: %s", job.Error)
	}

	call, ok := f.agent.LastCall("/v1/panel/install")
	if !ok {
		t.Fatal("the agent was never asked to install the package")
	}
	var req agent.InstallPanelRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.s.cfg.DownloadsDir(), rel.pkgName)
	if req.Package != want || req.Version != "9.9.9" || !strings.EqualFold(req.SHA256, rel.pkgSum) {
		t.Fatalf("agent asked to install %+v, want %s at %s", req, rel.pkgSum, want)
	}
	if req.Sums == "" || req.Sig == "" {
		t.Fatal("the agent must receive the signed checksums, not just a digest the API computed itself")
	}
	if err := updater.VerifySums(rel.pub, []byte(req.Sums), []byte(req.Sig)); err != nil {
		t.Fatalf("what the agent got does not verify: %v", err)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != string(rel.pkgBody) {
		t.Fatalf("staged package: %v", err)
	}
}

func TestUpdateRefusesAPackageTheChecksumsDoNotDescribe(t *testing.T) {
	rel := newRelease(t, "9.9.9", withWrongSums)
	f := newFixture(t, func(c *config.Config) { c.Update.PublicKey = rel.pub })
	f.configureUpdates(rel)

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "контрольная сумма") {
		t.Fatalf("a package that does not match its signed digest must not install: %s %s", job.Status, job.Error)
	}
	if f.agent.Called("/v1/panel/install") {
		t.Fatal("the agent was asked to install it anyway")
	}
}

func TestUpdateRefusesAnUnsignedReleaseWhenAKeyIsPinned(t *testing.T) {
	rel := newRelease(t, "9.9.9", unsigned)
	f := newFixture(t, func(c *config.Config) { c.Update.PublicKey = rel.pub })
	f.configureUpdates(rel)

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "не подписан") {
		t.Fatalf("an unsigned release must be refused where a key is pinned: %s %s", job.Status, job.Error)
	}

	// Without a pinned key the same release installs: the digest still has to
	// match, but nothing vouches for who produced it.
	f2 := newFixture(t, nil)
	f2.configureUpdates(rel)
	f2.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusAccepted, &ref)
	if job := f2.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("unsigned release on an unpinned host: %s %s", job.Status, job.Error)
	}
}

func TestUpdateSettingsValidatedAndAdminOnly(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPut, "/system/update", map[string]any{"repo": "not a repo"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPut, "/system/update", map[string]any{"repo": "acme/panel", "api": "ftp://nope"}, http.StatusUnprocessableEntity, nil)

	// A repository URL is accepted where owner/name is expected: it is what
	// people paste out of the browser.
	var st apitypes.UpdateStatus
	f.call(http.MethodPut, "/system/update", map[string]any{"repo": "https://github.com/acme/panel.git", "check_hours": 0}, http.StatusOK, &st)
	if st.Settings.Repo != "acme/panel" || st.Settings.CheckHours != 0 {
		t.Fatalf("settings: %+v", st.Settings)
	}
	// Nothing was checked yet, so nothing is offered.
	f.call(http.MethodGet, "/system/update", nil, http.StatusOK, &st)
	if st.Available || st.Latest != "" {
		t.Fatalf("no check has run: %+v", st)
	}

	h, _ := auth.HashPassword("alex-password")
	owner, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserPassword(f.ctx, owner.ID, h); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password")
	f.call(http.MethodGet, "/system/update", nil, http.StatusForbidden, nil)
	f.call(http.MethodPost, "/system/update/apply", map[string]any{}, http.StatusForbidden, nil)
}
