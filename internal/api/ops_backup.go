package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

var envKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

type targetsOutput struct {
	Body []*store.BackupTarget
}

type targetInput struct {
	Body apitypes.BackupTargetRequest
}

type targetOutput struct {
	Status int
	Body   apitypes.BackupTargetResponse
}

type targetRefInput struct {
	Ref string `path:"ref"`
}

type backupsListInput struct {
	Target string `query:"target"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500"`
}

type backupsOutput struct {
	Body []*store.Backup
}

type snapshotsOutput struct {
	Body []apitypes.Snapshot
}

type backupRunInput struct {
	Body apitypes.BackupRunRequest
}

type restoreInput struct {
	Body apitypes.RestoreRequest
}

type backupPayload struct {
	TargetID int64    `json:"target_id"`
	Scope    string   `json:"scope,omitempty"`
	Snapshot string   `json:"snapshot,omitempty"`
	Include  []string `json:"include,omitempty"`
	InPlace  bool     `json:"in_place,omitempty"`
}

func (s *Server) resticEnv(t *store.BackupTarget) ([]string, error) {
	if s.secrets == nil {
		return nil, errors.New("secret key unavailable; backups need /etc/monopanel/secret.key readable by the panel")
	}
	pw, err := s.secrets.Decrypt(t.PasswordEnc)
	if err != nil {
		return nil, err
	}
	env := []string{"RESTIC_REPOSITORY=" + t.Repository, "RESTIC_PASSWORD=" + pw, "RESTIC_CACHE_DIR=" + filepath.Join(s.cfg.DataDir, "backups", "cache")}
	if t.EnvEnc != "" {
		raw, err := s.secrets.Decrypt(t.EnvEnc)
		if err != nil {
			return nil, err
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+m[k])
		}
	}
	return env, nil
}

func (s *Server) restic(ctx context.Context, env []string, timeout int, args ...string) (*agent.ToolResponse, error) {
	return s.agent.Tool(ctx, &agent.ToolRequest{Name: "restic", Args: args, Env: env, TimeoutSeconds: timeout})
}

func (s *Server) loadTarget(ctx context.Context, ref string) (*store.BackupTarget, error) {
	t, err := s.db.GetBackupTarget(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("backup target not found")
	}
	return t, err
}

