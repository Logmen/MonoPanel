package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

type tokensOutput struct {
	Body []*store.APIToken
}

type createTokenInput struct {
	Body apitypes.CreateTokenRequest
}

type createTokenOutput struct {
	Status int
	Body   apitypes.CreateTokenResponse
}

type tokenIDInput struct {
	ID int64 `path:"id" minimum:"1"`
}

type listTokensInput struct {
	User string `query:"user" doc:"Account whose tokens to list (administrators only)"`
}

// tokenOwner resolves which account a token belongs to. A normal user always
// gets their own; an administrator may name another account. Root on the local
// socket has no account at all, so it must name one — unless the panel has a
// single administrator, which is the usual case and is then used implicitly.
func (s *Server) tokenOwner(ctx context.Context, p *principal, login string) (*store.User, error) {
	if login != "" {
		if p.Role != store.RoleAdmin {
			return nil, huma.Error403Forbidden("only administrators can act for another account")
		}
		u, err := s.db.GetUserByLogin(ctx, login)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error422UnprocessableEntity("no such account: " + login)
		}
		return u, err
	}
	if p.UserID != 0 {
		return s.db.GetUserByID(ctx, p.UserID)
	}
	// Local socket as root: pick the administrator when there is exactly one.
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	admins := make([]*store.User, 0, 2)
	for _, u := range users {
		if u.Role == store.RoleAdmin {
			admins = append(admins, u)
		}
	}
	switch len(admins) {
	case 1:
		return admins[0], nil
	case 0:
		return nil, huma.Error422UnprocessableEntity("the panel has no administrator account yet")
	default:
		names := make([]string, 0, len(admins))
		for _, u := range admins {
			names = append(names, u.Login)
		}
		return nil, huma.Error422UnprocessableEntity("several administrators exist, name one with user: " + strings.Join(names, ", "))
	}
}

func (s *Server) registerTokens() {
	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-list", Method: http.MethodGet, Path: "/tokens", Summary: "List API tokens (administrators may pass ?user=)", Tags: []string{"tokens"}, Security: secured,
	}, func(ctx context.Context, in *listTokensInput) (*tokensOutput, error) {
		p := principalFrom(ctx)
		owner, err := s.tokenOwner(ctx, p, in.User)
		if err != nil {
			return nil, err
		}
		list, err := s.db.ListAPITokens(ctx, owner.ID)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.APIToken{}
		}
		return &tokensOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-create", Method: http.MethodPost, Path: "/tokens", Summary: "Create an API token (shown once); administrators may issue one for another account with \"user\"", Tags: []string{"tokens"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createTokenInput) (*createTokenOutput, error) {
		p := principalFrom(ctx)
		owner, err := s.tokenOwner(ctx, p, in.Body.User)
		if err != nil {
			return nil, err
		}
		plain, err := auth.NewToken(32)
		if err != nil {
			return nil, err
		}
		rec := &store.APIToken{UserID: owner.ID, Name: in.Body.Name, Hash: auth.HashToken(plain), Scopes: in.Body.Scopes}
		if in.Body.ExpiresInDays > 0 {
			t := time.Now().Add(time.Duration(in.Body.ExpiresInDays) * 24 * time.Hour)
			rec.ExpiresAt = &t
		}
		if err := s.db.CreateAPIToken(ctx, rec); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "token.create", Target: in.Body.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"account": owner.Login}})
		return &createTokenOutput{Status: http.StatusCreated, Body: apitypes.CreateTokenResponse{Token: plain, Record: rec}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-delete", Method: http.MethodDelete, Path: "/tokens/{id}", Summary: "Revoke a token", Tags: []string{"tokens"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *tokenIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		scope := p.UserID
		if p.Role == store.RoleAdmin {
			scope = 0 // any token
		}
		err := s.db.DeleteAPIToken(ctx, in.ID, scope)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("token not found")
		}
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "token.revoke", IP: requestInfo(ctx).IP})
		return nil, nil
	})
}
