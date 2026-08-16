package mtuwatch_test

// DB-backed mtuwatch tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin the SQL semantics — the ON CONFLICT
// stale guard on path_mtu_current and event emission — which the pure
// decide tests cannot exercise.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
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
