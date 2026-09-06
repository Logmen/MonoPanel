package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Job statuses.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Job is a unit of asynchronous work executed by the job runner.
type Job struct {
	ID             int64           `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"`
	Progress       int             `json:"progress"`
	Message        string          `json:"message"`
	Log            string          `json:"log,omitempty"`
	Error          string          `json:"error,omitempty"`
	RequestedBy    string          `json:"requested_by"`
	LockKey        string          `json:"lock_key,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Worker         string          `json:"worker,omitempty"`
	CreatedAt      Time            `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
}

const jobCols = `id, type, payload, status, progress, message, log, error, requested_by, lock_key, COALESCE(idempotency_key,''), worker, created_at, started_at, finished_at`

func scanJob(s scanner) (*Job, error) {
	var j Job
	var payload, created string
	var started, finished sql.NullString
	if err := s.Scan(&j.ID, &j.Type, &payload, &j.Status, &j.Progress, &j.Message, &j.Log, &j.Error, &j.RequestedBy, &j.LockKey, &j.IdempotencyKey, &j.Worker, &created, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	j.Payload = json.RawMessage(payload)
	j.CreatedAt = Time(parseTime(created))
	j.StartedAt, j.FinishedAt = parseTimePtr(started), parseTimePtr(finished)
	return &j, nil
}

// EnqueueJob inserts a queued job. With an idempotency key that already
// exists, the existing job is loaded into j instead.
func (d *DB) EnqueueJob(ctx context.Context, j *Job) error {
	if len(j.Payload) == 0 {
		j.Payload = json.RawMessage("{}")
	}
	if j.IdempotencyKey != "" {
		existing, err := scanJob(d.sql.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE idempotency_key=?`, j.IdempotencyKey))
		if err == nil {
			*j = *existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO jobs(type, payload, status, requested_by, lock_key, idempotency_key, created_at) VALUES(?,?,?,?,?,?,?) RETURNING id`,
		j.Type, string(j.Payload), JobQueued, j.RequestedBy, j.LockKey, nullStr(j.IdempotencyKey), ts).Scan(&j.ID)
	if err != nil {
		return err
	}
	j.Status = JobQueued
	j.CreatedAt = Time(parseTime(ts))
	return nil
}

// ClaimJob atomically takes the oldest queued job whose lock key is not held
// by a running job. It returns nil, nil when nothing is runnable.
func (d *DB) ClaimJob(ctx context.Context, worker string) (*Job, error) {
	j, err := scanJob(d.sql.QueryRowContext(ctx, `UPDATE jobs SET status=?, started_at=?, worker=? WHERE id = (
			SELECT j.id FROM jobs j
			WHERE j.status=? AND NOT EXISTS (SELECT 1 FROM jobs r WHERE r.status=? AND r.lock_key<>'' AND r.lock_key=j.lock_key)
			ORDER BY j.id LIMIT 1
		) RETURNING `+jobCols, JobRunning, now(), worker, JobQueued, JobRunning))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return j, err
}

// UpdateJobProgress sets progress and message.
func (d *DB) UpdateJobProgress(ctx context.Context, id int64, progress int, message string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE jobs SET progress=?, message=? WHERE id=?`, progress, message, id)
	return err
}

// AppendJobLog appends text to the job log.
func (d *DB) AppendJobLog(ctx context.Context, id int64, text string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE jobs SET log = log || ? WHERE id=?`, text, id)
	return err
}

// FinishJob marks a job done or failed.
func (d *DB) FinishJob(ctx context.Context, id int64, status, errMsg string) error {
	progress := 100
	if status != JobDone {
		progress = -1
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE jobs SET status=?, error=?, finished_at=?, progress=CASE WHEN ?>=0 THEN ? ELSE progress END WHERE id=?`, status, errMsg, now(), progress, progress, id)
	return err
}

// GetJob returns a job by id.
func (d *DB) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(d.sql.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

// ListJobs returns recent jobs (newest first), optionally filtered by status.
func (d *DB) ListJobs(ctx context.Context, limit int, status string) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status=? ORDER BY id DESC LIMIT ?`, status, limit)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		j.Log = ""
		out = append(out, j)
	}
	return out, rows.Err()
}

// RequeueStaleJobs returns jobs left "running" by a previous process to the queue.
func (d *DB) RequeueStaleJobs(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `UPDATE jobs SET status=?, worker='', log = log || ? WHERE status=?`, JobQueued, "[runner] requeued after restart\n", JobRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountJobs returns counts per status.
func (d *DB) CountJobs(ctx context.Context) (map[string]int, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}
