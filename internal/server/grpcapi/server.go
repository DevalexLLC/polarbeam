// Package grpcapi implements the agent-facing gRPC services: enrollment
// (token-authenticated, no client cert) and AgentService (mTLS only).
package grpcapi

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"math/big"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/devalexllc/polarbeam/internal/server/meshexpand"
	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
	"github.com/devalexllc/polarbeam/internal/server/outage"
	"github.com/devalexllc/polarbeam/internal/server/pathwatch"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/google/uuid"
)

// Server implements polarbeam.v1.EnrollmentService and AgentService.
type Server struct {
	pb.UnimplementedEnrollmentServiceServer
	pb.UnimplementedAgentServiceServer

	store       *store.Store
	ca          *ca.CA
	assignments assignmentCache
	certs       certCache

	// fetchCertValid is the test seam behind certValidCached; nil means
	// s.store.CertValid. The stream sweep bypasses it (and the cache) on
	// purpose — see certCache.
	fetchCertValid func(ctx context.Context, serial *big.Int, agentID uuid.UUID) (bool, error)
}

func New(st *store.Store, authority *ca.CA) *Server {
	return &Server{store: st, ca: authority}
}

// Register attaches both services to a grpc.Server.
func (s *Server) Register(g *grpc.Server) {
	pb.RegisterEnrollmentServiceServer(g, s)
	pb.RegisterAgentServiceServer(g, s)
}

// Enroll consumes a one-time join token and issues the agent's first
// certificate. This is the only RPC that works without a client cert.
func (s *Server) Enroll(ctx context.Context, req *pb.EnrollRequest) (*pb.EnrollResponse, error) {
	if req.GetJoinToken() == "" || len(req.GetCsrDer()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "join_token and csr_der are required")
	}

	// Validate the CSR before touching the single-use token so a malformed
	// CSR is rejected without consuming anything.
	if err := ca.ValidateCSR(req.GetCsrDer()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR rejected: %v", err)
	}

	probeAddress := req.GetProbeAddress()
	if probeAddress == "" {
		if p, ok := peer.FromContext(ctx); ok {
			if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
				probeAddress = host
			}
		}
		// With listen.proxy_protocol the observed source is the agent's
		// real pre-proxy egress address; without it, behind the
		// SNI-passthrough proxy, it is the PROXY itself. Either way it is
		// an egress address, not necessarily probeable (NAT), so
		// --probe-address remains the recommendation.
		slog.Warn("enrollment without --probe-address: falling back to observed source, "+
			"which may be a NAT egress or (without listen.proxy_protocol) the proxy's address",
			"observed", probeAddress, "hostname", req.GetHostname())
	}

	var certDER []byte
	var notAfter time.Time
	csrHash := sha256.Sum256(req.GetCsrDer())
	agentID, siteID, err := s.store.EnrollAgent(ctx,
		req.GetJoinToken(), req.GetHostname(), probeAddress, req.GetAgentVersion(), csrHash[:],
		func(agentID uuid.UUID) (store.IssuedCert, error) {
			der, serial, na, err := s.ca.SignAgentCSR(req.GetCsrDer(), agentID, req.GetHostname())
			if err != nil {
				return store.IssuedCert{}, err
			}
			certDER, notAfter = der, na
			return store.IssuedCert{
				Serial:    serial,
				NotBefore: time.Now().Add(-5 * time.Minute),
				NotAfter:  na,
			}, nil
		})
	if err == store.ErrTokenInvalid {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		slog.Error("enrollment failed", "err", err)
		return nil, status.Error(codes.Internal, "enrollment failed")
	}

	slog.Info("agent enrolled", "agent", agentID, "site", siteID,
		"hostname", req.GetHostname(), "probe_address", probeAddress)
	return &pb.EnrollResponse{
		AgentId:     agentID.String(),
		CertDer:     certDER,
		CaBundleDer: s.ca.BundleDER(),
		NotAfter:    timestamppb.New(notAfter),
	}, nil
}

// forcedRebuildTicks bounds how long a config edit made behind the store's
// back (manual SQL — nothing bumps the generation) can go unnoticed by a
// connected agent: 10 ticks × 30s = 5 minutes.
const forcedRebuildTicks = 10

