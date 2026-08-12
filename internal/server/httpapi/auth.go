package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

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
	// AuthSource ("local" or "oidc") lets the SPA hide password management
	// for federated accounts, whose credential lives at the IdP.
	AuthSource string `json:"auth_source"`
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
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		if isBodyTooLarge(w, err) {
			return
		}
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	// Exactly one JSON value, like decodeStrict: a single Decode stops at
	// the object's end, so without this read-to-EOF a request with no
	// declared Content-Length could smuggle unbounded trailing data past
	// the body cap.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
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

	// The session insert revalidates the hash this request verified: a
	// password reset or change landing after VerifyPassword revokes every
	// session of the old credential, and minting unchecked would hand the
	// old (presumed leaked) password a session that outlives the rotation.
	create := func(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time) error {
		return a.db.CreateLocalSession(ctx, userID, tokenHash, csrfToken, expiresAt, hash)
	}
	csrf, err := a.issueSession(w, r, user, create)
	if err != nil {
		if errors.Is(err, store.ErrPasswordChanged) {
			// Byte-identical to the other credential failures.
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		internalError(w, "issue session", err)
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		User:      userJSON{Username: user.Username, Role: user.Role, AuthSource: user.AuthSource},
		CSRFToken: csrf,
		Version:   version.String(),
	})
}

// sessionCreator inserts the session row. Both logins pass closures that
// revalidate their credential inside the insert, because verification and
// insert can straddle a revocation: local login closes over the verified
// hash (CreateLocalSession — a password rotation revokes old sessions),
// the OIDC callback over the provider identity (CreateOIDCSession — the
// code exchange can straddle a provider switch).
type sessionCreator func(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time) error

// issueSession mints a session for an already-authenticated user and sets
// the session cookie; local login and the OIDC callback share it (the
// callback redirects instead of writing JSON, so the response body is the
// caller's). The Strict cookie is fine even on the callback's cross-site
// redirect: browsers accept Set-Cookie there, and nothing before the SPA's
// same-site /api fetches needs the cookie sent.
func (a *api) issueSession(w http.ResponseWriter, r *http.Request, user *store.UserInfo, create sessionCreator) (csrf string, err error) {
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
	if err := create(r.Context(), user.ID, tokenHash, csrf, expires); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	// Audit metrics must never lock an authenticated user out; the session
	// is already committed (same posture as the cleanup warn above).
	if err := a.db.RecordLogin(r.Context(), user.ID); err != nil {
		slog.Warn("httpapi: record login", "err", err)
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
		User:      userJSON{Username: s.Username, Role: s.Role, AuthSource: s.AuthSource},
		CSRFToken: s.CSRFToken,
		Version:   version.String(),
	})
}

// handlePasswordChange lets a local user rotate their own password. The
// current password is required — a hijacked session must not be able to
// silently take over the credential — and its verification is a password
// oracle, so attempts are rate-limited per account. On success every OTHER
// session of the user is deleted (the change-because-compromised case is
// the one that matters); the current session survives.
func (a *api) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	// Refused before body decode and before the limiter: federated accounts
	// have no password by schema, and the refusal must be explicit — never
	// a silent no-op.
	if s.AuthSource != "local" {
		writeError(w, http.StatusConflict, "federated accounts authenticate at the identity provider")
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	var problems []string
	if req.CurrentPassword == "" {
		problems = append(problems, "current_password is required")
	}
	if req.NewPassword == "" {
		problems = append(problems, "new_password is required")
	} else if utf8.RuneCountInString(req.NewPassword) < auth.MinPasswordLen {
		// Runes, not bytes: the policy says characters, and byte counting
		// would let two 4-byte emoji pass an 8-character minimum.
		problems = append(problems, fmt.Sprintf("new_password must be at least %d characters", auth.MinPasswordLen))
	}
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	// Only actual verification attempts burn the limiter; a fumbled request
	// body above should not lock anyone out.
	if !a.pwLimiter.allow(s.UserID.String()) {
		writeError(w, http.StatusTooManyRequests, "too many password attempts; try again in a minute")
		return
	}

	user, err := a.db.GetUserByID(r.Context(), s.UserID)
	if err != nil {
		internalError(w, "password change lookup", err)
		return
	}
	if user == nil {
		// The account was deleted after this session was validated.
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if user.PasswordHash == "" {
		// Unreachable for a local account (users_auth_shape); guard anyway
		// so an empty hash never reaches VerifyPassword as a malformed PHC.
		internalError(w, "password change", fmt.Errorf("local user %s has no password hash", s.UserID))
		return
	}
	ok, err := auth.VerifyPassword(req.CurrentPassword, user.PasswordHash)
	if err != nil {
		internalError(w, "verify password", err)
		return
	}
	if !ok {
		// 403, not 401: the SPA treats 401 as session death and would log
		// the user out over a typo.
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		internalError(w, "hash password", err)
		return
	}
	// The verified hash rides along so the store can refuse a stale update:
	// if the password changed between verification and the transaction (an
	// admin reset landing mid-request), this must fail, not overwrite it.
	if err := a.db.UpdateOwnPassword(r.Context(), s.UserID, user.PasswordHash, hash, s.ID); err != nil {
		writeStoreError(w, "update password", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
