package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

func (f *siteFixture) stackJob(t *testing.T, method, path string, body any) *store.Job {
	t.Helper()
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(method, path, body, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

// memcached: the package, the panel's config with the stored settings, the
// service; changing the settings rewrites the config and restarts it.
func TestMemcachedInstallAndSettings(t *testing.T) {
	f := newSiteFixture(t)
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "memcached"}); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	conf, _ := f.agent.File("/etc/memcached.conf")
	if !strings.Contains(conf, "-m 128") || !strings.Contains(conf, "-l 127.0.0.1") || !strings.Contains(conf, "-u memcache") {
		t.Fatalf("memcached.conf:\n%s", conf)
	}
	if act := f.agent.UnitAction("memcached.service"); act != "enable" {
		t.Fatalf("memcached.service not enabled: %q", act)
	}
	var st struct {
		Installed      bool
		MemoryMB       int `json:"memory_mb"`
		MaxConnections int `json:"max_connections"`
	}
	f.call(http.MethodPut, "/stack/memcached", map[string]any{"memory_mb": 512, "max_connections": 2048}, http.StatusOK, &st)
	if !st.Installed || st.MemoryMB != 512 || st.MaxConnections != 2048 {
		t.Fatalf("settings: %+v", st)
	}
	if conf, _ := f.agent.File("/etc/memcached.conf"); !strings.Contains(conf, "-m 512") || !strings.Contains(conf, "-c 2048") {
		t.Fatalf("settings not applied:\n%s", conf)
	}
	if act := f.agent.UnitAction("memcached.service"); act != "restart" {
		t.Fatalf("memcached not restarted after the settings change: %q", act)
	}
	f.call(http.MethodPut, "/stack/memcached", map[string]any{"memory_mb": 4, "max_connections": 2048}, http.StatusUnprocessableEntity, nil)
	// The listing knows the tools and marks them removable.
	var list []struct {
		Name      string
		Kind      string
		Installed bool
		Removable bool
	}
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &list)
	seen := map[string]bool{}
	for _, c := range list {
		if c.Kind == "tool" {
			seen[c.Name] = c.Removable
		}
	}
	for _, n := range []string{"memcached", "jpegoptim", "git", "composer"} {
		if !seen[n] {
			t.Fatalf("%s missing from the stack listing or not removable: %+v", n, list)
		}
	}
	if job := f.stackJob(t, http.MethodDelete, "/stack/memcached", nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s", job.Status, job.Error)
	}
	if act := f.agent.UnitAction("memcached.service"); act != "disable" {
		t.Fatalf("memcached.service not disabled on removal: %q", act)
	}
	f.call(http.MethodDelete, "/stack/nginx", nil, http.StatusUnprocessableEntity, nil)
}

// On EL memcached lives in /etc/sysconfig and jpegoptim comes from EPEL.
func TestToolsOnEL(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "memcached"}); job.Status != store.JobDone {
		t.Fatalf("memcached: %s %s", job.Status, job.Error)
	}
	if conf, _ := f.agent.File("/etc/sysconfig/memcached"); !strings.Contains(conf, `CACHESIZE="128"`) || !strings.Contains(conf, `USER="memcached"`) {
		t.Fatalf("sysconfig/memcached:\n%s", conf)
	}
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "jpegoptim"}); job.Status != store.JobDone {
		t.Fatalf("jpegoptim: %s %s", job.Status, job.Error)
	}
	epel := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/pkg" && strings.Contains(string(c.Body), "jpegoptim") && strings.Contains(string(c.Body), "epel-release") {
			epel = true
		}
	}
	if !epel {
		t.Fatal("jpegoptim on EL must pull EPEL in")
	}
}

// composer: the phar from getcomposer.org (checksum verified) under
// /usr/local/lib and a wrapper that runs it with the newest PHP branch;
// without PHP the install refuses; removal drops both files.
func TestComposerInstallAndRemove(t *testing.T) {
	phar := []byte("<?php echo 'composer';")
	prevLatest, prevDownload := composerLatest, composerDownload
	composerLatest = func(context.Context) (string, string, string, error) {
		return "2.9.0", "https://getcomposer.org/download/2.9.0/composer.phar", "8fbbc4f0d8ef0c9a4b1d8b0c1f6a1b8e5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9", nil
	}
	composerDownload = func(context.Context, string) ([]byte, error) { return phar, nil }
	t.Cleanup(func() { composerLatest, composerDownload = prevLatest, prevDownload })
	f := newSiteFixture(t)

	// A wrong checksum is refused.
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "composer"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "checksum") {
		t.Fatalf("bad checksum must fail the install: %s %s", job.Status, job.Error)
	}
	composerLatest = func(context.Context) (string, string, string, error) {
		return "2.9.0", "https://getcomposer.org/download/2.9.0/composer.phar", "", nil // no checksum published: accepted
	}
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "composer"}); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if got, _ := f.agent.File(composerPhar); got != string(phar) {
		t.Fatalf("phar not written verbatim: %q", got)
	}
	wrapper, _ := f.agent.File(composerBin)
	if !strings.Contains(wrapper, "/usr/bin/php8.4 "+composerPhar) {
		t.Fatalf("wrapper must run the phar with the installed PHP CLI:\n%s", wrapper)
	}
	f.agent.Stat[composerBin] = false
	var list []struct {
		Name      string
		Installed bool
		Version   string
	}
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &list)
	for _, c := range list {
		if c.Name == "composer" && (!c.Installed || c.Version != "2.9.0") {
			t.Fatalf("composer in the listing: %+v", c)
		}
	}
	if job := f.stackJob(t, http.MethodDelete, "/stack/composer", nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s", job.Status, job.Error)
	}
	removed := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/paths/remove" && strings.Contains(string(c.Body), composerBin) && strings.Contains(string(c.Body), composerPhar) {
			removed = true
		}
	}
	if !removed {
		t.Fatal("composer files were not removed")
	}
	_ = base64.StdEncoding
}

