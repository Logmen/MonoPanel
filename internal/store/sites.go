package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Site statuses and modes.
const (
	SitePending   = "pending"
	SiteActive    = "active"
	SiteSuspended = "suspended"
	SiteError     = "error"
	SiteDeleting  = "deleting"

	ModeFPM    = "fpm"
	ModeApache = "apache"
	ModeProxy  = "proxy"
)

// Site is a hosted website: one nginx server block and one php-fpm pool.
type Site struct {
	ID             int64             `json:"id"`
	UserID         int64             `json:"user_id"`
	Domain         string            `json:"domain"`
	Aliases        []string          `json:"aliases"`
	Mode           string            `json:"mode"`
	PHPVersion     string            `json:"php_version"`
	Docroot        string            `json:"docroot"`
	IP             string            `json:"ip"`
	HTTP2          bool              `json:"http2"`
	HTTP3          bool              `json:"http3"`
	SSL            string            `json:"ssl"`
	RedirectHTTPS  bool              `json:"redirect_https"`
	RedirectWWW    string            `json:"redirect_www"`
	StaticByNginx  bool              `json:"static_by_nginx"`
	FPMPM          string            `json:"fpm_pm"`
	FPMMaxChildren int               `json:"fpm_max_children"`
	PHPIni         map[string]string `json:"php_ini"`
	AllowExec      bool              `json:"allow_exec"`
	ClientMaxBody  string            `json:"client_max_body"`
	CertificateID  *int64            `json:"certificate_id,omitempty"`
	Backend        string            `json:"backend,omitempty"`
	AllowFrom      []string          `json:"allow_from"`
	Preset         string            `json:"preset"`
	// CMS the panel installed here (mp cms install), its version and when.
	CMS        string `json:"cms"`
	CMSVersion string `json:"cms_version,omitempty"`
	CMSAt      string `json:"cms_at,omitempty"`
	Status     string `json:"status"`
	LastError  string `json:"last_error,omitempty"`
	CreatedAt  Time   `json:"created_at"`
	UpdatedAt  Time   `json:"updated_at"`
	// Login is the owner's login (joined, read-only).
	Login string `json:"login,omitempty"`
}

const siteCols = `s.id, s.user_id, s.domain, s.aliases, s.mode, s.php_version, s.docroot, s.ip, s.http2, s.http3, s.ssl, s.redirect_https, s.redirect_www, s.static_by_nginx, s.fpm_pm, s.fpm_max_children, s.php_ini, s.allow_exec, s.client_max_body, s.certificate_id, s.status, s.last_error, s.created_at, s.updated_at, u.login, s.backend, s.allow_from, s.preset, s.cms, s.cms_version, s.cms_at`
const siteFrom = ` FROM sites s JOIN users u ON u.id = s.user_id`

func scanSite(sc scanner) (*Site, error) {
	var s Site
	var aliases, ini, allow, created, updated string
	var http2, http3, redir, static, exec int
	var cert sql.NullInt64
	if err := sc.Scan(&s.ID, &s.UserID, &s.Domain, &aliases, &s.Mode, &s.PHPVersion, &s.Docroot, &s.IP, &http2, &http3, &s.SSL, &redir, &s.RedirectWWW, &static, &s.FPMPM, &s.FPMMaxChildren, &ini, &exec, &s.ClientMaxBody, &cert, &s.Status, &s.LastError, &created, &updated, &s.Login, &s.Backend, &allow, &s.Preset, &s.CMS, &s.CMSVersion, &s.CMSAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(aliases), &s.Aliases)
	if s.Aliases == nil {
		s.Aliases = []string{}
	}
	_ = json.Unmarshal([]byte(allow), &s.AllowFrom)
	if s.AllowFrom == nil {
		s.AllowFrom = []string{}
	}
	_ = json.Unmarshal([]byte(ini), &s.PHPIni)
	if s.PHPIni == nil {
		s.PHPIni = map[string]string{}
	}
	s.HTTP2, s.HTTP3, s.RedirectHTTPS, s.StaticByNginx, s.AllowExec = http2 != 0, http3 != 0, redir != 0, static != 0, exec != 0
	if cert.Valid {
		v := cert.Int64
		s.CertificateID = &v
	}
	s.CreatedAt, s.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &s, nil
}

