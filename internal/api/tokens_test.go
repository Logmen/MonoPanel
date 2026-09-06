package api

import (
	"net/http"
	"testing"

	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// Tokens belong to a panel account. Administrators may mint one for another
// account, which is what root on the local socket relies on: it has no account
// of its own.
func TestTokenForAnotherAccount(t *testing.T) {
	f := newSiteFixture(t)

	var created struct {
		Token  string          `json:"token"`
		Record *store.APIToken `json:"record"`
	}
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "for-alex", "user": "alex"}, http.StatusCreated, &created)
	owner, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if created.Record.UserID != owner.ID {
		t.Fatalf("token belongs to user %d, want alex (%d)", created.Record.UserID, owner.ID)
	}

	// The token authenticates as that account, not as the administrator.
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, f.ts.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("me with token: %v %v", res, err)
	}
	defer res.Body.Close()

	// An administrator sees and revokes tokens of any account.
	var list []*store.APIToken
	f.call(http.MethodGet, "/tokens?user=alex", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Name != "for-alex" {
		t.Fatalf("tokens of alex: %+v", list)
	}
	f.call(http.MethodDelete, "/tokens/"+itoa(created.Record.ID), nil, http.StatusNoContent, nil)
	f.call(http.MethodGet, "/tokens?user=alex", nil, http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("token survived revocation: %+v", list)
	}

	f.call(http.MethodPost, "/tokens", map[string]any{"name": "ghost", "user": "nobody"}, http.StatusUnprocessableEntity, nil)
}

// A user may only ever mint tokens for themselves.
func TestTokenUserCannotActForOthers(t *testing.T) {
	f := newSiteFixture(t)
	h, _ := auth.HashPassword("alex-password")
	owner, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserPassword(f.ctx, owner.ID, h); err != nil {
		t.Fatal(err)
	}
	f.loginAs("alex", "alex-password")

	f.call(http.MethodPost, "/tokens", map[string]any{"name": "escalate", "user": "admin"}, http.StatusForbidden, nil)

	var created struct{ Record *store.APIToken }
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "mine"}, http.StatusCreated, &created)
	if created.Record.UserID != owner.ID {
		t.Fatalf("own token stored for user %d, want %d", created.Record.UserID, owner.ID)
	}

	// Someone else's token stays out of reach.
	var admin *store.User
	if admin, err = f.db.GetUserByLogin(f.ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	other := &store.APIToken{UserID: admin.ID, Name: "admin-token", Hash: auth.HashToken("plain-value-for-test")}
	if err := f.db.CreateAPIToken(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodDelete, "/tokens/"+itoa(other.ID), nil, http.StatusNotFound, nil)
	f.call(http.MethodGet, "/tokens?user=admin", nil, http.StatusForbidden, nil)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
