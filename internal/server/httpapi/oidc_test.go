package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeProvider scripts the IdP boundary and records what the handlers pass.
type fakeProvider struct {
	claims      *oidcauth.Claims
	exchangeErr error

	gotAuthState, gotAuthNonce, gotAuthVerifier string
	gotCode, gotVerifier, gotNonce              string
}

func (p *fakeProvider) AuthCodeURL(state, nonce, verifier string) string {
	p.gotAuthState, p.gotAuthNonce, p.gotAuthVerifier = state, nonce, verifier
	return "https://idp.example/auth?state=" + state
}

func (p *fakeProvider) Exchange(_ context.Context, code, verifier, nonce string) (*oidcauth.Claims, error) {
	p.gotCode, p.gotVerifier, p.gotNonce = code, verifier, nonce
	if p.exchangeErr != nil {
		return nil, p.exchangeErr
	}
	return p.claims, nil
}

// fakeProviders implements OIDCProviders without any discovery.
type fakeProviders struct {
	provider *fakeProvider
	// settings returned alongside the provider (the row it was built from);
	// nil defaults to enabledSettings().
	settings    *store.OIDCSettings
	providerErr error
	testInfo    *oidcauth.DiscoveryInfo
	testErr     error
	invalidated int
}

func (f *fakeProviders) Provider(context.Context) (oidcauth.Provider, *store.OIDCSettings, error) {
	if f.providerErr != nil {
		return nil, nil, f.providerErr
	}
	s := f.settings
	if s == nil {
		s = enabledSettings()
	}
	return f.provider, s, nil
}

func (f *fakeProviders) Test(context.Context, store.OIDCSettings) (*oidcauth.DiscoveryInfo, error) {
	return f.testInfo, f.testErr
}

func (f *fakeProviders) Invalidate() { f.invalidated++ }

// testIssuer matches enabledSettings().Issuer: the issuer half of the
// (issuer, subject) identity key.
const testIssuer = "https://idp.example/realms/x"

func enabledSettings() *store.OIDCSettings {
	return &store.OIDCSettings{
		Enabled: true, Issuer: testIssuer, ClientID: "polarbeam",
		ClientSecret: "sekrit", RedirectURL: "https://dash.example/api/v1/auth/oidc/callback",
		Scopes: []string{"openid", "profile"}, UsernameClaim: "preferred_username",
		RoleClaim: "groups", AdminValues: []string{"polarbeam-admins"},
		UpdatedAt: time.Now(), UpdatedBy: "test",
	}
}

// adminAndCookie logs in an admin local user (loginAndCookie's alice is a
// viewer).
func adminAndCookie(t *testing.T, h http.Handler, f *fakeDB) (*http.Cookie, string) {
	t.Helper()
	f.addUser("root", "rootpw12345", "admin", false)
	w := doLogin(t, h, "root", "rootpw12345")
	if w.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d %s", w.Code, w.Body)
	}
	var res struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	return w.Result().Cookies()[0], res.CSRFToken
}

// --- providers advertisement ---

func TestAuthProvidersOpenAndShape(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)

	get := func() map[string]struct {
		Enabled bool `json:"enabled"`
	} {
		req := httptest.NewRequest("GET", "/api/v1/auth/providers", nil) // no session
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("providers = %d, want 200: %s", w.Code, w.Body)
		}
		var out map[string]struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad providers body: %v", err)
		}
		return out
	}

	if out := get(); out["oidc"].Enabled {
		t.Error("default settings must advertise oidc disabled")
	}
	f.oidcSettings = enabledSettings()
	if out := get(); !out["oidc"].Enabled {
		t.Error("enabled settings must advertise oidc enabled")
	}

	// This endpoint is unauthenticated, so it must stay a bare enabled flag.
	// The server build is deliberately post-auth only (auth/me carries it);
	// the login screen's attribution byline is static for that reason.
	req := httptest.NewRequest("GET", "/api/v1/auth/providers", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(strings.ToLower(w.Body.String()), "version") {
		t.Errorf("providers must not leak a version pre-authentication: %s", w.Body)
	}
}

