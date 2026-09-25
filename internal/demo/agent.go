// Package demo runs the panel as a public live demo: the real API and web UI
// on top of an agent that only pretends. Packages, services, unix accounts and
// configuration files live in memory; the site files under the www root are
// real, so the file manager and the editor work. Nothing is executed on the
// host and nothing reaches the network. It is linked only into the demo build
// (go build -tags demo), never into the released binary.
package demo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"monopanel/internal/agent"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

// Hostname is the name the demo server goes by, as on the documentation's
// screenshots.
const Hostname = "web-01"

// Agent answers the agent protocol without touching the host.
type Agent struct {
	cfg  config.Config
	self string // the monopanel binary: the file manager runs its fsop
	log  *slog.Logger
	boot time.Time

	mu sync.Mutex
	st state
}

// state is what the pretend server holds; it is saved with the prepared demo
// and loaded when a container starts.
type state struct {
	Files    map[string]*vfile     `json:"files"`
	Packages map[string]string     `json:"packages"`
	Units    map[string]*unitState `json:"units"`
	Users    map[string]*unixUser  `json:"users"`
	Groups   map[string]int        `json:"groups"`
	NextID   int                   `json:"next_id"`
	Crontabs map[string]string     `json:"crontabs"`
	Firewall bool                  `json:"firewall"`
	MySQL    map[string]bool       `json:"mysql"`
	Restic   []resticSnapshot      `json:"restic"`
}

// vfile is a file or a directory outside the www root.
type vfile struct {
	Dir     bool      `json:"dir,omitempty"`
	Content string    `json:"content,omitempty"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"mtime"`
}

type unitState struct {
	Active  bool      `json:"active"`
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since"`
}

type unixUser struct {
	UID    int      `json:"uid"`
	GID    int      `json:"gid"`
	Home   string   `json:"home"`
	Shell  string   `json:"shell"`
	Hash   string   `json:"hash"`
	Groups []string `json:"groups,omitempty"`
}

// NewAgent makes an empty pretend server.
func NewAgent(cfg config.Config, self string, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{cfg: cfg, self: self, log: log, boot: time.Now(), st: state{
		Files: map[string]*vfile{}, Packages: map[string]string{}, Units: map[string]*unitState{},
		Users: map[string]*unixUser{}, Groups: map[string]int{}, NextID: 1000,
		Crontabs: map[string]string{}, MySQL: map[string]bool{},
	}}
}

// Save writes the state for the next start.
func (a *Agent) Save(file string) error {
	a.mu.Lock()
	b, err := json.Marshal(a.st)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(file, b, 0o600)
}

// Load reads a saved state.
func (a *Agent) Load(file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return json.Unmarshal(b, &a.st)
}

// ListenAndServe answers on the agent socket until ctx ends.
func (a *Agent) ListenAndServe(ctx context.Context) error {
	sock := a.cfg.AgentSocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the agent's RPC router.
func (a *Agent) Handler() http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/ping", handle(a.ping))
	r.Post("/v1/system/info", handle(a.systemInfo))
	r.Post("/v1/config/apply", handle(a.applyConfigSet))
	r.Post("/v1/group/ensure", handle(a.ensureGroup))
	r.Post("/v1/user/ensure", handle(a.ensureUnixUser))
	r.Post("/v1/dirs/ensure", handle(a.ensureDirs))
	r.Post("/v1/file/ensure", handle(a.ensureFile))
	r.Post("/v1/symlink/ensure", handle(a.ensureSymlink))
	r.Post("/v1/acl/set", handle(ok[agent.SetACLRequest]))
	r.Post("/v1/acl/site", handle(func(context.Context, *agent.SiteACLRequest) (*agent.SiteACLResponse, error) {
		return &agent.SiteACLResponse{}, nil
	}))
	r.Post("/v1/paths/remove", handle(a.removePaths))
	r.Post("/v1/apache/ctl", handle(a.apacheCtl))
	r.Post("/v1/tool", handle(a.tool))
	r.Post("/v1/stat", handle(a.stat))
	r.Post("/v1/dir/list", handle(a.listDir))
	r.Post("/v1/file/read", handle(a.readFile))
	r.Post("/v1/user/password", handle(a.setUnixPassword))
	r.Post("/v1/user/remove", handle(a.removeUnixUser))
	r.Post("/v1/runas", handle(a.runAsUser))
	r.Post("/v1/service", handle(a.service))
	r.Post("/v1/pkg", handle(a.pkg))
	r.Post("/v1/panel/install", handle(a.installPanel))
	r.Post("/v1/chown", handle(a.chown))
	r.Post("/v1/user/shadow", handle(a.unixShadow))
	r.Post("/v1/stream/out", notInDemo)
	r.Post("/v1/stream/in", notInDemo)
	return r
}

