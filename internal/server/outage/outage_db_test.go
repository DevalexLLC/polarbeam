package outage_test

// DB-backed outage tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin the SQL the pure step tests cannot
// exercise: the FOR UPDATE OF ss lock join deriving OpenKind, the per-kind
// ON CONFLICT arbiter indexes, and the widened series_state bulk upsert —
// across the full degraded lifecycle (open → escalate → de-escalate →
// close) against the real migrated schema.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/outage"
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

func TestDegradedLifecycle(t *testing.T) {
	t.Parallel()
	ctx, pool := newPool(t)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	detail := "latency at or above critical threshold (40ms)"
	res := func(sec int, kind byte) outage.Result {
		r := outage.Result{
			ProbeID: probeID, TargetID: targetID, ProbeType: 1,
			Time: t0.Add(time.Duration(sec) * time.Second),
			OK:   kind != 'f', StatusCode: 1,
		}
		if kind == 'f' {
			r.StatusCode = 2
			r.Error = "timeout"
		}
		if kind == 'd' {
			r.Degraded = true
			r.DegradedDetail = detail
		}
		return r
	}
	apply := func(sec int, pattern string) []outage.Transition {
		t.Helper()
		var rs []outage.Result
		for i, c := range pattern {
			rs = append(rs, res(sec+i, byte(c)))
		}
		trs, err := outage.Apply(ctx, pool, agentID, rs)
		if err != nil {
			t.Fatalf("Apply %q: %v", pattern, err)
		}
		return trs
	}
	event := func(kind string) (openedAt time.Time, closedAt *time.Time, openError *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT opened_at, closed_at, open_error FROM outage_events
			WHERE agent_id = $1 AND probe_id = $2 AND kind = $3
			ORDER BY opened_at DESC LIMIT 1`,
			agentID, probeID, kind).Scan(&openedAt, &closedAt, &openError); err != nil {
			t.Fatalf("read %s event: %v", kind, err)
		}
		return
	}

	// Three breaching successes open probe_degraded at the first breach.
	trs := apply(0, "ddd")
	if len(trs) != 1 || !trs[0].Opened || trs[0].Kind != outage.KindProbeDegraded {
		t.Fatalf("ddd transitions = %+v, want one degraded open", trs)
	}
	openedAt, closedAt, openError := event("probe_degraded")
	if !openedAt.Equal(t0) || closedAt != nil || openError == nil || *openError != detail {
		t.Fatalf("degraded event = %v/%v/%v, want open at t0 with detail", openedAt, closedAt, openError)
	}

	// Escalation: three failures close degraded and open failing at the
	// first failure — contiguous history.
	trs = apply(10, "fff")
	if len(trs) != 2 || trs[0].Opened || trs[0].Kind != outage.KindProbeDegraded ||
		!trs[1].Opened || trs[1].Kind != outage.KindProbeFailing {
		t.Fatalf("fff transitions = %+v, want degraded close then failing open", trs)
	}
	_, closedAt, _ = event("probe_degraded")
	failOpenedAt, _, _ := event("probe_failing")
	if closedAt == nil || !closedAt.Equal(t0.Add(10*time.Second)) || !failOpenedAt.Equal(*closedAt) {
		t.Fatalf("escalation not contiguous: degraded closed %v, failing opened %v", closedAt, failOpenedAt)
	}

	// De-escalation: three breaching successes close failing AND reopen
	// degraded in the same batch, at the same instant.
	trs = apply(20, "ddd")
	if len(trs) != 2 || trs[0].Opened || trs[0].Kind != outage.KindProbeFailing ||
		!trs[1].Opened || trs[1].Kind != outage.KindProbeDegraded {
		t.Fatalf("recovery transitions = %+v, want failing close then degraded open", trs)
	}
	openedAt, closedAt, _ = event("probe_degraded")
	if closedAt != nil || !openedAt.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("reopened degraded = %v/%v, want open at t0+20s", openedAt, closedAt)
	}

	// Three clean successes close it; the series ends with no open event.
	trs = apply(30, "ooo")
	if len(trs) != 1 || trs[0].Opened || trs[0].Kind != outage.KindProbeDegraded {
		t.Fatalf("ooo transitions = %+v, want one degraded close", trs)
	}
	var openEventID *uuid.UUID
	var consecClean int
	if err := pool.QueryRow(ctx, `
		SELECT open_event_id, consec_clean FROM series_state
		WHERE agent_id = $1 AND probe_id = $2`, agentID, probeID).Scan(&openEventID, &consecClean); err != nil {
		t.Fatalf("read series_state: %v", err)
	}
	if openEventID != nil || consecClean != 3 {
		t.Fatalf("final state = open_event_id %v consec_clean %d, want nil/3", openEventID, consecClean)
	}
	var open int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outage_events WHERE agent_id = $1 AND closed_at IS NULL`,
		agentID).Scan(&open); err != nil {
		t.Fatalf("count open events: %v", err)
	}
	if open != 0 {
		t.Fatalf("open events = %d, want 0", open)
	}
}
