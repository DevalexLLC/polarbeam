package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeIDP is a loopback OIDC provider: TLS discovery + JWKS + token
// endpoints with an RSA-signed ID token, so the REAL go-oidc/oauth2 path
// runs offline.
type fakeIDP struct {
	t   *testing.T
	ts  *httptest.Server
	key *rsa.PrivateKey

	discoveryHits atomic.Int64
	// tokenClaims builds the ID-token claims for the next /token call;
	// tests override it to serve bad tokens.
	tokenClaims func(idp *fakeIDP) map[string]any
	// dropDiscoveryKeys removes keys from the discovery document, and
	// overrideDiscovery replaces values, to test endpoint validation.
	dropDiscoveryKeys []string
	overrideDiscovery map[string]any

	gotCode     atomic.Value // string
	gotVerifier atomic.Value // string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &fakeIDP{t: t, key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.discoveryHits.Add(1)
		doc := map[string]any{
			"issuer":                                idp.ts.URL,
			"authorization_endpoint":                idp.ts.URL + "/auth",
			"token_endpoint":                        idp.ts.URL + "/token",
			"jwks_uri":                              idp.ts.URL + "/jwks",
			"userinfo_endpoint":                     idp.ts.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		for _, k := range idp.dropDiscoveryKeys {
			delete(doc, k)
		}
		maps.Copy(doc, idp.overrideDiscovery)
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &idp.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		idp.gotCode.Store(r.PostFormValue("code"))
		idp.gotVerifier.Store(r.PostFormValue("code_verifier"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "bearer", "expires_in": 300,
			"id_token": idp.sign(idp.tokenClaims(idp)),
		})
	})

	idp.ts = httptest.NewTLSServer(mux)
	t.Cleanup(idp.ts.Close)
	return idp
}

// goodClaims are a valid Keycloak-shaped identity for this IdP.
func goodClaims(idp *fakeIDP) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": idp.ts.URL, "aud": "polarbeam", "sub": "user-1",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"nonce":              "nonce-1",
		"preferred_username": "alice",
		"groups":             []string{"dev", "polarbeam-admins"},
	}
}

func (f *fakeIDP) sign(claims map[string]any) string {
	f.t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", "test-key"))
	if err != nil {
		f.t.Fatalf("new signer: %v", err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		f.t.Fatalf("serialize: %v", err)
	}
	return raw
}

func (f *fakeIDP) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: f.ts.Certificate().Raw,
	}))
}

func (f *fakeIDP) settings() *store.OIDCSettings {
	return &store.OIDCSettings{
		Enabled: true, Issuer: f.ts.URL, ClientID: "polarbeam", ClientSecret: "s3cr3t",
		RedirectURL:   "https://dash.example/api/v1/auth/oidc/callback",
		Scopes:        []string{"openid", "profile"},
		UsernameClaim: "preferred_username", RoleClaim: "groups",
		AdminValues: []string{"polarbeam-admins"},
		CAPEM:       f.caPEM(),
		UpdatedAt:   time.Now(),
	}
}

func TestProviderRoundTrip(t *testing.T) {
	idp := newFakeIDP(t)
	idp.tokenClaims = goodClaims

	p, err := newProvider(context.Background(), idp.settings())
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}

	verifier := oauth2.GenerateVerifier()
	authURL := p.AuthCodeURL("state-1", "nonce-1", verifier)
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if q.Get("state") != "state-1" || q.Get("nonce") != "nonce-1" {
		t.Errorf("auth url params: %v", q)
	}
	if q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier) ||
		q.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE challenge missing or wrong: %v", q)
	}

	claims, err := p.Exchange(context.Background(), "code-1", verifier, "nonce-1")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if claims.Subject != "user-1" || claims.Username != "alice" || claims.Role != "admin" {
		t.Errorf("claims = %+v", claims)
	}
	if idp.gotCode.Load() != "code-1" || idp.gotVerifier.Load() != verifier {
		t.Errorf("token endpoint got code=%v verifier=%v", idp.gotCode.Load(), idp.gotVerifier.Load())
	}
}

func TestProviderRejectsBadTokens(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		nonce   string
		wantErr string
	}{
		{name: "wrong issuer", mutate: func(m map[string]any) { m["iss"] = "https://evil.example" },
			nonce: "nonce-1", wantErr: "issued by a different provider"},
		{name: "wrong audience", mutate: func(m map[string]any) { m["aud"] = "other-client" },
			nonce: "nonce-1", wantErr: "audience"},
		{name: "expired", mutate: func(m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() },
			nonce: "nonce-1", wantErr: "expired"},
		{name: "nonce mismatch", mutate: func(m map[string]any) {},
			nonce: "some-other-nonce", wantErr: "nonce mismatch"},
		{name: "missing username claim", mutate: func(m map[string]any) { delete(m, "preferred_username") },
			nonce: "nonce-1", wantErr: "missing username claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.tokenClaims = func(idp *fakeIDP) map[string]any {
				m := goodClaims(idp)
				tc.mutate(m)
				return m
			}
			p, err := newProvider(context.Background(), idp.settings())
			if err != nil {
				t.Fatalf("newProvider: %v", err)
			}
			_, err = p.Exchange(context.Background(), "code-1", oauth2.GenerateVerifier(), tc.nonce)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("exchange err = %v, want mention of %q", err, tc.wantErr)
			}
			if tc.name == "missing username claim" {
				var ce *ClaimsError
				if !errors.As(err, &ce) {
					t.Errorf("claim mapping failure must be *ClaimsError, got %T", err)
				}
			}
		})
	}
}

