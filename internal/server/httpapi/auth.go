package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/version"
)

const (
	loginLimit  = 10
	loginWindow = time.Minute
	// touchInterval rate-limits last_used_at writes: dashboard polling
	// would otherwise turn every page refresh into a session UPDATE.
	touchInterval = 5 * time.Minute
)

type userJSON struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// loginResponse is shared by handleLogin and handleMe, so the dashboard
// learns the server build on both the fresh-login and session-restore paths.
// Both are post-authentication: the open auth/providers endpoint the login
// screen calls deliberately carries no version.
type loginResponse struct {
	User      userJSON `json:"user"`
	CSRFToken string   `json:"csrf_token"`
	Version   string   `json:"version"`
}

// handleLogin authenticates a username/password and mints a session.
// Unknown-user and wrong-password responses are byte-identical, and unknown
// users still burn an argon2 verification so timing is uniform too.
func (a *api) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again in a minute")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isBodyTooLarge(w, err) {
			return
		}
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	user, err := a.db.GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		internalError(w, "login lookup", err)
		return
	}
	// Federated (OIDC) users have no password hash; they must fall into the
	// same dummy-hash burn and byte-identical 401 as unknown users — an
	// empty hash reaching VerifyPassword would be a malformed-PHC 500.
	hash := auth.DummyHash
	if user != nil && !user.Disabled && user.PasswordHash != "" {
		hash = user.PasswordHash
	}
	ok, err := auth.VerifyPassword(req.Password, hash)
	if err != nil {
		internalError(w, "verify password", err)
		return
	}
	if !ok || user == nil || user.Disabled || user.PasswordHash == "" {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	csrf, err := a.issueSession(w, r, user)
	if err != nil {
		internalError(w, "issue session", err)
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		User:      userJSON{Username: user.Username, Role: user.Role},
		CSRFToken: csrf,
		Version:   version.String(),
	})
}

// issueSession mints a session for an already-authenticated user and sets
// the session cookie; local login and the OIDC callback share it (the
// callback redirects instead of writing JSON, so the response body is the
// caller's). The Strict cookie is fine even on the callback's cross-site
// redirect: browsers accept Set-Cookie there, and nothing before the SPA's
// same-site /api fetches needs the cookie sent.
func (a *api) issueSession(w http.ResponseWriter, r *http.Request, user *store.UserInfo) (csrf string, err error) {
	// Opportunistic cleanup keeps the sessions table bounded without a
	// background job; expired rows are invisible to lookups either way.
	if n, err := a.db.DeleteExpiredSessions(r.Context()); err != nil {
		slog.Warn("httpapi: delete expired sessions", "err", err)
	} else if n > 0 {
		slog.Debug("httpapi: cleaned expired sessions", "count", n)
	}

	token, tokenHash, err := auth.NewToken()
	if err != nil {
		return "", fmt.Errorf("mint session token: %w", err)
	}
	csrf, _, err = auth.NewToken()
	if err != nil {
		return "", fmt.Errorf("mint csrf token: %w", err)
	}
	expires := time.Now().Add(sessionTTL)
	if err := a.db.CreateSession(r.Context(), user.ID, tokenHash, csrf, expires); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	return csrf, nil
}

// handleLogout deletes the session and clears the cookie. Idempotent.
func (a *api) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := a.db.DeleteSessionByTokenHash(r.Context(), auth.HashToken(c.Value)); err != nil {
			internalError(w, "delete session", err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMe restores the SPA's view of the session (user + CSRF token).
func (a *api) handleMe(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, loginResponse{
		User:      userJSON{Username: s.Username, Role: s.Role},
		CSRFToken: s.CSRFToken,
		Version:   version.String(),
	})
}

// withSession authenticates the request's session cookie, enforces CSRF on
// non-GET methods, and stores the session in the request context.
func (a *api) withSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		s, err := a.db.GetSessionByTokenHash(r.Context(), auth.HashToken(c.Value))
		if err != nil {
			internalError(w, "session lookup", err)
			return
		}
		if s == nil {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			got := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.CSRFToken)) != 1 {
				writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
				return
			}
		}
		if time.Since(s.LastUsedAt) > touchInterval {
			if err := a.db.TouchSession(r.Context(), s.ID); err != nil {
				slog.Warn("httpapi: touch session", "err", err)
			}
		}
		next.ServeHTTP(w, r.WithContext(withSessionCtx(r.Context(), s)))
	})
}

// requireRole guards a handler behind a role; PUT /api/v1/settings mounts
// behind requireRole("admin"). Compose inside withSession — it reads the
// session context withSession populates.
func requireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := sessionFrom(r.Context()); s == nil || s.Role != role {
			writeError(w, http.StatusForbidden, "requires "+role+" role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the rate-limit key. RemoteAddr is the real client address only
// proxy-less or when listen.proxy_protocol makes the listener adopt the
// PROXY-header source; behind a passthrough proxy without it, every request
// carries the proxy's address and this limiter degenerates into one global
// bucket for the whole deployment — which is why the shipped compose configs
// enable the knob. X-Forwarded-For can never exist here (the proxy does not
// terminate TLS), so there is no header to trust or spoof.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter is a fixed-window per-IP counter. Windows reset lazily; the
// map is pruned on each reset so it cannot grow past one entry per active
// IP per window.
type loginLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	start    time.Time
	attempts map[string]int
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{limit: limit, window: window, start: time.Now(), attempts: map[string]int{}}
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.start) > l.window {
		l.start = time.Now()
		clear(l.attempts)
	}
	l.attempts[ip]++
	return l.attempts[ip] <= l.limit
}
