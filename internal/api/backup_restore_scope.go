package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// A restore takes a part of a snapshot the way a backup takes a part of the
// server (backupPaths): an account — its home and its databases —, a site's
// files, a database, or the whole snapshot. The panel finds the paths and the
// dumps in the snapshot itself. Aside, they land in the agent's restore
// directory, the dumps as .sql files; in place, the files go back where they
// were, and each dump is loaded back into its database (restoreDatabases).

// snapshotOf finds a snapshot of a target by its id or short id.
func (s *Server) snapshotOf(ctx context.Context, t *store.BackupTarget, id string) (*apitypes.Snapshot, error) {
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
	for _, snap := range parseSnapshots(res.Output) {
		if snap.ID == id || snap.ShortID == id {
			return &snap, nil
		}
	}
	return nil, huma.Error404NotFound("no snapshot " + id + " in " + t.Name)
}

// snapshotHolds says whether a snapshot's paths take p in: p itself or a
// directory above it.
func snapshotHolds(snap *apitypes.Snapshot, p string) bool {
	for _, sp := range snap.Paths {
		if p == sp || strings.HasPrefix(p, strings.TrimSuffix(sp, "/")+"/") {
			return true
		}
	}
	return false
}

// snapshotDump is the path of a database's dump in a snapshot: a backup run
// writes <data>/backups/tmp/run-<id>/<name>.sql and backs it up as a path of
// its own (jobBackupRun).
func (s *Server) snapshotDump(snap *apitypes.Snapshot, name string) string {
	dir := filepath.Join(s.cfg.DataDir, "backups", "tmp") + "/"
	for _, p := range snap.Paths {
		if strings.HasPrefix(p, dir) && filepath.Base(p) == name+".sql" {
			return p
		}
	}
	return ""
}

// restorePlan resolves what a scope restores from a snapshot: the paths to
// bring back and, by database, the dump to load. A part the snapshot does
// not hold is a 422 that says so.
func (s *Server) restorePlan(ctx context.Context, snap *apitypes.Snapshot, scope string) (include []string, dumps map[string]string, err error) {
	if err := s.validateScope(ctx, scope); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, huma.Error422UnprocessableEntity("restore " + scope + ": no such account, site or database here; one that is gone is restored aside by its paths")
		}
		return nil, nil, huma.Error422UnprocessableEntity(err.Error())
	}
	dumps = map[string]string{}
	files := func(p string) error {
		if !snapshotHolds(snap, p) {
			return huma.Error422UnprocessableEntity(fmt.Sprintf("snapshot %s holds no %s (its paths: %s)", snap.ShortID, p, strings.Join(snap.Paths, ", ")))
		}
		include = append(include, p)
		return nil
	}
	database := func(name string, required bool) error {
		p := s.snapshotDump(snap, name)
		if p == "" {
			if required {
				return huma.Error422UnprocessableEntity(fmt.Sprintf("snapshot %s holds no dump of the database %s", snap.ShortID, name))
			}
			return nil
		}
		dumps[name] = p
		include = append(include, p)
		return nil
	}
	kind, arg, _ := strings.Cut(scope, ":")
	switch kind {
	case "user":
		u, err := s.db.GetUserByLogin(ctx, arg)
		if err != nil {
			return nil, nil, err
		}
		if err := files(s.homeOf(u)); err != nil {
			return nil, nil, err
		}
		dbs, _ := s.db.ListDatabases(ctx, u.ID)
		for _, d := range dbs {
			if err := database(d.Name, false); err != nil {
				return nil, nil, err
			}
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
		if err := files(s.layoutFor(site, u).siteRoot); err != nil {
			return nil, nil, err
		}
	case "db":
		if err := database(arg, true); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, huma.Error422UnprocessableEntity("restore by scope: user:<login>, site:<domain> or db:<name>; the whole snapshot is restored aside")
	}
	return include, dumps, nil
}

