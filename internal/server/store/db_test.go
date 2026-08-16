package store_test

// DB-backed store tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin behavior that lives in the SQL, not the
// Go — dedupe-index semantics, ON CONFLICT upserts, and the FOR UPDATE
// locking the last-admin guard's race-proofing rests on — which the
// in-memory fakes used elsewhere in the test suite cannot exercise.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func newStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s, err := store.Connect(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	t.Cleanup(s.Close)
	return ctx, s
}

func insertResults(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID, rows []store.ResultRow) []store.ResultRow {
	t.Helper()
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	inserted, err := store.InsertResultsTx(ctx, tx, agentID, rows)
	if err != nil {
		t.Fatalf("InsertResultsTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inserted
}

func resultCount(t *testing.T, ctx context.Context, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM probe_results`).Scan(&n); err != nil {
		t.Fatalf("count probe_results: %v", err)
	}
	return n
}

// TestInsertResultsTxDedupe pins the spool-replay contract: the dedupe index
// is (agent_id, probe_id, time), replayed rows are dropped silently, and the
// returned slice holds only genuinely added rows so outage/pathwatch
// bookkeeping never double-counts.
func TestInsertResultsTxDedupe(t *testing.T) {
	ctx, s := newStore(t)
	agentA, agentB := uuid.New(), uuid.New()
	p1, p2 := uuid.New(), uuid.New()
	// Postgres stores timestamptz at microsecond precision; sub-microsecond
	// input would round and change the dedupe key.
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	row := func(probeID uuid.UUID, at time.Time) store.ResultRow {
		return store.ResultRow{
			Time: at, TargetID: uuid.New(), ProbeID: probeID,
			ProbeType: 1, Status: 1, Sent: 1, Received: 1,
		}
	}

	batch := []store.ResultRow{row(p1, t0), row(p2, t0)}
	if got := insertResults(t, ctx, s, agentA, batch); len(got) != 2 {
		t.Fatalf("fresh batch: inserted %d rows, want 2", len(got))
	}

	// Full replay (agent re-pushed an unacked spool batch): nothing new.
	if got := insertResults(t, ctx, s, agentA, batch); len(got) != 0 {
		t.Errorf("replayed batch: inserted %d rows, want 0", len(got))
	}
	if n := resultCount(t, ctx, s); n != 2 {
		t.Errorf("after replay: %d rows, want 2", n)
	}

	// Partial overlap: only the genuinely new row comes back.
	t1 := t0.Add(time.Second)
	if got := insertResults(t, ctx, s, agentA,
		[]store.ResultRow{row(p1, t0), row(p1, t1)}); len(got) != 1 || !got[0].Time.Equal(t1) {
		t.Errorf("overlapping batch: inserted %v, want exactly the t1 row", got)
	}

	// In-batch duplicate counts once.
	t2 := t0.Add(2 * time.Second)
	if got := insertResults(t, ctx, s, agentA,
		[]store.ResultRow{row(p2, t2), row(p2, t2)}); len(got) != 1 {
		t.Errorf("in-batch duplicate: inserted %d rows, want 1", len(got))
	}

	// The agent is part of the key: another agent may report the same
	// (probe_id, time) — directions are distinct series.
	if got := insertResults(t, ctx, s, agentB, []store.ResultRow{row(p1, t0)}); len(got) != 1 {
		t.Errorf("same probe/time from another agent: inserted %d rows, want 1", len(got))
	}
}

func createUser(t *testing.T, ctx context.Context, s *store.Store, username, role string) uuid.UUID {
	t.Helper()
	id, err := s.CreateUser(ctx, username, "$argon2id$test-hash", role)
	if err != nil {
		t.Fatalf("create %s %q: %v", role, username, err)
	}
	return id
}

func TestLastAdminGuard(t *testing.T) {
	ctx, s := newStore(t)
	alice := createUser(t, ctx, s, "alice", "admin")

	if _, err := s.CreateUser(ctx, "alice", "$argon2id$other", "viewer"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate username: err = %v, want ErrConflict", err)
	}

	// Sole enabled admin: disable and delete are both refused.
	if err := s.SetUserDisabled(ctx, alice, true); !errors.Is(err, store.ErrConflict) {
		t.Errorf("disable sole admin: err = %v, want ErrConflict", err)
	}
	if err := s.DeleteUser(ctx, alice); !errors.Is(err, store.ErrConflict) {
		t.Errorf("delete sole admin: err = %v, want ErrConflict", err)
	}

	// A viewer is not an admin for the guard's purposes.
	vic := createUser(t, ctx, s, "vic", "viewer")
	if err := s.SetUserDisabled(ctx, alice, true); !errors.Is(err, store.ErrConflict) {
		t.Errorf("disable sole admin with viewer present: err = %v, want ErrConflict", err)
	}
	if err := s.SetUserDisabled(ctx, vic, true); err != nil {
		t.Errorf("disable viewer: %v", err)
	}

	// With a second enabled admin the first becomes disableable — and the
	// second then inherits last-admin protection.
	bob := createUser(t, ctx, s, "bob", "admin")
	if err := s.SetUserDisabled(ctx, alice, true); err != nil {
		t.Fatalf("disable admin with backup present: %v", err)
	}
	if err := s.SetUserDisabled(ctx, bob, true); !errors.Is(err, store.ErrConflict) {
		t.Errorf("disable new last admin: err = %v, want ErrConflict", err)
	}
	if err := s.DeleteUser(ctx, bob); !errors.Is(err, store.ErrConflict) {
		t.Errorf("delete new last admin: err = %v, want ErrConflict", err)
	}

	// A disabled admin does not count as backup, but re-enabling is always
	// allowed and restores it as one.
	if err := s.SetUserDisabled(ctx, alice, false); err != nil {
		t.Fatalf("re-enable admin: %v", err)
	}
	if err := s.DeleteUser(ctx, bob); err != nil {
		t.Errorf("delete admin with enabled backup: %v", err)
	}

	if err := s.SetUserDisabled(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disable unknown user: err = %v, want ErrNotFound", err)
	}
}

// TestLastAdminGuardRace exercises the write-skew scenario the FOR UPDATE
// lock set exists for: two admins concurrently disabling each other. Without
// locking the whole enabled-admin set, both transactions would count the
// still-uncommitted peer and both succeed, leaving zero enabled admins on an
// air-gapped deployment with no recovery path short of container CLI access.
func TestLastAdminGuardRace(t *testing.T) {
	ctx, s := newStore(t)
	alice := createUser(t, ctx, s, "alice", "admin")
	bob := createUser(t, ctx, s, "bob", "admin")

	enabledAdmins := func() int {
		var n int
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM users WHERE role = 'admin' AND NOT disabled`).Scan(&n); err != nil {
			t.Fatalf("count enabled admins: %v", err)
		}
		return n
	}

	for round := range 25 {
		for _, id := range []uuid.UUID{alice, bob} {
			if err := s.SetUserDisabled(ctx, id, false); err != nil {
				t.Fatalf("round %d: re-enable: %v", round, err)
			}
		}

		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, id := range []uuid.UUID{alice, bob} {
			wg.Go(func() {
				<-start
				errs[i] = s.SetUserDisabled(ctx, id, true)
			})
		}
		close(start)
		wg.Wait()

		var oks, conflicts int
		for _, err := range errs {
			switch {
			case err == nil:
				oks++
			case errors.Is(err, store.ErrConflict):
				conflicts++
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if oks != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d succeeded and %d refused, want exactly 1 and 1", round, oks, conflicts)
		}
		if n := enabledAdmins(); n != 1 {
			t.Fatalf("round %d: %d enabled admins left, want 1", round, n)
		}
	}
}

// TestEnsureSiteUpsert pins enrollment's site upsert: repeated enrollments
// into the same site name converge on one row and one ID.
func TestEnsureSiteUpsert(t *testing.T) {
	ctx, s := newStore(t)
	first, err := s.EnsureSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	again, err := s.EnsureSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("EnsureSite again: %v", err)
	}
	if first != again {
		t.Errorf("EnsureSite returned %s then %s for the same name", first, again)
	}
	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM sites`).Scan(&n); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if n != 1 {
		t.Errorf("sites rows = %d, want 1", n)
	}
}

// TestCurrentPathMTUs pins the per-pair lookup: agent/target filtering and
// the nullable rtt_us column.
func TestCurrentPathMTUs(t *testing.T) {
	ctx, s := newStore(t)
	agentA, agentB := uuid.New(), uuid.New()
	probeA, probeB := uuid.New(), uuid.New()
	targetA, targetB := uuid.New(), uuid.New()

	insert := func(agentID, probeID, targetID uuid.UUID, mtu int32, rtt *int32) {
		t.Helper()
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO path_mtu_current (agent_id, probe_id, target_id, updated_at,
				largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes,
				ip_version, black_hole, local_constraint, rtt_us)
			VALUES ($1, $2, $3, now(), $4, 0, 0, 4, false, false, $5)`,
			agentID, probeID, targetID, mtu, rtt); err != nil {
			t.Fatalf("insert path_mtu_current: %v", err)
		}
	}
	rtt := int32(420)
	insert(agentA, probeA, targetA, 1400, &rtt)
	insert(agentB, probeB, targetB, 1500, nil)

	got, err := s.CurrentPathMTUs(ctx, []uuid.UUID{agentA}, []uuid.UUID{targetA})
	if err != nil {
		t.Fatalf("CurrentPathMTUs: %v", err)
	}
	if len(got) != 1 || got[0].AgentID != agentA || got[0].ProbeID != probeA ||
		got[0].LargestOK != 1400 || got[0].RttUS == nil || *got[0].RttUS != 420 {
		t.Errorf("got %+v, want agentA's 1400-byte row with rtt 420", got)
	}

	// The other direction's agent/target pair must not leak in.
	got, err = s.CurrentPathMTUs(ctx, []uuid.UUID{agentA}, []uuid.UUID{targetB})
	if err != nil {
		t.Fatalf("CurrentPathMTUs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-pair lookup returned %+v, want none", got)
	}

	got, err = s.CurrentPathMTUs(ctx, []uuid.UUID{agentB}, []uuid.UUID{targetB})
	if err != nil {
		t.Fatalf("CurrentPathMTUs: %v", err)
	}
	if len(got) != 1 || got[0].RttUS != nil {
		t.Errorf("got %+v, want agentB's row with null rtt", got)
	}
}

