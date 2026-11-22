package demo

import (
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"monopanel/internal/agent"
	"monopanel/internal/config"
)

// notPretended are the agent's RPCs the pretend agent leaves out on
// purpose, with why. None so far: each answers, or refuses with "not
// available in the demo".
var notPretended = map[string]string{}

// routesOf lists a router's routes as "METHOD path".
func routesOf(t *testing.T, h http.Handler) []string {
	t.Helper()
	r, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("%T is no chi router", h)
	}
	var out []string
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// Every RPC of the real agent has its counterpart in the pretend one — an
// answer, or a refusal that says the demo does not do it — so that the next
// RPC the API gets is not met with a 404 the API reads as an agent older
// than itself ("restart monopanel-agent"); and the pretend agent pretends
// nothing the real one does not have.
func TestPretendAgentHasEveryRPC(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	rpcs := routesOf(t, agent.NewServer(cfg, Profile(), quiet).Handler())
	pretend := routesOf(t, NewAgent(cfg, "", quiet).Handler())
	if len(rpcs) == 0 {
		t.Fatal("the agent has no routes")
	}
	for _, r := range rpcs {
		if !slices.Contains(pretend, r) && notPretended[r] == "" {
			t.Errorf("the demo agent lacks %s: pretend it, refuse it with notInDemo, or list it in notPretended with why", r)
		}
	}
	for _, r := range pretend {
		if !slices.Contains(rpcs, r) {
			t.Errorf("the demo agent has %s, which the agent does not", r)
		}
	}
	for r := range notPretended {
		if slices.Contains(pretend, r) || !slices.Contains(rpcs, r) {
			t.Errorf("notPretended lists %s, which the demo has or the agent has not", r)
		}
	}
}
