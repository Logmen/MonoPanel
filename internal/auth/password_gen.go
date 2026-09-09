package auth

import (
	"crypto/rand"
	"errors"
	"math/big"
)

// Character classes of a generated password. The specials are the ones every
// MySQL password policy, shell and URL take without quoting trouble.
const (
	passwordLower   = "abcdefghijkmnopqrstuvwxyz"
	passwordUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordDigits  = "23456789"
	passwordSpecial = "-_!%+=."
)

// NewPassword returns n random characters with at least one lower-case
// letter, one upper-case letter, one digit and one special character, so a
// server-side policy such as MySQL's validate_password (MEDIUM) accepts it;
// a plain random token lacks a special character about half the time.
// Look-alike characters (0/O, 1/l/I) are left out.
func NewPassword(n int) (string, error) {
	if n < 8 {
		return "", errors.New("password shorter than 8 characters")
	}
	classes := []string{passwordLower, passwordUpper, passwordDigits, passwordSpecial}
	all := passwordLower + passwordUpper + passwordDigits + passwordSpecial
	out := make([]byte, 0, n)
	for _, class := range classes {
		c, err := randomByte(class)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < n {
		c, err := randomByte(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	// Shuffle so the guaranteed characters do not sit at the front.
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

func randomByte(alphabet string) (byte, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
	if err != nil {
		return 0, err
	}
	return alphabet[i.Int64()], nil
}
