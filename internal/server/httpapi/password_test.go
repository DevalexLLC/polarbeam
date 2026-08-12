package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/auth"
)

const changePath = "/api/v1/auth/password"

func changeBody(current, new string) string {
	return fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, current, new)
}

func TestPasswordChange(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookieA, csrfA := loginRole(t, h, f, "alice", "viewer")
	alice := f.users["alice"]

	// A second session — another browser — that the change must sign out.
	wB := doLogin(t, h, "alice", "hunter22222")
	if wB.Code != http.StatusOK {
		t.Fatalf("second login = %d: %s", wB.Code, wB.Body)
	}
	cookieB := wB.Result().Cookies()[0]
	if f.sessionCountFor(alice.ID) != 2 {
		t.Fatalf("alice sessions = %d, want 2", f.sessionCountFor(alice.ID))
	}

	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookieA, csrfA); w.Code != http.StatusOK {
		t.Fatalf("change = %d: %s", w.Code, w.Body)
	}
	if ok, err := auth.VerifyPassword("correct-horse-battery", alice.PasswordHash); err != nil || !ok {
		t.Errorf("new password does not verify: ok=%v err=%v", ok, err)
	}

	// The session that proved the current password survives; the other is gone.
	if w := doConfig(t, h, "GET", "/api/v1/auth/me", "", cookieA, ""); w.Code != http.StatusOK {
		t.Errorf("me on current session after change = %d, want 200: %s", w.Code, w.Body)
	}
	if w := doConfig(t, h, "GET", "/api/v1/auth/me", "", cookieB, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("me on other session after change = %d, want 401", w.Code)
	}

	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusUnauthorized {
		t.Errorf("old-password login after change = %d, want 401", w.Code)
	}
	if w := doLogin(t, h, "alice", "correct-horse-battery"); w.Code != http.StatusOK {
		t.Errorf("new-password login = %d: %s", w.Code, w.Body)
	}
}

func TestPasswordChangeWrongCurrent(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "alice", "viewer")
	alice := f.users["alice"]
	oldHash := alice.PasswordHash

	w := doConfig(t, h, "PUT", changePath, changeBody("not-my-password", "correct-horse-battery"), cookie, csrf)
	// 403, never 401: the SPA logs the user out on any 401, and a typo must
	// not cost the session.
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong current = %d, want 403: %s", w.Code, w.Body)
	}
	if msg := errBody(t, w); !strings.Contains(msg, "current password is incorrect") {
		t.Errorf("error = %q", msg)
	}
	if alice.PasswordHash != oldHash {
		t.Error("hash changed despite wrong current password")
	}
	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusOK {
		t.Errorf("original password login = %d: %s", w.Code, w.Body)
	}
}

func TestPasswordChangeValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "alice", "viewer")

	// Both problems reported at once; too-short pinned to the shared minimum.
	w := doConfig(t, h, "PUT", changePath, `{"current_password":"","new_password":""}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty fields = %d, want 400: %s", w.Code, w.Body)
	}
	msg := errBody(t, w)
	for _, want := range []string{"current_password is required", "new_password is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	w = doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "short"), cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("short new password = %d, want 400: %s", w.Code, w.Body)
	}
	if msg := errBody(t, w); !strings.Contains(msg, fmt.Sprintf("at least %d characters", auth.MinPasswordLen)) {
		t.Errorf("error = %q", msg)
	}
	// The minimum counts characters, not bytes: two 4-byte emoji are 8
	// bytes but 2 characters.
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "😀😀"), cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("2-emoji new password = %d, want 400: %s", w.Code, w.Body)
	}
	// Unknown fields are client bugs, never dropped.
	if w := doConfig(t, h, "PUT", changePath, `{"current_password":"a","new_password":"longenough","role":"admin"}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
	// Validation failures must not have burned the limiter.
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("change after validation noise = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestPasswordChangeAuth(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginRole(t, h, f, "alice", "viewer")

	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon change = %d, want 401", w.Code)
	}
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("change without CSRF = %d, want 403", w.Code)
	}
}

