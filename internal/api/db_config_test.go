package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
)

// lastDBConfig is the last zz-monopanel.cnf the panel applied and its request.
func lastDBConfig(t *testing.T, f *siteFixture) (string, agent.ApplyConfigSetRequest) {
	t.Helper()
	var conf string
	var last agent.ApplyConfigSetRequest
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/config/apply" {
			continue
		}
		var req agent.ApplyConfigSetRequest
		if err := json.Unmarshal(c.Body, &req); err != nil {
			t.Fatal(err)
		}
		for _, file := range req.Files {
			if strings.HasSuffix(file.Path, "zz-monopanel.cnf") {
				conf, last = file.Content, req
			}
		}
	}
	if conf == "" {
		t.Fatal("zz-monopanel.cnf was never applied")
	}
	return conf, last
}

// MySQL server settings are changed in the panel: an administrator's value
// takes the place of the panel's in zz-monopanel.cnf (once, with a note of
// what the panel had), mysqld validates the file before the server restarts,
// and a setting the server does not take puts the previous ones back.
func TestDBConfigInThePanel(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	f.call(http.MethodGet, "/db/engine/config", nil, http.StatusUnprocessableEntity, nil) // no server yet
	f.installDB()
	f.agent.MemTotalBytes = 4 << 30

	var cfg apitypes.DBConfig
	f.call(http.MethodGet, "/db/engine/config", nil, http.StatusOK, &cfg)
	val := func(c apitypes.DBConfig, key string) (string, string) {
		for _, v := range c.Values {
			if v.Key == key {
				return v.Value, v.Source
			}
		}
		return "", ""
	}
	if v, s := val(cfg, "innodb_buffer_pool_size"); v != "1024M" || s != "default" || cfg.RAMMB != 4096 {
		t.Fatalf("panel default for 4 GB: %s %s (%d MB)", v, s, cfg.RAMMB)
	}

	f.call(http.MethodPut, "/db/engine/config", map[string]any{"settings": map[string]string{
		"innodb_buffer_pool_size": "2G", "sql_mode": "''", "wait_timeout": "600", "character-set-server": "utf8mb4", "default_time_zone": "+05:00",
	}}, http.StatusOK, &cfg)
	conf, req := lastDBConfig(t, f)
	for _, want := range []string{
		"# set in the panel; the panel's value: 1024M\ninnodb_buffer_pool_size = 2G",
		"sql_mode = ''",
		"# Set in the panel.\ndefault_time_zone = +05:00\nwait_timeout = 600",
		"character-set-server = utf8mb4",
		"bind-address = 127.0.0.1",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("zz-monopanel.cnf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Count(conf, "innodb_buffer_pool_size") != 1 {
		t.Errorf("a changed key must appear once:\n%s", conf)
	}
	if len(req.Validate) != 1 || strings.Join(req.Validate[0], " ") != "/usr/sbin/mysqld --validate-config --user=mysql" {
		t.Errorf("no mysqld validation before the restart: %v", req.Validate)
	}
	if act := f.agent.UnitAction("mysql.service"); act != "restart" {
		t.Errorf("mysql not restarted: %q", act)
	}
	if v, s := val(cfg, "wait_timeout"); v != "600" || s != "custom" {
		t.Errorf("view: wait_timeout %s %s", v, s)
	}
	if v, s := val(cfg, "character_set_server"); v != "utf8mb4" || s != "custom" {
		t.Errorf("view: the dashed spelling is the same option: %s %s", v, s)
	}

	// Keys the panel owns or that cannot change on a live server, and values
	// that could smuggle another line in, are refused.
	for _, bad := range []map[string]string{
		{"bind-address": "0.0.0.0"}, {"lower_case_table_names": "1"}, {"datadir": "/tmp"},
		{"wait_timeout": "600\nskip-grant-tables"}, {"sql_mode": "a b"},
		// what mysqld --validate-config lets through and the server then does not start with
		{"default_time_zone": "Europe/Nowhere"}, {"innodb_buffer_pool_size": "8G"},
	} {
		f.call(http.MethodPut, "/db/engine/config", map[string]any{"settings": bad}, http.StatusUnprocessableEntity, nil)
	}

	// A value the server does not take: the previous settings come back.
	applies := func() int {
		n := 0
		for _, c := range f.agent.Calls() {
			if c.Path == "/v1/config/apply" {
				n++
			}
		}
		return n
	}
	before := applies()
	f.agent.Fail["/v1/config/apply"] = "files applied but restart failed"
	f.call(http.MethodPut, "/db/engine/config", map[string]any{"settings": map[string]string{"innodb_flush_log_at_trx_commit": "1"}}, http.StatusBadGateway, nil)
	delete(f.agent.Fail, "/v1/config/apply")
	if got := f.s.dbOverrides(f.ctx); got["innodb_flush_log_at_trx_commit"] != "" || got["wait_timeout"] != "600" {
		t.Fatalf("the previous settings are not back: %v", got)
	}
	if applies()-before != 2 {
		t.Fatalf("a failed start must be followed by the previous settings: %d applies", applies()-before)
	}

	// A file mysqld refuses never reaches the running server: the agent keeps
	// the old one, so there is nothing to rewrite or restart.
	before = applies()
	f.agent.Fail["/v1/config/apply"] = "[ERROR] [MY-000077] [Server] /usr/sbin/mysqld: Error while setting value 'BOGUS' to 'innodb_flush_method'"
	f.agent.FailStatus = map[string]int{"/v1/config/apply": http.StatusUnprocessableEntity}
	f.call(http.MethodPut, "/db/engine/config", map[string]any{"settings": map[string]string{"innodb_flush_method": "BOGUS"}}, http.StatusUnprocessableEntity, nil)
	delete(f.agent.Fail, "/v1/config/apply")
	f.agent.FailStatus = nil
	if applies()-before != 1 {
		t.Fatalf("a refused file must not be followed by another write and restart: %d applies", applies()-before)
	}
	if got := f.s.dbOverrides(f.ctx); got["innodb_flush_method"] != "" {
		t.Fatalf("the refused value stayed in the settings: %v", got)
	}

	// An empty value hands the key back to the panel.
	f.call(http.MethodPut, "/db/engine/config", map[string]any{"settings": map[string]string{"innodb_buffer_pool_size": "", "wait_timeout": ""}}, http.StatusOK, &cfg)
	if v, s := val(cfg, "innodb_buffer_pool_size"); v != "1024M" || s != "default" {
		t.Errorf("after reset: %s %s", v, s)
	}
	conf, _ = lastDBConfig(t, f)
	if strings.Contains(conf, "wait_timeout") || strings.Contains(conf, "the panel's value: 1024M") {
		t.Errorf("reset keys linger:\n%s", conf)
	}

	// tune keeps the administrator's settings.
	f.call(http.MethodPost, "/db/engine/tune", nil, http.StatusOK, nil)
	if conf, _ := lastDBConfig(t, f); !strings.Contains(conf, "sql_mode = ''") {
		t.Errorf("tune dropped a custom setting:\n%s", conf)
	}
}
