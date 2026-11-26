package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
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
	IPv6           string            `json:"ipv6"`
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
	// CertPinned: the certificate was given by hand and stays (the column
	// cert_pinned says why).
	CertPinned bool `json:"cert_pinned,omitempty"`
	// DefaultTLS: a TLS client without SNI on the site's address gets its
	// certificate (one site per address; the column default_tls says why).
	DefaultTLS bool     `json:"default_tls,omitempty"`
	Backend    string   `json:"backend,omitempty"`
	AllowFrom  []string `json:"allow_from"`
	// SecurityHeaders adds X-Content-Type-Options, X-Frame-Options and
	// Referrer-Policy to every response; RateLimit caps requests per second
	// from one address on dynamic pages (0 — no limit).
	SecurityHeaders bool   `json:"security_headers"`
	RateLimit       int    `json:"rate_limit"`
	Preset          string `json:"preset"`
	// CMS the panel installed here (mp cms install), its version and when.
	CMS        string `json:"cms"`
	CMSVersion string `json:"cms_version,omitempty"`
	CMSAt      string `json:"cms_at,omitempty"`
	// CMSDatabase is the database that install made: a forced reinstall
	// empties only this one.
	CMSDatabase string `json:"cms_database,omitempty"`
	// SessionStore is where PHP keeps the site's sessions: "" — files in the
	// account's tmp, SessionStoreValkey — the account's sessions instance.
	SessionStore string `json:"session_store"`
	Status       string `json:"status"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    Time   `json:"created_at"`
	UpdatedAt    Time   `json:"updated_at"`
	// Login is the owner's login (joined, read-only).
	Login string `json:"login,omitempty"`
}

const siteCols = `s.id, s.user_id, s.domain, s.aliases, s.mode, s.php_version, s.docroot, s.ip, s.http2, s.http3, s.ssl, s.redirect_https, s.redirect_www, s.static_by_nginx, s.fpm_pm, s.fpm_max_children, s.php_ini, s.allow_exec, s.client_max_body, s.certificate_id, s.status, s.last_error, s.created_at, s.updated_at, u.login, s.backend, s.allow_from, s.preset, s.cms, s.cms_version, s.cms_at, s.cms_database, s.session_store, s.ipv6, s.security_headers, s.rate_limit, s.cert_pinned, s.default_tls`
const siteFrom = ` FROM sites s JOIN users u ON u.id = s.user_id`

func scanSite(sc scanner) (*Site, error) {
	var s Site
	var aliases, ini, allow, created, updated string
	var http2, http3, redir, static, exec, headers, pinned, defTLS int
	var cert sql.NullInt64
	if err := sc.Scan(&s.ID, &s.UserID, &s.Domain, &aliases, &s.Mode, &s.PHPVersion, &s.Docroot, &s.IP, &http2, &http3, &s.SSL, &redir, &s.RedirectWWW, &static, &s.FPMPM, &s.FPMMaxChildren, &ini, &exec, &s.ClientMaxBody, &cert, &s.Status, &s.LastError, &created, &updated, &s.Login, &s.Backend, &allow, &s.Preset, &s.CMS, &s.CMSVersion, &s.CMSAt, &s.CMSDatabase, &s.SessionStore, &s.IPv6, &headers, &s.RateLimit, &pinned, &defTLS); err != nil {
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
	s.HTTP2, s.HTTP3, s.RedirectHTTPS, s.StaticByNginx, s.AllowExec, s.SecurityHeaders, s.CertPinned, s.DefaultTLS = http2 != 0, http3 != 0, redir != 0, static != 0, exec != 0, headers != 0, pinned != 0, defTLS != 0
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
	err := d.sql.QueryRowContext(ctx, `INSERT INTO sites(user_id, domain, aliases, mode, php_version, docroot, ip, http2, http3, ssl, redirect_https, redirect_www, static_by_nginx, fpm_pm, fpm_max_children, php_ini, allow_exec, client_max_body, certificate_id, backend, allow_from, preset, cms, cms_version, cms_at, cms_database, session_store, ipv6, security_headers, rate_limit, cert_pinned, status, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		s.UserID, s.Domain, string(aliases), s.Mode, s.PHPVersion, s.Docroot, s.IP, boolInt(s.HTTP2), boolInt(s.HTTP3), s.SSL, boolInt(s.RedirectHTTPS), s.RedirectWWW, boolInt(s.StaticByNginx), s.FPMPM, s.FPMMaxChildren, string(ini), boolInt(s.AllowExec), s.ClientMaxBody, nullInt64Ptr(s.CertificateID), s.Backend, string(allow), s.Preset, s.CMS, s.CMSVersion, s.CMSAt, s.CMSDatabase, s.SessionStore, s.IPv6, boolInt(s.SecurityHeaders), s.RateLimit, boolInt(s.CertPinned), s.Status, s.LastError, ts, ts).Scan(&s.ID)
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

