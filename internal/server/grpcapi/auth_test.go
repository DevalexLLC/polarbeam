package grpcapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"math/big"
	"net"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/devalexllc/polarbeam/internal/audit"
)

type recHandler struct{ recs []slog.Record }

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

// peerAuth lifts the TLS auth info out of a context agentPeerCtx built, so
// a test can attach an address to it.
func peerAuth(ctx context.Context) credentials.AuthInfo {
	p, _ := peer.FromContext(ctx)
	return p.AuthInfo
}

func attrOf(r slog.Record, key string) string {
	v := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
		}
		return true
	})
	return v
}

// TestAuthenticateAgentAuditsRefusals: each refusal is an agent.auth
// denial with its reason and the peer address; a valid identity emits
// nothing here (its session start is StreamConfig's record).
func TestAuthenticateAgentAuditsRefusals(t *testing.T) {
	rec := &recHandler{}
	revoked := uuid.New()
	s := &Server{
		audit: audit.New(slog.New(rec)),
		fetchCertValid: func(_ context.Context, _ *big.Int, id uuid.UUID) (bool, error) {
			return id != revoked, nil
		},
	}
	addr := &net.TCPAddr{IP: net.ParseIP("198.51.100.4"), Port: 5555}
	withPeer := func(ctx context.Context, chains [][]*x509.Certificate) context.Context {
		return peer.NewContext(ctx, &peer.Peer{Addr: addr, AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: chains}}})
	}
	foreign := &x509.Certificate{SerialNumber: big.NewInt(1)} // no agent URI SAN

	cases := []struct {
		name   string
		ctx    context.Context
		code   codes.Code
		reason string
		agent  string
	}{
		{"no peer", context.Background(), codes.Unauthenticated, "no_peer", ""},
		{"no cert", withPeer(context.Background(), nil), codes.Unauthenticated, "no_client_certificate", ""},
		{"foreign cert", withPeer(context.Background(), [][]*x509.Certificate{{foreign}}), codes.PermissionDenied, "not_agent_certificate", ""},
		{"revoked", peer.NewContext(context.Background(), &peer.Peer{Addr: addr, AuthInfo: peerAuth(agentPeerCtx(context.Background(), revoked, big.NewInt(7)))}), codes.PermissionDenied, "revoked_or_unknown", revoked.String()},
	}
	for _, tc := range cases {
		before := len(rec.recs)
		_, err := s.authenticateAgent(tc.ctx)
		if status.Code(err) != tc.code {
			t.Errorf("%s: code = %v, want %v", tc.name, status.Code(err), tc.code)
		}
		if len(rec.recs) != before+1 {
			t.Fatalf("%s: %d records, want 1", tc.name, len(rec.recs)-before)
		}
		r := rec.recs[before]
		if audit.EventID(r) != audit.EventAgentAuth || attrOf(r, "outcome") != "denied" || attrOf(r, "reason") != tc.reason {
			t.Errorf("%s: event=%s outcome=%s reason=%s", tc.name, audit.EventID(r), attrOf(r, "outcome"), attrOf(r, "reason"))
		}
		if attrOf(r, "agent") != tc.agent || attrOf(r, "user") != tc.agent {
			t.Errorf("%s: agent=%q user=%q, want %q", tc.name, attrOf(r, "agent"), attrOf(r, "user"), tc.agent)
		}
		if tc.name != "no peer" && attrOf(r, "remote") != "198.51.100.4" {
			t.Errorf("%s: remote = %q", tc.name, attrOf(r, "remote"))
		}
	}

	// A valid agent is admitted silently.
	ok := uuid.New()
	before := len(rec.recs)
	if _, err := s.authenticateAgent(agentPeerCtx(context.Background(), ok, big.NewInt(8))); err != nil {
		t.Fatalf("valid agent refused: %v", err)
	}
	if len(rec.recs) != before {
		t.Error("valid agent produced an audit record from authenticateAgent")
	}
}
