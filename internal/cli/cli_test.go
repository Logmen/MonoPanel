package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/boombuler/barcode/qr"
	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"monopanel/internal/config"
)

// fakeAPI stands in for the panel: it records every request and answers from
// a table keyed by "METHOD /path" (the path under /api/v1, without the query).
type fakeAPI struct {
	mu     sync.Mutex
	calls  []fakeCall
	routes map[string]fakeResp
}

type fakeCall struct {
	Method, Path, Query string
	Body                []byte
}

type fakeResp struct {
	status int
	body   any
}

func newFakeAPI(t *testing.T, routes map[string]fakeResp) *fakeAPI {
	t.Helper()
	f := &fakeAPI{routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	t.Setenv("MP_SERVER", srv.URL)
	t.Setenv("MP_TOKEN", "test-token")
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{r.Method, p, r.URL.RawQuery, body})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"status": 401, "title": "Unauthorized"}) //nolint:errcheck // test server
		return
	}
	resp, ok := f.routes[r.Method+" "+p]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"status": 404, "title": "Not Found", "detail": "no route " + r.Method + " " + p}) //nolint:errcheck // test server
		return
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	w.WriteHeader(resp.status)
	if resp.body != nil {
		json.NewEncoder(w).Encode(resp.body) //nolint:errcheck // test server
	}
}

// call returns the one request made to METHOD /path, failing when there is
// not exactly one.
func (f *fakeAPI) call(t *testing.T, method, path string) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []fakeCall
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s: %d requests, want 1 (all: %v)", method, path, len(found), f.calls)
	}
	return found[0]
}

func (f *fakeAPI) none(t *testing.T, method, path string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			t.Fatalf("unexpected %s %s %s", method, path, c.Body)
		}
	}
}

// body decodes a recorded JSON body.
func (c fakeCall) body(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("%s %s body %q: %v", c.Method, c.Path, c.Body, err)
	}
	return m
}

// mp runs one command line the way Main does, with stdin given and not a
// terminal (as under the TUI), and returns what it printed on stdout.
func mp(t *testing.T, stdinText string, args ...string) (string, error) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr, oldIn, oldTTY := os.Stdout, os.Stderr, stdin, stdinIsTerminal
	os.Stdout, os.Stderr = outW, errW
	stdin, stdinIsTerminal = strings.NewReader(stdinText), func() bool { return false }
	defer func() { os.Stdout, os.Stderr, stdin, stdinIsTerminal = oldOut, oldErr, oldIn, oldTTY }()
	out, errOut := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); out <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errOut <- string(b) }()
	root := newRoot()
	root.SetArgs(args)
	runErr := root.ExecuteContext(context.Background())
	outW.Close()
	errW.Close()
	stdout, stderr := <-out, <-errOut
	if testing.Verbose() && stderr != "" {
		t.Logf("stderr of mp %s:\n%s", strings.Join(args, " "), stderr)
	}
	return stdout, runErr
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return 1
	}
	return 0
}

func wantBody(t *testing.T, c fakeCall, want map[string]any) {
	t.Helper()
	got := c.body(t)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s %s body:\n got %v\nwant %v", c.Method, c.Path, got, want)
	}
}

var account = map[string]any{"user_id": 5, "login": "alex", "role": "user", "via": "token"}

func TestTOTPSelfService(t *testing.T) {
	url := "otpauth://totp/MonoPanel:alex?issuer=MonoPanel&secret=JBSWY3DPEHPK3PXP"
	f := newFakeAPI(t, map[string]fakeResp{
		"GET /auth/me":            {body: account},
		"GET /auth/totp":          {body: map[string]any{"enabled": false}},
		"POST /auth/totp/setup":   {body: map[string]any{"secret": "JBSWY3DPEHPK3PXP", "url": url}},
		"POST /auth/totp/enable":  {body: map[string]any{"enabled": true}},
		"POST /auth/totp/disable": {body: map[string]any{"enabled": false}},
	})
	out, err := mp(t, "", "totp", "status")
	if err != nil || !strings.Contains(out, "2FA of alex: off") {
		t.Fatalf("status: %v %q", err, out)
	}
	out, err = mp(t, "", "totp", "setup")
	if err != nil || !strings.Contains(out, "JBSWY3DPEHPK3PXP") || !strings.Contains(out, url) || !strings.Contains(out, "mp totp enable") {
		t.Fatalf("setup: %v %q", err, out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("setup drew a QR code although stdout is not a terminal")
	}
	if out, err = mp(t, "", "totp", "setup", "--qr"); err != nil || !strings.Contains(out, "\x1b[40;97m") {
		t.Fatalf("setup --qr: %v %q", err, out)
	}
	if _, err = mp(t, "", "totp", "enable", "123456"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/auth/totp/enable"), map[string]any{"code": "123456"})

	// disable never waits for a password nobody types
	if _, err = mp(t, "", "totp", "disable"); exitCode(err) != 2 {
		t.Fatalf("disable without a terminal: %v", err)
	}
	if _, err = mp(t, "", "totp", "disable", "--password-stdin"); exitCode(err) != 2 {
		t.Fatalf("disable with an empty stdin: %v", err)
	}
	f.none(t, "POST", "/auth/totp/disable")
	if _, err = mp(t, "s3cret pass\n", "totp", "disable", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/auth/totp/disable"), map[string]any{"password": "s3cret pass"})
}

func TestTOTPErrors(t *testing.T) {
	newFakeAPI(t, map[string]fakeResp{
		"GET /auth/me":           {body: account},
		"POST /auth/totp/setup":  {status: 409, body: map[string]any{"status": 409, "detail": "2FA is already on"}},
		"POST /auth/totp/enable": {status: 401, body: map[string]any{"status": 401, "detail": "invalid code"}},
	})
	if _, err := mp(t, "", "totp", "setup"); exitCode(err) != 2 || !strings.Contains(err.Error(), "mp totp disable") {
		t.Errorf("setup with 2FA on: %v", err)
	}
	if _, err := mp(t, "", "totp", "enable", "000000"); exitCode(err) != 4 || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("enable with a wrong code: %v", err)
	}
}

// Root over the socket has no account: the commands explain that instead of
// passing on the API's 422, and send nothing.
func TestTOTPRoot(t *testing.T) {
	f := newFakeAPI(t, map[string]fakeResp{
		"GET /auth/me":   {body: map[string]any{"login": "root", "role": "admin", "via": "peercred"}},
		"GET /auth/totp": {body: map[string]any{"enabled": false}},
	})
	for _, args := range [][]string{{"setup"}, {"enable", "123456"}, {"disable", "--password-stdin"}} {
		_, err := mp(t, "pw\n", append([]string{"totp"}, args...)...)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "mp user totp-reset") {
			t.Errorf("totp %v as root: %v", args, err)
		}
	}
	for _, p := range []string{"/auth/totp/setup", "/auth/totp/enable", "/auth/totp/disable"} {
		f.none(t, "POST", p)
	}
	if out, err := mp(t, "", "totp", "status"); err != nil || !strings.Contains(out, "no panel account") {
		t.Errorf("status as root: %v %q", err, out)
	}
}

// The QR code read back from the half blocks is the encoder's matrix inside a
// light quiet zone.
func TestQRHalfBlocks(t *testing.T) {
	text := "otpauth://totp/MonoPanel%20panel.example.com:alex?algorithm=SHA1&digits=6&issuer=MonoPanel&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	out, err := qrHalfBlocks(text)
	if err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(text, qr.M, qr.Auto)
	if err != nil {
		t.Fatal(err)
	}
	n := code.Bounds().Dx()
	size := n + 4
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != (size+1)/2 {
		t.Fatalf("%d lines for %d module rows", len(lines), size)
	}
	light := make([][]bool, len(lines)*2)
	for i, l := range lines {
		colour := "\x1b[40;97m" // white on black
		if 2*i+1 >= size {
			colour = "\x1b[49;97m" // the last half line is outside the code
		}
		if !strings.HasPrefix(l, colour) || !strings.HasSuffix(l, "\x1b[0m") {
			t.Fatalf("line %d is not coloured %q: %q", i, colour, l)
		}
		cells := []rune(strings.TrimSuffix(strings.TrimPrefix(l, colour), "\x1b[0m"))
		if len(cells) != size {
			t.Fatalf("line %d: %d cells, want %d", i, len(cells), size)
		}
		light[2*i], light[2*i+1] = make([]bool, size), make([]bool, size)
		for x, c := range cells {
			light[2*i][x] = c == '█' || c == '▀'
			light[2*i+1][x] = c == '█' || c == '▄'
		}
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			want := true
			if mx, my := x-2, y-2; mx >= 0 && my >= 0 && mx < n && my < n {
				r, _, _, _ := code.At(mx, my).RGBA()
				want = r >= 0x8000
			}
			if light[y][x] != want {
				t.Fatalf("module %d,%d: light=%v, want %v", x, y, light[y][x], want)
			}
		}
	}
	if light[size][0] {
		t.Error("the half line below an odd-sized code must show no module")
	}
}