// --- start ---

func startFlow(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/auth/oidc/start", nil)
	req.RemoteAddr = "203.0.113.9:4242"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestOIDCStartDisabled(t *testing.T) {
	h := newTestAPI(t, newFakeDB()) // provider manager returns ErrDisabled
	w := startFlow(t, h)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/#/sso-error=config" {
		t.Errorf("start while disabled = %d %q, want 303 to sso-error=config",
			w.Code, w.Header().Get("Location"))
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("failed start must not set cookies")
	}
}

func TestOIDCStartProviderError(t *testing.T) {
	h := newTestAPIWithProviders(t, newFakeDB(), &fakeProviders{providerErr: errors.New("boom")})
	w := startFlow(t, h)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/#/sso-error=provider" {
		t.Errorf("start with provider error = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestOIDCStartHappyPath(t *testing.T) {
	fp := &fakeProviders{provider: &fakeProvider{}}
	h := newTestAPIWithProviders(t, newFakeDB(), fp)
	w := startFlow(t, h)

	if w.Code != http.StatusFound {
		t.Fatalf("start = %d, want 302: %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1 state cookie", len(cookies))
	}
	c := cookies[0]
	if c.Name != oidcStateCookie || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/api/v1/auth/oidc" ||
		c.MaxAge != int(oidcStateTTL.Seconds()) {
		t.Errorf("state cookie flags wrong: %+v", c)
	}
	parts := strings.SplitN(c.Value, ".", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("state cookie value %q, want state.nonce.verifier", c.Value)
	}
	p := fp.provider
	if p.gotAuthState != parts[0] || p.gotAuthNonce != parts[1] || p.gotAuthVerifier != parts[2] {
		t.Error("AuthCodeURL params do not match the state cookie")
	}
	if loc := w.Header().Get("Location"); loc != "https://idp.example/auth?state="+parts[0] {
		t.Errorf("redirect location = %q", loc)
	}
}

// --- callback ---

func callback(t *testing.T, h http.Handler, query string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/auth/oidc/callback"+query, nil)
	req.RemoteAddr = "203.0.113.9:4242"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func stateCookie(value string) *http.Cookie {
	return &http.Cookie{Name: oidcStateCookie, Value: value}
}

func TestOIDCCallbackHappyPath(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	// The provider shares the STORED row (as the real manager's cache
	// does): the callback's writes are bound to its updated_at.
	fp := &fakeProviders{provider: &fakeProvider{
		claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "alice@corp", Role: "admin"},
	}, settings: f.oidcSettings}
	h := newTestAPIWithProviders(t, f, fp)

	w := callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q, want 303 to /: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if fp.provider.gotCode != "authcode" || fp.provider.gotVerifier != "ver" || fp.provider.gotNonce != "n1" {
		t.Errorf("exchange got (%q,%q,%q), want cookie-derived values",
			fp.provider.gotCode, fp.provider.gotVerifier, fp.provider.gotNonce)
	}

	var session, cleared *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			session = c
		case oidcStateCookie:
			cleared = c
		}
	}
	if session == nil || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie missing or flags wrong: %+v", session)
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("state cookie not cleared: %+v", cleared)
	}

	u := f.oidcUsers[oidcKey(testIssuer, "sub-1")]
	if u == nil || u.Username != "alice@corp" || u.Role != "admin" {
		t.Fatalf("JIT user = %+v", u)
	}

	if len(f.logins) != 1 || f.logins[0].AuthSource != "oidc" || f.logins[0].Role != "admin" || f.logins[0].UserID != u.ID {
		t.Errorf("recorded logins = %+v, want one oidc admin event for the JIT user", f.logins)
	}

	// The minted session works for /auth/me.
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(session)
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, req)
	if mw.Code != http.StatusOK || !strings.Contains(mw.Body.String(), `"alice@corp"`) {
		t.Errorf("me with sso session = %d %s", mw.Code, mw.Body)
	}
}

