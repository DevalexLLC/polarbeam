package outage_test

// DB-backed ingest benchmarks for outage.Apply, gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest) and run manually via
// `make bench` — never in CI. Workloads are built inline rather than through
// internal/server/seed: GeneratePair feeds the aggregate pipeline and
// carries no probe/target identities, so adapting it here would couple the
// benchmarks to it for no fidelity gain.
//
// Each iteration folds the identical batch inside a transaction that is
// rolled back, so nothing accumulates against the committed baseline and
// ns/op stays comparable across -benchtime. Begin and Rollback are timed
// deliberately: they are real round trips of the real ingest path and
// constant per iteration.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/outage"
)

// benchSeries × benchPerSeries is the baseline push shape: 50 series, a
// handful of results each — the batch size the #126 bulk rewrite was sized
// against.
const (
	benchSeries    = 50
	benchPerSeries = 5
)

type benchSeriesID struct{ probe, target uuid.UUID }

func newBenchSeriesIDs(n int) []benchSeriesID {
	ids := make([]benchSeriesID, n)
	for i := range ids {
		ids[i] = benchSeriesID{probe: uuid.New(), target: uuid.New()}
	}
	return ids
}

// benchResults builds perSeries results per series, one second apart from
// t0, all-OK or all-failing.
func benchResults(ids []benchSeriesID, t0 time.Time, perSeries int, ok bool) []outage.Result {
	code := int16(1)
	if !ok {
		code = 2 // TIMEOUT
	}
	rs := make([]outage.Result, 0, len(ids)*perSeries)
	for _, id := range ids {
		for j := range perSeries {
			rs = append(rs, outage.Result{
				ProbeID:    id.probe,
				TargetID:   id.target,
				ProbeType:  1,
				Time:       t0.Add(time.Duration(j) * time.Second),
				OK:         ok,
				StatusCode: code,
			})
		}
	}
	return rs
}

// BenchmarkApplySteadyState is the quiet hot path: every series has
// committed state, every result is OK, no transitions — the pure bulk
// seed-noop/lock/fold/upsert shape.
func BenchmarkApplySteadyState(b *testing.B) {
	ctx, pool := newPool(b)
	agentID := uuid.New()
	ids := newBenchSeriesIDs(benchSeries)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	if _, err := outage.Apply(ctx, pool, agentID, benchResults(ids, t0, 1, true)); err != nil {
		b.Fatalf("seed Apply: %v", err)
	}
	batch := benchResults(ids, t0.Add(time.Minute), benchPerSeries, true)
	b.ReportAllocs()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		if _, err := outage.Apply(ctx, tx, agentID, batch); err != nil {
			b.Fatalf("Apply: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}

// BenchmarkApplyFirstSighting seeds nothing: the rollback discards the
// series_state rows, so every iteration exercises ensureStates' full
// speculative insert.
func BenchmarkApplyFirstSighting(b *testing.B) {
	ctx, pool := newPool(b)
	agentID := uuid.New()
	ids := newBenchSeriesIDs(benchSeries)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	batch := benchResults(ids, t0, benchPerSeries, true)
	b.ReportAllocs()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		if _, err := outage.Apply(ctx, tx, agentID, batch); err != nil {
			b.Fatalf("Apply: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}

// BenchmarkApplyTransitions crosses the failure threshold on every series:
// one openEvent QueryRow per series on top of the bulk statements — the
// remaining per-row cost on this path, and the number to watch.
func BenchmarkApplyTransitions(b *testing.B) {
	ctx, pool := newPool(b)
	agentID := uuid.New()
	ids := newBenchSeriesIDs(benchSeries)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	if _, err := outage.Apply(ctx, pool, agentID, benchResults(ids, t0, 1, true)); err != nil {
		b.Fatalf("seed Apply: %v", err)
	}
	batch := benchResults(ids, t0.Add(time.Minute), benchPerSeries, false)
	b.ReportAllocs()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		transitions, err := outage.Apply(ctx, tx, agentID, batch)
		if err != nil {
			b.Fatalf("Apply: %v", err)
		}
		if len(transitions) != benchSeries {
			b.Fatalf("transitions = %d, want one open per series (%d)", len(transitions), benchSeries)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatalf("rollback: %v", err)
		}
	}
}
