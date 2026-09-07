package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MailDomain is a domain the panel accepts mail for. The DKIM private key is
// kept encrypted next to the public one so a restored panel can rewrite the
// key file opendkim signs with.
type MailDomain struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	Login        string `json:"login,omitempty"`
	Name         string `json:"name"`
	Active       bool   `json:"active"`
	DKIMSelector string `json:"dkim_selector,omitempty"`
	DKIMPublic   string `json:"dkim_public,omitempty"`
	DKIMKeyEnc   string `json:"-"`
	// Lenient принимает почту домена без проверок HELO и домена отправителя:
	// диагностическому приёмнику нужны и те письма, которые обычный домен
	// отверг бы.
	Lenient   bool `json:"lenient"`
	CreatedAt Time `json:"created_at"`
	UpdatedAt Time `json:"updated_at"`
	// Mailboxes and Aliases are filled by the listing calls.
	Mailboxes int `json:"mailboxes"`
	Aliases   int `json:"aliases"`
}

// Mailbox is one mail account: a Maildir plus a password dovecot checks.
type Mailbox struct {
	ID           int64  `json:"id"`
	DomainID     int64  `json:"domain_id"`
	Domain       string `json:"domain,omitempty"`
	LocalPart    string `json:"local_part"`
	Address      string `json:"address"`
	Name         string `json:"name,omitempty"`
	PasswordHash string `json:"-"`
	QuotaMB      int    `json:"quota_mb"`
	Active       bool   `json:"active"`
	SizeBytes    int64  `json:"size_bytes"`
	CreatedAt    Time   `json:"created_at"`
	UpdatedAt    Time   `json:"updated_at"`
}

// MailAlias forwards an address to one or more destinations. Source "@" is the
// catch-all of its domain.
type MailAlias struct {
	ID          int64  `json:"id"`
	DomainID    int64  `json:"domain_id"`
	Domain      string `json:"domain,omitempty"`
	Source      string `json:"source"`
	Address     string `json:"address"`
	Destination string `json:"destination"`
	Active      bool   `json:"active"`
	CreatedAt   Time   `json:"created_at"`
	UpdatedAt   Time   `json:"updated_at"`
}

