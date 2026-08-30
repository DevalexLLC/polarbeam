package pathwatch

// DB-backed pathwatch tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). White-box on purpose: the upsert stale guard
// exists for a two-transaction race Apply cannot reproduce single-threaded,
// so its SQL is pinned directly. Everything else goes through Apply, the
// way the ingest transaction calls it.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
)

func newPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func hash(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func pathCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (current, events int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM traceroute_current`).Scan(&current); err != nil {
		t.Fatalf("count traceroute_current: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM path_events`).Scan(&events); err != nil {
		t.Fatalf("count path_events: %v", err)
	}
	return current, events
}

func TestApplyLifecycle(t *testing.T) {
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, h []byte, reached bool, hops string) Run {
		return Run{ProbeID: probeID, TargetID: targetID, Time: at,
			DestReached: reached, PathHash: h, Hops: []byte(hops)}
	}

	// First complete sighting: current row, no event.
	changes, err := Apply(ctx, pool, agentID, []Run{run(t0, hash(1), true, `[{"ttl":1}]`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cur, ev := pathCounts(t, ctx, pool); len(changes) != 0 || cur != 1 || ev != 0 {
		t.Fatalf("first sighting: changes=%d current=%d events=%d, want 0/1/0", len(changes), cur, ev)
	}

	// Same path later: refresh only.
	if _, err := Apply(ctx, pool, agentID, []Run{run(t0.Add(time.Minute), hash(1), true, `[{"ttl":1}]`)}); err != nil {
		t.Fatalf("Apply refresh: %v", err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&updatedAt); err != nil {
		t.Fatalf("read current: %v", err)
	}
	if _, ev := pathCounts(t, ctx, pool); ev != 0 || !updatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("refresh: events=%d updated_at=%v, want 0 events and bumped timestamp", ev, updatedAt)
	}

	// Hash change: one event carrying old AND new identity.
	changes, err = Apply(ctx, pool, agentID, []Run{run(t0.Add(2*time.Minute), hash(2), true, `[{"ttl":2}]`)})
	if err != nil {
		t.Fatalf("Apply change: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("change: %d changes, want 1", len(changes))
	}
	var oldH, newH, oldHops, newHops []byte
	if err := pool.QueryRow(ctx,
		`SELECT old_path_hash, new_path_hash, old_hops, new_hops FROM path_events`).
		Scan(&oldH, &newH, &oldHops, &newHops); err != nil {
		t.Fatalf("read event: %v", err)
	}
	// jsonb normalizes whitespace, so compare the round-tripped spelling.
	if !bytes.Equal(oldH, hash(1)) || !bytes.Equal(newH, hash(2)) ||
		string(oldHops) != `[{"ttl": 1}]` || string(newHops) != `[{"ttl": 2}]` {
		t.Fatalf("event = %x->%x hops %s->%s, want full old/new identity", oldH, newH, oldHops, newHops)
	}

	// Stale spool replay of the ORIGINAL path: ignored, no flap event.
	if _, err := Apply(ctx, pool, agentID, []Run{run(t0, hash(1), true, `[{"ttl":1}]`)}); err != nil {
		t.Fatalf("Apply stale replay: %v", err)
	}
	// An incomplete newer run must not count either.
	if _, err := Apply(ctx, pool, agentID, []Run{run(t0.Add(3*time.Minute), hash(3), false, `[]`)}); err != nil {
		t.Fatalf("Apply incomplete: %v", err)
	}
	if cur, ev := pathCounts(t, ctx, pool); cur != 1 || ev != 1 {
		t.Fatalf("after replay+incomplete: current=%d events=%d, want 1/1", cur, ev)
	}
	var curHash []byte
	if err := pool.QueryRow(ctx,
		`SELECT path_hash FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&curHash); err != nil {
		t.Fatalf("read current after replay: %v", err)
	}
	if !bytes.Equal(curHash, hash(2)) {
		t.Fatalf("current hash = %x, want the change's hash", curHash)
	}
}

// TestUpsertCurrentStaleGuard pins the SQL guard for the concurrent
// first-sighting race: FOR UPDATE cannot lock a missing row, so two initial
// transactions can both decide to insert — whatever the commit order, the
// NEWER run must stay current.
func TestUpsertCurrentStaleGuard(t *testing.T) {
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, h []byte) Run {
		return Run{ProbeID: probeID, TargetID: targetID, Time: at,
			DestReached: true, PathHash: h, Hops: []byte(`[]`)}
	}

	if applied, err := upsertCurrent(ctx, pool, agentID, run(t0.Add(time.Minute), hash(2))); err != nil || !applied {
		t.Fatalf("newer first sighting: applied=%v err=%v", applied, err)
	}
	// The racing OLDER first sighting must be skipped, not clobber it.
	if applied, err := upsertCurrent(ctx, pool, agentID, run(t0, hash(1))); err != nil || applied {
		t.Fatalf("older racer: applied=%v err=%v, want skipped", applied, err)
	}
	var curHash []byte
	if err := pool.QueryRow(ctx,
		`SELECT path_hash FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&curHash); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(curHash, hash(2)) {
		t.Fatalf("current hash = %x, want the newer run's", curHash)
	}
}
