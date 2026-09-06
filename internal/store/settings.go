package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// GetSetting returns a raw setting value.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting upserts a setting.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ID      int64          `json:"id"`
	Time    time.Time      `json:"ts"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"`
	Target  string         `json:"target,omitempty"`
	IP      string         `json:"ip,omitempty"`
	Result  string         `json:"result"`
	Details map[string]any `json:"details,omitempty"`
}

// Audit appends an entry.
func (d *DB) Audit(ctx context.Context, e AuditEntry) error {
	if e.Result == "" {
		e.Result = "ok"
	}
	details := "{}"
	if e.Details != nil {
		b, _ := json.Marshal(e.Details)
		details = string(b)
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO audit_log(ts, actor, action, target, ip, result, details) VALUES(?,?,?,?,?,?,?)`,
		now(), e.Actor, e.Action, e.Target, e.IP, e.Result, details)
	return err
}

// ListAudit returns the newest entries.
func (d *DB) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT id, ts, actor, action, target, ip, result, details FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts, details string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.IP, &e.Result, &details); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}
