package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// consoleRaw posts to the console and returns the streamed text as is; lang
// is the interface language the page sends along.
func consoleRaw(f *siteFixture, args []string, lang ...string) (int, string, string) {
	f.t.Helper()
	payload := map[string]any{"args": args}
	if len(lang) > 0 {
		payload["lang"] = lang[0]
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.ts.URL+"/api/v1/system/console", bytes.NewReader(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), string(body)
}

// The console runs mp as a client of the API with a token that lives for
// the command only; the output streams back with the exit status.
func TestConsoleRunsMPWithAOneOffToken(t *testing.T) {
	f := newSiteFixture(t)
	script := filepath.Join(t.TempDir(), "mp")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\necho \"home: $HOME\"\necho \"lang: $MP_LANG\"\necho oops 1>&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := consoleBinary
	consoleBinary = func() string { return script }
	t.Cleanup(func() { consoleBinary = prev })

	status, ctype, body := consoleRaw(f, []string{"site", "list", "--json"})
	if status != http.StatusOK || !strings.HasPrefix(ctype, "text/plain") {
		t.Fatalf("console: %d %s", status, ctype)
	}
	if !strings.Contains(body, "$ mp site list --json\n") || !strings.Contains(body, "args: site list --json --server https://127.0.0.1:") || !strings.Contains(body, "--insecure") || !strings.Contains(body, "oops") || !strings.Contains(body, "[exit 3]") {
		t.Fatalf("output:\n%s", body)
	}
	if strings.Contains(body, "--token \n") {
		t.Fatalf("a token must be passed: %s", body)
	}
	// mp speaks the interface's language, English unless the page says ru.
	if !strings.Contains(body, "lang: en\n") {
		t.Fatalf("default language: %s", body)
	}
	if _, _, body := consoleRaw(f, []string{"status"}, "ru"); !strings.Contains(body, "lang: ru\n") {
		t.Fatalf("Russian interface: %s", body)
	}
	admin, err := f.db.GetUserByLogin(f.ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := f.db.ListAPITokens(f.ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tokens {
		if strings.HasPrefix(tk.Name, "console ") {
			t.Fatalf("the one-off token must be gone: %+v", tk)
		}
	}

	// The daemons and the installer are not for a browser; the client flags are the console's own.
	for _, args := range [][]string{{"agent"}, {"helper", "--uid", "1"}, {"doctor", "--server", "https://x"}, {"doctor", "--token=abc"}} {
		if st, _, _ := consoleRaw(f, args); st != http.StatusUnprocessableEntity {
			t.Fatalf("%v must be refused, got %d", args, st)
		}
	}
}
