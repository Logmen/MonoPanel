package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Параметры сервера MySQL. Панель считает свои значения под объём памяти
// хоста и держит их в zz-monopanel.cnf; администратор переопределяет любые
// из разрешённых ключей в самой панели — значение встаёт на место панельного
// одной строкой, а не дописывается в чужой файл. Перед перезапуском конфиг
// проверяет mysqld --validate-config, а если сервер всё равно не поднялся,
// панель возвращает прежние значения и запускает его снова.

const settingDBConfig = "db.config"

// mysqldBinary validates the configuration before a restart; Percona and
// MySQL install it here on every supported OS.
const mysqldBinary = "/usr/sbin/mysqld"

// allowedDBKeys are the server variables the panel lets an administrator set.
// Left out on purpose: bind-address (the panel keeps MySQL on localhost),
// paths, authentication and replication settings, and variables that cannot
// change after the data directory exists (lower_case_table_names).
var allowedDBKeys = map[string]bool{
	"innodb_buffer_pool_size": true, "innodb_buffer_pool_instances": true, "innodb_redo_log_capacity": true,
	"innodb_log_buffer_size": true, "innodb_flush_log_at_trx_commit": true, "innodb_flush_method": true,
	"innodb_io_capacity": true, "innodb_io_capacity_max": true, "innodb_lock_wait_timeout": true,
	"innodb_strict_mode": true, "innodb_file_per_table": true, "innodb_open_files": true,
	"innodb_read_io_threads": true, "innodb_write_io_threads": true, "innodb_thread_concurrency": true,
	"innodb_print_all_deadlocks": true, "innodb_adaptive_hash_index": true, "innodb_ft_min_token_size": true,
	"innodb_ft_max_token_size": true, "innodb_sort_buffer_size": true,
	"max_connections": true, "max_user_connections": true, "max_connect_errors": true,
	"wait_timeout": true, "interactive_timeout": true, "connect_timeout": true,
	"net_read_timeout": true, "net_write_timeout": true, "max_execution_time": true,
	"table_open_cache": true, "table_definition_cache": true, "open_files_limit": true, "thread_cache_size": true,
	"tmp_table_size": true, "max_heap_table_size": true, "max_allowed_packet": true,
	"sort_buffer_size": true, "join_buffer_size": true, "read_buffer_size": true, "read_rnd_buffer_size": true,
	"key_buffer_size": true, "bulk_insert_buffer_size": true, "group_concat_max_len": true,
	"sql_mode": true, "transaction_isolation": true, "character_set_server": true, "collation_server": true,
	"default_time_zone": true, "local_infile": true, "skip_name_resolve": true, "event_scheduler": true,
	"slow_query_log": true, "long_query_time": true, "log_queries_not_using_indexes": true,
	"performance_schema": true, "ft_min_word_len": true, "ft_max_word_len": true, "optimizer_switch": true,
}

// dbValueRe admits sizes (256M), numbers, words, comma lists (sql_mode),
// optimizer_switch pairs and time zones; ” sets an empty value.
var dbValueRe = regexp.MustCompile(`^([A-Za-z0-9_.,:+/=*@-]{1,255}|'')$`)

// dbKey is the canonical spelling: MySQL reads character-set-server and
// character_set_server as the same option.
func dbKey(k string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "-", "_")
}

// tzOffsetRe is what default_time_zone may be without the time zone tables:
// a named zone passes mysqld --validate-config and then stops the server
// from starting when the tables are not loaded.
var tzOffsetRe = regexp.MustCompile(`^([+-]\d{2}:\d{2}|SYSTEM)$`)

// validateDBSettings checks keys and values; ramMB (when known) bounds the
// buffer pool, which --validate-config does not look at either.
func validateDBSettings(m map[string]string, ramMB int) error {
	for k, v := range m {
		key := dbKey(k)
		if !allowedDBKeys[key] {
			return fmt.Errorf("settings: key %q cannot be set in the panel", k)
		}
		if v == "" {
			continue
		}
		if !dbValueRe.MatchString(v) {
			return fmt.Errorf("settings: invalid value for %q", k)
		}
		switch key {
		case "default_time_zone":
			if !tzOffsetRe.MatchString(v) {
				return fmt.Errorf("settings: default_time_zone must be an offset like +05:00 or SYSTEM: named zones need the time zone tables, and without them MySQL does not start")
			}
		case "innodb_buffer_pool_size":
			if n, ok := parseDBSize(v); ok && ramMB > 0 && n > int64(ramMB)<<20 {
				return fmt.Errorf("settings: innodb_buffer_pool_size %s is larger than the server's memory (%d MB)", v, ramMB)
			}
		}
	}
	return nil
}

