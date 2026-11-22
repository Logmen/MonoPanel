package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// selfAgent stands in for the agent where an action on the panel's own units
// ends the API: status answers at once, an action waits until the test lets
// it go — as the real call does, which systemd completes only once the
// process that made it is gone — and then answers, with an error when told.
type selfAgent struct {
	sock    string
	release chan struct{}

	mu        sync.Mutex
	loadState string
	statusErr string
	actionErr string
	actions   []string // "<unit> <action>", in the order they arrived
	answered  int
}

func startSelfAgent(t *testing.T) *selfAgent {
	t.Helper()
	dir, err := os.MkdirTemp("", "mpself")
	if err != nil {
		t.Fatal(err)
	}
	a := &selfAgent{sock: filepath.Join(dir, "agent.sock"), release: make(chan struct{}), loadState: "loaded"}
	ln, err := net.Listen("unix", a.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(a.serve)}
	go srv.Serve(ln) //nolint:errcheck // ends with Close
	t.Cleanup(func() {
		srv.Close()
		os.RemoveAll(dir)
	})
	return a
}

func (a *selfAgent) serve(w http.ResponseWriter, r *http.Request) {
	var req agent.ServiceRequest
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &req) //nolint:errcheck // test double
	fail := func(msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(agent.Error{Message: msg}) //nolint:errcheck // test double
	}
	a.mu.Lock()
	loadState, statusErr, actionErr := a.loadState, a.statusErr, a.actionErr
	if req.Action != "status" {
		a.actions = append(a.actions, req.Unit+" "+req.Action)
	}
	a.mu.Unlock()
	if r.URL.Path != "/v1/service" {
		w.Write([]byte("{}")) //nolint:errcheck // test double
		return
	}
	if req.Action == "status" {
		if statusErr != "" {
			fail(statusErr)
			return
		}
		json.NewEncoder(w).Encode(agent.ServiceResponse{Status: systemd.Status{Unit: req.Unit, LoadState: loadState, ActiveState: "active", SubState: "running", UnitFileState: "enabled"}}) //nolint:errcheck // test double
		return
	}
	select {
	case <-a.release:
	case <-r.Context().Done():
		return
	}
	defer func() {
		a.mu.Lock()
		a.answered++
		a.mu.Unlock()
	}()
	if actionErr != "" {
		fail(actionErr)
		return
	}
	json.NewEncoder(w).Encode(agent.ServiceResponse{Status: systemd.Status{Unit: req.Unit, LoadState: "loaded", ActiveState: "active", SubState: "running"}}) //nolint:errcheck // test double
}

func (a *selfAgent) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.actions...)
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serviceCall posts a service action and gives up after a few seconds: an
// answer that waits for the action itself would never come.
func serviceCall(f *siteFixture, unit, action string) (int, systemd.Status, string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"action": action})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.ts.URL+"/api/v1/services/"+unit, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("POST /services/%s %s: %v", unit, action, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var st systemd.Status
	json.Unmarshal(raw, &st) //nolint:errcheck // an error answer has another shape
	return res.StatusCode, st, string(raw)
}

func serviceAudit(f *siteFixture, action string) []store.AuditEntry {
	f.t.Helper()
	entries, err := f.db.ListAudit(f.ctx, store.AuditFilter{Action: action, Limit: 10})
	if err != nil {
		f.t.Fatal(err)
	}
	return entries
}

// Stopping or restarting monopanel-api ends the process that serves the
// request, and monopanel-agent takes monopanel-api with it: the action is in
// the audit log and the answer is out before the agent is asked to act — the
// agent here holds the action, as the real one does while systemd stops the
// API, and the answer does not wait for it.
func TestPanelUnitActionAnswersFirst(t *testing.T) {
	for _, c := range []struct{ unit, action string }{
		{"monopanel-api.service", "restart"},
		{"monopanel-api", "reload-or-restart"},
		{"monopanel-agent", "stop"},
		{"monopanel-agent.service", "restart"},
	} {
		f := newSiteFixture(t)
		f.s.restartDelay = 0
		a := startSelfAgent(t)
		f.s.agent = agent.NewClient(a.sock)

		code, st, raw := serviceCall(f, c.unit, c.action)
		if code != http.StatusOK || st.Unit != c.unit || st.ActiveState != "active" {
			t.Fatalf("%s %s: %d %s", c.action, c.unit, code, raw)
		}
		entries := serviceAudit(f, "service."+c.action)
		if len(entries) != 1 || entries[0].Result != "ok" || entries[0].Target != c.unit || entries[0].Actor != "admin" || entries[0].Details["after_answer"] != true {
			t.Fatalf("%s %s: audit before the action: %+v", c.action, c.unit, entries)
		}
		want := c.unit + " " + c.action
		waitFor(t, want, func() bool { return slices.Contains(a.seen(), want) })
		close(a.release)
		waitFor(t, "the agent's answer", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.answered == 1 })
		time.Sleep(20 * time.Millisecond)
		if entries := serviceAudit(f, "service."+c.action); len(entries) != 1 {
			t.Fatalf("%s %s: a successful action was audited again: %+v", c.action, c.unit, entries)
		}
		if n := len(a.seen()); n != 1 {
			t.Fatalf("%s %s: the agent was asked %d times: %v", c.action, c.unit, n, a.seen())
		}
	}
}

