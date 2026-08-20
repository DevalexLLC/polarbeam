package oidcauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// realProvider is the live implementation of Provider over a discovered
// IdP. It keeps its own http.Client (bounded timeout, optional private-PKI
// roots) — the IdP is admin-configured runtime egress and must never ride
// http.DefaultClient.
type realProvider struct {
	client        *http.Client
	oauth         oauth2.Config
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	roleClaim     string
	adminValues   []string
	roleRules     []store.OIDCRoleRule
	unmatchedRole string
}

// httpClient builds the dedicated IdP client. caPEM, when set, REPLACES the
// system roots: a private-PKI deployment pins exactly its own CA.
func httpClient(caPEM string) (*http.Client, error) {
	c := &http.Client{Timeout: discoveryTimeout}
	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("ca_pem contains no usable PEM certificates")
		}
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}
	}
	return c, nil
}

// discover runs OIDC discovery for cfg with a bounded, dedicated client.
// go-oidc retains the client (not the context) for later JWKS fetches, so
// the short-lived discovery context here is safe.
func discover(ctx context.Context, cfg *store.OIDCSettings) (*oidc.Provider, *http.Client, error) {
	client, err := httpClient(cfg.CAPEM)
	if err != nil {
		return nil, nil, err
	}
	dctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), discoveryTimeout)
	defer cancel()
	p, err := oidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	if err := validateEndpoints(p, cfg.Issuer); err != nil {
		return nil, nil, err
	}
	return p, client, nil
}

// validateEndpoints refuses a discovery document missing the endpoints the
// code flow needs. go-oidc validates only the issuer, and an empty
// authorization endpoint would otherwise turn /auth/oidc/start into a
// redirect-to-self loop instead of a loud error. An https issuer must
// advertise https endpoints — anything else downgrades credentials, tokens,
// or key fetches to plaintext; all-http stays possible only for the
// explicitly configured (and warned-about) http-issuer test setups.
func validateEndpoints(p *oidc.Provider, issuer string) error {
	var info DiscoveryInfo
	if err := p.Claims(&info); err != nil {
		return fmt.Errorf("decode discovery document: %w", err)
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("parse issuer: %w", err)
	}
	for _, e := range []struct{ name, value string }{
		{"authorization_endpoint", info.AuthorizationEndpoint},
		{"token_endpoint", info.TokenEndpoint},
		{"jwks_uri", info.JWKSURI},
	} {
		u, err := url.Parse(e.value)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("discovery document has no usable %s (got %q)", e.name, e.value)
		}
		if issuerURL.Scheme == "https" && u.Scheme != "https" {
			return fmt.Errorf("discovery document advertises non-https %s (%q) for an https issuer", e.name, e.value)
		}
	}
	return nil
}

// discoverInfo is discovery for the test-connection surface: endpoints out,
// nothing cached.
func discoverInfo(ctx context.Context, cfg *store.OIDCSettings) (*DiscoveryInfo, error) {
	p, _, err := discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var info DiscoveryInfo
	if err := p.Claims(&info); err != nil {
		return nil, fmt.Errorf("decode discovery document: %w", err)
	}
	return &info, nil
}

// newProvider discovers cfg's issuer and wires the oauth2 + verifier pair
// used by the login flow.
func newProvider(ctx context.Context, cfg *store.OIDCSettings) (*realProvider, error) {
	p, client, err := discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &realProvider{
		client: client,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       cfg.Scopes,
		},
		verifier:      p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		usernameClaim: cfg.UsernameClaim,
		roleClaim:     cfg.RoleClaim,
		adminValues:   cfg.AdminValues,
		roleRules:     cfg.RoleRules,
		unmatchedRole: cfg.UnmatchedRole,
	}, nil
}

// AuthCodeURL returns the IdP authorization URL carrying state, nonce, and
// the S256 challenge derived from pkceVerifier.
func (p *realProvider) AuthCodeURL(state, nonce, pkceVerifier string) string {
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier))
}

// Exchange redeems the authorization code, requires and verifies an ID
// token (signature, issuer, audience, expiry via go-oidc), checks the nonce
// round-trip, and maps claims to the dashboard identity.
func (p *realProvider) Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (*Claims, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("token response carried no id_token")
	}
	idToken, err := p.verifier.Verify(oidc.ClientContext(ctx, p.client), raw)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return nil, errors.New("id_token nonce mismatch")
	}
	var all map[string]any
	if err := idToken.Claims(&all); err != nil {
		return nil, fmt.Errorf("decode id_token claims: %w", err)
	}
	return mapClaims(p.usernameClaim, p.roleClaim, p.adminValues, p.roleRules, p.unmatchedRole,
		idToken.Issuer, idToken.Subject, all)
}
