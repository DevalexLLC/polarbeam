package pathwatch

// DB-backed pathwatch tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). White-box on purpose: the bulk upsert's stale
// guard is pinned directly (Apply's fold makes it unreachable), and the
// concurrent-first-sighting race — which the seed insert now resolves by
// blocking — is driven through Apply on two real transactions. Everything
// else goes through Apply, the way the ingest transaction calls it.

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

// TestBulkUpsertStaleGuard pins the bulk statement's updated_at guard
// directly: Apply's fold makes it unreachable (run times always beat the
// locked row's, and sentinel-time runs are dropped before grouping), so
// only a white-box call can prove an older write cannot clobber a newer
// row — and that the skip surfaces as an error, never a silent no-op.
func TestBulkUpsertStaleGuard(t *testing.T) {
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, h []byte) Run {
		return Run{ProbeID: probeID, TargetID: targetID, Time: at,
			DestReached: true, PathHash: h, Hops: []byte(`[]`)}
	}

	if err := bulkUpsertCurrent(ctx, pool, agentID, []Run{run(t0.Add(time.Minute), hash(2))}); err != nil {
		t.Fatalf("newer first write: %v", err)
	}
	// An OLDER write must be skipped by the guard — reported loudly as a
	// shortfall error — and must not clobber the row.
	if err := bulkUpsertCurrent(ctx, pool, agentID, []Run{run(t0, hash(1))}); err == nil {
		t.Fatal("older write: want a shortfall error, got nil")
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

// TestConcurrentFirstSighting drives the race the seed insert exists for:
// two transactions Apply a first sighting of the same series. The loser
// must block on the winner's speculative seed insert, then fold against
// the COMMITTED row — the old per-run code recovered via a stale-guard
// re-lock; the bulk form prevents the race outright. Reproducible now
// precisely because the protection is blocking.
func TestConcurrentFirstSighting(t *testing.T) {
	ctx, pool := newPool(t)
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	// Case 1: newer run's tx wins the race; older racer must fold to skip —
	// no event, newer row survives.
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	run := func(at time.Time, h []byte, hops string) Run {
		return Run{ProbeID: probeID, TargetID: targetID, Time: at,
			DestReached: true, PathHash: h, Hops: []byte(hops)}
	}
	race := func(first, second Run) {
		t.Helper()
		tx1, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx1: %v", err)
		}
		defer tx1.Rollback(ctx)
		if _, err := Apply(ctx, tx1, agentID, []Run{first}); err != nil {
			t.Fatalf("Apply tx1: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			tx2, err := pool.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			defer tx2.Rollback(ctx)
			if _, err := Apply(ctx, tx2, agentID, []Run{second}); err != nil {
				done <- err
				return
			}
			done <- tx2.Commit(ctx)
		}()
		// Give tx2 time to reach the seed insert and block on tx1.
		time.Sleep(200 * time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("tx2 finished before tx1 committed (not blocked): %v", err)
		default:
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatalf("commit tx1: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("tx2: %v", err)
		}
	}

	race(run(t0.Add(time.Minute), hash(2), `[{"ttl":2}]`), run(t0, hash(1), `[{"ttl":1}]`))
	var curHash []byte
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT path_hash FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&curHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM path_events WHERE agent_id = $1`, agentID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(curHash, hash(2)) || events != 0 {
		t.Fatalf("older racer: hash=%x events=%d, want newer hash and no event", curHash, events)
	}

	// Case 2: OLDER run's tx wins; the newer racer must record a change
	// whose old identity is the COMMITTED winner's path — the exact event
	// a fold against never-current state would get wrong.
	agentID, probeID, targetID = uuid.New(), uuid.New(), uuid.New()
	race(run(t0, hash(1), `[{"ttl":1}]`), run(t0.Add(time.Minute), hash(3), `[{"ttl":3}]`))
	var oldH, newH []byte
	if err := pool.QueryRow(ctx,
		`SELECT old_path_hash, new_path_hash FROM path_events WHERE agent_id = $1`, agentID).
		Scan(&oldH, &newH); err != nil {
		t.Fatalf("read race event: %v", err)
	}
	if !bytes.Equal(oldH, hash(1)) || !bytes.Equal(newH, hash(3)) {
		t.Fatalf("race event = %x->%x, want committed winner's hash as old identity", oldH, newH)
	}
	if err := pool.QueryRow(ctx,
		`SELECT path_hash FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&curHash); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(curHash, hash(3)) {
		t.Fatalf("current hash = %x, want the newer racer's", curHash)
	}
}

// TestApplyMultipleRunsOneSeries pins the in-push fold: shuffled runs sort
// by time, every intermediate change emits its event, and only the final
// state is committed.
func TestApplyMultipleRunsOneSeries(t *testing.T) {
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, h []byte) Run {
		return Run{ProbeID: probeID, TargetID: targetID, Time: at,
			DestReached: true, PathHash: h, Hops: []byte(`[]`)}
	}

	// Time order: t0:A insert, t1:B change, t2:B refresh, t3:C change.
	changes, err := Apply(ctx, pool, agentID, []Run{
		run(t0.Add(2*time.Minute), hash(2)),
		run(t0, hash(1)),
		run(t0.Add(3*time.Minute), hash(3)),
		run(t0.Add(time.Minute), hash(2)),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2 (A→B, B→C)", len(changes))
	}
	rows, err := pool.Query(ctx,
		`SELECT old_path_hash, new_path_hash FROM path_events WHERE agent_id = $1 ORDER BY time`, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2][]byte
	for rows.Next() {
		var oldH, newH []byte
		if err := rows.Scan(&oldH, &newH); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2][]byte{oldH, newH})
	}
	if len(got) != 2 ||
		!bytes.Equal(got[0][0], hash(1)) || !bytes.Equal(got[0][1], hash(2)) ||
		!bytes.Equal(got[1][0], hash(2)) || !bytes.Equal(got[1][1], hash(3)) {
		t.Fatalf("events = %x, want A→B then B→C", got)
	}
	var curHash []byte
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT path_hash, updated_at FROM traceroute_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&curHash, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(curHash, hash(3)) || !updatedAt.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("current = %x@%v, want C at t3", curHash, updatedAt)
	}
}
