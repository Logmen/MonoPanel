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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
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
	// actions are the /v1/service actions, "<unit> <action>" in the order
	// they came (status left out).
	actions []string
	// instances are the states of the template instances the panel acts on
	// (an account's Apache, monopanel-apache@<login>.service): one nothing
	// started is inactive and disabled, and one whose httpd.conf the panel
	// did not write does not start (ConditionPathExists).
	instances map[string]*instanceState
	// apacheCtls are the /v1/apache/ctl calls the fake ran.
	apacheCtls []agent.ApacheCtlRequest
	// before runs ahead of every answer (SetBefore).
	before func(path string, body []byte)
	dirs   []string
	// dirSpecs is how dirs/ensure was last told to make a directory (an
	// empty owner keeps the one before), symlinks what symlink/ensure made:
	// the fake judges the way to a shared folder's mount point by them, as
	// the real agent judges the directories themselves.
	dirSpecs map[string]agent.DirSpec
	symlinks map[string]string
	// mounted and enabled are the state of the mounts by their units' names
	// (as systemd shows a mount); mountedAt is where one is mounted (the
	// unit of an older panel may be gone meanwhile), busy the folders in
	// use: an unbind fails, and so does the removal of their mount point, as
	// umount and rmdir do. shares are the agent's records of the folders it
	// bound, mount point → folder (sharebind.go).
	mounted   map[string]bool
	enabled   map[string]bool
	mountedAt map[string]string
	busy      map[string]bool
	shares    map[string]string
	tools     []agent.ToolRequest
	runas     []agent.RunAsUserRequest
	panel     []agent.PanelConfigRequest
	// siteLogs are the site logs the panel had the agent prepare, by login;
	// siteLogsGone the logins whose log directory it removed.
	siteLogs     map[string]map[string]bool
	siteLogsGone []string

	// Fail makes the endpoint at the given path answer with an error; the
	// message is returned to the panel as the agent's failure output.
	Fail map[string]string
	// FailStatus is the HTTP status of a Fail answer (default 502), e.g. 422
	// for a configuration a validator refused.
	FailStatus map[string]int
	// Stat answers agent.Stat for these paths as existing files; a path with
	// a trailing "/" is reported as a directory. Unknown paths do not exist.
	Stat map[string]bool
	// StatWritten makes agent.Stat also find what config/apply wrote and
	// paths/remove has not taken away since, as the real agent finds it on
	// disk: code that asks whether it wrote a file before.
	StatWritten bool
	// ReadFile answers agent.ReadFile for these paths.
	ReadFile map[string]string
	// ToolOutput answers agent.Tool by tool name.
	ToolOutput map[string]string
	// StreamOutput answers agent.StreamOut by tool name.
	StreamOutput map[string]string
	// MissingPackages are absent from every repository: "available" leaves
	// them out and "query" reports them as not installed.
	MissingPackages map[string]bool
	// InstallFails makes "install" fail with the given output when the
	// request names the package: a dependency no enabled repository carries.
	InstallFails map[string]string
	// PackageVersions overrides the version "query" and "available" report
	// (default 1.0-test).
	PackageVersions map[string]string
	// Upgrades are the newer versions the repositories offer of installed
	// packages, for "candidates"; "upgrade" installs them (PackageVersions
	// follows, the entry goes) and refuses, as the agent does, a package of
	// none of the panel's sets or one that is not installed. UpgradeFails
	// makes "upgrade" fail with the given output when it names the package.
	Upgrades     map[string]osprofile.Candidate
	UpgradeFails map[string]string
	// HeldPackages are held back by the administrator (apt-mark hold):
	// "candidates" reports their updates apart, "upgrade" leaves them.
	HeldPackages map[string]bool
	// BinaryReplaced marks the units whose running binary an upgrade
	// replaced (status says so) until they restart.
	BinaryReplaced map[string]bool
	// CheckFails makes a config/apply whose checks include the validator
	// (argv[0]) fail with the output, as a refused configuration does (422);
	// NewBinaryRefuses the same once "upgrade" upgraded the refusal's
	// package: a configuration the old binary took and the new one refuses.
	CheckFails       map[string]string
	NewBinaryRefuses map[string]Refusal
	// RestartFails makes a config/apply whose checks passed fail when it
	// reloads or restarts one of these units, with the output, as the agent
	// does ("files applied but restart failed", 500): the unit is down from
	// then on (status reports it failed, an account's Apache inactive).
	RestartFails map[string]string
	// ToolHook, when set, may answer a tool call itself (nil = default).
	ToolHook func(req agent.ToolRequest) *agent.ToolResponse
	// Sandbox is what ping says of restic's sandbox (nil: nothing, as an
	// agent before it did).
	Sandbox *agent.SandboxInfo
	// WriteThrough, when set, says which applied files also land on disk —
	// for code that reads a file back from the filesystem after the agent
	// wrote it (only ever temp paths in tests).
	WriteThrough func(path string) bool
	// RunAsHook answers a run-as-user call (fsop) itself; nil means exit 0
	// with no output.
	RunAsHook func(req agent.RunAsUserRequest) *agent.RunAsUserResponse
	// Dirs answers agent.ListDir: directory path -> names inside it.
	DirEntries map[string][]string
	// MemTotalBytes is the RAM agent.SystemInfo reports (0 by default),
	// Container the container it says it runs in ("" for none).
	MemTotalBytes uint64
	Container     string
	// Shadow answers agent.UnixShadow: login -> password hash.
	Shadow map[string]string
	// Streams records the streaming calls (tar, mysqldump) with the bytes that
	// went through them: перенос аккаунта — это два потока, и тест должен
	// видеть, что они были и куда шли.
	streams []Stream
	// UnitHost is what the fake checks units against with the real agent's
	// rules (agent.CheckUnit): by default every valid login is a panel
	// account with its home in /var/www, and the layouts are the
	// distributions' own. A unit the real agent would refuse fails here too.
	UnitHost *agent.UnitHost
	// LoadState answers the load state of these units ("not-found": a unit
	// the host does not have); every other unit is "loaded".
	LoadState map[string]string
	// ActiveState and UnitFileState answer the status of these units (not a
	// template's instances, whose state the fake keeps); every other unit
	// is active and enabled.
	ActiveState   map[string]string
	UnitFileState map[string]string
	// ApacheLegacySites is the shared Apache's directory of 0.8.19 that
	// /v1/apache/legacy reads with the real agent's code (empty: none).
	ApacheLegacySites string

	// upgraded are the package lists of the "upgrade" calls (Upgraded).
	upgraded [][]string

	srv *httptest.Server
}

