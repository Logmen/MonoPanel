// Package agenttest is an in-process stand-in for the privileged agent.
//
// The panel reaches the agent over a unix socket with a small typed protocol,
// so tests can point a real agent.Client at a fake that answers successfully,
// records every request and lets a test inspect what the panel would have
// written to disk (nginx server blocks, php-fpm pools, systemd units) without
// root, systemd or a real host.
package agenttest

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/systemd"
)

// Call is one recorded agent request: the endpoint path and the raw body.
type Call struct {
	Path string
	Body []byte
}

// Agent is a fake agent listening on a unix socket.
type Agent struct {
	Socket string

	mu      sync.Mutex
	calls   []Call
	files   map[string]agent.FileSpec // last written content per path
	removed []string
	units   map[string]string // unit -> last action
	dirs    []string
	tools   []agent.ToolRequest

	// Fail makes the endpoint at the given path answer with an error; the
	// message is returned to the panel as the agent's failure output.
	Fail map[string]string
	// Stat answers agent.Stat for these paths as existing files; a path with
	// a trailing "/" is reported as a directory. Unknown paths do not exist.
	Stat map[string]bool
	// ReadFile answers agent.ReadFile for these paths.
	ReadFile map[string]string
	// ToolOutput answers agent.Tool by tool name.
	ToolOutput map[string]string
	// StreamOutput answers agent.StreamOut by tool name.
	StreamOutput map[string]string
	// MissingPackages are absent from every repository: "available" leaves
	// them out and "query" reports them as not installed.
	MissingPackages map[string]bool
	// ToolHook, when set, may answer a tool call itself (nil = default).
	ToolHook func(req agent.ToolRequest) *agent.ToolResponse
	// Dirs answers agent.ListDir: directory path -> names inside it.
	DirEntries map[string][]string
	// Shadow answers agent.UnixShadow: login -> password hash.
	Shadow map[string]string
	// Streams records the streaming calls (tar, mysqldump) with the bytes that
	// went through them: перенос аккаунта — это два потока, и тест должен
	// видеть, что они были и куда шли.
	streams []Stream

	srv *httptest.Server
}

// Start launches a fake agent on a unix socket inside a temporary directory
// and stops it when the test ends.
func Start(t *testing.T) *Agent {
	t.Helper()
	// The socket path has to stay short: unix sockets are capped near 100
	// bytes, and t.TempDir() with a long test name can overflow that.
	dir, err := os.MkdirTemp("", "mpagent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	a := &Agent{
		Socket:       filepath.Join(dir, "agent.sock"),
		files:        map[string]agent.FileSpec{},
		units:        map[string]string{},
		Fail:         map[string]string{},
		Stat:         map[string]bool{},
		ReadFile:     map[string]string{},
		ToolOutput:   map[string]string{},
		StreamOutput: map[string]string{},
		DirEntries:   map[string][]string{},
		Shadow:       map[string]string{},
	}
	ln, err := net.Listen("unix", a.Socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", a.Socket, err)
	}
	a.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(a.handle)}}
	a.srv.Start()
	t.Cleanup(a.srv.Close)
	return a
}

// Stream is one recorded streaming call.
type Stream struct {
	Direction string // out | in
	Name      string
	Args      []string
	Bytes     int
}

// Streams returns the recorded streaming calls.
func (a *Agent) Streams() []Stream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Stream{}, a.streams...)
}

// Client returns an agent client wired to this fake.
func (a *Agent) Client() *agent.Client { return agent.NewClient(a.Socket) }