func TestOIDCCallbackRoleRefreshKeepsDisabled(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	f.addOIDCUser(testIssuer, "sub-1", "old-name", "admin", false)
	fp := &fakeProviders{provider: &fakeProvider{
		claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "new-name", Role: "viewer"},
	}, settings: f.oidcSettings}
	h := newTestAPIWithProviders(t, f, fp)

	w := callback(t, h, "?code=c&state=st", stateCookie("st.n.v"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q", w.Code, w.Header().Get("Location"))
	}
	if u := f.oidcUsers[oidcKey(testIssuer, "sub-1")]; u.Username != "new-name" || u.Role != "viewer" {
		t.Errorf("user not refreshed from IdP: %+v", u)
	}
}

// TestOIDCCallbackScopedToIssuer pins the identity key: subjects are unique
// only within an issuer, so the same sub from a different provider must
// provision a distinct user instead of taking over the existing row (and
// its role and live sessions).
func TestOIDCCallbackScopedToIssuer(t *testing.T) {
	f := newFakeDB()
	old := f.addOIDCUser("https://old-idp.example", "sub-1", "old-admin", "admin", false)
	// The configuration now points at the new provider.
	cur := enabledSettings()
	cur.Issuer = "https://new-idp.example"
	f.oidcSettings = cur
	fp := &fakeProviders{
		provider: &fakeProvider{
			claims: &oidcauth.Claims{Issuer: "https://new-idp.example", Subject: "sub-1", Username: "eve", Role: "viewer"},
		},
		settings: cur,
	}
	h := newTestAPIWithProviders(t, f, fp)

	w := callback(t, h, "?code=c&state=st", stateCookie("st.n.v"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}

	nu := f.oidcUsers[oidcKey("https://new-idp.example", "sub-1")]
	if nu == nil || nu.ID == old.ID {
		t.Fatalf("same sub from a new issuer must create a distinct user, got %+v", nu)
	}
	if ou := f.oidcUsers[oidcKey("https://old-idp.example", "sub-1")]; ou.Username != "old-admin" || ou.Role != "admin" {
		t.Errorf("old-issuer user must be untouched: %+v", ou)
	}

	// The minted session belongs to the new-issuer identity.
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(session)
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, req)
	if mw.Code != http.StatusOK || !strings.Contains(mw.Body.String(), `"eve"`) {
		t.Errorf("me with new-issuer session = %d %s, want eve", mw.Code, mw.Body)
	}
	if !strings.Contains(mw.Body.String(), `"auth_source":"oidc"`) {
		t.Errorf("me for a federated session must carry auth_source oidc: %s", mw.Body)
	}
}

// TestOIDCCallbackProviderChangedMidFlight pins the race guard: a callback
// holds its provider across the code exchange's IdP round-trips, so an admin
// can switch providers — revoking every SSO session — while it is in flight.
// The late callback must not mint a fresh old-provider session behind the
// revocation.
func TestOIDCCallbackProviderChangedMidFlight(t *testing.T) {
	f := newFakeDB()
	// The stored settings already point at the NEW provider, as after a
	// settings PUT that landed during this callback's exchange...
	cur := enabledSettings()
	cur.Issuer = "https://new-idp.example"
	f.oidcSettings = cur
	// ...but the request still holds the OLD provider and its claims.
	fp := &fakeProviders{
		provider: &fakeProvider{
			claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "fed", Role: "admin"},
		},
		settings: enabledSettings(),
	}
	h := newTestAPIWithProviders(t, f, fp)

	w := callback(t, h, "?code=c&state=st", stateCookie("st.n.v"))
	// Any settings write bumps updated_at, so the stale callback is now
	// refused at the USER upsert (sso-error=state, "try again") — before it
	// can overwrite the role or scope a newer policy established. The
	// provider-identity re-check in the session insert remains as
	// defense-in-depth behind it.
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/#/sso-error=state" {
		t.Fatalf("stale-provider callback = %d %q, want 303 to sso-error=state: %s",
			w.Code, w.Header().Get("Location"), w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Error("stale-provider callback must not set a session cookie")
		}
	}
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none", len(f.sessions))
	}
	if len(f.logins) != 0 {
		t.Errorf("recorded logins = %d, want none (event only after the session commits)", len(f.logins))
	}
	if len(f.oidcUsers) != 0 {
		t.Errorf("oidc users = %d, want none (stale mapping must not provision)", len(f.oidcUsers))
	}
}