// TestProviderRequiresEndpoints pins the fail-loud guard: go-oidc accepts a
// discovery document missing authorization/token/jwks endpoints, and an
// empty authorization endpoint would turn /auth/oidc/start into a
// redirect-to-self loop.
func TestProviderRequiresEndpoints(t *testing.T) {
	for _, missing := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(missing, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.tokenClaims = goodClaims
			idp.dropDiscoveryKeys = []string{missing}
			_, err := newProvider(context.Background(), idp.settings())
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Errorf("newProvider without %s: err = %v, want loud mention", missing, err)
			}
			// The test-connection surface must report the same failure.
			if _, err := discoverInfo(context.Background(), idp.settings()); err == nil {
				t.Errorf("discoverInfo without %s must fail too", missing)
			}
		})
	}
}

// TestProviderRejectsPlaintextEndpointDowngrade pins the https rule: an
// https issuer advertising an http endpoint would push credentials, tokens,
// or key fetches onto plaintext.
func TestProviderRejectsPlaintextEndpointDowngrade(t *testing.T) {
	idp := newFakeIDP(t)
	idp.tokenClaims = goodClaims
	idp.overrideDiscovery = map[string]any{"token_endpoint": "http://idp.example/token"}
	_, err := newProvider(context.Background(), idp.settings())
	if err == nil || !strings.Contains(err.Error(), "non-https token_endpoint") {
		t.Errorf("https issuer with http token_endpoint: err = %v, want downgrade rejection", err)
	}
}

func TestProviderCAPEMHonored(t *testing.T) {
	idp := newFakeIDP(t)
	idp.tokenClaims = goodClaims

	// Without ca_pem the IdP's self-signed certificate must be rejected —
	// proof the custom pool is actually applied, not merged with defaults.
	cfg := idp.settings()
	cfg.CAPEM = ""
	if _, err := newProvider(context.Background(), cfg); err == nil {
		t.Fatal("discovery against an untrusted IdP certificate must fail without ca_pem")
	}

	cfg.CAPEM = "-----BEGIN GARBAGE-----\nzz\n-----END GARBAGE-----\n"
	if _, err := newProvider(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "no usable PEM certificates") {
		t.Fatalf("garbage ca_pem err = %v", err)
	}
}

// settingsStub returns scripted settings and counts reads.
type settingsStub struct {
	cfg  *store.OIDCSettings
	errs error
}

func (s *settingsStub) GetOIDCSettings(context.Context) (*store.OIDCSettings, error) {
	return s.cfg, s.errs
}

func TestManagerCacheAndInvalidate(t *testing.T) {
	idp := newFakeIDP(t)
	idp.tokenClaims = goodClaims
	src := &settingsStub{cfg: idp.settings()}
	m := NewManager(src)
	ctx := context.Background()

	for range 3 {
		if _, _, err := m.Provider(ctx); err != nil {
			t.Fatalf("provider: %v", err)
		}
	}
	if n := idp.discoveryHits.Load(); n != 1 {
		t.Errorf("discoveries after 3 calls = %d, want 1 (cached)", n)
	}

	// A settings edit (new updated_at) rebuilds.
	fresh := *src.cfg
	fresh.UpdatedAt = src.cfg.UpdatedAt.Add(time.Second)
	src.cfg = &fresh
	if _, _, err := m.Provider(ctx); err != nil {
		t.Fatalf("provider after bump: %v", err)
	}
	if n := idp.discoveryHits.Load(); n != 2 {
		t.Errorf("discoveries after updated_at bump = %d, want 2", n)
	}

	m.Invalidate()
	if _, _, err := m.Provider(ctx); err != nil {
		t.Fatalf("provider after invalidate: %v", err)
	}
	if n := idp.discoveryHits.Load(); n != 3 {
		t.Errorf("discoveries after invalidate = %d, want 3", n)
	}
}

func TestManagerDisabled(t *testing.T) {
	idp := newFakeIDP(t)
	cfg := idp.settings()
	cfg.Enabled = false
	m := NewManager(&settingsStub{cfg: cfg})

	if _, _, err := m.Provider(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled provider err = %v, want ErrDisabled", err)
	}
	if n := idp.discoveryHits.Load(); n != 0 {
		t.Errorf("disabled must never discover, got %d hits", n)
	}
}

func TestManagerTestUsesCandidateConfig(t *testing.T) {
	idp := newFakeIDP(t)
	idp.tokenClaims = goodClaims
	// The stored row is disabled; Test must still discover the candidate.
	stored := idp.settings()
	stored.Enabled = false
	m := NewManager(&settingsStub{cfg: stored})

	info, err := m.Test(context.Background(), *idp.settings())
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if info.Issuer != idp.ts.URL || info.TokenEndpoint != idp.ts.URL+"/token" {
		t.Errorf("discovery info = %+v", info)
	}
}
