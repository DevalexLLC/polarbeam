// Optional OIDC single sign-on: the browser-facing authorization-code+PKCE
// flow (start/callback), the open provider advertisement the login page
// reads, and the admin-only settings surface. Local login never touches any
// of this — OIDC failing (or being off) must leave break-glass accounts
// untouched.
package httpapi

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

const (
	// oidcStateCookie carries state + nonce + PKCE verifier across the IdP
	// round-trip: "<state>.<nonce>.<verifier>" (all three are dot-free).
	// SameSite=Lax on purpose — it must ride the cross-site top-level
	// navigation back from the IdP; it holds no session authority.
	oidcStateCookie = "polarbeam_oidc_state"
	oidcStateTTL    = 10 * time.Minute
	// oidcCallbackPath is the one redirect URI shape the server accepts —
	// the admin-entered redirect_url must end exactly here.
	oidcCallbackPath = "/api/v1/auth/oidc/callback"
	// exchangeTimeout bounds the callback's token+JWKS round-trips.
	exchangeTimeout = 15 * time.Second
)

// ssoRedirect sends the browser back to the SPA after a flow step. code is
// one of the short sso-error identifiers the login page maps to friendly
// text (provider|config|state|claims|disabled|internal) — IdP-supplied
// strings never reach the URL; the detail lives in the server log.
func ssoRedirect(w http.ResponseWriter, r *http.Request, code string) {
	target := "/"
	if code != "" {
		target = "/#/sso-error=" + code
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAuthProviders is open: the login page asks whether to offer SSO
// before any session exists. It reveals only the boolean.
func (a *api) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	s, err := a.db.GetOIDCSettings(r.Context())
	if err != nil {
		internalError(w, "get oidc settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"oidc": map[string]bool{"enabled": s.Enabled},
	})
}

// handleOIDCStart begins the flow: mint state/nonce/PKCE, stash them in the
// transient cookie, and bounce to the IdP's authorization endpoint.
func (a *api) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !a.ssoLimiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again in a minute")
		return
	}
	prov, _, err := a.providers.Provider(r.Context())
	if err != nil {
		code := "provider"
		if errors.Is(err, oidcauth.ErrDisabled) {
			code = "config"
		}
		slog.Warn("httpapi: oidc start", "err", err)
		ssoRedirect(w, r, code)
		return
	}
	state, _, err := auth.NewToken()
	if err != nil {
		internalError(w, "mint oidc state", err)
		return
	}
	nonce, _, err := auth.NewToken()
	if err != nil {
		internalError(w, "mint oidc nonce", err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    state + "." + nonce + "." + verifier,
		Path:     "/api/v1/auth/oidc",
		MaxAge:   int(oidcStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, prov.AuthCodeURL(state, nonce, verifier), http.StatusFound)
}

// handleOIDCCallback finishes the flow: state check, code exchange (with
// PKCE verifier and nonce), JIT user provisioning, session issue. Every
// failure logs its full detail server-side and sends the browser to a
// short sso-error code — never IdP error strings in a URL.
func (a *api) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !a.ssoLimiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again in a minute")
		return
	}
	// The state cookie is single-use: expire it before anything can fail.
	stateCookie, cookieErr := r.Cookie(oidcStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    "",
		Path:     "/api/v1/auth/oidc",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	fail := func(code, what string, err error) {
		slog.Warn("httpapi: oidc callback: "+what, "err", err)
		ssoRedirect(w, r, code)
	}

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail("provider", "idp returned error", errors.New(e+": "+q.Get("error_description")))
		return
	}
	if cookieErr != nil {
		fail("state", "missing state cookie", cookieErr)
		return
	}
	parts := strings.SplitN(stateCookie.Value, ".", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		fail("state", "malformed state cookie", nil)
		return
	}
	if s := q.Get("state"); s == "" || subtle.ConstantTimeCompare([]byte(s), []byte(parts[0])) != 1 {
		fail("state", "state mismatch", nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail("state", "missing authorization code", nil)
		return
	}

	prov, cfg, err := a.providers.Provider(r.Context())
	if err != nil {
		c := "provider"
		if errors.Is(err, oidcauth.ErrDisabled) {
			c = "config"
		}
		fail(c, "provider unavailable", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exchangeTimeout)
	defer cancel()
	claims, err := prov.Exchange(ctx, code, parts[2], parts[1])
	if err != nil {
		c := "provider"
		if _, ok := errors.AsType[*oidcauth.ClaimsError](err); ok {
			c = "claims"
		}
		fail(c, "code exchange", err)
		return
	}

	user, err := a.db.UpsertOIDCUser(r.Context(), claims.Issuer, claims.Subject, claims.Username, claims.Role)
	if err != nil {
		fail("internal", "upsert oidc user", err)
		return
	}
	// disabled survives the upsert by design: it is the operator's per-user
	// SSO revocation lever, independent of what the IdP still asserts.
	if user.Disabled {
		fail("disabled", "user is disabled", errors.New("subject "+claims.Subject))
		return
	}
	// The session insert revalidates cfg's provider identity inside its own
	// transaction: this request has been holding cfg since before Exchange's
	// IdP round-trips, and an admin may have switched providers (revoking
	// every SSO session) in that window. Minting unchecked would hand the
	// old provider's user a fresh session that outlives the revocation.
	create := func(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time) error {
		return a.db.CreateOIDCSession(ctx, userID, tokenHash, csrfToken, expiresAt, cfg.Issuer, cfg.ClientID)
	}
	if _, err := a.issueSession(w, r, user, create); err != nil {
		if errors.Is(err, store.ErrProviderChanged) {
			fail("config", "provider changed during login", err)
			return
		}
		fail("internal", "issue session", err)
		return
	}
	ssoRedirect(w, r, "")
}

// --- admin settings surface ---

type oidcSettingsJSON struct {
	Enabled  bool   `json:"enabled"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// The secret is write-only: GET reports only whether one is stored.
	ClientSecretSet bool      `json:"client_secret_set"`
	RedirectURL     string    `json:"redirect_url"`
	Scopes          []string  `json:"scopes"`
	UsernameClaim   string    `json:"username_claim"`
	RoleClaim       string    `json:"role_claim"`
	AdminValues     []string  `json:"admin_values"`
	CAPEM           string    `json:"ca_pem"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedBy       string    `json:"updated_by"`
	// Warnings is advisory and set only on a write response.
	Warnings []string `json:"warnings,omitempty"`
}

func toOIDCSettingsJSON(o *store.OIDCSettings) oidcSettingsJSON {
	scopes, admins := o.Scopes, o.AdminValues
	if scopes == nil {
		scopes = []string{}
	}
	if admins == nil {
		admins = []string{}
	}
	return oidcSettingsJSON{
		Enabled: o.Enabled, Issuer: o.Issuer, ClientID: o.ClientID,
		ClientSecretSet: o.ClientSecret != "", RedirectURL: o.RedirectURL,
		Scopes: scopes, UsernameClaim: o.UsernameClaim, RoleClaim: o.RoleClaim,
		AdminValues: admins, CAPEM: o.CAPEM,
		UpdatedAt: o.UpdatedAt, UpdatedBy: o.UpdatedBy,
	}
}

type oidcSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// Empty means "keep the stored secret" — the GET never echoes it, so
	// the form cannot round-trip it.
	ClientSecret  string   `json:"client_secret"`
	RedirectURL   string   `json:"redirect_url"`
	Scopes        []string `json:"scopes"`
	UsernameClaim string   `json:"username_claim"`
	RoleClaim     string   `json:"role_claim"`
	AdminValues   []string `json:"admin_values"`
	CAPEM         string   `json:"ca_pem"`
}