// Destinations splits the stored comma-separated list.
func (a *MailAlias) Destinations() []string {
	out := []string{}
	for _, d := range strings.Split(a.Destination, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

const mailDomainCols = `d.id, d.user_id, u.login, d.name, d.active, d.dkim_selector, d.dkim_public, d.dkim_key_enc, d.lenient, d.created_at, d.updated_at`

func scanMailDomain(sc scanner) (*MailDomain, error) {
	var d MailDomain
	var active, lenient int
	var created, updated string
	if err := sc.Scan(&d.ID, &d.UserID, &d.Login, &d.Name, &active, &d.DKIMSelector, &d.DKIMPublic, &d.DKIMKeyEnc, &lenient, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.Active, d.Lenient = active != 0, lenient != 0
	d.CreatedAt, d.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &d, nil
}

// CreateMailDomain inserts a domain.
func (d *DB) CreateMailDomain(ctx context.Context, m *MailDomain) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO mail_domains(user_id, name, active, dkim_selector, dkim_public, dkim_key_enc, lenient, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`,
		m.UserID, m.Name, boolInt(m.Active), m.DKIMSelector, m.DKIMPublic, m.DKIMKeyEnc, boolInt(m.Lenient), ts, ts).Scan(&m.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	m.CreatedAt, m.UpdatedAt = Time(parseTime(ts)), Time(parseTime(ts))
	return nil
}

// UpdateMailDomain saves the mutable fields.
func (d *DB) UpdateMailDomain(ctx context.Context, m *MailDomain) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mail_domains SET user_id=?, active=?, dkim_selector=?, dkim_public=?, dkim_key_enc=?, lenient=?, updated_at=? WHERE id=?`,
		m.UserID, boolInt(m.Active), m.DKIMSelector, m.DKIMPublic, m.DKIMKeyEnc, boolInt(m.Lenient), now(), m.ID)
	return err
}

// GetMailDomain returns a domain by name.
func (d *DB) GetMailDomain(ctx context.Context, name string) (*MailDomain, error) {
	return scanMailDomain(d.sql.QueryRowContext(ctx, `SELECT `+mailDomainCols+` FROM mail_domains d JOIN users u ON u.id=d.user_id WHERE d.name=?`, name))
}

// ListMailDomains returns every domain (userID 0) or the ones of a user, with
// mailbox and alias counts.
func (d *DB) ListMailDomains(ctx context.Context, userID int64) ([]*MailDomain, error) {
	q := `SELECT ` + mailDomainCols + ` FROM mail_domains d JOIN users u ON u.id=d.user_id`
	args := []any{}
	if userID > 0 {
		q += ` WHERE d.user_id=?`
		args = append(args, userID)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY d.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MailDomain{}
	for rows.Next() {
		m, err := scanMailDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range out {
		d.sql.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes WHERE domain_id=?`, m.ID).Scan(&m.Mailboxes)  //nolint:errcheck // counters are decoration
		d.sql.QueryRowContext(ctx, `SELECT count(*) FROM mail_aliases WHERE domain_id=?`, m.ID).Scan(&m.Aliases) //nolint:errcheck // counters are decoration
	}
	return out, nil
}

// DeleteMailDomain removes a domain with its mailboxes and aliases.
func (d *DB) DeleteMailDomain(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM mail_domains WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const mailboxCols = `b.id, b.domain_id, d.name, b.local_part, b.address, b.name, b.password_hash, b.quota_mb, b.active, b.size_bytes, b.created_at, b.updated_at`

func scanMailbox(sc scanner) (*Mailbox, error) {
	var b Mailbox
	var active int
	var created, updated string
	if err := sc.Scan(&b.ID, &b.DomainID, &b.Domain, &b.LocalPart, &b.Address, &b.Name, &b.PasswordHash, &b.QuotaMB, &active, &b.SizeBytes, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.Active = active != 0
	b.CreatedAt, b.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &b, nil
}

// CreateMailbox inserts a mailbox.
func (d *DB) CreateMailbox(ctx context.Context, b *Mailbox) error {
	if b.QuotaMB < 0 {
		b.QuotaMB = 0
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO mailboxes(domain_id, local_part, address, name, password_hash, quota_mb, active, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`,
		b.DomainID, b.LocalPart, b.Address, b.Name, b.PasswordHash, b.QuotaMB, boolInt(b.Active), ts, ts).Scan(&b.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	b.CreatedAt, b.UpdatedAt = Time(parseTime(ts)), Time(parseTime(ts))
	return nil
}

// UpdateMailbox saves the mutable fields.
func (d *DB) UpdateMailbox(ctx context.Context, b *Mailbox) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mailboxes SET name=?, password_hash=?, quota_mb=?, active=?, size_bytes=?, updated_at=? WHERE id=?`,
		b.Name, b.PasswordHash, b.QuotaMB, boolInt(b.Active), b.SizeBytes, now(), b.ID)
	return err
}

// GetMailbox returns a mailbox by address.
func (d *DB) GetMailbox(ctx context.Context, address string) (*Mailbox, error) {
	return scanMailbox(d.sql.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes b JOIN mail_domains d ON d.id=b.domain_id WHERE b.address=?`, address))
}

// ListMailboxes returns the mailboxes of a domain (domainID > 0), of a user
// (userID > 0) or all of them.
func (d *DB) ListMailboxes(ctx context.Context, domainID, userID int64) ([]*Mailbox, error) {
	q := `SELECT ` + mailboxCols + ` FROM mailboxes b JOIN mail_domains d ON d.id=b.domain_id`
	args := []any{}
	switch {
	case domainID > 0:
		q += ` WHERE b.domain_id=?`
		args = append(args, domainID)
	case userID > 0:
		q += ` WHERE d.user_id=?`
		args = append(args, userID)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY b.address`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Mailbox{}
	for rows.Next() {
		b, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteMailbox removes a mailbox row.
func (d *DB) DeleteMailbox(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM mailboxes WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const mailAliasCols = `a.id, a.domain_id, d.name, a.source, a.destination, a.active, a.created_at, a.updated_at`

func scanMailAlias(sc scanner) (*MailAlias, error) {
	var a MailAlias
	var active int
	var created, updated string
	if err := sc.Scan(&a.ID, &a.DomainID, &a.Domain, &a.Source, &a.Destination, &active, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Active = active != 0
	// Catch-all хранится под именем "@", а показывается как "@домен".
	a.Address = a.Source + "@" + a.Domain
	if a.Source == "@" {
		a.Address = "@" + a.Domain
	}
	a.CreatedAt, a.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &a, nil
}

// CreateMailAlias inserts an alias.
func (d *DB) CreateMailAlias(ctx context.Context, a *MailAlias) error {
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO mail_aliases(domain_id, source, destination, active, created_at, updated_at) VALUES(?,?,?,?,?,?) RETURNING id`,
		a.DomainID, a.Source, a.Destination, boolInt(a.Active), ts, ts).Scan(&a.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	a.CreatedAt, a.UpdatedAt = Time(parseTime(ts)), Time(parseTime(ts))
	return nil
}

// UpdateMailAlias saves destinations and state.
func (d *DB) UpdateMailAlias(ctx context.Context, a *MailAlias) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mail_aliases SET destination=?, active=?, updated_at=? WHERE id=?`, a.Destination, boolInt(a.Active), now(), a.ID)
	return err
}

// GetMailAlias returns an alias by domain and source.
func (d *DB) GetMailAlias(ctx context.Context, domainID int64, source string) (*MailAlias, error) {
	return scanMailAlias(d.sql.QueryRowContext(ctx, `SELECT `+mailAliasCols+` FROM mail_aliases a JOIN mail_domains d ON d.id=a.domain_id WHERE a.domain_id=? AND a.source=?`, domainID, source))
}

// ListMailAliases returns the aliases of a domain (domainID > 0), of a user
// (userID > 0) or all of them.
func (d *DB) ListMailAliases(ctx context.Context, domainID, userID int64) ([]*MailAlias, error) {
	q := `SELECT ` + mailAliasCols + ` FROM mail_aliases a JOIN mail_domains d ON d.id=a.domain_id`
	args := []any{}
	switch {
	case domainID > 0:
		q += ` WHERE a.domain_id=?`
		args = append(args, domainID)
	case userID > 0:
		q += ` WHERE d.user_id=?`
		args = append(args, userID)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY a.source`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MailAlias{}
	for rows.Next() {
		a, err := scanMailAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteMailAlias removes an alias row.
func (d *DB) DeleteMailAlias(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM mail_aliases WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
