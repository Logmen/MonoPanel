package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

type servicesOutput struct {
	Body []systemd.Status
}

type serviceInput struct {
	Unit string `path:"unit" pattern:"^[A-Za-z0-9_.@:-]+$"`
}

type serviceActionInput struct {
	Unit string `path:"unit" pattern:"^[A-Za-z0-9_.@:-]+$"`
	Body apitypes.ServiceActionRequest
}

type serviceOutput struct {
	Body systemd.Status
}

func (s *Server) registerServices() {
	huma.Register(s.api, huma.Operation{
		OperationID: "services-list", Method: http.MethodGet, Path: "/services", Summary: "Status of the managed services", Tags: []string{"services"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*servicesOutput, error) {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out := &servicesOutput{Body: []systemd.Status{}}
		for _, unit := range s.knownUnits() {
			res, err := s.agent.Service(actx, unit, "status")
			if err != nil {
				out.Body = append(out.Body, systemd.Status{Unit: unit, LoadState: "unknown", ActiveState: "unknown"})
				continue
			}
			out.Body = append(out.Body, res.Status)
		}
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "services-get", Method: http.MethodGet, Path: "/services/{unit}", Summary: "Status of one unit", Tags: []string{"services"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *serviceInput) (*serviceOutput, error) {
		res, err := s.agent.Service(ctx, in.Unit, "status")
		if err != nil {
			return nil, huma.Error502BadGateway("agent: " + err.Error())
		}
		return &serviceOutput{Body: res.Status}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "services-action", Method: http.MethodPost, Path: "/services/{unit}", Summary: "Start, stop, reload, restart, enable or disable a unit", Tags: []string{"services"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *serviceActionInput) (*serviceOutput, error) {
		p := principalFrom(ctx)
		actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		res, err := s.agent.Service(actx, in.Unit, in.Body.Action)
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "service." + in.Body.Action, Target: in.Unit, IP: requestInfo(ctx).IP, Result: resultOf(err)})
		if err != nil {
			return nil, huma.Error502BadGateway("agent: " + err.Error())
		}
		return &serviceOutput{Body: res.Status}, nil
	})
	_ = http.StatusOK
}

func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
