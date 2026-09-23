package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// PHP-параметры сайта собираются из четырёх слоёв, от общего к частному:
//
//	default — значения самой панели (memory_limit 256M, display_errors Off…);
//	global  — заданные администратором на весь сервер;
//	preset  — требования CMS-пресета сайта (Битриксу нужно 512M и short_open_tag);
//	site    — заданные для этого сайта.
//
// Каждый следующий слой перекрывает предыдущий по ключу, так что любой
// параметр меняется либо для одного сайта, либо для всех сразу.

const settingPHPGlobal = "php.ini.global"

// panelIniDefaults are the panel's own values, written into every pool.
func (s *Server) panelIniDefaults(ctx context.Context) []apitypes.PHPValue {
	tz, _ := s.db.GetSetting(ctx, settingTZ)
	if tz == "" {
		tz = "UTC"
	}
	return []apitypes.PHPValue{
		{Key: "memory_limit", Value: "256M"}, {Key: "upload_max_filesize", Value: "64M"}, {Key: "post_max_size", Value: "64M"},
		{Key: "max_execution_time", Value: "120"}, {Key: "date.timezone", Value: tz}, {Key: "display_errors", Value: "Off"},
		// Set here, not left to the global ini: the CLI copy of it opens
		// short tags for 1C-Bitrix, and on Remi FPM reads the same file.
		{Key: "short_open_tag", Value: "Off"},
	}
}

// globalIni is the server-wide layer the administrator set.
func (s *Server) globalIni(ctx context.Context) map[string]string {
	out := map[string]string{}
	if raw, _ := s.db.GetSetting(ctx, settingPHPGlobal); raw != "" {
		json.Unmarshal([]byte(raw), &out) //nolint:errcheck // a broken value reads as no global layer
	}
	return out
}

// presetValues is the preset layer of a site.
func presetValues(site *store.Site, tls bool) map[string]string {
	preset := map[string]string{}
	for k, v := range presetIni[site.Preset] {
		preset[k] = v
	}
	// A secure-only session cookie needs HTTPS to exist: on a site that is
	// still on HTTP the login would not stick.
	if site.Preset == presetBitrix && tls {
		preset["session.cookie_secure"] = "On"
	}
	return preset
}

// layerValues folds the layers over the panel's defaults: the defaults keep
// their order, keys the defaults lack follow sorted; every value carries the
// layer it came from.
func layerValues(defaults []apitypes.PHPValue, layers ...struct {
	name   string
	values map[string]string
}) []apitypes.PHPValue {
	out := make([]apitypes.PHPValue, 0, len(defaults))
	seen := map[string]bool{}
	resolve := func(k string) (string, string, bool) {
		val, src, ok := "", "", false
		for _, l := range layers {
			if v, has := l.values[k]; has {
				val, src, ok = v, l.name, true
			}
		}
		return val, src, ok
	}
	for _, d := range defaults {
		d.Source = "default"
		if v, src, ok := resolve(d.Key); ok {
			d.Value, d.Source = v, src
		}
		seen[d.Key] = true
		out = append(out, d)
	}
	var extra []string
	for _, l := range layers {
		for k := range l.values {
			if !seen[k] {
				seen[k] = true
				extra = append(extra, k)
			}
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		v, src, _ := resolve(k)
		out = append(out, apitypes.PHPValue{Key: k, Value: v, Source: src})
	}
	return out
}

type layer = struct {
	name   string
	values map[string]string
}

// sitePHPValues are the effective php.ini values of a site with their source.
func (s *Server) sitePHPValues(ctx context.Context, site *store.Site, tls bool) []apitypes.PHPValue {
	return layerValues(s.panelIniDefaults(ctx),
		layer{"global", s.globalIni(ctx)},
		layer{"preset", presetValues(site, tls)},
		layer{"site", site.PHPIni},
	)
}

func (s *Server) globalPHP(ctx context.Context) apitypes.GlobalPHP {
	out := apitypes.GlobalPHP{Allowed: allowedKeysSorted()}
	out.Values = layerValues(s.panelIniDefaults(ctx), layer{"global", s.globalIni(ctx)})
	return out
}

func allowedKeysSorted() []string {
	keys := make([]string, 0, len(allowedIniKeys))
	for k := range allowedIniKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type globalPHPOutput struct {
	Body apitypes.GlobalPHP
}

type globalPHPInput struct {
	Body apitypes.GlobalPHPRequest
}

type globalPHPResultOutput struct {
	Body apitypes.GlobalPHPResult
}

func (s *Server) registerPHPGlobal() {
	huma.Register(s.api, huma.Operation{
		OperationID: "php-settings-get", Method: http.MethodGet, Path: "/php/settings", Summary: "Server-wide php.ini values every site inherits", Tags: []string{"php"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*globalPHPOutput, error) {
		return &globalPHPOutput{Body: s.globalPHP(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "php-settings-set", Method: http.MethodPut, Path: "/php/settings", Summary: "Change server-wide php.ini values and re-apply every PHP site", Tags: []string{"php"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *globalPHPInput) (*globalPHPResultOutput, error) {
		p := principalFrom(ctx)
		if len(in.Body.PHPIni) == 0 {
			return nil, huma.Error422UnprocessableEntity("php_ini: nothing to change")
		}
		if err := validateIni(in.Body.PHPIni); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		cur := s.globalIni(ctx)
		for k, v := range in.Body.PHPIni {
			if v == "" {
				delete(cur, k)
			} else {
				cur[k] = v
			}
		}
		raw, _ := json.Marshal(cur)
		if err := s.db.SetSetting(ctx, settingPHPGlobal, string(raw)); err != nil {
			return nil, err
		}
		// Every PHP site gets the new layer in its pool now, not at its
		// next unrelated change.
		sites, err := s.db.ListSites(ctx, 0)
		if err != nil {
			return nil, err
		}
		out := apitypes.GlobalPHPResult{Jobs: []int64{}}
		for _, site := range sites {
			if site.Mode == store.ModeProxy {
				continue
			}
			id, err := s.enqueueSiteApply(ctx, site, p.Login)
			if err != nil {
				return nil, err
			}
			out.Jobs = append(out.Jobs, id)
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "php.settings", IP: requestInfo(ctx).IP, Details: map[string]any{"php_ini": in.Body.PHPIni, "sites": len(out.Jobs)}})
		out.Settings = s.globalPHP(ctx)
		return &globalPHPResultOutput{Body: out}, nil
	})
}
