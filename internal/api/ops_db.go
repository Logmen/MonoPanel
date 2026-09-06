package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const (
	perconaReleaseDeb = "https://repo.percona.com/apt/percona-release_latest.generic_all.deb"
	perconaReleaseRPM = "https://repo.percona.com/yum/percona-release-latest.noarch.rpm"
	mysqlKeyURL       = "https://repo.mysql.com/RPM-GPG-KEY-mysql-2023"
	settingDB         = "stack.db"
)

var (
	dbSuffixRe = regexp.MustCompile(`^[a-z0-9_]{1,24}$`)
	sqlEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)
)

type dbEngineOutput struct {
	Body apitypes.DBEngineStatus
}

type databasesOutput struct {
	Body []*store.Database
}

type databaseInput struct {
	Body apitypes.DatabaseRequest
}

type databaseOutput struct {
	Status int
	Body   apitypes.DatabaseResponse
}

type dbNameInput struct {
	Name string `path:"name" pattern:"^[a-z0-9_]{1,64}$"`
}

type dbPasswordInput struct {
	Name string `path:"name" pattern:"^[a-z0-9_]{1,64}$"`
	Body apitypes.PasswordRequest
}

// mysqlExec runs SQL as root over the unix socket (auth_socket) through the agent.
func (s *Server) mysqlExec(ctx context.Context, sql string) (string, error) {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "mysql", Args: []string{"--protocol=socket", "--batch", "--skip-column-names"}, Stdin: sql, TimeoutSeconds: 120})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Output, fmt.Errorf("mysql: %s", strings.TrimSpace(res.Output))
	}
	return res.Output, nil
}

func (s *Server) dbInstance(ctx context.Context) (*store.DBInstance, error) {
	inst, err := s.db.GetDBInstance(ctx)
	if errors.Is(err, store.ErrNotFound) || (err == nil && inst.Status != store.DBReady) {
		return nil, errors.New("database server is not installed (mp stack install percona)")
	}
	return inst, err
}

