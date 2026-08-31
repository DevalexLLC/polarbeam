package mtuwatch_test

// DB-backed ingest benchmarks for mtuwatch.Apply, gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest) and run manually via
// `make bench` — never in CI. One run per series: a real push carries at
// most one path-MTU probe per probe ID. Each iteration folds the identical
// batch inside a rolled-back transaction against the committed baseline, so
// nothing accumulates and ns/op stays comparable across -benchtime;
// Begin/Rollback are timed as real, constant round trips of the real path.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
)

const benchSeries = 50

func benchRuns(probes, targets []uuid.UUID, at time.Time, mtu int32, black bool) []mtuwatch.Run {
	rtt := int32(311)
	runs := make([]mtuwatch.Run, len(probes))
	for i := range probes {
		runs[i] = mtuwatch.Run{
			ProbeID: probes[i], TargetID: targets[i], Time: at,
			LargestOK: mtu, IPVersion: 4, BlackHole: black, RttUS: &rtt, Usable: true,
		}
	}
	return runs
}

func benchIDs(n int) (probes, targets []uuid.UUID) {
	probes, targets = make([]uuid.UUID, n), make([]uuid.UUID, n)
	for i := range probes {
		probes[i], targets[i] = uuid.New(), uuid.New()
	}
	return probes, targets
}

// BenchmarkApplyRefresh re-measures the committed MTU on every series: the
// quiet hot path — seed no-op, ordered lock, no events, bulk refresh.
func BenchmarkApplyRefresh(b *testing.B) {
	ctx, pool := newPool(b)
	agentID := uuid.New()
	probes, targets := benchIDs(benchSeries)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	if _, err := mtuwatch.Apply(ctx, pool, agentID, benchRuns(probes, targets, t0, 1500, false)); err != nil {
		b.Fatalf("seed Apply: %v", err)
	}
	batch := benchRuns(probes, targets, t0.Add(time.Minute), 1500, false)
	b.ReportAllocs()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		if _, err := mtuwatch.Apply(ctx, tx, agentID, batch); err != nil {
			b.Fatalf("Apply: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}

// BenchmarkApplyMTUChange drops every series' MTU: the bulk event insert
// rides along. The rollback restores the seeded value, so every iteration
// is a change.
func BenchmarkApplyMTUChange(b *testing.B) {
	ctx, pool := newPool(b)
	agentID := uuid.New()
	probes, targets := benchIDs(benchSeries)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	if _, err := mtuwatch.Apply(ctx, pool, agentID, benchRuns(probes, targets, t0, 1500, false)); err != nil {
		b.Fatalf("seed Apply: %v", err)
	}
	batch := benchRuns(probes, targets, t0.Add(time.Minute), 1400, true)
	b.ReportAllocs()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		changes, err := mtuwatch.Apply(ctx, tx, agentID, batch)
		if err != nil {
			b.Fatalf("Apply: %v", err)
		}
		if len(changes) != benchSeries {
			b.Fatalf("changes = %d, want one per series (%d)", len(changes), benchSeries)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}