func TestOIDCCallbackFailures(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		cookie   *http.Cookie
		provider *fakeProvider
		presetDB func(f *fakeDB)
		wantCode string
	}{
		{name: "idp error param", query: "?error=access_denied&error_description=nope",
			cookie: stateCookie("st.n.v"), wantCode: "provider"},
		{name: "missing state cookie", query: "?code=c&state=st", wantCode: "state"},
		{name: "malformed state cookie", query: "?code=c&state=st",
			cookie: stateCookie("just-state"), wantCode: "state"},
		{name: "state mismatch", query: "?code=c&state=WRONG",
			cookie: stateCookie("st.n.v"), wantCode: "state"},
		{name: "missing code", query: "?state=st",
			cookie: stateCookie("st.n.v"), wantCode: "state"},
		{name: "exchange failure", query: "?code=c&state=st", cookie: stateCookie("st.n.v"),
			provider: &fakeProvider{exchangeErr: errors.New("token endpoint 500")},
			wantCode: "provider"},
		{name: "claims failure", query: "?code=c&state=st", cookie: stateCookie("st.n.v"),
			provider: &fakeProvider{exchangeErr: &oidcauth.ClaimsError{}},
			wantCode: "claims"},
		{name: "disabled user", query: "?code=c&state=st", cookie: stateCookie("st.n.v"),
			provider: &fakeProvider{claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-9", Username: "m", Role: "viewer"}},
			presetDB: func(f *fakeDB) { f.addOIDCUser(testIssuer, "sub-9", "m", "viewer", true) },
			wantCode: "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDB()
			f.oidcSettings = enabledSettings()
			if tc.presetDB != nil {
				tc.presetDB(f)
			}
			p := tc.provider
			if p == nil {
				p = &fakeProvider{claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "s", Username: "u", Role: "viewer"}}
			}
			h := newTestAPIWithProviders(t, f, &fakeProviders{provider: p, settings: f.oidcSettings})

			w := callback(t, h, tc.query, tc.cookie)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("callback = %d, want 303: %s", w.Code, w.Body)
			}
			if loc := w.Header().Get("Location"); loc != "/#/sso-error="+tc.wantCode {
				t.Errorf("location = %q, want sso-error=%s", loc, tc.wantCode)
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == sessionCookie {
					t.Error("failed callback must not set a session cookie")
				}
				if c.Name == oidcStateCookie && c.MaxAge >= 0 {
					t.Error("state cookie must be cleared even on failure")
				}
			}
		})
	}
}

// --- break-glass invariants ---

