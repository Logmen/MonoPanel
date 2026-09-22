package api

import (
	"net/http"
	"testing"

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