// unitDir is where the fake keeps the units it was given, as the real agent
// writes them.
const unitDir = "/etc/systemd/system"

// refused names what the real agent never writes, removes or makes for the
// API through config/apply, paths/remove or dirs/ensure: systemd's trees (the
// units go through /v1/units/*), systemd-tmpfiles' configuration (the
// php-fpm socket directory comes with the package), the API's own data
// directory (it writes there itself) and the agent's own directory. The fake
// refuses them too, so that a call site still using them fails its test.
func refused(p string) bool {
	p = filepath.Clean(p)
	for _, t := range []string{"/etc/systemd", "/run/systemd", "/usr/lib/systemd", "/lib/systemd", "/etc/tmpfiles.d", config.Default().DataDir, config.DefaultAgentStateDir} {
		if p == t || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	return false
}

func refusal(p string) *agent.Error {
	return &agent.Error{Status: http.StatusForbidden, Message: "path not allowed: " + p}
}

// DefaultUnitHost answers the unit checks for a test host: any login is a
// panel account (home /var/www/<login>, primary group of the same name), the
// Sphinx unit is EL's, nginx is nginx.service.
func DefaultUnitHost() *agent.UnitHost {
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"))
	el, err := osprofile.FromRelease(rel)
	h := &agent.UnitHost{
		WWWRoot:        config.Default().WWWRoot,
		NginxService:   "nginx.service",
		ValkeyBinaries: []string{"/usr/bin/valkey-server", "/usr/bin/redis-server"},
		Account: func(login string) (*agent.UnitAccount, error) {
			return &agent.UnitAccount{Login: login, Home: filepath.Join(config.Default().WWWRoot, login), Group: login}, nil
		},
	}
	if err == nil {
		h.Sphinx = osprofile.Sphinx(el)
	}
	return h
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
		instances:    map[string]*instanceState{},
		dirSpecs:     map[string]agent.DirSpec{},
		symlinks:     map[string]string{},
		mounted:      map[string]bool{},
		enabled:      map[string]bool{},
		mountedAt:    map[string]string{},
		busy:         map[string]bool{},
		shares:       map[string]string{},
		siteLogs:     map[string]map[string]bool{},
		Fail:         map[string]string{},
		Stat:         map[string]bool{},
		ReadFile:     map[string]string{},
		ToolOutput:   map[string]string{},
		StreamOutput: map[string]string{},
		DirEntries:   map[string][]string{},
		Shadow:       map[string]string{},
		UnitHost:     DefaultUnitHost(),
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
		// A failing tool answers as the agent does: the stream was read,
		// the tool's exit code and output come back.
		msg, failing := a.Fail[r.URL.Path]
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if failing {
			json.NewEncoder(w).Encode(agent.StreamResponse{ExitCode: 2, Output: msg}) //nolint:errcheck // test double
			return
		}
		json.NewEncoder(w).Encode(agent.StreamResponse{}) //nolint:errcheck // test double
		return
	}
	body, _ := readAll(r)
	a.mu.Lock()
	before := a.before
	a.mu.Unlock()
	if before != nil {
		before(r.URL.Path, body)
	}
	a.mu.Lock()
	a.calls = append(a.calls, Call{Path: r.URL.Path, Body: body})
	msg, failing := a.Fail[r.URL.Path]
	status := a.FailStatus[r.URL.Path]
	if r.URL.Path == "/v1/pkg" && !failing {
		var req agent.PkgRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		for _, p := range req.Packages {
			if out, ok := a.InstallFails[p]; ok && req.Action == "install" {
				msg, failing = out, true
			}
			if out, ok := a.UpgradeFails[p]; ok && req.Action == "upgrade" {
				msg, failing = out, true
			}
		}
	}
	restartFailed := ""
	if r.URL.Path == "/v1/config/apply" && !failing {
		if out, ok := a.refusedCheck(body); ok {
			msg, failing, status = out, true, http.StatusUnprocessableEntity
		} else {
			restartFailed = a.failedRestart(body)
		}
	}
	a.mu.Unlock()

	if restartFailed != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(agent.Error{Status: http.StatusInternalServerError, Message: "files applied but restart failed", Output: restartFailed}) //nolint:errcheck // test double
		return
	}

	if failing {
		if status == 0 {
			status = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(agent.Error{Status: status, Message: msg, Output: msg}) //nolint:errcheck // test double
		return
	}

	resp := a.respond(r.URL.Path, body)
	w.Header().Set("Content-Type", "application/json")
	if e, ok := resp.(*agent.Error); ok {
		w.WriteHeader(e.Status)
		json.NewEncoder(w).Encode(e) //nolint:errcheck // test double
		return
	}
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
		return agent.PingResponse{OK: true, Version: "test", Hostname: "test-host", Sandbox: a.Sandbox}

	case "/v1/config/apply":
		var req agent.ApplyConfigSetRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		// Like the agent: the units to reload or restart pass the service
		// policy before anything is written.
		if err := agent.CheckReloads(req.Reload, req.Restart); err != nil {
			return err
		}
		for _, f := range req.Files {
			if refused(f.Path) {
				return refusal(f.Path)
			}
		}
		written := make([]string, 0, len(req.Files))
		for _, f := range req.Files {
			a.files[f.Path] = f
			if a.WriteThrough != nil && a.WriteThrough(f.Path) {
				os.WriteFile(f.Path, []byte(f.Content), 0o644) //nolint:errcheck // test double
			}
			written = append(written, f.Path)
		}
		for _, u := range req.Reload {
			a.units[u] = "reload"
			a.startInstance(u)
		}
		for _, u := range req.Restart {
			a.units[u] = "restart"
			a.startInstance(u)
			a.restarted(u, "restart")
		}
		return agent.ApplyConfigSetResponse{Written: written, Validated: len(req.Validate), Reloaded: req.Reload}

	case "/v1/units/apply":
		// Checked with the real agent's rules, kept where the real agent
		// writes it: tests read units back with File.
		var req agent.ApplyUnitRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if err := agent.CheckUnit(req.Name, []byte(req.Content), a.UnitHost); err != nil {
			return &agent.Error{Status: http.StatusForbidden, Message: "unit " + req.Name + " is not allowed: " + err.Error()}
		}
		if strings.HasSuffix(req.Name, ".mount") {
			if err := a.mountPointRefusal(unitWhere(req.Content)); err != nil {
				return err
			}
		}
		p := filepath.Join(unitDir, req.Name)
		old, had := a.files[p]
		a.files[p] = agent.FileSpec{Path: p, Content: req.Content, Mode: 0o644, Owner: "root", Group: "root"}
		return agent.ApplyUnitResponse{Path: p, Changed: !had || old.Content != req.Content}

	case "/v1/shares/bind":
		// Checked with the real agent's rules of the text; the folder is
		// reached without a symlink the fake made, the mount point only
		// below directories that are root's alone, as the agent checks
		// them on disk.
		var req agent.ShareBindRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if err := agent.CheckShareBind(req.What, req.Where, a.UnitHost); err != nil {
			return &agent.Error{Status: http.StatusForbidden, Message: "binding " + req.What + " at " + req.Where + " is not allowed: " + err.Error()}
		}
		if l := a.symlinkOnWay(req.What); l != "" {
			return &agent.Error{Status: http.StatusForbidden, Message: "the shared folder " + req.What + " is refused: " + l + " is a symlink or no directory"}
		}
		if err := a.mountPointRefusal(req.Where); err != nil {
			return err
		}
		unit := agent.MountUnitName(req.Where)
		changed := !a.mounted[unit]
		a.shares[req.Where] = req.What
		a.mounted[unit], a.mountedAt[unit] = true, req.Where
		return agent.ShareBindResponse{Mounted: true, Changed: changed}

	case "/v1/shares/unbind":
		// A unit an older panel wrote goes first, then the mount — not one
		// in use — and the record.
		var req agent.ShareUnbindRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		unit := agent.MountUnitName(req.Where)
		var resp agent.ShareUnbindResponse
		if p := filepath.Join(unitDir, unit); a.hasFile(p) {
			delete(a.files, p)
			a.removed = append(a.removed, p)
			a.enabled[unit] = false
			resp.UnitRemoved = true
		}
		if a.mounted[unit] && a.busy[unit] {
			return &agent.Error{Status: http.StatusConflict, Message: "unmounting the shared folder at " + req.Where + ": it is in use"}
		}
		resp.Unmounted = a.mounted[unit]
		a.mounted[unit] = false
		delete(a.mountedAt, unit)
		delete(a.shares, req.Where)
		return resp

	case "/v1/shares/status":
		var req agent.ShareStatusRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		resp := agent.ShareStatusResponse{Entries: []agent.ShareMountState{}}
		for _, p := range req.Paths {
			unit := agent.MountUnitName(p)
			_, recorded := a.shares[p]
			resp.Entries = append(resp.Entries, agent.ShareMountState{Where: p, Mounted: a.mounted[unit] && a.mountedAt[unit] == p, Recorded: recorded, Unit: a.hasFile(filepath.Join(unitDir, unit))})
		}
		return resp

	case "/v1/units/remove":
		var req agent.RemoveUnitRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if err := agent.CheckUnitName(req.Name, a.UnitHost); err != nil {
			return &agent.Error{Status: http.StatusForbidden, Message: "unit " + req.Name + " is not allowed: " + err.Error()}
		}
		p := filepath.Join(unitDir, req.Name)
		a.removed = append(a.removed, p)
		_, had := a.files[p]
		delete(a.files, p)
		return agent.RemoveUnitResponse{Path: p, Removed: had}

	case "/v1/file/ensure":
		var req agent.EnsureFileRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.files[req.Path] = agent.FileSpec{Path: req.Path, Content: req.Content, Mode: req.Mode, Owner: req.Owner}
		return agent.EnsureFileResponse{Written: true}

	case "/v1/sitelogs/ensure":
		// The real agent works in its own log_dir; the fake answers with the
		// default one and remembers the names.
		var req agent.SiteLogsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		dir := filepath.Join(config.Default().LogDir, "sites", req.Login)
		if a.siteLogs[req.Login] == nil {
			a.siteLogs[req.Login] = map[string]bool{}
		}
		created := []string{}
		for _, n := range req.Files {
			if !a.siteLogs[req.Login][n] {
				a.siteLogs[req.Login][n] = true
				created = append(created, filepath.Join(dir, n))
			}
		}
		return agent.SiteLogsResponse{Dir: dir, Created: created}

	case "/v1/sitelogs/remove":
		var req agent.RemoveSiteLogsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.siteLogsGone = append(a.siteLogsGone, req.Login)
		_, had := a.siteLogs[req.Login]
		delete(a.siteLogs, req.Login)
		return agent.RemoveSiteLogsResponse{Removed: had}

	case "/v1/dirs/ensure":
		var req agent.EnsureDirsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		for _, d := range req.Dirs {
			if refused(d.Path) {
				return refusal(d.Path)
			}
			// Like the agent: no symlink on the way below the web root, and
			// the way to a shared folder's mount point stays root's.
			if l := a.symlinkOnWay(d.Path); l != "" {
				return &agent.Error{Status: http.StatusForbidden, Message: "a symlink or a file in the path: refusing " + l}
			}
			prev, had := a.dirSpecs[d.Path]
			next := d
			if next.Owner == "" && had {
				next.Owner = prev.Owner
			}
			if a.belowWebRoot(d.Path) && !rootsAlone(next) && !(had && prev.Owner == next.Owner && prev.Mode == next.Mode) {
				if m, ok := a.mountAtOrBelow(d.Path); ok {
					return shareRefusal("making "+d.Path+" "+next.Owner+"'s", m)
				}
			}
		}
		created := make([]string, 0, len(req.Dirs))
		for _, d := range req.Dirs {
			a.dirs = append(a.dirs, d.Path)
			if prev, had := a.dirSpecs[d.Path]; had && d.Owner == "" {
				d.Owner = prev.Owner
			}
			a.dirSpecs[d.Path] = d
			created = append(created, d.Path)
		}
		return agent.EnsureDirsResponse{Created: created}

	case "/v1/paths/remove":
		var req agent.RemovePathsRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if err := agent.CheckReloads(req.Reload); err != nil {
			return err
		}
		for _, p := range req.Paths {
			if refused(p) {
				return refusal(p)
			}
			if a.belowWebRoot(p) {
				if m, ok := a.mountAtOrBelow(p); ok {
					return shareRefusal("removing "+p, m)
				}
				// rmdir of a directory something is mounted on
				if a.mountedOn(filepath.Clean(p)) {
					return &agent.Error{Status: http.StatusInternalServerError, Message: "remove " + p, Output: "device or resource busy"}
				}
			}
		}
		a.removed = append(a.removed, req.Paths...)
		for _, p := range req.Paths {
			delete(a.dirSpecs, p)
			delete(a.symlinks, p)
		}
		// A real agent reports only what existed, and the panel acts on that
		// (a removed pool means the old php-fpm has to be reloaded). The fake
		// knows what it wrote, so it answers the same way; a directory
		// removed with recursive takes what the fake wrote in it along.
		gone := []string{}
		for _, p := range req.Paths {
			had := false
			if _, ok := a.files[p]; ok {
				delete(a.files, p)
				had = true
			}
			if req.Recursive {
				for f := range a.files {
					if strings.HasPrefix(f, filepath.Clean(p)+"/") {
						delete(a.files, f)
						had = true
					}
				}
			}
			if had {
				gone = append(gone, p)
			}
		}
		// Like the agent: the units reload only when something went.
		if len(gone) > 0 {
			for _, u := range req.Reload {
				a.startInstance(u)
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
		// Like the agent: templates' instances, targets and root shells
		// only report their state.
		if err := agent.CheckService(unit, req.Action); err != nil {
			return err
		}
		if strings.HasSuffix(unit, ".mount") {
			return a.mountService(unit, req.Action)
		}
		if strings.Contains(unit, "@") {
			return a.instanceService(unit, req.Action)
		}
		if unit != "" && req.Action != "" && req.Action != "status" {
			a.units[unit] = req.Action
			a.actions = append(a.actions, unit+" "+req.Action)
		}
		state := "active"
		if req.Action == "stop" || req.Action == "disable" {
			state = "inactive"
		}
		if v, ok := a.ActiveState[unit]; ok && (req.Action == "" || req.Action == "status") {
			state = v
		}
		fileState := "enabled"
		if v, ok := a.UnitFileState[unit]; ok {
			fileState = v
		}
		load := "loaded"
		if l, ok := a.LoadState[unit]; ok {
			load = l
		}
		a.restarted(unit, req.Action)
		return agent.ServiceResponse{Status: systemd.Status{Unit: unit, LoadState: load, ActiveState: state, SubState: "running", UnitFileState: fileState, BinaryReplaced: a.BinaryReplaced[unit] && state == "active"}}

	case "/v1/pkg":
		var req agent.PkgRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		switch req.Action {
		case "candidates":
			return a.candidates(req.Packages)
		case "upgrade":
			return a.upgrade(req.Packages)
		}
		versions := map[string]string{}
		for _, p := range req.Packages {
			if !a.MissingPackages[p] {
				versions[p] = "1.0-test"
				if v, ok := a.PackageVersions[p]; ok {
					versions[p] = v
				}
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

	case "/v1/panel/config":
		// Like the real agent: the panel keys only, edited in the file the
		// panel names (a temporary config.yaml in tests), the rest untouched.
		var req agent.PanelConfigRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.panel = append(a.panel, req)
		for k := range req.Set {
			if !config.IsPanelKey(k) {
				return &agent.Error{Status: http.StatusForbidden, Message: k + " cannot be changed through the agent"}
			}
		}
		resp := agent.PanelConfigResponse{Path: req.Path, Changed: []string{}}
		if req.Path == "" {
			return resp
		}
		old, err := os.ReadFile(req.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return &agent.Error{Status: http.StatusInternalServerError, Message: err.Error()}
		}
		out, changed, err := config.EditPanelKeys(old, req.Set)
		if err != nil {
			return &agent.Error{Status: http.StatusUnprocessableEntity, Message: err.Error()}
		}
		if len(changed) > 0 {
			if err := os.WriteFile(req.Path, out, 0o640); err != nil {
				return &agent.Error{Status: http.StatusInternalServerError, Message: err.Error()}
			}
			resp.Changed = changed
		}
		return resp

	case "/v1/runas":
		var req agent.RunAsUserRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		a.runas = append(a.runas, req)
		if a.RunAsHook != nil {
			if res := a.RunAsHook(req); res != nil {
				return *res
			}
		}
		return agent.RunAsUserResponse{}

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
			if _, wrote := a.files[filepath.Clean(p)]; !exists && wrote && a.StatWritten {
				exists = true
			}
			e := agent.StatEntry{Path: p, Exists: exists, IsDir: isDir, Mode: "0644"}
			// What the fake made below the web root itself: a directory is
			// root's when dirs/ensure made it so, and one with a folder
			// mounted on it shows the folder, an account's.
			if !exists && a.belowWebRoot(p) {
				if d, ok := a.dirSpecs[filepath.Clean(p)]; ok {
					e.Exists, e.IsDir, e.Mode = true, true, "drwxr-xr-x"
					if !rootsAlone(agent.DirSpec{Owner: d.Owner}) || a.mountedOn(filepath.Clean(p)) {
						e.UID, e.GID = 1500, 1500
					}
				} else if _, ok := a.symlinks[filepath.Clean(p)]; ok {
					e.Exists, e.Mode, e.UID, e.GID = true, "Lrwxrwxrwx", 1500, 1500
				}
			}
			entries = append(entries, e)
		}
		return agent.StatResponse{Entries: entries}

	case "/v1/file/read":
		var req agent.ReadFileRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		c := a.ReadFile[req.Path]
		if f, ok := a.files[req.Path]; ok {
			c = f.Content
		}
		return agent.ReadFileResponse{Content: c, Size: int64(len(c))}

	case "/v1/chown":
		var req agent.ChownRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if a.belowWebRoot(req.Path) {
			if m, ok := a.mountAtOrBelow(filepath.Clean(req.Path)); ok {
				return shareRefusal("handing "+req.Path+" to "+req.Owner, m)
			}
		}
		return agent.ChownResponse{Changed: 1}

	case "/v1/symlink/ensure":
		var req agent.EnsureSymlinkRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		p := filepath.Clean(req.Path)
		if a.belowWebRoot(p) {
			if m, ok := a.mountAtOrBelow(p); ok {
				return shareRefusal("a symlink at "+p, m)
			}
		}
		if _, dir := a.dirSpecs[p]; dir {
			return &agent.Error{Status: http.StatusForbidden, Message: "exists and is not a symlink: " + p}
		}
		if _, had := a.symlinks[p]; !had || !req.OnlyIfMissing {
			a.symlinks[p] = req.Target
		}
		return map[string]any{}

	case "/v1/user/password":
		// A hash set with chpasswd -e is what UnixShadow reads back later.
		var req agent.SetUnixPasswordRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if req.Encrypted {
			a.Shadow[req.Login] = req.Password
		}
		return map[string]any{}

	case "/v1/user/shadow":
		var req agent.UnixShadowRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if h, ok := a.Shadow[req.Login]; ok {
			return agent.UnixShadowResponse{Hash: h}
		}
		return agent.UnixShadowResponse{}

	case "/v1/system/info":
		return map[string]any{"hostname": "test-host", "cpus": 2, "mem_total_bytes": a.MemTotalBytes, "container": a.Container}

	case "/v1/apache/ctl":
		// Like the agent: switching something of the distribution's Apache
		// on is refused.
		var req agent.ApacheCtlRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if _, err := agent.CheckApacheCtl(req.Action, req.Name); err != nil {
			var ae *agent.Error
			if errors.As(err, &ae) {
				return ae
			}
			return &agent.Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		a.apacheCtls = append(a.apacheCtls, req)
		return agent.ApacheCtlResponse{Output: req.Action + " " + req.Name + ": done\n"}

	case "/v1/apache/legacy":
		// The real agent's reading, of the directory a test points it at:
		// on a host it is root's /etc/apache2/monopanel/sites.
		var req agent.ApacheLegacyRequest
		json.Unmarshal(body, &req) //nolint:errcheck // test double
		if a.ApacheLegacySites == "" {
			return agent.ApacheLegacyResponse{}
		}
		dir, err := filepath.EvalSymlinks(a.ApacheLegacySites)
		if err != nil {
			return agent.ApacheLegacyResponse{}
		}
		res, err := agent.ReadApacheLegacy(dir, req.Domain)
		if err != nil {
			var ae *agent.Error
			if errors.As(err, &ae) {
				return ae
			}
			return &agent.Error{Status: http.StatusInternalServerError, Message: err.Error()}
		}
		return res
	}
	return map[string]any{}
}

// instanceState is what the fake knows of a template instance.
type instanceState struct{ active, enabled bool }

// instanceConfigured tells whether the unit has what it needs to start: an
// account's Apache its httpd.conf. The caller holds mu.
func (a *Agent) instanceConfigured(unit string) bool {
	if login, ok := confcheck.ApacheInstanceLoginOf(unit); ok {
		_, has := a.files[confcheck.ApacheInstanceConf(login)]
		return has
	}
	return true
}

// startInstance starts a template instance that config/apply or
// paths/remove reloads (reload-or-restart starts a stopped unit). The caller
// holds mu.
func (a *Agent) startInstance(unit string) {
	if !strings.Contains(unit, "@") {
		return
	}
	if !strings.Contains(unit, ".") {
		unit += ".service"
	}
	st := a.instance(unit)
	st.active = a.instanceConfigured(unit)
}

// instance is the state of a template instance, inactive and disabled until
// the panel acts on it. The caller holds mu.
func (a *Agent) instance(unit string) *instanceState {
	st, ok := a.instances[unit]
	if !ok {
		st = &instanceState{}
		a.instances[unit] = st
	}
	return st
}

// instanceService is /v1/service for a template instance. The caller holds
// mu.
func (a *Agent) instanceService(unit, action string) any {
	st := a.instance(unit)
	switch action {
	case "start", "restart", "reload-or-restart":
		st.active = a.instanceConfigured(unit)
	case "reload":
		if !st.active {
			return &agent.Error{Status: http.StatusBadGateway, Message: "reload " + unit + " failed", Output: "Unit " + unit + " is not active."}
		}
	case "stop":
		st.active = false
	case "enable":
		st.enabled = true
	case "disable":
		st.enabled = false
	}
	if action != "" && action != "status" {
		a.units[unit] = action
		a.actions = append(a.actions, unit+" "+action)
	}
	a.restarted(unit, action)
	status := systemd.Status{Unit: unit, LoadState: "loaded", ActiveState: "inactive", SubState: "dead", UnitFileState: "disabled"}
	if l, ok := a.LoadState[unit]; ok {
		status.LoadState = l
	}
	if st.active {
		status.ActiveState, status.SubState = "active", "running"
		status.BinaryReplaced = a.BinaryReplaced[unit]
	}
	if st.enabled {
		status.UnitFileState = "enabled"
	}
	return agent.ServiceResponse{Status: status}
}

// InstanceState tells whether a template instance runs and is enabled.
func (a *Agent) InstanceState(unit string) (active, enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.instances[unit]; ok {
		return st.active, st.enabled
	}
	return false, false
}

// SetInstance puts a template instance in a state: the one a host was left
// in.
func (a *Agent) SetInstance(unit string, active, enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.instances[unit] = &instanceState{active: active, enabled: enabled}
}

// SetBefore has fn run ahead of every call the fake answers (path and body
// as they came), outside its lock: a test can hold one call back while
// others go on. nil removes it.
func (a *Agent) SetBefore(fn func(path string, body []byte)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.before = fn
}

// ApacheCtls returns the /v1/apache/ctl calls the fake ran.
func (a *Agent) ApacheCtls() []agent.ApacheCtlRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.ApacheCtlRequest{}, a.apacheCtls...)
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
	if ok && f.Content == "" && f.ContentBase64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(f.ContentBase64); err == nil {
			return string(raw), true
		}
	}
	return f.Content, ok
}