func (a *Agent) handle(w http.ResponseWriter, r *http.Request) {
	// Потоки не JSON: у /v1/stream/out тело ответа — данные, у /v1/stream/in
	// данные приходят телом запроса.
	switch r.URL.Path {
	case "/v1/stream/out":
		var req agent.StreamRequest
		body, _ := readAll(r)
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.mu.Lock()
		a.calls = append(a.calls, Call{Path: r.URL.Path, Body: body})
		a.streams = append(a.streams, Stream{Direction: "out", Name: req.Name, Args: req.Args})
		out := a.StreamOutput[req.Name]
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(out)) //nolint:errcheck // test double
		return
	case "/v1/stream/in":
		n, _ := io.Copy(io.Discard, r.Body)
		spec, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Stream-Spec"))
		var req agent.StreamRequest
		json.Unmarshal(spec, &req) //nolint:errcheck // test double
		a.mu.Lock()
		a.calls = append(a.calls, Call{Path: r.URL.Path, Body: spec})
		a.streams = append(a.streams, Stream{Direction: "in", Name: req.Name, Args: req.Args, Bytes: int(n)})
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agent.StreamResponse{}) //nolint:errcheck // test double
		return
	}
	body, _ := readAll(r)
	a.mu.Lock()
	a.calls = append(a.calls, Call{Path: r.URL.Path, Body: body})
	msg, failing := a.Fail[r.URL.Path]
	a.mu.Unlock()

	if failing {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(agent.Error{Status: http.StatusBadGateway, Message: msg, Output: msg}) //nolint:errcheck // test double
		return
	}

	resp := a.respond(r.URL.Path, body)
	w.Header().Set("Content-Type", "application/json")
	if resp == nil {
		w.Write([]byte("{}")) //nolint:errcheck // test double
		return
	}
	json.NewEncoder(w).Encode(resp) //nolint:errcheck // test double
}

// respond builds a plausible success answer and records the side effects a
// real agent would have performed.
func (a *Agent) respond(path string, body []byte) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch path {
	case "/v1/ping":
		return agent.PingResponse{OK: true, Version: "test", Hostname: "test-host"}

	case "/v1/config/apply":
		var req agent.ApplyConfigSetRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		written := make([]string, 0, len(req.Files))
		for _, f := range req.Files {
			a.files[f.Path] = f
			written = append(written, f.Path)
		}
		for _, u := range append(append([]string{}, req.Reload...), req.Restart...) {
			a.units[u] = "reload"
		}
		return agent.ApplyConfigSetResponse{Written: written, Validated: len(req.Validate), Reloaded: req.Reload}

	case "/v1/file/ensure":
		var req agent.EnsureFileRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.files[req.Path] = agent.FileSpec{Path: req.Path, Content: req.Content, Mode: req.Mode, Owner: req.Owner}
		return agent.EnsureFileResponse{Written: true}

	case "/v1/dirs/ensure":
		var req agent.EnsureDirsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		created := make([]string, 0, len(req.Dirs))
		for _, d := range req.Dirs {
			a.dirs = append(a.dirs, d.Path)
			created = append(created, d.Path)
		}
		return agent.EnsureDirsResponse{Created: created}

	case "/v1/paths/remove":
		var req agent.RemovePathsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.removed = append(a.removed, req.Paths...)
		// A real agent reports only what existed, and the panel acts on that
		// (a removed pool means the old php-fpm has to be reloaded). The fake
		// knows what it wrote, so it answers the same way.
		gone := []string{}
		for _, p := range req.Paths {
			if _, ok := a.files[p]; ok {
				delete(a.files, p)
				gone = append(gone, p)
			}
		}
		return agent.RemovePathsResponse{Removed: gone}

	case "/v1/group/ensure":
		return agent.EnsureGroupResponse{GID: 3000, Created: true}

	case "/v1/user/ensure":
		var req agent.EnsureUnixUserRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		home := req.Home
		if home == "" {
			home = "/var/www/" + req.Login
		}
		return agent.EnsureUnixUserResponse{UID: 1500, GID: 1500, Home: home, Created: true}

	case "/v1/user/remove":
		return agent.RemoveUnixUserResponse{Removed: true, HomeRemoved: true}

	case "/v1/service":
		var req agent.ServiceRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		unit := req.Unit
		if unit != "" && !strings.Contains(unit, ".") {
			unit += ".service"
		}
		if unit != "" && req.Action != "" && req.Action != "status" {
			a.units[unit] = req.Action
		}
		state := "active"
		if req.Action == "stop" || req.Action == "disable" {
			state = "inactive"
		}
		return agent.ServiceResponse{Status: systemd.Status{Unit: unit, LoadState: "loaded", ActiveState: state, SubState: "running", UnitFileState: "enabled"}}

	case "/v1/pkg":
		var req agent.PkgRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		versions := map[string]string{}
		for _, p := range req.Packages {
			if !a.MissingPackages[p] {
				versions[p] = "1.0-test"
			}
		}
		if req.Action == "available" {
			return agent.PkgResponse{Available: versions, Output: "ok"}
		}
		return agent.PkgResponse{Installed: versions, Output: "ok"}

	case "/v1/dir/list":
		var req agent.ListDirRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		out := agent.ListDirResponse{Entries: []agent.ListDirEntry{}}
		for _, name := range a.DirEntries[req.Path] {
			out.Entries = append(out.Entries, agent.ListDirEntry{Name: name})
		}
		return out

	case "/v1/panel/install":
		var req agent.InstallPanelRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		return agent.InstallPanelResponse{Unit: "monopanel-update.service", Started: true, Signed: req.Sig != ""}

	case "/v1/tool":
		var req agent.ToolRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.tools = append(a.tools, req)
		if a.ToolHook != nil {
			if res := a.ToolHook(req); res != nil {
				return *res
			}
		}
		return agent.ToolResponse{ExitCode: 0, Output: a.ToolOutput[req.Name]}

	case "/v1/stat":
		var req agent.StatRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		entries := make([]agent.StatEntry, 0, len(req.Paths))
		for _, p := range req.Paths {
			isDir, exists := a.Stat[p]
			if !exists {
				isDir, exists = a.Stat[p+"/"]
			}
			entries = append(entries, agent.StatEntry{Path: p, Exists: exists, IsDir: isDir, Mode: "0644"})
		}
		return agent.StatResponse{Entries: entries}

	case "/v1/file/read":
		var req agent.ReadFileRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		c := a.ReadFile[req.Path]
		return agent.ReadFileResponse{Content: c, Size: int64(len(c))}

	case "/v1/chown":
		return agent.ChownResponse{Changed: 1}

	case "/v1/user/shadow":
		var req agent.UnixShadowRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if h, ok := a.Shadow[req.Login]; ok {
			return agent.UnixShadowResponse{Hash: h}
		}
		return agent.UnixShadowResponse{}

	case "/v1/system/info":
		return map[string]any{"hostname": "test-host", "cpus": 2}
	}
	return map[string]any{}
}

