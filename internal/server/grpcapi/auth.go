// Agent authentication: identity comes from the mTLS client certificate's
// URI SAN, and every authenticated RPC re-checks the certificate against the
// database (the sole revocation authority) through a short TTL cache.
package grpcapi

import (
	"context"
	"crypto/x509"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/google/uuid"
)

// agentIdentity is the verified caller of an AgentService RPC.
type agentIdentity struct {
	AgentID uuid.UUID
	Cert    *x509.Certificate
}

// certCacheTTL matches the stream sweep cadence: a revoked cert keeps
// pushing for at most one sweep interval longer than before, while the
// per-push (≈5s cadence) point lookups collapse ~6:1.
const certCacheTTL = 30 * time.Second

// certCache is a TTL cache over CertValid for UNARY RPC authentication
// only. The DB remains the sole revocation authority: the config-stream
// sweep still queries it directly every 30s and stays the enforcement path
// that drops a revoked agent's stream. Both outcomes are cached (a revoked
// cert stays revoked); DB ERRORS are never cached — fail-closed stays
// fail-closed and the next RPC retries the lookup.
type certCache struct {
	mu      sync.Mutex
	entries map[certKey]certEntry
}

type certKey struct {
	agentID uuid.UUID
	serial  string
}

type certEntry struct {
	valid   bool
	expires time.Time
}

func (c *certCache) lookup(k certKey, now time.Time) (valid, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[k]
	if !found || now.After(e.expires) {
		return false, false
	}
	return e.valid, true
}

func (c *certCache) put(k certKey, valid bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[certKey]certEntry)
	}
	// Opportunistic sweep keeps the map bounded without a background task
	// (same shape as assignmentCache).
	if len(c.entries) > 4096 {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[k] = certEntry{valid: valid, expires: now.Add(certCacheTTL)}
}

// certValidCached answers CertValid through the cache. fetch is a seam for
// tests; production wiring uses the store.
func (s *Server) certValidCached(ctx context.Context, serial *big.Int, agentID uuid.UUID) (bool, error) {
	k := certKey{agentID: agentID, serial: serial.String()}
	now := time.Now()
	if valid, found := s.certs.lookup(k, now); found {
		return valid, nil
	}
	fetch := s.fetchCertValid
	if fetch == nil {
		fetch = s.store.CertValid
	}
	valid, err := fetch(ctx, serial, agentID)
	if err != nil {
		return false, err
	}
	s.certs.put(k, valid, now)
	return valid, nil
}

// authenticateAgent extracts and validates the caller's identity. It returns
// PermissionDenied for anything short of a valid, unrevoked, agent-bound
// certificate.
func (s *Server) authenticateAgent(ctx context.Context) (*agentIdentity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer info")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]

	agentID, err := ca.AgentIDFromCert(leaf)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "certificate is not an agent certificate")
	}
	valid, err := s.certValidCached(ctx, leaf.SerialNumber, agentID)
	if err != nil {
		slog.Error("certificate validity check failed", "err", err)
		return nil, status.Error(codes.Internal, "certificate check failed")
	}
	if !valid {
		return nil, status.Error(codes.PermissionDenied, "certificate revoked or unknown")
	}
	return &agentIdentity{AgentID: agentID, Cert: leaf}, nil
}