// FileSpec returns how a path was last written (content, mode, owner), and
// whether it was.
func (a *Agent) FileSpec(path string) (agent.FileSpec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.files[path]
	return f, ok
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

// ServiceActions returns the actions asked of /v1/service, "<unit> <action>"
// in the order they came; status queries are left out.
func (a *Agent) ServiceActions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.actions...)
}

// RunAs returns the recorded run-as-user (fsop) invocations.
func (a *Agent) RunAs() []agent.RunAsUserRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.RunAsUserRequest{}, a.runas...)
}

// PanelConfigs returns the recorded changes of config.yaml (/v1/panel/config).
func (a *Agent) PanelConfigs() []agent.PanelConfigRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.PanelConfigRequest{}, a.panel...)
}

// SiteLogs returns the names of the site logs the panel had the agent
// prepare for an account, sorted.
func (a *Agent) SiteLogs(login string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for n := range a.siteLogs[login] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SiteLogsRemoved returns the logins whose log directory the panel had the
// agent remove.
func (a *Agent) SiteLogsRemoved() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.siteLogsGone...)
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
	a.actions = nil
	a.instances = map[string]*instanceState{}
	a.apacheCtls = nil
	a.dirs = nil
	a.dirSpecs = map[string]agent.DirSpec{}
	a.symlinks = map[string]string{}
	a.mounted = map[string]bool{}
	a.enabled = map[string]bool{}
	a.mountedAt = map[string]string{}
	a.busy = map[string]bool{}
	a.shares = map[string]string{}
	a.tools = nil
	a.upgraded = nil
	a.panel = nil
	a.siteLogs = map[string]map[string]bool{}
	a.siteLogsGone = nil
}

