package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword("correct horse battery staple", h); err != nil || !ok {
		t.Fatalf("verify failed: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := VerifyPassword("x", "$md5$garbage"); err == nil {
		t.Fatal("garbage hash accepted")
	}
}

func TestTokens(t *testing.T) {
	a, _ := NewToken(32)
	b, _ := NewToken(32)
	if a == b || len(a) < 40 {
		t.Fatalf("tokens not random enough: %s %s", a, b)
	}
	if HashToken(a) == HashToken(b) || len(HashToken(a)) != 64 {
		t.Fatal("hash broken")
	}
}