func TestPasswordChangeFederated(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	fed := f.addOIDCUser("https://idp.example", "sub-1", "fed", "viewer", false)
	// Hand-mint a federated session: the OIDC callback flow is exercised in
	// oidc_test.go; here only the session's auth_source matters.
	if err := f.CreateSession(t.Context(), fed.ID, auth.HashToken("fed-token"), "fed-csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: "fed-token"}

	w := doConfig(t, h, "PUT", changePath, changeBody("anything", "correct-horse-battery"), cookie, "fed-csrf")
	if w.Code != http.StatusConflict {
		t.Fatalf("federated change = %d, want 409: %s", w.Code, w.Body)
	}
	if msg := errBody(t, w); !strings.Contains(msg, "identity provider") {
		t.Errorf("error %q should name the identity provider", msg)
	}
}

// TestPasswordChangeConcurrentReset pins the stale-verification guard: a
// change verified against the OLD hash must not land after an admin reset
// replaced it — that would hand the account back to whoever held the old
// (presumed compromised) credential.
func TestPasswordChangeConcurrentReset(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "alice", "viewer")
	alice := f.users["alice"]

	// An admin reset lands between the handler's verification and the
	// store transaction.
	resetHash, err := auth.HashPassword("admin-reset-secret")
	if err != nil {
		t.Fatal(err)
	}
	f.beforeUpdateOwnPassword = func() { alice.PasswordHash = resetHash }

	w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "attacker-chosen-pw"), cookie, csrf)
	if w.Code != http.StatusConflict {
		t.Fatalf("raced change = %d, want 409: %s", w.Code, w.Body)
	}
	if alice.PasswordHash != resetHash {
		t.Error("stale change overwrote the admin reset")
	}
}

// TestLoginConcurrentPasswordChange pins the login-side race guard: a login
// verified against the OLD password must not mint a session after a rotation
// revoked that credential — the session insert revalidates the hash.
func TestLoginConcurrentPasswordChange(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	f.addUser("alice", "hunter22222", "viewer", false)
	alice := f.users["alice"]

	// A reset lands between login's verification and the session insert.
	resetHash, err := auth.HashPassword("admin-reset-secret")
	if err != nil {
		t.Fatal(err)
	}
	f.beforeCreateLocalSession = func() { alice.PasswordHash = resetHash }

	w := doLogin(t, h, "alice", "hunter22222")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("raced login = %d, want 401: %s", w.Code, w.Body)
	}
	// Byte-identical to a plain bad-credentials failure.
	f.beforeCreateLocalSession = nil
	plain := doLogin(t, h, "alice", "wrong-password")
	if w.Body.String() != plain.Body.String() {
		t.Errorf("raced-login and bad-password bodies differ: %q vs %q", w.Body, plain.Body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("raced login must not set cookies")
	}
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none", len(f.sessions))
	}
}

// TestPasswordChangeRateLimit pins the oracle guard: per-account budget,
// isolated from the login limiter in both directions and between users.
func TestPasswordChangeRateLimit(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "alice", "viewer")
	bobCookie, bobCSRF := loginRole(t, h, f, "bob", "viewer")

	var last *httptest.ResponseRecorder
	for range loginLimit {
		last = doConfig(t, h, "PUT", changePath, changeBody("wrong-guess", "correct-horse-battery"), cookie, csrf)
	}
	if last.Code != http.StatusForbidden {
		t.Fatalf("attempt %d = %d, want 403", loginLimit, last.Code)
	}
	// Over budget — even the right password is refused now.
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookie, csrf); w.Code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429: %s", loginLimit+1, w.Code, w.Body)
	}

	// Per-account keying: alice's exhaustion never touches bob.
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), bobCookie, bobCSRF); w.Code != http.StatusOK {
		t.Errorf("bob change during alice exhaustion = %d, want 200: %s", w.Code, w.Body)
	}
	// And it never burns a login attempt (break-glass isolation).
	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusOK {
		t.Errorf("login after change-limiter exhaustion = %d, want 200: %s", w.Code, w.Body)
	}
}

// TestPasswordChangeUnaffectedByLoginLimiter pins the reverse direction: a
// credential-stuffing wave against the login endpoint must not lock an
// authenticated user out of rotating their password.
func TestPasswordChangeUnaffectedByLoginLimiter(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "alice", "viewer")

	var last *httptest.ResponseRecorder
	for range loginLimit + 1 {
		last = doLogin(t, h, "nobody", "irrelevant")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("login limiter not exhausted: %d", last.Code)
	}
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("change during login exhaustion = %d, want 200: %s", w.Code, w.Body)
	}
}