func TestFilesTouchRmPut(t *testing.T) {
	f := newFakeAPI(t, map[string]fakeResp{
		"POST /files/op":     {body: map[string]any{"ok": true}},
		"PUT /files/content": {body: map[string]any{"ok": true, "value": 5}},
	})
	if _, err := mp(t, "", "files", "touch", "notes.txt", "--user", "alex", "--force"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/files/op"), map[string]any{"user": "alex", "op": "touch", "path": "notes.txt", "force": true})

	f = newFakeAPI(t, f.routes)
	if _, err := mp(t, "", "files", "rm", "a", "b/c", "d", "--user", "alex"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/files/op"), map[string]any{"user": "alex", "op": "rm", "path": "a", "paths": []any{"b/c", "d"}})

	// an empty upload is not accepted by the API: an empty file becomes a
	// forced touch, an empty stdin an error (it must not empty the file)
	f = newFakeAPI(t, f.routes)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mp(t, "", "files", "put", empty, "empty.txt", "--user", "alex"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/files/op"), map[string]any{"user": "alex", "op": "touch", "path": "empty.txt", "force": true})
	f = newFakeAPI(t, f.routes)
	if _, err := mp(t, "", "files", "put", "-", "x.txt", "--user", "alex"); exitCode(err) != 2 {
		t.Fatalf("put - with an empty stdin: %v", err)
	}
	if _, err := mp(t, "", "site", "nginx", "example.com", "--set", "-"); exitCode(err) != 2 {
		t.Fatalf("site nginx --set - with an empty stdin: %v", err)
	}
	f.none(t, "POST", "/files/op")
	f.none(t, "PUT", "/files/content")
	f.none(t, "PUT", "/sites/example.com/nginx")

	f = newFakeAPI(t, f.routes)
	if _, err := mp(t, "hello", "files", "put", "-", "x.txt", "--user", "alex"); err != nil {
		t.Fatal(err)
	}
	if c := f.call(t, "PUT", "/files/content"); string(c.Body) != "hello" || c.Query != "user=alex&path=x.txt" {
		t.Errorf("upload: %q ?%s", c.Body, c.Query)
	}
}

func TestConfigThroughAPI(t *testing.T) {
	pc := map[string]any{"path": "/etc/monopanel/config.yaml", "hostname": "panel.example.com", "listen": ":8443", "log_level": "info", "log_format": "text", "jobs_workers": 2,
		"data_dir": "/var/lib/monopanel", "templates_dir": "/etc/monopanel/templates", "key_pinned": true, "restart_pending": true}
	f := newFakeAPI(t, map[string]fakeResp{
		"GET /system/config":    {body: pc},
		"PUT /system/config":    {body: map[string]any{"config": pc, "restarted": false}},
		"GET /system/templates": {body: map[string]any{"dir": "/etc/monopanel/templates", "templates": []any{map[string]any{"name": "nginx/site.conf.tmpl"}, map[string]any{"name": "php/pool.conf.tmpl", "override": true, "path": "/etc/monopanel/templates/php/pool.conf.tmpl"}}}},
	})
	out, err := mp(t, "", "config", "show")
	if err != nil || !strings.Contains(out, "panel.example.com") || !strings.Contains(out, ":8443") || !strings.Contains(out, "restart") {
		t.Fatalf("show: %v %q", err, out)
	}
	for _, c := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"web.hostname", " Panel.Example.COM "}, map[string]any{"hostname": "panel.example.com"}},
		{[]string{"web.hostname", ""}, map[string]any{"hostname": ""}},
		{[]string{"web.listen", "203.0.113.5:9443"}, map[string]any{"listen": "203.0.113.5:9443"}},
		{[]string{"log.level", "DEBUG"}, map[string]any{"log_level": "debug"}},
		{[]string{"log.format", "json"}, map[string]any{"log_format": "json"}},
		{[]string{"jobs.workers", "4", "--restart"}, map[string]any{"jobs_workers": float64(4), "restart": true}},
	} {
		f = newFakeAPI(t, f.routes)
		if _, err := mp(t, "", append([]string{"config", "set"}, c.args...)...); err != nil {
			t.Fatalf("set %v: %v", c.args, err)
		}
		wantBody(t, f.call(t, "PUT", "/system/config"), c.want)
	}
	f = newFakeAPI(t, f.routes)
	for _, args := range [][]string{{"update.public_key", "x"}, {"jobs.workers", "many"}} {
		if _, err := mp(t, "", append([]string{"config", "set"}, args...)...); exitCode(err) != 2 {
			t.Errorf("set %v: %v, want a usage error", args, err)
		}
	}
	f.none(t, "PUT", "/system/config")
	out, err = mp(t, "", "config", "templates")
	if err != nil || !strings.Contains(out, "nginx/site.conf.tmpl") || !strings.Contains(out, "/etc/monopanel/templates/php/pool.conf.tmpl") || !strings.Contains(out, "1 of 2") {
		t.Fatalf("templates: %v %q", err, out)
	}
}

// With --restart over the local socket, config set waits for the old API
// process to go and the new one to answer; remotely it only says so.
func TestConfigRestartWait(t *testing.T) {
	fastRestartWait(t)
	pc := map[string]any{"path": "/etc/monopanel/config.yaml", "listen": ":9443"}
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Skip("unix socket:", err)
	}
	var mu sync.Mutex
	health, put := 0, 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/system/config":
			put++
			json.NewEncoder(w).Encode(map[string]any{"config": pc, "restarted": true}) //nolint:errcheck // test server
		case "/api/v1/health":
			health++
			if health == 2 || health == 3 { // the old process is gone, the new one not up yet
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck // test server
		}
	})}
	go srv.Serve(ln) //nolint:errcheck // closed by the cleanup
	t.Cleanup(func() { srv.Close() })
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("run_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MP_SERVER", "")
	out, err := mp(t, "", "--config", cfgPath, "config", "set", "web.listen", ":9443", "--restart")
	if err != nil || !strings.Contains(out, "monopanel-api restarted") {
		t.Fatalf("set --restart: %v %q", err, out)
	}
	mu.Lock()
	if put != 1 || health < 4 {
		t.Errorf("PUT %d times, health %d times: did not follow the restart", put, health)
	}
	mu.Unlock()

	newFakeAPI(t, map[string]fakeResp{"PUT /system/config": {body: map[string]any{"config": pc, "restarted": true}}})
	if out, err := mp(t, "", "config", "set", "web.listen", ":9443", "--restart"); err != nil || !strings.Contains(out, "restarting") {
		t.Fatalf("remote set --restart: %v %q", err, out)
	}
}

func TestConfigLocal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  listen: \":9443\"\n  hostname: panel.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MP_SERVER", "")
	out, err := mp(t, "", "--config", path, "config", "show", "--local", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Web struct{ Listen, Hostname string }
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil || cfg.Web.Listen != ":9443" || cfg.Web.Hostname != "panel.example.com" {
		t.Fatalf("show --local --json: %v %q", err, out)
	}
	if out, err = mp(t, "", "--config", path, "config", "show", "--local"); err != nil || !strings.Contains(out, "listen: :9443") {
		t.Fatalf("show --local: %v %q", err, out)
	}
	if os.Geteuid() != 0 {
		if _, err := mp(t, "", "--config", path, "config", "set", "web.listen", ":8443", "--local"); exitCode(err) != 4 {
			t.Errorf("set --local as a user: %v", err)
		}
	}
}

// config set --local checks and normalizes a value by the rules of PUT
// /system/config: what it writes, the API would have written.
func TestPanelValue(t *testing.T) {
	for _, c := range []struct {
		key, val, want string
		ok             bool
	}{
		{"web.hostname", " Panel.Example.COM. ", "panel.example.com", true}, {"web.hostname", "", "", true},
		{"web.hostname", "203.0.113.5", "", false}, {"web.hostname", "1.2.3", "", false},
		{"web.hostname", "bad_name", "", false}, {"web.hostname", "-x.example.com", "", false}, {"web.hostname", strings.Repeat("a", 64) + ".example.com", "", false},
		{"web.listen", ":8443", ":8443", true}, {"web.listen", "203.0.113.5:8443", "203.0.113.5:8443", true},
		{"web.listen", "[2001:db8:0::1]:8443", "[2001:db8::1]:8443", true}, {"web.listen", "Panel.Example.com:443", "panel.example.com:443", true},
		{"web.listen", "8443", "", false}, {"web.listen", ":0", "", false}, {"web.listen", ":70000", "", false}, {"web.listen", ":08443", "", false},
		{"web.listen", "bad host:8443", "", false},
		{"log.level", "WARN", "warn", true}, {"log.level", "warning", "warn", true}, {"log.level", "verbose", "", false},
		{"log.format", "json", "json", true}, {"log.format", "xml", "", false},
		{"jobs.workers", "1", "1", true}, {"jobs.workers", " 08 ", "8", true}, {"jobs.workers", "64", "64", true}, {"jobs.workers", "0", "", false}, {"jobs.workers", "65", "", false},
		{"update.public_key", "x", "", false},
	} {
		got, err := panelValue(c.key, c.val)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("panelValue(%s, %q) = %q, %v; want %q, ok=%v", c.key, c.val, got, err, c.want, c.ok)
		}
		if err != nil && exitCode(err) != 2 {
			t.Errorf("panelValue(%s, %q): exit code %d, want 2", c.key, c.val, exitCode(err))
		}
	}
}

// fastRestartWait shortens the waits for monopanel-api to come back.
func fastRestartWait(t *testing.T) {
	t.Helper()
	poll, settle, timeout := restartPoll, restartSettle, restartTimeout
	restartPoll, restartSettle, restartTimeout = 20*time.Millisecond, 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { restartPoll, restartSettle, restartTimeout = poll, settle, timeout })
}

// healthSocket serves GET /api/v1/health on dir/api.sock, failing while down
// says so.
func healthSocket(t *testing.T, dir string, down func() bool) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Skip("unix socket:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" || down() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck // test server
	})}
	go srv.Serve(ln) //nolint:errcheck // closed by the cleanup
	t.Cleanup(func() { srv.Close() })
}