// DirSpec returns how dirs/ensure was last told to make a directory (an
// empty owner keeps the one told before), and whether it was.
func (a *Agent) DirSpec(path string) (agent.DirSpec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.dirSpecs[path]
	return d, ok
}

// Symlink returns the target of a symlink symlink/ensure made (and paths/remove
// did not take away since), and whether there is one.
func (a *Agent) Symlink(path string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.symlinks[path]
	return t, ok
}

// Mounted reports whether a folder is mounted by the name of its unit.
func (a *Agent) Mounted(unit string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mounted[unit]
}

// SetUnit puts a unit file where the agent keeps the panel's units, enabled
// and, for a mount unit, mounted when asked: the state an older panel left.
func (a *Agent) SetUnit(name, content string, mounted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := filepath.Join(unitDir, name)
	a.files[p] = agent.FileSpec{Path: p, Content: content, Mode: 0o644, Owner: "root", Group: "root"}
	a.enabled[name] = true
	a.mounted[name] = mounted
	if mounted {
		a.mountedAt[name] = unitWhere(content)
	}
}

// SetShare is what the agent keeps of a shared folder it bound — or took
// over from an older panel's unit as it started: its record, the folder
// mounted when asked (not after a reboot).
func (a *Agent) SetShare(where, what string, mounted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unit := agent.MountUnitName(where)
	a.shares[where] = what
	a.mounted[unit] = mounted
	if mounted {
		a.mountedAt[unit] = where
	} else {
		delete(a.mountedAt, unit)
	}
}