// StreamConfig registers the agent as connected and pushes config snapshots:
// a full snapshot on connect (unless the agent already runs it), then a fresh
// full snapshot whenever a rebuild on the liveness tick yields a new hash.
// Admin CLI changes converge within one tick (~30 s) — the CLI runs in a
// separate process, so the database is the only change-propagation medium.
func (s *Server) StreamConfig(hello *pb.AgentHello, stream grpc.ServerStreamingServer[pb.ConfigSnapshot]) error {
	ctx := stream.Context()
	id, err := s.authenticateAgent(ctx)
	if err != nil {
		return err
	}
	slog.Info("agent connected", "agent", id.AgentID, "version", hello.GetAgentVersion())

	buildSnapshot := func() (*pb.ConfigSnapshot, error) {
		in, err := s.store.LoadAgentConfigInputs(ctx, id.AgentID)
		if err != nil {
			return nil, err
		}
		return meshexpand.BuildSnapshot(in)
	}

	// The config version (a single-row DB counter every config write path
	// bumps — the admin CLI writes from its own process, so the signal
	// must live in the DB) is snapshotted BEFORE each build: a write that
	// lands mid-build bumps it, so the next tick compares unequal and
	// rebuilds again — a build can never mask the write it raced.
	lastVer, err := s.store.ConfigDBVersion(ctx)
	if err != nil {
		slog.Error("config version read failed", "agent", id.AgentID, "err", err)
		return status.Error(codes.Unavailable, "config unavailable")
	}
	snapshot, err := buildSnapshot()
	if err != nil {
		slog.Error("config snapshot build failed", "agent", id.AgentID, "err", err)
		return status.Error(codes.Unavailable, "config unavailable")
	}
	if hello.GetConfigHash() != snapshot.GetConfigHash() {
		if err := stream.Send(snapshot); err != nil {
			return err
		}
		slog.Info("config snapshot sent", "agent", id.AgentID,
			"hash", snapshot.GetConfigHash(), "probes", len(snapshot.GetProbes()))
	}
	if err := s.store.TouchAgent(ctx, id.AgentID, hello.GetAgentVersion(), snapshot.GetConfigHash()); err != nil {
		slog.Error("touch agent failed", "err", err)
	}

	// Keep the stream open; periodically refresh liveness and re-check the
	// certificate so revocation cuts live streams within a minute.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	sinceRebuild := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("agent disconnected", "agent", id.AgentID)
			return nil
		case <-ticker.C:
			// Fail closed: the DB is the sole revocation authority, so a
			// stream whose certificate cannot be confirmed valid does not
			// stay up. The agent reconnects with backoff once the check
			// passes again.
			valid, err := s.store.CertValid(ctx, id.Cert.SerialNumber, id.AgentID)
			if err != nil {
				slog.Error("dropping stream: certificate validity unconfirmable", "agent", id.AgentID, "err", err)
				return status.Error(codes.Unavailable, "certificate validity check failed")
			}
			if !valid {
				slog.Info("dropping stream for revoked certificate", "agent", id.AgentID)
				return status.Error(codes.PermissionDenied, "certificate revoked")
			}
			// Rebuild only when a config write happened since the last
			// build: config_version covers every store write path that can
			// change expansion — CLI processes included — so at steady
			// state N connected agents cost N single-row point reads per
			// tick instead of N four-query batches + snapshot hashes. A
			// periodic forced rebuild backstops config edited behind the
			// store's back (manual SQL) and version-read failures.
			sinceRebuild++
			ver, verErr := s.store.ConfigDBVersion(ctx)
			if verErr != nil {
				slog.Error("config version read failed", "agent", id.AgentID, "err", verErr)
			}
			if verErr == nil && ver != lastVer || sinceRebuild >= forcedRebuildTicks {
				// Config staleness must not kill the stream (liveness and
				// revocation checking matter more): a failed rebuild keeps
				// the last snapshot, leaves lastVer/sinceRebuild untouched,
				// and thereby retries next tick.
				if fresh, err := buildSnapshot(); err != nil {
					slog.Error("config snapshot rebuild failed", "agent", id.AgentID, "err", err)
				} else {
					if fresh.GetConfigHash() != snapshot.GetConfigHash() {
						if err := stream.Send(fresh); err != nil {
							return err
						}
						snapshot = fresh
						slog.Info("config snapshot sent", "agent", id.AgentID,
							"hash", snapshot.GetConfigHash(), "probes", len(snapshot.GetProbes()))
					}
					if verErr == nil {
						lastVer = ver
					}
					sinceRebuild = 0
				}
			}
			if err := s.store.TouchAgent(ctx, id.AgentID, hello.GetAgentVersion(), snapshot.GetConfigHash()); err != nil {
				slog.Error("touch agent failed", "err", err)
			}
		}
	}
}

