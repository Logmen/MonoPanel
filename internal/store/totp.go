package store

import "context"

// TOTP returns the encrypted secret and whether 2FA is enabled.
func (d *DB) TOTP(ctx context.Context, userID int64) (secretEnc string, enabled bool, err error) {
	var en int
	err = d.sql.QueryRowContext(ctx, `SELECT totp_secret_enc, totp_enabled FROM users WHERE id=?`, userID).Scan(&secretEnc, &en)
	return secretEnc, en != 0, err
}

// SetTOTP stores the secret and enabled flag.
func (d *DB) SetTOTP(ctx context.Context, userID int64, secretEnc string, enabled bool) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE users SET totp_secret_enc=?, totp_enabled=?, updated_at=? WHERE id=?`, secretEnc, boolInt(enabled), now(), userID)
	return err
}