// Share returns the folder the agent recorded at the mount point where, and
// whether it did.
func (a *Agent) Share(where string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	what, ok := a.shares[where]
	return what, ok
}

// SetBusy marks the folder mounted by the name unit as in use (a session
// with its working directory inside): unmounting it fails, and so does
// removing its mount point, until SetBusy(unit, false).
func (a *Agent) SetBusy(unit string, busy bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.busy[unit] = busy
}

// Unmount takes a mount away without its unit, as a reboot does with a
// mount whose unit is gone.
func (a *Agent) Unmount(unit string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mounted[unit] = false
	delete(a.mountedAt, unit)
}

// mountedOn tells whether a started mount unit is mounted at p. The caller
// holds mu.
func (a *Agent) mountedOn(p string) bool {
	for unit, where := range a.mountedAt {
		if a.mounted[unit] && where == p {
			return true
		}
	}
	return false
}

// SetSymlink puts a symlink at p, as an account may in its own tree: the
// fake refuses to go through it below the web root, as the agent does.
func (a *Agent) SetSymlink(p, target string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.symlinks[filepath.Clean(p)] = target
}

// SetDir records a directory as dirs/ensure would have made it: the state an
// older panel left.
func (a *Agent) SetDir(d agent.DirSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dirSpecs[d.Path] = d
}