// When the action fails after the answer, this process is still there to
// say so: the audit log gets the failure next to the request.
func TestPanelUnitActionFailureIsAudited(t *testing.T) {
	f := newSiteFixture(t)
	f.s.restartDelay = 0
	a := startSelfAgent(t)
	a.actionErr = "restart monopanel-api.service failed: Job canceled"
	f.s.agent = agent.NewClient(a.sock)
	if code, _, raw := serviceCall(f, "monopanel-api.service", "restart"); code != http.StatusOK {
		t.Fatalf("restart: %d %s", code, raw)
	}
	close(a.release)
	waitFor(t, "the failure in the audit log", func() bool { return len(serviceAudit(f, "service.restart")) == 2 })
	entries := serviceAudit(f, "service.restart")
	if entries[0].Result != "error" || entries[0].Details["after_answer"] != true || !strings.Contains(entries[0].Details["error"].(string), "Job canceled") || entries[1].Result != "ok" {
		t.Fatalf("audit: %+v", entries)
	}
}

// What would make the action fail is found before the answer promises it:
// an agent that does not answer, a unit systemd does not have. Nothing is
// scheduled then, and the refusal is audited like any failed action.
func TestPanelUnitActionRefusedBeforeTheAnswer(t *testing.T) {
	f := newSiteFixture(t)
	f.s.restartDelay = 0
	a := startSelfAgent(t)
	f.s.agent = agent.NewClient(a.sock)

	a.statusErr = "systemd unreachable"
	if code, _, raw := serviceCall(f, "monopanel-api.service", "restart"); code != http.StatusBadGateway || !strings.Contains(raw, "systemd unreachable") {
		t.Fatalf("agent failing: %d %s", code, raw)
	}
	a.mu.Lock()
	a.statusErr, a.loadState = "", "not-found"
	a.mu.Unlock()
	if code, _, raw := serviceCall(f, "monopanel-agent", "stop"); code != http.StatusConflict || !strings.Contains(raw, "not a loaded systemd unit") {
		t.Fatalf("a unit systemd does not have: %d %s", code, raw)
	}
	time.Sleep(50 * time.Millisecond)
	if seen := a.seen(); len(seen) != 0 {
		t.Fatalf("a refused action reached the agent: %v", seen)
	}
	restart, stop := serviceAudit(f, "service.restart"), serviceAudit(f, "service.stop")
	if len(restart) != 1 || restart[0].Result != "error" || len(stop) != 1 || stop[0].Result != "error" {
		t.Fatalf("audit: %+v %+v", restart, stop)
	}
}

// Every other unit, and the actions that stop nothing, work as before: the
// action is done when the answer comes, and audited after it.
func TestServiceActionInlineForOtherUnits(t *testing.T) {
	f := newSiteFixture(t)
	for _, c := range []struct{ unit, action string }{
		{"nginx.service", "restart"},
		{"nginx.service", "stop"},
		{"monopanel-api.service", "disable"},
		{"monopanel-api.service", "enable"},
		{"monopanel-agent.service", "start"},
	} {
		code, _, raw := serviceCall(f, c.unit, c.action)
		if code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", c.action, c.unit, code, raw)
		}
		if got := f.agent.UnitAction(c.unit); got != c.action {
			t.Fatalf("%s %s: done %q by the time of the answer", c.action, c.unit, got)
		}
		entries := serviceAudit(f, "service."+c.action)
		if len(entries) == 0 || entries[0].Target != c.unit || entries[0].Details["after_answer"] != nil {
			t.Fatalf("%s %s: audit %+v", c.action, c.unit, entries)
		}
	}
}

// Which actions end the panel does not depend on how the unit is spelt: the
// request and config.yaml may each leave .service off, as systemd does.
func TestStopsPanelReadsUnitNamesAsSystemd(t *testing.T) {
	s := &Server{}
	s.cfg.Update.APIUnit, s.cfg.Update.AgentUnit = "monopanel-api", "monopanel-agent.service"
	for _, c := range []struct {
		unit, action string
		want         bool
	}{
		{"monopanel-api", "restart", true},
		{"monopanel-api.service", "stop", true},
		{"monopanel-agent", "reload-or-restart", true},
		{"monopanel-agent.service", "restart", true},
		{"monopanel-api.service", "reload", false},
		{"monopanel-api.service", "start", false},
		{"monopanel-agent", "disable", false},
		{"monopanel-api.socket", "stop", false},
		{"monopanel-api.socket", "restart", true},
		{"monopanel-api.socket", "reload-or-restart", true},
		{"monopanel-api.socket", "start", false},
		{"monopanel-firewall", "restart", false},
	} {
		if got := s.stopsPanel(c.unit, c.action); got != c.want {
			t.Errorf("stopsPanel(%q, %q) = %v, want %v", c.unit, c.action, got, c.want)
		}
	}
}
