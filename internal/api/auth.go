package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/peercred"
	"monopanel/internal/store"
)

const sessionCookieName = "mp_session"

type principalKey struct{}

// principal carries the authenticated caller plus session bookkeeping.
type principal struct {
	apitypes.Principal
	SessionID string
}

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	return p
}

func (s *Server) authMiddleware(ctx huma.Context, next func(huma.Context)) {
	p := s.authenticate(ctx)
	if p != nil {
		ctx = huma.WithValue(ctx, principalKey{}, p)
	}
	op := ctx.Operation()
	if len(op.Security) > 0 {
		if p == nil {
			huma.WriteErr(s.api, ctx, http.StatusUnauthorized, "authentication required") //nolint:errcheck // the error response itself is best effort
			return
		}
		if admin, _ := op.Metadata["admin"].(bool); admin && p.Role != store.RoleAdmin {
			huma.WriteErr(s.api, ctx, http.StatusForbidden, "administrator role required") //nolint:errcheck // the error response itself is best effort
			return
		}
		// Токен переезда не должен уметь ничего, кроме отдачи того аккаунта,
		// ради которого его выпустили. Прочие области панель по-прежнему
		// считает пометками: токены с ними выпускались как обычные.
		if !scopeAllowsOperation(p.Scopes, op) {
			huma.WriteErr(s.api, ctx, http.StatusForbidden, "token scope does not allow this operation") //nolint:errcheck // the error response itself is best effort
			return
		}
		if p.Via == "session" && !isSafeMethod(ctx.Method()) && !sameOrigin(ctx) {
			huma.WriteErr(s.api, ctx, http.StatusForbidden, "cross-site request rejected") //nolint:errcheck // the error response itself is best effort
			return
		}
	}
	next(ctx)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin rejects session-authenticated mutations from other origins.
// The cookie is SameSite=Strict already; this is defence in depth.
func sameOrigin(ctx huma.Context) bool {
	if o := ctx.Header("Origin"); o != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
		return strings.EqualFold(host, ctx.Host())
	}
	if site := ctx.Header("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	return true
}

func (s *Server) authenticate(ctx huma.Context) *principal {
	rctx := ctx.Context()
	if cred, ok := peercred.FromContext(rctx); ok {
		if cred.UID == 0 {
			return &principal{Principal: apitypes.Principal{Login: "root", Role: store.RoleAdmin, Via: "peercred"}}
		}
		if u, err := s.db.GetUserByUnixUID(rctx, int(cred.UID)); err == nil && u.Status == store.UserActive {
			return &principal{Principal: apitypes.Principal{UserID: u.ID, Login: u.Login, Role: u.Role, Via: "peercred"}}
		}
		return nil
	}
	if h := ctx.Header("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok, err := s.db.GetAPITokenByHash(rctx, auth.HashToken(strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))))
		if err != nil {
			return nil
		}
		u, err := s.db.GetUserByID(rctx, tok.UserID)
		if err != nil || u.Status != store.UserActive {
			return nil
		}
		go s.db.TouchAPIToken(context.Background(), tok.ID) //nolint:errcheck // last-used bookkeeping must not fail the request
		return &principal{Principal: apitypes.Principal{UserID: u.ID, Login: u.Login, Role: u.Role, Via: "token", Scopes: tok.Scopes}}
	}
	if c, err := huma.ReadCookie(ctx, sessionCookieName); err == nil && c.Value != "" {
		sess, err := s.db.GetSession(rctx, c.Value)
		if err != nil {
			return nil
		}
		u, err := s.db.GetUserByID(rctx, sess.UserID)
		if err != nil || u.Status != store.UserActive {
			return nil
		}
		return &principal{Principal: apitypes.Principal{UserID: u.ID, Login: u.Login, Role: u.Role, Via: "session"}, SessionID: sess.ID}
	}
	return nil
}

// scopeAllowsOperation decides what a scoped token may reach. Only migration
// scopes are enforced: "migrate:user:alex" opens the read-only endpoints under
// /migrate and closes everything else. The match against the requested account
// happens in the handler, which sees the query. Scopes the panel does not know
// stay what they always were — a label on the token.
func scopeAllowsOperation(scopes []string, op *huma.Operation) bool {
	migration := false
	for _, sc := range scopes {
		if strings.HasPrefix(sc, migrateScopePrefix) {
			migration = true
		}
	}
	if !migration {
		return true
	}
	return strings.HasPrefix(op.Path, "/migrate") && op.Method == http.MethodGet
}

// loginLimiter is a tiny per-IP fixed-window limiter for the login endpoint.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*hitWindow
}

type hitWindow struct {
	start time.Time
	n     int
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, hits: map[string]*hitWindow{}}
}

func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w := l.hits[key]
	if w == nil || now.Sub(w.start) > l.window {
		w = &hitWindow{start: now}
		l.hits[key] = w
		if len(l.hits) > 10000 {
			for k, v := range l.hits {
				if now.Sub(v.start) > l.window {
					delete(l.hits, k)
				}
			}
		}
	}
	w.n++
	return w.n <= l.max
}
