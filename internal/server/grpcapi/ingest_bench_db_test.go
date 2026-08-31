package grpcapi

// DB-backed benchmark of the whole PushResults transaction body — insert,
// outage, pathwatch, mtuwatch — without gRPC or mTLS auth, mirroring the
// statement sequence in (*Server).PushResults. Gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest) and run manually via
// `make bench` — never in CI.
//
// Setup commits one baseline push so every watcher has current state; each
// iteration then replays the identical steady-state batch — later
// timestamps, same path hashes and MTU values, all OK — inside a
// rolled-back transaction, so nothing accumulates and ns/op stays
// comparable across -benchtime.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
	"github.com/devalexllc/polarbeam/internal/server/outage"
	"github.com/devalexllc/polarbeam/internal/server/pathwatch"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/thresholds"
)

// benchPush is the baseline push shape: 40 latency series with 5 results
// each, plus 5 traceroute and 5 path-MTU series with one run each — 50
// series, 210 rows.
func benchPush(assigned map[uuid.UUID]probeAssignment, t0 time.Time) []store.ResultRow {
	// Critical thresholds far above the measurements: grading runs but the
	// steady-state batch never comes out degraded.
	crit := thresholds.T{LatencyCritUS: 1 << 40, LossCritPct: 100}
	rtt := int32(311)
	loss := float32(0)
	var rows []store.ResultRow
	base := func(probeID, targetID uuid.UUID, at time.Time, probeType int16) store.ResultRow {
		return store.ResultRow{
			Time: at, TargetID: targetID, ProbeID: probeID,
			ProbeType: probeType, Status: 1, Sent: 1, Received: 1,
			LossPct: &loss, RttAvgUS: &rtt,
		}
	}
	series := func(probeType int16) (uuid.UUID, uuid.UUID) {
		probeID, targetID := uuid.New(), uuid.New()
		assigned[probeID] = probeAssignment{TargetID: targetID, Crit: crit}
		return probeID, targetID
	}
	icmp := int16(pb.ProbeType_PROBE_TYPE_ICMP)
	trace := int16(pb.ProbeType_PROBE_TYPE_TRACEROUTE)
	pmtu := int16(pb.ProbeType_PROBE_TYPE_PATH_MTU)
	for range 40 {
		probeID, targetID := series(icmp)
		for j := range 5 {
			rows = append(rows, base(probeID, targetID, t0.Add(time.Duration(j)*time.Second), icmp))
		}
	}
	for range 5 {
		probeID, targetID := series(trace)
		r := base(probeID, targetID, t0, trace)
		r.Traceroute = &store.TraceroutePayload{
			DestReached: true,
			PathHash:    make([]byte, 32),
			Hops:        []byte(`[{"ttl":1}]`),
		}
		rows = append(rows, r)
	}
	for range 5 {
		probeID, targetID := series(pmtu)
		r := base(probeID, targetID, t0, pmtu)
		r.PathMtu = &store.PathMtuPayload{LargestOK: 1500, IPVersion: 4, RttUS: &rtt, Usable: true}
		rows = append(rows, r)
	}
	return rows
}

// ingestTx runs the PushResults transaction body over rows and hands the
// open transaction back for the caller to commit or roll back.
func ingestTx(ctx context.Context, b *testing.B, s *store.Store, agentID uuid.UUID,
	assigned map[uuid.UUID]probeAssignment, rows []store.ResultRow) pgx.Tx {
	b.Helper()
	tx, err := s.Begin(ctx)
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	inserted, err := store.InsertResultsTx(ctx, tx, agentID, rows)
	if err != nil {
		b.Fatalf("InsertResultsTx: %v", err)
	}
	if len(inserted) != len(rows) {
		b.Fatalf("inserted = %d, want %d fresh rows", len(inserted), len(rows))
	}
	if _, err := outage.Apply(ctx, tx, agentID, toOutageResults(inserted, assigned)); err != nil {
		b.Fatalf("outage.Apply: %v", err)
	}
	if _, err := pathwatch.Apply(ctx, tx, agentID, toPathRuns(inserted)); err != nil {
		b.Fatalf("pathwatch.Apply: %v", err)
	}
	if _, err := mtuwatch.Apply(ctx, tx, agentID, toMTURuns(inserted)); err != nil {
		b.Fatalf("mtuwatch.Apply: %v", err)
	}
	return tx
}

func BenchmarkIngestTx(b *testing.B) {
	ctx, s := ingestStore(b)
	agentID := uuid.New()
	assigned := make(map[uuid.UUID]probeAssignment)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	seed := benchPush(assigned, t0)
	if err := ingestTx(ctx, b, s, agentID, assigned, seed).Commit(ctx); err != nil {
		b.Fatalf("seed commit: %v", err)
	}
	// Same series (assigned already holds them), later timestamps.
	batch := make([]store.ResultRow, len(seed))
	copy(batch, seed)
	for i := range batch {
		batch[i].Time = batch[i].Time.Add(time.Minute)
	}
	// Materialize the 1-day hypertable chunks bracketing the timed window
	// (it may straddle a chunk boundary the committed seed did not touch):
	// the iterations roll back, so an unseeded chunk would be created and
	// discarded on every one, measuring DDL instead of ingest. Throwaway
	// identities, insert only — no watcher state.
	warmTx, err := s.Begin(ctx)
	if err != nil {
		b.Fatalf("begin warm: %v", err)
	}
	warm := []store.ResultRow{
		{Time: t0.Add(time.Minute), TargetID: uuid.New(), ProbeID: uuid.New(), ProbeType: 1, Status: 1, Sent: 1, Received: 1},
		{Time: t0.Add(2 * time.Minute), TargetID: uuid.New(), ProbeID: uuid.New(), ProbeType: 1, Status: 1, Sent: 1, Received: 1},
	}
	if _, err := store.InsertResultsTx(ctx, warmTx, agentID, warm); err != nil {
		b.Fatalf("warm insert: %v", err)
	}
	if err := warmTx.Commit(ctx); err != nil {
		b.Fatalf("warm commit: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := ingestTx(ctx, b, s, agentID, assigned, batch).Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}
