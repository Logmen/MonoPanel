package store

import (
	"context"
	"database/sql"
	"errors"
)

// Database engines and statuses.
const (
	EngineMySQL   = "mysql"
	EnginePercona = "percona"

	DBInstalling = "installing"
	DBReady      = "ready"
	DBError      = "error"
)

// DBInstance is the local database server managed by the panel.
type DBInstance struct {
	ID             int64  `json:"id"`
	Engine         string `json:"engine"`
	Version        string `json:"version"`
	Socket         string `json:"socket"`
	Service        string `json:"service"`
	NativePassword bool   `json:"native_password"`
	Status         string `json:"status"`
	LastError      string `json:"last_error,omitempty"`
	CreatedAt      Time   `json:"created_at"`
	UpdatedAt      Time   `json:"updated_at"`
}

// Database is a client database with its owner.
type Database struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Login     string    `json:"login,omitempty"`
	Name      string    `json:"name"`
	Charset   string    `json:"charset"`
	Collation string    `json:"collation"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt Time      `json:"created_at"`
	UpdatedAt Time      `json:"updated_at"`
	Users     []*DBUser `json:"users,omitempty"`
}

// DBUser is a MySQL account bound to a database.
type DBUser struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	DatabaseID *int64 `json:"database_id,omitempty"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	AuthPlugin string `json:"auth_plugin"`
	CreatedAt  Time   `json:"created_at"`
}

// GetDBInstance returns the single instance row.
func (d *DB) GetDBInstance(ctx context.Context) (*DBInstance, error) {
	var i DBInstance
	var native int
	var created, updated string
	err := d.sql.QueryRowContext(ctx, `SELECT id, engine, version, socket, service, native_password, status, last_error, created_at, updated_at FROM db_instances ORDER BY id LIMIT 1`).
		Scan(&i.ID, &i.Engine, &i.Version, &i.Socket, &i.Service, &native, &i.Status, &i.LastError, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	i.NativePassword = native != 0
	i.CreatedAt, i.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &i, nil
}

// UpsertDBInstance replaces the instance row.
func (d *DB) UpsertDBInstance(ctx context.Context, i *DBInstance) error {
	ts := now()
	if i.ID == 0 {
		if existing, err := d.GetDBInstance(ctx); err == nil {
			i.ID = existing.ID
		}
	}
	if i.ID == 0 {
		return d.sql.QueryRowContext(ctx, `INSERT INTO db_instances(engine, version, socket, service, native_password, status, last_error, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`,
			i.Engine, i.Version, i.Socket, i.Service, boolInt(i.NativePassword), i.Status, i.LastError, ts, ts).Scan(&i.ID)
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE db_instances SET engine=?, version=?, socket=?, service=?, native_password=?, status=?, last_error=?, updated_at=? WHERE id=?`,
		i.Engine, i.Version, i.Socket, i.Service, boolInt(i.NativePassword), i.Status, i.LastError, ts, i.ID)
	return err
}

const dbCols = `d.id, d.user_id, u.login, d.name, d.charset, d.collation, d.size_bytes, d.created_at, d.updated_at`

func scanDatabase(s scanner) (*Database, error) {
	var db Database
	var created, updated string
	if err := s.Scan(&db.ID, &db.UserID, &db.Login, &db.Name, &db.Charset, &db.Collation, &db.SizeBytes, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	db.CreatedAt, db.UpdatedAt = Time(parseTime(created)), Time(parseTime(updated))
	return &db, nil
}

// CreateDatabase inserts a database row.
func (d *DB) CreateDatabase(ctx context.Context, db *Database) error {
	if db.Charset == "" {
		db.Charset = "utf8mb4"
	}
	if db.Collation == "" {
		db.Collation = "utf8mb4_0900_ai_ci"
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO databases(user_id, name, charset, collation, created_at, updated_at) VALUES(?,?,?,?,?,?) RETURNING id`,
		db.UserID, db.Name, db.Charset, db.Collation, ts, ts).Scan(&db.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	db.CreatedAt = Time(parseTime(ts))
	db.UpdatedAt = db.CreatedAt
	return nil
}

// GetDatabaseByName returns a database with its users.
func (d *DB) GetDatabaseByName(ctx context.Context, name string) (*Database, error) {
	db, err := scanDatabase(d.sql.QueryRowContext(ctx, `SELECT `+dbCols+` FROM databases d JOIN users u ON u.id=d.user_id WHERE d.name=?`, name))
	if err != nil {
		return nil, err
	}
	db.Users, err = d.ListDBUsers(ctx, db.ID)
	return db, err
}

// ListDatabases returns databases (all or of one user) with their users.
func (d *DB) ListDatabases(ctx context.Context, userID int64) ([]*Database, error) {
	var rows *sql.Rows
	var err error
	if userID > 0 {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+dbCols+` FROM databases d JOIN users u ON u.id=d.user_id WHERE d.user_id=? ORDER BY d.name`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT `+dbCols+` FROM databases d JOIN users u ON u.id=d.user_id ORDER BY d.name`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Database
	for rows.Next() {
		db, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, db)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, db := range out {
		if db.Users, err = d.ListDBUsers(ctx, db.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetDatabaseSize records the measured size.
func (d *DB) SetDatabaseSize(ctx context.Context, id, size int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE databases SET size_bytes=?, updated_at=? WHERE id=?`, size, now(), id)
	return err
}

// DeleteDatabase removes the row (users cascade).
func (d *DB) DeleteDatabase(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM databases WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateDBUser inserts a MySQL account row.
func (d *DB) CreateDBUser(ctx context.Context, u *DBUser) error {
	if u.Host == "" {
		u.Host = "localhost"
	}
	if u.AuthPlugin == "" {
		u.AuthPlugin = "caching_sha2_password"
	}
	ts := now()
	err := d.sql.QueryRowContext(ctx, `INSERT INTO db_users(user_id, database_id, name, host, auth_plugin, created_at) VALUES(?,?,?,?,?,?) RETURNING id`,
		u.UserID, nullInt64Ptr(u.DatabaseID), u.Name, u.Host, u.AuthPlugin, ts).Scan(&u.ID)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	u.CreatedAt = Time(parseTime(ts))
	return nil
}

// ListDBUsers returns the accounts of a database.
func (d *DB) ListDBUsers(ctx context.Context, databaseID int64) ([]*DBUser, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, user_id, database_id, name, host, auth_plugin, created_at FROM db_users WHERE database_id=? ORDER BY name, host`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DBUser{}
	for rows.Next() {
		var u DBUser
		var dbid sql.NullInt64
		var created string
		if err := rows.Scan(&u.ID, &u.UserID, &dbid, &u.Name, &u.Host, &u.AuthPlugin, &created); err != nil {
			return nil, err
		}
		if dbid.Valid {
			v := dbid.Int64
			u.DatabaseID = &v
		}
		u.CreatedAt = Time(parseTime(created))
		out = append(out, &u)
	}
	return out, rows.Err()
}

// DeleteDBUser removes an account row.
func (d *DB) DeleteDBUser(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM db_users WHERE id=?`, id)
	return err
}