// Without a PHP branch composer has nothing to run on.
func TestComposerNeedsPHP(t *testing.T) {
	f := newSiteFixture(t)
	if err := f.db.DeletePHPVersion(f.ctx, "8.4"); err != nil {
		t.Fatal(err)
	}
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "composer"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "PHP") {
		t.Fatalf("composer without PHP: %s %s", job.Status, job.Error)
	}
}

// Sphinx for Bitrix: on Debian the distribution's Sphinx 2.2 with the
// daemon switched on in /etc/default, on EL Manticore from its repository;
// both get the bitrix real-time index and a local SphinxQL listener.
func TestSphinxInstall(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.ToolOutput["mysql"] = "bitrix\n"
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "sphinx"}); job.Status != store.JobDone {
		t.Fatalf("sphinx on Debian: %s %s", job.Status, job.Error)
	}
	conf, _ := f.agent.File("/etc/sphinxsearch/sphinx.conf")
	for _, want := range []string{"index bitrix", "type = rt", "rt_attr_multi = site", "rt_attr_timestamp = date_change", "listen = 127.0.0.1:9306:mysql41", "path = /var/lib/sphinxsearch/data/bitrix", "workers = threads"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("sphinx.conf lacks %q:\n%s", want, conf)
		}
	}
	if d, _ := f.agent.File("/etc/default/sphinxsearch"); !strings.Contains(d, "START=yes") {
		t.Fatalf("/etc/default/sphinxsearch: %q", d)
	}
	if act := f.agent.UnitAction("sphinxsearch.service"); act != "enable" {
		t.Fatalf("sphinxsearch.service: %q", act)
	}
	if job := f.stackJob(t, http.MethodDelete, "/stack/sphinx", nil); job.Status != store.JobDone {
		t.Fatalf("remove: %s %s", job.Status, job.Error)
	}
	if act := f.agent.UnitAction("sphinxsearch.service"); act != "disable" {
		t.Fatalf("sphinxsearch.service after removal: %q", act)
	}
}

func TestSphinxInstallOnEL(t *testing.T) {
	withOSRelease(t, "ID=rocky\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	// The daemon is asked whether it serves the index; a daemon that does
	// not answer with it fails the install.
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "sphinx"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "bitrix index") {
		t.Fatalf("an index that is not served must fail the install: %s %s", job.Status, job.Error)
	}
	f.agent.ToolOutput["mysql"] = "bitrix\n"
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "sphinx"}); job.Status != store.JobDone {
		t.Fatalf("manticore on EL: %s %s", job.Status, job.Error)
	}
	repo, pkg := false, false
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/pkg" {
			continue
		}
		if strings.Contains(string(c.Body), "manticore-repo.noarch.rpm") {
			repo = true
		}
		if strings.Contains(string(c.Body), `"manticore"`) {
			pkg = true
		}
	}
	if !repo || !pkg {
		t.Fatalf("manticore repository and package must be installed: repo=%v pkg=%v", repo, pkg)
	}
	conf, _ := f.agent.File("/etc/manticoresearch/manticore.conf")
	if !strings.Contains(conf, "index bitrix") || strings.Contains(conf, "workers =") || strings.Contains(conf, "docinfo") || !strings.Contains(conf, "path = /var/lib/manticore/bitrix") {
		t.Fatalf("manticore.conf:\n%s", conf)
	}
	if _, ok := f.agent.File("/etc/default/sphinxsearch"); ok {
		t.Fatal("Debian's defaults file has no place on EL")
	}
	var list []struct {
		Name      string
		Installed bool
		Version   string
	}
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &list)
	for _, c := range list {
		if c.Name == "sphinx" && (!c.Installed || !strings.HasPrefix(c.Version, "manticore ")) {
			t.Fatalf("sphinx in the listing: %+v", c)
		}
	}
}