// ErrNotInDemo is what an operation says when the demo cannot pretend it.
const ErrNotInDemo = "not available in the demo"

func notInDemo(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, &agent.Error{Status: http.StatusForbidden, Message: ErrNotInDemo})
}

func handle[Req, Resp any](fn func(context.Context, *Req) (*Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, &agent.Error{Status: http.StatusBadRequest, Message: "bad request: " + err.Error()})
				return
			}
		}
		resp, err := fn(r.Context(), &req)
		if err != nil {
			var ae *agent.Error
			if !errors.As(err, &ae) {
				ae = &agent.Error{Message: err.Error()}
			}
			if ae.Status == 0 {
				ae.Status = http.StatusInternalServerError
			}
			writeErr(w, ae)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func writeErr(w http.ResponseWriter, e *agent.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

func ok[Req any](context.Context, *Req) (*struct{}, error) { return &struct{}{}, nil }

// real tells whether a path is a site file on disk rather than a pretend one.
func (a *Agent) real(p string) bool {
	root := filepath.Clean(a.cfg.WWWRoot)
	p = filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+"/")
}

// mkdirs creates a pretend directory with its parents; the caller holds mu.
func (a *Agent) mkdirs(p string) {
	for p = path.Clean(p); p != "/" && p != "."; p = path.Dir(p) {
		if f, ok := a.st.Files[p]; ok && f.Dir {
			return
		}
		a.st.Files[p] = &vfile{Dir: true, Mode: 0o755, ModTime: time.Now()}
	}
}

// put stores a pretend file and reports whether its content changed; the
// caller holds mu.
func (a *Agent) put(p, content string, mode uint32) bool {
	p = path.Clean(p)
	if f, ok := a.st.Files[p]; ok && !f.Dir && f.Content == content {
		return false
	}
	if mode == 0 {
		mode = 0o644
	}
	a.mkdirs(path.Dir(p))
	a.st.Files[p] = &vfile{Content: content, Mode: mode, ModTime: time.Now()}
	return true
}

func (a *Agent) ping(context.Context, *struct{}) (*agent.PingResponse, error) {
	return &agent.PingResponse{OK: true, Version: buildinfo.Version, Hostname: Hostname, PeerUID: 0}, nil
}

// Release is the system the demo pretends to be: Ubuntu 24.04, like the
// screenshots in the documentation.
var Release = osprofile.Release{ID: "ubuntu", IDLike: []string{"debian"}, VersionID: "24.04", Codename: "noble", PrettyName: "Ubuntu 24.04.4 LTS", Arch: "amd64"}

// systemInfo is a 4-CPU, 8 GB host with a gentle daily rhythm, so the
// dashboard's gauges move like a server's do.
func (a *Agent) systemInfo(context.Context, *struct{}) (*agent.SystemInfoResponse, error) {
	now := time.Now()
	load, used := HostLoad(now)
	const gb = 1 << 30
	return &agent.SystemInfoResponse{
		Hostname: Hostname, Family: "debian", Release: Release, Kernel: "6.8.0-138-generic",
		UptimeSeconds: now.Sub(a.boot).Seconds() + 23*86400 + 4*3600,
		Load:          [3]float64{load, load * 0.9, load * 0.8}, CPUs: 4,
		MemTotalBytes: 8 * gb, MemAvailableBytes: 8*gb - used,
		SwapTotalBytes: 2 * gb, SwapFreeBytes: 2*gb - 96<<20,
		Disks: []sysinfo.Disk{{Mount: "/", TotalBytes: 80 * gb, FreeBytes: 51*gb + 300<<20}},
		Time:  now,
	}, nil
}

// HostLoad is the pretend load average and used memory at a moment: a busier
// day than night and a little noise.
func HostLoad(t time.Time) (load float64, memUsed uint64) {
	day := math.Sin(2 * math.Pi * float64(t.Hour()*60+t.Minute()) / 1440)
	noise := math.Sin(float64(t.Unix()/60)*1.7) * 0.12
	load = 0.45 + 0.25*day + noise
	if load < 0.05 {
		load = 0.05
	}
	memUsed = uint64((2.6 + 0.35*day + noise) * (1 << 30))
	return math.Round(load*100) / 100, memUsed
}

func (a *Agent) applyConfigSet(_ context.Context, req *agent.ApplyConfigSetRequest) (*agent.ApplyConfigSetResponse, error) {
	resp := &agent.ApplyConfigSetResponse{Written: []string{}, Unchanged: []string{}, Validated: len(req.Validate)}
	for _, f := range req.Files {
		content := f.Content
		if f.ContentBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
			if err != nil {
				return nil, &agent.Error{Status: http.StatusBadRequest, Message: "content_base64: " + err.Error()}
			}
			content = string(b)
		}
		changed, err := a.write(f.Path, content, f.Mode)
		if err != nil {
			return nil, err
		}
		if changed {
			resp.Written = append(resp.Written, f.Path)
		} else {
			resp.Unchanged = append(resp.Unchanged, f.Path)
		}
	}
	if len(resp.Written) > 0 || req.Force {
		a.mu.Lock()
		for _, u := range append(append([]string{}, req.Reload...), req.Restart...) {
			a.unit(u).start()
		}
		a.mu.Unlock()
		resp.Reloaded = append(append([]string{}, req.Reload...), req.Restart...)
	}
	return resp, nil
}

