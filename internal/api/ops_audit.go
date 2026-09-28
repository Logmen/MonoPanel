package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/store"
)

type auditListInput struct {
	Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
	Actor  string `query:"actor" doc:"Only this account's entries"`
	Action string `query:"action" doc:"Action prefix, e.g. auth. for sign-ins"`
}

type auditOutput struct {
	Body []store.AuditEntry
}

// registerAudit exposes the audit log the panel has been writing all along:
// who signed in and from where, who changed what. An administrator sees
// everything, an account only its own entries.
func (s *Server) registerAudit() {
	huma.Register(s.api, huma.Operation{
		OperationID: "system-audit", Method: http.MethodGet, Path: "/system/audit", Summary: "Audit log: sign-ins, tokens, changes — who, what, from where", Tags: []string{"system"}, Security: secured,
	}, func(ctx context.Context, in *auditListInput) (*auditOutput, error) {
		f := store.AuditFilter{Limit: in.Limit, Actor: in.Actor, Action: in.Action}
		if p := principalFrom(ctx); p.Role != store.RoleAdmin {
			f.Actor = p.Login
		}
		list, err := s.db.ListAudit(ctx, f)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []store.AuditEntry{}
		}
		return &auditOutput{Body: list}, nil
	})
}

// auditFile mirrors audit entries to a file as JSON lines, one per entry,
// for incident reviews with the usual tools. The file is opened for every
// line, so logrotate may move it away at any moment; a file that cannot be
// written is reported once and then ignored — the database keeps the log.
func (s *Server) auditFile(path string) func(store.AuditEntry) {
	var once sync.Once
	return func(e store.AuditEntry) {
		line, err := json.Marshal(e)
		if err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
		if err != nil {
			once.Do(func() { s.log.Warn("audit file", "path", path, "err", err) })
			return
		}
		defer f.Close()
		f.Write(append(line, '\n')) //nolint:errcheck // best effort: the database holds the entry
	}
}