// mountService is /v1/service for a mount unit: one below the web root is
// never started, restarted, reloaded or enabled — the agent binds the shared
// folders itself — another needs its unit file; status says not-found for a
// unit that is neither written nor mounted, as systemd does, and loaded for
// a mount systemd found in the mount table. The caller holds mu.
func (a *Agent) mountService(unit, action string) any {
	f, has := a.files[filepath.Join(unitDir, unit)]
	switch action {
	case "start", "restart", "reload", "reload-or-restart", "enable":
		if strings.HasPrefix(unit, strings.TrimSuffix(agent.MountUnitName(a.UnitHost.WWWRoot), ".mount")+"-") {
			return &agent.Error{Status: http.StatusForbidden, Message: action + " of " + unit + " is not allowed: the agent binds the shared folders itself"}
		}
		if !has {
			return &agent.Error{Status: http.StatusBadGateway, Message: action + " " + unit + " failed", Output: "Unit " + unit + " not found."}
		}
		if action == "enable" {
			a.enabled[unit] = true
		} else {
			a.mounted[unit] = true
			a.mountedAt[unit] = unitWhere(f.Content)
		}
	case "stop":
		if a.mounted[unit] && a.busy[unit] {
			return &agent.Error{Status: http.StatusBadGateway, Message: "stop " + unit + " failed", Output: "umount: target is busy"}
		}
		a.mounted[unit] = false
		delete(a.mountedAt, unit)
	case "disable":
		a.enabled[unit] = false
	}
	if action != "" && action != "status" {
		a.units[unit] = action
	}
	st := systemd.Status{Unit: unit, LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}
	if has {
		st.LoadState, st.UnitFileState = "loaded", "disabled"
		if a.enabled[unit] {
			st.UnitFileState = "enabled"
		}
	}
	if a.mounted[unit] {
		st.LoadState, st.ActiveState, st.SubState = "loaded", "active", "mounted"
	}
	return agent.ServiceResponse{Status: st}
}