// restoreDatabases loads the dumps an in-place restore brought back into
// their databases — as root over the socket, the dump's own CREATE DATABASE
// IF NOT EXISTS and USE leading it to its database, each table replaced
// (mysqldump writes DROP TABLE IF EXISTS before each) — and removes each
// dump once loaded or not: it lies in the panel's data directory, which the
// next backup run does not want. A database removed from the panel since the
// request is not brought back.
func (s *Server) restoreDatabases(ctx context.Context, jc *jobs.Context, dumps map[string]string) error {
	names := make([]string, 0, len(dumps))
	for name := range dumps {
		names = append(names, name)
	}
	sort.Strings(names)
	var failed []string
	for _, name := range names {
		p := dumps[name]
		err := s.loadRestoredDump(ctx, name, p)
		// The run's directory too, once empty: the panel's own, and a
		// leftover is only clutter.
		os.Remove(p)               //nolint:errcheck // see above
		os.Remove(filepath.Dir(p)) //nolint:errcheck // not empty: another dump of it waits for its turn
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		jc.Logf("database %s loaded from the snapshot", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("databases not restored: %s", strings.Join(failed, "; "))
	}
	return nil
}

// loadRestoredDump loads a dump back into its database the way a migration
// loads one (migrate_dump.go): as the database's own account, which may do
// anything inside it and nothing outside — a dump from a repository somebody
// else could write to must not run as MySQL's root —, through the filter that
// points every DEFINER at that account. The account takes auth_socket for
// root while it loads and gets its own authentication back after it, the hash
// read from mysql.user, whatever happened in between.
func (s *Server) loadRestoredDump(ctx context.Context, name, p string) error {
	d, err := s.db.GetDatabaseByName(ctx, name)
	if err != nil {
		return fmt.Errorf("not in the panel any more: %w", err)
	}
	if !strings.HasPrefix(p, filepath.Join(s.cfg.DataDir, "backups", "tmp")+"/") {
		return errors.New("a dump outside the backup runs' directory")
	}
	account := ""
	if users, err := s.db.ListDBUsers(ctx, d.ID); err == nil {
		for _, u := range users {
			if u.Host == "localhost" && mysqlAccountRe.MatchString(u.Name) {
				account = u.Name
				break
			}
		}
	}
	if account == "" {
		return errors.New("the database has no account of its own on localhost to load it as: restore its dump aside and load it with mysql by hand")
	}
	out, err := s.mysqlExec(ctx, fmt.Sprintf("SELECT plugin, HEX(authentication_string) FROM mysql.user WHERE user='%s' AND host='localhost';", account))
	if err != nil {
		return err
	}
	plugin, hash, ok := strings.Cut(strings.TrimSpace(out), "\t")
	if !ok || !mysqlAccountRe.MatchString(plugin) || !hexRe.MatchString(hash) {
		return fmt.Errorf("the account %s@localhost: its authentication could not be read", account)
	}
	if _, err := s.mysqlExec(ctx, fmt.Sprintf("ALTER USER '%s'@'localhost' %s;", account, loaderAuth)); err != nil {
		return err
	}
	loadErr := s.streamRestoredDump(ctx, name, account, p)
	back := fmt.Sprintf("ALTER USER '%s'@'localhost' IDENTIFIED WITH %s AS 0x%s;", account, plugin, hash)
	if hash == "" {
		back = fmt.Sprintf("ALTER USER '%s'@'localhost' IDENTIFIED WITH %s;", account, plugin)
	}
	if _, err := s.mysqlExec(context.WithoutCancel(ctx), back); err != nil {
		return errors.Join(loadErr, fmt.Errorf("the account %s@localhost did not get its password back and stays on auth_socket: mp db passwd %s sets one (and the site's settings need it): %w", account, name, err))
	}
	return loadErr
}

// mysqlAccountRe is a MySQL account or plugin name the panel writes into SQL
// unquoted-safe; hexRe a hex string.
var (
	mysqlAccountRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	hexRe          = regexp.MustCompile(`^[0-9A-Fa-f]*$`)
)

func (s *Server) streamRestoredDump(ctx context.Context, name, account, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	// --local-infile=0 and --binary-mode as for a migration's dump.
	out, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "mysql", Args: []string{"--protocol=socket", "--binary-mode", "--local-infile=0", "--user=" + account, name}, TimeoutSeconds: seconds(resticLimit)}, newDumpFilter(f))
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("mysql: %s", toolOutput(out.Output))
	}
	return nil
}