// CreateSite inserts a site.
func (d *DB) CreateSite(ctx context.Context, s *Site) error {
	if s.Aliases == nil {
		s.Aliases = []string{}
	}
	if s.PHPIni == nil {
		s.PHPIni = map[string]string{}
	}
	if s.Mode == "" {
		s.Mode = ModeFPM
	}
	if s.SSL == "" {
		s.SSL = "auto"
	}
	if s.RedirectWWW == "" {
		s.RedirectWWW = "none"
	}
	if s.FPMPM == "" {
		s.FPMPM = "ondemand"
	}
	if s.FPMMaxChildren <= 0 {
		s.FPMMaxChildren = 8
	}
	if s.ClientMaxBody == "" {
		s.ClientMaxBody = "64m"
	}
	if s.Status == "" {
		s.Status = SitePending
	}
	if s.AllowFrom == nil {
		s.AllowFrom = []string{}
	}
	aliases, _ := json.Marshal(s.Aliases)
	ini, _ := json.Marshal(s.PHPIni)
	allow, _ := json.Marshal(s.AllowFrom)
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO sites(user_id, domain, aliases, mode, php_version, docroot, ip, http2, http3, ssl, redirect_https, redirect_www, static_by_nginx, fpm_pm, fpm_max_children, php_ini, allow_exec, client_max_body, certificate_id, backend, allow_from, preset, cms, cms_version, cms_at, status, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		s.UserID, s.Domain, string(aliases), s.Mode, s.PHPVersion, s.Docroot, s.IP, boolInt(s.HTTP2), boolInt(s.HTTP3), s.SSL, boolInt(s.RedirectHTTPS), s.RedirectWWW, boolInt(s.StaticByNginx), s.FPMPM, s.FPMMaxChildren, string(ini), boolInt(s.AllowExec), s.ClientMaxBody, nullInt64Ptr(s.CertificateID), s.Backend, string(allow), s.Preset, s.CMS, s.CMSVersion, s.CMSAt, s.Status, s.LastError, ts, ts).Scan(&s.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	s.CreatedAt = Time(parseTime(ts))
	s.UpdatedAt = s.CreatedAt
	return nil
}

// UpdateSite writes every mutable field.
func (d *DB) UpdateSite(ctx context.Context, s *Site) error {
	aliases, _ := json.Marshal(s.Aliases)
	ini, _ := json.Marshal(s.PHPIni)
	if s.AllowFrom == nil {
		s.AllowFrom = []string{}
	}
	allow, _ := json.Marshal(s.AllowFrom)
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET aliases=?, mode=?, php_version=?, docroot=?, ip=?, http2=?, http3=?, ssl=?, redirect_https=?, redirect_www=?, static_by_nginx=?, fpm_pm=?, fpm_max_children=?, php_ini=?, allow_exec=?, client_max_body=?, certificate_id=?, backend=?, allow_from=?, preset=?, cms=?, cms_version=?, cms_at=?, status=?, last_error=?, updated_at=? WHERE id=?`,
		string(aliases), s.Mode, s.PHPVersion, s.Docroot, s.IP, boolInt(s.HTTP2), boolInt(s.HTTP3), s.SSL, boolInt(s.RedirectHTTPS), s.RedirectWWW, boolInt(s.StaticByNginx), s.FPMPM, s.FPMMaxChildren, string(ini), boolInt(s.AllowExec), s.ClientMaxBody, nullInt64Ptr(s.CertificateID), s.Backend, string(allow), s.Preset, s.CMS, s.CMSVersion, s.CMSAt, s.Status, s.LastError, now(), s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSiteStatus updates status and error only.
func (d *DB) SetSiteStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sites SET status=?, last_error=?, updated_at=? WHERE id=?`, status, lastError, now(), id)
	return err
}

// GetSite returns a site by id.
func (d *DB) GetSite(ctx context.Context, id int64) (*Site, error) {
	return scanSite(d.sql.QueryRowContext(ctx, `SELECT `+siteCols+siteFrom+` WHERE s.id=?`, id))
}

// GetSiteByDomain returns a site by domain.
func (d *DB) GetSiteByDomain(ctx context.Context, domain string) (*Site, error) {
	return scanSite(d.sql.QueryRowContext(ctx, `SELECT `+siteCols+siteFrom+` WHERE s.domain=?`, domain))
}

// ListSites returns sites, optionally for one user (0 = all).
func (d *DB) ListSites(ctx context.Context, userID int64) ([]*Site, error) {
	var rows *sql.Rows
	var err error
	if userID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+siteCols+siteFrom+` WHERE s.user_id=? ORDER BY s.domain`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+siteCols+siteFrom+` ORDER BY s.domain`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FindSitesByName returns sites whose domain or alias equals name.
func (d *DB) FindSitesByName(ctx context.Context, name string) ([]*Site, error) {
	all, err := d.ListSites(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []*Site
	for _, s := range all {
		if s.Domain == name {
			out = append(out, s)
			continue
		}
		for _, a := range s.Aliases {
			if a == name {
				out = append(out, s)
				break
			}
		}
	}
	return out, nil
}

// DeleteSite removes the row.
func (d *DB) DeleteSite(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM sites WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
