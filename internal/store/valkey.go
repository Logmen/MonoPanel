package store

import (
	"context"
	"database/sql"
	"errors"
)

// Purposes of a Valkey instance: an account has at most one of each.
const (
	ValkeyCache    = "cache"
	ValkeySessions = "sessions"
)

// SessionStoreValkey keeps a site's PHP sessions in the account's sessions instance.
const SessionStoreValkey = "valkey"

// ValkeyInstance is a Valkey (or Redis) server of one account, reachable only
// through a unix socket that belongs to the account: the cache instance keeps
// nothing on disk and evicts any key, the sessions one saves snapshots and
// evicts only keys with a TTL.
type ValkeyInstance struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Login     string `json:"login,omitempty"`
	Purpose   string `json:"purpose"`
	MemoryMB  int    `json:"memory_mb"`
	Status    string `json:"status,omitempty"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt Time   `json:"created_at"`
	UpdatedAt Time   `json:"updated_at"`
}

// Name is <login>-<purpose>: the unit, the config and the directories carry it.
func (v *ValkeyInstance) Name() string { return v.Login + "-" + v.Purpose }

// Unit is the systemd unit name of the instance.
func (v *ValkeyInstance) Unit() string { return "monopanel-valkey-" + v.Name() + ".service" }

const valkeyCols = `v.id, v.user_id, u.login, v.purpose, v.memory_mb, v.status, v.last_error, v.created_at, v.updated_at`
const valkeyFrom = ` FROM valkey_instances v JOIN users u ON u.id = v.user_id`

func scanValkey(sc scanner) (*ValkeyInstance, error) {
	var v ValkeyInstance
	var created, updated string
	if err := sc.Scan(&v.ID, &v.UserID, &v.Login, &v.Purpose, &v.MemoryMB, &v.Status, &v.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	v.CreatedAt, v.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &v, nil
}

// CreateValkey inserts an instance.
func (d *DB) CreateValkey(ctx context.Context, v *ValkeyInstance) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO valkey_instances(user_id, purpose, memory_mb, status, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?) RETURNING id`,
		v.UserID, v.Purpose, v.MemoryMB, v.Status, v.LastError, ts, ts).Scan(&v.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	v.CreatedAt = Time(parseTime(ts))
	v.UpdatedAt = v.CreatedAt
	return nil
}

// UpdateValkey writes the memory limit.
func (d *DB) UpdateValkey(ctx context.Context, v *ValkeyInstance) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE valkey_instances SET memory_mb=?, updated_at=? WHERE id=?`, v.MemoryMB, now(), v.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetValkeyStatus records the last observed state.
func (d *DB) SetValkeyStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE valkey_instances SET status=?, last_error=?, updated_at=? WHERE id=?`, status, lastError, now(), id)
	return err
}

// GetValkey returns the instance of a user for a purpose.
func (d *DB) GetValkey(ctx context.Context, userID int64, purpose string) (*ValkeyInstance, error) {
	return scanValkey(d.sql.QueryRowContext(ctx, `SELECT `+valkeyCols+valkeyFrom+` WHERE v.user_id=? AND v.purpose=?`, userID, purpose))
}

// ListValkey returns the instances of a user (0 = all).
func (d *DB) ListValkey(ctx context.Context, userID int64) ([]*ValkeyInstance, error) {
	var rows *sql.Rows
	var err error
	if userID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+valkeyCols+valkeyFrom+` WHERE v.user_id=? ORDER BY v.purpose`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+valkeyCols+valkeyFrom+` ORDER BY u.login, v.purpose`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ValkeyInstance{}
	for rows.Next() {
		v, err := scanValkey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteValkey removes the row.
func (d *DB) DeleteValkey(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM valkey_instances WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