// TestCurrentPaths pins the per-pair traceroute lookup: agent/target
// filtering and the (agent_id, probe_id) series identity — one agent
// holds several rows toward the same target set (one per probe), and two
// source agents legitimately share a site-wide probe ID.
func TestCurrentPaths(t *testing.T) {
	ctx, s := newStore(t)
	agentA, agentA2, agentB := uuid.New(), uuid.New(), uuid.New()
	probe1, probe2, probeB := uuid.New(), uuid.New(), uuid.New()
	targetA, targetB := uuid.New(), uuid.New()

	insert := func(agentID, probeID, targetID uuid.UUID, reached bool, hash []byte) {
		t.Helper()
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO traceroute_current (agent_id, probe_id, target_id, updated_at,
				dest_reached, path_hash, hops)
			VALUES ($1, $2, $3, now(), $4, $5, '[]'::jsonb)`,
			agentID, probeID, targetID, reached, hash); err != nil {
			t.Fatalf("insert traceroute_current: %v", err)
		}
	}
	insert(agentA, probe1, targetA, true, []byte{0x01})
	insert(agentA, probe2, targetA, false, []byte{0x02})
	// Second source agent sharing probe1: distinct series per the
	// (agent_id, probe_id) primary key.
	insert(agentA2, probe1, targetA, true, []byte{0x04})
	insert(agentB, probeB, targetB, true, []byte{0x03})

	got, err := s.CurrentPaths(ctx, []uuid.UUID{agentA}, []uuid.UUID{targetA})
	if err != nil {
		t.Fatalf("CurrentPaths: %v", err)
	}
	if len(got) != 2 || got[0].AgentID != agentA || got[1].AgentID != agentA ||
		got[0].ProbeID == got[1].ProbeID {
		t.Fatalf("got %+v, want agentA's two rows with distinct probe IDs", got)
	}
	// Same hostname ('' — no agents row) and same agent both times, so
	// probe_id is the final tiebreaker: ascending, per the ORDER BY.
	lo, hi := probe1, probe2
	if hi.String() < lo.String() {
		lo, hi = hi, lo
	}
	if got[0].ProbeID != lo || got[1].ProbeID != hi {
		t.Errorf("order = %s, %s, want %s, %s", got[0].ProbeID, got[1].ProbeID, lo, hi)
	}

	// Both source agents together: the shared probe1 appears once per
	// agent — distinct series, told apart by agent ID alone.
	got, err = s.CurrentPaths(ctx, []uuid.UUID{agentA, agentA2}, []uuid.UUID{targetA})
	if err != nil {
		t.Fatalf("CurrentPaths: %v", err)
	}
	sharedAgents := map[uuid.UUID]bool{}
	for _, p := range got {
		if p.ProbeID == probe1 {
			sharedAgents[p.AgentID] = true
		}
	}
	if len(got) != 3 || !sharedAgents[agentA] || !sharedAgents[agentA2] {
		t.Errorf("got %+v, want 3 rows with probe %s under both agents", got, probe1)
	}

	// The other direction's agent/target pair must not leak in.
	got, err = s.CurrentPaths(ctx, []uuid.UUID{agentA}, []uuid.UUID{targetB})
	if err != nil {
		t.Fatalf("CurrentPaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-pair lookup returned %+v, want none", got)
	}

	got, err = s.CurrentPaths(ctx, []uuid.UUID{agentB}, []uuid.UUID{targetB})
	if err != nil {
		t.Fatalf("CurrentPaths: %v", err)
	}
	if len(got) != 1 || got[0].ProbeID != probeB || !got[0].DestReached {
		t.Errorf("got %+v, want agentB's reached row", got)
	}
}