type snapshotLsInput struct {
	Ref      string `path:"ref"`
	Snapshot string `path:"snapshot" pattern:"^([0-9a-f]{8,64}|latest)$"`
	Path     string `query:"path" maxLength:"4096" doc:"an absolute directory of the snapshot; empty lists the snapshot's own paths"`
}

type snapshotLsOutput struct {
	Body apitypes.SnapshotListing
}

// registerSnapshotBrowse is the listing a restore picks its paths from, a
// file at a time if need be: one directory of a snapshot, its entries with
// type, size and time — names, never content.
func (s *Server) registerSnapshotBrowse() {
	huma.Register(s.api, huma.Operation{
		OperationID: "backup-snapshot-ls", Method: http.MethodGet, Path: "/backups/targets/{ref}/snapshots/{snapshot}/ls", Summary: "What a directory of a snapshot holds", Tags: []string{"backups"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *snapshotLsInput) (*snapshotLsOutput, error) {
		t, err := s.loadTarget(ctx, in.Ref)
		if err != nil {
			return nil, err
		}
		if in.Path == "" {
			snap, err := s.snapshotOf(ctx, t, in.Snapshot)
			if err != nil {
				return nil, err
			}
			out := apitypes.SnapshotListing{Path: "", Entries: []apitypes.SnapshotEntry{}}
			for _, p := range snap.Paths {
				kind := "dir"
				if ext := filepath.Ext(p); ext == ".sql" || ext == ".db" {
					kind = "file"
				}
				out.Entries = append(out.Entries, apitypes.SnapshotEntry{Name: p, Type: kind, Path: p})
			}
			return &snapshotLsOutput{Body: out}, nil
		}
		p := filepath.Clean(in.Path)
		if !filepath.IsAbs(p) || p != in.Path && p+"/" != in.Path {
			return nil, huma.Error422UnprocessableEntity("path must be a clean absolute path")
		}
		env, err := s.resticEnv(t)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		res, err := s.restic(ctx, env, 300, "ls", "--json", in.Snapshot, p)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if res.ExitCode != 0 {
			return nil, huma.Error502BadGateway("restic: " + lastLinesJoin(res.Output, 3))
		}
		return &snapshotLsOutput{Body: parseSnapshotLs(p, res.Output)}, nil
	})
}

// parseSnapshotLs reads restic ls --json: the snapshot's line, then one line
// a node — the directory itself among them, left out (a file asked for is
// its only entry). The agent keeps the last 512 KiB of a tool's output: a
// directory of more entries comes back cut at its start, which says so.
func parseSnapshotLs(dir, out string) apitypes.SnapshotListing {
	listing := apitypes.SnapshotListing{Path: dir, Entries: []apitypes.SnapshotEntry{}}
	listing.Truncated = strings.HasPrefix(out, "[...truncated...]")
	var self *apitypes.SnapshotEntry
	for _, line := range strings.Split(out, "\n") {
		var n struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Path        string `json:"path"`
			Size        int64  `json:"size"`
			MTime       string `json:"mtime"`
			StructType  string `json:"struct_type"`
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &n) != nil || n.Path == "" || n.StructType == "snapshot" || n.MessageType == "snapshot" {
			continue
		}
		e := apitypes.SnapshotEntry{Name: n.Name, Type: n.Type, Path: n.Path, Size: n.Size, MTime: n.MTime}
		if n.Path == dir {
			self = &e
			continue
		}
		if filepath.Dir(n.Path) == dir {
			listing.Entries = append(listing.Entries, e)
		}
	}
	if len(listing.Entries) == 0 && self != nil && self.Type != "dir" {
		listing.Entries = append(listing.Entries, *self)
	}
	sort.SliceStable(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return a.Name < b.Name
	})
	return listing
}
