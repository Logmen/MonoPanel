package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// PHP version statuses.
const (
	PHPInstalling = "installing"
	PHPInstalled  = "installed"
	PHPError      = "error"
	PHPRemoving   = "removing"
)

// PHPVersion is an installed (or installing) PHP branch.
type PHPVersion struct {
	Version        string   `json:"version"`
	Source         string   `json:"source"`
	Status         string   `json:"status"`
	FPMService     string   `json:"fpm_service,omitempty"`
	FPMBinary      string   `json:"fpm_binary,omitempty"`
	CLIBinary      string   `json:"cli_binary,omitempty"`
	PoolDir        string   `json:"pool_dir,omitempty"`
	PackageVersion string   `json:"package_version,omitempty"`
	Extensions     []string `json:"extensions"`
	IsDefault      bool     `json:"is_default"`
	LastError      string   `json:"last_error,omitempty"`
	CreatedAt      Time     `json:"created_at"`
	UpdatedAt      Time     `json:"updated_at"`
}

const phpCols = `version, source, status, fpm_service, fpm_binary, cli_binary, pool_dir, package_version, extensions, is_default, last_error, created_at, updated_at`

func scanPHP(s scanner) (*PHPVersion, error) {
	var p PHPVersion
	var ext, created, updated string
	var def int
	if err := s.Scan(&p.Version, &p.Source, &p.Status, &p.FPMService, &p.FPMBinary, &p.CLIBinary, &p.PoolDir, &p.PackageVersion, &ext, &def, &p.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(ext), &p.Extensions)
	if p.Extensions == nil {
		p.Extensions = []string{}
	}
	p.IsDefault = def != 0
	p.CreatedAt, p.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &p, nil
}

// UpsertPHPVersion inserts or updates a version row.
func (d *DB) UpsertPHPVersion(ctx context.Context, p *PHPVersion) error {
	if p.Extensions == nil {
		p.Extensions = []string{}
	}
	ext, _ := json.Marshal(p.Extensions)
	ts := now()
	_, err := d.sql.ExecContext(ctx, `INSERT INTO php_versions(version, source, status, fpm_service, fpm_binary, cli_binary, pool_dir, package_version, extensions, is_default, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(version) DO UPDATE SET source=excluded.source, status=excluded.status, fpm_service=excluded.fpm_service, fpm_binary=excluded.fpm_binary, cli_binary=excluded.cli_binary,
			pool_dir=excluded.pool_dir, package_version=excluded.package_version, extensions=excluded.extensions, is_default=excluded.is_default, last_error=excluded.last_error, updated_at=excluded.updated_at`,
		p.Version, p.Source, p.Status, p.FPMService, p.FPMBinary, p.CLIBinary, p.PoolDir, p.PackageVersion, string(ext), boolInt(p.IsDefault), p.LastError, ts, ts)
	return err
}

// GetPHPVersion returns one version.
func (d *DB) GetPHPVersion(ctx context.Context, version string) (*PHPVersion, error) {
	return scanPHP(d.sql.QueryRowContext(ctx, `SELECT `+phpCols+` FROM php_versions WHERE version=?`, version))
}

// ListPHPVersions returns all rows ordered by version.
func (d *DB) ListPHPVersions(ctx context.Context) ([]*PHPVersion, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+phpCols+` FROM php_versions ORDER BY CAST(substr(version,1,instr(version,'.')-1) AS INTEGER), CAST(substr(version,instr(version,'.')+1) AS INTEGER)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PHPVersion
	for rows.Next() {
		p, err := scanPHP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPHPStatus updates status and error.
func (d *DB) SetPHPStatus(ctx context.Context, version, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE php_versions SET status=?, last_error=?, updated_at=? WHERE version=?`, status, lastError, now(), version)
	return err
}

// DeletePHPVersion removes the row.
func (d *DB) DeletePHPVersion(ctx context.Context, version string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM php_versions WHERE version=?`, version)
	return err
}

// CountSitesByPHP returns how many sites use a version.
func (d *DB) CountSitesByPHP(ctx context.Context, version string) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE php_version=?`, version).Scan(&n)
	return n, err
}