func (in oidcSettingsRequest) settings() store.OIDCSettings {
	// Omitted/null arrays decode as nil, which pgx would write as SQL NULL
	// into NOT NULL columns — normalize to empty so the request either
	// fails validation or stores cleanly, never 500s.
	scopes, admins := in.Scopes, in.AdminValues
	if scopes == nil {
		scopes = []string{}
	}
	if admins == nil {
		admins = []string{}
	}
	return store.OIDCSettings{
		Enabled: in.Enabled, Issuer: in.Issuer, ClientID: in.ClientID,
		ClientSecret: in.ClientSecret, RedirectURL: in.RedirectURL,
		Scopes: scopes, UsernameClaim: in.UsernameClaim, RoleClaim: in.RoleClaim,
		AdminValues: admins, CAPEM: in.CAPEM,
	}
}

// validateOIDCSettings names every problem at once (the settings-PUT
// idiom). current is the stored row: it tells us whether a secret exists
// (so "enabled" validates without reading the secret back) and which
// provider that secret belongs to.
func validateOIDCSettings(in oidcSettingsRequest, current *store.OIDCSettings) (problems, warnings []string) {
	secretStored := current.ClientSecret != ""
	checkURL := func(field, raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			problems = append(problems, field+" must be an absolute http(s) URL")
			return nil
		}
		if u.Fragment != "" {
			problems = append(problems, field+" must not contain a fragment")
			return nil
		}
		if u.Scheme == "http" {
			warnings = append(warnings, field+" uses plain http — credentials and tokens will cross the network unencrypted; use https outside isolated test setups")
		}
		return u
	}
	if in.Issuer != "" {
		checkURL("issuer", in.Issuer)
	}
	if in.RedirectURL != "" {
		if u := checkURL("redirect_url", in.RedirectURL); u != nil {
			if u.RawQuery != "" {
				problems = append(problems, "redirect_url must not contain a query string")
			}
			if u.Path != oidcCallbackPath {
				problems = append(problems, "redirect_url path must be exactly "+oidcCallbackPath)
			}
		}
	}
	if !slices.Contains(in.Scopes, "openid") {
		problems = append(problems, `scopes must include "openid"`)
	}
	if in.UsernameClaim == "" {
		problems = append(problems, "username_claim is required")
	}
	if in.RoleClaim == "" {
		problems = append(problems, "role_claim is required")
	}
	if in.CAPEM != "" {
		if err := validateCAPEM(in.CAPEM); err != nil {
			problems = append(problems, "ca_pem: "+err.Error())
		}
	}
	// The stored secret belongs to the stored provider. Keeping it while
	// pointing at a different issuer or client would submit the old
	// provider's credential to the new one — a leak, and a broken login.
	if in.ClientSecret == "" && secretStored &&
		(in.Issuer != current.Issuer || in.ClientID != current.ClientID) {
		problems = append(problems, "changing issuer or client_id requires entering a new client_secret (the stored secret belongs to the previous provider)")
	}
	if in.Enabled {
		var missing []string
		for _, f := range []struct{ name, v string }{
			{"issuer", in.Issuer}, {"client_id", in.ClientID}, {"redirect_url", in.RedirectURL},
		} {
			if f.v == "" {
				missing = append(missing, f.name)
			}
		}
		if in.ClientSecret == "" && !secretStored {
			missing = append(missing, "client_secret")
		}
		if len(missing) > 0 {
			problems = append(problems, "enabled requires "+strings.Join(missing, ", "))
		}
		if len(in.AdminValues) == 0 {
			warnings = append(warnings, "admin_values is empty: every SSO user will be a viewer; local admin accounts remain the only administrators")
		}
	}
	return problems, warnings
}

