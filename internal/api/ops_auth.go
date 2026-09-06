package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

const sessionTTL = 7 * 24 * time.Hour

type loginInput struct {
	Body apitypes.LoginRequest
}

type loginOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      apitypes.LoginResponse
}

type logoutOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

type meOutput struct {
	Body apitypes.Principal
}

func (s *Server) registerAuth() {
	huma.Register(s.api, huma.Operation{
		OperationID: "auth-login", Method: http.MethodPost, Path: "/auth/login", Summary: "Log in with login and password", Tags: []string{"auth"},
	}, func(ctx context.Context, in *loginInput) (*loginOutput, error) {
		info := requestInfo(ctx)
		if !s.limiter.allow(info.IP) {
			return nil, huma.Error429TooManyRequests("too many login attempts; try again in a minute")
		}
		u, err := s.db.GetUserByLogin(ctx, in.Body.Login)
		if err != nil || u.PasswordHash == "" {
			auth.VerifyPassword(in.Body.Password, "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") // equalise timing
			s.db.Audit(ctx, store.AuditEntry{Actor: in.Body.Login, Action: "auth.login", IP: info.IP, Result: "denied"})
			s.log.Warn("login denied", "login", in.Body.Login, "ip", info.IP)
			return nil, huma.Error401Unauthorized("invalid login or password")
		}
		ok, err := auth.VerifyPassword(in.Body.Password, u.PasswordHash)
		if err != nil || !ok || u.Status != store.UserActive {
			s.db.Audit(ctx, store.AuditEntry{Actor: u.Login, Action: "auth.login", IP: info.IP, Result: "denied"})
			s.log.Warn("login denied", "login", u.Login, "ip", info.IP)
			return nil, huma.Error401Unauthorized("invalid login or password")
		}
		if s.totpEnabled(ctx, u.ID) {
			if in.Body.Code == "" {
				return nil, huma.Error401Unauthorized("totp_required")
			}
			if !s.verifyTOTP(ctx, u.ID, in.Body.Code) {
				s.db.Audit(ctx, store.AuditEntry{Actor: u.Login, Action: "auth.totp", IP: info.IP, Result: "denied"})
				s.log.Warn("login denied", "login", u.Login, "ip", info.IP, "reason", "totp")
				return nil, huma.Error401Unauthorized("invalid one-time code")
			}
		}
		id, err := auth.NewToken(32)
		if err != nil {
			return nil, err
		}
		sess := &store.Session{ID: id, UserID: u.ID, ExpiresAt: time.Now().Add(sessionTTL), IP: info.IP, UA: info.UA}
		if err := s.db.CreateSession(ctx, sess); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: u.Login, Action: "auth.login", IP: info.IP})
		out := &loginOutput{SetCookie: sessionCookie(id, sess.ExpiresAt, info.TLS)}
		out.Body.Principal = apitypes.Principal{UserID: u.ID, Login: u.Login, Role: u.Role, Via: "session"}
		out.Body.User = u
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "auth-logout", Method: http.MethodPost, Path: "/auth/logout", Summary: "Log out", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*logoutOutput, error) {
		p := principalFrom(ctx)
		if p.SessionID != "" {
			s.db.DeleteSession(ctx, p.SessionID)
		}
		return &logoutOutput{SetCookie: sessionCookie("", time.Unix(0, 0), requestInfo(ctx).TLS)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "auth-me", Method: http.MethodGet, Path: "/auth/me", Summary: "Who am I", Tags: []string{"auth"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		return &meOutput{Body: principalFrom(ctx).Principal}, nil
	})
}

func sessionCookie(value string, expires time.Time, secure bool) http.Cookie {
	c := http.Cookie{Name: sessionCookieName, Value: value, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, Expires: expires}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}