func (s *Server) registerBackups() {
	huma.Register(s.api, huma.Operation{
		OperationID: "backup-targets-list", Method: http.MethodGet, Path: "/backups/targets", Summary: "Backup targets (restic repositories)", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*targetsOutput, error) {
		list, err := s.db.ListBackupTargets(ctx)
		if err != nil {
			return nil, err
		}
		return &targetsOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backup-targets-create", Method: http.MethodPost, Path: "/backups/targets", Summary: "Register a repository (initialised on first run)", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *targetInput) (*targetOutput, error) {
		p := principalFrom(ctx)
		if s.secrets == nil {
			return nil, huma.Error422UnprocessableEntity("secret key unavailable; run mp setup")
		}
		b := in.Body
		switch b.Type {
		case "local":
			if !filepath.IsAbs(b.Repository) || strings.HasPrefix(filepath.Clean(b.Repository), s.cfg.WWWRoot+"/") {
				return nil, huma.Error422UnprocessableEntity("local repository must be an absolute path outside " + s.cfg.WWWRoot)
			}
		case "sftp", "s3", "b2", "rest":
			if !strings.HasPrefix(b.Repository, b.Type+":") {
				return nil, huma.Error422UnprocessableEntity("repository must start with " + b.Type + ":")
			}
		}
		for k := range b.Env {
			if !envKeyRe.MatchString(k) {
				return nil, huma.Error422UnprocessableEntity("invalid env key " + k)
			}
		}
		password := b.Password
		generated := false
		if password == "" {
			password, _ = auth.NewToken(24)
			generated = true
		}
		pwEnc, err := s.secrets.Encrypt(password)
		if err != nil {
			return nil, err
		}
		envEnc := ""
		if len(b.Env) > 0 {
			raw, _ := json.Marshal(b.Env)
			if envEnc, err = s.secrets.Encrypt(string(raw)); err != nil {
				return nil, err
			}
		}
		t := &store.BackupTarget{Name: b.Name, Type: b.Type, Repository: b.Repository, PasswordEnc: pwEnc, EnvEnc: envEnc, KeepDaily: b.KeepDaily, KeepWeekly: b.KeepWeekly, KeepMonthly: b.KeepMonthly, Schedule: b.Schedule, Enabled: true}
		if t.KeepDaily == 0 && t.KeepWeekly == 0 && t.KeepMonthly == 0 {
			t.KeepDaily, t.KeepWeekly, t.KeepMonthly = 7, 4, 3
		}
		if err := s.db.CreateBackupTarget(ctx, t); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("target name already used")
			}
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "backup.target.create", Target: t.Name, IP: requestInfo(ctx).IP})
		out := &targetOutput{Status: http.StatusCreated, Body: apitypes.BackupTargetResponse{Target: t}}
		if generated {
			out.Body.Password = password
		}
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backup-targets-delete", Method: http.MethodDelete, Path: "/backups/targets/{ref}", Summary: "Forget a target (repository data is left untouched)", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *targetRefInput) (*struct{}, error) {
		p := principalFrom(ctx)
		t, err := s.loadTarget(ctx, in.Ref)
		if err != nil {
			return nil, err
		}
		if err := s.db.DeleteBackupTarget(ctx, t.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "backup.target.delete", Target: t.Name, IP: requestInfo(ctx).IP})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backup-snapshots", Method: http.MethodGet, Path: "/backups/targets/{ref}/snapshots", Summary: "Snapshots in the repository", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *targetRefInput) (*snapshotsOutput, error) {
		t, err := s.loadTarget(ctx, in.Ref)
		if err != nil {
			return nil, err
		}
		env, err := s.resticEnv(t)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		res, err := s.restic(ctx, env, 120, "snapshots", "--json", "--tag", "monopanel")
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if res.ExitCode != 0 {
			return nil, huma.Error502BadGateway("restic: " + strings.TrimSpace(res.Output))
		}
		return &snapshotsOutput{Body: parseSnapshots(res.Output)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backups-list", Method: http.MethodGet, Path: "/backups", Summary: "Backup runs", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *backupsListInput) (*backupsOutput, error) {
		var tid int64
		if in.Target != "" {
			t, err := s.loadTarget(ctx, in.Target)
			if err != nil {
				return nil, err
			}
			tid = t.ID
		}
		list, err := s.db.ListBackups(ctx, tid, in.Limit)
		if err != nil {
			return nil, err
		}
		return &backupsOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backups-run", Method: http.MethodPost, Path: "/backups/run", Summary: "Run a backup now (async)", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *backupRunInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		t, err := s.loadTarget(ctx, in.Body.Target)
		if err != nil {
			return nil, err
		}
		scope := in.Body.Scope
		if scope == "" {
			scope = "server"
		}
		if err := s.validateScope(ctx, scope); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		job, err := s.jobs.Enqueue(ctx, "backup.run", backupPayload{TargetID: t.ID, Scope: scope}, jobs.WithLockKey("backup:"+t.Name), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "backup.run", Target: t.Name + " " + scope, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "backups-restore", Method: http.MethodPost, Path: "/backups/restore", Summary: "Restore a snapshot (async)", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *restoreInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		t, err := s.loadTarget(ctx, in.Body.Target)
		if err != nil {
			return nil, err
		}
		if in.Body.InPlace && len(in.Body.Include) == 0 {
			return nil, huma.Error422UnprocessableEntity("in-place restore requires include paths")
		}
		for _, inc := range in.Body.Include {
			if !filepath.IsAbs(inc) {
				return nil, huma.Error422UnprocessableEntity("include paths must be absolute")
			}
		}
		job, err := s.jobs.Enqueue(ctx, "backup.restore", backupPayload{TargetID: t.ID, Snapshot: in.Body.Snapshot, Include: in.Body.Include, InPlace: in.Body.InPlace}, jobs.WithLockKey("backup:"+t.Name), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "backup.restore", Target: t.Name + " " + in.Body.Snapshot, IP: requestInfo(ctx).IP, Details: map[string]any{"in_place": in.Body.InPlace, "include": in.Body.Include}})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

func parseSnapshots(out string) []apitypes.Snapshot {
	var raw []struct {
		ID       string   `json:"id"`
		ShortID  string   `json:"short_id"`
		Time     string   `json:"time"`
		Hostname string   `json:"hostname"`
		Paths    []string `json:"paths"`
		Tags     []string `json:"tags"`
	}
	list := []apitypes.Snapshot{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			if json.Unmarshal([]byte(line), &raw) == nil {
				for _, r := range raw {
					list = append(list, apitypes.Snapshot{ID: r.ID, ShortID: r.ShortID, Time: r.Time, Hostname: r.Hostname, Paths: r.Paths, Tags: r.Tags})
				}
			}
		}
	}
	return list
}

