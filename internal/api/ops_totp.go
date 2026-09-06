package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/pquerna/otp/totp"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

type totpSetupOutput struct {
	Body apitypes.TOTPSetup
}

type totpCodeInput struct {
	Body apitypes.TOTPCode
}

type totpDisableInput struct {
	Body apitypes.TOTPDisable
}

type totpStatusOutput struct {
	Body apitypes.TOTPStatus
}

func (s *Server) totpEnabled(ctx context.Context, userID int64) bool {
	_, enabled, err := s.db.TOTP(ctx, userID)
	return err == nil && enabled
}

// verifyTOTP checks a code against the user's stored secret.
func (s *Server) verifyTOTP(ctx context.Context, userID int64, code string) bool {
	if s.secrets == nil {
		return false
	}
	enc, enabled, err := s.db.TOTP(ctx, userID)
	if err != nil || !enabled {
		return false
	}
	secret, err := s.secrets.Decrypt(enc)
	if err != nil {
		return false
	}
	return totp.Validate(strings.ReplaceAll(code, " ", ""), secret)
}

func (s *Server) registerTOTP() {
	huma.Register(s.api, huma.Operation{
		OperationID: "totp-status", Method: http.MethodGet, Path: "/auth/totp", Summary: "Whether 2FA is enabled for the caller", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*totpStatusOutput, error) {
		p := principalFrom(ctx)
		return &totpStatusOutput{Body: apitypes.TOTPStatus{Enabled: p.UserID != 0 && s.totpEnabled(ctx, p.UserID)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "totp-setup", Method: http.MethodPost, Path: "/auth/totp/setup", Summary: "Generate a new TOTP secret (not active until confirmed)", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*totpSetupOutput, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return nil, huma.Error422UnprocessableEntity("2FA belongs to panel accounts, not to root over the socket")
		}
		if s.secrets == nil {
			return nil, huma.Error422UnprocessableEntity("secret key unavailable")
		}
		key, err := totp.Generate(totp.GenerateOpts{Issuer: "MonoPanel " + s.cfg.Web.Hostname, AccountName: p.Login})
		if err != nil {
			return nil, err
		}
		enc, err := s.secrets.Encrypt(key.Secret())
		if err != nil {
			return nil, err
		}
		if err := s.db.SetTOTP(ctx, p.UserID, enc, false); err != nil {
			return nil, err
		}
		return &totpSetupOutput{Body: apitypes.TOTPSetup{Secret: key.Secret(), URL: key.URL()}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "totp-enable", Method: http.MethodPost, Path: "/auth/totp/enable", Summary: "Confirm the secret with a code and enable 2FA", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, in *totpCodeInput) (*totpStatusOutput, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 || s.secrets == nil {
			return nil, huma.Error422UnprocessableEntity("2FA unavailable for this principal")
		}
		enc, _, err := s.db.TOTP(ctx, p.UserID)
		if err != nil || enc == "" {
			return nil, huma.Error422UnprocessableEntity("run setup first")
		}
		secret, err := s.secrets.Decrypt(enc)
		if err != nil {
			return nil, err
		}
		if !totp.Validate(strings.ReplaceAll(in.Body.Code, " ", ""), secret) {
			return nil, huma.Error401Unauthorized("invalid code")
		}
		if err := s.db.SetTOTP(ctx, p.UserID, enc, true); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "totp.enable", IP: requestInfo(ctx).IP})
		return &totpStatusOutput{Body: apitypes.TOTPStatus{Enabled: true}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "totp-disable", Method: http.MethodPost, Path: "/auth/totp/disable", Summary: "Disable 2FA (password required)", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, in *totpDisableInput) (*totpStatusOutput, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return nil, huma.Error422UnprocessableEntity("2FA unavailable for this principal")
		}
		u, err := s.db.GetUserByID(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		if ok, _ := auth.VerifyPassword(in.Body.Password, u.PasswordHash); !ok {
			return nil, huma.Error401Unauthorized("invalid password")
		}
		if err := s.db.SetTOTP(ctx, p.UserID, "", false); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "totp.disable", IP: requestInfo(ctx).IP})
		return &totpStatusOutput{Body: apitypes.TOTPStatus{Enabled: false}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "users-totp-reset", Method: http.MethodPost, Path: "/users/{login}/totp/reset", Summary: "Admin: remove a user's 2FA (account recovery)", Tags: []string{"users"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *loginInputPath) (*totpStatusOutput, error) {
		p := principalFrom(ctx)
		u, err := s.db.GetUserByLogin(ctx, in.Login)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("user not found")
		}
		if err != nil {
			return nil, err
		}
		if err := s.db.SetTOTP(ctx, u.ID, "", false); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "totp.reset", Target: u.Login, IP: requestInfo(ctx).IP})
		return &totpStatusOutput{Body: apitypes.TOTPStatus{Enabled: false}}, nil
	})
}
