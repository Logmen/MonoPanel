package demo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The demo is prepared the way the image build does it and served the way a
// container does: every seed step must still pass through the real API, and
// the front door must act as the demo administrator and refuse what cannot
// work without the internet. A change in the API that the pretend server does
// not follow fails here, not on demo.monopanel.app.
func TestPrepareAndServe(t *testing.T) {
	if testing.Short() {
		t.Skip("prepares a whole demo")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := os.MkdirTemp("", "mpd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(run)
	dir := t.TempDir()
	o := Options{Dir: filepath.Join(dir, "demo"), WWW: filepath.Join(dir, "www"), Run: run, PanelAddr: freeAddr(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := Prepare(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.WWW, "alex", "data", "www", "example.com", "wp-config.php")); err != nil {
		t.Fatalf("the WordPress files: %v", err)
	}

	o.Listen = freeAddr(t)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, o) }()
	base := "http://" + o.Listen
	var res *http.Response
	for i := 0; i < 100; i++ {
		if res, err = http.Get(base + "/ping"); err == nil {
			res.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the demo did not answer: %v", err)
	}
	get := func(path string, v any) int {
		t.Helper()
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if v != nil {
			_ = json.NewDecoder(res.Body).Decode(v)
		}
		return res.StatusCode
	}
	var me struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if get("/api/v1/auth/me", &me); me.Login != AdminLogin || me.Role != "admin" {
		t.Fatalf("the front door must sign in as the demo administrator: %+v", me)
	}
	var sites []struct {
		Domain string `json:"domain"`
		Status string `json:"status"`
		CMS    string `json:"cms"`
	}
	get("/api/v1/sites", &sites)
	found := map[string]string{}
	for _, s := range sites {
		found[s.Domain] = s.Status + "/" + s.CMS
	}
	if found["example.com"] != "active/wordpress" || found["shop.example.com"] != "active/opencart" || found["app.example.org"] != "active/" || len(sites) != 4 {
		t.Fatalf("sites: %v", found)
	}
	var mail struct {
		Warnings []string `json:"warnings"`
	}
	get("/api/v1/mail", &mail)
	for _, w := range mail.Warnings {
		if !strings.Contains(w, "self-signed") {
			t.Fatalf("mail must look healthy but for its certificate: %v", mail.Warnings)
		}
	}
	res, err = http.Post(base+"/api/v1/system/update/apply", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Not available in the demo") {
		t.Fatalf("an update must be refused: %d %s", res.StatusCode, body)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the demo did not stop")
	}
}