func (s *Server) validateScope(ctx context.Context, scope string) error {
	kind, arg, _ := strings.Cut(scope, ":")
	switch kind {
	case "server":
		return nil
	case "user":
		_, err := s.db.GetUserByLogin(ctx, arg)
		return err
	case "site":
		_, err := s.db.GetSiteByDomain(ctx, arg)
		return err
	case "db":
		_, err := s.db.GetDatabaseByName(ctx, arg)
		return err
	}
	return errors.New("scope must be server, user:<login>, site:<domain> or db:<name>")
}

// backupPaths resolves the file paths and databases of a scope.
func (s *Server) backupPaths(ctx context.Context, scope string) (paths []string, dbs []string, err error) {
	kind, arg, _ := strings.Cut(scope, ":")
	switch kind {
	case "server":
		paths = []string{s.cfg.WWWRoot, s.cfg.ConfigDir, filepath.Join(s.cfg.DataDir, "certs"), filepath.Join(s.cfg.DataDir, "acme")}
		all, err := s.db.ListDatabases(ctx, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range all {
			dbs = append(dbs, d.Name)
		}
	case "user":
		u, err := s.db.GetUserByLogin(ctx, arg)
		if err != nil {
			return nil, nil, err
		}
		home := u.Home
		if home == "" {
			home = path.Join(s.cfg.WWWRoot, u.Login)
		}
		paths = []string{home}
		list, _ := s.db.ListDatabases(ctx, u.ID)
		for _, d := range list {
			dbs = append(dbs, d.Name)
		}
	case "site":
		site, err := s.db.GetSiteByDomain(ctx, arg)
		if err != nil {
			return nil, nil, err
		}
		u, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, nil, err
		}
		l := s.layoutFor(site, u)
		paths = []string{l.siteRoot}
		list, _ := s.db.ListDatabases(ctx, u.ID)
		for _, d := range list {
			dbs = append(dbs, d.Name)
		}
	case "db":
		dbs = []string{arg}
	}
	return paths, dbs, nil
}