// TestSSORateLimitIsolatedFromLocalLogin pins the separate-limiter rule:
// exhausting the SSO limiter (e.g. a failing IdP behind one NAT) must not
// consume a single local-login attempt.
func TestSSORateLimitIsolatedFromLocalLogin(t *testing.T) {
	f := newFakeDB()
	f.addUser("breakglass", "hunter22222", "admin", false)
	h := newTestAPIWithProviders(t, f, &fakeProviders{providerErr: errors.New("idp down")})

	var last *httptest.ResponseRecorder
	for range loginLimit + 1 {
		last = startFlow(t, h)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("sso start after %d attempts = %d, want 429", loginLimit+1, last.Code)
	}
	// startFlow and doLogin use different fixture IPs; pin the same-IP case
	// explicitly.
	body, _ := json.Marshal(map[string]string{"username": "breakglass", "password": "hunter22222"})
	req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.9:4242" // the IP that just exhausted the SSO limiter
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("local login after SSO limiter exhaustion = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestLocalLoginUnaffectedByOIDC(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	f.addUser("breakglass", "hunter22222", "admin", false)
	// Provider errors loudly — as if the IdP is down — and local login must
	// not care (it never touches the manager).
	h := newTestAPIWithProviders(t, f, &fakeProviders{providerErr: errors.New("idp down")})

	if w := doLogin(t, h, "breakglass", "hunter22222"); w.Code != http.StatusOK {
		t.Errorf("local login with oidc enabled and idp down = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestFederatedUserCannotPasswordLogin(t *testing.T) {
	f := newFakeDB()
	f.addOIDCUser(testIssuer, "sub-1", "fed", "viewer", false) // empty password hash
	h := newTestAPI(t, f)

	fed := doLogin(t, h, "fed", "anything")
	unknown := doLogin(t, h, "ghost", "anything")
	if fed.Code != http.StatusUnauthorized {
		t.Fatalf("federated local login = %d, want 401 (never 500)", fed.Code)
	}
	if fed.Body.String() != unknown.Body.String() {
		t.Errorf("federated-user and unknown-user 401 bodies differ: %q vs %q",
			fed.Body, unknown.Body)
	}
}

// --- settings surface ---

func oidcSettingsBody() map[string]any {
	return map[string]any{
		"enabled": false, "issuer": "https://idp.example/realms/x",
		"client_id": "polarbeam", "client_secret": "",
		"redirect_url": "https://dash.example/api/v1/auth/oidc/callback",
		"scopes":       []string{"openid", "profile"}, "username_claim": "preferred_username",
		"role_claim": "groups", "admin_values": []string{}, "ca_pem": "",
	}
}

func doSettings(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	b, _ := json.Marshal(body)
	rdr = bytes.NewReader(b)
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "203.0.113.9:4242"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestOIDCSettingsGetRedactsSecret(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	h := newTestAPI(t, f)
	cookie, _ := adminAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/settings/oidc", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("settings get = %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if _, leaked := out["client_secret"]; leaked {
		t.Error("GET must never carry client_secret")
	}
	if set, _ := out["client_secret_set"].(bool); !set {
		t.Error("client_secret_set should be true for a stored secret")
	}
	if strings.Contains(w.Body.String(), "sekrit") {
		t.Error("stored secret leaked into the response body")
	}
}

func TestOIDCSettingsRequireAdminAndCSRF(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	viewerCookie, viewerCSRF := loginAndCookie(t, h, f) // alice, viewer

	req := httptest.NewRequest("GET", "/api/v1/settings/oidc", nil)
	req.AddCookie(viewerCookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer GET settings/oidc = %d, want 403", w.Code)
	}

	if w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", oidcSettingsBody(), viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403", w.Code)
	}

	adminCookie, _ := adminAndCookie(t, h, f)
	if w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", oidcSettingsBody(), adminCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("admin PUT without CSRF = %d, want 403", w.Code)
	}
}

func TestOIDCSettingsPutValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)

	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantMsg string
	}{
		{"relative issuer", func(m map[string]any) { m["issuer"] = "idp.example" },
			"issuer must be an absolute http(s) URL"},
		{"issuer fragment", func(m map[string]any) { m["issuer"] = "https://idp.example/x#frag" },
			"issuer must not contain a fragment"},
		{"redirect query", func(m map[string]any) {
			m["redirect_url"] = "https://d.example/api/v1/auth/oidc/callback?x=1"
		}, "redirect_url must not contain a query string"},
		{"redirect wrong path", func(m map[string]any) { m["redirect_url"] = "https://d.example/callback" },
			"redirect_url path must be exactly /api/v1/auth/oidc/callback"},
		{"missing openid scope", func(m map[string]any) { m["scopes"] = []string{"profile"} },
			`scopes must include "openid"`},
		{"empty username claim", func(m map[string]any) { m["username_claim"] = "" },
			"username_claim is required"},
		{"empty role claim", func(m map[string]any) { m["role_claim"] = "" },
			"role_claim is required"},
		{"garbage ca_pem", func(m map[string]any) { m["ca_pem"] = "not a pem" },
			"ca_pem: no PEM certificates found"},
		{"enabled without secret", func(m map[string]any) { m["enabled"] = true },
			"enabled requires client_secret"},
		{"enabled missing everything", func(m map[string]any) {
			m["enabled"] = true
			m["issuer"], m["client_id"], m["redirect_url"] = "", "", ""
		}, "enabled requires issuer, client_id, redirect_url, client_secret"},
		{"unknown field", func(m map[string]any) { m["surprise"] = 1 }, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := oidcSettingsBody()
			tc.mutate(body)
			w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("put = %d, want 400: %s", w.Code, w.Body)
			}
			var out struct {
				Error string `json:"error"`
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			if !strings.Contains(out.Error, tc.wantMsg) {
				t.Errorf("error %q does not name the problem %q", out.Error, tc.wantMsg)
			}
		})
	}
}

