package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// chownsAfterStream reports whether the docroot was handed to the account,
// recursively, after the distribution was streamed into tar.
func chownsAfterStream(t *testing.T, f *siteFixture, docroot, login string) (streamed, handed bool) {
	t.Helper()
	for _, c := range f.agent.Calls() {
		switch c.Path {
		case "/v1/stream/in":
			streamed = true
		case "/v1/chown":
			var req agent.ChownRequest
			if err := json.Unmarshal(c.Body, &req); err != nil {
				t.Fatal(err)
			}
			if streamed && req.Path == docroot && req.Owner == login && req.Group == login && req.Recursive {
				handed = true
			}
		}
	}
	return streamed, handed
}

// A download that breaks off leaves what tar unpacked of it root's (tar runs
// with --no-same-owner): the job says the download broke off, and why, and
// hands those files to the account, which could not remove them otherwise.
func TestCMSBrokenDownloadHandsTheFilesOver(t *testing.T) {
	f := cmsFixture(t, nil)
	stub := cmsFetch
	cmsFetch = func(_ context.Context, url string) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("partial tarball ", 4096)),
			failingReader{errors.New("GET " + url + ": no data for 1m0s: context canceled")})), nil
	}
	t.Cleanup(func() { cmsFetch = stub })
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	_, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "download WordPress broke off") || !strings.Contains(job.Error, "no data for 1m0s") {
		t.Fatalf("the job must fail on the download and say so: %s %q", job.Status, job.Error)
	}
	if strings.Contains(job.Error, "agent unreachable") {
		t.Fatalf("the agent did nothing wrong: %q", job.Error)
	}
	streamed, handed := chownsAfterStream(t, f, "/var/www/alex/data/www/wp.example.com", "alex")
	if !streamed || !handed {
		t.Fatalf("what was unpacked must be handed to the account (streamed %v, handed %v): %+v", streamed, handed, f.agent.Calls())
	}
	stored, _ := f.db.GetSiteByDomain(f.ctx, site.Domain)
	if stored.CMS != "" {
		t.Fatalf("a failed install is no CMS of the site: %q", stored.CMS)
	}
}

// tar's own failure (a corrupt archive) hands the part over too.
func TestCMSFailedUnpackHandsTheFilesOver(t *testing.T) {
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": []byte("not a tarball")})
	f.agent.Fail["/v1/stream/in"] = "tar: Unexpected EOF in archive"
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	_, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "unpack WordPress") {
		t.Fatalf("job: %s %q", job.Status, job.Error)
	}
	if _, handed := chownsAfterStream(t, f, "/var/www/alex/data/www/wp.example.com", "alex"); !handed {
		t.Fatalf("what was unpacked must be handed to the account: %+v", f.agent.Calls())
	}
}

// An install with force over files the account cannot remove (root's, left
// by an unpack that broke off before 0.8.20) hands the docroot over and
// empties it again, instead of failing until somebody deletes them as root.
func TestCMSForceOverRootsLeftovers(t *testing.T) {
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})})
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	fixture := f.agent.RunAsHook
	rms := 0
	// The hook runs inside the fake agent's lock: it only counts, the order
	// of the calls is read from the call log afterwards.
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		switch req.Args[0] {
		case "list":
			if len(req.Args) > 1 && req.Args[1] == "data/www/wp.example.com" && rms == 0 {
				return &agent.RunAsUserResponse{StdoutBase64: b64(`[{"name":"bitrix","type":"dir"}]`)}
			}
		case "rm":
			rms++
			if rms == 1 {
				return &agent.RunAsUserResponse{ExitCode: 1, Stderr: "error: unlinkat data/www/wp.example.com/bitrix/.config.php: permission denied"}
			}
			return &agent.RunAsUserResponse{}
		}
		return fixture(req)
	}
	_, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog", "force": true})
	if job.Status != store.JobDone {
		t.Fatalf("install over root's leftovers: %s %s", job.Status, job.Error)
	}
	if rms < 2 {
		t.Fatalf("the docroot must be emptied again after the first refusal: %d rm calls", rms)
	}
	// Between the refused rm and the one that worked: the docroot handed over.
	seen, handed := 0, false
	for _, c := range f.agent.Calls() {
		switch c.Path {
		case "/v1/runas":
			var req agent.RunAsUserRequest
			if err := json.Unmarshal(c.Body, &req); err != nil {
				t.Fatal(err)
			}
			if req.Args[0] == "rm" {
				seen++
			}
		case "/v1/chown":
			var req agent.ChownRequest
			if err := json.Unmarshal(c.Body, &req); err != nil {
				t.Fatal(err)
			}
			if seen == 1 && req.Path == "/var/www/alex/data/www/wp.example.com" && req.Owner == "alex" && req.Recursive {
				handed = true
			}
		}
	}
	if !handed {
		t.Fatalf("the docroot must be handed over before it is emptied again: %+v", f.agent.Calls())
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// brokenFilesSource is a migration source whose files stream breaks off
// after a part of the archive.
type brokenFilesSource struct {
	migrateSource
	archive []byte
}

func (b brokenFilesSource) files(context.Context, migrateFilesRequest) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(bytes.NewReader(b.archive), failingReader{errors.New("ssh: connection lost")})), nil
}

// A migration whose files stream breaks off hands what tar unpacked into the
// home to the account, as a finished one does.
func TestMigrateFilesHandsTheFilesOverWhenTheStreamBreaks(t *testing.T) {
	f := newSiteFixture(t)
	u, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o750})                     //nolint:errcheck // test archive
	tw.WriteHeader(&tar.Header{Name: "data/a.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: 1 << 20}) //nolint:errcheck // test archive
	tw.Write(bytes.Repeat([]byte("a"), 4096))                                                          //nolint:errcheck // the rest never comes
	err = f.s.migrateFiles(f.ctx, nil, brokenFilesSource{archive: buf.Bytes()}, migratePayload{}, u, "home", "", "")
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("the break must fail the part with its reason: %v", err)
	}
	if _, handed := chownsAfterStream(t, f, "/var/www/alex", "alex"); !handed {
		t.Fatalf("what was unpacked must be handed to the account: %+v", f.agent.Calls())
	}
}
