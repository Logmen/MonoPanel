// Package updater finds MonoPanel releases published as packages in a git
// repository, verifies them against a signed checksum list and installs them.
//
// The panel updates itself, so the install half runs detached from both the
// API and the agent (see install.go): the process that starts an update is
// killed by the very restart it triggers.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Channels. Beta also considers prereleases; stable never does.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Names of the release assets the panel looks for. SumsFile lists every other
// asset's SHA-256; SigFile is a detached ed25519 signature over SumsFile, so
// one signature covers the whole release.
const (
	SumsFile = "SHA256SUMS"
	SigFile  = "SHA256SUMS.sig"
)

// DefaultAPI is GitHub's REST endpoint; tests point Client.API elsewhere.
const DefaultAPI = "https://api.github.com"

// DefaultSite is GitHub's web host: a public repository's releases are
// reachable there without the API and its anonymous rate limit.
const DefaultSite = "https://github.com"

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
	Size int64  `json:"size"`
	// URL is the plain download address, set when the release was resolved
	// without the API (the id and the size are unknown then).
	URL string `json:"url,omitempty"`
}

// Release is a published version.
type Release struct {
	Tag         string    `json:"tag"`
	Version     string    `json:"version"`
	Name        string    `json:"name,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	Assets      []Asset   `json:"assets,omitempty"`
}

// Asset returns the named file of the release.
func (r *Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client reads releases from a GitHub repository. A token is required for
// private repositories and raises the rate limit for public ones.
type Client struct {
	Repo  string // owner/name
	Token string
	API   string // defaults to DefaultAPI
	Site  string // defaults to DefaultSite
	HTTP  *http.Client
	// Idle is how long a response may send nothing before the request is
	// given up (default one minute). There is no cap on the whole transfer:
	// a package on a slow link takes as long as it takes.
	Idle time.Duration
}

// ErrNoRelease means the repository has no release for the channel.
var ErrNoRelease = errors.New("no release published yet")

// ErrRateLimited means GitHub's API refused the request because the address
// used up its quota: 60 anonymous requests an hour, which a shared NAT
// exhausts quickly. A public repository is then read without the API.
var ErrRateLimited = errors.New("GitHub API rate limit exceeded")

var repoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ValidRepo reports whether s is an owner/name pair.
func ValidRepo(s string) bool { return repoRe.MatchString(s) }

func (c *Client) base() string {
	if c.API != "" {
		return strings.TrimSuffix(c.API, "/")
	}
	return DefaultAPI
}

func (c *Client) site() string {
	if c.Site != "" {
		return strings.TrimSuffix(c.Site, "/")
	}
	return DefaultSite
}

// http is a client without an overall deadline: connecting and the first
// byte are bounded by the transport, the body by the idle watch in get.
func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}}
}

func (c *Client) idle() time.Duration {
	if c.Idle > 0 {
		return c.Idle
	}
	return time.Minute
}

// idleBody cancels the request when the body stops delivering data.
type idleBody struct {
	io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

func (c *Client) get(ctx context.Context, path, accept string) (*http.Response, error) {
	return c.getURL(ctx, c.base()+path, accept)
}

func (c *Client) getURL(ctx context.Context, url, accept string) (*http.Response, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.http().Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	idle := c.idle()
	res.Body = &idleBody{ReadCloser: res.Body, timer: time.AfterFunc(idle, cancel), idle: idle, cancel: cancel}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		msg := strings.TrimSpace(string(body))
		switch res.StatusCode {
		case http.StatusNotFound:
			// A private repository answers 404 for a wrong or missing token too.
			return nil, fmt.Errorf("%s: not found (check the repository name and the token)", c.Repo)
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			if res.Header.Get("X-RateLimit-Remaining") == "0" || res.StatusCode == http.StatusTooManyRequests {
				return nil, fmt.Errorf("%s: %w (%s)", c.Repo, ErrRateLimited, msg)
			}
			// A public repository answers 401 to a token that is no longer
			// valid — the token is then the problem, not the access.
			if c.Token != "" {
				return nil, fmt.Errorf("%s: access denied (%s); if the repository is public, drop the token: mp update settings --clear-token", c.Repo, res.Status)
			}
			return nil, fmt.Errorf("%s: access denied (%s)", c.Repo, res.Status)
		}
		return nil, fmt.Errorf("%s: %s: %s", c.Repo, res.Status, msg)
	}
	return res, nil
}

// ghRelease is the part of GitHub's release object the panel uses.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (g ghRelease) release() *Release {
	r := &Release{Tag: g.TagName, Version: Version(g.TagName), Name: g.Name, Notes: g.Body, Prerelease: g.Prerelease, PublishedAt: g.PublishedAt}
	for _, a := range g.Assets {
		r.Assets = append(r.Assets, Asset{Name: a.Name, ID: a.ID, Size: a.Size})
	}
	return r
}

// Latest returns the newest release of the channel. Releases are ordered by
// version, not by publication date, so re-publishing an old tag cannot push a
// downgrade onto every panel.
func (c *Client) Latest(ctx context.Context, channel string) (*Release, error) {
	if !ValidRepo(c.Repo) {
		return nil, errors.New("repository must be owner/name")
	}
	res, err := c.get(ctx, "/repos/"+c.Repo+"/releases?per_page=30", "application/vnd.github+json")
	if err != nil {
		if errors.Is(err, ErrRateLimited) && c.Token == "" {
			return c.latestWithoutAPI(ctx)
		}
		return nil, err
	}
	defer res.Body.Close()
	var list []ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	var best *Release
	for _, g := range list {
		if g.Draft || (g.Prerelease && channel != ChannelBeta) {
			continue
		}
		r := g.release()
		if !semver.IsValid(Normalize(r.Version)) {
			continue
		}
		if best == nil || Newer(best.Version, r.Version) {
			best = r
		}
	}
	if best == nil {
		return nil, ErrNoRelease
	}
	return best, nil
}

// ByTag returns one release by its tag ("v0.6.0" or "0.6.0").
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	if !ValidRepo(c.Repo) {
		return nil, errors.New("repository must be owner/name")
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	res, err := c.get(ctx, "/repos/"+c.Repo+"/releases/tags/"+tag, "application/vnd.github+json")
	if err != nil {
		if errors.Is(err, ErrRateLimited) && c.Token == "" {
			return c.releaseWithoutAPI(ctx, tag)
		}
		return nil, err
	}
	defer res.Body.Close()
	var g ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&g); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if g.Draft {
		return nil, ErrNoRelease
	}
	return g.release(), nil
}

// latestWithoutAPI resolves the newest release the way a browser does: the
// site redirects /releases/latest to the tag page. That is GitHub's own
// notion of latest (the most recent non-prerelease), good enough while the
// API's anonymous quota is gone; whoever installs still compares versions.
func (c *Client) latestWithoutAPI(ctx context.Context) (*Release, error) {
	loc, err := c.redirect(ctx, c.site()+"/"+c.Repo+"/releases/latest")
	if err != nil {
		return nil, err
	}
	// A repository without releases redirects to the releases list instead.
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if !strings.HasPrefix(tag, "v") || !semver.IsValid(Normalize(Version(tag))) {
		return nil, ErrNoRelease
	}
	return c.releaseWithoutAPI(ctx, tag)
}

// releaseWithoutAPI builds a release from its tag alone: the asset names are
// fixed by the packaging and their download addresses are plain. The
// checksum list is looked up first, so a wrong tag reads as no release.
func (c *Client) releaseWithoutAPI(ctx context.Context, tag string) (*Release, error) {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	base := c.site() + "/" + c.Repo + "/releases/download/" + tag + "/"
	if _, err := c.redirect(ctx, base+SumsFile); err != nil {
		return nil, err
	}
	version := Version(tag)
	r := &Release{Tag: tag, Version: version, Name: "MonoPanel " + version}
	names := []string{SumsFile, SigFile}
	for _, kind := range []string{"deb", "rpm", "bin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			names = append(names, AssetName(kind, arch, version))
		}
	}
	for _, n := range names {
		r.Assets = append(r.Assets, Asset{Name: n, URL: base + n})
	}
	return r, nil
}

// redirect asks where the site sends url without following: the redirect
// target for a release page or an asset, url itself when it answers
// directly, ErrNoRelease when there is nothing there.
func (c *Client) redirect(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	hc := *c.http()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	switch {
	case res.StatusCode >= 300 && res.StatusCode < 400:
		if loc := res.Header.Get("Location"); loc != "" {
			return loc, nil
		}
		return "", fmt.Errorf("%s: redirect without a location", url)
	case res.StatusCode == http.StatusOK:
		return url, nil
	case res.StatusCode == http.StatusNotFound:
		return "", ErrNoRelease
	}
	return "", fmt.Errorf("%s: %s", url, res.Status)
}

// getAsset fetches an asset: by its plain address when the release was
// resolved without the API, through the API by id otherwise.
func (c *Client) getAsset(ctx context.Context, a Asset) (*http.Response, error) {
	if a.URL != "" {
		return c.getURL(ctx, a.URL, "application/octet-stream")
	}
	return c.get(ctx, fmt.Sprintf("/repos/%s/releases/assets/%d", c.Repo, a.ID), "application/octet-stream")
}

// Bytes downloads a small asset (the checksum list and its signature).
func (c *Client) Bytes(ctx context.Context, a Asset) ([]byte, error) {
	res, err := c.getAsset(ctx, a)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

// SaveTo downloads an asset to path and returns its SHA-256. The file is
// written next to its final name and renamed, so a half-finished download is
// never handed to the package manager.
func (c *Client) SaveTo(ctx context.Context, a Asset, path string) (string, error) {
	res, err := c.getAsset(ctx, a)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), res.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// gitDescribe matches the "-<commits>-g<sha>" suffix git describe appends to a
// build made after the tag; such a build is not a prerelease of that tag.
var gitDescribe = regexp.MustCompile(`-\d+-g[0-9a-f]{6,}$`)

// Version strips a leading "v" and the "-dirty" marker of a working-tree build.
func Version(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return strings.TrimSuffix(v, "-dirty")
}

// Normalize returns a comparable semver string. A build made after a tag
// compares as that tag: it already contains the release.
func Normalize(v string) string {
	v = Version(v)
	if v == "" || v == "dev" {
		return ""
	}
	v = gitDescribe.ReplaceAllString(v, "")
	return "v" + v
}

// Newer reports whether candidate is a later version than current. An
// unparsable current version (a hand-built binary) never blocks an update.
func Newer(current, candidate string) bool {
	c, n := Normalize(current), Normalize(candidate)
	if !semver.IsValid(n) {
		return false
	}
	if !semver.IsValid(c) {
		return true
	}
	return semver.Compare(n, c) > 0
}

// AssetName is the file name of a release artefact. The release workflow
// writes exactly these names; nothing here guesses at packaging defaults.
func AssetName(kind, arch, version string) string {
	v := Version(version)
	switch kind {
	case "deb":
		return fmt.Sprintf("monopanel_%s_%s.deb", v, arch)
	case "rpm":
		return fmt.Sprintf("monopanel-%s.%s.rpm", v, rpmArch(arch))
	case "bin":
		return fmt.Sprintf("monopanel-linux-%s", arch)
	}
	return ""
}

func rpmArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goarch
}

// Kind returns the package format of an OS family.
func Kind(family string) string {
	if family == "rhel" {
		return "rpm"
	}
	return "deb"
}

// Select picks the package for this host out of a release.
func Select(r *Release, family, arch string) (Asset, error) {
	name := AssetName(Kind(family), arch, r.Version)
	a, ok := r.Asset(name)
	if !ok {
		return Asset{}, fmt.Errorf("release %s has no %s", r.Tag, name)
	}
	return a, nil
}

// ParseSums reads a sha256sum(1) listing into name -> hex digest.
func ParseSums(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			continue
		}
		out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return out
}

// VerifySums checks the detached signature of the checksum list. An empty
// public key means the panel was never given a trust anchor: the caller
// decides whether to go ahead on checksums alone.
func VerifySums(publicKey string, sums, sig []byte) error {
	key, err := ParsePublicKey(publicKey)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return fmt.Errorf("signature is not base64: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(raw), ed25519.SignatureSize)
	}
	if !ed25519.Verify(key, sums, raw) {
		return errors.New("signature does not match the release key")
	}
	return nil
}

// ParsePublicKey decodes a base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no release key configured")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("release key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("release key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// FileSHA256 hashes a file on disk.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
