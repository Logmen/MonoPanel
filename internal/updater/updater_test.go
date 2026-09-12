package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewerComparesVersions(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"0.5.1", "0.6.0", true},
		{"0.5.1", "v0.6.0", true},
		{"0.6.0", "0.6.0", false},
		{"0.6.0", "0.5.9", false},
		{"0.6.0-dev", "0.6.0", true},        // a dev build is superseded by the release
		{"0.6.0", "0.6.1-rc1", true},        // prereleases only arrive on the beta channel
		{"0.6.0-3-gabc123", "0.6.0", false}, // built after the tag: already newer
		{"0.6.0-3-gabc123", "0.6.1", true},
		{"0.6.0-dirty", "0.6.0", false},
		{"dev", "0.1.0", true}, // a hand-built binary never blocks an update
		{"0.6.0", "not-a-version", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.candidate); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetNames(t *testing.T) {
	if got := AssetName("deb", "amd64", "v0.6.0"); got != "monopanel_0.6.0_amd64.deb" {
		t.Errorf("deb name: %s", got)
	}
	if got := AssetName("rpm", "arm64", "0.6.0"); got != "monopanel-0.6.0.aarch64.rpm" {
		t.Errorf("rpm name: %s", got)
	}
	if got := AssetName("bin", "amd64", "0.6.0"); got != "monopanel-linux-amd64" {
		t.Errorf("binary name: %s", got)
	}
	rel := &Release{Tag: "v0.6.0", Version: "0.6.0", Assets: []Asset{{Name: "monopanel-0.6.0.x86_64.rpm", ID: 7}}}
	if _, err := Select(rel, "debian", "amd64"); err == nil {
		t.Fatal("a release without a .deb must not satisfy a debian host")
	}
	a, err := Select(rel, "rhel", "amd64")
	if err != nil || a.ID != 7 {
		t.Fatalf("select rpm: %+v %v", a, err)
	}
}

func TestSumsAndSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("d0c2f8e9  monopanel_0.6.0_amd64.deb\n" + strings.Repeat("a", 64) + "  SHA256SUMS.other\n")
	parsed := ParseSums(sums)
	if len(parsed) != 1 || parsed["SHA256SUMS.other"] != strings.Repeat("a", 64) {
		t.Fatalf("short digests must be ignored: %v", parsed)
	}
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)))
	key := base64.StdEncoding.EncodeToString(pub)
	if err := VerifySums(key, sums, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySums(key, append(sums, 'x'), sig); err == nil {
		t.Fatal("a modified checksum list must not verify")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifySums(base64.StdEncoding.EncodeToString(other), sums, sig); err == nil {
		t.Fatal("a signature from another key must not verify")
	}
	if err := VerifySums("", sums, sig); err == nil {
		t.Fatal("an empty key is not a trust anchor")
	}
}

// fakeReleases serves the part of the GitHub API the panel uses.
type fakeReleases struct {
	*httptest.Server
	assets map[int64][]byte
}

func newFakeReleases(t *testing.T, releases ...map[string]any) *fakeReleases {
	t.Helper()
	f := &fakeReleases{assets: map[int64][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/panel/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(releases) //nolint:errcheck // test double
	})
	mux.HandleFunc("/repos/acme/panel/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscanf(filepath.Base(r.URL.Path), "%d", &id)
		body, ok := f.assets[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(body) //nolint:errcheck // test double
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestClientPicksTheHighestVersion(t *testing.T) {
	f := newFakeReleases(t,
		map[string]any{"tag_name": "v0.7.0", "prerelease": true},
		map[string]any{"tag_name": "v0.6.0", "assets": []map[string]any{{"name": "monopanel_0.6.0_amd64.deb", "id": 11, "size": 3}}},
		map[string]any{"tag_name": "v0.6.1", "draft": true},
		map[string]any{"tag_name": "v0.5.9"},
		map[string]any{"tag_name": "nightly"},
	)
	f.assets[11] = []byte("deb")
	cl := &Client{Repo: "acme/panel", Token: "secret-token", API: f.URL}
	ctx := context.Background()

	rel, err := cl.Latest(ctx, ChannelStable)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.6.0" {
		t.Fatalf("stable channel picked %s, want 0.6.0 (drafts, prereleases and junk tags are ignored)", rel.Version)
	}
	beta, err := cl.Latest(ctx, ChannelBeta)
	if err != nil {
		t.Fatal(err)
	}
	if beta.Version != "0.7.0" {
		t.Fatalf("beta channel picked %s, want 0.7.0", beta.Version)
	}

	a, err := Select(rel, "debian", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), a.Name)
	sum, err := cl.SaveTo(ctx, a, path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("deb"))
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("digest %s does not describe the downloaded file", sum)
	}
	if b, _ := os.ReadFile(path); string(b) != "deb" {
		t.Fatalf("downloaded %q", b)
	}
	if onDisk, _ := FileSHA256(path); onDisk != sum {
		t.Fatal("FileSHA256 disagrees with the download")
	}
}

func TestClientReportsMissingAccess(t *testing.T) {
	f := newFakeReleases(t)
	cl := &Client{Repo: "acme/panel", Token: "wrong", API: f.URL}
	_, err := cl.Latest(context.Background(), ChannelStable)
	if err == nil || !strings.Contains(err.Error(), "check the repository name and the token") {
		t.Fatalf("a private repository answers 404; the panel must say why: %v", err)
	}
	if _, err := (&Client{Repo: "not-a-repo"}).Latest(context.Background(), ChannelStable); err == nil {
		t.Fatal("owner/name is required")
	}
}

func TestReadStateSurvivesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	st, err := ReadState(dir)
	if err != nil || st != nil {
		t.Fatalf("no update yet: %v %v", st, err)
	}
	if err := writeState(dir, &State{Status: StatusDone, From: "0.5.1", To: "0.6.0"}); err != nil {
		t.Fatal(err)
	}
	st, err = ReadState(dir)
	if err != nil || st == nil || st.To != "0.6.0" {
		t.Fatalf("state round trip: %+v %v", st, err)
	}
}

