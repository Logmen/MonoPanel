package api

import (
	"context"
	"errors"
	"net/http"
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

func (s *Server) registerTokens() {
	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-list", Method: http.MethodGet, Path: "/tokens", Summary: "List my API tokens", Tags: []string{"tokens"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*tokensOutput, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return &tokensOutput{Body: []*store.APIToken{}}, nil
		}
		list, err := s.db.ListAPITokens(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.APIToken{}
		}
		return &tokensOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-create", Method: http.MethodPost, Path: "/tokens", Summary: "Create an API token (shown once)", Tags: []string{"tokens"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createTokenInput) (*createTokenOutput, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return nil, huma.Error422UnprocessableEntity("tokens belong to panel accounts; root via the local socket has none. Use --user to act as an account")
		}
		plain, err := auth.NewToken(32)
		if err != nil {
			return nil, err
		}
		rec := &store.APIToken{UserID: p.UserID, Name: in.Body.Name, Hash: auth.HashToken(plain), Scopes: in.Body.Scopes}
		if in.Body.ExpiresInDays > 0 {
			t := time.Now().Add(time.Duration(in.Body.ExpiresInDays) * 24 * time.Hour)
			rec.ExpiresAt = &t
		}
		if err := s.db.CreateAPIToken(ctx, rec); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "token.create", Target: in.Body.Name, IP: requestInfo(ctx).IP})
		return &createTokenOutput{Status: http.StatusCreated, Body: apitypes.CreateTokenResponse{Token: plain, Record: rec}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "tokens-delete", Method: http.MethodDelete, Path: "/tokens/{id}", Summary: "Revoke a token", Tags: []string{"tokens"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *tokenIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		err := s.db.DeleteAPIToken(ctx, in.ID, p.UserID)
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
