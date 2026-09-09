package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// Перенос — это две панели: одна отдаёт, другая принимает. Обе поднимаются
// фикстурой с собственным фейковым агентом, между ними ходит настоящий HTTP.
func TestMigrationMovesAccountBetweenPanels(t *testing.T) {
	source := newSiteFixture(t)
	source.agent.Shadow["alex"] = "$y$j9T$sourcehash"
	owner, err := source.db.GetUserByLogin(source.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("secret-password")
	if err := source.db.SetUserPassword(source.ctx, owner.ID, hash); err != nil {
		t.Fatal(err)
	}
	site := source.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	if err := source.db.CreateCronJob(source.ctx, &store.CronJob{UserID: site.UserID, Schedule: "*/5 * * * *", Command: "php cron.php", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Источник выдаёт токен на один аккаунт.
	var grant struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}
	source.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:alex", "hours": 1}, http.StatusCreated, &grant)
	if grant.Token == "" || grant.Scope != "user:alex" {
		t.Fatalf("токен не выдан: %+v", grant)
	}

	// Этим токеном можно забрать только переезд и только этот аккаунт.
	for _, c := range []struct {
		path string
		want int
	}{
		{"/migrate/plan?scope=user:alex", http.StatusOK},
		{"/migrate/plan?scope=user:admin", http.StatusForbidden},
		{"/users", http.StatusForbidden},
	} {
		req, _ := http.NewRequest(http.MethodGet, source.ts.URL+"/api/v1"+c.path, nil)
		req.Header.Set("Authorization", "Bearer "+grant.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("токеном переезда %s → %d, ожидалось %d", c.path, res.StatusCode, c.want)
		}
	}

	target := newSiteFixture(t)
	body := map[string]any{"source": source.ts.URL, "token": grant.Token, "scope": "user:alex", "as": "alex2", "insecure": true}

	var plan struct {
		Login     string `json:"login"`
		OK        bool   `json:"ok"`
		Conflicts []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"conflicts"`
		Bundle struct {
			Sites []struct {
				Domain string `json:"domain"`
			} `json:"sites"`
			Secrets any `json:"secrets"`
		} `json:"bundle"`
	}
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	if !plan.OK {
		t.Fatalf("разбор нашёл препятствия: %+v", plan.Conflicts)
	}
	if plan.Login != "alex2" || len(plan.Bundle.Sites) != 1 || plan.Bundle.Sites[0].Domain != "shop.example.com" {
		t.Fatalf("разбор: %+v", plan)
	}
	if plan.Bundle.Secrets != nil {
		t.Error("предварительный разбор не должен тянуть хеши паролей")
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	target.call(http.MethodPost, "/migrate/run", body, http.StatusAccepted, &ref)
	if job := target.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("перенос: %s %s", job.Status, job.Error)
	}

	u, err := target.db.GetUserByLogin(target.ctx, "alex2")
	if err != nil {
		t.Fatalf("аккаунт не приехал: %v", err)
	}
	if u.Status != store.UserActive {
		t.Errorf("статус аккаунта: %s", u.Status)
	}
	src, _ := source.db.GetUserByLogin(source.ctx, "alex")
	if u.PasswordHash != src.PasswordHash || u.PasswordHash == "" {
		t.Error("пароль панели должен переехать хешем")
	}
	moved, err := target.db.GetSiteByDomain(target.ctx, "shop.example.com")
	if err != nil {
		t.Fatalf("сайт не приехал: %v", err)
	}
	if moved.PHPVersion != "8.4" || moved.UserID != u.ID {
		t.Errorf("сайт приехал не тем: %+v", moved)
	}
	cron, _ := target.db.ListCronJobs(target.ctx, u.ID)
	if len(cron) != 1 || cron[0].Command != "php cron.php" {
		t.Errorf("cron не приехал: %+v", cron)
	}

	// Файлы должны идти потоком через агента, а не складываться на диск.
	var tarOut, tarIn bool
	for _, st := range target.agent.Streams() {
		if st.Direction == "in" && st.Name == "tar" {
			tarIn = true
		}
	}
	for _, st := range source.agent.Streams() {
		if st.Direction == "out" && st.Name == "tar" {
			tarOut = true
			if !strings.Contains(strings.Join(st.Args, " "), "--exclude=./data/logs") {
				t.Errorf("логи старого сервера не исключены: %v", st.Args)
			}
		}
	}
	if !tarOut || !tarIn {
		t.Errorf("потоков tar не было: источник=%v приёмник=%v", tarOut, tarIn)
	}

	// Повторный перенос под тем же логином должен упереться в конфликты.
	var second struct {
		OK        bool `json:"ok"`
		Conflicts []struct {
			Kind string `json:"kind"`
		} `json:"conflicts"`
	}
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &second)
	if second.OK {
		t.Error("второй разбор обязан заметить, что аккаунт и сайт уже здесь")
	}
	kinds := map[string]bool{}
	for _, c := range second.Conflicts {
		kinds[c.Kind] = true
	}
	if !kinds["user"] || !kinds["site"] {
		t.Errorf("конфликты: %+v", second.Conflicts)
	}
}