// write stores a file: a real one below the www root, a pretend one elsewhere.
func (a *Agent) write(p, content string, mode uint32) (bool, error) {
	if a.real(p) {
		if old, err := os.ReadFile(p); err == nil && string(old) == content {
			return false, nil
		}
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		return true, os.WriteFile(p, []byte(content), os.FileMode(mode))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.put(p, content, mode), nil
}

func (a *Agent) ensureGroup(_ context.Context, req *agent.EnsureGroupRequest) (*agent.EnsureGroupResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gid, ok := a.st.Groups[req.Name]; ok {
		return &agent.EnsureGroupResponse{GID: gid}, nil
	}
	gid := a.nextID(req.System)
	a.st.Groups[req.Name] = gid
	return &agent.EnsureGroupResponse{GID: gid, Created: true}, nil
}

// nextID hands out ids: system accounts below 1000, people from 1000; the
// caller holds mu.
func (a *Agent) nextID(system bool) int {
	if system {
		return 900 + len(a.st.Groups)%99
	}
	a.st.NextID++
	return a.st.NextID
}

func (a *Agent) ensureUnixUser(_ context.Context, req *agent.EnsureUnixUserRequest) (*agent.EnsureUnixUserResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.st.Users[req.Login]; ok {
		if req.UpdateShell && req.Shell != "" {
			u.Shell = req.Shell
		}
		u.Groups = mergeGroups(u.Groups, req.Groups, req.RemoveGroups)
		return &agent.EnsureUnixUserResponse{UID: u.UID, GID: u.GID, Home: u.Home}, nil
	}
	id := a.nextID(req.System)
	home := req.Home
	if home == "" {
		home = "/home/" + req.Login
	}
	u := &unixUser{UID: id, GID: id, Home: home, Shell: req.Shell, Groups: mergeGroups(nil, req.Groups, nil)}
	if req.PrimaryGroup != "" {
		if gid, ok := a.st.Groups[req.PrimaryGroup]; ok {
			u.GID = gid
		}
	} else {
		a.st.Groups[req.Login] = id
	}
	a.st.Users[req.Login] = u
	if req.CreateHome && a.real(home) {
		if err := os.MkdirAll(home, 0o750); err != nil {
			return nil, err
		}
	}
	return &agent.EnsureUnixUserResponse{UID: u.UID, GID: u.GID, Home: home, Created: true}, nil
}

func mergeGroups(have, add, remove []string) []string {
	set := map[string]bool{}
	for _, g := range append(have, add...) {
		set[g] = true
	}
	for _, g := range remove {
		delete(set, g)
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func (a *Agent) ensureDirs(_ context.Context, req *agent.EnsureDirsRequest) (*agent.EnsureDirsResponse, error) {
	resp := &agent.EnsureDirsResponse{Created: []string{}}
	for _, d := range req.Dirs {
		if a.real(d.Path) {
			if _, err := os.Stat(d.Path); err == nil {
				continue
			}
			mode := os.FileMode(d.Mode)
			if mode == 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(d.Path, mode); err != nil {
				return nil, err
			}
			resp.Created = append(resp.Created, d.Path)
			continue
		}
		a.mu.Lock()
		if _, ok := a.st.Files[path.Clean(d.Path)]; !ok {
			a.mkdirs(d.Path)
			resp.Created = append(resp.Created, d.Path)
		}
		a.mu.Unlock()
	}
	return resp, nil
}

func (a *Agent) ensureFile(_ context.Context, req *agent.EnsureFileRequest) (*agent.EnsureFileResponse, error) {
	content := req.Content
	if req.ContentBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			return nil, &agent.Error{Status: http.StatusBadRequest, Message: "content_base64: " + err.Error()}
		}
		content = string(b)
	}
	if req.OnlyIfMissing || req.OnlyIfDirEmpty {
		if st, _ := a.stat(context.Background(), &agent.StatRequest{Paths: []string{req.Path}}); st != nil && st.Entries[0].Exists {
			return &agent.EnsureFileResponse{}, nil
		}
		if req.OnlyIfDirEmpty {
			if list, err := a.listDir(context.Background(), &agent.ListDirRequest{Path: path.Dir(req.Path)}); err == nil && len(list.Entries) > 0 {
				return &agent.EnsureFileResponse{}, nil
			}
		}
	}
	written, err := a.write(req.Path, content, req.Mode)
	if err != nil {
		return nil, err
	}
	return &agent.EnsureFileResponse{Written: written}, nil
}

func (a *Agent) ensureSymlink(_ context.Context, req *agent.EnsureSymlinkRequest) (*struct{}, error) {
	if a.real(req.Path) {
		if _, err := os.Lstat(req.Path); err == nil {
			if req.OnlyIfMissing {
				return &struct{}{}, nil
			}
			_ = os.Remove(req.Path)
		}
		if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
			return nil, err
		}
		return &struct{}{}, os.Symlink(req.Target, req.Path)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.put(req.Path, "", 0o777)
	return &struct{}{}, nil
}

func (a *Agent) removePaths(_ context.Context, req *agent.RemovePathsRequest) (*agent.RemovePathsResponse, error) {
	resp := &agent.RemovePathsResponse{Removed: []string{}}
	for _, p := range req.Paths {
		if a.real(p) {
			var err error
			if req.Recursive {
				err = os.RemoveAll(p)
			} else {
				err = os.Remove(p)
			}
			if err == nil {
				resp.Removed = append(resp.Removed, p)
			}
			continue
		}
		a.mu.Lock()
		clean := path.Clean(p)
		if _, ok := a.st.Files[clean]; ok {
			delete(a.st.Files, clean)
			for name := range a.st.Files {
				if strings.HasPrefix(name, clean+"/") {
					delete(a.st.Files, name)
				}
			}
			resp.Removed = append(resp.Removed, p)
		}
		a.mu.Unlock()
	}
	return resp, nil
}

func (a *Agent) apacheCtl(_ context.Context, req *agent.ApacheCtlRequest) (*agent.ApacheCtlResponse, error) {
	return &agent.ApacheCtlResponse{Output: fmt.Sprintf("%s %s: done\n", req.Action, req.Name)}, nil
}

func (a *Agent) stat(_ context.Context, req *agent.StatRequest) (*agent.StatResponse, error) {
	resp := &agent.StatResponse{Entries: make([]agent.StatEntry, 0, len(req.Paths))}
	for _, p := range req.Paths {
		e := agent.StatEntry{Path: p}
		if a.real(p) {
			if fi, err := os.Lstat(p); err == nil {
				e.Exists, e.IsDir, e.Size, e.Mode, e.ModTime = true, fi.IsDir(), fi.Size(), fmt.Sprintf("%04o", fi.Mode().Perm()), fi.ModTime().UTC().Format(time.RFC3339)
			}
		} else {
			a.mu.Lock()
			if f, ok := a.st.Files[path.Clean(p)]; ok {
				e.Exists, e.IsDir, e.Size, e.Mode, e.ModTime = true, f.Dir, int64(len(f.Content)), fmt.Sprintf("%04o", f.Mode), f.ModTime.UTC().Format(time.RFC3339)
			}
			a.mu.Unlock()
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

func (a *Agent) listDir(_ context.Context, req *agent.ListDirRequest) (*agent.ListDirResponse, error) {
	resp := &agent.ListDirResponse{Entries: []agent.ListDirEntry{}}
	if a.real(req.Path) {
		entries, err := os.ReadDir(req.Path)
		if err != nil {
			return nil, &agent.Error{Status: http.StatusNotFound, Message: err.Error()}
		}
		for _, e := range entries {
			resp.Entries = append(resp.Entries, agent.ListDirEntry{Name: e.Name(), IsDir: e.IsDir()})
		}
		return resp, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	dir := path.Clean(req.Path)
	if f, ok := a.st.Files[dir]; !ok || !f.Dir {
		return nil, &agent.Error{Status: http.StatusNotFound, Message: "open " + dir + ": no such file or directory"}
	}
	for name, f := range a.st.Files {
		if path.Dir(name) == dir && name != dir {
			resp.Entries = append(resp.Entries, agent.ListDirEntry{Name: path.Base(name), IsDir: f.Dir})
		}
	}
	sort.Slice(resp.Entries, func(i, j int) bool { return resp.Entries[i].Name < resp.Entries[j].Name })
	return resp, nil
}

func (a *Agent) readFile(_ context.Context, req *agent.ReadFileRequest) (*agent.ReadFileResponse, error) {
	var content string
	if a.real(req.Path) {
		b, err := os.ReadFile(req.Path)
		if err != nil {
			return nil, &agent.Error{Status: http.StatusNotFound, Message: err.Error()}
		}
		content = string(b)
	} else {
		a.mu.Lock()
		f, ok := a.st.Files[path.Clean(req.Path)]
		a.mu.Unlock()
		if !ok || f.Dir {
			return nil, &agent.Error{Status: http.StatusNotFound, Message: "open " + req.Path + ": no such file or directory"}
		}
		content = f.Content
	}
	resp := &agent.ReadFileResponse{Content: content, Size: int64(len(content))}
	if req.TailBytes > 0 && int64(len(content)) > req.TailBytes {
		resp.Content, resp.Truncated = content[int64(len(content))-req.TailBytes:], true
	}
	return resp, nil
}

func (a *Agent) setUnixPassword(_ context.Context, req *agent.SetUnixPasswordRequest) (*struct{}, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.st.Users[req.Login]
	if !ok {
		return nil, &agent.Error{Status: http.StatusBadRequest, Message: "unknown user " + req.Login}
	}
	u.Hash = req.Password
	if !req.Encrypted {
		u.Hash = "$y$j9T$demo$" + base64.RawStdEncoding.EncodeToString([]byte(req.Login))
	}
	return &struct{}{}, nil
}

func (a *Agent) unixShadow(_ context.Context, req *agent.UnixShadowRequest) (*agent.UnixShadowResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.st.Users[req.Login]; ok {
		return &agent.UnixShadowResponse{Hash: u.Hash}, nil
	}
	return nil, &agent.Error{Status: http.StatusBadRequest, Message: "unknown user " + req.Login}
}

func (a *Agent) removeUnixUser(_ context.Context, req *agent.RemoveUnixUserRequest) (*agent.RemoveUnixUserResponse, error) {
	a.mu.Lock()
	u, ok := a.st.Users[req.Login]
	delete(a.st.Users, req.Login)
	delete(a.st.Groups, req.Login)
	delete(a.st.Crontabs, req.Login)
	a.mu.Unlock()
	resp := &agent.RemoveUnixUserResponse{Removed: ok}
	home := req.Home
	if home == "" && ok {
		home = u.Home
	}
	if req.RemoveHome && home != "" && a.real(home) && home != filepath.Clean(a.cfg.WWWRoot) {
		resp.HomeRemoved = os.RemoveAll(home) == nil
	}
	return resp, nil
}

// runAsUser runs the file manager's fsop over the account's real directory.
// There is no helper dropping privileges: the container is the sandbox, and
// it holds nothing but this demo.
func (a *Agent) runAsUser(ctx context.Context, req *agent.RunAsUserRequest) (*agent.RunAsUserResponse, error) {
	a.mu.Lock()
	u, ok := a.st.Users[req.Login]
	a.mu.Unlock()
	if !ok || len(req.Args) == 0 {
		return nil, &agent.Error{Status: http.StatusBadRequest, Message: "unknown user " + req.Login}
	}
	if !a.real(u.Home) {
		return nil, &agent.Error{Status: http.StatusBadRequest, Message: "no home directory for " + req.Login}
	}
	timeout := 2 * time.Minute
	if req.TimeoutSeconds > 0 && time.Duration(req.TimeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, a.self, append([]string{"fsop"}, req.Args...)...)
	cmd.Env = []string{"HOME=" + u.Home, "PATH=/usr/bin:/bin", "MP_LANG=en"}
	cmd.Dir = u.Home
	cmd.WaitDelay = 5 * time.Second
	if req.StdinBase64 != "" {
		in, err := base64.StdEncoding.DecodeString(req.StdinBase64)
		if err != nil {
			return nil, &agent.Error{Status: http.StatusBadRequest, Message: "stdin_base64: " + err.Error()}
		}
		cmd.Stdin = bytes.NewReader(in)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	resp := &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString(stdout.Bytes()), Stderr: strings.TrimSpace(stderr.String())}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		resp.ExitCode = ee.ExitCode()
	case err != nil:
		return nil, &agent.Error{Message: "fsop: " + err.Error()}
	}
	return resp, nil
}

// unit returns the pretend unit, creating it; the caller holds mu.
func (a *Agent) unit(name string) *unitState {
	name = unitName(name)
	u, ok := a.st.Units[name]
	if !ok {
		u = &unitState{}
		a.st.Units[name] = u
	}
	return u
}

func unitName(name string) string {
	if name != "" && !strings.Contains(name, ".") {
		return name + ".service"
	}
	return name
}

func (u *unitState) start() {
	if !u.Active {
		u.Active, u.Since = true, time.Now()
	}
}

func (a *Agent) service(_ context.Context, req *agent.ServiceRequest) (*agent.ServiceResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Action == "daemon-reload" || req.Unit == "" {
		return &agent.ServiceResponse{}, nil
	}
	name := unitName(req.Unit)
	u, known := a.st.Units[name]
	switch req.Action {
	case "status":
	case "start", "restart", "reload", "reload-or-restart", "try-restart", "try-reload-or-restart":
		u = a.unit(name)
		if req.Action == "restart" || req.Action == "try-restart" {
			u.Active, u.Since = true, time.Now()
		}
		u.start()
		known = true
	case "stop":
		u = a.unit(name)
		u.Active = false
		known = true
	case "enable", "disable":
		u = a.unit(name)
		u.Enabled = req.Action == "enable"
		known = true
	default:
		return nil, &agent.Error{Status: http.StatusBadRequest, Message: "unknown action " + req.Action}
	}
	st := systemd.Status{Unit: name, LoadState: "not-found", ActiveState: "inactive", SubState: "dead", UnitFileState: ""}
	if known {
		st.LoadState, st.UnitFileState = "loaded", "disabled"
		if u.Enabled {
			st.UnitFileState = "enabled"
		}
		if u.Active {
			st.ActiveState, st.SubState, st.ActiveSince = "active", "running", u.Since
		}
	}
	return &agent.ServiceResponse{Status: st}, nil
}

func (a *Agent) installPanel(context.Context, *agent.InstallPanelRequest) (*agent.InstallPanelResponse, error) {
	return nil, &agent.Error{Status: http.StatusForbidden, Message: ErrNotInDemo}
}

func (a *Agent) chown(context.Context, *agent.ChownRequest) (*agent.ChownResponse, error) {
	return &agent.ChownResponse{Changed: 1}, nil
}
