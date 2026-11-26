package api

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // mysql_native_password is SHA1(SHA1()), by its definition
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The hashes MySQL stores for a password, made here: an account whose
// password the server's validate_password policy refuses in plain text
// (IDENTIFIED BY) gets it as a hash (IDENTIFIED … AS), which the policy does
// not judge — the password stays the one the site's settings hold. A
// migrated 1C-Bitrix database kept auth_socket otherwise, and the site could
// not connect (new.qaj.kz, 25.11.2026).

// crypt64 is the alphabet of crypt(3)'s base 64.
const crypt64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// cachingSHA2Rounds is MySQL's default: 5 thousands (the "005" of the hash).
const cachingSHA2Rounds = 5

// mysqlCachingSHA2Hash is the authentication string of caching_sha2_password
// for a password: "$A$", the rounds in thousands as three hex digits, "$", a
// salt of 20 characters and the SHA-256 crypt of the password with it.
func mysqlCachingSHA2Hash(password string) (string, error) {
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	for i, b := range salt {
		salt[i] = crypt64[int(b)%len(crypt64)]
	}
	return cachingSHA2HashWith(password, salt), nil
}

func cachingSHA2HashWith(password string, salt []byte) string {
	return fmt.Sprintf("$A$%03X$%s%s", cachingSHA2Rounds, salt, sha256Crypt([]byte(password), salt, cachingSHA2Rounds*1000))
}

// mysqlNativeHash is mysql_native_password's: "*" and SHA1(SHA1(password)) in
// upper-case hex.
func mysqlNativeHash(password string) string {
	first := sha1.Sum([]byte(password)) //nolint:gosec // the plugin's own definition
	second := sha1.Sum(first[:])        //nolint:gosec // the plugin's own definition
	return "*" + strings.ToUpper(hex.EncodeToString(second[:]))
}

// sha256Crypt is the digest of Ulrich Drepper's SHA-256 crypt (the 43
// characters after the salt in $5$salt$…), with the whole salt — MySQL's
// takes 20 bytes of it, where crypt(3) stops at 16.
func sha256Crypt(key, salt []byte, rounds int) string {
	sum := func(parts ...[]byte) []byte {
		h := sha256.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}
	alt := sum(key, salt, key)
	h := sha256.New()
	h.Write(key)
	h.Write(salt)
	n := len(key)
	for ; n > 32; n -= 32 {
		h.Write(alt)
	}
	h.Write(alt[:n])
	for n = len(key); n > 0; n >>= 1 {
		if n&1 != 0 {
			h.Write(alt)
		} else {
			h.Write(key)
		}
	}
	a := h.Sum(nil)

	h.Reset()
	for range len(key) {
		h.Write(key)
	}
	p := repeatTo(h.Sum(nil), len(key))
	h.Reset()
	for range 16 + int(a[0]) {
		h.Write(salt)
	}
	s := repeatTo(h.Sum(nil), len(salt))

	for r := range rounds {
		h.Reset()
		if r&1 != 0 {
			h.Write(p)
		} else {
			h.Write(a)
		}
		if r%3 != 0 {
			h.Write(s)
		}
		if r%7 != 0 {
			h.Write(p)
		}
		if r&1 != 0 {
			h.Write(a)
		} else {
			h.Write(p)
		}
		a = h.Sum(nil)
	}

	var out strings.Builder
	b64 := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(crypt64[w&0x3f])
			w >>= 6
		}
	}
	for _, t := range [][3]int{{0, 10, 20}, {21, 1, 11}, {12, 22, 2}, {3, 13, 23}, {24, 4, 14}, {15, 25, 5}, {6, 16, 26}, {27, 7, 17}, {18, 28, 8}, {9, 19, 29}} {
		b64(a[t[0]], a[t[1]], a[t[2]], 4)
	}
	b64(0, a[31], a[30], 3)
	return out.String()
}

// repeatTo is b repeated, cut to n bytes.
func repeatTo(b []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, b[:min(len(b), n-len(out))]...)
	}
	return out
}
