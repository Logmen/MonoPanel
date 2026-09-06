package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// App is a long-running program of a user (gunicorn, a Node server, a bot)
// managed as a systemd unit that runs under the user's unix account.
type App struct {
	ID          int64    `json:"id"`
	UserID      int64    `json:"user_id"`
	Login       string   `json:"login,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Command     string   `json:"command"`
	WorkDir     string   `json:"workdir"`
	EnvFile     string   `json:"env_file,omitempty"`
	Env         []string `json:"env"`
	Restart     string   `json:"restart"`
	Enabled     bool     `json:"enabled"`
	Status      string   `json:"status,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	CreatedAt   Time     `json:"created_at"`
	UpdatedAt   Time     `json:"updated_at"`
}

// Unit is the systemd unit name of the app.
func (a *App) Unit() string { return "monopanel-app-" + a.Login + "-" + a.Name + ".service" }

const appCols = `a.id, a.user_id, u.login, a.name, a.description, a.command, a.workdir, a.env_file, a.env, a.restart, a.enabled, a.status, a.last_error, a.created_at, a.updated_at`
const appFrom = ` FROM apps a JOIN users u ON u.id = a.user_id`

func scanApp(sc scanner) (*App, error) {
	var a App
	var env, created, updated string
	var enabled int
	if err := sc.Scan(&a.ID, &a.UserID, &a.Login, &a.Name, &a.Description, &a.Command, &a.WorkDir, &a.EnvFile, &env, &a.Restart, &enabled, &a.Status, &a.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(env), &a.Env)
	if a.Env == nil {
		a.Env = []string{}
	}
	a.Enabled = enabled != 0
	a.CreatedAt, a.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &a, nil
}

// CreateApp inserts an app.
func (d *DB) CreateApp(ctx context.Context, a *App) error {
	if a.Env == nil {
		a.Env = []string{}
	}
	if a.Restart == "" {
		a.Restart = "always"
	}
	env, _ := json.Marshal(a.Env)
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO apps(user_id, name, description, command, workdir, env_file, env, restart, enabled, status, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		a.UserID, a.Name, a.Description, a.Command, a.WorkDir, a.EnvFile, string(env), a.Restart, boolInt(a.Enabled), a.Status, a.LastError, ts, ts).Scan(&a.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	a.CreatedAt = Time(parseTime(ts))
	a.UpdatedAt = a.CreatedAt
	return nil
}

// UpdateApp writes every mutable field.
func (d *DB) UpdateApp(ctx context.Context, a *App) error {
	env, _ := json.Marshal(a.Env)
	res, err := d.sql.ExecContext(ctx, `UPDATE apps SET description=?, command=?, workdir=?, env_file=?, env=?, restart=?, enabled=?, status=?, last_error=?, updated_at=? WHERE id=?`,
		a.Description, a.Command, a.WorkDir, a.EnvFile, string(env), a.Restart, boolInt(a.Enabled), a.Status, a.LastError, now(), a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppStatus records the last observed state.
func (d *DB) SetAppStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE apps SET status=?, last_error=?, updated_at=? WHERE id=?`, status, lastError, now(), id)
	return err
}

// GetApp returns an app by id.
func (d *DB) GetApp(ctx context.Context, id int64) (*App, error) {
	return scanApp(d.sql.QueryRowContext(ctx, `SELECT `+appCols+appFrom+` WHERE a.id=?`, id))
}

// GetAppByName returns the app <name> of a user.
func (d *DB) GetAppByName(ctx context.Context, userID int64, name string) (*App, error) {
	return scanApp(d.sql.QueryRowContext(ctx, `SELECT `+appCols+appFrom+` WHERE a.user_id=? AND a.name=?`, userID, name))
}

// ListApps returns apps of a user (0 = all).
func (d *DB) ListApps(ctx context.Context, userID int64) ([]*App, error) {
	var rows *sql.Rows
	var err error
	if userID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+appCols+appFrom+` WHERE a.user_id=? ORDER BY a.name`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+appCols+appFrom+` ORDER BY u.login, a.name`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*App{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteApp removes the row.
func (d *DB) DeleteApp(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM apps WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
