package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
		Description: "Answers with the unit's state after the action. Stopping or restarting monopanel-api, or monopanel-agent (which monopanel-api requires), stops the process that serves the request: for these, stop, restart and reload-or-restart are recorded in the audit log and answered with the unit's current state first, and carried out right after the answer. Reconnect a few seconds later.",
	}, func(ctx context.Context, in *serviceActionInput) (*serviceOutput, error) {
		p := principalFrom(ctx)
		if s.stopsPanel(in.Unit, in.Body.Action) {
			return s.panelUnitAction(ctx, in.Unit, in.Body.Action)
		}
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

// stopsPanel reports whether the action ends the process that serves the
// request: a stop or restart of monopanel-api itself, or of monopanel-agent,
// which monopanel-api requires — systemd stops and restarts the API with it.
// Neither unit can reload, so reload-or-restart restarts.
func (s *Server) stopsPanel(unit, action string) bool {
	switch action {
	case "stop", "restart", "reload-or-restart":
	default:
		return false
	}
	// Both sides as systemd names them: the request and config.yaml may
	// each leave the .service off, as the agent does.
	unit = serviceUnit(unit)
	return unit == serviceUnit(s.cfg.Update.APIUnit) || unit == serviceUnit(s.cfg.Update.AgentUnit)
}

// serviceUnit is a unit name with the .service suffix systemd implies when
// a name has none.
func serviceUnit(unit string) string {
	if unit != "" && !strings.Contains(unit, ".") {
		return unit + ".service"
	}
	return unit
}

// panelUnitAction records an action on the panel's own units and answers
// before carrying it out. Done inside the request, it never reached the
// audit log, and the client waited ten seconds for nothing: SIGTERM starts a
// graceful shutdown that waits for this very request, the request waits for
// the agent, and the agent waits for systemd, which waits for the shutdown —
// until its timeout ends the process. From the after-response hook, the
// answer is out first.
func (s *Server) panelUnitAction(ctx context.Context, unit, action string) (*serviceOutput, error) {
	p, ip := principalFrom(ctx), requestInfo(ctx).IP
	entry := store.AuditEntry{Actor: p.Login, Action: "service." + action, Target: unit, IP: ip}
	// What would make the action fail is checked first: the answer says it
	// happens, and a client told so waits for the panel to come back.
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := s.agent.Service(actx, unit, "status")
	if err != nil {
		entry.Result, entry.Details = "error", map[string]any{"error": err.Error()}
		s.db.Audit(ctx, entry)
		return nil, huma.Error502BadGateway("agent: " + err.Error())
	}
	if res.Status.LoadState != "loaded" {
		msg := fmt.Sprintf("%s is not a loaded systemd unit on this server (load state %q): systemd cannot %s it", unit, res.Status.LoadState, action)
		entry.Result, entry.Details = "error", map[string]any{"error": msg}
		s.db.Audit(ctx, entry)
		return nil, huma.Error409Conflict(msg)
	}
	// Recorded as asked for: when the action succeeds, this process is gone
	// before it could record anything else.
	entry.Details = map[string]any{"after_answer": true}
	s.db.Audit(ctx, entry)
	afterResponse(ctx, func() { s.actOnPanelUnit(entry, unit, action) })
	return &serviceOutput{Body: res.Status}, nil
}

// actOnPanelUnit carries out an action on the panel's own units once the
// answer has gone out. When it works, systemd stops this process before the
// agent answers; an error means the action did not happen, and the audit log
// says so.
func (s *Server) actOnPanelUnit(entry store.AuditEntry, unit, action string) {
	time.Sleep(s.restartDelay)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.log.Info("acting on the panel's own unit", "unit", unit, "action", action, "actor", entry.Actor)
	if _, err := s.agent.Service(ctx, unit, action); err != nil {
		s.log.Error("acting on the panel's own unit", "unit", unit, "action", action, "err", err)
		entry.Result, entry.Details = "error", map[string]any{"after_answer": true, "error": err.Error()}
		s.db.Audit(ctx, entry)
	}
}

func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
