package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/render"
)

// CMS presets: an nginx location set that matches the application's routing
// and hardening rules, plus PHP defaults the application expects. The preset
// replaces the generic php-fpm locations; custom.conf still applies.

const (
	presetWordPress = "wordpress"
	presetJoomla    = "joomla"
	presetBitrix    = "bitrix"
	presetOpenCart  = "opencart"
)

var sitePresets = []apitypes.SitePreset{
	{ID: "", Name: "Универсальный PHP", Description: "index.php front controller, статика через nginx; подходит для Laravel, Symfony и любого PHP-приложения."},
	{ID: presetWordPress, Name: "WordPress", Description: "ЧПУ-ссылки, /wp-admin, xmlrpc.php закрыт, запрет PHP в wp-content/uploads, лимиты загрузки 128M."},
	{ID: presetJoomla, Name: "Joomla", Description: "SEF-ссылки, /api для Joomla 4+, закрыты configuration.php, cache/logs/tmp, запрет PHP в images/media."},
	{ID: presetBitrix, Name: "1С-Битрикс", Description: "urlrewrite.php, правила BitrixVM для bitrix/ и upload/, short_open_tag, max_input_vars 20000, лимиты 256M, memory 512M."},
	{ID: presetOpenCart, Name: "OpenCart", Description: "SEO URL через _route_, sitemap/googlebase, закрыты system/ и storage/, .tpl/.twig/.log не отдаются."},
}

// presetIni is applied under the panel defaults and above nothing else: site
// overrides (php_ini) still win.
var presetIni = map[string]map[string]string{
	presetWordPress: {"upload_max_filesize": "128M", "post_max_size": "128M", "max_execution_time": "300", "max_input_vars": "5000"},
	presetJoomla:    {"upload_max_filesize": "64M", "post_max_size": "64M", "max_execution_time": "300", "max_input_vars": "5000", "output_buffering": "Off"},
	presetBitrix: {
		"memory_limit": "512M", "upload_max_filesize": "256M", "post_max_size": "256M", "max_execution_time": "600", "max_input_vars": "20000",
		"short_open_tag": "On", "mbstring.internal_encoding": "UTF-8", "default_charset": "UTF-8", "opcache.revalidate_freq": "0",
		"realpath_cache_size": "4096k", "pcre.backtrack_limit": "1000000", "pcre.recursion_limit": "14000",
	},
	presetOpenCart: {"upload_max_filesize": "64M", "post_max_size": "64M", "max_execution_time": "300"},
}

// presetTemplates lists the template files that define preset-* blocks.
func (s *Server) presetTemplates() ([]string, error) {
	names, err := s.render.ListDir("nginx/presets")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "nginx/presets/"+n)
	}
	return out, nil
}

func normalizePreset(p string) (string, error) {
	if p == "" || p == "none" {
		return "", nil
	}
	for _, sp := range sitePresets {
		if sp.ID == p {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown preset %q (wordpress, joomla, bitrix, opencart)", p)
}

func presetTitle(id string) string {
	for _, sp := range sitePresets {
		if sp.ID == id {
			return sp.Name
		}
	}
	return id
}

// renderSiteNginx renders a site server block with the preset blocks available.
func (s *Server) renderSiteNginx(tmpl string, rs render.Site) (string, error) {
	extra, err := s.presetTemplates()
	if err != nil {
		return "", err
	}
	return s.render.RenderSet(tmpl, rs, extra...)
}

type presetsOutput struct {
	Body []apitypes.SitePreset
}

func (s *Server) registerPresets() {
	huma.Register(s.api, huma.Operation{
		OperationID: "sites-presets", Method: http.MethodGet, Path: "/sites/presets", Summary: "CMS presets for new sites (nginx rules + PHP defaults)", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*presetsOutput, error) {
		return &presetsOutput{Body: sitePresets}, nil
	})
}