func TestOIDCSettingsPutKeepsSecretAndInvalidates(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	fp := &fakeProviders{providerErr: oidcauth.ErrDisabled}
	h := newTestAPIWithProviders(t, f, fp)
	cookie, csrf := adminAndCookie(t, h, f)

	body := oidcSettingsBody()
	body["enabled"] = true // valid: a secret is already stored
	w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body)
	}
	if f.oidcSettings.ClientSecret != "sekrit" {
		t.Errorf("empty client_secret must keep the stored one, got %q", f.oidcSettings.ClientSecret)
	}
	if f.oidcSettings.UpdatedBy != "root" {
		t.Errorf("updated_by = %q, want session username", f.oidcSettings.UpdatedBy)
	}
	if fp.invalidated != 1 {
		t.Errorf("invalidate calls = %d, want 1", fp.invalidated)
	}
	if strings.Contains(w.Body.String(), "sekrit") {
		t.Error("PUT response leaked the stored secret")
	}

	// Replacing the secret stores the new value.
	body["client_secret"] = "newsecret"
	if w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("put2 = %d: %s", w.Code, w.Body)
	}
	if f.oidcSettings.ClientSecret != "newsecret" {
		t.Errorf("new secret not stored, got %q", f.oidcSettings.ClientSecret)
	}
}

// TestOIDCSettingsSecretScopedToProvider pins the leak guard: a stored
// secret belongs to the stored issuer+client, so re-pointing the config at
// a different provider must demand a fresh secret rather than silently
// submitting the old credential to the new token endpoint.
func TestOIDCSettingsSecretScopedToProvider(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)

	for _, tc := range []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"issuer change", func(m map[string]any) { m["issuer"] = "https://other-idp.example/realms/y" }},
		{"client_id change", func(m map[string]any) { m["client_id"] = "other-client" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := oidcSettingsBody() // client_secret "" = keep stored
			tc.mutate(body)
			w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("provider change with kept secret = %d, want 400: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), "requires entering a new client_secret") {
				t.Errorf("error does not name the secret rule: %s", w.Body)
			}
		})
	}

	// Same provider + kept secret stays a normal edit.
	body := oidcSettingsBody()
	body["enabled"] = true
	if w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("same-provider edit with kept secret = %d: %s", w.Code, w.Body)
	}
}

