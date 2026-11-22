package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
)

// agentStatus answers a request whose agent call failed. The agent's own
// refusals keep their status — a policy that says no (403), a conflict
// (409: a shared folder in use), a name it does not take (400, 422), not now
// (503: an extraction or a restore below the web root holds the guard of the
// shared folders) — so a client tells "not allowed" or "try again later"
// from "the agent is down or the tool failed", which stays a bad gateway.
func agentStatus(msg string, err error) error {
	var ae *agent.Error
	if errors.As(err, &ae) && (ae.Status >= 400 && ae.Status < 500 || ae.Status == http.StatusServiceUnavailable) {
		return huma.NewError(ae.Status, msg)
	}
	return huma.Error502BadGateway(msg)
}
