package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Certificate kinds and statuses.
const (
	CertKindACME       = "acme"
	CertKindCustom     = "custom"
	CertKindSelfSigned = "selfsigned"

	CertPending = "pending"
	CertValid   = "valid"
	CertError   = "error"
)

// Certificate is a TLS certificate managed by the panel. Name is the primary
// hostname and the directory under <data>/certs/.
type Certificate struct {
	ID           int64      `json:"id"`
	UserID       *int64     `json:"user_id,omitempty"`
	Name         string     `json:"name"`
	Names        []string   `json:"names"`
	Kind         string     `json:"kind"`
	DirectoryURL string     `json:"directory_url,omitempty"`
	KeyType      string     `json:"key_type"`
	Email        string     `json:"email,omitempty"`
	Issuer       string     `json:"issuer,omitempty"`
	Serial       string     `json:"serial,omitempty"`
	NotBefore    *time.Time `json:"not_before,omitempty"`
	NotAfter     *time.Time `json:"not_after,omitempty"`
	AutoRenew    bool       `json:"auto_renew"`
	Status       string     `json:"status"`
	LastError    string     `json:"last_error,omitempty"`
	LastAttempt  *time.Time `json:"last_attempt,omitempty"`
	// UsedByPanel and UsedBySites are computed by the API for listings:
	// a certificate somebody serves cannot simply be deleted.
	UsedByPanel bool     `json:"used_by_panel"`
	UsedBySites []string `json:"used_by_sites"`
	CertPath    string   `json:"cert_path,omitempty"`
	KeyPath     string   `json:"key_path,omitempty"`
	ChainPath   string   `json:"chain_path,omitempty"`
	DNSProvider string   `json:"dns_provider,omitempty"`
	CreatedAt   Time     `json:"created_at"`
	UpdatedAt   Time     `json:"updated_at"`
}

const certCols = `id, user_id, name, names, kind, directory_url, key_type, email, issuer, serial, not_before, not_after, auto_renew, status, last_error, last_attempt, cert_path, key_path, chain_path, created_at, updated_at, dns_provider`

func scanCert(s scanner) (*Certificate, error) {
	var c Certificate
	var userID sql.NullInt64
	var names, created, updated string
	var nb, na, la sql.NullString
	var auto int
	if err := s.Scan(&c.ID, &userID, &c.Name, &names, &c.Kind, &c.DirectoryURL, &c.KeyType, &c.Email, &c.Issuer, &c.Serial, &nb, &na, &auto, &c.Status, &c.LastError, &la, &c.CertPath, &c.KeyPath, &c.ChainPath, &created, &updated, &c.DNSProvider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if userID.Valid {
		v := userID.Int64
		c.UserID = &v
	}
	_ = json.Unmarshal([]byte(names), &c.Names)
	if c.Names == nil {
		c.Names = []string{}
	}
	c.NotBefore, c.NotAfter, c.LastAttempt = parseTimePtr(nb), parseTimePtr(na), parseTimePtr(la)
	c.AutoRenew = auto != 0
	c.CreatedAt, c.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &c, nil
}

func nullInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// UpsertCertificate inserts or updates by name and fills ID.
func (d *DB) UpsertCertificate(ctx context.Context, c *Certificate) error {
	if c.Names == nil {
		c.Names = []string{c.Name}
	}
	names, _ := json.Marshal(c.Names)
	if c.Kind == "" {
		c.Kind = CertKindACME
	}
	if c.KeyType == "" {
		c.KeyType = "ec256"
	}
	if c.Status == "" {
		c.Status = CertPending
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO certificates(user_id, name, names, kind, directory_url, key_type, email, issuer, serial, not_before, not_after, auto_renew, status, last_error, last_attempt, cert_path, key_path, chain_path, dns_provider, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET user_id=excluded.user_id, names=excluded.names, kind=excluded.kind, directory_url=excluded.directory_url, key_type=excluded.key_type, email=excluded.email,
			issuer=excluded.issuer, serial=excluded.serial, not_before=excluded.not_before, not_after=excluded.not_after, auto_renew=excluded.auto_renew, status=excluded.status,
			last_error=excluded.last_error, last_attempt=excluded.last_attempt, cert_path=excluded.cert_path, key_path=excluded.key_path, chain_path=excluded.chain_path, dns_provider=excluded.dns_provider, updated_at=excluded.updated_at
		RETURNING id, created_at`,
		nullInt64Ptr(c.UserID), c.Name, string(names), c.Kind, c.DirectoryURL, c.KeyType, c.Email, c.Issuer, c.Serial, timePtrStr(c.NotBefore), timePtrStr(c.NotAfter), boolInt(c.AutoRenew), c.Status, c.LastError, timePtrStr(c.LastAttempt), c.CertPath, c.KeyPath, c.ChainPath, c.DNSProvider, ts, ts).Scan(&c.ID, &ts)
	if err != nil {
		return err
	}
	c.CreatedAt = Time(parseTime(ts))
	c.UpdatedAt = Time(time.Now().UTC())
	return nil
}

// GetCertificate returns a certificate by id.
func (d *DB) GetCertificate(ctx context.Context, id int64) (*Certificate, error) {
	return scanCert(d.sql.QueryRowContext(ctx, `SELECT `+certCols+` FROM certificates WHERE id=?`, id))
}

// GetCertificateByName returns a certificate by primary name.
func (d *DB) GetCertificateByName(ctx context.Context, name string) (*Certificate, error) {
	return scanCert(d.sql.QueryRowContext(ctx, `SELECT `+certCols+` FROM certificates WHERE name=?`, name))
}

// ListCertificates returns all certificates ordered by name.
func (d *DB) ListCertificates(ctx context.Context) ([]*Certificate, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+certCols+` FROM certificates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Certificate
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCertificatesForRenewal returns ACME certificates with auto-renew whose
// expiry is before the given time (or unknown) and that are not mid-issue.
func (d *DB) ListCertificatesForRenewal(ctx context.Context, before time.Time) ([]*Certificate, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+certCols+` FROM certificates WHERE auto_renew=1 AND kind=? AND status<>? AND (not_after IS NULL OR not_after < ?) ORDER BY not_after`,
		CertKindACME, CertPending, before.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Certificate
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCertificateStatus records the outcome of an issue attempt.
func (d *DB) SetCertificateStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE certificates SET status=?, last_error=?, last_attempt=?, updated_at=? WHERE id=?`, status, lastError, now(), now(), id)
	return err
}

// DeleteCertificate removes the row.
func (d *DB) DeleteCertificate(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM certificates WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
