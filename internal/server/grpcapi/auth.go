// Agent authentication: identity comes from the mTLS client certificate's
// URI SAN, and every authenticated RPC re-checks the certificate against the
// database (the sole revocation authority).
package grpcapi

import (
	"context"
	"crypto/x509"
	"log/slog"

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
	valid, err := s.store.CertValid(ctx, leaf.SerialNumber, agentID)
	if err != nil {
		slog.Error("certificate validity check failed", "err", err)
		return nil, status.Error(codes.Internal, "certificate check failed")
	}
	if !valid {
		return nil, status.Error(codes.PermissionDenied, "certificate revoked or unknown")
	}
	return &agentIdentity{AgentID: agentID, Cert: leaf}, nil
}