// TestOIDCSettingsProviderChangeRevokesSSOSessions pins the session rule
// paired with the identity key: re-pointing the config at a different
// provider signs out every SSO session (their roles came from the old IdP)
// while local break-glass sessions survive.
func TestOIDCSettingsProviderChangeRevokesSSOSessions(t *testing.T) {
	ssoLogin := func(t *testing.T, h http.Handler) *http.Cookie {
		t.Helper()
		w := callback(t, h, "?code=c&state=st", stateCookie("st.n.v"))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Fatalf("sso login = %d %q: %s", w.Code, w.Header().Get("Location"), w.Body)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie {
				return c
			}
		}
		t.Fatal("sso login set no session cookie")
		return nil
	}
	me := func(t *testing.T, h http.Handler, cookie *http.Cookie) int {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	for _, tc := range []struct {
		name       string
		mutate     func(m map[string]any)
		wantRevoke bool
	}{
		{"issuer change", func(m map[string]any) {
			m["issuer"] = "https://other-idp.example/realms/y"
			m["client_secret"] = "fresh" // provider change demands a new secret
		}, true},
		{"client_id change", func(m map[string]any) {
			m["client_id"] = "other-client"
			m["client_secret"] = "fresh"
		}, true},
		// Role policy is authorization: sessions join role/scope live from
		// the users row, which only a LOGIN remaps, so a policy edit must
		// sign federated users out for remapping.
		{"role policy edit", func(m map[string]any) {
			m["admin_values"] = []string{"some-group"}
		}, true},
		{"policy-neutral edit", func(m map[string]any) {
			m["scopes"] = []string{"openid", "profile", "email"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDB()
			f.oidcSettings = enabledSettings()
			fp := &fakeProviders{provider: &fakeProvider{
				claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "fed", Role: "viewer"},
			}, settings: f.oidcSettings}
			h := newTestAPIWithProviders(t, f, fp)
			ssoCookie := ssoLogin(t, h)
			adminCookie, csrf := adminAndCookie(t, h, f)

			body := oidcSettingsBody()
			// Match the stored admin_values so only tc.mutate decides
			// whether the write is a policy change.
			body["admin_values"] = []string{"polarbeam-admins"}
			tc.mutate(body)
			w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, adminCookie, csrf)
			if w.Code != http.StatusOK {
				t.Fatalf("put = %d: %s", w.Code, w.Body)
			}

			wantSSO := http.StatusOK
			if tc.wantRevoke {
				wantSSO = http.StatusUnauthorized
			}
			if got := me(t, h, ssoCookie); got != wantSSO {
				t.Errorf("sso session after put = %d, want %d", got, wantSSO)
			}
			if got := me(t, h, adminCookie); got != http.StatusOK {
				t.Errorf("local session after put = %d, want 200 (break-glass must survive)", got)
			}
			var out struct {
				Warnings []string `json:"warnings"`
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			warned := false
			for _, warning := range out.Warnings {
				if strings.Contains(warning, "signed out") {
					warned = true
				}
			}
			if warned != tc.wantRevoke {
				t.Errorf("sign-out warning present = %v, want %v (warnings: %v)", warned, tc.wantRevoke, out.Warnings)
			}
		})
	}
}

// raceToProviderB simulates a concurrent settings write landing between a
// PUT's validation (which read snapshot A) and its store transaction: the
// stored row now points at provider B and a B login has committed a session.
func raceToProviderB(f *fakeDB) {
	f.beforeUpdateOIDCSettings = func() {
		b := enabledSettings()
		b.Issuer = "https://b-idp.example"
		f.oidcSettings = b
		u := f.addOIDCUser("https://b-idp.example", "sub-b", "bee", "admin", false)
		f.sessions["b-tok"] = &store.SessionInfo{
			ID: uuid.New(), UserID: u.ID, Username: "bee", Role: "admin",
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}
}

// TestOIDCSettingsStaleSnapshotStillRevokes pins the in-transaction
// provider-change decision: with two writes based on the same snapshot A,
// one switches A→B and a B login commits; the other writes A back. Its
// handler-level comparison (A == A) sees no change, so only the store —
// comparing against the row it locked — can catch the switch and revoke the
// B-issued session.
func TestOIDCSettingsStaleSnapshotStillRevokes(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings() // provider A, the snapshot this PUT reads
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)
	raceToProviderB(f)

	body := oidcSettingsBody() // issuer/client_id A — "unchanged" vs the snapshot
	body["client_secret"] = "fresh"
	w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body)
	}
	if _, alive := f.sessions["b-tok"]; alive {
		t.Error("B-issued session survived the stale write back to A")
	}
	if !strings.Contains(w.Body.String(), "signed out") {
		t.Errorf("response must warn about the sign-out: %s", w.Body)
	}
}