func (s *Server) jobBackupRun(ctx context.Context, jc *jobs.Context) error {
	var p backupPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	t, err := s.db.GetBackupTarget(ctx, fmt.Sprint(p.TargetID))
	if err != nil {
		return err
	}
	scope := p.Scope
	if scope == "" {
		scope = "server"
	}
	run := &store.Backup{TargetID: t.ID, Scope: scope}
	if err := s.db.CreateBackup(ctx, run); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = s.db.FinishBackup(context.WithoutCancel(ctx), run.ID, "failed", "", 0, 0, err.Error())
		_ = s.db.SetBackupTargetRun(context.WithoutCancel(ctx), t.ID, "failed", err.Error())
		return err
	}
	env, err := s.resticEnv(t)
	if err != nil {
		return fail(err)
	}
	if err := s.ensurePackages(ctx, jc, "restic"); err != nil {
		return fail(err)
	}
	jc.Progress(5, "repository")
	if res, err := s.restic(ctx, env, 300, "cat", "config"); err != nil {
		return fail(err)
	} else if res.ExitCode != 0 {
		jc.Logf("initialising repository %s", t.Repository)
		init, err := s.restic(ctx, env, 300, "init")
		if err != nil {
			return fail(err)
		}
		if init.ExitCode != 0 {
			return fail(fmt.Errorf("restic init: %s", strings.TrimSpace(init.Output)))
		}
	}
	paths, dbs, err := s.backupPaths(ctx, scope)
	if err != nil {
		return fail(err)
	}
	tmp := filepath.Join(s.cfg.DataDir, "backups", "tmp", fmt.Sprintf("run-%d", run.ID))
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fail(err)
	}
	// The staging directory is the API's own and so is everything in it —
	// the dumps come through the agent's stream into files the API makes —
	// so it goes the same way: the agent writes and removes nothing below
	// the API's data directory.
	defer os.RemoveAll(tmp) //nolint:errcheck // cleanup: the caller already reports the real error
	jc.Progress(15, "database dumps")
	inst, instErr := s.db.GetDBInstance(ctx)
	if len(dbs) > 0 && (instErr != nil || inst.Status != store.DBReady) {
		jc.Logf("database server not installed; skipping %d dumps", len(dbs))
		dbs = nil
	}
	for _, name := range dbs {
		out := filepath.Join(tmp, name+".sql")
		if err := s.dumpDatabase(ctx, name, out); err != nil {
			return fail(err)
		}
		paths = append(paths, out)
		jc.Logf("dumped %s", name)
	}
	if scope == "server" {
		dbCopy := filepath.Join(tmp, "panel.db")
		if _, err := s.db.SQL().ExecContext(ctx, "VACUUM INTO ?", dbCopy); err != nil {
			jc.Logf("warning: panel database copy failed: %v", err)
		} else {
			paths = append(paths, dbCopy)
		}
	}
	if len(paths) == 0 {
		return fail(errors.New("nothing to back up"))
	}
	jc.Progress(30, "restic backup")
	args := append([]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=" + scope}, paths...)
	res, err := s.restic(ctx, env, 6*3600, args...)
	if err != nil {
		return fail(err)
	}
	var summary struct {
		SnapshotID string `json:"snapshot_id"`
		Files      int64  `json:"total_files_processed"`
		Bytes      int64  `json:"total_bytes_processed"`
		Added      int64  `json:"data_added"`
	}
	for _, line := range strings.Split(res.Output, "\n") {
		if strings.Contains(line, `"message_type":"summary"`) {
			_ = json.Unmarshal([]byte(line), &summary)
		}
	}
	if summary.SnapshotID == "" {
		return fail(fmt.Errorf("restic backup (exit %d): %s", res.ExitCode, lastLinesJoin(res.Output, 5)))
	}
	// A snapshot restic made without some files (exit 3: they vanished, or
	// it could not read them — the agent's sandbox refuses what a symlink in
	// a home leads to outside it) holds everything else and restores: the
	// run is not failed for it, and it is not "done" either. It is
	// "partial", with what was left out, on the run and on the target.
	status, note := "done", ""
	if res.ExitCode != 0 {
		status, note = "partial", incompleteBackup(res.ExitCode, res.Output)
		jc.Logf("warning: %s", note)
	}
	jc.Logf("snapshot %s: %d files, %s processed, %s added", summary.SnapshotID[:8], summary.Files, humanBytes(summary.Bytes), humanBytes(summary.Added))
	jc.Progress(80, "retention")
	forget, err := s.restic(ctx, env, 3600, "forget", "--prune", "--tag", "monopanel", "--keep-daily", fmt.Sprint(t.KeepDaily), "--keep-weekly", fmt.Sprint(t.KeepWeekly), "--keep-monthly", fmt.Sprint(t.KeepMonthly))
	if err != nil {
		jc.Logf("warning: forget: %v", err)
	} else if forget.ExitCode != 0 {
		jc.Logf("warning: forget exit %d: %s", forget.ExitCode, lastLinesJoin(forget.Output, 3))
	}
	if err := s.db.FinishBackup(context.WithoutCancel(ctx), run.ID, status, summary.SnapshotID, summary.Bytes, summary.Files, note); err != nil {
		return err
	}
	_ = s.db.SetBackupTargetRun(context.WithoutCancel(ctx), t.ID, status, note)
	if status == "partial" {
		jc.Progress(100, "backup "+summary.SnapshotID[:8]+" done without some files")
		return nil
	}
	jc.Progress(100, "backup "+summary.SnapshotID[:8]+" done")
	return nil
}

