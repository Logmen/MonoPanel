package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A restore by scope finds its paths and dumps in the snapshot: an account
// is its home and its databases, a site its files, a database its dump. In
// place, a database's dump is loaded back as the database's own account —
// auth_socket while it loads, its own password hash after — through the
// DEFINER filter, and the restored dump goes; aside it is a file. What the
// snapshot does not hold, or a scope with paths besides, is refused.
func TestRestoreByScope(t *testing.T) {
	f := newSiteFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	d := &store.Database{UserID: alex.ID, Name: "alex_shop"}
	if err := f.db.CreateDatabase(f.ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateDBUser(f.ctx, &store.DBUser{UserID: alex.ID, DatabaseID: &d.ID, Name: "alex_shop", Host: "localhost", AuthPlugin: "caching_sha2_password"}); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)

	dump := filepath.Join(f.s.cfg.DataDir, "backups", "tmp", "run-5", "alex_shop.sql")
	snap, _ := json.Marshal([]map[string]any{{"id": "abcdef0123456789", "short_id": "abcdef01", "time": "2026-11-27T03:00:00Z", "paths": []string{"/var/www", "/etc/monopanel", dump}, "tags": []string{"monopanel", "scope=server"}}})
	var mu sync.Mutex
	var sqls, restores []string
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Name == "restic" && len(req.Args) > 0 && req.Args[0] == "snapshots":
			return &agent.ToolResponse{Output: string(snap) + "\n"}
		case req.Name == "restic" && len(req.Args) > 0 && req.Args[0] == "restore":
			restores = append(restores, strings.Join(req.Args, " "))
			// In place, the dump comes back to where the run wrote it.
			if slices.Contains(req.Args, "/") && slices.Contains(req.Args, dump) {
				os.MkdirAll(filepath.Dir(dump), 0o750)                                                                    //nolint:errcheck // the test's own directory
				os.WriteFile(dump, []byte("CREATE VIEW v AS SELECT 1;\n/*!50013 DEFINER=`root`@`localhost` */\n"), 0o640) //nolint:errcheck // the test's own directory
			}
			return &agent.ToolResponse{}
		case req.Name == "mysql":
			sqls = append(sqls, req.Stdin)
			if strings.Contains(req.Stdin, "HEX(authentication_string)") {
				return &agent.ToolResponse{Output: "caching_sha2_password\t2441243030350A\n"}
			}
		}
		return nil
	}
	restore := func(body map[string]any, want int) string {
		t.Helper()
		body["target"], body["snapshot"] = "local", "abcdef01"
		status, raw := f.do(http.MethodPost, "/backups/restore", body)
		if status != want {
			t.Fatalf("%v: %d %s", body, status, raw)
		}
		var ref struct {
			JobID int64 `json:"job_id"`
		}
		json.Unmarshal(raw, &ref) //nolint:errcheck // checked by the caller
		if want == http.StatusAccepted {
			if j := f.waitJob(ref.JobID); j.Status != store.JobDone {
				t.Fatalf("%v: %s %s", body, j.Status, j.Error)
			}
		}
		return string(raw)
	}

	// A database in place: its dump back, loaded as its account, gone after.
	restore(map[string]any{"scope": "db:alex_shop", "in_place": true}, http.StatusAccepted)
	mu.Lock()
	if len(restores) != 1 || !strings.Contains(restores[0], "--target / --include "+dump) {
		t.Errorf("restic restore %q", restores)
	}
	var socket, back bool
	for _, q := range sqls {
		socket = socket || strings.Contains(q, "ALTER USER 'alex_shop'@'localhost' IDENTIFIED WITH auth_socket AS 'root'")
		back = back || strings.Contains(q, "ALTER USER 'alex_shop'@'localhost' IDENTIFIED WITH caching_sha2_password AS 0x2441243030350A")
	}
	mu.Unlock()
	if !socket || !back {
		t.Errorf("the account was not lent and given back: %q", sqls)
	}
	var loaded *agenttestStream
	for _, st := range f.agent.Streams() {
		if st.Direction == "in" && st.Name == "mysql" {
			s := agenttestStream{args: strings.Join(st.Args, " "), head: string(st.Head)}
			loaded = &s
		}
	}
	if loaded == nil || loaded.args != "--protocol=socket --binary-mode --local-infile=0 --user=alex_shop alex_shop" || !strings.Contains(loaded.head, "DEFINER=CURRENT_USER") {
		t.Errorf("the dump was not loaded as its account through the filter: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Dir(dump)); !os.IsNotExist(err) {
		t.Errorf("the restored dump or its run's directory stays: %v", err)
	}

	// An account aside: its home and its database's dump, nothing loaded.
	mu.Lock()
	restores = nil
	mu.Unlock()
	streams := len(f.agent.Streams())
	restore(map[string]any{"scope": "user:alex"}, http.StatusAccepted)
	mu.Lock()
	if len(restores) != 1 || !strings.Contains(restores[0], "--include /var/www/alex") || !strings.Contains(restores[0], "--include "+dump) || !strings.Contains(restores[0], "--target "+filepath.Join(agent.RestoreDir, "abcdef01")+" ") {
		t.Errorf("account aside: %q", restores)
	}
	mu.Unlock()
	if len(f.agent.Streams()) != streams {
		t.Error("a restore aside loaded a database")
	}

	// A site in place: its files alone.
	mu.Lock()
	restores = nil
	mu.Unlock()
	restore(map[string]any{"scope": "site:shop.example.com", "in_place": true}, http.StatusAccepted)
	mu.Lock()
	if len(restores) != 1 || !strings.HasSuffix(restores[0], "--target / --include /var/www/alex/data/www/shop.example.com") {
		t.Errorf("site in place: %q", restores)
	}
	mu.Unlock()

	// Refused: a database the panel has not, one the snapshot holds no dump
	// of, a scope with paths, the whole snapshot in place.
	restore(map[string]any{"scope": "db:nothing", "in_place": true}, http.StatusUnprocessableEntity)
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_blog"}); err != nil {
		t.Fatal(err)
	}
	if out := restore(map[string]any{"scope": "db:alex_blog"}, http.StatusUnprocessableEntity); !strings.Contains(out, "holds no dump of the database alex_blog") {
		t.Errorf("no dump: %s", out)
	}
	restore(map[string]any{"scope": "user:alex", "include": []string{"/var/www/alex"}}, http.StatusUnprocessableEntity)
	restore(map[string]any{"scope": "server", "in_place": true}, http.StatusUnprocessableEntity)
}