// Calls returns the recorded requests.
func (a *Agent) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call{}, a.calls...)
}

// Called reports whether the endpoint was called at least once.
func (a *Agent) Called(path string) bool {
	for _, c := range a.Calls() {
		if c.Path == path {
			return true
		}
	}
	return false
}

// LastCall returns the most recent request to an endpoint.
func (a *Agent) LastCall(path string) (Call, bool) {
	calls := a.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Path == path {
			return calls[i], true
		}
	}
	return Call{}, false
}

// File returns the content last written to a path, and whether it was written.
func (a *Agent) File(path string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.files[path]
	return f.Content, ok
}

// FileContaining returns the content of the first written file whose path
// contains the substring; handy when the exact layout is not the point.
func (a *Agent) FileContaining(substr string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for p, f := range a.files {
		if strings.Contains(p, substr) {
			return f.Content, true
		}
	}
	return "", false
}

// Files returns every written path.
func (a *Agent) Files() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.files))
	for p := range a.files {
		out = append(out, p)
	}
	return out
}

// Removed returns every path the panel asked to remove.
func (a *Agent) Removed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.removed...)
}

// UnitAction returns the last action requested for a systemd unit.
func (a *Agent) UnitAction(unit string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.units[unit]
}

// Tools returns the recorded tool invocations.
func (a *Agent) Tools() []agent.ToolRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.ToolRequest{}, a.tools...)
}

// Reset clears the recorded calls, keeping the configured behaviour.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = nil
	a.files = map[string]agent.FileSpec{}
	a.removed = nil
	a.units = map[string]string{}
	a.dirs = nil
	a.tools = nil
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
