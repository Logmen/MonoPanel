package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const settingRealIP = "nginx.real_ip"

type realIPOutput struct {
	Body apitypes.RealIPSettings
}

type realIPInput struct {
	Body apitypes.RealIPSettings
}

// realIPSettings returns the stored trusted-proxy configuration.
func (s *Server) realIPSettings(ctx context.Context) apitypes.RealIPSettings {
	var set apitypes.RealIPSettings
	if v, err := s.db.GetSetting(ctx, settingRealIP); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &set)
	}
	if set.From == nil {
		set.From = []string{}
	}
	return set
}

// applyRealIP writes http.d/10-real-ip.conf and reloads nginx.
func (s *Server) applyRealIP(ctx context.Context, set apitypes.RealIPSettings) error {
	web := s.profile.Web()
	conf, err := s.render.Render("nginx/real-ip.conf.tmpl", render.RealIP{Cloudflare: set.Cloudflare, CloudflareRanges: render.CloudflareRanges, From: set.From})
	if err != nil {
		return err
	}
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:    []agent.FileSpec{{Path: path.Join(web.NginxConfDir, "monopanel", "http.d", "10-real-ip.conf"), Content: conf, Mode: 0o644}},
		Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Origin: "stack:real-ip",
	})
	return err
}

func (s *Server) registerRealIP() {
	huma.Register(s.api, huma.Operation{
		OperationID: "nginx-real-ip-get", Method: http.MethodGet, Path: "/stack/nginx/real-ip", Summary: "Proxies trusted for the real client address", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*realIPOutput, error) {
		return &realIPOutput{Body: s.realIPSettings(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "nginx-real-ip-set", Method: http.MethodPut, Path: "/stack/nginx/real-ip", Summary: "Trust Cloudflare and/or custom proxies for the client address (nginx real_ip)", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *realIPInput) (*realIPOutput, error) {
		p := principalFrom(ctx)
		set := in.Body
		from, err := normalizeAllowFrom(set.From)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		set.From = from
		if err := s.applyRealIP(ctx, set); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		raw, _ := json.Marshal(set)
		if err := s.db.SetSetting(ctx, settingRealIP, string(raw)); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "nginx.real_ip", Target: "nginx", IP: requestInfo(ctx).IP, Details: map[string]any{"cloudflare": set.Cloudflare, "from": set.From}})
		return &realIPOutput{Body: set}, nil
	})
}