func (s *Server) registerDB() {
	huma.Register(s.api, huma.Operation{
		OperationID: "db-engine", Method: http.MethodGet, Path: "/db/engine", Summary: "Database server status", Tags: []string{"db"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*dbEngineOutput, error) {
		out := &dbEngineOutput{}
		inst, err := s.db.GetDBInstance(ctx)
		if err == nil {
			out.Body.Installed = inst.Status == store.DBReady
			out.Body.Instance = inst
			actx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if st, err := s.agent.Service(actx, inst.Service, "status"); err == nil {
				out.Body.Service = &st.Status
			}
			cancel()
		}
		if list, err := s.db.ListDatabases(ctx, 0); err == nil {
			out.Body.Databases = len(list)
		}
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "databases-list", Method: http.MethodGet, Path: "/databases", Summary: "List databases (admins: all, users: own)", Tags: []string{"db"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*databasesOutput, error) {
		p := principalFrom(ctx)
		var uid int64
		if p.Role != store.RoleAdmin {
			uid = p.UserID
		}
		list, err := s.db.ListDatabases(ctx, uid)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.Database{}
		}
		s.refreshDBSizes(ctx, list)
		return &databasesOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "databases-create", Method: http.MethodPost, Path: "/databases", Summary: "Create a database and its account", Tags: []string{"db"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *databaseInput) (*databaseOutput, error) {
		p := principalFrom(ctx)
		inst, err := s.dbInstance(ctx)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		var owner *store.User
		switch {
		case p.Role == store.RoleAdmin && in.Body.User != "":
			owner, err = s.db.GetUserByLogin(ctx, in.Body.User)
		case p.UserID != 0:
			owner, err = s.db.GetUserByID(ctx, p.UserID)
		default:
			return nil, huma.Error422UnprocessableEntity("user is required")
		}
		if err != nil || owner.Role != store.RoleUser {
			return nil, huma.Error422UnprocessableEntity("owner must be a panel user")
		}
		if in.Body.Remote {
			return nil, huma.Error422UnprocessableEntity("remote access is not available until the firewall stage")
		}
		if !dbSuffixRe.MatchString(in.Body.Name) {
			return nil, huma.Error422UnprocessableEntity("invalid database name")
		}
		full := owner.Login + "_" + in.Body.Name
		if len(full) > 32 {
			return nil, huma.Error422UnprocessableEntity("login_name must be at most 32 characters (MySQL account limit)")
		}
		password := in.Body.Password
		generated := false
		if password == "" {
			password, _ = auth.NewToken(15)
			generated = true
		}
		plugin := "caching_sha2_password"
		legacy := false
		if in.Body.LegacyAuth != nil {
			legacy = *in.Body.LegacyAuth
		} else {
			legacy = s.ownerNeedsLegacyAuth(ctx, owner.ID)
		}
		if legacy {
			if err := s.ensureNativePassword(ctx, inst); err != nil {
				return nil, err
			}
			plugin = "mysql_native_password"
		}
		sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;\n"+
			"CREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED WITH %s BY '%s';\n"+
			"ALTER USER '%s'@'localhost' IDENTIFIED WITH %s BY '%s';\n"+
			"GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'localhost';\nFLUSH PRIVILEGES;\n",
			full, full, plugin, sqlEscaper.Replace(password), full, plugin, sqlEscaper.Replace(password), full, full)
		if _, err := s.mysqlExec(ctx, sql); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		db := &store.Database{UserID: owner.ID, Name: full}
		if err := s.db.CreateDatabase(ctx, db); err != nil && !errors.Is(err, store.ErrExists) {
			return nil, err
		} else if errors.Is(err, store.ErrExists) {
			db, _ = s.db.GetDatabaseByName(ctx, full)
		}
		if len(db.Users) == 0 {
			s.db.CreateDBUser(ctx, &store.DBUser{UserID: owner.ID, DatabaseID: &db.ID, Name: full, Host: "localhost", AuthPlugin: plugin}) //nolint:errcheck // best effort while rolling back a failed create
			db.Users, _ = s.db.ListDBUsers(ctx, db.ID)
		}
		db.Login = owner.Login
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "db.create", Target: full, IP: requestInfo(ctx).IP, Details: map[string]any{"auth": plugin}})
		out := &databaseOutput{Status: http.StatusCreated, Body: apitypes.DatabaseResponse{Database: db, DSN: fmt.Sprintf("mysql:host=localhost;unix_socket=%s;dbname=%s", inst.Socket, full)}}
		if generated {
			out.Body.Password = password
		}
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "databases-delete", Method: http.MethodDelete, Path: "/databases/{name}", Summary: "Drop a database and its accounts", Tags: []string{"db"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *dbNameInput) (*struct{}, error) {
		p := principalFrom(ctx)
		db, err := s.loadDatabaseFor(ctx, in.Name)
		if err != nil {
			return nil, err
		}
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;\n", db.Name)
		for _, u := range db.Users {
			sql += fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s';\n", u.Name, u.Host)
		}
		sql += "FLUSH PRIVILEGES;\n"
		if _, err := s.mysqlExec(ctx, sql); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if err := s.db.DeleteDatabase(ctx, db.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "db.delete", Target: db.Name, IP: requestInfo(ctx).IP})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "databases-password", Method: http.MethodPost, Path: "/databases/{name}/password", Summary: "Reset the account password", Tags: []string{"db"}, Security: secured,
	}, func(ctx context.Context, in *dbPasswordInput) (*databaseOutput, error) {
		p := principalFrom(ctx)
		db, err := s.loadDatabaseFor(ctx, in.Name)
		if err != nil {
			return nil, err
		}
		password := in.Body.Password
		generated := false
		if password == "" {
			password, _ = auth.NewToken(15)
			generated = true
		}
		sql := ""
		for _, u := range db.Users {
			sql += fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED WITH %s BY '%s';\n", u.Name, u.Host, u.AuthPlugin, sqlEscaper.Replace(password))
		}
		if sql == "" {
			return nil, huma.Error422UnprocessableEntity("database has no accounts")
		}
		if _, err := s.mysqlExec(ctx, sql+"FLUSH PRIVILEGES;\n"); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "db.password", Target: db.Name, IP: requestInfo(ctx).IP})
		out := &databaseOutput{Status: http.StatusOK, Body: apitypes.DatabaseResponse{Database: db}}
		if generated {
			out.Body.Password = password
		}
		return out, nil
	})
}

func (s *Server) loadDatabaseFor(ctx context.Context, name string) (*store.Database, error) {
	db, err := s.db.GetDatabaseByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("database not found")
	}
	if err != nil {
		return nil, err
	}
	p := principalFrom(ctx)
	if p.Role != store.RoleAdmin && p.UserID != db.UserID {
		return nil, huma.Error404NotFound("database not found")
	}
	return db, nil
}

func (s *Server) ownerNeedsLegacyAuth(ctx context.Context, userID int64) bool {
	sites, err := s.db.ListSites(ctx, userID)
	if err != nil {
		return false
	}
	for _, site := range sites {
		if versionLess(site.PHPVersion, "7.4") {
			return true
		}
	}
	return false
}

