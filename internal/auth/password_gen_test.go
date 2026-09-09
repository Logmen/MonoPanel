package auth

import (
	"strings"
	"testing"
)

func TestNewPasswordHasEveryClass(t *testing.T) {
	for i := 0; i < 200; i++ {
		p, err := NewPassword(20)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 20 {
			t.Fatalf("length %d: %q", len(p), p)
		}
		for _, class := range []string{passwordLower, passwordUpper, passwordDigits, passwordSpecial} {
			if !strings.ContainsAny(p, class) {
				t.Fatalf("%q lacks a character from %q", p, class)
			}
		}
		if strings.ContainsAny(p, "0O1lI'\"\\ ") {
			t.Fatalf("%q has a look-alike or a quoting character", p)
		}
	}
	if _, err := NewPassword(4); err == nil {
		t.Fatal("too short must be refused")
	}
}