// As root, config set --local writes the normalized value, leaves an equal one
// alone, restarts the unit from config.yaml and reports a restart only once
// monopanel-api answers again. web.listen goes to the socket of the panel's
// port as well, and a restart takes the socket along when its address moved.
func TestConfigSetLocalAsRoot(t *testing.T) {
	fastRestartWait(t)
	euid := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = euid })
	var restarted, synced []string
	restart, sync := restartAPIUnit, syncAPISocket
	restartAPIUnit = func(_ context.Context, cfg config.Config, moved bool) error {
		restarted = append(restarted, fmt.Sprintf("%s moved=%v", cfg.Update.APIUnit, moved))
		return nil
	}
	// the drop-in: written when the address differs from the one it holds
	socketAt := ":9443"
	syncAPISocket = func(_ context.Context, cfg config.Config) (bool, error) {
		synced = append(synced, cfg.Web.Listen)
		moved := cfg.Web.Listen != socketAt
		socketAt = cfg.Web.Listen
		return moved, nil
	}
	t.Cleanup(func() { restartAPIUnit, syncAPISocket = restart, sync })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("run_dir: "+dir+"\nweb:\n  listen: \":9443\"\nupdate:\n  api_unit: test-api.service\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MP_SERVER", "")
	read := func() (cfg struct {
		Web struct{ Listen, Hostname string }
		Log struct{ Level string }
	}) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if _, err := mp(t, "", "--config", path, "config", "set", "web.hostname", "Panel.Example.com.", "--local"); err != nil {
		t.Fatal(err)
	}
	if got := read().Web.Hostname; got != "panel.example.com" {
		t.Errorf("hostname written as %q", got)
	}
	if _, err := mp(t, "", "--config", path, "config", "set", "web.hostname", "203.0.113.5", "--local"); exitCode(err) != 2 {
		t.Errorf("an IP address as web.hostname: %v", err)
	}
	if out, err := mp(t, "", "--config", path, "config", "set", "log.level", "info", "--local"); err != nil || !strings.Contains(out, "already says so") {
		t.Errorf("an unchanged value: %v %q", err, out)
	}
	if len(restarted) != 0 {
		t.Errorf("restarted without --restart: %v", restarted)
	}

	var healthy atomic.Bool
	healthSocket(t, dir, func() bool { return !healthy.Load() })
	// monopanel-api does not come back (say, the new address is taken): the
	// command says so instead of "restarted"
	out, err := mp(t, "", "--config", path, "config", "set", "web.listen", "[2001:db8:0::1]:8443", "--local", "--restart")
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "journalctl -u monopanel-api") || strings.Contains(out, "restarted") {
		t.Errorf("a restart that does not come back: %v %q", err, out)
	}
	if got := read().Web.Listen; got != "[2001:db8::1]:8443" {
		t.Errorf("listen written as %q", got)
	}
	healthy.Store(true)
	out, err = mp(t, "", "--config", path, "--json", "config", "set", "web.listen", ":9443", "--local", "--restart")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Config    struct{ Listen string }
		Restarted bool
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Config.Listen != ":9443" || !res.Restarted {
		t.Errorf("--json: %v %q", err, out)
	}
	// The value the file has already still goes to the socket — a socket
	// that missed a change gets there — and a restart that moves nothing
	// leaves it alone; so does one of another key.
	if out, err := mp(t, "", "--config", path, "config", "set", "web.listen", ":9443", "--local", "--restart"); err != nil || !strings.Contains(out, "already says so") {
		t.Errorf("an unchanged web.listen: %v %q", err, out)
	}
	if _, err := mp(t, "", "--config", path, "config", "set", "jobs.workers", "3", "--local", "--restart"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"test-api.service moved=true", "test-api.service moved=true", "test-api.service moved=false", "test-api.service moved=false"}; !reflect.DeepEqual(restarted, want) {
		t.Errorf("restarted %v, want %v", restarted, want)
	}
	// Without --restart, the hint says how to restart with the socket.
	out, err = mp(t, "", "--config", path, "config", "set", "web.listen", "127.0.0.1:9555", "--local")
	if err != nil || !strings.Contains(out, "systemctl stop test-api.socket && systemctl restart monopanel-api") {
		t.Errorf("a moved socket without a restart: %v %q", err, out)
	}
	if want := []string{"[2001:db8::1]:8443", ":9443", ":9443", "127.0.0.1:9555"}; !reflect.DeepEqual(synced, want) {
		t.Errorf("synced %v, want %v", synced, want)
	}
	// A socket that cannot follow does not stop the change: the API listens
	// on the address itself then.
	syncAPISocket = func(context.Context, config.Config) (bool, error) { return false, errors.New("does not resolve") }
	if _, err := mp(t, "", "--config", path, "config", "set", "web.listen", ":9556", "--local"); err != nil {
		t.Errorf("a socket that cannot follow: %v", err)
	}
	if got := read().Web.Listen; got != ":9556" {
		t.Errorf("listen written as %q", got)
	}

	// systemd refuses the restart (the unit used up its start limit): say how
	// to get past that
	restartAPIUnit = func(context.Context, config.Config, bool) error {
		return errors.New(`restart test-api.service: job result "failed"`)
	}
	if _, err := mp(t, "", "--config", path, "config", "set", "jobs.workers", "4", "--local", "--restart"); err == nil || !strings.Contains(err.Error(), "systemctl reset-failed test-api.service") {
		t.Errorf("a refused restart: %v", err)
	}
}

