package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// APIToken is a Bearer credential; only the hash is stored.
type APIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Hash       string     `json:"-"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

const tokenCols = `id, user_id, name, hash, scopes, last_used_at, expires_at, created_at`

func scanToken(s scanner) (*APIToken, error) {
	var t APIToken
	var scopes, created string
	var last, exp sql.NullString
	if err := s.Scan(&t.ID, &t.UserID, &t.Name, &t.Hash, &scopes, &last, &exp, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(scopes), &t.Scopes)
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	t.LastUsedAt, t.ExpiresAt, t.CreatedAt = parseTimePtr(last), parseTimePtr(exp), parseTime(created)
	return &t, nil
}

// CreateAPIToken inserts a token record.
func (d *DB) CreateAPIToken(ctx context.Context, t *APIToken) error {
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	scopes, _ := json.Marshal(t.Scopes)
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO api_tokens(user_id, name, hash, scopes, expires_at, created_at) VALUES(?,?,?,?,?,?) RETURNING id`,
		t.UserID, t.Name, t.Hash, string(scopes), timePtrStr(t.ExpiresAt), ts).Scan(&t.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	t.CreatedAt = parseTime(ts)
	return nil
}

// GetAPITokenByHash returns a valid (unexpired) token.
func (d *DB) GetAPITokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	return scanToken(d.sql.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE hash=? AND (expires_at IS NULL OR expires_at > ?)`, hash, now()))
}

// TouchAPIToken records usage.
func (d *DB) TouchAPIToken(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, now(), id)
	return err
}

// ListAPITokens returns a user's tokens.
func (d *DB) ListAPITokens(ctx context.Context, userID int64) ([]*APIToken, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token owned by userID; userID 0 revokes any token
// and is reserved for administrators.
func (d *DB) DeleteAPIToken(ctx context.Context, id, userID int64) error {
	query, args := `DELETE FROM api_tokens WHERE id=? AND user_id=?`, []any{id, userID}
	if userID == 0 {
		query, args = `DELETE FROM api_tokens WHERE id=?`, []any{id}
	}
	res, err := d.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
