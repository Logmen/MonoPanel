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
	{ID: "", Name: "Universal PHP", Description: "index.php front controller, static files served by nginx; suits Laravel, Symfony and any PHP application."},
	{ID: presetWordPress, Name: "WordPress", Description: "Pretty permalinks, /wp-admin, xmlrpc.php closed, no PHP in wp-content/uploads, 128M upload limits."},
	{ID: presetJoomla, Name: "Joomla", Description: "SEF URLs, /api for Joomla 4+, configuration.php and cache/logs/tmp closed, no PHP in images/media."},
	{ID: presetBitrix, Name: "1C-Bitrix", Description: "urlrewrite.php, BitrixVM rules for bitrix/ and upload/, short_open_tag, max_input_vars 20000, 256M limits, 512M memory, opcache for 100000 files, no open_basedir."},
	{ID: presetOpenCart, Name: "OpenCart", Description: "SEO URLs via _route_, sitemap/googlebase, system/ and storage/ closed, .tpl/.twig/.log not served."},
}

// presetIni is applied under the panel defaults and above nothing else: site
// overrides (php_ini) still win.
var presetIni = map[string]map[string]string{
	presetWordPress: {"upload_max_filesize": "128M", "post_max_size": "128M", "max_execution_time": "300", "max_input_vars": "5000"},
	presetJoomla:    {"upload_max_filesize": "64M", "post_max_size": "64M", "max_execution_time": "300", "max_input_vars": "5000", "output_buffering": "Off"},
	presetBitrix: {
		"memory_limit": "512M", "upload_max_filesize": "256M", "post_max_size": "256M", "max_execution_time": "600", "max_input_vars": "20000",
		"short_open_tag": "On", "mbstring.internal_encoding": "UTF-8", "default_charset": "UTF-8", "opcache.revalidate_freq": "0",
		"realpath_cache_size": "4096k", "realpath_cache_ttl": "3600", "opcache.max_accelerated_files": "100000",
		"pcre.backtrack_limit": "1000000", "pcre.recursion_limit": "14000",
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