// monopanel api-socket-sync, for the package's scripts: as root only, the
// socket of the panel's port gets the address of config.yaml's web.listen,
// and one it cannot take is an error (the script goes on regardless).
func TestAPISocketSyncCommand(t *testing.T) {
	euid, sync := geteuid, syncAPISocket
	t.Cleanup(func() { geteuid, syncAPISocket = euid, sync })
	var synced []string
	syncAPISocket = func(_ context.Context, cfg config.Config) (bool, error) {
		synced = append(synced, cfg.Web.Listen)
		if strings.HasPrefix(cfg.Web.Listen, "nowhere.") {
			return false, errors.New("nowhere.example.com does not resolve")
		}
		return true, nil
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  listen: \"203.0.113.5:9443\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	geteuid = func() int { return 1000 }
	if _, err := mp(t, "", "--config", path, "api-socket-sync"); exitCode(err) != 4 || len(synced) != 0 {
		t.Fatalf("not root: %v, synced %v", err, synced)
	}
	geteuid = func() int { return 0 }
	out, err := mp(t, "", "--config", path, "api-socket-sync")
	if err != nil || !strings.Contains(out, "monopanel-api.socket") || !strings.Contains(out, "203.0.113.5:9443") || !reflect.DeepEqual(synced, []string{"203.0.113.5:9443"}) {
		t.Fatalf("%v %q, synced %v", err, out, synced)
	}
	if err := os.WriteFile(path, []byte("web:\n  listen: nowhere.example.com:8443\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := mp(t, "", "--config", path, "api-socket-sync"); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("a web.listen the socket cannot take: %v", err)
	}
}

// monopanel prune-unit-copies, for the package's postinstall: as root only,
// it names each copy it removed — of the units, of the modprobe rule — says
// that a rule the administrator changed stays, and an error is the
// command's (the script goes on regardless).
func TestPruneUnitCopiesCommand(t *testing.T) {
	euid, prune, pruneRule := geteuid, pruneUnitCopies, pruneModprobeCopy
	t.Cleanup(func() { geteuid, pruneUnitCopies, pruneModprobeCopy = euid, prune, pruneRule })
	calls, ruleCalls := 0, 0
	var fail, ruleFail error
	pruneUnitCopies = func(context.Context) ([]string, error) {
		calls++
		return []string{"/etc/systemd/system/monopanel-agent.service", "/etc/systemd/system/monopanel-api.service"}, fail
	}
	ruleGone, ruleKept := false, false
	pruneModprobeCopy = func() (bool, bool, error) {
		ruleCalls++
		return ruleGone, ruleKept, ruleFail
	}
	geteuid = func() int { return 1000 }
	if _, err := mp(t, "", "prune-unit-copies"); exitCode(err) != 4 || calls != 0 || ruleCalls != 0 {
		t.Fatalf("not root: %v, %d calls", err, calls)
	}
	geteuid = func() int { return 0 }
	out, err := mp(t, "", "prune-unit-copies")
	if err != nil || calls != 1 || ruleCalls != 1 || strings.Count(out, "removed /etc/systemd/system/monopanel-") != 2 || strings.Contains(out, "modprobe") {
		t.Fatalf("%v %q, %d calls", err, out, calls)
	}
	fail = errors.New("daemon-reload: no systemd")
	if out, err := mp(t, "", "prune-unit-copies"); err == nil || !strings.Contains(err.Error(), "no systemd") || !strings.Contains(out, "monopanel-api.service") || ruleCalls != 2 {
		t.Fatalf("a failed reload: %v %q", err, out)
	}
	fail, ruleGone = nil, true
	if out, err := mp(t, "", "prune-unit-copies"); err != nil || !strings.Contains(out, "removed /etc/modprobe.d/monopanel.conf, a copy of the package's rule: /usr/lib/modprobe.d/monopanel.conf applies\n") {
		t.Fatalf("the rule's copy: %v %q", err, out)
	}
	ruleGone, ruleKept = false, true
	if out, err := mp(t, "", "prune-unit-copies"); err != nil || !strings.Contains(out, "warning: /etc/modprobe.d/monopanel.conf replaces the package's rule /usr/lib/modprobe.d/monopanel.conf") {
		t.Fatalf("the administrator's rule: %v %q", err, out)
	}
	ruleKept, ruleFail = false, errors.New("remove /etc/modprobe.d/monopanel.conf: read-only file system")
	if out, err := mp(t, "", "prune-unit-copies"); err == nil || !strings.Contains(err.Error(), "read-only file system") || !strings.Contains(out, "monopanel-api.service") {
		t.Fatalf("a rule that cannot go: %v %q", err, out)
	}
}

// api-socket-sync --probe answers the API unit's drop-in before each start:
// 0 for any user, whatever config.yaml is, and nothing done. A binary that
// does not know it — 0.8.19 knows no api-socket-sync at all — fails, and
// the drop-in stops the socket then.
func TestAPISocketSyncProbe(t *testing.T) {
	euid, sync := geteuid, syncAPISocket
	t.Cleanup(func() { geteuid, syncAPISocket = euid, sync })
	syncAPISocket = func(context.Context, config.Config) (bool, error) {
		t.Fatal("the probe synced the socket")
		return false, nil
	}
	broken := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(broken, []byte("web: [\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int{0, 990} {
		geteuid = func() int { return uid }
		for _, cfg := range []string{broken, filepath.Join(t.TempDir(), "missing.yaml")} {
			if out, err := mp(t, "", "--config", cfg, "api-socket-sync", "--probe"); err != nil || out != "" {
				t.Errorf("uid %d, config %s: %v %q", uid, cfg, err, out)
			}
		}
	}
	if _, err := mp(t, "", "no-such-command", "--probe"); err == nil {
		t.Error("an unknown command succeeded: the probe would take an old binary for a new one")
	}
}

// mp doctor warns when the API listens on the port itself while the socket
// of the panel's port could hold it, enabled or not (the API's unit starts
// it with itself): the port is free while the API restarts. Not when the
// socket holds it, when there is none or the administrator masked it, when
// the API is too old to say, nor for a port no account may bind, which the
// socket leaves alone. That is mp's own judgement, by the API's rule, for
// an API whose doctor does not judge the socket itself; one that does is
// taken as it is.
func TestDoctorWarnsOfTheIdleSocket(t *testing.T) {
	start := unprivilegedPortStart
	unprivilegedPortStart = func() int { return 1024 }
	t.Cleanup(func() { unprivilegedPortStart = start })
	doctor := fakeResp{body: map[string]any{"summary": "2 checks, 0 failed, 0 warnings", "checks": []map[string]any{{"name": "agent", "status": "ok", "detail": "version test"}}}}
	status := func(via string) fakeResp {
		panel := map[string]any{"version": "0.8.20", "agent_ok": true}
		if via != "" {
			panel["listen_via"] = via
		}
		return fakeResp{body: map[string]any{"panel": panel}}
	}
	socket := func(state string) fakeResp {
		load := "loaded"
		switch state {
		case "not-found":
			load, state = state, ""
		case "masked":
			load = state
		}
		return fakeResp{body: map[string]any{"unit": "monopanel-api.socket", "load_state": load, "active_state": "inactive", "sub_state": "dead", "unit_file_state": state}}
	}
	dir := t.TempDir()
	conf := func(listen string) string {
		p := filepath.Join(dir, strings.NewReplacer(":", "_", ".", "_").Replace(listen)+".yaml")
		if err := os.WriteFile(p, []byte("web:\n  listen: \""+listen+"\"\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		name, via, state, listen string
		warn                     bool
	}{
		{"listens itself", "itself", "enabled", ":8443", true},
		{"on the socket", "socket", "enabled", ":8443", false},
		{"socket disabled", "itself", "disabled", ":8443", true},
		{"socket static", "itself", "static", ":8443", true},
		{"socket masked", "itself", "masked", ":8443", false},
		{"no socket", "itself", "not-found", ":8443", false},
		{"an older API", "", "enabled", ":8443", false},
		{"a privileged port", "itself", "enabled", "203.0.113.5:443", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeAPI(t, map[string]fakeResp{
				"GET /system/doctor":                 doctor,
				"GET /system/status":                 status(c.via),
				"GET /services/monopanel-api.socket": socket(c.state),
			})
			out, err := mp(t, "", "--config", conf(c.listen), "doctor")
			if err != nil {
				t.Fatal(err)
			}
			warned := strings.Contains(out, "[WARN] api socket") && strings.Contains(out, "monopanel-api.socket is "+c.state+", but monopanel-api listens on the port itself") &&
				strings.Contains(out, "mp setup brings the socket back, and systemctl mask monopanel-api.socket has the API listen on the port itself on purpose")
			if warned != c.warn {
				t.Fatalf("warned %v, want %v:\n%s", warned, c.warn, out)
			}
			if !strings.HasSuffix(out, "2 checks, 0 failed, 0 warnings\n") {
				t.Errorf("the summary is not last:\n%s", out)
			}
			if c.via != "itself" {
				f.none(t, "GET", "/services/monopanel-api.socket")
			}
		})
	}
	// --json prints the API's checks as they are.
	newFakeAPI(t, map[string]fakeResp{"GET /system/doctor": doctor, "GET /system/status": status("itself"), "GET /services/monopanel-api.socket": socket("enabled")})
	if out, err := mp(t, "", "--config", conf(":8443"), "--json", "doctor"); err != nil || strings.Contains(out, "api socket") {
		t.Fatalf("--json: %v\n%s", err, out)
	}

	// The API's own line, judged on its host, counted in its summary: mp
	// adds nothing and asks nothing more, whatever its own config.yaml says.
	judged := fakeResp{body: map[string]any{"summary": "2 checks, 0 failed, 1 warning", "checks": []map[string]any{
		{"name": "agent", "status": "ok", "detail": "version test"},
		{"name": "api socket", "status": "warn", "detail": "monopanel-api.socket is enabled, but monopanel-api listens on the port itself"},
	}}}
	f := newFakeAPI(t, map[string]fakeResp{"GET /system/doctor": judged, "GET /system/status": status("itself"), "GET /services/monopanel-api.socket": socket("enabled")})
	out, err := mp(t, "", "--config", conf("203.0.113.5:443"), "doctor")
	if err != nil || strings.Count(out, "api socket") != 1 || !strings.HasSuffix(out, "2 checks, 0 failed, 1 warning\n") {
		t.Fatalf("an API that judges the socket: %v\n%s", err, out)
	}
	f.none(t, "GET", "/system/status")
	f.none(t, "GET", "/services/monopanel-api.socket")
}

// mp doctor prints a check per line: a detail with the output of apt-get or
// of a validator in it is folded onto it, and what a stranger could have put
// into it (an escape sequence) does not reach the terminal.
func TestDoctorPrintsALinePerCheck(t *testing.T) {
	newFakeAPI(t, map[string]fakeResp{"GET /system/doctor": {body: map[string]any{"summary": "1 check, 0 failed, 1 warning", "checks": []map[string]any{
		{"name": "stack upgrade", "status": "warn", "detail": "the upgrade failed: E: Failed to fetch\nW: Some index files failed\x1b]52;c;cHduZWQ=\x07 to download"},
		{"name": "api socket", "status": "ok", "detail": "monopanel-api.socket holds the port while monopanel-api restarts"},
	}}}})
	out, err := mp(t, "", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	want := "[WARN] stack upgrade                    the upgrade failed: E: Failed to fetch W: Some index files failed to download\n"
	if !strings.HasPrefix(out, want) || strings.Contains(out, "\x1b") || strings.Count(out, "\n") != 3 {
		t.Fatalf("doctor:\n%q", out)
	}
}

// mp doctor exits 3 once a check failed — the restic sandbox line of a
// restic that does not start in its sandbox, which fails every backup — and
// 0 when the worst is a warning, as of a restic version that did not finish
// in time on a busy host; it prints the line either way.
func TestDoctorExitStatus(t *testing.T) {
	for _, c := range []struct {
		status, detail, line string
		code                 int
	}{
		{"fail", "restic does not start in its sandbox — /usr/bin/restic version there: Fatal: boom (exit 1). The sandbox: Landlock ABI 6",
			"[FAIL] restic sandbox                   restic does not start in its sandbox", 3},
		{"warn", "restic took too long to start in its sandbox — /usr/bin/restic version there: restic version did not finish within 30s. The sandbox: Landlock ABI 6",
			"[WARN] restic sandbox                   restic took too long to start in its sandbox", 0},
	} {
		t.Run(c.status, func(t *testing.T) {
			newFakeAPI(t, map[string]fakeResp{"GET /system/doctor": {body: map[string]any{"summary": "3 checks", "checks": []map[string]any{
				{"name": "agent", "status": "ok", "detail": "version test"},
				{"name": "restic sandbox", "status": c.status, "detail": c.detail},
				{"name": "api socket", "status": "ok", "detail": "monopanel-api.socket holds the port while monopanel-api restarts"},
			}}}})
			out, err := mp(t, "", "doctor")
			if code := exitCode(err); code != c.code || !strings.Contains(out, "\n"+c.line) {
				t.Fatalf("exit %d (%v), want %d:\n%s", code, err, c.code, out)
			}
		})
	}
}

// A start of the API that monopanel-api.socket made and that fails lingers
// for the unit's restart pause before it exits: a connection waiting on the
// socket would start it again at once, twenty times in two seconds, and the
// socket would give up and leave the port to anybody. Started otherwise —
// by hand, or with the sockets of another process in the environment — it
// fails at once.
func TestAPIFailedStartLingersUnderTheSocket(t *testing.T) {
	pause := failedStartPause
	failedStartPause = 400 * time.Millisecond
	t.Cleanup(func() { failedStartPause = pause })
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("web: [\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	run := func() time.Duration {
		t.Helper()
		start := time.Now()
		if _, err := mp(t, "", "--config", path, "api"); err == nil || !strings.Contains(err.Error(), "parse config") {
			t.Fatalf("a broken config.yaml: %v", err)
		}
		return time.Since(start)
	}
	if took := run(); took >= failedStartPause {
		t.Errorf("by hand the failure took %v", took)
	}
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", "1")
	if took := run(); took >= failedStartPause {
		t.Errorf("with another process's sockets the failure took %v", took)
	}
	t.Setenv("LISTEN_PID", fmt.Sprint(os.Getpid()))
	if took := run(); took < failedStartPause {
		t.Errorf("started by the socket the failure took only %v", took)
	}
}

// A start of the API that cannot listen — web.listen's port held by another
// process — returns its error at once instead of running on without its
// port, its job workers and start-up refresh changing the host: systemd sees
// it fail and starts it again, the socket's start after the unit's restart
// pause. The local socket it made is gone again.
func TestAPIFailedListenReturns(t *testing.T) {
	pause := failedStartPause
	t.Cleanup(func() { failedStartPause = pause })
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { busy.Close() })
	dir := t.TempDir()
	// A unix socket's path stays short.
	run, err := os.MkdirTemp("", "mpapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	conf := fmt.Sprintf("data_dir: %q\nrun_dir: %q\nlog_dir: %q\ntemplates_dir: %q\nsecret_key_file: %q\nagent:\n  socket: %q\nweb:\n  listen: %q\nlog:\n  level: error\n  audit_file: %q\n",
		filepath.Join(dir, "data"), run, filepath.Join(dir, "log"), filepath.Join(dir, "templates"), filepath.Join(dir, "secret.key"), filepath.Join(run, "agent.sock"), busy.Addr().String(), filepath.Join(dir, "audit.log"))
	if err := os.WriteFile(path, []byte(conf), 0o640); err != nil {
		t.Fatal(err)
	}
	start := func() time.Duration {
		t.Helper()
		begun := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := mp(t, "", "--config", path, "api")
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("a busy web.listen: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("monopanel api runs on without its port")
		}
		if _, err := os.Stat(filepath.Join(run, "api.sock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("api.sock stays behind: %v", err)
		}
		return time.Since(begun)
	}
	// By hand it does not linger: a pause far longer than a start takes, also
	// one under the race detector (CI), whose store, routes and certificate
	// take seconds there, would show.
	failedStartPause = 20 * time.Second
	if took := start(); took >= failedStartPause {
		t.Errorf("by hand the failure took %v", took)
	}
	// Started by the socket: LISTEN_FDS=0 hands over no descriptor, as this
	// test process has none at 3 to give; Listen unsets both again.
	failedStartPause = 400 * time.Millisecond
	t.Setenv("LISTEN_FDS", "0")
	t.Setenv("LISTEN_PID", fmt.Sprint(os.Getpid()))
	if took := start(); took < failedStartPause {
		t.Errorf("started by the socket the failure took only %v", took)
	}
}

// mainRun runs the binary's Main as the process would and returns its exit
// code and what it wrote to stderr. Main takes the language from the
// environment: English, as the other tests expect it.
func mainRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	t.Setenv("MP_LANG", "en")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	out := make(chan string)
	go func() { b, _ := io.ReadAll(r); out <- string(b) }()
	code := Main(args)
	os.Stderr = old
	w.Close()
	return code, <-out
}

// The word on who may use the panel's socket follows a refused connection
// to it, and nothing else that lacks a permission: not the API that cannot
// read its TLS key (it would send the administrator reading the journal
// after the socket's group), not fsop that cannot read a file in a home
// (its error goes back to the API, and on to the file manager or the log
// tail). All of them exit with 4, permission denied.
func TestPermissionHintOnlyForThePanelSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads and connects past the modes")
	}
	const hint = "monopanel-cli"
	run, err := os.MkdirTemp("", "mpsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	ln, err := net.Listen("unix", filepath.Join(run, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(filepath.Join(run, "api.sock"), 0); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "panel.key")
	for p, mode := range map[string]os.FileMode{key: 0, filepath.Join(dir, "panel.crt"): 0o644} {
		if err := os.WriteFile(p, []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("data_dir: %q\nrun_dir: %q\nlog_dir: %q\ntemplates_dir: %q\nsecret_key_file: %q\nagent:\n  socket: %q\nweb:\n  listen: 127.0.0.1:0\n  tls_cert: %q\n  tls_key: %q\nlog:\n  level: error\n  audit_file: %q\n",
		filepath.Join(dir, "data"), run, filepath.Join(dir, "log"), filepath.Join(dir, "templates"), filepath.Join(dir, "secret.key"), filepath.Join(run, "agent.sock"), filepath.Join(dir, "panel.crt"), key, filepath.Join(dir, "audit.log"))), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MP_SERVER", "")
	if code, stderr := mainRun(t, "--config", conf, "status"); code != 4 || !strings.Contains(stderr, hint) {
		t.Errorf("mp refused by the socket: %d %q", code, stderr)
	}
	if code, stderr := mainRun(t, "--config", conf, "api"); code != 4 || !strings.Contains(stderr, "permission denied") || strings.Contains(stderr, hint) {
		t.Errorf("the API that cannot read its key: %d %q", code, stderr)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "data", "logs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "logs", "example.com.access.log"), []byte("GET /\n"), 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if code, stderr := mainRun(t, "fsop", "tail", "data/logs/example.com.access.log", "400"); code != 4 || !strings.Contains(stderr, "permission denied") || strings.Contains(stderr, hint) {
		t.Errorf("fsop that cannot read a file of the account's: %d %q", code, stderr)
	}
}

// Any administrator lists every account's cron jobs, apps and Valkey, not only
// root over the socket; --user narrows it, and a user sees their own.
func TestAdminListsEveryAccount(t *testing.T) {
	lists := map[string]fakeResp{
		"GET /cron":              {body: []any{}},
		"GET /apps":              {body: []any{}},
		"GET /valkey":            {body: map[string]any{"engine": "valkey", "instances": []any{}}},
		"GET /users/bob/cron":    {body: []any{}},
		"GET /users/bob/apps":    {body: []any{}},
		"GET /users/bob/valkey":  {body: map[string]any{"instances": []any{}}},
		"GET /users/alex/cron":   {body: []any{}},
		"GET /users/alex/apps":   {body: []any{}},
		"GET /users/alex/valkey": {body: map[string]any{"instances": []any{}}},
		"GET /auth/me":           {body: map[string]any{"user_id": 3, "login": "admin", "role": "admin", "via": "token"}},
	}
	f := newFakeAPI(t, lists)
	for _, group := range []string{"cron", "app", "valkey"} {
		if _, err := mp(t, "", group, "list"); err != nil {
			t.Fatalf("%s list: %v", group, err)
		}
		if _, err := mp(t, "", group, "list", "--user", "bob"); err != nil {
			t.Fatalf("%s list --user bob: %v", group, err)
		}
	}
	for _, p := range []string{"/cron", "/apps", "/valkey", "/users/bob/cron", "/users/bob/apps", "/users/bob/valkey"} {
		f.call(t, "GET", p)
	}
	lists["GET /auth/me"] = fakeResp{body: account}
	f = newFakeAPI(t, lists)
	for _, group := range []string{"cron", "app", "valkey"} {
		if _, err := mp(t, "", group, "list"); err != nil {
			t.Fatalf("%s list as a user: %v", group, err)
		}
	}
	for _, p := range []string{"/users/alex/cron", "/users/alex/apps", "/users/alex/valkey"} {
		f.call(t, "GET", p)
	}
	f.none(t, "GET", "/cron")
}

// Every request field the API takes has a flag: the ones added for parity.
func TestParityFlags(t *testing.T) {
	routes := map[string]fakeResp{
		"POST /sites":                         {status: 202, body: map[string]any{"site": map[string]any{"domain": "example.com"}, "job_id": 9}},
		"POST /users":                         {status: 202, body: map[string]any{"user": map[string]any{"login": "bob", "id": 7}, "job_id": 9}},
		"PATCH /users/bob":                    {body: map[string]any{"user": map[string]any{"login": "bob"}}},
		"POST /databases":                     {status: 201, body: map[string]any{"database": map[string]any{"name": "alex_shop"}}},
		"POST /databases/alex_shop/password":  {body: map[string]any{"database": map[string]any{"name": "alex_shop"}}},
		"GET /backups":                        {body: []any{}},
		"GET /jobs":                           {body: []any{}},
		"POST /services/php8.4-fpm.service":   {body: map[string]any{"unit": "php8.4-fpm.service"}},
		"PATCH /users/alex/cron/7":            {body: map[string]any{"id": 7}},
		"POST /migrate/plan":                  {body: map[string]any{"ok": true}},
		"POST /webhooks":                      {status: 201, body: map[string]any{"id": "h1"}},
		"POST /dns-providers":                 {status: 201, body: map[string]any{"name": "cf", "type": "cloudflare"}},
		"POST /backups/targets":               {status: 201, body: map[string]any{"target": map[string]any{"name": "t1"}}},
		"PATCH /mail/mailboxes/a@example.com": {body: map[string]any{}},
		"POST /mail/mailboxes":                {status: 201, body: map[string]any{"mailbox": map[string]any{"address": "a@example.com"}}},
	}
	for _, c := range []struct {
		stdin  string
		args   []string
		method string
		path   string
		query  string
		want   map[string]any
	}{
		{"", []string{"site", "add", "example.com", "--user", "alex", "--max-body", "64m", "--http2=false", "--static-by-nginx=false", "--no-wait"}, "POST", "/sites", "",
			map[string]any{"domain": "example.com", "user": "alex", "mode": "fpm", "ssl": "auto", "client_max_body": "64m", "http2": false, "static_by_nginx": false}},
		{"", []string{"site", "add", "example.com", "--user", "alex", "--no-wait"}, "POST", "/sites", "",
			map[string]any{"domain": "example.com", "user": "alex", "mode": "fpm", "ssl": "auto"}},
		{"", []string{"user", "add", "bob", "--quota", "500", "--no-wait"}, "POST", "/users", "", map[string]any{"login": "bob", "role": "user", "quota_mb": float64(500)}},
		{"pass-word-1\n", []string{"user", "add", "bob", "--password-stdin", "--no-wait"}, "POST", "/users", "", map[string]any{"login": "bob", "role": "user", "password": "pass-word-1"}},
		{"pass-word-2\n", []string{"user", "set", "bob", "--password-stdin"}, "PATCH", "/users/bob", "", map[string]any{"password": "pass-word-2"}},
		{"", []string{"db", "create", "shop", "--user", "alex", "--remote", "--legacy-auth"}, "POST", "/databases", "", map[string]any{"name": "shop", "user": "alex", "remote": true, "legacy_auth": true}},
		{"db-pass-123\n", []string{"db", "create", "shop", "--user", "alex", "--password-stdin"}, "POST", "/databases", "", map[string]any{"name": "shop", "user": "alex", "password": "db-pass-123"}},
		{"db-pass-456\n", []string{"db", "passwd", "alex_shop", "--password-stdin"}, "POST", "/databases/alex_shop/password", "", map[string]any{"password": "db-pass-456"}},
		{"", []string{"backup", "list", "--limit", "200", "--target", "t1"}, "GET", "/backups", "limit=200&target=t1", nil},
		{"", []string{"job", "list", "--status", "cancelled", "--limit", "500"}, "GET", "/jobs", "limit=500&status=cancelled", nil},
		{"", []string{"service", "reload-or-restart", "php8.4-fpm.service"}, "POST", "/services/php8.4-fpm.service", "", map[string]any{"action": "reload-or-restart"}},
		{"", []string{"cron", "set", "7", "--user", "alex", "--disable"}, "PATCH", "/users/alex/cron/7", "", map[string]any{"enabled": false}},
		{"", []string{"cron", "set", "7", "--user", "alex", "--enable", "--comment", ""}, "PATCH", "/users/alex/cron/7", "", map[string]any{"enabled": true, "comment": ""}},
		{"src-token\n", []string{"--json", "migrate", "plan", "--source", "https://old.example.com:8443", "--scope", "user:alex", "--token-stdin"}, "POST", "/migrate/plan", "",
			map[string]any{"panel": "monopanel", "source": "https://old.example.com:8443", "scope": "user:alex", "token": "src-token"}},
		{"hmac-secret\n", []string{"webhook", "add", "--url", "https://hooks.example.com/x", "--secret-stdin", "--events", "job.failed"}, "POST", "/webhooks", "",
			map[string]any{"url": "https://hooks.example.com/x", "secret": "hmac-secret", "events": []any{"job.failed"}}},
		{"CLOUDFLARE_DNS_API_TOKEN=abc=def\n\n", []string{"dns-provider", "add", "cf", "--cred-stdin", "--cred", "EXTRA=1"}, "POST", "/dns-providers", "",
			map[string]any{"name": "cf", "type": "cloudflare", "credentials": map[string]any{"CLOUDFLARE_DNS_API_TOKEN": "abc=def", "EXTRA": "1"}}},
		{"AWS_ACCESS_KEY_ID=k\nAWS_SECRET_ACCESS_KEY=s\n", []string{"--json", "backup", "target", "add", "t1", "--type", "s3", "--repo", "s3:https://s3.example.com/b", "--env-stdin"}, "POST", "/backups/targets", "",
			map[string]any{"name": "t1", "type": "s3", "repository": "s3:https://s3.example.com/b", "keep_daily": float64(7), "keep_weekly": float64(4), "keep_monthly": float64(3), "env": map[string]any{"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"}}},
		{"mail-pass-1\n", []string{"mail", "box", "set", "a@example.com", "--password-stdin"}, "PATCH", "/mail/mailboxes/a@example.com", "", map[string]any{"password": "mail-pass-1"}},
		{"mail-pass-2\n", []string{"mail", "box", "add", "a@example.com", "--password-stdin"}, "POST", "/mail/mailboxes", "", map[string]any{"address": "a@example.com", "password": "mail-pass-2"}},
		{"", []string{"mail", "box", "add", "a@example.com", "--quota", "0"}, "POST", "/mail/mailboxes", "", map[string]any{"address": "a@example.com", "quota_mb": float64(0)}},
		{"", []string{"mail", "box", "add", "a@example.com", "--quota", "2048"}, "POST", "/mail/mailboxes", "", map[string]any{"address": "a@example.com", "quota_mb": float64(2048)}},
		{"", []string{"mail", "box", "set", "a@example.com", "--quota", "0"}, "PATCH", "/mail/mailboxes/a@example.com", "", map[string]any{"quota_mb": float64(0)}},
	} {
		f := newFakeAPI(t, routes)
		if _, err := mp(t, c.stdin, c.args...); err != nil {
			t.Errorf("mp %s: %v", strings.Join(c.args, " "), err)
			continue
		}
		call := f.call(t, c.method, c.path)
		if call.Query != c.query {
			t.Errorf("mp %s: query %q, want %q", strings.Join(c.args, " "), call.Query, c.query)
		}
		if c.want != nil {
			wantBody(t, call, c.want)
		}
	}
}

// A *-stdin flag with nothing on stdin, or together with its plain twin, is
// an error before anything is sent.
func TestStdinSecretsFailClearly(t *testing.T) {
	f := newFakeAPI(t, map[string]fakeResp{"POST /users": {status: 202, body: map[string]any{"user": map[string]any{"login": "bob"}}}})
	for _, c := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"user", "add", "bob", "--password-stdin"}},
		{"\n", []string{"user", "add", "bob", "--password-stdin"}},
		{"pw\n", []string{"user", "add", "bob", "--password", "x", "--password-stdin"}},
		{"", []string{"update", "settings", "--token-stdin"}},
		{"", []string{"dns-provider", "add", "cf", "--cred-stdin"}},
		{"NOEQUALS\n", []string{"dns-provider", "add", "cf", "--cred-stdin"}},
		{"a\n", []string{"migrate", "plan", "--from", "fastpanel", "--source", "root@old", "--scope", "user:x", "--password-stdin", "--token-stdin"}},
		{"", []string{"cms", "install", "example.com", "wordpress", "--password-stdin"}},
	} {
		if _, err := mp(t, c.stdin, c.args...); exitCode(err) != 2 {
			t.Errorf("mp %s with stdin %q: %v, want exit code 2", strings.Join(c.args, " "), c.stdin, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("requests sent: %v", f.calls)
	}
}

// Questions are answered from stdin; a closed stdin fails at once with a hint
// at --yes instead of waiting.
func TestConfirmations(t *testing.T) {
	routes := map[string]fakeResp{
		"DELETE /users/bob":   {status: 202, body: map[string]any{"job_id": 4}},
		"GET /system/selinux": {body: map[string]any{"supported": true, "mode": "enforcing", "warning": "permissive logs denials only"}},
		"PUT /system/selinux": {body: map[string]any{"supported": true, "mode": "permissive"}},
	}
	f := newFakeAPI(t, routes)
	if _, err := mp(t, "", "user", "rm", "bob"); exitCode(err) != 2 || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("user rm without an answer: %v", err)
	}
	if _, err := mp(t, "n\n", "user", "rm", "bob"); exitCode(err) != 1 {
		t.Errorf("user rm answered no: %v", err)
	}
	if _, err := mp(t, "", "selinux", "permissive"); exitCode(err) != 2 {
		t.Errorf("selinux permissive without an answer: %v", err)
	}
	f.none(t, "DELETE", "/users/bob")
	f.none(t, "PUT", "/system/selinux")
	if _, err := mp(t, "y\n", "user", "rm", "bob", "--no-wait"); err != nil {
		t.Fatal(err)
	}
	f.call(t, "DELETE", "/users/bob")
	if _, err := mp(t, "", "selinux", "permissive", "--yes"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "PUT", "/system/selinux"), map[string]any{"mode": "permissive"})
}

// The command tree is machine-readable for the TUI: positional arguments in
// Use are <required>, [optional] or variadic with ..., every command and flag
// has a help text, and no local flag hides a global one except the source
// flags of a migration and the repository token of the updates.
func TestCommandTree(t *testing.T) {
	root := newRoot()
	argRe := regexp.MustCompile(`^(<[^<>\s]+>|\[[^\[\]\s]+\])(\.\.\.)?$`)
	globals := map[string]bool{}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { globals[f.Name] = true })
	shadows := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		path := c.CommandPath()
		if c.Short == "" {
			t.Errorf("%s: no Short", path)
		}
		for _, tok := range strings.Fields(c.Use)[1:] {
			if !argRe.MatchString(tok) {
				t.Errorf("%s: Use %q: argument %q is not <x>, [x] or <x>...", path, c.Use, tok)
			}
		}
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if f.Usage == "" {
				t.Errorf("%s --%s: no usage", path, f.Name)
			}
			if globals[f.Name] {
				shadows[strings.TrimPrefix(path, "mp ")+" --"+f.Name] = true
			}
		})
		if !c.HasSubCommands() && c.RunE == nil && c.Run == nil {
			t.Errorf("%s: a leaf without RunE", path)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	want := map[string]bool{"migrate plan --token": true, "migrate plan --insecure": true, "migrate run --token": true, "migrate run --insecure": true, "update settings --token": true}
	if !reflect.DeepEqual(shadows, want) {
		t.Errorf("local flags hiding global ones: %v, want %v", shadows, want)
	}
	for _, path := range [][]string{{"totp", "status"}, {"totp", "setup"}, {"totp", "enable"}, {"totp", "disable"}, {"files", "touch"}, {"config", "show"}, {"config", "set"}, {"config", "templates"}, {"service", "reload-or-restart"}, {"whoami"}} {
		if c, _, err := root.Find(path); err != nil || c.Name() != path[len(path)-1] {
			t.Errorf("mp %s: not found (%v)", strings.Join(path, " "), err)
		}
	}
}

