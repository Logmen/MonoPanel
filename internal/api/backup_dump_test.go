package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// A database dump for a backup is the API's own file: mysqldump runs as root
// through the agent's stream, its output lands in a file the API makes in its
// own staging directory, and the API removes that directory when the run is
// over. The agent writes and removes nothing below the API's data directory:
// a root write there could be steered by the directory's owner.
func TestBackupDumpsAreTheAPIsOwnFiles(t *testing.T) {
	f := newSiteFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "alex_wp"}); err != nil {
		t.Fatal(err)
	}
	f.agent.StreamOutput["mysqldump"] = "-- dump of alex_wp\n"
	dumped := ""
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name != "restic" || len(req.Args) == 0 || req.Args[0] != "backup" {
			return nil
		}
		for _, a := range req.Args {
			if strings.HasSuffix(a, "alex_wp.sql") {
				b, _ := os.ReadFile(a)
				dumped = string(b)
			}
		}
		return &agent.ToolResponse{Output: `{"message_type":"summary","snapshot_id":"0123456789abcdef","total_files_processed":1,"total_bytes_processed":19,"data_added":19}` + "\n"}
	}
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup"}, http.StatusCreated, nil)
	var job apitypes.JobRef
	f.call(http.MethodPost, "/backups/run", map[string]any{"target": "local", "scope": "db:alex_wp"}, http.StatusAccepted, &job)
	if j := f.waitJob(job.JobID); j.Status != store.JobDone {
		t.Fatalf("backup: %s %s", j.Status, j.Error)
	}
	if dumped != "-- dump of alex_wp\n" {
		t.Fatalf("restic got the dump %q", dumped)
	}
	streamed := false
	for _, c := range f.agent.Calls() {
		body := string(c.Body)
		switch {
		case c.Path == "/v1/stream/out" && strings.Contains(body, `"mysqldump"`) && strings.Contains(body, "alex_wp"):
			streamed = true
		case c.Path == "/v1/tool" && strings.Contains(body, `"mysqldump"`):
			t.Errorf("mysqldump wrote its output as root: %s", body)
		case strings.Contains(body, f.s.cfg.DataDir) && c.Path != "/v1/tool" && c.Path != "/v1/stream/out":
			t.Errorf("%s below the API's data directory: %s", c.Path, body)
		}
	}
	if !streamed {
		t.Error("the dump did not come through the agent's stream")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.s.cfg.DataDir, "backups", "tmp")); len(entries) != 0 {
		t.Errorf("the staging directory stayed: %v", entries)
	}
}
