package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
)

// Every PHP parameter can be changed for the whole server: the global layer
// replaces the panel's default on every site, a preset still wins for the
// keys it needs, and a site's own value wins over all of them. Saving the
// global layer re-applies every PHP site, so the pools change at once.
func TestGlobalPHPLayer(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	f.createSite(map[string]any{"domain": "plain.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "bx.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "preset": "bitrix"})
	f.createSite(map[string]any{"domain": "own.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "php_ini": map[string]string{"display_errors": "On"}})

	var res apitypes.GlobalPHPResult
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"memory_limit": "1G", "display_errors": "Off", "max_input_vars": "8000"}}, http.StatusOK, &res)
	if len(res.Jobs) != 3 {
		t.Fatalf("one site.apply per PHP site: %v", res.Jobs)
	}
	for _, id := range res.Jobs {
		if job := f.waitJob(id); job.Status != "done" {
			t.Fatalf("job %d: %s %s", id, job.Status, job.Error)
		}
	}
	src := func(vals []apitypes.PHPValue, key string) (string, string) {
		for _, v := range vals {
			if v.Key == key {
				return v.Value, v.Source
			}
		}
		return "", ""
	}
	if v, s := src(res.Settings.Values, "memory_limit"); v != "1G" || s != "global" {
		t.Errorf("global memory_limit: %s %s", v, s)
	}
	if v, s := src(res.Settings.Values, "upload_max_filesize"); v != "64M" || s != "default" {
		t.Errorf("untouched default: %s %s", v, s)
	}

	get := func(domain string) []apitypes.PHPValue {
		var out apitypes.SitePHP
		f.call(http.MethodGet, "/sites/"+domain+"/php", nil, http.StatusOK, &out)
		return out.Values
	}
	for _, c := range []struct{ domain, key, value, source string }{
		{"plain.example.com", "memory_limit", "1G", "global"},
		{"plain.example.com", "max_input_vars", "8000", "global"},
		{"plain.example.com", "post_max_size", "64M", "default"},
		{"bx.example.com", "memory_limit", "512M", "preset"},
		{"bx.example.com", "max_input_vars", "20000", "preset"},
		{"own.example.com", "display_errors", "On", "site"},
		{"own.example.com", "memory_limit", "1G", "global"},
	} {
		if v, s := src(get(c.domain), c.key); v != c.value || s != c.source {
			t.Errorf("%s %s = %s (%s), want %s (%s)", c.domain, c.key, v, s, c.value, c.source)
		}
	}
	pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/plain.example.com.conf")
	if !strings.Contains(pool, "php_value[memory_limit] = 1G") || !strings.Contains(pool, "php_value[max_input_vars] = 8000") {
		t.Fatalf("pool did not get the global values:\n%s", pool)
	}

	// An empty value hands the key back to the panel's default.
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"memory_limit": ""}}, http.StatusOK, &res)
	for _, id := range res.Jobs {
		f.waitJob(id)
	}
	if v, s := src(get("plain.example.com"), "memory_limit"); v != "256M" || s != "default" {
		t.Errorf("after reset: %s %s", v, s)
	}
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"disable_functions": ""}}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"memory_limit": "1G; rm -rf"}}, http.StatusUnprocessableEntity, nil)

	// Only an administrator changes the server.
	alex, _ := f.db.GetUserByLogin(f.ctx, "alex")
	h, _ := auth.HashPassword("alex-password-1")
	if err := f.db.SetUserPassword(f.ctx, alex.ID, h); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password-1")
	f.call(http.MethodGet, "/php/settings", nil, http.StatusForbidden, nil)
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"memory_limit": "2G"}}, http.StatusForbidden, nil)
}