// incompleteBackup says what a restic backup that exited with code (3: the
// snapshot is there, some of its sources are not) left out, from its --json
// error lines — {"message_type":"error","error":{"message":…},"item":…},
// one for the scan and one for the archival of the same item — or the plain
// "error: …" lines of older versions.
func incompleteBackup(code int, out string) string {
	var shown []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		var e struct {
			MessageType string `json:"message_type"`
			Error       struct {
				Message string `json:"message"`
			} `json:"error"`
			Item string `json:"item"`
		}
		what, item := "", ""
		switch {
		case strings.HasPrefix(line, "{") && strings.Contains(line, `"message_type":"error"`) && json.Unmarshal([]byte(line), &e) == nil:
			what, item = e.Error.Message, e.Item
			switch {
			case what == "":
				what = item
			case item != "" && !strings.Contains(what, item):
				what = item + ": " + what
			}
		case strings.HasPrefix(line, "error: "):
			what = strings.TrimPrefix(line, "error: ")
		default:
			continue
		}
		if item == "" {
			item = what
		}
		if what == "" || seen[item] {
			continue
		}
		seen[item] = true
		if len(shown) < 3 {
			if len(what) > 200 {
				what = what[:200] + "…"
			}
			shown = append(shown, what)
		}
	}
	if len(seen) == 0 {
		return fmt.Sprintf("restic exit %d: the snapshot lacks what restic could not read", code)
	}
	paths := "1 path"
	if len(seen) > 1 {
		paths = fmt.Sprintf("%d paths", len(seen))
	}
	more := ""
	if len(seen) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(seen)-len(shown))
	}
	return fmt.Sprintf("restic exit %d: the snapshot lacks %s restic could not read: %s%s", code, paths, strings.Join(shown, "; "), more)
}

func (s *Server) jobBackupRestore(ctx context.Context, jc *jobs.Context) error {
	var p backupPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	t, err := s.db.GetBackupTarget(ctx, fmt.Sprint(p.TargetID))
	if err != nil {
		return err
	}
	env, err := s.resticEnv(t)
	if err != nil {
		return err
	}
	// Aside, a snapshot lands in a directory of the agent's, root's alone:
	// restic writes as root, and in this process's data directory a symlink
	// would carry the write anywhere. In place, the agent stages the restore
	// and moves the result into place itself.
	target := filepath.Join(agent.RestoreDir, p.Snapshot)
	if p.InPlace {
		target = "/"
	}
	args := []string{"restore", p.Snapshot, "--target", target}
	for _, inc := range p.Include {
		args = append(args, "--include", inc)
	}
	jc.Progress(10, "restic restore")
	res, err := s.restic(ctx, env, 6*3600, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restic restore (exit %d): %s", res.ExitCode, lastLinesJoin(res.Output, 5))
	}
	logTail(jc, res.Output, 3)
	jc.Progress(100, "restored to "+target)
	return nil
}

// dumpDatabase writes the dump of a database into out, a file of the API's
// own: mysqldump runs as root through the agent's stream, and its output
// arrives here instead of being written by root into the API's directory.
func (s *Server) dumpDatabase(ctx context.Context, name, out string) (err error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if err := s.agent.StreamOut(ctx, &agent.StreamRequest{Name: "mysqldump", Args: []string{"--protocol=socket", "--single-transaction", "--routines", "--triggers", "--events", "--databases", name}, TimeoutSeconds: 3600}, f); err != nil {
		return fmt.Errorf("mysqldump %s: %w", name, err)
	}
	return nil
}

func lastLinesJoin(s string, n int) string {
	return strings.Join(lastLines(s, n), " | ")
}

func humanBytes(b int64) string {
	const unit = 1024.0
	f := float64(b)
	for _, s := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= unit
	}
	return fmt.Sprintf("%.1f PB", f)
}

// scheduledBackups enqueues daily backups for targets that are due.
func (s *Server) scheduledBackups(ctx context.Context) {
	targets, err := s.db.ListBackupTargets(ctx)
	if err != nil {
		return
	}
	for _, t := range targets {
		if !t.Enabled || t.Schedule != "daily" {
			continue
		}
		if t.LastRunAt != nil && time.Since(*t.LastRunAt) < 23*time.Hour {
			continue
		}
		key := fmt.Sprintf("backup:%d:%s", t.ID, time.Now().UTC().Format("2006-01-02"))
		if _, err := s.jobs.Enqueue(ctx, "backup.run", backupPayload{TargetID: t.ID, Scope: "server"}, jobs.WithLockKey("backup:"+t.Name), jobs.WithRequestedBy("scheduler"), jobs.WithIdempotencyKey(key)); err == nil {
			s.log.Info("scheduled backup", "target", t.Name)
		}
	}
}