// Clone is a copy of the site that shares nothing with it: the site as a
// caller read it, kept aside before the caller changes it
// (UpdateSiteChanges). An empty list or map stays empty.
func (s *Site) Clone() *Site {
	c := *s
	c.Aliases = slices.Clone(s.Aliases)
	c.AllowFrom = slices.Clone(s.AllowFrom)
	c.PHPIni = maps.Clone(s.PHPIni)
	if s.CertificateID != nil {
		id := *s.CertificateID
		c.CertificateID = &id
	}
	return &c
}

// siteColumn is one mutable column of a site with its value as stored.
type siteColumn struct {
	name  string
	value any
}

// siteColumns are the mutable columns of a site with s's values, in one
// order: what UpdateSite writes and what UpdateSiteChanges compares. A list
// or a map that is not there is the empty one, as the store keeps it:
// compared as "null" against "[]", the aliases of a site without any would
// differ from themselves, and every change of the site would write the
// aliases it read over one added meanwhile.
func siteColumns(s *Site) []siteColumn {
	aliases, _ := json.Marshal(orEmpty(s.Aliases))
	ini := []byte("{}")
	if len(s.PHPIni) > 0 {
		ini, _ = json.Marshal(s.PHPIni)
	}
	allow, _ := json.Marshal(orEmpty(s.AllowFrom))
	return []siteColumn{
		{"aliases", string(aliases)}, {"mode", s.Mode}, {"php_version", s.PHPVersion}, {"docroot", s.Docroot}, {"ip", s.IP},
		{"http2", boolInt(s.HTTP2)}, {"http3", boolInt(s.HTTP3)}, {"ssl", s.SSL}, {"redirect_https", boolInt(s.RedirectHTTPS)},
		{"redirect_www", s.RedirectWWW}, {"static_by_nginx", boolInt(s.StaticByNginx)}, {"fpm_pm", s.FPMPM},
		{"fpm_max_children", s.FPMMaxChildren}, {"php_ini", string(ini)}, {"allow_exec", boolInt(s.AllowExec)},
		{"client_max_body", s.ClientMaxBody}, {"certificate_id", nullInt64Ptr(s.CertificateID)}, {"cert_pinned", boolInt(s.CertPinned)}, {"default_tls", boolInt(s.DefaultTLS)}, {"backend", s.Backend},
		{"allow_from", string(allow)}, {"preset", s.Preset}, {"cms", s.CMS}, {"cms_version", s.CMSVersion}, {"cms_at", s.CMSAt},
		{"cms_database", s.CMSDatabase}, {"session_store", s.SessionStore}, {"ipv6", s.IPv6},
		{"security_headers", boolInt(s.SecurityHeaders)}, {"rate_limit", s.RateLimit}, {"status", s.Status}, {"last_error", s.LastError},
	}
}

