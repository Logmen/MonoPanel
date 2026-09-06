package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

type healthOutput struct {
	Body apitypes.Health
}

type statusOutput struct {
	Body apitypes.SystemStatus
}

func (s *Server) knownUnits() []string {
	web := s.profile.Web()
	units := []string{"monopanel-agent.service", web.NginxService, web.ApacheService}
	if s.profile.Family() == "rhel" {
		units = append(units, "mysqld.service")
	} else {
		units = append(units, "mysql.service")
	}
	if versions, err := s.db.ListPHPVersions(context.Background()); err == nil {
		for _, v := range versions {
			if v.Status == store.PHPInstalled {
				units = append(units, v.FPMService)
			}
		}
	}
	return units
}

func (s *Server) registerSystem() {
	huma.Register(s.api, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/health", Summary: "Liveness (public)", Tags: []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		return &healthOutput{Body: apitypes.Health{Status: "ok", Version: buildinfo.Version, Time: time.Now()}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "system-status", Method: http.MethodGet, Path: "/system/status", Summary: "Panel, host and service status", Tags: []string{"system"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*statusOutput, error) {
		out := &statusOutput{}
		out.Body.Panel = apitypes.PanelStatus{Version: buildinfo.Version, UptimeSeconds: time.Since(s.started).Seconds()}
		out.Body.Panel.SchemaVersion, _ = s.db.SchemaVersion(ctx)
		out.Body.Panel.Jobs, _ = s.db.CountJobs(ctx)
		out.Body.Panel.Users, _ = s.db.CountUsers(ctx)
		if out.Body.Panel.Jobs == nil {
			out.Body.Panel.Jobs = map[string]int{}
		}
		if out.Body.Panel.Users == nil {
			out.Body.Panel.Users = map[string]int{}
		}
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ping, err := s.agent.Ping(actx)
		if err != nil {
			out.Body.Panel.AgentError = err.Error()
			return out, nil
		}
		out.Body.Panel.AgentOK = ping.OK
		out.Body.Panel.AgentVersion = ping.Version
		if info, err := s.agent.SystemInfo(actx); err == nil {
			out.Body.Host = info
		}
		for _, unit := range s.knownUnits() {
			res, err := s.agent.Service(actx, unit, "status")
			if err != nil {
				out.Body.Services = append(out.Body.Services, systemd.Status{Unit: unit, LoadState: "unknown", ActiveState: "unknown"})
				continue
			}
			out.Body.Services = append(out.Body.Services, res.Status)
		}
		return out, nil
	})
}
