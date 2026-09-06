package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BackupTarget is a restic repository with credentials (encrypted at rest).
type BackupTarget struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Repository  string     `json:"repository"`
	PasswordEnc string     `json:"-"`
	EnvEnc      string     `json:"-"`
	KeepDaily   int        `json:"keep_daily"`
	KeepWeekly  int        `json:"keep_weekly"`
	KeepMonthly int        `json:"keep_monthly"`
	Schedule    string     `json:"schedule"`
	Enabled     bool       `json:"enabled"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	LastStatus  string     `json:"last_status,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   Time       `json:"created_at"`
	UpdatedAt   Time       `json:"updated_at"`
}

// Backup is one run.
type Backup struct {
	ID         int64      `json:"id"`
	TargetID   int64      `json:"target_id"`
	Scope      string     `json:"scope"`
	SnapshotID string     `json:"snapshot_id,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	Files      int64      `json:"files"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  Time       `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

const targetCols = `id, name, type, repository, password_enc, env_enc, keep_daily, keep_weekly, keep_monthly, schedule, enabled, last_run_at, last_status, last_error, created_at, updated_at`

func scanTarget(s scanner) (*BackupTarget, error) {
	var t BackupTarget
	var enabled int
	var last sql.NullString
	var created, updated string
	if err := s.Scan(&t.ID, &t.Name, &t.Type, &t.Repository, &t.PasswordEnc, &t.EnvEnc, &t.KeepDaily, &t.KeepWeekly, &t.KeepMonthly, &t.Schedule, &enabled, &last, &t.LastStatus, &t.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.Enabled = enabled != 0
	t.LastRunAt = parseTimePtr(last)
	t.CreatedAt, t.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &t, nil
}

// CreateBackupTarget inserts a target.
func (d *DB) CreateBackupTarget(ctx context.Context, t *BackupTarget) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO backup_targets(name, type, repository, password_enc, env_enc, keep_daily, keep_weekly, keep_monthly, schedule, enabled, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		t.Name, t.Type, t.Repository, t.PasswordEnc, t.EnvEnc, t.KeepDaily, t.KeepWeekly, t.KeepMonthly, t.Schedule, boolInt(t.Enabled), ts, ts).Scan(&t.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	t.CreatedAt = Time(parseTime(ts))
	t.UpdatedAt = t.CreatedAt
	return nil
}

// GetBackupTarget returns a target by id or name.
func (d *DB) GetBackupTarget(ctx context.Context, ref string) (*BackupTarget, error) {
	return scanTarget(d.sql.QueryRowContext(ctx, `SELECT `+targetCols+` FROM backup_targets WHERE name=? OR CAST(id AS TEXT)=?`, ref, ref))
}

// ListBackupTargets returns all targets.
func (d *DB) ListBackupTargets(ctx context.Context) ([]*BackupTarget, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+targetCols+` FROM backup_targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BackupTarget{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetBackupTargetRun records the last run outcome.
func (d *DB) SetBackupTargetRun(ctx context.Context, id int64, status, errMsg string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE backup_targets SET last_run_at=?, last_status=?, last_error=?, updated_at=? WHERE id=?`, now(), status, errMsg, now(), id)
	return err
}

// DeleteBackupTarget removes a target and its runs.
func (d *DB) DeleteBackupTarget(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM backup_targets WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateBackup starts a run record.
func (d *DB) CreateBackup(ctx context.Context, b *Backup) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO backups(target_id, scope, status, started_at) VALUES(?,?,?,?) RETURNING id`, b.TargetID, b.Scope, "running", ts).Scan(&b.ID)
	if err == nil {
		b.Status, b.StartedAt = "running", Time(parseTime(ts))
	}
	return err
}

// FinishBackup records the outcome.
func (d *DB) FinishBackup(ctx context.Context, id int64, status, snapshot string, size, files int64, errMsg string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE backups SET status=?, snapshot_id=?, size_bytes=?, files=?, error=?, finished_at=? WHERE id=?`, status, snapshot, size, files, errMsg, now(), id)
	return err
}

// ListBackups returns runs (newest first), optionally for one target.
func (d *DB) ListBackups(ctx context.Context, targetID int64, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if targetID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT id, target_id, scope, snapshot_id, size_bytes, files, status, error, started_at, finished_at FROM backups WHERE target_id=? ORDER BY id DESC LIMIT ?`, targetID, limit)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT id, target_id, scope, snapshot_id, size_bytes, files, status, error, started_at, finished_at FROM backups ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		var b Backup
		var started string
		var finished sql.NullString
		if err := rows.Scan(&b.ID, &b.TargetID, &b.Scope, &b.SnapshotID, &b.SizeBytes, &b.Files, &b.Status, &b.Error, &started, &finished); err != nil {
			return nil, err
		}
		b.StartedAt = Time(parseTime(started))
		b.FinishedAt = parseTimePtr(finished)
		out = append(out, &b)
	}
	return out, rows.Err()
}