func versionLess(a, b string) bool {
	var am, an, bm, bn int
	fmt.Sscanf(a, "%d.%d", &am, &an)
	fmt.Sscanf(b, "%d.%d", &bm, &bn)
	return am < bm || (am == bm && an < bn)
}

func (s *Server) anyLegacyPHP(ctx context.Context) bool {
	versions, _ := s.db.ListPHPVersions(ctx)
	for _, v := range versions {
		if v.Status == store.PHPInstalled && versionLess(v.Version, "7.4") {
			return true
		}
	}
	return false
}

// ensureNativePassword enables mysql_native_password (restarting the server)
// when legacy PHP needs it.
func (s *Server) ensureNativePassword(ctx context.Context, inst *store.DBInstance) error {
	if inst.NativePassword {
		return nil
	}
	inst.NativePassword = true
	if err := s.writeDBConfig(ctx, inst, true); err != nil {
		return err
	}
	return s.db.UpsertDBInstance(ctx, inst)
}

// writeDBConfig renders zz-monopanel.cnf for the host size and restarts the server.
func (s *Server) writeDBConfig(ctx context.Context, inst *store.DBInstance, restart bool) error {
	layout := osprofile.DB(s.profile, inst.Engine)
	info, err := s.agent.SystemInfo(ctx)
	if err != nil {
		return err
	}
	ramMB := int(info.MemTotalBytes / 1024 / 1024)
	pool := ramMB / 4
	if pool < 128 {
		pool = 128
	}
	redo := pool / 4
	if redo < 64 {
		redo = 64
	}
	if redo > 2048 {
		redo = 2048
	}
	conns := 100 + ramMB/64
	if conns > 1000 {
		conns = 1000
	}
	conf, err := s.render.Render("mysql/monopanel.cnf.tmpl", render.MySQLConf{RAMMB: ramMB, BindAddress: "127.0.0.1", BufferPoolMB: pool, RedoLogMB: redo, MaxConnections: conns, SlowLog: layout.SlowLog, NativePassword: inst.NativePassword})
	if err != nil {
		return err
	}
	req := &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: layout.ConfFile, Content: conf, Mode: 0o644}}, Origin: "db:config"}
	if restart {
		req.Restart = []string{layout.Service}
	}
	_, err = s.agent.ApplyConfigSet(ctx, req)
	return err
}

func (s *Server) refreshDBSizes(ctx context.Context, list []*store.Database) {
	if len(list) == 0 {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := s.mysqlExec(actx, "SELECT table_schema, COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables GROUP BY table_schema;")
	if err != nil {
		return
	}
	sizes := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			sizes[f[0]] = n
		}
	}
	for _, db := range list {
		if n, ok := sizes[db.Name]; ok && n != db.SizeBytes {
			db.SizeBytes = n
			s.db.SetDatabaseSize(ctx, db.ID, n) //nolint:errcheck // size refresh is informational
		}
	}
}