// The positional arguments in Use are what the command accepts: the TUI builds
// its forms from Use and checks the required ones before running anything.
func TestUseMatchesArgs(t *testing.T) {
	try := func(c *cobra.Command, n int) bool {
		args := make([]string, n)
		for i := range args {
			args[i] = "x"
		}
		return c.ValidateArgs(args) == nil
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if !c.Runnable() || !c.HasParent() {
			return
		}
		lo, hi := 0, 0
		for _, tok := range strings.Fields(c.Use)[1:] {
			switch {
			case strings.Contains(tok, "..."):
				hi = -1
			case hi >= 0:
				hi++
			}
			if strings.HasPrefix(tok, "<") {
				lo++
			}
		}
		path := c.CommandPath()
		if c.Args == nil {
			if hi != 0 {
				t.Errorf("%s: Use %q names arguments, but nothing checks them (Args is nil)", path, c.Use)
			}
			return
		}
		if lo > 0 && try(c, lo-1) {
			t.Errorf("%s: Use %q needs %d arguments, but %d pass", path, c.Use, lo, lo-1)
		}
		top := hi
		if hi < 0 {
			top = lo + 3
		}
		for n := lo; n <= top; n++ {
			if !try(c, n) {
				t.Errorf("%s: Use %q allows %d arguments, but they are refused", path, c.Use, n)
			}
		}
		if hi >= 0 && try(c, hi+1) {
			t.Errorf("%s: Use %q allows at most %d arguments, but %d pass", path, c.Use, hi, hi+1)
		}
	}
	walk(newRoot())
}

