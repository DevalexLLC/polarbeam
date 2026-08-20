// Package oidcauth implements optional OIDC single sign-on for the
// dashboard: lazy, cached IdP discovery driven by the DB-stored
// oidc_settings row, the authorization-code + PKCE exchange, and the
// claims-to-role mapping. It is the only place the server performs outbound
// HTTP, and only when an admin has enabled OIDC — server startup and local
// (break-glass) login never depend on the IdP.
package oidcauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// discoveryTimeout bounds every outbound IdP call (discovery, token, JWKS):
// the dedicated http.Client carries it, so a slow or dead IdP can never hang
// a dashboard request longer than this.
const discoveryTimeout = 10 * time.Second

// ErrDisabled is returned by Provider when OIDC is switched off; handlers
// map it to their "not configured" surface rather than an IdP error.
var ErrDisabled = errors.New("oidc is disabled")

// Claims is the dashboard identity extracted from a verified ID token. Role
// is already mapped to a dashboard role — handlers never see raw IdP
// claims. Networks is set only for the network-scoped roles: the mapped
// network NAMES from the matching role rules, still unresolved (the
// callback resolves them against the networks table and fails the login
// loudly when none survive). Issuer is the verified iss of the token that
// produced these claims: subjects are unique only within an issuer, so
// identities are scoped to it.
type Claims struct {
	Issuer   string
	Subject  string
	Username string
	Role     string
	Networks []string
}

// Provider is the handler-facing boundary of a discovered IdP; httpapi
// tests fake it so no test ever performs discovery.
type Provider interface {
	AuthCodeURL(state, nonce, pkceVerifier string) string
	Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (*Claims, error)
}

// DiscoveryInfo is the test-connection result: the endpoints the IdP
// advertised, proving discovery (and PKI, when ca_pem is set) works.
type DiscoveryInfo struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint,omitempty"`
}

// SettingsSource is the slice of the store the manager reads.
type SettingsSource interface {
	GetOIDCSettings(ctx context.Context) (*store.OIDCSettings, error)
}

// Manager owns the discovered provider, rebuilding it lazily whenever the
// settings row changes (keyed on updated_at) or Invalidate is called. A
// build failure is returned, never cached: the next request retries, so a
// transient IdP outage heals without a restart.
type Manager struct {
	db SettingsSource

	mu       sync.Mutex
	cacheKey string
	cached   *realProvider
}

// NewManager returns a manager reading configuration from db. No discovery
// happens until the first Provider call needs it.
func NewManager(db SettingsSource) *Manager {
	return &Manager{db: db}
}

// Provider returns the discovered IdP for the current settings, or
// ErrDisabled when OIDC is off. The mutex is held across discovery on
// purpose: concurrent first requests share one discovery instead of racing.
func (m *Manager) Provider(ctx context.Context) (Provider, *store.OIDCSettings, error) {
	cfg, err := m.db.GetOIDCSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Enabled {
		return nil, nil, ErrDisabled
	}
	key := cfg.UpdatedAt.UTC().Format(time.RFC3339Nano)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached != nil && m.cacheKey == key {
		return m.cached, cfg, nil
	}
	p, err := newProvider(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery for %s: %w", cfg.Issuer, err)
	}
	m.cached, m.cacheKey = p, key
	return p, cfg, nil
}

// Invalidate drops the cached provider; the settings PUT calls it so edits
// apply on the next request without waiting for updated_at comparison.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached, m.cacheKey = nil, ""
}

// Test runs discovery against a candidate configuration (not the stored
// row) and reports the advertised endpoints. It never touches the cache:
// testing a broken config must not break live logins.
func (m *Manager) Test(ctx context.Context, cfg store.OIDCSettings) (*DiscoveryInfo, error) {
	return discoverInfo(ctx, &cfg)
}