// A slow link is not an error: a package that trickles in for longer than
// any fixed deadline still arrives, while a body that stops sending is
// given up after the idle period.
func TestSaveToSlowLinkAndStall(t *testing.T) {
	stall := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			w.Write([]byte("chunk-of-the-package\n")) //nolint:errcheck // test server
			fl.Flush()
			if stall && i == 2 {
				time.Sleep(700 * time.Millisecond)
			} else {
				time.Sleep(60 * time.Millisecond)
			}
		}
	}))
	defer srv.Close()
	c := &Client{Repo: "o/r", API: srv.URL, Idle: 300 * time.Millisecond}
	dir := t.TempDir()
	// 8 × 60 ms is longer than the idle period, but data keeps coming.
	sum, err := c.SaveTo(context.Background(), Asset{ID: 1}, filepath.Join(dir, "a.deb"))
	if err != nil || sum == "" {
		t.Fatalf("slow link must succeed: %v", err)
	}
	stall = true
	if _, err := c.SaveTo(context.Background(), Asset{ID: 1}, filepath.Join(dir, "b.deb")); err == nil {
		t.Fatal("a stalled body must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.deb")); err == nil {
		t.Fatal("a failed download must not leave the file in place")
	}
}

// With the anonymous API quota gone, a public repository is still readable:
// the site's redirect names the latest tag and the assets have plain
// addresses. A token means a private repository, where nothing else works.
func TestClientFallsBackWithoutAPIWhenRateLimited(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for 203.0.113.9."}`)
	}))
	defer api.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acme/panel/releases/latest":
			http.Redirect(w, r, "/acme/panel/releases/tag/v0.7.2", http.StatusFound)
		case "/acme/empty/releases/latest":
			http.Redirect(w, r, "/acme/empty/releases", http.StatusFound)
		case "/acme/panel/releases/download/v0.7.2/SHA256SUMS":
			http.Redirect(w, r, "/objects/sums", http.StatusFound)
		case "/objects/sums":
			fmt.Fprint(w, "0000  monopanel_0.7.2_amd64.deb\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	ctx := context.Background()
	c := &Client{Repo: "acme/panel", API: api.URL, Site: site.URL}
	rel, err := c.Latest(ctx, ChannelStable)
	if err != nil || rel.Version != "0.7.2" || rel.Tag != "v0.7.2" {
		t.Fatalf("latest without the API: %v %+v", err, rel)
	}
	a, err := Select(rel, "debian", "amd64")
	if err != nil || a.URL != site.URL+"/acme/panel/releases/download/v0.7.2/monopanel_0.7.2_amd64.deb" {
		t.Fatalf("asset by name: %v %+v", err, a)
	}
	sums, _ := rel.Asset(SumsFile)
	if b, err := c.Bytes(ctx, sums); err != nil || !strings.Contains(string(b), "monopanel_0.7.2_amd64.deb") {
		t.Fatalf("checksum list by its address: %v %q", err, b)
	}
	if rel, err := c.ByTag(ctx, "0.7.2"); err != nil || rel.Version != "0.7.2" {
		t.Fatalf("by tag without the API: %v %+v", err, rel)
	}
	if _, err := c.ByTag(ctx, "0.1.0"); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("a tag without assets is no release: %v", err)
	}
	if _, err := (&Client{Repo: "acme/empty", API: api.URL, Site: site.URL}).Latest(ctx, ChannelStable); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("a repository without releases: %v", err)
	}
	c.Token = "token"
	if _, err := c.Latest(ctx, ChannelStable); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("with a token the rate limit is reported, not worked around: %v", err)
	}
}