// secretFlag names the flags the person actually gave, and does not touch
// stdin unless asked to.
func TestSecretFlag(t *testing.T) {
	newCmd := func() (*cobra.Command, *string) {
		var pw string
		c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().StringVar(&pw, "admin-password", "", "")
		c.Flags().Bool("password-stdin", false, "")
		c.SetContext(context.Background())
		return c, &pw
	}
	old, oldTTY := stdin, stdinIsTerminal
	t.Cleanup(func() { stdin, stdinIsTerminal = old, oldTTY })
	stdinIsTerminal = func() bool { return false }

	c, pw := newCmd()
	stdin = strings.NewReader("from-stdin\n")
	if err := c.ParseFlags([]string{"--admin-password", "x", "--password-stdin"}); err != nil {
		t.Fatal(err)
	}
	err := secretFlag(c, "password-stdin", "admin-password", "", pw)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--admin-password") {
		t.Errorf("both flags: %v", err)
	}

	c, pw = newCmd()
	stdin = strings.NewReader("from-stdin\nsecond line\n")
	if err := c.ParseFlags([]string{"--password-stdin"}); err != nil {
		t.Fatal(err)
	}
	if err := secretFlag(c, "password-stdin", "admin-password", "", pw); err != nil || *pw != "from-stdin" {
		t.Errorf("stdin: %v %q", err, *pw)
	}

	c, pw = newCmd()
	stdin = iotestReader{t}
	if err := c.ParseFlags([]string{"--admin-password", "x"}); err != nil {
		t.Fatal(err)
	}
	if err := secretFlag(c, "password-stdin", "admin-password", "", pw); err != nil || *pw != "x" {
		t.Errorf("plain flag: %v %q", err, *pw)
	}
}