type agenttestStream struct{ args, head string }

// A directory of a snapshot as restic ls --json prints it (struct_type of
// restic 0.16, message_type of 0.17): the directory itself left out, its own
// entries only, directories first; a file asked for is its one entry; output
// the agent cut at its start says so.
func TestParseSnapshotLs(t *testing.T) {
	out := `{"time":"2026-11-27T03:00:00Z","paths":["/var/www"],"struct_type":"snapshot"}
{"name":"alex","type":"dir","path":"/var/www/alex","mtime":"2026-11-01T00:00:00Z","struct_type":"node"}
{"name":"z.txt","type":"file","path":"/var/www/alex/z.txt","size":12,"mtime":"2026-11-02T00:00:00Z","message_type":"node"}
{"name":"data","type":"dir","path":"/var/www/alex/data","message_type":"node"}
{"name":"a.php","type":"file","path":"/var/www/alex/a.php","size":5,"message_type":"node"}
{"name":"deep","type":"file","path":"/var/www/alex/data/deep","size":1,"message_type":"node"}
`
	l := parseSnapshotLs("/var/www/alex", out)
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.Type+":"+e.Name)
	}
	if strings.Join(got, " ") != "dir:data file:a.php file:z.txt" || l.Truncated {
		t.Errorf("entries %v, truncated %v", got, l.Truncated)
	}
	if l.Entries[2].Size != 12 || l.Entries[2].MTime == "" {
		t.Errorf("z.txt: %+v", l.Entries[2])
	}
	f := parseSnapshotLs("/var/www/alex/z.txt", `{"name":"z.txt","type":"file","path":"/var/www/alex/z.txt","size":12,"struct_type":"node"}`)
	if len(f.Entries) != 1 || f.Entries[0].Path != "/var/www/alex/z.txt" {
		t.Errorf("a file: %+v", f.Entries)
	}
	cut := parseSnapshotLs("/var/www", "[...truncated...]\n"+`ame":"x","type":"dir","path":"/var/www/x"}`+"\n"+`{"name":"y","type":"dir","path":"/var/www/y","struct_type":"node"}`)
	if !cut.Truncated || len(cut.Entries) != 1 || cut.Entries[0].Name != "y" {
		t.Errorf("cut: %+v", cut)
	}
}

// The listing asks restic for one directory, and at the top shows the
// snapshot's own paths; a path that is not clean is refused.
func TestSnapshotLsEndpoint(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	snap, _ := json.Marshal([]map[string]any{{"id": "abcdef0123456789", "short_id": "abcdef01", "paths": []string{"/var/www", "/var/lib/monopanel/backups/tmp/run-5/alex_shop.sql"}, "tags": []string{"monopanel"}}})
	var asked []string
	var mu sync.Mutex
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name != "restic" || len(req.Args) == 0 {
			return nil
		}
		mu.Lock()
		asked = append(asked, strings.Join(req.Args, " "))
		mu.Unlock()
		switch req.Args[0] {
		case "snapshots":
			return &agent.ToolResponse{Output: string(snap) + "\n"}
		case "ls":
			return &agent.ToolResponse{Output: `{"name":"alex","type":"dir","path":"/var/www/alex","struct_type":"node"}` + "\n" + `{"name":"index.php","type":"file","path":"/var/www/alex/index.php","size":7,"struct_type":"node"}` + "\n"}
		}
		return nil
	}
	var top apitypes.SnapshotListing
	f.call(http.MethodGet, "/backups/targets/local/snapshots/abcdef01/ls", nil, http.StatusOK, &top)
	if len(top.Entries) != 2 || top.Entries[0].Type != "dir" || top.Entries[1].Type != "file" {
		t.Errorf("top: %+v", top.Entries)
	}
	var dir apitypes.SnapshotListing
	f.call(http.MethodGet, "/backups/targets/local/snapshots/abcdef01/ls?path=/var/www/alex", nil, http.StatusOK, &dir)
	if len(dir.Entries) != 1 || dir.Entries[0].Name != "index.php" {
		t.Errorf("dir: %+v", dir.Entries)
	}
	mu.Lock()
	if !slices.Contains(asked, "ls --json abcdef01 /var/www/alex") {
		t.Errorf("asked %q", asked)
	}
	mu.Unlock()
	f.call(http.MethodGet, "/backups/targets/local/snapshots/abcdef01/ls?path=/var/www/../etc", nil, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodGet, "/backups/targets/local/snapshots/abcdef01/ls?path=var/www", nil, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodGet, "/backups/targets/local/snapshots/abc:def/ls?path=/var/www", nil, http.StatusUnprocessableEntity, nil)
}
