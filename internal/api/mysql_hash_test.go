package api

import (
	"regexp"
	"strings"
	"testing"
)

// The SHA-256 crypt matches crypt(3)'s (openssl passwd -5, the reference of
// Drepper's spec among the vectors), a key longer than one digest included;
// caching_sha2_password's string is "$A$005$", 20 characters of salt and the
// 43 of the digest; mysql_native_password's the documented one.
func TestMySQLPasswordHashes(t *testing.T) {
	for _, tc := range []struct{ key, salt, want string }{
		{"Hello world!", "saltstring", "5B8vYYiY.CVt1RlTTf8KbXBH3hsxY/GNooZaBBGWEc5"},
		{"pässwörd-with-more-than-thirty-two-characters-123", "ab", "bqFVg6do2dAZOCrcS6uI/bZP4HCJAHmSfziz9ohSWl5"},
	} {
		if got := sha256Crypt([]byte(tc.key), []byte(tc.salt), 5000); got != tc.want {
			t.Errorf("%q/%q: %s, want %s", tc.key, tc.salt, got, tc.want)
		}
	}
	h, err := mysqlCachingSHA2Hash("bitrix0-Pass")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$A\$005\$[./0-9A-Za-z]{20}[./0-9A-Za-z]{43}$`).MatchString(h) {
		t.Errorf("caching_sha2 string %q", h)
	}
	if again := cachingSHA2HashWith("bitrix0-Pass", []byte(h[7:27])); again != h {
		t.Errorf("not the same with the same salt: %s, %s", again, h)
	}
	if got := mysqlNativeHash("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Errorf("native %s", got)
	}
	if strings.Contains(h, "bitrix0") {
		t.Error("the password shows in its hash")
	}
}
