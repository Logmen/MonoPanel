package store

import (
	"context"
	"database/sql"
	"errors"
)

// CronJob is one line of a user's crontab.
type CronJob struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Login      string `json:"login,omitempty"`
	Schedule   string `json:"schedule"`
	Command    string `json:"command"`
	Enabled    bool   `json:"enabled"`
	PHPVersion string `json:"php_version,omitempty"`
	Comment    string `json:"comment,omitempty"`
	CreatedAt  Time   `json:"created_at"`
	UpdatedAt  Time   `json:"updated_at"`
}

const cronCols = `c.id, c.user_id, u.login, c.schedule, c.command, c.enabled, c.php_version, c.comment, c.created_at, c.updated_at`

func scanCron(s scanner) (*CronJob, error) {
	var c CronJob
	var enabled int
	var created, updated string
	if err := s.Scan(&c.ID, &c.UserID, &c.Login, &c.Schedule, &c.Command, &enabled, &c.PHPVersion, &c.Comment, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Enabled = enabled != 0
	c.CreatedAt, c.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &c, nil
}

// CreateCronJob inserts a job.
func (d *DB) CreateCronJob(ctx context.Context, c *CronJob) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO cron_jobs(user_id, schedule, command, enabled, php_version, comment, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?) RETURNING id`,
		c.UserID, c.Schedule, c.Command, boolInt(c.Enabled), c.PHPVersion, c.Comment, ts, ts).Scan(&c.ID)
	if err == nil {
		c.CreatedAt = Time(parseTime(ts))
		c.UpdatedAt = c.CreatedAt
	}
	return err
}

// UpdateCronJob writes mutable fields.
func (d *DB) UpdateCronJob(ctx context.Context, c *CronJob) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE cron_jobs SET schedule=?, command=?, enabled=?, php_version=?, comment=?, updated_at=? WHERE id=?`,
		c.Schedule, c.Command, boolInt(c.Enabled), c.PHPVersion, c.Comment, now(), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetCronJob returns a job.
func (d *DB) GetCronJob(ctx context.Context, id int64) (*CronJob, error) {
	return scanCron(d.sql.QueryRowContext(ctx, `SELECT `+cronCols+` FROM cron_jobs c JOIN users u ON u.id=c.user_id WHERE c.id=?`, id))
}

// ListCronJobs returns jobs of a user (0 = all).
func (d *DB) ListCronJobs(ctx context.Context, userID int64) ([]*CronJob, error) {
	var rows *sql.Rows
	var err error
	if userID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+cronCols+` FROM cron_jobs c JOIN users u ON u.id=c.user_id WHERE c.user_id=? ORDER BY c.id`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+cronCols+` FROM cron_jobs c JOIN users u ON u.id=c.user_id ORDER BY u.login, c.id`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CronJob{}
	for rows.Next() {
		c, err := scanCron(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCronJob removes a job.
func (d *DB) DeleteCronJob(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FirewallRule is one allow/deny entry rendered into the nftables table.
type FirewallRule struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Proto     string `json:"proto"`
	Port      string `json:"port,omitempty"`
	Source    string `json:"source,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt Time   `json:"created_at"`
}

// CreateFirewallRule inserts a rule.
func (d *DB) CreateFirewallRule(ctx context.Context, r *FirewallRule) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO firewall_rules(kind, proto, port, source, comment, enabled, created_at) VALUES(?,?,?,?,?,?,?) RETURNING id`,
		r.Kind, r.Proto, r.Port, r.Source, r.Comment, boolInt(r.Enabled), ts).Scan(&r.ID)
	if err == nil {
		r.CreatedAt = Time(parseTime(ts))
	}
	return err
}

// ListFirewallRules returns all rules.
func (d *DB) ListFirewallRules(ctx context.Context) ([]*FirewallRule, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, kind, proto, port, source, comment, enabled, created_at FROM firewall_rules ORDER BY kind DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FirewallRule{}
	for rows.Next() {
		var r FirewallRule
		var enabled int
		var created string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Proto, &r.Port, &r.Source, &r.Comment, &enabled, &created); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		r.CreatedAt = Time(parseTime(created))
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteFirewallRule removes a rule.
func (d *DB) DeleteFirewallRule(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM firewall_rules WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MetricPoint is one minute of host metrics.
type MetricPoint struct {
	TS        int64   `json:"ts"`
	CPU       float64 `json:"cpu"`
	Load1     float64 `json:"load1"`
	MemUsed   int64   `json:"mem_used"`
	MemTotal  int64   `json:"mem_total"`
	DiskUsed  int64   `json:"disk_used"`
	DiskTotal int64   `json:"disk_total"`
	NetRx     int64   `json:"net_rx"`
	NetTx     int64   `json:"net_tx"`
}

// InsertMetric stores a minute sample.
func (d *DB) InsertMetric(ctx context.Context, m *MetricPoint) error {
	_, err := d.sql.ExecContext(ctx, `INSERT OR REPLACE INTO metrics(ts, cpu, load1, mem_used, mem_total, disk_used, disk_total, net_rx, net_tx) VALUES(?,?,?,?,?,?,?,?,?)`,
		m.TS, m.CPU, m.Load1, m.MemUsed, m.MemTotal, m.DiskUsed, m.DiskTotal, m.NetRx, m.NetTx)
	return err
}

// QueryMetrics returns points since `from`, averaged into buckets of `step` seconds.
func (d *DB) QueryMetrics(ctx context.Context, from int64, step int64) ([]*MetricPoint, error) {
	if step < 60 {
		step = 60
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT (ts/?)*?, AVG(cpu), AVG(load1), AVG(mem_used), MAX(mem_total), MAX(disk_used), MAX(disk_total), SUM(net_rx), SUM(net_tx)
		FROM metrics WHERE ts >= ? GROUP BY ts/? ORDER BY 1`, step, step, from, step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MetricPoint{}
	for rows.Next() {
		var m MetricPoint
		var memUsed float64
		if err := rows.Scan(&m.TS, &m.CPU, &m.Load1, &memUsed, &m.MemTotal, &m.DiskUsed, &m.DiskTotal, &m.NetRx, &m.NetTx); err != nil {
			return nil, err
		}
		m.MemUsed = int64(memUsed)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// PruneMetrics deletes samples older than `before`.
func (d *DB) PruneMetrics(ctx context.Context, before int64) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM metrics WHERE ts < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
