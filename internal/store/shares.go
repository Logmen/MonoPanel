package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Share is a folder of one account's site that another account (the guest)
// reaches over SFTP: a bind mount of the folder into the guest's chroot plus
// ACLs on the folder itself.
type Share struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Login      string `json:"login"` // the guest
	SiteID     int64  `json:"site_id"`
	Domain     string `json:"domain"`
	OwnerLogin string `json:"owner_login"`
	Path       string `json:"path"` // inside the site's docroot
	Name       string `json:"name"` // folder name in the guest's home
	NoPHP      bool   `json:"no_php"`
	Status     string `json:"status,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	CreatedAt  Time   `json:"created_at"`
	UpdatedAt  Time   `json:"updated_at"`
}

// MountUnit names the systemd mount unit for the guest-side path, the way
// systemd-escape --path would: slashes become dashes, everything else
// outside [A-Za-z0-9:_.] is \xHH.
func MountUnit(where string) string {
	var b strings.Builder
	for _, r := range strings.Trim(where, "/") {
		switch {
		case r == '/':
			b.WriteByte('-')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ':', r == '_', r == '.':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String() + ".mount"
}

const shareCols = `sh.id, sh.user_id, u.login, sh.site_id, s.domain, o.login, sh.path, sh.name, sh.no_php, sh.status, sh.last_error, sh.created_at, sh.updated_at`
const shareFrom = ` FROM user_shares sh JOIN users u ON u.id = sh.user_id JOIN sites s ON s.id = sh.site_id JOIN users o ON o.id = s.user_id`

func scanShare(sc scanner) (*Share, error) {
	var sh Share
	var noPHP int
	var created, updated string
	if err := sc.Scan(&sh.ID, &sh.UserID, &sh.Login, &sh.SiteID, &sh.Domain, &sh.OwnerLogin, &sh.Path, &sh.Name, &noPHP, &sh.Status, &sh.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sh.NoPHP = noPHP != 0
	sh.CreatedAt, sh.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &sh, nil
}

// CreateShare stores a share; the guest may not have two folders of one name.
func (d *DB) CreateShare(ctx context.Context, sh *Share) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO user_shares(user_id, site_id, path, name, no_php, status, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, sh.UserID, sh.SiteID, sh.Path, sh.Name, boolInt(sh.NoPHP), sh.Status, sh.LastError, ts, ts).Scan(&sh.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	sh.CreatedAt = Time(parseTime(ts))
	sh.UpdatedAt = sh.CreatedAt
	return nil
}

func (d *DB) SetShareStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE user_shares SET status=?, last_error=?, updated_at=? WHERE id=?`, status, lastError, now(), id)
	return err
}

func (d *DB) DeleteShare(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM user_shares WHERE id=?`, id)
	return err
}

// GetShare finds a guest's folder by name.
func (d *DB) GetShare(ctx context.Context, userID int64, name string) (*Share, error) {
	return scanShare(d.sql.QueryRowContext(ctx, `SELECT `+shareCols+shareFrom+` WHERE sh.user_id=? AND sh.name=?`, userID, name))
}

// ListShares lists the folders a guest reaches (userID) or every share (0).
func (d *DB) ListShares(ctx context.Context, userID int64) ([]*Share, error) {
	if userID > 0 {
		return d.shares(ctx, `SELECT `+shareCols+shareFrom+` WHERE sh.user_id=? ORDER BY sh.name`, userID)
	}
	return d.shares(ctx, `SELECT `+shareCols+shareFrom+` ORDER BY u.login, sh.name`)
}

// ListSiteShares lists the folders of one site that guests reach.
func (d *DB) ListSiteShares(ctx context.Context, siteID int64) ([]*Share, error) {
	return d.shares(ctx, `SELECT `+shareCols+shareFrom+` WHERE sh.site_id=? ORDER BY sh.path`, siteID)
}

func (d *DB) shares(ctx context.Context, query string, args ...any) ([]*Share, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Share{}
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}