// hasFile tells whether the fake keeps a file at p. The caller holds mu.
func (a *Agent) hasFile(p string) bool {
	_, ok := a.files[p]
	return ok
}

// mountPointRefusal is the agent's answer to a mount point whose way the
// panel made someone else's than root's, writable by others or a symlink;
// nil when it did not. The caller holds mu.
func (a *Agent) mountPointRefusal(where string) *agent.Error {
	root := filepath.Clean(a.UnitHost.WWWRoot)
	for p := filepath.Clean(where); strings.HasPrefix(p, root+"/"); p = filepath.Dir(p) {
		if _, link := a.symlinks[p]; link {
			return &agent.Error{Status: http.StatusForbidden, Message: "the mount point " + where + " is refused: " + p + " is a symlink"}
		}
		if d, ok := a.dirSpecs[p]; ok && !rootsAlone(d) {
			return &agent.Error{Status: http.StatusForbidden, Message: "the mount point " + where + " is refused: " + p + " is not root's alone"}
		}
	}
	return nil
}

// mountAtOrBelow returns a mount point at or below p the agent knows of: a
// shared folder it recorded, or a mount unit the fake keeps. The caller holds
// mu.
func (a *Agent) mountAtOrBelow(p string) (string, bool) {
	p = filepath.Clean(p)
	for w := range a.shares {
		if w == p || strings.HasPrefix(w, p+"/") {
			return w, true
		}
	}
	for path, f := range a.files {
		if filepath.Dir(path) != unitDir || !strings.HasSuffix(path, ".mount") {
			continue
		}
		if w := unitWhere(f.Content); w != "" && (w == p || strings.HasPrefix(w, p+"/")) {
			return w, true
		}
	}
	return "", false
}

