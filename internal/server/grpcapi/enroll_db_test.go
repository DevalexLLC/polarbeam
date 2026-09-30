package grpcapi

// DB-backed tests for the Enroll and RenewCert handlers' refusal mapping:
// a replayed enrollment for a revoked agent is audited as revoked_agent but
// answered exactly like any bad token, and renewal maps the store's
// in-transaction revocation check, CSR rejection, and success.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/devalexllc/polarbeam/internal/audit"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func enrollHarness(t *testing.T) (context.Context, *store.Store, *Server, *recHandler, string) {
	t.Helper()
	ctx, s, _ := streamHarness(t)
	dir := t.TempDir()
	if err := ca.Init(dir, ca.AlgECDSAP256, false); err != nil {
		t.Fatalf("ca.Init: %v", err)
	}
	authority, err := ca.Load(dir, ca.Lifetimes{})
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}
	rec := &recHandler{}
	srv := New(s, authority)
	srv.audit = audit.New(slog.New(rec))

	site, err := s.EnsureSite(ctx, "enroll-site")
	if err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	network, err := s.NetworkIDByName(ctx, "default")
	if err != nil {
		t.Fatalf("NetworkIDByName: %v", err)
	}
	token, err := s.CreateJoinToken(ctx, site, network, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	return ctx, s, srv, rec, token
}

func newCSR(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func certSerial(t *testing.T, der []byte) *big.Int {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	return cert.SerialNumber
}

// lastAudit returns the newest record for event id.
func lastAudit(t *testing.T, rec *recHandler, id string) slog.Record {
	t.Helper()
	for i := len(rec.recs) - 1; i >= 0; i-- {
		if audit.EventID(rec.recs[i]) == id {
			return rec.recs[i]
		}
	}
	t.Fatalf("no %s audit record", id)
	return slog.Record{}
}

func wantRefusal(t *testing.T, rec *recHandler, err error, code codes.Code, msg, event, outcome, reason string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != code || (msg != "" && st.Message() != msg) {
		t.Fatalf("got %v, want %s %q", err, code, msg)
	}
	r := lastAudit(t, rec, event)
	if attrOf(r, "outcome") != outcome || attrOf(r, "reason") != reason {
		t.Fatalf("%s audit: outcome=%s reason=%s, want %s/%s",
			event, attrOf(r, "outcome"), attrOf(r, "reason"), outcome, reason)
	}
}

func TestEnrollReplayRefusalAfterRevocation(t *testing.T) {
	ctx, s, srv, rec, token := enrollHarness(t)
	csr := newCSR(t)
	req := &pb.EnrollRequest{JoinToken: token, CsrDer: csr, Hostname: "h", ProbeAddress: "192.0.2.1"}

	first, err := srv.Enroll(ctx, req)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	again, err := srv.Enroll(ctx, req) // lost-response retry
	if err != nil || again.GetAgentId() != first.GetAgentId() {
		t.Fatalf("replay before revocation = (%v, %v), want agent %s", again.GetAgentId(), err, first.GetAgentId())
	}

	_, err = srv.Enroll(ctx, &pb.EnrollRequest{JoinToken: token, CsrDer: newCSR(t), Hostname: "h", ProbeAddress: "192.0.2.1"})
	wantRefusal(t, rec, err, codes.PermissionDenied, store.ErrTokenInvalid.Error(),
		audit.EventAgentEnroll, "denied", "invalid_or_used_token")

	if _, err := s.RevokeAgentCertificates(ctx, uuid.MustParse(first.GetAgentId())); err != nil {
		t.Fatalf("RevokeAgentCertificates: %v", err)
	}
	_, err = srv.Enroll(ctx, req)
	// Same wire refusal as any bad token; only the audit reason differs.
	wantRefusal(t, rec, err, codes.PermissionDenied, store.ErrTokenInvalid.Error(),
		audit.EventAgentEnroll, "denied", "revoked_agent")
}

func TestRenewCertRefusalMapping(t *testing.T) {
	ctx, s, srv, rec, token := enrollHarness(t)
	resp, err := srv.Enroll(ctx, &pb.EnrollRequest{JoinToken: token, CsrDer: newCSR(t), Hostname: "h", ProbeAddress: "192.0.2.1"})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	agentID := uuid.MustParse(resp.GetAgentId())
	serial := certSerial(t, resp.GetCertDer())
	agentCtx := agentPeerCtx(ctx, agentID, serial)

	renewed, err := srv.RenewCert(agentCtx, &pb.RenewCertRequest{CsrDer: newCSR(t)})
	if err != nil {
		t.Fatalf("RenewCert: %v", err)
	}
	valid, err := s.CertValid(ctx, certSerial(t, renewed.GetCertDer()), agentID)
	if err != nil || !valid {
		t.Fatalf("renewed certificate valid=%v err=%v", valid, err)
	}
	if r := lastAudit(t, rec, audit.EventAgentCertRenew); attrOf(r, "outcome") != "success" {
		t.Fatalf("renew audit outcome=%s, want success", attrOf(r, "outcome"))
	}

	_, err = srv.RenewCert(agentCtx, &pb.RenewCertRequest{CsrDer: []byte("not a csr")})
	wantRefusal(t, rec, err, codes.InvalidArgument, "", audit.EventAgentCertRenew, "failure", "csr_rejected")

	// Revoke behind a cached "valid" identity: the handler's store call is
	// the uncached in-transaction check that must refuse.
	srv.fetchCertValid = func(context.Context, *big.Int, uuid.UUID) (bool, error) { return true, nil }
	if _, _, err := s.RevokeCertificate(ctx, serial); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}
	_, err = srv.RenewCert(agentCtx, &pb.RenewCertRequest{CsrDer: newCSR(t)})
	wantRefusal(t, rec, err, codes.PermissionDenied, "certificate revoked or unknown",
		audit.EventAgentCertRenew, "denied", "revoked_or_unknown")
}