// parseDBSize reads a MySQL size: bytes or a number with K, M, G or T.
func parseDBSize(v string) (int64, bool) {
	mult := int64(1)
	switch strings.ToUpper(v[len(v)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	num := v
	if mult != 1 {
		num = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

// dbOverrides are the values the administrator set.
func (s *Server) dbOverrides(ctx context.Context) map[string]string {
	out := map[string]string{}
	if raw, _ := s.db.GetSetting(ctx, settingDBConfig); raw != "" {
		json.Unmarshal([]byte(raw), &out) //nolint:errcheck // a broken value reads as no overrides
	}
	return out
}

// dbDefaults are the panel's own settings for a host with this much memory.
func dbDefaults(ramMB int, slowLog string) []render.MySQLSetting {
	pool := max(ramMB/4, 128)
	redo := min(max(pool/4, 64), 2048)
	conns := min(100+ramMB/64, 1000)
	return []render.MySQLSetting{
		{Key: "bind-address", Value: "127.0.0.1"},
		{Key: "character-set-server", Value: "utf8mb4"},
		{Key: "collation-server", Value: "utf8mb4_0900_ai_ci"},
		{Key: "innodb_buffer_pool_size", Value: fmt.Sprintf("%dM", pool)},
		{Key: "innodb_redo_log_capacity", Value: fmt.Sprintf("%dM", redo)},
		{Key: "innodb_flush_method", Value: "O_DIRECT"},
		{Key: "innodb_flush_log_at_trx_commit", Value: "2"},
		{Key: "max_connections", Value: fmt.Sprint(conns)},
		{Key: "table_open_cache", Value: "4000"},
		{Key: "tmp_table_size", Value: "64M"},
		{Key: "max_heap_table_size", Value: "64M"},
		{Key: "local_infile", Value: "OFF"},
		{Key: "innodb_strict_mode", Value: "OFF", Comment: []string{
			"Strict mode refuses tables whose row size exceeds the InnoDB limit and",
			"imports from older servers; CMS installers and dumps hit both, so it is off."}},
		{Key: "transaction_isolation", Value: "READ-COMMITTED", Comment: []string{
			"READ-COMMITTED and a lenient sql_mode are what 1C-Bitrix requires and what",
			"WordPress, Joomla and OpenCart run on anyway."}},
		{Key: "sql_mode", Value: "NO_ENGINE_SUBSTITUTION"},
		{Key: "max_allowed_packet", Value: "64M"},
		{Key: "thread_cache_size", Value: "32"},
		{Key: "sort_buffer_size", Value: "2M"},
		{Key: "join_buffer_size", Value: "2M"},
		{Key: "read_rnd_buffer_size", Value: "1M"},
		{Key: "performance_schema", Value: "ON"},
		{Key: "disable_log_bin", Flag: true},
		{Key: "slow_query_log", Value: "ON"},
		{Key: "slow_query_log_file", Value: slowLog},
		{Key: "long_query_time", Value: "2"},
	}
}

// dbSettings folds the administrator's values into the panel's: a key the
// panel sets keeps its place and says what the panel's value was, the rest
// follow in alphabetical order. The view lists every line with its source.
func dbSettings(defaults []render.MySQLSetting, overrides map[string]string) ([]render.MySQLSetting, []apitypes.DBParam) {
	custom := map[string]string{}
	for k, v := range overrides {
		custom[dbKey(k)] = v
	}
	out := make([]render.MySQLSetting, 0, len(defaults)+len(custom))
	view := make([]apitypes.DBParam, 0, len(defaults)+len(custom))
	seen := map[string]bool{}
	for _, d := range defaults {
		key := dbKey(d.Key)
		seen[key] = true
		src := "default"
		if v, ok := custom[key]; ok && allowedDBKeys[key] {
			d.Comment = []string{"set in the panel; the panel's value: " + d.Value}
			d.Value, d.Flag, src = v, false, "custom"
		}
		out = append(out, d)
		view = append(view, apitypes.DBParam{Key: key, Value: d.Value, Source: src})
	}
	var extra []string
	for k := range custom {
		if !seen[k] && allowedDBKeys[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for i, k := range extra {
		st := render.MySQLSetting{Key: k, Value: custom[k]}
		if i == 0 {
			st.Comment = []string{"Set in the panel."}
		}
		out = append(out, st)
		view = append(view, apitypes.DBParam{Key: k, Value: custom[k], Source: "custom"})
	}
	return out, view
}

func allowedDBKeysSorted() []string {
	keys := make([]string, 0, len(allowedDBKeys))
	for k := range allowedDBKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// dbConfigView is what GET /db/engine/config shows for the running host.
func (s *Server) dbConfigView(ctx context.Context, inst *store.DBInstance) (apitypes.DBConfig, error) {
	ramMB, slowLog, err := s.dbHost(ctx, inst)
	if err != nil {
		return apitypes.DBConfig{}, err
	}
	_, view := dbSettings(dbDefaults(ramMB, slowLog), s.dbOverrides(ctx))
	return apitypes.DBConfig{Allowed: allowedDBKeysSorted(), Values: view, RAMMB: ramMB}, nil
}

type dbConfigOutput struct {
	Body apitypes.DBConfig
}

type dbConfigInput struct {
	Body apitypes.DBConfigRequest
}

func (s *Server) registerDBConfig() {
	huma.Register(s.api, huma.Operation{
		OperationID: "db-config-get", Method: http.MethodGet, Path: "/db/engine/config", Summary: "MySQL server settings: the panel's values and the ones set by the administrator", Tags: []string{"db"},
		Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*dbConfigOutput, error) {
		inst, err := s.dbInstance(ctx)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		view, err := s.dbConfigView(ctx, inst)
		if err != nil {
			return nil, err
		}
		return &dbConfigOutput{Body: view}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "db-config-set", Method: http.MethodPut, Path: "/db/engine/config", Summary: "Change MySQL server settings and restart the server (the previous settings come back if it does not start)", Tags: []string{"db"},
		Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *dbConfigInput) (*dbConfigOutput, error) {
		p := principalFrom(ctx)
		inst, err := s.dbInstance(ctx)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if len(in.Body.Settings) == 0 {
			return nil, huma.Error422UnprocessableEntity("settings: nothing to change")
		}
		ramMB, _, err := s.dbHost(ctx, inst)
		if err != nil {
			return nil, err
		}
		if err := validateDBSettings(in.Body.Settings, ramMB); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		prev := s.dbOverrides(ctx)
		next := map[string]string{}
		for k, v := range prev {
			next[dbKey(k)] = v
		}
		for k, v := range in.Body.Settings {
			if v == "" {
				delete(next, dbKey(k))
			} else {
				next[dbKey(k)] = v
			}
		}
		if err := s.saveDBOverrides(ctx, next); err != nil {
			return nil, err
		}
		if err := s.writeDBConfig(ctx, inst, true); err != nil {
			if serr := s.saveDBOverrides(ctx, prev); serr != nil {
				s.log.Warn("db config: restoring the previous settings", "err", serr)
			}
			// mysqld refused the file: the agent already put the old one back
			// and the running server was never touched — nothing to restart.
			var ae *agent.Error
			if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
				return nil, huma.Error422UnprocessableEntity("MySQL refused the settings, nothing was changed: " + firstLines(ae.Output, 3))
			}
			// The file was valid but the server did not start with it: the
			// server must not stay down over a setting — back to what ran.
			if rerr := s.writeDBConfig(ctx, inst, true); rerr != nil {
				s.log.Error("db config: the previous settings did not start either", "err", rerr)
				return nil, huma.Error502BadGateway("MySQL did not start with the new settings, nor with the previous ones: " + rerr.Error())
			}
			return nil, huma.Error422UnprocessableEntity("MySQL did not start with the new settings; the previous ones are back and it runs again: " + err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "db.config", Target: inst.Engine, IP: requestInfo(ctx).IP, Details: map[string]any{"settings": in.Body.Settings}})
		view, err := s.dbConfigView(ctx, inst)
		if err != nil {
			return nil, err
		}
		return &dbConfigOutput{Body: view}, nil
	})
}

func (s *Server) saveDBOverrides(ctx context.Context, m map[string]string) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, settingDBConfig, string(raw))
}

// firstLines keeps the part of a tool's output worth showing: mysqld prints
// warnings before the error, so the lines with ERROR come first.
func firstLines(out string, n int) string {
	var errs, rest []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if strings.Contains(l, "[ERROR]") {
			errs = append(errs, l)
		} else {
			rest = append(rest, l)
		}
	}
	lines := append(errs, rest...)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}
