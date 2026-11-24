package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"monopanel/internal/store"
)

// settingDBGrantsLiteral records that the database grants of an older panel
// were made literal (refreshLiteralGrants); set, the step does not run again.
const settingDBGrantsLiteral = "db.grants_literal"

// grantRowEscaper is a database name as mysql.db keeps the Db of a grant
// made ON grantDB(name): _ and % escaped, so that the row matches the name
// alone.
var grantRowEscaper = strings.NewReplacer(`_`, `\_`, `%`, `\%`)

// refreshLiteralGrants rewrites, once after an upgrade, the grants an older
// panel made with the database's name as a pattern. Before 0.9.1
// createDatabase and the webmail granted ON `<name>`.* unescaped, and in a
// database-level grant _ and % are wildcards: the login "my" with the
// database "ql" made my_ql, whose grant also opened `mysql`. For every
// database the panel records with _ or % in its name, the mysql.db row of
// each of its accounts (those recorded, and the one of the database's own
// name that createDatabase and the webmail make) moves to the escaped name
// with its privileges as they are; where an escaped row is there already it
// wins and the pattern row goes. The panel installs MySQL Community or
// Percona Server 8.x, whose grant tables take the UPDATE; FLUSH PRIVILEGES
// has the server read them again. A failure leaves the flag unset: the next
// start tries again (the statements change nothing a second time).
func (s *Server) refreshLiteralGrants(ctx context.Context) {
	if v, _ := s.db.GetSetting(ctx, settingDBGrantsLiteral); v != "" {
		return
	}
	inst, err := s.db.GetDBInstance(ctx)
	if errors.Is(err, store.ErrNotFound) {
		// No database server: every grant made from now on is literal.
		s.db.SetSetting(ctx, settingDBGrantsLiteral, "1") //nolint:errcheck // retried at the next start
		return
	}
	if err != nil || inst.Status != store.DBReady {
		return
	}
	dbs, err := s.db.ListDatabases(ctx, 0)
	if err != nil {
		s.log.Warn("database grants: cannot list the databases to make their grants literal; the next start tries again", "err", err)
		return
	}
	type row struct{ host, user, raw string }
	var rows []row
	for _, d := range dbs {
		if !strings.ContainsAny(d.Name, "_%") || strings.ContainsAny(d.Name, "\\'`") {
			continue
		}
		seen := map[[2]string]bool{}
		accounts := append([]*store.DBUser{{Name: d.Name, Host: "localhost"}}, d.Users...)
		for _, u := range accounts {
			k := [2]string{u.Host, u.Name}
			if seen[k] || u.Name == "" || u.Host == "" {
				continue
			}
			seen[k] = true
			rows = append(rows, row{u.Host, u.Name, d.Name})
		}
	}
	if len(rows) == 0 {
		s.db.SetSetting(ctx, settingDBGrantsLiteral, "1") //nolint:errcheck // retried at the next start
		return
	}
	var b strings.Builder
	// The literals below escape with backslashes: whatever sql_mode the
	// administrator set globally, this session reads them so.
	b.WriteString("SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';\n")
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, fmt.Sprintf("('%s','%s','%s')", sqlEscaper.Replace(r.host), sqlEscaper.Replace(r.user), sqlEscaper.Replace(r.raw)))
	}
	fmt.Fprintf(&b, "SELECT COUNT(*) FROM mysql.db WHERE (Host, User, Db) IN (%s);\n", strings.Join(keys, ", "))
	for _, r := range rows {
		where := fmt.Sprintf("Host = '%s' AND User = '%s' AND Db = '%s'", sqlEscaper.Replace(r.host), sqlEscaper.Replace(r.user), sqlEscaper.Replace(r.raw))
		// IGNORE: where the escaped row is there already, the pattern row
		// stays as it is and the DELETE takes it; otherwise it is renamed
		// and the DELETE finds nothing.
		fmt.Fprintf(&b, "UPDATE IGNORE mysql.db SET Db = '%s' WHERE %s;\n", sqlEscaper.Replace(grantRowEscaper.Replace(r.raw)), where)
		fmt.Fprintf(&b, "DELETE FROM mysql.db WHERE %s;\n", where)
	}
	b.WriteString("FLUSH PRIVILEGES;\n")
	out, err := s.mysqlExec(ctx, b.String())
	if err != nil {
		s.log.Warn("database grants: cannot make the grants of older databases literal; the next start tries again", "err", err)
		return
	}
	n, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]))
	if err := s.db.SetSetting(ctx, settingDBGrantsLiteral, "1"); err != nil {
		s.log.Warn("database grants: made literal, but the flag was not saved; the next start looks again", "err", err)
	}
	s.log.Info("database grants: the grants of older databases name their database literally", "rewritten", n, "accounts", len(rows))
}
