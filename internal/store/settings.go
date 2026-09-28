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

// SetAuditSink adds a second destination for every audit entry — the API
// mirrors the log to a file for people who read logs on disk.
func (d *DB) SetAuditSink(sink func(AuditEntry)) { d.auditSink = sink }

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
	ts := now()
	res, err := d.sql.ExecContext(ctx, `INSERT INTO audit_log(ts, actor, action, target, ip, result, details) VALUES(?,?,?,?,?,?,?)`,
		ts, e.Actor, e.Action, e.Target, e.IP, e.Result, details)
	if err == nil && d.auditSink != nil {
		e.ID, _ = res.LastInsertId()
		e.Time = parseTime(ts)
		d.auditSink(e)
	}
	return err
}

// AuditFilter narrows ListAudit: an empty field matches everything, Action
// matches a prefix ("auth." is every sign-in event).
type AuditFilter struct {
	Limit  int
	Actor  string
	Action string
}

// ListAudit returns the newest entries.
func (d *DB) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT id, ts, actor, action, target, ip, result, details FROM audit_log
		WHERE (? = '' OR actor = ?) AND (? = '' OR action LIKE ? || '%') ORDER BY id DESC LIMIT ?`, f.Actor, f.Actor, f.Action, f.Action, f.Limit)
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