// symlinkOnWay names a symlink the fake made at p or on the way to it. The
// caller holds mu.
func (a *Agent) symlinkOnWay(p string) string {
	for q := filepath.Clean(p); q != "/" && q != "."; q = filepath.Dir(q) {
		if _, ok := a.symlinks[q]; ok {
			return q
		}
	}
	return ""
}

func (a *Agent) belowWebRoot(p string) bool {
	return strings.HasPrefix(filepath.Clean(p), filepath.Clean(a.UnitHost.WWWRoot)+"/")
}

// rootsAlone tells a directory spec that leaves it root's and writable by
// nobody else (an empty owner is taken for root's).
func rootsAlone(d agent.DirSpec) bool {
	return (d.Owner == "" || d.Owner == "root") && (d.Mode&0o1000 != 0 || d.Mode&0o022 == 0)
}

// shareRefusal is the agent's 409 for a change on the way to a mount point.
func shareRefusal(what, mount string) *agent.Error {
	return &agent.Error{Status: http.StatusConflict, Message: what + " is not allowed: the shared folder mounted at " + mount + " lies there; remove or move the shared folders first"}
}

// unitWhere is the Where= of a mount unit's text.
func unitWhere(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if w, ok := strings.CutPrefix(strings.TrimSpace(line), "Where="); ok {
			return strings.TrimSpace(w)
		}
	}
	return ""
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