// iotestReader fails the test when anything reads it.
type iotestReader struct{ t *testing.T }

func (r iotestReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// Ctrl-C (a cancelled context) ends a wait for an answer or a secret at once,
// even when stdin stays open and silent.
func TestPromptsCancel(t *testing.T) {
	old, oldTTY, oldNoEcho := stdin, stdinIsTerminal, readNoEcho
	t.Cleanup(func() { stdin, stdinIsTerminal, readNoEcho = old, oldTTY, oldNoEcho })
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	stdin, stdinIsTerminal = pr, func() bool { return false }

	for name, wait := range map[string]func(context.Context) error{
		"confirm": func(ctx context.Context) error { return confirm(ctx, "?") },
		"secret":  func(ctx context.Context) error { _, err := readSecret(ctx, "password-stdin", ""); return err },
		"pairs":   func(ctx context.Context) error { return readKeyValues(ctx, "cred-stdin", map[string]string{}) },
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- wait(ctx) }()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: %v, want context.Canceled", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: still waiting after Ctrl-C", name)
		}
	}

	// a terminal: the no-echo prompt is given the same context
	stdinIsTerminal = func() bool { return true }
	readNoEcho = func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readSecret(ctx, "password-stdin", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("terminal prompt: %v", err)
	}
	readNoEcho = func(context.Context) (string, error) { return "", nil }
	if _, err := readSecret(context.Background(), "password-stdin", ""); exitCode(err) != 2 {
		t.Errorf("nothing typed at the prompt: %v", err)
	}
}

// mp whoami says who the panel takes the caller for: the account and its
// role, the token with its scopes, the account's 2FA and the panel asked.
func TestWhoami(t *testing.T) {
	me := map[string]any{"user_id": 5, "login": "alex", "role": "user", "via": "token", "scopes": []any{"billing", "monitoring"}, "token_id": 12, "token_name": "billing-sync"}
	newFakeAPI(t, map[string]fakeResp{
		"GET /auth/me":   {body: me},
		"GET /auth/totp": {body: map[string]any{"enabled": true}},
	})
	server := os.Getenv("MP_SERVER")
	out, err := mp(t, "", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Account:  alex (user)\n", "Auth:     API token #12 \"billing-sync\"\n", "Scopes:   billing, monitoring\n", "2FA:      on\n", "Panel:    " + server + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami: no %q in\n%s", want, out)
		}
	}
	out, err = mp(t, "", "--json", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("whoami --json: %v %q", err, out)
	}
	if got["login"] != "alex" || got["role"] != "user" || got["via"] != "token" || got["token_id"] != float64(12) || got["token_name"] != "billing-sync" ||
		got["totp_enabled"] != true || got["server"] != server || got["socket"] != nil || !reflect.DeepEqual(got["scopes"], []any{"billing", "monitoring"}) {
		t.Errorf("whoami --json: %v", got)
	}

	// Root over the socket has no account, and so no 2FA to ask about.
	f := newFakeAPI(t, map[string]fakeResp{"GET /auth/me": {body: map[string]any{"login": "root", "role": "admin", "via": "peercred"}}})
	out, err = mp(t, "", "whoami")
	if err != nil || !strings.Contains(out, "root (admin) — root on the server, without a panel account") || !strings.Contains(out, "the panel socket, as root") || !strings.Contains(out, "mp user totp-reset") {
		t.Errorf("whoami as root: %v\n%s", err, out)
	}
	if out, err = mp(t, "", "--json", "whoami"); err != nil || strings.Contains(out, "totp_enabled") {
		t.Errorf("whoami --json as root: %v %s", err, out)
	}
	f.none(t, "GET", "/auth/totp")

	// The identity is the answer: a 2FA status that cannot be read is unknown,
	// not a failure.
	newFakeAPI(t, map[string]fakeResp{"GET /auth/me": {body: map[string]any{"user_id": 7, "login": "bob", "role": "admin", "via": "peercred"}}})
	out, err = mp(t, "", "whoami")
	if err != nil || !strings.Contains(out, "Account:  bob (admin)\n") || !strings.Contains(out, "the panel socket, as the account's unix user") || !strings.Contains(out, "2FA:      unknown: ") {
		t.Errorf("whoami without a 2FA status: %v\n%s", err, out)
	}
}

// The plan names what each conflict and warning is about: two sites already
// served here are two different lines.
func TestMigratePlanNamesTargets(t *testing.T) {
	plan := map[string]any{
		"bundle": map[string]any{"hostname": "old.example.com", "panel": "0.8.19", "family": "debian", "user": map[string]any{"login": "alex"}, "sizes": map[string]any{}},
		"login":  "alex",
		"conflicts": []any{
			map[string]any{"kind": "site", "target": "shop.example.com", "text": "the site is already served by this panel (owner bob)", "fix": "delete it here or exclude it from the migration"},
			map[string]any{"kind": "site", "target": "blog.example.com", "text": "the site is already served by this panel (owner bob)", "fix": "delete it here or exclude it from the migration"},
			map[string]any{"kind": "site", "text": "sites are coming but nginx is not installed here", "fix": "mp stack install nginx"},
		},
		"warnings": []any{map[string]any{"kind": "disk", "target": "/", "text": "less than half of the free space will be left after the migration"}},
	}
	newFakeAPI(t, map[string]fakeResp{"POST /migrate/plan": {body: plan}})
	out, err := mp(t, "src-token\n", "migrate", "plan", "--source", "https://old.example.com:8443", "--scope", "user:alex", "--token-stdin")
	if exitCode(err) != 3 {
		t.Fatalf("a plan with conflicts: %v", err)
	}
	for _, want := range []string{
		"\n✗ shop.example.com: the site is already served by this panel (owner bob) → delete it here or exclude it from the migration\n",
		"\n✗ blog.example.com: the site is already served by this panel (owner bob) → delete it here or exclude it from the migration\n",
		"\n✗ sites are coming but nginx is not installed here → mp stack install nginx\n",
		"\n! /: less than half of the free space will be left after the migration\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan: no %q in\n%s", want, out)
		}
	}
}

