package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// GET /auth/me names the API token a request came with, as GET /tokens lists
// it, so mp whoami can say which token a script uses; a browser session has
// none.
func TestAuthMeNamesTheToken(t *testing.T) {
	f := newSiteFixture(t)
	var created struct {
		Token  string          `json:"token"`
		Record *store.APIToken `json:"record"`
	}
	f.call(http.MethodPost, "/tokens", map[string]any{"name": "billing-sync", "user": "alex", "scopes": []string{"billing"}}, http.StatusCreated, &created)

	req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, f.ts.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me apitypes.Principal
	if err := json.NewDecoder(res.Body).Decode(&me); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("me with a token: %d %v", res.StatusCode, err)
	}
	if me.Login != "alex" || me.Via != "token" || me.TokenID != created.Record.ID || me.TokenName != "billing-sync" || len(me.Scopes) != 1 || me.Scopes[0] != "billing" {
		t.Fatalf("me with a token: %+v (token #%d)", me, created.Record.ID)
	}

	var session apitypes.Principal
	f.call(http.MethodGet, "/auth/me", nil, http.StatusOK, &session)
	if session.Via != "session" || session.TokenID != 0 || session.TokenName != "" {
		t.Fatalf("me in a browser session: %+v", session)
	}
}
