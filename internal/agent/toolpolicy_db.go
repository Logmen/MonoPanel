package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var mysqlUserRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,80}$`)

// mysqlForced goes in front of every mysql argv. --binary-mode turns off the
// client's own commands (\! runs a shell, \. and source read a file, \T
// writes one) for piped input, and --local-infile=0 stops LOAD DATA LOCAL
// from having the client, which runs as root, read a file of the input's
// choosing — the SQL comes from the API, and a migration's dump from another
// server. Nothing the panel pipes needs a client command: mysqldump's
// DELIMITER stays allowed in binary mode.
var mysqlForced = []string{"--binary-mode", "--local-infile=0"}

// planMySQL: SQL as root over the socket (internal/api/ops_db.go mysqlExec,
// mysqlExecWithPassword), a client's password check (ops_mail_webmail.go
// mysqlSignsIn), a statement for SphinxQL on 127.0.0.1:9306 (ops_tools.go
// sphinxQL) and a migration's dump loaded as the database's account
// (ops_migrate_import.go loadDump, /v1/stream/in). --defaults-file,
// --plugin-dir and --default-auth (a library loaded as root), --pager,
// --tee, --init-command, other hosts and the like are not among them.
func (s *Server) planMySQL(c *toolCall) (*toolPlan, error) {
	if c.mode == modeStreamOut {
		return nil, notAllowed("mysql through /v1/stream/out", "the panel streams dumps with mysqldump")
	}
	var proto, db string
	host, port, stmt, user := false, false, false, false
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		switch {
		case a == "--protocol=socket" || a == "--protocol=tcp":
			if proto != "" {
				return nil, notAllowed("mysql "+a, "the protocol is given once")
			}
			proto = strings.TrimPrefix(a, "--protocol=")
		case a == "-h127.0.0.1":
			host = true
		case a == "-P9306":
			port = true
		case a == "--batch", a == "--skip-column-names", a == "--connect-expired-password", a == "--binary-mode", a == "--local-infile=0":
		case strings.HasPrefix(a, "--user="):
			if user || !mysqlUserRe.MatchString(strings.TrimPrefix(a, "--user=")) {
				return nil, notAllowed("mysql "+a, "one account name of letters, digits and _")
			}
			user = true
		case a == "-e":
			if i+1 >= len(c.args) || stmt {
				return nil, notAllowed("mysql -e", "one statement follows it")
			}
			i++
			stmt = true
		case strings.HasPrefix(a, "-"):
			return nil, notAllowed("the mysql option "+a, "the panel never passes it")
		default:
			if db != "" || !dbNameRe.MatchString(a) {
				return nil, notAllowed("mysql "+a, "one database name of letters, digits and _")
			}
			db = a
		}
	}
	switch {
	case proto == "":
		return nil, refuseArgs("mysql", c.args, "the panel always names the protocol")
	case proto == "socket" && (host || port || stmt):
		return nil, refuseArgs("mysql", c.args, "over the socket the SQL comes on stdin")
	case proto == "tcp" && (!host || !port || !stmt || user || db != "" || c.mode != modeTool):
		// the only TCP use: one SphinxQL statement to the local searchd
		return nil, refuseArgs("mysql", c.args, "TCP only for a SphinxQL statement on 127.0.0.1:9306")
	}
	plan := &toolPlan{args: append(append([]string{}, mysqlForced...), c.args...)}
	if len(c.env) > 0 {
		for _, e := range c.env {
			k, v, _ := strings.Cut(e, "=")
			if k != "MYSQL_PWD" || strings.ContainsAny(v, "\n\x00") {
				return nil, notAllowed("the environment variable "+k+" for mysql", "only MYSQL_PWD (the password, kept off the command line)")
			}
		}
		plan.env = append([]string{}, c.env...)
	}
	return plan, nil
}

// planMySQLDump: a database dumped into the backup's directory
// (internal/api/ops_backup.go, with OutputFile) or streamed to the other
// server of a migration (ops_migrate.go). --result-file, --tab, --log-error
// (files written as root), --plugin-dir, --default-auth and --defaults-*
// are not among them.
func (s *Server) planMySQLDump(c *toolCall) (*toolPlan, error) {
	if c.mode == modeStreamIn {
		return nil, notAllowed("mysqldump through /v1/stream/in", "a dump is read, not written")
	}
	seen := map[string]bool{}
	databases := false
	names := 0
	for _, a := range c.args {
		switch a {
		case "--protocol=socket", "--single-transaction", "--routines", "--triggers", "--events", "--set-gtid-purged=OFF", "--databases":
			if seen[a] {
				return nil, notAllowed("mysqldump "+a+" twice", "each option once")
			}
			seen[a] = true
			databases = databases || a == "--databases"
		default:
			if strings.HasPrefix(a, "-") {
				return nil, notAllowed("the mysqldump option "+a, "the panel never passes it")
			}
			if !dbNameRe.MatchString(a) {
				return nil, notAllowed("mysqldump of "+a, "a database name is letters, digits and _")
			}
			names++
		}
	}
	if !databases || names == 0 || !seen["--protocol=socket"] {
		return nil, refuseArgs("mysqldump", c.args, "the panel dumps named databases over the socket (--protocol=socket … --databases <name>)")
	}
	plan := &toolPlan{args: c.args}
	if c.outputFile != "" {
		if c.mode != modeTool {
			return nil, notAllowed("an output file through "+c.mode.String(), "a stream has the response for its output")
		}
		f, err := s.openDumpFile(c.outputFile)
		if err != nil {
			return nil, err
		}
		plan.stdout = f
	}
	return plan, nil
}

var backupRunRe = regexp.MustCompile(`^run-[0-9]{1,19}$`)

// backupTmpDir is where a backup run keeps its dumps (internal/api/
// ops_backup.go jobBackupRun: <data_dir>/backups/tmp/run-<id>).
func (s *Server) backupTmpDir() string {
	return filepath.Join(s.cfg.DataDir, "backups", "tmp")
}

// openDumpFile opens <data_dir>/backups/tmp/run-<id>/<database>.sql for
// mysqldump's output. The directories belong to the API process, which could
// leave a symlink on the way or a second name of another file there: they
// are opened one by one without following a symlink, the file so too, and
// it is truncated only as a plain file with one name.
func (s *Server) openDumpFile(p string) (*os.File, error) {
	dir, base := filepath.Dir(p), filepath.Base(p)
	if !cleanAbs(p) || filepath.Dir(dir) != s.backupTmpDir() || !backupRunRe.MatchString(filepath.Base(dir)) ||
		!strings.HasSuffix(base, ".sql") || !dbNameRe.MatchString(strings.TrimSuffix(base, ".sql")) {
		return nil, notAllowed("the output file "+p, "dumps go to "+s.backupTmpDir()+"/run-<id>/<database>.sql")
	}
	dfd, err := openDirBelow(s.cfg.DataDir, dir, false, 0)
	if err != nil {
		return nil, notAllowed("the output file "+p, err.Error())
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	fd, err := unix.Openat(dfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, notAllowed("the output file "+p, err.Error())
	}
	f := os.NewFile(uintptr(fd), p)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || hasOtherNames(st) {
		f.Close()
		return nil, notAllowed("the output file "+p, "it is not a plain file of its own")
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncate %s: %w", p, err)
	}
	return f, nil
}
