package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func (f *siteFixture) doctorCheck(name string) string {
	for _, c := range f.s.doctor(f.ctx).Checks {
		if c.Name == name {
			return c.Status + ": " + c.Detail
		}
	}
	return ""
}

// The server-wide layer reaches PHP from the command line too: a timezone set
// with mp php ini set used to land in the pools only, so cron — Bitrix agents
// included — kept running in UTC, and the doctor saw nothing.
func TestGlobalLayerReachesCLI(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	const cli, fpm = "/etc/php/8.4/cli/conf.d/99-monopanel.ini", "/etc/php/8.4/fpm/conf.d/99-monopanel.ini"

	// What an older panel left: the CLI copy without the server-wide layer.
	if err := f.db.SetSetting(f.ctx, settingPHPGlobal, `{"date.timezone":"Asia/Almaty","max_input_vars":"10000"}`); err != nil {
		t.Fatal(err)
	}
	if got := f.doctorCheck("php cli"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "date.timezone UTC instead of Asia/Almaty") {
		t.Errorf("doctor on a stale CLI copy: %q", got)
	}
	// The panel's start brings it up to date.
	f.s.refreshPHPCLIInis(f.ctx)
	ini, _ := f.agent.File(cli)
	if !strings.Contains(ini, "date.timezone = Asia/Almaty\n") || !strings.Contains(ini, "max_input_vars = 10000\n") {
		t.Fatalf("CLI copy after start:\n%s", ini)
	}
	if got := f.doctorCheck("php cli"); !strings.HasPrefix(got, "ok: ") {
		t.Errorf("doctor after the refresh: %q", got)
	}

	// mp php ini set writes both copies at once, before the sites re-apply.
	var res apitypes.GlobalPHPResult
	f.call(http.MethodPut, "/php/settings", map[string]any{"php_ini": map[string]string{"date.timezone": "Europe/Berlin", "opcache.memory_consumption": "256"}}, http.StatusOK, &res)
	for _, id := range res.Jobs {
		f.waitJob(id)
	}
	for _, p := range []string{cli, fpm} {
		ini, _ := f.agent.File(p)
		if !strings.Contains(ini, "date.timezone = Europe/Berlin\n") || !strings.Contains(ini, "opcache.memory_consumption = 256\n") || strings.Count(ini, "date.timezone") != 1 {
			t.Errorf("%s:\n%s", p, ini)
		}
	}
	if f.agent.UnitAction("php8.4-fpm.service") == "" {
		t.Error("FPM not reloaded after its ini changed")
	}
}

// On Remi CLI and FPM share one php.d. Short tags opened there for 1C-Bitrix's
// command line reach every pool of the branch that does not close them
// itself, so the panel's start leaves that one change to a site apply — and
// still writes the rest, the timezone among it.
func TestRemiSharedIniKeepsShortTagsForSiteApply(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.8\n")
	f := newSiteFixture(t)
	const shared = "/etc/opt/remi/php84/php.d/99-monopanel.ini"
	if err := f.db.SetSetting(f.ctx, settingPHPGlobal, `{"date.timezone":"Asia/Almaty"}`); err != nil {
		t.Fatal(err)
	}
	f.s.refreshPHPCLIInis(f.ctx)
	if ini, _ := f.agent.File(shared); !strings.Contains(ini, "date.timezone = Asia/Almaty\n") || !strings.Contains(ini, "short_open_tag = Off\n") {
		t.Fatalf("shared php.d after start:\n%s", ini)
	}

	// A Bitrix site arrives without its apply having run yet (a migration
	// mid-way): the start must not open short tags for the whole branch.
	owner, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: owner.ID, Domain: "bx.example.com", PHPVersion: "8.4", Mode: store.ModeFPM, Preset: presetBitrix, SSL: "none", Status: store.SiteActive}); err != nil {
		t.Fatal(err)
	}
	f.s.refreshPHPCLIInis(f.ctx)
	if ini, _ := f.agent.File(shared); !strings.Contains(ini, "short_open_tag = Off\n") {
		t.Errorf("the start opened short tags in the shared php.d:\n%s", ini)
	}
	// The doctor says what brings the file in line: on Remi the start does
	// not, a site apply does.
	if got := f.doctorCheck("php cli"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "short_open_tag Off instead of On") || !strings.Contains(got, "PHP 8.4: FPM reads the same file, so short_open_tag there changes only with mp site apply <domain>") {
		t.Errorf("doctor while the change waits for a site apply: %q", got)
	}
}

// A branch no site's pool runs on has nothing to wait for, and no site apply
// would ever write its shared php.d: the Bitrix site gone, short tags close
// at the panel's next start, and the doctor's warning clears. It used to stay
// for good.
func TestRemiSharedIniWithoutPoolsIsWrittenAtOnce(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.8\n")
	f := newSiteFixture(t)
	const shared = "/etc/opt/remi/php84/php.d/99-monopanel.ini"
	owner, _ := f.db.GetUserByLogin(f.ctx, "alex")
	bx := &store.Site{UserID: owner.ID, Domain: "bx.example.com", PHPVersion: "8.4", Mode: store.ModeFPM, Preset: presetBitrix, SSL: "none", Status: store.SiteActive}
	if err := f.db.CreateSite(f.ctx, bx); err != nil {
		t.Fatal(err)
	}
	f.s.refreshPHPCLIInis(f.ctx)
	if ini, _ := f.agent.File(shared); !strings.Contains(ini, "short_open_tag = On\n") {
		t.Fatalf("a Bitrix site on the branch, a fresh file:\n%s", ini)
	}
	// The site goes; a proxy site on the branch runs no pool.
	if err := f.db.DeleteSite(f.ctx, bx.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateSite(f.ctx, &store.Site{UserID: owner.ID, Domain: "app.example.com", PHPVersion: "8.4", Mode: store.ModeProxy, Backend: "http://127.0.0.1:3000", SSL: "none", Status: store.SiteActive}); err != nil {
		t.Fatal(err)
	}
	if got := f.doctorCheck("php cli"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "short_open_tag On instead of Off") || strings.Contains(got, "FPM reads the same file") {
		t.Errorf("doctor before the start: %q", got)
	}
	f.s.refreshPHPCLIInis(f.ctx)
	if ini, _ := f.agent.File(shared); !strings.Contains(ini, "short_open_tag = Off\n") {
		t.Errorf("no pool on the branch, the start left short tags open:\n%s", ini)
	}
	if got := f.doctorCheck("php cli"); !strings.HasPrefix(got, "ok: ") {
		t.Errorf("doctor after the start: %q", got)
	}
}
