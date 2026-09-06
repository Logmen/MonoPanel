package api

import (
	"context"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// Per-site nginx customisation lives in sites/<domain>.d/custom.conf, which
// the generated server block includes; the panel never overwrites it.

const customConfName = "custom.conf"

type siteNginxOutput struct {
	Body apitypes.SiteNginx
}

type siteNginxInput struct {
	Domain string `path:"domain"`
	Body   apitypes.SiteNginxRequest
}

type sitePHPOutput struct {
	Body apitypes.SitePHP
}

func (s *Server) siteNginx(ctx context.Context, site *store.Site) (*apitypes.SiteNginx, error) {
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return nil, err
	}
	l := s.layoutFor(site, user)
	out := &apitypes.SiteNginx{ConfigPath: l.nginxConf, IncludeDir: l.nginxDir, CustomPath: path.Join(l.nginxDir, customConfName), Others: []string{}}
	if b, err := os.ReadFile(l.nginxConf); err == nil {
		out.Generated = string(b)
	}
	if b, err := os.ReadFile(out.CustomPath); err == nil {
		out.Custom = string(b)
	}
	if entries, err := os.ReadDir(l.nginxDir); err == nil {
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".conf") && n != customConfName {
				out.Others = append(out.Others, n)
			}
		}
	}
	return out, nil
}

func (s *Server) registerSiteNginx() {
	huma.Register(s.api, huma.Operation{
		OperationID: "sites-nginx-get", Method: http.MethodGet, Path: "/sites/{domain}/nginx", Summary: "Generated nginx server block and the custom directives file of a site", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, in *siteDomainInput) (*siteNginxOutput, error) {
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		out, err := s.siteNginx(ctx, site)
		if err != nil {
			return nil, err
		}
		return &siteNginxOutput{Body: *out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-nginx-set", Method: http.MethodPut, Path: "/sites/{domain}/nginx", Summary: "Write custom nginx directives of a site (validated with nginx -t, rolled back on error)", Tags: []string{"sites"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *siteNginxInput) (*siteNginxOutput, error) {
		p := principalFrom(ctx)
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		cur, err := s.siteNginx(ctx, site)
		if err != nil {
			return nil, err
		}
		web := s.profile.Web()
		content := strings.ReplaceAll(in.Body.Custom, "\r\n", "\n")
		if strings.TrimSpace(content) == "" {
			if cur.Custom != "" {
				if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{cur.CustomPath}, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}}); err != nil {
					return nil, huma.Error422UnprocessableEntity(err.Error())
				}
			}
		} else {
			if !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			_, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
				Files: []agent.FileSpec{{Path: cur.CustomPath, Content: content, Mode: 0o644}}, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Origin: "site:" + site.Domain + ":custom",
			})
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("nginx rejected the configuration (previous version restored): " + err.Error())
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.nginx", Target: site.Domain, IP: requestInfo(ctx).IP, Details: map[string]any{"bytes": len(content)}})
		out, err := s.siteNginx(ctx, site)
		if err != nil {
			return nil, err
		}
		return &siteNginxOutput{Body: *out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-php-get", Method: http.MethodGet, Path: "/sites/{domain}/php", Summary: "Effective PHP settings of a site (defaults + overrides) and the keys that may be overridden", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, in *siteDomainInput) (*sitePHPOutput, error) {
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		user, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, err
		}
		l := s.layoutFor(site, user)
		out := apitypes.SitePHP{Version: site.PHPVersion, PoolPath: l.poolConf, Socket: l.socket, Allowed: make([]string, 0, len(allowedIniKeys)), Values: []apitypes.PHPValue{}}
		for k := range allowedIniKeys {
			out.Allowed = append(out.Allowed, k)
		}
		sort.Strings(out.Allowed)
		for _, kv := range s.poolValues(ctx, site) {
			src := "default"
			if _, ok := presetIni[site.Preset][kv.Key]; ok {
				src = "preset"
			}
			if _, ok := site.PHPIni[kv.Key]; ok {
				src = "site"
			}
			out.Values = append(out.Values, apitypes.PHPValue{Key: kv.Key, Value: kv.Value, Source: src})
		}
		return &sitePHPOutput{Body: out}, nil
	})
}
