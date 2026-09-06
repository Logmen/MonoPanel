package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a browser login.
type Session struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
	IP        string
	UA        string
	CreatedAt time.Time
}

// CreateSession inserts a session.
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	ts := now()
	_, err := d.sql.ExecContext(ctx, `INSERT INTO sessions(id, user_id, expires_at, ip, ua, created_at) VALUES(?,?,?,?,?,?)`,
		s.ID, s.UserID, s.ExpiresAt.UTC().Format(timeFormat), s.IP, s.UA, ts)
	if err == nil {
		s.CreatedAt = parseTime(ts)
	}
	return err
}

// GetSession returns a live (unexpired) session.
func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	var exp, created string
	err := d.sql.QueryRowContext(ctx, `SELECT id, user_id, expires_at, ip, ua, created_at FROM sessions WHERE id=? AND expires_at > ?`, id, now()).
		Scan(&s.ID, &s.UserID, &exp, &s.IP, &s.UA, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.ExpiresAt, s.CreatedAt = parseTime(exp), parseTime(created)
	return &s, nil
}

// DeleteSession removes a session (logout).
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}

// DeleteExpiredSessions purges old sessions.
func (d *DB) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
