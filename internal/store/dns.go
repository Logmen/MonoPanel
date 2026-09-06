package store

import (
	"context"
	"database/sql"
	"errors"
)

// DNSProvider holds credentials for a lego DNS challenge provider.
type DNSProvider struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	CredentialsEnc string `json:"-"`
	CreatedAt      Time   `json:"created_at"`
}

// CreateDNSProvider inserts a provider.
func (d *DB) CreateDNSProvider(ctx context.Context, p *DNSProvider) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO dns_providers(name, type, credentials_enc, created_at) VALUES(?,?,?,?) RETURNING id`, p.Name, p.Type, p.CredentialsEnc, ts).Scan(&p.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	p.CreatedAt = Time(parseTime(ts))
	return nil
}

// GetDNSProvider returns a provider by name.
func (d *DB) GetDNSProvider(ctx context.Context, name string) (*DNSProvider, error) {
	var p DNSProvider
	var created string
	err := d.sql.QueryRowContext(ctx, `SELECT id, name, type, credentials_enc, created_at FROM dns_providers WHERE name=?`, name).Scan(&p.ID, &p.Name, &p.Type, &p.CredentialsEnc, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = Time(parseTime(created))
	return &p, nil
}

// ListDNSProviders returns all providers.
func (d *DB) ListDNSProviders(ctx context.Context) ([]*DNSProvider, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, name, type, credentials_enc, created_at FROM dns_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DNSProvider{}
	for rows.Next() {
		var p DNSProvider
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.CredentialsEnc, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = Time(parseTime(created))
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeleteDNSProvider removes a provider.
func (d *DB) DeleteDNSProvider(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM dns_providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