// updateSiteColumns writes the assignments given, and updated_at, into the
// site id; ErrNotFound when there is no such site.
func (d *DB) updateSiteColumns(ctx context.Context, id int64, set []string, args []any) error {
	set = append(set, "updated_at=?")
	args = append(args, now(), id)
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET `+strings.Join(set, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSite writes every mutable field.
func (d *DB) UpdateSite(ctx context.Context, s *Site) error {
	if s.AllowFrom == nil {
		s.AllowFrom = []string{}
	}
	var set []string
	var args []any
	for _, c := range siteColumns(s) {
		set, args = append(set, c.name+"=?"), append(args, c.value)
	}
	return d.updateSiteColumns(ctx, s.ID, set, args)
}

// orEmpty is list, or the empty list for none.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// UpdateSiteChanges writes the fields of s that differ from was — the site
// as the caller read it, kept aside (Clone) before the change — and leaves
// every other column as it is now. A request or a job that read the site,
// changed a field and wrote the whole row back would put back what changed
// beside it meanwhile: the address sites.move-ip gave the site, a change
// asked for at the same time — lost for good, the next apply renders the
// row as it is. A list or a map is one column: two changes of the same one
// at once still end with the later.
//
// Nothing goes onto a site being removed (ErrBeingRemoved): its removal is
// queued and must find the site as its mark left it, and whoever asked for
// the change must hear that it did not go in. The way back from a failure,
// error to pending, is the one change of status a change of the site makes
// of itself: it goes in only while the site is still failed — a suspension
// given meanwhile stays. A status asked for (a suspension, its end) goes in
// over any other. A site gone meanwhile is ErrNotFound.
func (d *DB) UpdateSiteChanges(ctx context.Context, was, s *Site) error {
	return d.updateSiteChanges(ctx, was, s, false)
}

// UpdateSiteChangesIfOwnerStays is UpdateSiteChanges for a change a request
// asks for: nothing goes in while the site's account is being removed
// either (ErrBeingRemoved). The account's removal marks the account, not
// its sites, and takes them with it: a change written meanwhile would be
// answered as accepted and its apply queued for nothing. A job that records
// what it made under the site's lock — a CMS install the account's removal
// waits for — writes with UpdateSiteChanges: the removal comes after it and
// takes what it made.
func (d *DB) UpdateSiteChangesIfOwnerStays(ctx context.Context, was, s *Site) error {
	return d.updateSiteChanges(ctx, was, s, true)
}

func (d *DB) updateSiteChanges(ctx context.Context, was, s *Site, ownerStays bool) error {
	before := siteColumns(was)
	var set []string
	var args []any
	for i, c := range siteColumns(s) {
		switch {
		case c.value == before[i].value:
		case c.name == "status" && was.Status == SiteError && s.Status == SitePending:
			set, args = append(set, "status=CASE WHEN status=? THEN ? ELSE status END"), append(args, SiteError, SitePending)
		default:
			set, args = append(set, c.name+"=?"), append(args, c.value)
		}
	}
	set = append(set, "updated_at=?")
	args = append(args, now(), s.ID, SiteDeleting)
	where := `id=? AND status<>?`
	if ownerStays {
		where += ` AND NOT EXISTS (SELECT 1 FROM users WHERE users.id=sites.user_id AND users.status=?)`
		args = append(args, UserDeleting)
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET `+strings.Join(set, ", ")+` WHERE `+where, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var status string
	if err := d.sql.QueryRowContext(ctx, `SELECT status FROM sites WHERE id=?`, s.ID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return ErrBeingRemoved
}

// MarkSiteDeleting marks a site being removed and changes nothing else of
// its row: what another request changed meanwhile stays, as the removal
// finds it. A site marked already is marked again — its removal asked for
// once more, say after the panel stopped before it queued the first.
func (d *DB) MarkSiteDeleting(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET status=?, updated_at=? WHERE id=?`, SiteDeleting, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveAppliedSite stores what a site.apply decided and nothing else: the
// address it took for a site without one (unless one was set meanwhile), the
// certificate it serves (withCert; unless the site's certificate changed from
// fromCert, the one the apply read, meanwhile — given by hand or let go with
// mp site set --cert), the status and the error. The other
// fields stay as they are now: a change made while the apply ran is the next
// apply's to render, not to be overwritten with what this one read. The
// status and the error, too, go in only while the status is still from, the
// one the apply read at its start: a suspension, its end or a removal asked
// for meanwhile has its own job queued, which waits for this apply and then
// reads the site — it must find what was asked for, not what this apply
// decided from the status before.
func (d *DB) SaveAppliedSite(ctx context.Context, s *Site, from string, withCert bool, fromCert *int64) error {
	// SQLite evaluates every expression of SET on the row as it was: both
	// CASEs see the status before this update.
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET ip=CASE WHEN ip='' THEN ? ELSE ip END, certificate_id=CASE WHEN ? AND certificate_id IS ? THEN ? ELSE certificate_id END,
		status=CASE WHEN status=? THEN ? ELSE status END, last_error=CASE WHEN status=? THEN ? ELSE last_error END, updated_at=? WHERE id=?`,
		s.IP, boolInt(withCert), nullInt64Ptr(fromCert), nullInt64Ptr(s.CertificateID), from, s.Status, from, s.LastError, now(), s.ID)
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

// SetSiteStatusFrom is SetSiteStatus while the status is still from, the
// one a job read at its start (SaveAppliedSite says why).
func (d *DB) SetSiteStatusFrom(ctx context.Context, id int64, from, status, lastError string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sites SET status=?, last_error=?, updated_at=? WHERE id=? AND status=?`, status, lastError, now(), id, from)
	return err
}

// MoveSiteIP puts a site that listens on from onto to, and changes nothing
// else of the row but a status of error, which goes back to pending: the
// site comes back on the new address with its next apply. A change made to
// the site meanwhile (mp site set) stays; a site no longer on from, or one
// being removed, is left as it is. It tells whether the site moved.
func (d *DB) MoveSiteIP(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := d.sql.ExecContext(ctx, `UPDATE sites SET ip=?, status=CASE WHEN status=? THEN ? ELSE status END, updated_at=? WHERE id=? AND ip=? AND status<>?`,
		to, SiteError, SitePending, now(), id, from, SiteDeleting)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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

// ClearDefaultTLS takes default_tls off the other sites of an address: one
// site per address answers the clients without SNI.
func (d *DB) ClearDefaultTLS(ctx context.Context, ip string, except int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sites SET default_tls=0, updated_at=? WHERE ip=? AND id<>? AND default_tls<>0`, now(), ip, except)
	return err
}
