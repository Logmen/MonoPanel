package api

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

// grantsMySQL plays mysql.db for refreshLiteralGrants: rows by host, user
// and Db with their privileges, and the statements the step sends, in the
// way MySQL takes them (UPDATE IGNORE keeps a row whose new key is taken).
type grantsMySQL struct {
	mu      sync.Mutex
	rows    map[[3]string]string // host, user, Db → privileges
	batches []string
	fail    bool
}

var (
	grantsLit    = `'((?:[^'\\]|\\.)*)'`
	grantsWhere  = `Host = ` + grantsLit + ` AND User = ` + grantsLit + ` AND Db = ` + grantsLit
	grantsUpdate = regexp.MustCompile(`^UPDATE IGNORE mysql\.db SET Db = ` + grantsLit + ` WHERE ` + grantsWhere + `;$`)
	grantsDelete = regexp.MustCompile(`^DELETE FROM mysql\.db WHERE ` + grantsWhere + `;$`)
	grantsCount  = regexp.MustCompile(`^SELECT COUNT\(\*\) FROM mysql\.db WHERE \(Host, User, Db\) IN \((.*)\);$`)
	grantsTuple  = regexp.MustCompile(`\(` + grantsLit + `,` + grantsLit + `,` + grantsLit + `\)`)
)

func sqlUnquote(s string) string { return strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(s) }

func (m *grantsMySQL) hook(req agent.ToolRequest) *agent.ToolResponse {
	if req.Name != "mysql" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(m.batches, req.Stdin)
	if m.fail {
		return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 2002 (HY000): Can't connect to local MySQL server through socket"}
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(req.Stdin), "\n") {
		switch {
		case line == "FLUSH PRIVILEGES;", line == "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';":
		case grantsCount.MatchString(line):
			n := 0
			for _, t := range grantsTuple.FindAllStringSubmatch(grantsCount.FindStringSubmatch(line)[1], -1) {
				if _, ok := m.rows[[3]string{sqlUnquote(t[1]), sqlUnquote(t[2]), sqlUnquote(t[3])}]; ok {
					n++
				}
			}
			out = append(out, strconv.Itoa(n))
		case grantsUpdate.MatchString(line):
			g := grantsUpdate.FindStringSubmatch(line)
			from := [3]string{sqlUnquote(g[2]), sqlUnquote(g[3]), sqlUnquote(g[4])}
			to := [3]string{from[0], from[1], sqlUnquote(g[1])}
			if p, ok := m.rows[from]; ok {
				if _, taken := m.rows[to]; !taken {
					delete(m.rows, from)
					m.rows[to] = p
				}
			}
		case grantsDelete.MatchString(line):
			g := grantsDelete.FindStringSubmatch(line)
			delete(m.rows, [3]string{sqlUnquote(g[1]), sqlUnquote(g[2]), sqlUnquote(g[3])})
		default:
			return &agent.ToolResponse{ExitCode: 1, Output: "ERROR 1064 (42000): the fake does not know " + line}
		}
	}
	return &agent.ToolResponse{Output: strings.Join(out, "\n")}
}

func (m *grantsMySQL) state() (map[[3]string]string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.rows), len(m.batches)
}

// Grants an older panel made with the name as a pattern (_ and % are
// wildcards: my_ql opened `mysql`) name their database literally after the
// upgrade, once: each recorded account of a database keeps its privileges
// on the escaped name, an escaped row already there wins, nothing else of
// mysql.db changes, and a failure is tried again at the next start.
func TestOlderPatternGrantsBecomeLiteralOnce(t *testing.T) {
	f := newSiteFixture(t)
	f.installDB()
	my := f.userWithPassword("my", "my-password-1", 1502)
	carl := f.userWithPassword("carl", "carl-password-1", 1503)
	record := func(owner *store.User, name string, users ...[2]string) {
		t.Helper()
		d := &store.Database{UserID: owner.ID, Name: name}
		if err := f.db.CreateDatabase(f.ctx, d); err != nil {
			t.Fatal(err)
		}
		for _, u := range users {
			if err := f.db.CreateDBUser(f.ctx, &store.DBUser{UserID: owner.ID, DatabaseID: &d.ID, Name: u[0], Host: u[1]}); err != nil {
				t.Fatal(err)
			}
		}
	}
	record(my, "my_ql", [2]string{"my_ql", "localhost"})
	record(carl, "carl_shop", [2]string{"carl_shop", "localhost"}, [2]string{"carl_ro", "127.0.0.1"})
	record(carl, "carl_blog", [2]string{"carl_blog", "localhost"})
	record(carl, "carl_roundcube") // the webmail's: the record without its account row
	record(carl, "plain", [2]string{"plain", "localhost"})
	fake := &grantsMySQL{rows: map[[3]string]string{
		{"localhost", "my_ql", "my_ql"}:                   "ALL",
		{"localhost", "carl_shop", "carl_shop"}:           "ALL",
		{"127.0.0.1", "carl_ro", "carl_shop"}:             "SELECT",
		{"localhost", "carl_blog", "carl_blog"}:           "SELECT,INSERT",
		{"localhost", "carl_blog", `carl\_blog`}:          "ALL", // made again by 0.9.1 meanwhile
		{"localhost", "carl_roundcube", "carl_roundcube"}: "ALL",
		{"localhost", "plain", "plain"}:                   "ALL",
		{"localhost", "other", "other_db"}:                "ALL", // no account of the panel's
	}}
	f.agent.ToolHook = fake.hook

	// MySQL does not answer: nothing is marked done.
	fake.fail = true
	f.s.refreshLiteralGrants(f.ctx)
	if v, _ := f.db.GetSetting(f.ctx, settingDBGrantsLiteral); v != "" {
		t.Fatalf("the step failed, but the flag is %q", v)
	}
	fake.fail = false

	f.s.refreshLiteralGrants(f.ctx)
	got, batches := fake.state()
	want := map[[3]string]string{
		{"localhost", "my_ql", `my\_ql`}:                   "ALL",
		{"localhost", "carl_shop", `carl\_shop`}:           "ALL",
		{"127.0.0.1", "carl_ro", `carl\_shop`}:             "SELECT",
		{"localhost", "carl_blog", `carl\_blog`}:           "ALL",
		{"localhost", "carl_roundcube", `carl\_roundcube`}: "ALL",
		{"localhost", "plain", "plain"}:                    "ALL",
		{"localhost", "other", "other_db"}:                 "ALL",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("mysql.db after the step:\n got %v\nwant %v", got, want)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingDBGrantsLiteral); v != "1" {
		t.Fatalf("flag %q after the step", v)
	}
	last := fake.batches[len(fake.batches)-1]
	if !strings.HasSuffix(last, "FLUSH PRIVILEGES;\n") {
		t.Fatalf("the server does not read the grant tables again:\n%s", last)
	}

	// Once: the next start asks MySQL nothing.
	f.s.refreshLiteralGrants(f.ctx)
	if _, again := fake.state(); again != batches {
		t.Fatalf("the step ran again: %d batches, then %d", batches, again)
	}
}
