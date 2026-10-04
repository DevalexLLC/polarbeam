package grpcapi

import (
	"context"
	"crypto/x509"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCertValidCached: within the TTL one DB lookup serves repeated checks,
// both outcomes are cached, and errors are never cached (fail-closed).
func TestCertValidCached(t *testing.T) {
	ctx := context.Background()
	agent := uuid.New()
	serial := big.NewInt(4242)

	calls := 0
	answer, failWith := true, error(nil)
	s := &Server{fetchCertValid: func(context.Context, *big.Int, uuid.UUID) (bool, error) {
		calls++
		return answer, failWith
	}}

	for i := 0; i < 5; i++ {
		valid, err := s.certValidCached(ctx, serial, agent)
		if err != nil || !valid {
			t.Fatalf("call %d: valid=%v err=%v", i, valid, err)
		}
	}
	if calls != 1 {
		t.Errorf("5 checks inside the TTL cost %d lookups, want 1", calls)
	}

	// A different serial (rotation) is its own entry.
	if _, err := s.certValidCached(ctx, big.NewInt(4243), agent); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("new serial did not fetch: %d lookups", calls)
	}

	// Revoked is cached too — a revoked cert cannot flap back.
	answer = false
	revokedAgent := uuid.New()
	for i := 0; i < 3; i++ {
		valid, err := s.certValidCached(ctx, serial, revokedAgent)
		if err != nil || valid {
			t.Fatalf("revoked check %d: valid=%v err=%v", i, valid, err)
		}
	}
	if calls != 3 {
		t.Errorf("3 revoked checks cost %d lookups, want exactly 1 more", calls)
	}

	// Errors are NEVER cached: every check retries the DB.
	failWith = errors.New("db down")
	errAgent := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := s.certValidCached(ctx, serial, errAgent); err == nil {
			t.Fatal("expected error to propagate")
		}
	}
	if calls != 6 {
		t.Errorf("3 failing checks cost %d extra lookups, want 3 (errors uncached)", calls-3)
	}

	// Expiry: a stale entry refetches.
	failWith = nil
	answer = true
	s.certs.mu.Lock()
	for k, e := range s.certs.entries {
		e.expires = time.Now().Add(-time.Second)
		s.certs.entries[k] = e
	}
	s.certs.mu.Unlock()
	if _, err := s.certValidCached(ctx, serial, agent); err != nil {
		t.Fatal(err)
	}
	if calls != 7 {
		t.Errorf("expired entry did not refetch: %d lookups", calls)
	}
}

// TestCacheRevokedOverridesValidEntry: a revocation proven by an uncached
// check replaces a fresh "valid" entry, so the next unary check refuses
// without waiting out the TTL — and without another lookup.
func TestCacheRevokedOverridesValidEntry(t *testing.T) {
	ctx := context.Background()
	agent := uuid.New()
	serial := big.NewInt(4242)
	calls := 0
	s := &Server{fetchCertValid: func(context.Context, *big.Int, uuid.UUID) (bool, error) {
		calls++
		return true, nil
	}}
	if valid, err := s.certValidCached(ctx, serial, agent); err != nil || !valid {
		t.Fatalf("seed: valid=%v err=%v", valid, err)
	}

	s.cacheRevoked(&agentIdentity{AgentID: agent, Cert: &x509.Certificate{SerialNumber: serial}})

	if valid, err := s.certValidCached(ctx, serial, agent); err != nil || valid {
		t.Errorf("after cacheRevoked: valid=%v err=%v, want false", valid, err)
	}
	if calls != 1 {
		t.Errorf("cacheRevoked check cost %d lookups, want 1 (the seed only)", calls)
	}
}
