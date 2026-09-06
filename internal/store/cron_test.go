package store

import (
	"context"
	"testing"
	"time"
)

func TestCronFirewallMetrics(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	c := &CronJob{UserID: u.ID, Schedule: "*/5 * * * *", Command: "php cron.php", Enabled: true}
	if err := db.CreateCronJob(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Enabled = false
	db.UpdateCronJob(ctx, c)
	list, _ := db.ListCronJobs(ctx, u.ID)
	if len(list) != 1 || list[0].Enabled || list[0].Login != "alex" {
		t.Fatalf("cron: %+v", list)
	}
	db.DeleteCronJob(ctx, c.ID)
	if l, _ := db.ListCronJobs(ctx, 0); len(l) != 0 {
		t.Fatal("cron delete")
	}
	r := &FirewallRule{Kind: "allow", Proto: "tcp", Port: "2222", Enabled: true}
	db.CreateFirewallRule(ctx, r)
	db.CreateFirewallRule(ctx, &FirewallRule{Kind: "deny", Source: "203.0.113.0/24", Enabled: true})
	rules, _ := db.ListFirewallRules(ctx)
	if len(rules) != 2 || rules[0].Kind != "deny" {
		t.Fatalf("rules: %+v", rules)
	}
	now := time.Now().Unix() / 60 * 60
	for i := int64(0); i < 10; i++ {
		db.InsertMetric(ctx, &MetricPoint{TS: now - i*60, CPU: float64(i), Load1: 1, MemUsed: 100, MemTotal: 200, DiskUsed: 1, DiskTotal: 2, NetRx: 10, NetTx: 5})
	}
	pts, _ := db.QueryMetrics(ctx, now-600, 300)
	if len(pts) < 2 || pts[len(pts)-1].NetRx == 0 {
		t.Fatalf("metrics: %+v", pts)
	}
	if n, _ := db.PruneMetrics(ctx, now-300); n == 0 {
		t.Fatal("prune")
	}
}
