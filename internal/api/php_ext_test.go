package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
)

// Расширения ветки: список берётся из mods-available, состояние — из phpquery.
func TestPHPExtensionsListAndToggle(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.DirEntries["/etc/php/8.4/mods-available"] = []string{"opcache.ini", "imagick.ini", "redis.ini", "README"}
	f.agent.DirEntries["/etc/php/8.4/fpm/conf.d"] = []string{"10-opcache.ini", "20-imagick.ini"}

	var out apitypes.PHPExtensions
	f.call(http.MethodGet, "/php/versions/8.4/extensions", nil, http.StatusOK, &out)
	if len(out.Extensions) != 3 {
		t.Fatalf("не .ini попал в список: %+v", out.Extensions)
	}
	state := map[string]apitypes.PHPExtension{}
	for _, e := range out.Extensions {
		state[e.Name] = e
	}
	if !state["imagick"].Enabled || state["redis"].Enabled {
		t.Fatalf("состояние не из phpquery: %+v", out.Extensions)
	}
	if !state["opcache"].Critical || state["redis"].Critical {
		t.Fatalf("пометка о критичности: %+v", out.Extensions)
	}

	// Выключение зовёт phpdismod и перезапускает php-fpm ветки.
	f.agent.DirEntries["/etc/php/8.4/fpm/conf.d"] = []string{"10-opcache.ini"}
	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "imagick", "enabled": false}, http.StatusOK, &out)
	dismod := false
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/tool" && strings.Contains(string(c.Body), "phpdismod") && strings.Contains(string(c.Body), "imagick") {
			dismod = true
		}
	}
	if !dismod {
		t.Fatalf("phpdismod не вызван: %v", f.agent.Calls())
	}
	if act := f.agent.UnitAction("php8.4-fpm.service"); act != "reload-or-restart" {
		t.Fatalf("php-fpm не перезапущен: %q", act)
	}
	for _, e := range out.Extensions {
		if e.Name == "imagick" && e.Enabled {
			t.Fatal("ответ должен показывать новое состояние")
		}
	}

	// Несуществующее расширение и неустановленная ветка.
	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "nosuch", "enabled": true}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodGet, "/php/versions/7.4/extensions", nil, http.StatusNotFound, nil)
}

// On EL the extensions are the ini files of the branch's php.d: switching one
// off comments its extension= line out and keeps the rest of the file.
func TestPHPExtensionsOnEL(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	dir := "/etc/opt/remi/php84/php.d"
	f.agent.DirEntries[dir] = []string{"10-opcache.ini", "40-imagick.ini", "50-memcached.ini", "99-monopanel.ini"}
	f.agent.ReadFile[dir+"/10-opcache.ini"] = "zend_extension=opcache\n"
	f.agent.ReadFile[dir+"/40-imagick.ini"] = "; Enable imagick extension module\nextension = imagick.so\n\nimagick.skip_version_check=1\n"
	f.agent.ReadFile[dir+"/50-memcached.ini"] = "; extension=memcached.so ; switched off in MonoPanel\n"

	var out struct {
		Extensions []struct {
			Name    string
			Enabled bool
		}
	}
	f.call(http.MethodGet, "/php/versions/8.4/extensions", nil, http.StatusOK, &out)
	got := map[string]bool{}
	for _, e := range out.Extensions {
		got[e.Name] = e.Enabled
	}
	if len(out.Extensions) != 3 || !got["opcache"] || !got["imagick"] || got["memcached"] {
		t.Fatalf("EL extensions: %+v", out.Extensions)
	}

	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "imagick", "enabled": false}, http.StatusOK, &out)
	ini, _ := f.agent.File(dir + "/40-imagick.ini")
	if !strings.Contains(ini, "; extension = imagick.so ; switched off in MonoPanel") || !strings.Contains(ini, "imagick.skip_version_check=1") {
		t.Fatalf("imagick.ini after switching off:\n%s", ini)
	}
	if act := f.agent.UnitAction("php84-php-fpm.service"); act != "reload-or-restart" {
		t.Fatalf("php-fpm not restarted: %q", act)
	}
	for _, e := range out.Extensions {
		if e.Name == "imagick" && e.Enabled {
			t.Fatal("imagick still reported enabled")
		}
	}
	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "memcached", "enabled": true}, http.StatusOK, &out)
	if ini, _ := f.agent.File(dir + "/50-memcached.ini"); !strings.HasPrefix(ini, "extension=memcached.so\n") {
		t.Fatalf("memcached.ini after switching on must lose the marker:\n%s", ini)
	}
	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "nosuch", "enabled": true}, http.StatusUnprocessableEntity, nil)
}