// Comma lists are trimmed and empty items dropped, as the web forms do.
func TestCommaListsTrimmed(t *testing.T) {
	routes := map[string]fakeResp{
		"POST /tokens":   {status: 201, body: map[string]any{"token": "made-up-token-value", "record": map[string]any{"id": 3}}},
		"POST /webhooks": {status: 201, body: map[string]any{"id": "h1"}},
	}
	f := newFakeAPI(t, routes)
	if _, err := mp(t, "", "token", "create", "--name", "ci", "--scopes", " billing, monitoring ,,backup, "); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/tokens"), map[string]any{"name": "ci", "scopes": []any{"billing", "monitoring", "backup"}})
	if _, err := mp(t, "", "webhook", "add", "--url", "https://hooks.example.com/x", "--events", "job.failed, site.apply.done"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/webhooks"), map[string]any{"url": "https://hooks.example.com/x", "events": []any{"job.failed", "site.apply.done"}})
	f = newFakeAPI(t, routes)
	if _, err := mp(t, "", "token", "create", "--scopes", " , "); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/tokens"), map[string]any{"name": "cli"})
}

// One stdin carries the repository password and the cloud credentials: the
// password on the first line, KEY=VALUE lines after it, in the order the TUI
// pipes them.
func TestBackupTargetSecretsOnStdin(t *testing.T) {
	routes := map[string]fakeResp{"POST /backups/targets": {status: 201, body: map[string]any{"target": map[string]any{"name": "t1"}}}}
	f := newFakeAPI(t, routes)
	stdin := "repo pass word\nAWS_ACCESS_KEY_ID=k\nAWS_SECRET_ACCESS_KEY=s=x\n\nNOT_READ=1\n"
	if _, err := mp(t, stdin, "--json", "backup", "target", "add", "t1", "--type", "s3", "--repo", "s3:https://s3.example.com/b", "--password-stdin", "--env-stdin", "--env", "AWS_DEFAULT_REGION=eu-central-1"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/backups/targets"), map[string]any{"name": "t1", "type": "s3", "repository": "s3:https://s3.example.com/b", "password": "repo pass word",
		"keep_daily": float64(7), "keep_weekly": float64(4), "keep_monthly": float64(3), "env": map[string]any{"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s=x", "AWS_DEFAULT_REGION": "eu-central-1"}})

	// Without the password line, or without a variable after it, nothing is sent.
	f = newFakeAPI(t, routes)
	for _, in := range []string{"", "\n", "repo-pass\n", "repo-pass\n\nAWS_ACCESS_KEY_ID=k\n"} {
		if _, err := mp(t, in, "backup", "target", "add", "t1", "--repo", "/srv/backup", "--password-stdin", "--env-stdin"); exitCode(err) != 2 {
			t.Errorf("stdin %q: %v, want exit code 2", in, err)
		}
	}
	f.none(t, "POST", "/backups/targets")
}

// One stdin holds one value (its first line) and one list after it, never two
// of either.
func TestShareStdin(t *testing.T) {
	c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	for _, name := range []string{"password-stdin", "token-stdin", "env-stdin", "cred-stdin"} {
		c.Flags().Bool(name, false, "")
	}
	if err := c.ParseFlags([]string{"--password-stdin", "--env-stdin"}); err != nil {
		t.Fatal(err)
	}
	if err := shareStdin(c, []string{"password-stdin", "token-stdin"}, []string{"env-stdin", "cred-stdin"}); err != nil {
		t.Errorf("a value and a list: %v", err)
	}
	if err := c.ParseFlags([]string{"--token-stdin"}); err != nil {
		t.Fatal(err)
	}
	if err := shareStdin(c, []string{"password-stdin", "token-stdin"}, nil); exitCode(err) != 2 || !strings.Contains(err.Error(), "--password-stdin and --token-stdin") {
		t.Errorf("two values: %v", err)
	}
	if err := c.ParseFlags([]string{"--cred-stdin"}); err != nil {
		t.Fatal(err)
	}
	if err := shareStdin(c, nil, []string{"env-stdin", "cred-stdin"}); exitCode(err) != 2 {
		t.Errorf("two lists: %v", err)
	}
}

// config show and config set say where the panel still runs while a new name
// or address waits for the restart, naming only what differs.
func TestConfigShowsRunningAddress(t *testing.T) {
	pc := map[string]any{"path": "/etc/monopanel/config.yaml", "hostname": "new.example.com", "listen": ":9443", "log_level": "info", "log_format": "text", "jobs_workers": 2,
		"restart_pending": true, "running_hostname": "panel.example.com", "running_listen": ":8443"}
	newFakeAPI(t, map[string]fakeResp{"GET /system/config": {body: pc}, "PUT /system/config": {body: map[string]any{"config": pc}}})
	const both = "pending restart: monopanel-api still runs with web.hostname panel.example.com, web.listen :8443\n"
	for _, args := range [][]string{{"config", "show"}, {"config", "set", "web.listen", ":9443"}} {
		if out, err := mp(t, "", args...); err != nil || !strings.Contains(out, both) {
			t.Errorf("mp %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	for _, c := range []struct {
		hostname, runningHostname, runningListen, want string
	}{
		{"new.example.com", "new.example.com", ":8443", "still runs with web.listen :8443\n"},
		{"", "panel.example.com", ":9443", "still runs with web.hostname panel.example.com\n"},
		{"new.example.com", "", ":9443", "still runs with web.hostname (not set)\n"},
		{"new.example.com", "new.example.com", ":9443", ""}, // the running process has what the file says
		{"new.example.com", "", "", ""},                     // an API from before running_listen
	} {
		pc["hostname"], pc["running_hostname"], pc["running_listen"] = c.hostname, c.runningHostname, c.runningListen
		out, err := mp(t, "", "config", "show")
		if err != nil || (c.want == "") == strings.Contains(out, "still runs") || !strings.Contains(out, c.want) {
			t.Errorf("hostname %q, running %q %q: %v\n%s", c.hostname, c.runningHostname, c.runningListen, err, out)
		}
	}
}

// Flags that ask for opposite things are refused before anything is sent,
// instead of one of them winning silently (both are toggles in a TUI form).
func TestOppositeFlagsRefused(t *testing.T) {
	f := newFakeAPI(t, map[string]fakeResp{"GET /auth/me": {body: account}})
	for _, args := range [][]string{
		{"user", "set", "bob", "--shell", "--sftp-only"},
		{"user", "set", "bob", "--generate", "--password", "pass-word-3"},
		{"user", "add", "bob", "--generate", "--password", "pass-word-4"},
		{"app", "set", "bot", "--enable", "--disable"},
		{"stack", "real-ip", "--cloudflare", "--no-cloudflare"},
		{"stack", "real-ip", "--clear-from", "--from", "203.0.113.5"},
		{"site", "set", "example.com", "--www", "--no-www"},
		{"site", "set", "example.com", "--allow-all", "--allow", "203.0.113.0/24"},
		{"site", "nginx", "example.com", "--set", "custom.conf", "--clear"},
		{"update", "trust", "--clear", "--off"},
		{"update", "settings", "--clear-token", "--token", "made-up-repo-token"},
		{"update", "settings", "--token-stdin", "--clear-token"},
	} {
		if _, err := mp(t, "made-up-repo-token\n", args...); exitCode(err) != 2 || !strings.Contains(err.Error(), "cannot be used together") {
			t.Errorf("mp %s: %v, want exit code 2", strings.Join(args, " "), err)
		}
	}
	// A certificate named and --all: not every certificate.
	if _, err := mp(t, "", "ssl", "renew", "5", "--all"); exitCode(err) != 2 || !strings.Contains(err.Error(), "not both") {
		t.Errorf("mp ssl renew 5 --all: %v, want exit code 2", err)
	}
	f.mu.Lock()
	if len(f.calls) != 0 {
		t.Errorf("requests sent: %v", f.calls)
	}
	f.mu.Unlock()
	// a switch that is off asks for nothing
	f = newFakeAPI(t, map[string]fakeResp{"PATCH /users/bob": {body: map[string]any{"user": map[string]any{"login": "bob"}}}})
	if _, err := mp(t, "", "user", "set", "bob", "--shell=false", "--sftp-only"); err != nil {
		t.Fatalf("--shell=false --sftp-only: %v", err)
	}
	wantBody(t, f.call(t, "PATCH", "/users/bob"), map[string]any{"shell": false})
	// nor does an emptied list, which asks for the same as the switch (a
	// TUI form sends --allow= for a cleared field)
	f = newFakeAPI(t, map[string]fakeResp{
		"PATCH /sites/example.com": {body: map[string]any{"site": map[string]any{"domain": "example.com"}}},
		"GET /stack/nginx/real-ip": {body: map[string]any{"cloudflare": true, "from": []string{"203.0.113.5"}}},
		"PUT /stack/nginx/real-ip": {body: map[string]any{"cloudflare": true}},
	})
	if _, err := mp(t, "", "--no-wait", "site", "set", "example.com", "--allow-all", "--allow="); err != nil {
		t.Fatalf("--allow-all --allow=: %v", err)
	}
	wantBody(t, f.call(t, "PATCH", "/sites/example.com"), map[string]any{"allow_from": []any{}})
	if _, err := mp(t, "", "stack", "real-ip", "--clear-from", "--from="); err != nil {
		t.Fatalf("--clear-from --from=: %v", err)
	}
	wantBody(t, f.call(t, "PUT", "/stack/nginx/real-ip"), map[string]any{"cloudflare": true})
}

// mp app-exec reads an app's env file as systemd reads EnvironmentFile=.
func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n; another\n\nPLAIN=value with spaces \nSINGLE='a \"b\" $c'\nDOUBLE=\"x \\\"y\\\" \\$z \\\\ w\"\nnoequals\n BAD KEY=1\nEMPTY=\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PLAIN=value with spaces", `SINGLE=a "b" $c`, `DOUBLE=x "y" $z \ w`, "EMPTY="}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if _, err := readEnvFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing env file must fail, as EnvironmentFile= does")
	}
}

// mp service waits for the panel to come back after the actions that take
// it down: a stop or restart of its own units, and a restart of the socket
// of its port, which the API carries out as a restart of itself. A stop of
// the socket leaves the API running.
func TestEndsPanel(t *testing.T) {
	for _, c := range []struct {
		unit, action string
		want         bool
	}{
		{"monopanel-api.service", "restart", true},
		{"monopanel-api", "stop", true},
		{"monopanel-agent.service", "reload-or-restart", true},
		{"monopanel-api.socket", "restart", true},
		{"monopanel-api.socket", "reload-or-restart", true},
		{"monopanel-api.socket", "stop", false},
		{"monopanel-api.socket", "start", false},
		{"monopanel-api.service", "start", false},
		{"nginx.service", "restart", false},
	} {
		if got := endsPanel(c.unit, c.action); got != c.want {
			t.Errorf("endsPanel(%q, %q) = %v, want %v", c.unit, c.action, got, c.want)
		}
	}
}