// PushResults ingests a result batch. The agent identity comes exclusively
// from the mTLS certificate; each result's (probe, target) pair must match
// the agent's current expanded config or the row is rejected (direction
// identity stays unforgeable, and results for deleted/disabled probes are
// dropped so admin cleanup of their series is durable). Rejections are
// counted and logged, never silent.
func (s *Server) PushResults(ctx context.Context, req *pb.PushResultsRequest) (*pb.PushResultsResponse, error) {
	id, err := s.authenticateAgent(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetResults()) > maxBatchSize {
		return nil, status.Errorf(codes.InvalidArgument, "batch of %d exceeds the %d-result limit",
			len(req.GetResults()), maxBatchSize)
	}

	assigned, err := s.agentProbeMap(ctx, id.AgentID)
	if err != nil {
		slog.Error("probe assignment load failed", "agent", id.AgentID, "err", err)
		return nil, status.Error(codes.Unavailable, "assignment check failed, retry")
	}

	now := time.Now()
	rows := make([]store.ResultRow, 0, len(req.GetResults()))
	rejected := 0
	for _, r := range req.GetResults() {
		row, err := resultToRow(r, now)
		if err != nil {
			rejected++
			slog.Warn("rejecting malformed probe result", "agent", id.AgentID, "err", err)
			continue
		}
		if a, ok := assigned[row.ProbeID]; !ok || a.TargetID != row.TargetID {
			rejected++
			slog.Warn("rejecting result for probe not assigned to agent",
				"agent", id.AgentID, "target", row.TargetID, "probe", row.ProbeID)
			continue
		}
		rows = append(rows, row)
	}

	// Drop accounting + insert + outage bookkeeping happen in one
	// transaction: hysteresis only ever sees rows the insert genuinely added,
	// so replayed spool batches (dedupe-skipped) can never advance a failure
	// streak twice, and the drop counter only ever advances in the same
	// commit as the batch, so a retried push can never re-add it.
	tx, err := s.store.Begin(ctx)
	if err != nil {
		slog.Error("result tx begin failed", "agent", id.AgentID, "err", err)
		// Unavailable: the agent keeps the batch spooled and retries.
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	defer tx.Rollback(ctx)
	if req.DroppedTotal != nil {
		// Total-reporting agent: fold the idempotent delta. The legacy field
		// is passed along only for the agent's first total-bearing report,
		// where it bounds what is genuinely new (see droppedDelta).
		if total := req.GetDroppedTotal(); total > 0 {
			delta, reset, err := store.RecordDroppedTotalTx(ctx, tx, id.AgentID, total, req.GetDroppedSinceLastPush())
			if err != nil {
				slog.Error("drop accounting failed", "agent", id.AgentID, "err", err)
				return nil, status.Error(codes.Unavailable, "drop accounting failed, retry")
			}
			if reset {
				slog.Warn("agent drop total went backwards, spool state reset assumed",
					"agent", id.AgentID, "total", total, "counted", delta)
			} else if delta > 0 {
				slog.Warn("agent reported spooled results dropped",
					"agent", id.AgentID, "new", delta, "total", total)
			}
		}
	} else if req.GetDroppedSinceLastPush() > 0 {
		slog.Warn("agent reported spooled results dropped (legacy delta)",
			"agent", id.AgentID, "dropped", req.GetDroppedSinceLastPush())
		if err := store.RecordDroppedResultsTx(ctx, tx, id.AgentID, req.GetDroppedSinceLastPush()); err != nil {
			slog.Error("drop accounting failed", "agent", id.AgentID, "err", err)
			return nil, status.Error(codes.Unavailable, "drop accounting failed, retry")
		}
	}
	inserted, err := store.InsertResultsTx(ctx, tx, id.AgentID, rows)
	if err != nil {
		slog.Error("result insert failed", "agent", id.AgentID, "count", len(rows), "err", err)
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	transitions, err := outage.Apply(ctx, tx, id.AgentID, toOutageResults(inserted, assigned))
	if err != nil {
		slog.Error("outage bookkeeping failed", "agent", id.AgentID, "err", err)
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	changes, err := pathwatch.Apply(ctx, tx, id.AgentID, toPathRuns(inserted))
	if err != nil {
		slog.Error("pathwatch bookkeeping failed", "agent", id.AgentID, "err", err)
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	mtuChanges, err := mtuwatch.Apply(ctx, tx, id.AgentID, toMTURuns(inserted))
	if err != nil {
		slog.Error("mtuwatch bookkeeping failed", "agent", id.AgentID, "err", err)
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("result tx commit failed", "agent", id.AgentID, "err", err)
		return nil, status.Error(codes.Unavailable, "result insert failed, retry")
	}
	for _, tr := range transitions {
		if tr.Opened {
			slog.Warn("outage opened", "agent", id.AgentID, "probe", tr.ProbeID, "kind", tr.Kind, "since", tr.At)
		} else {
			slog.Info("outage closed", "agent", id.AgentID, "probe", tr.ProbeID, "kind", tr.Kind, "at", tr.At)
		}
	}
	for _, ch := range changes {
		slog.Warn("traceroute path changed", "agent", id.AgentID, "probe", ch.ProbeID, "event", ch.EventID)
	}
	for _, ch := range mtuChanges {
		slog.Warn("path MTU changed", "agent", id.AgentID, "probe", ch.ProbeID,
			"old_bytes", ch.OldMTU, "new_bytes", ch.NewMTU, "black_hole", ch.NewBlack, "event", ch.EventID)
	}
	if rejected > 0 {
		slog.Warn("push contained rejected results", "agent", id.AgentID,
			"accepted", len(rows), "rejected", rejected)
	}
	// Accepted covers every row this push consumed (dedupe-skipped rows were
	// accepted on an earlier push): the agent must not re-send any of them.
	return &pb.PushResultsResponse{Accepted: uint32(len(rows))}, nil
}

// RenewCert issues a fresh certificate to an already-authenticated agent.
func (s *Server) RenewCert(ctx context.Context, req *pb.RenewCertRequest) (*pb.RenewCertResponse, error) {
	id, err := s.authenticateAgent(ctx)
	if err != nil {
		return nil, err
	}
	// Renewal mints a FRESH unrevoked certificate, so a 30s-stale cached
	// auth is not good enough here: a just-revoked serial could otherwise
	// convert its cache window into a brand-new credential and escape
	// revocation entirely. Re-check uncached immediately before issuance.
	valid, err := s.store.CertValid(ctx, id.Cert.SerialNumber, id.AgentID)
	if err != nil {
		slog.Error("renewal revocation re-check failed", "err", err)
		return nil, status.Error(codes.Internal, "certificate check failed")
	}
	if !valid {
		return nil, status.Error(codes.PermissionDenied, "certificate revoked or unknown")
	}
	certDER, serial, notAfter, err := s.ca.SignAgentCSR(req.GetCsrDer(), id.AgentID, id.Cert.Subject.CommonName)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR rejected: %v", err)
	}
	if err := s.store.InsertCertificate(ctx, serial, id.AgentID,
		time.Now().Add(-5*time.Minute), notAfter); err != nil {
		slog.Error("record renewed certificate failed", "err", err)
		return nil, status.Error(codes.Internal, "renewal failed")
	}
	slog.Info("certificate renewed", "agent", id.AgentID, "not_after", notAfter)
	return &pb.RenewCertResponse{
		CertDer:     certDER,
		CaBundleDer: s.ca.BundleDER(),
		NotAfter:    timestamppb.New(notAfter),
	}, nil
}
