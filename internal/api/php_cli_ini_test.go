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
}
