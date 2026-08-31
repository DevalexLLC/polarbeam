package mtuwatch_test

// DB-backed mtuwatch tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin the SQL semantics — the seed-guarded
// first-sighting race, the ON CONFLICT stale guard on path_mtu_current,
// and event emission — which the pure decide tests cannot exercise.
// Black-box (unlike pathwatch's white-box file): everything goes through
// Apply, the way the ingest transaction calls it.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
)

func newPool(t testing.TB) (context.Context, *pgxpool.Pool) {
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

func counts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (current, events int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM path_mtu_current`).Scan(&current); err != nil {
		t.Fatalf("count path_mtu_current: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM path_mtu_events`).Scan(&events); err != nil {
		t.Fatalf("count path_mtu_events: %v", err)
	}
	return current, events
}

func TestApplyLifecycle(t *testing.T) {
	t.Parallel()
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	rtt := int32(311)
	run := func(at time.Time, mtu int32, black, usable bool) mtuwatch.Run {
		return mtuwatch.Run{
			ProbeID: probeID, TargetID: targetID, Time: at,
			LargestOK: mtu, SmallestFailed: 0, NextHopMTU: 0,
			IPVersion: 4, BlackHole: black, RttUS: &rtt, Usable: usable,
		}
	}

	// First sighting: current row, no event.
	changes, err := mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{run(t0, 1500, false, true)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cur, ev := counts(t, ctx, pool); len(changes) != 0 || cur != 1 || ev != 0 {
		t.Fatalf("first sighting: changes=%d current=%d events=%d, want 0/1/0", len(changes), cur, ev)
	}

	// Same measurement later: refresh, still no event.
	if _, err := mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{run(t0.Add(time.Minute), 1500, false, true)}); err != nil {
		t.Fatalf("Apply refresh: %v", err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM path_mtu_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&updatedAt); err != nil {
		t.Fatalf("read current: %v", err)
	}
	if _, ev := counts(t, ctx, pool); ev != 0 || !updatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("refresh: events=%d updated_at=%v, want 0 events and bumped timestamp", ev, updatedAt)
	}

	// MTU drops: one event recording old and new.
	changes, err = mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{run(t0.Add(2*time.Minute), 1400, true, true)})
	if err != nil {
		t.Fatalf("Apply change: %v", err)
	}
	if len(changes) != 1 || changes[0].OldMTU != 1500 || changes[0].NewMTU != 1400 || !changes[0].NewBlack {
		t.Fatalf("change: %+v, want 1500->1400 black hole", changes)
	}
	var oldB, newB bool
	var oldM, newM int32
	if err := pool.QueryRow(ctx,
		`SELECT old_mtu_bytes, new_mtu_bytes, old_black_hole, new_black_hole FROM path_mtu_events`,
	).Scan(&oldM, &newM, &oldB, &newB); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if oldM != 1500 || newM != 1400 || oldB || !newB {
		t.Fatalf("event row = %d->%d black %v->%v, want 1500->1400 false->true", oldM, newM, oldB, newB)
	}

	// Stale spool replay: ignored, no second event.
	if _, err := mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{run(t0, 1500, false, true)}); err != nil {
		t.Fatalf("Apply replay: %v", err)
	}
	// Unusable (non-converged) run: ignored even though the values differ.
	if _, err := mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{run(t0.Add(3*time.Minute), 900, false, false)}); err != nil {
		t.Fatalf("Apply unusable: %v", err)
	}
	var mtu int32
	if err := pool.QueryRow(ctx,
		`SELECT largest_ok_bytes FROM path_mtu_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&mtu); err != nil {
		t.Fatalf("read current: %v", err)
	}
	if cur, ev := counts(t, ctx, pool); cur != 1 || ev != 1 || mtu != 1400 {
		t.Fatalf("after replay+unusable: current=%d events=%d mtu=%d, want 1/1/1400", cur, ev, mtu)
	}
}

// TestConcurrentFirstSighting drives the race the seed insert exists for:
// two transactions Apply a first sighting of the same series. The loser
// must block on the winner's speculative seed insert and then fold against
// the COMMITTED row — here the older run wins the race, so the newer racer
// must record a change whose old identity is the committed winner's MTU.
func TestConcurrentFirstSighting(t *testing.T) {
	t.Parallel()
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, mtu int32) mtuwatch.Run {
		return mtuwatch.Run{ProbeID: probeID, TargetID: targetID, Time: at,
			LargestOK: mtu, IPVersion: 4, Usable: true}
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback(ctx)
	if _, err := mtuwatch.Apply(ctx, tx1, agentID, []mtuwatch.Run{run(t0, 1500)}); err != nil {
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
		if _, err := mtuwatch.Apply(ctx, tx2, agentID, []mtuwatch.Run{run(t0.Add(time.Minute), 1400)}); err != nil {
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

	var oldM, newM int32
	if err := pool.QueryRow(ctx,
		`SELECT old_mtu_bytes, new_mtu_bytes FROM path_mtu_events WHERE agent_id = $1`, agentID).
		Scan(&oldM, &newM); err != nil {
		t.Fatalf("read race event: %v", err)
	}
	if oldM != 1500 || newM != 1400 {
		t.Fatalf("race event = %d->%d, want committed winner's 1500 as old identity", oldM, newM)
	}
	var mtu int32
	if err := pool.QueryRow(ctx,
		`SELECT largest_ok_bytes FROM path_mtu_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&mtu); err != nil {
		t.Fatal(err)
	}
	if mtu != 1400 {
		t.Fatalf("current mtu = %d, want the newer racer's 1400", mtu)
	}
}

// TestApplyMultipleRunsOneSeries pins the in-push fold: shuffled runs sort
// by time, every intermediate change emits its event, and only the final
// state is committed.
func TestApplyMultipleRunsOneSeries(t *testing.T) {
	t.Parallel()
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	run := func(at time.Time, mtu int32) mtuwatch.Run {
		return mtuwatch.Run{ProbeID: probeID, TargetID: targetID, Time: at,
			LargestOK: mtu, IPVersion: 4, Usable: true}
	}

	// Time order: t0:1500 insert, t1:1400 change, t2:1400 refresh, t3:1300 change.
	changes, err := mtuwatch.Apply(ctx, pool, agentID, []mtuwatch.Run{
		run(t0.Add(2*time.Minute), 1400),
		run(t0, 1500),
		run(t0.Add(3*time.Minute), 1300),
		run(t0.Add(time.Minute), 1400),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 2 ||
		changes[0].OldMTU != 1500 || changes[0].NewMTU != 1400 ||
		changes[1].OldMTU != 1400 || changes[1].NewMTU != 1300 {
		t.Fatalf("changes = %+v, want 1500->1400 then 1400->1300", changes)
	}
	var mtu int32
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT largest_ok_bytes, updated_at FROM path_mtu_current WHERE agent_id = $1 AND probe_id = $2`,
		agentID, probeID).Scan(&mtu, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if cur, ev := counts(t, ctx, pool); cur != 1 || ev != 2 || mtu != 1300 || !updatedAt.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("final = current %d events %d mtu %d at %v, want 1/2/1300 at t3", cur, ev, mtu, updatedAt)
	}
}
