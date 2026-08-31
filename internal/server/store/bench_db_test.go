package store_test

// DB-backed ingest benchmark for InsertResultsTx, gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest) and run manually via
// `make bench` — never in CI. Each iteration inserts the identical batch
// inside a rolled-back transaction, so the table stays empty and every
// insert is genuinely fresh (the dedupe index never fires); Begin/Rollback
// are timed as real, constant round trips of the real ingest path.

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func benchRows(series, perSeries int, t0 time.Time) []store.ResultRow {
	rows := make([]store.ResultRow, 0, series*perSeries)
	for range series {
		probeID, targetID := uuid.New(), uuid.New()
		for j := range perSeries {
			rows = append(rows, store.ResultRow{
				Time: t0.Add(time.Duration(j) * time.Second), TargetID: targetID, ProbeID: probeID,
				ProbeType: 1, Status: 1, Sent: 1, Received: 1,
			})
		}
	}
	return rows
}

func BenchmarkInsertResultsTx(b *testing.B) {
	ctx, s := newStore(b)
	agentID := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	// Commit rows bracketing the timed window up front to materialize the
	// 1-day hypertable chunks it touches — BOTH ends, since the window can
	// straddle a chunk boundary: the iterations roll back, and without this
	// every one would pay the chunk-creation DDL (a fixed ~40ms that buries
	// the insert cost).
	seedTx, err := s.Begin(ctx)
	if err != nil {
		b.Fatalf("begin seed: %v", err)
	}
	warm := append(benchRows(1, 1, t0), benchRows(1, 1, t0.Add(time.Minute))...)
	if _, err := store.InsertResultsTx(ctx, seedTx, agentID, warm); err != nil {
		b.Fatalf("seed insert: %v", err)
	}
	if err := seedTx.Commit(ctx); err != nil {
		b.Fatalf("seed commit: %v", err)
	}
	// 50 series is the baseline push shape; the row counts bracket it.
	for _, rows := range []int{50, 250, 500} {
		batch := benchRows(50, rows/50, t0)
		b.Run(fmt.Sprintf("%drows", rows), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tx, err := s.Begin(ctx)
				if err != nil {
					b.Fatalf("begin: %v", err)
				}
				inserted, err := store.InsertResultsTx(ctx, tx, agentID, batch)
				if err != nil {
					b.Fatalf("InsertResultsTx: %v", err)
				}
				if len(inserted) != len(batch) {
					b.Fatalf("inserted = %d, want %d fresh rows", len(inserted), len(batch))
				}
				if err := tx.Rollback(ctx); err != nil {
					b.Fatalf("rollback: %v", err)
				}
			}
		})
	}
}