// installDB installs Percona Server or MySQL Community 8.4 and configures
// root access via auth_socket.
func (s *Server) installDB(ctx context.Context, jc *jobs.Context, engine string) error {
	rel := s.profile.Release()
	layout := osprofile.DB(s.profile, engine)
	inst := &store.DBInstance{Engine: engine, Status: store.DBInstalling, Service: layout.Service, Socket: layout.Socket}
	if existing, err := s.db.GetDBInstance(ctx); err == nil {
		if existing.Status == store.DBReady && existing.Engine != engine {
			return fmt.Errorf("%s is already installed; only one database engine per server", existing.Engine)
		}
		inst.ID = existing.ID
	}
	if err := s.db.UpsertDBInstance(ctx, inst); err != nil {
		return err
	}
	fail := func(err error) error {
		inst.Status, inst.LastError = store.DBError, err.Error()
		_ = s.db.UpsertDBInstance(context.WithoutCancel(ctx), inst)
		return err
	}
	jc.Progress(5, "repository")
	switch s.profile.Family() {
	case osprofile.FamilyDebian:
		if engine == store.EnginePercona {
			deb, err := fetchBytes(ctx, perconaReleaseDeb)
			if err != nil {
				return fail(fmt.Errorf("download percona-release: %w", err))
			}
			dl := filepath.Join(s.cfg.DataDir, "downloads")
			if err := os.MkdirAll(dl, 0o755); err != nil {
				return fail(err)
			}
			local := filepath.Join(dl, "percona-release_latest.generic_all.deb")
			if err := os.WriteFile(local, deb, 0o644); err != nil {
				return fail(err)
			}
			if _, err := s.agent.Pkg(ctx, "install", local); err != nil {
				return fail(err)
			}
			res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "percona-release", Args: []string{"setup", "ps-84-lts"}, TimeoutSeconds: 600})
			if err != nil {
				return fail(err)
			}
			if res.ExitCode != 0 {
				return fail(fmt.Errorf("percona-release setup: %s", strings.TrimSpace(res.Output)))
			}
			jc.Logf("repository: repo.percona.com ps-84-lts (%s)", rel.Codename)
		} else {
			key, err := fetchText(ctx, mysqlKeyURL)
			if err != nil {
				return fail(fmt.Errorf("download MySQL key: %w", err))
			}
			distro := strings.ToLower(rel.ID)
			files := []agent.FileSpec{
				{Path: "/etc/apt/keyrings/mysql.asc", Content: key, Mode: 0o644},
				{Path: "/etc/apt/sources.list.d/mysql.list", Content: fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/mysql.asc] http://repo.mysql.com/apt/%s/ %s mysql-8.4-lts mysql-tools\n", distro, rel.Codename), Mode: 0o644},
			}
			if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "db:repo"}); err != nil {
				return fail(err)
			}
			jc.Logf("repository: repo.mysql.com mysql-8.4-lts (%s)", rel.Codename)
		}
		if res, err := s.agent.Pkg(ctx, "update-index"); err != nil {
			return fail(err)
		} else {
			logTail(jc, res.Output, 2)
		}
	case osprofile.FamilyRHEL:
		if engine == store.EnginePercona {
			if _, err := s.agent.Pkg(ctx, "install", perconaReleaseRPM); err != nil {
				return fail(err)
			}
			if res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "percona-release", Args: []string{"setup", "ps-84-lts"}}); err != nil || res.ExitCode != 0 {
				return fail(fmt.Errorf("percona-release setup failed"))
			}
		} else {
			repo := fmt.Sprintf("[mysql-8.4-lts-community]\nname=MySQL 8.4 LTS Community Server\nbaseurl=https://repo.mysql.com/yum/mysql-8.4-community/el/%s/$basearch/\nenabled=1\ngpgcheck=1\ngpgkey=%s\n", rel.MajorVersion(), mysqlKeyURL)
			if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: "/etc/yum.repos.d/mysql-community.repo", Content: repo, Mode: 0o644}}, Origin: "db:repo"}); err != nil {
				return fail(err)
			}
		}
	default:
		return fail(errors.New("no database packages for this OS"))
	}

	jc.Progress(25, "installing "+engine+" server")
	res, err := s.agent.Pkg(ctx, "install", layout.Packages...)
	if err != nil {
		return fail(err)
	}
	logTail(jc, res.Output, 3)
	for pkg, v := range res.Installed {
		jc.Logf("%s %s", pkg, v)
	}

	jc.Progress(60, "root access via auth_socket")
	if _, err := s.agent.Service(ctx, layout.Service, "start"); err != nil {
		return fail(err)
	}
	if _, err := s.agent.Service(ctx, layout.Service, "enable"); err != nil {
		return fail(err)
	}
	var probe string
	for i := 0; i < 15; i++ {
		probe, err = s.mysqlExec(ctx, "SELECT VERSION();")
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fail(fmt.Errorf("cannot connect as root over the socket (%s); the package set a root password? %v", layout.Socket, err))
	}
	inst.Version = strings.TrimSpace(probe)
	if _, err := s.mysqlExec(ctx, "ALTER USER 'root'@'localhost' IDENTIFIED WITH auth_socket; FLUSH PRIVILEGES;"); err != nil {
		return fail(err)
	}
	jc.Logf("%s %s, root@localhost uses auth_socket", engine, inst.Version)

	jc.Progress(80, "configuration")
	inst.NativePassword = s.anyLegacyPHP(ctx)
	if err := s.writeDBConfig(ctx, inst, true); err != nil {
		return fail(err)
	}
	for i := 0; i < 15; i++ {
		if _, err = s.mysqlExec(ctx, "SELECT 1;"); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fail(fmt.Errorf("server did not come back after configuration: %v", err))
	}
	inst.Status, inst.LastError = store.DBReady, ""
	if err := s.db.UpsertDBInstance(context.WithoutCancel(ctx), inst); err != nil {
		return err
	}
	s.db.SetSetting(ctx, settingDB, engine)
	jc.Progress(100, engine+" "+inst.Version+" ready")
	return nil
}