// validateCAPEM requires at least one parseable CERTIFICATE block.
func validateCAPEM(pemText string) error {
	rest := []byte(pemText)
	certs := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return errors.New("contains a non-certificate PEM block (" + block.Type + ")")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errors.New("certificate does not parse: " + err.Error())
		}
		certs++
	}
	if certs == 0 {
		return errors.New("no PEM certificates found")
	}
	return nil
}

func (a *api) handleOIDCSettingsGet(w http.ResponseWriter, r *http.Request) {
	o, err := a.db.GetOIDCSettings(r.Context())
	if err != nil {
		internalError(w, "get oidc settings", err)
		return
	}
	writeJSON(w, http.StatusOK, toOIDCSettingsJSON(o))
}

func (a *api) handleOIDCSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in oidcSettingsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	current, err := a.db.GetOIDCSettings(r.Context())
	if err != nil {
		internalError(w, "get oidc settings", err)
		return
	}
	problems, warnings := validateOIDCSettings(in, current)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	o := in.settings()
	o.UpdatedBy = sessionFrom(r.Context()).Username
	// A different issuer or client is a different provider (the same rule
	// that forces a fresh client_secret above): sessions issued under the
	// old provider must not carry over. The store detects the switch against
	// the row it locks — not against `current`, which may be stale by now —
	// and revokes in the same transaction as the settings write, so neither
	// a login whose code exchange straddles the switch nor a concurrent
	// settings write can leave a stale session behind (CreateOIDCSession
	// re-checks the provider under a share lock on the same row).
	out, revoked, err := a.db.UpdateOIDCSettings(r.Context(), o, in.ClientSecret == "")
	if errors.Is(err, store.ErrConcurrentProviderChange) {
		writeError(w, http.StatusConflict,
			"settings changed concurrently: the stored client_secret belongs to a different provider; reload and enter a new client_secret")
		return
	}
	if err != nil {
		internalError(w, "update oidc settings", err)
		return
	}
	if revoked > 0 {
		slog.Info("httpapi: oidc provider changed; revoked sso sessions", "count", revoked)
		warnings = append(warnings, "identity provider changed: all single sign-on sessions were signed out")
	}
	// The next start/callback rebuilds the provider from the new row —
	// settings apply without a restart.
	a.providers.Invalidate()
	resp := toOIDCSettingsJSON(out)
	resp.Warnings = warnings
	writeJSON(w, http.StatusOK, resp)
}

// handleOIDCSettingsTest runs discovery for the SUBMITTED configuration
// (empty secret = the stored one) without saving anything. This is the
// loud surface for egress and PKI problems: a failure returns the real
// error text to the admin, 502 because the upstream IdP is what failed.
func (a *api) handleOIDCSettingsTest(w http.ResponseWriter, r *http.Request) {
	var in oidcSettingsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Issuer == "" {
		writeError(w, http.StatusBadRequest, "issuer is required")
		return
	}
	if in.CAPEM != "" {
		if err := validateCAPEM(in.CAPEM); err != nil {
			writeError(w, http.StatusBadRequest, "ca_pem: "+err.Error())
			return
		}
	}
	cfg := in.settings()
	if cfg.ClientSecret == "" {
		current, err := a.db.GetOIDCSettings(r.Context())
		if err != nil {
			internalError(w, "get oidc settings", err)
			return
		}
		cfg.ClientSecret = current.ClientSecret
	}
	ctx, cancel := context.WithTimeout(r.Context(), exchangeTimeout)
	defer cancel()
	info, err := a.providers.Test(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, "discovery failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}
