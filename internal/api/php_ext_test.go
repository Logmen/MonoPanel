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
