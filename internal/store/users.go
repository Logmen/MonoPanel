package store

import (
	"context"
	"database/sql"
	"errors"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User statuses.
const (
	UserActive    = "active"
	UserSuspended = "suspended"
	UserPending   = "pending"
	UserDeleting  = "deleting"
)

// User is a panel account; role=user accounts also own a unix user.
type User struct {
	ID           int64  `json:"id"`
	Login        string `json:"login"`
	Role         string `json:"role"`
	PasswordHash string `json:"-"`
	Email        string `json:"email,omitempty"`
	UnixUID      *int   `json:"unix_uid,omitempty"`
	UnixGID      *int   `json:"unix_gid,omitempty"`
	Home         string `json:"home,omitempty"`
	Shell        bool   `json:"shell"`
	Status       string `json:"status"`
	QuotaMB      *int   `json:"quota_mb,omitempty"`
	CreatedAt    Time   `json:"created_at"`
	UpdatedAt    Time   `json:"updated_at"`
}

const userCols = `id, login, role, COALESCE(password_hash,''), COALESCE(email,''), unix_uid, unix_gid, COALESCE(home,''), shell, status, quota_mb, created_at, updated_at`

func scanUser(s scanner) (*User, error) {
	var u User
	var uid, gid, quota sql.NullInt64
	var created, updated string
	var shell int
	if err := s.Scan(&u.ID, &u.Login, &u.Role, &u.PasswordHash, &u.Email, &uid, &gid, &u.Home, &shell, &u.Status, &quota, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Shell = shell != 0
	u.UnixUID, u.UnixGID, u.QuotaMB = intPtr(uid), intPtr(gid), intPtr(quota)
	u.CreatedAt, u.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &u, nil
}

// CreateUser inserts a user and fills ID and timestamps.
func (d *DB) CreateUser(ctx context.Context, u *User) error {
	if u.Role == "" {
		u.Role = RoleUser
	}
	if u.Status == "" {
		u.Status = UserActive
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO users(login, role, password_hash, email, unix_uid, unix_gid, home, shell, status, quota_mb, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		u.Login, u.Role, nullStr(u.PasswordHash), nullStr(u.Email), nullIntPtr(u.UnixUID), nullIntPtr(u.UnixGID), nullStr(u.Home), boolInt(u.Shell), u.Status, nullIntPtr(u.QuotaMB), ts, ts).Scan(&u.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	u.CreatedAt = Time(parseTime(ts))
	u.UpdatedAt = u.CreatedAt
	return nil
}

// GetUserByLogin returns a user by login.
func (d *DB) GetUserByLogin(ctx context.Context, login string) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE login=?`, login))
}

// GetUserByID returns a user by id.
func (d *DB) GetUserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// GetUserByUnixUID maps a unix uid (from SO_PEERCRED) to a panel user.
func (d *DB) GetUserByUnixUID(ctx context.Context, uid int) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE unix_uid=?`, uid))
}

// ListUsers returns all users ordered by login.
func (d *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of users per role.
func (d *DB) CountUsers(ctx context.Context) (map[string]int, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT role, COUNT(*) FROM users GROUP BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out[role] = n
	}
	return out, rows.Err()
}

// SetUserUnix records the provisioned unix identity.
func (d *DB) SetUserUnix(ctx context.Context, id int64, uid, gid int, home string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE users SET unix_uid=?, unix_gid=?, home=?, updated_at=? WHERE id=?`, uid, gid, home, now(), id)
	return err
}

// SetUserStatus updates the status, whatever it is now: the removal's own
// mark and the way back from its failure. Anything else that changes the
// status goes through ActivateUser or UpdateUserProfile, which leave an
// account being removed so.
func (d *DB) SetUserStatus(ctx context.Context, id int64, status string) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE users SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ActivateUser makes an account active once its unix side is made — unless
// its removal is under way (ErrBeingRemoved): the removal is queued, and
// what reads the account meanwhile (a restore into its home, an apply of
// its sites, the log rotation) must find it being removed.
func (d *DB) ActivateUser(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE users SET status=?, updated_at=? WHERE id=? AND status<>?`, UserActive, now(), id, UserDeleting)
	if err != nil {
		return err
	}
	return d.userWritten(ctx, res, id)
}

// userWritten tells why a write that leaves an account being removed alone
// changed no row: the account is gone (ErrNotFound) or being removed
// (ErrBeingRemoved).
func (d *DB) userWritten(ctx context.Context, res sql.Result, id int64) error {
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var status string
	if err := d.sql.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return ErrBeingRemoved
}

// SetUserPassword stores a new password hash.
func (d *DB) SetUserPassword(ctx context.Context, id int64, hash string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, hash, now(), id)
	return err
}

// DeleteUser removes a user and, through cascades, sessions and tokens.
func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserProfile writes email, shell, status and quota — not onto an
// account being removed (ErrBeingRemoved): a change that read the account
// before its removal was asked for would put the status it read back over
// the mark. A gone account is ErrNotFound.
func (d *DB) UpdateUserProfile(ctx context.Context, u *User) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE users SET email=?, shell=?, status=?, quota_mb=?, updated_at=? WHERE id=? AND status<>?`, nullStr(u.Email), boolInt(u.Shell), u.Status, nullIntPtr(u.QuotaMB), now(), u.ID, UserDeleting)
	if err != nil {
		return err
	}
	return d.userWritten(ctx, res, u.ID)
}