// TestOIDCSettingsStaleKeptSecretConflicts: same race, but the stale write
// keeps the stored secret. Applying it would submit provider B's secret to
// provider A, so the write must be rejected outright — leaving B's
// configuration, secret, and sessions intact and consistent.
func TestOIDCSettingsStaleKeptSecretConflicts(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)
	raceToProviderB(f)

	w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", oidcSettingsBody(), cookie, csrf) // secret kept
	if w.Code != http.StatusConflict {
		t.Fatalf("stale put with kept secret = %d, want 409: %s", w.Code, w.Body)
	}
	if f.oidcSettings.Issuer != "https://b-idp.example" {
		t.Errorf("rejected write must not change settings, issuer = %q", f.oidcSettings.Issuer)
	}
	if _, alive := f.sessions["b-tok"]; !alive {
		t.Error("rejected write must not revoke the still-valid B session")
	}
}

// TestOIDCSettingsPutNormalizesNullArrays pins the nil-slice guard: an
// omitted or null admin_values must store as an empty list, not as SQL
// NULL against a NOT NULL column (a 500 for an accepted request).
func TestOIDCSettingsPutNormalizesNullArrays(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)

	body := oidcSettingsBody()
	delete(body, "admin_values")
	w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("put without admin_values = %d: %s", w.Code, w.Body)
	}
	if f.oidcSettings.AdminValues == nil {
		t.Error("nil admin_values reached the store; must be normalized to an empty slice")
	}
}

func TestOIDCSettingsPutWarnings(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	h := newTestAPI(t, f)
	cookie, csrf := adminAndCookie(t, h, f)

	body := oidcSettingsBody()
	body["enabled"] = true
	body["issuer"] = "http://keycloak:8080/realms/x" // plain http
	body["client_secret"] = "kc-secret"              // issuer changed, so a new secret is mandatory
	body["admin_values"] = []string{}
	w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Warnings) != 2 {
		t.Fatalf("warnings = %v, want http-issuer and empty-admin-values", out.Warnings)
	}
}

func TestOIDCSettingsTest(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	fp := &fakeProviders{testInfo: &oidcauth.DiscoveryInfo{
		Issuer: "https://idp.example/realms/x", TokenEndpoint: "https://idp.example/token",
	}}
	h := newTestAPIWithProviders(t, f, fp)
	cookie, csrf := adminAndCookie(t, h, f)

	w := doSettings(t, h, "POST", "/api/v1/settings/oidc/test", oidcSettingsBody(), cookie, csrf)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "token_endpoint") {
		t.Errorf("test = %d %s, want 200 with endpoints", w.Code, w.Body)
	}

	fp.testInfo, fp.testErr = nil, errors.New("dial tcp: connection refused")
	w = doSettings(t, h, "POST", "/api/v1/settings/oidc/test", oidcSettingsBody(), cookie, csrf)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("failed test = %d %s, want 502 carrying the discovery error", w.Code, w.Body)
	}

	body := oidcSettingsBody()
	body["issuer"] = ""
	w = doSettings(t, h, "POST", "/api/v1/settings/oidc/test", body, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Errorf("test without issuer = %d, want 400", w.Code)
	}
}
