// Package secrets encrypts values stored in the panel database (backup
// passwords, cloud credentials, DNS API tokens) with a key kept outside the
// database in /etc/monopanel/secret.key.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// Box encrypts and decrypts with AES-256-GCM.
type Box struct{ key [32]byte }

// Open derives the key from the secret file (any content ≥ 32 bytes).
func Open(path string) (*Box, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) < 32 {
		return nil, errors.New("secret key too short: " + path)
	}
	return &Box{key: sha256.Sum256([]byte(strings.TrimSpace(string(b))))}, nil
}

// Encrypt returns "v1:" + base64(nonce|ciphertext).
func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return "v1:" + base64.RawStdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt; empty in gives empty out.
func (b *Box) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if !strings.HasPrefix(enc, "v1:") {
		return "", errors.New("unknown secret format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc[3:])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
