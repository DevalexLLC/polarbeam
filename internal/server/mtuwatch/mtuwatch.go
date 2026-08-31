// Package mtuwatch detects path MTU changes: it keeps the latest usable
// measurement per series in path_mtu_current and records a
// path_mtu_events row whenever the measured MTU or black-hole state
// changes. Measurements live in these tables only, never in the
// probe_results hypertable. The skeleton deliberately mirrors pathwatch;
// the two stay separate packages because their run semantics (hash
// comparison vs. size/flag comparison) share no code worth abstracting.
//
// Apply is set-based (the outage.Apply shape): seed missing rows, one bulk
// FOR UPDATE lock, a pure fold in Go, one bulk event insert, one bulk
// current upsert — a fixed handful of round trips per push instead of two
// or more per MTU series.
package mtuwatch

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the pgx surface Apply needs; satisfied by pgx.Tx and
// *pgxpool.Pool. Apply must run on the ingest transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Run is one genuinely inserted path MTU result. Dedupe-skipped rows must
// never reach Apply — a spool replay could otherwise re-emit an event.
// Usable marks a converged measurement (set at ingest); partial
// non-converged runs would flap events and are skipped.
type Run struct {
	ProbeID         uuid.UUID
	TargetID        uuid.UUID
	Time            time.Time
	LargestOK       int32
	SmallestFailed  int32
	NextHopMTU      int32
	IPVersion       int16
	BlackHole       bool
	LocalConstraint bool
	RttUS           *int32
	Usable          bool
}

// Change reports one recorded MTU event, for logging.
type Change struct {
	EventID  uuid.UUID
	ProbeID  uuid.UUID
	OldMTU   int32
	NewMTU   int32
	NewBlack bool
}

type current struct {
	updatedAt time.Time
	largestOK int32
	blackHole bool
}

type action int

const (
	actionSkip    action = iota // out-of-order or unusable run
	actionInsert                // first sighting: record current, no event
	actionRefresh               // same MTU and state: bump updated_at
	actionChange                // MTU or black-hole state changed: event + update
)

// decide is the pure per-run decision. Only usable (converged)
// measurements participate.
func decide(cur *current, r Run) action {
	if !r.Usable {
		return actionSkip
	}
	if cur == nil {
		return actionInsert
	}
	if !r.Time.After(cur.updatedAt) {
		return actionSkip // out-of-order spool replay
	}
	if cur.largestOK == r.LargestOK && cur.blackHole == r.BlackHole {
		return actionRefresh
	}
	return actionChange
}

// pendingEvent is one MTU change awaiting the bulk event insert, carrying
// the pre-run current identity the event's old_* columns record.
type pendingEvent struct {
	run      Run
	oldMTU   int32
	oldBlack bool
}

// Apply folds path MTU runs into path_mtu_current and path_mtu_events
// inside the caller's transaction. Runs may span series; each series is
// processed in time order.
//
// Only the FINAL applied run per series is written to path_mtu_current —
// intermediate states were never visible outside the transaction — while
// every intermediate change still records its event, so committed rows and
// events match the per-run fold exactly.
func Apply(ctx context.Context, db DB, agentID uuid.UUID, runs []Run) ([]Change, error) {
	if len(runs) == 0 {
		return nil, nil
	}

	// Unusable runs are dropped before grouping (decide would skip them),
	// so every grouped series has at least one applicable run — the
	// invariant that keeps ensureCurrent's placeholder rows from ever
	// surviving to commit. A run stamped at or before Go's zero time is
	// dropped for the same reason: it would collide with the seed sentinel
	// and be skipped by the upsert guard instead of applied (a nil
	// protobuf timestamp decodes as 1970, but year 1 exactly is still
	// encodable by a hostile or broken agent).
	bySeries := make(map[uuid.UUID][]Run)
	for _, r := range runs {
		if !r.Usable {
			continue
		}
		if !r.Time.After(time.Time{}) {
			slog.Warn("path MTU result at or before the year-1 sentinel skipped",
				"agent", agentID, "probe", r.ProbeID, "time", r.Time)
			continue
		}
		bySeries[r.ProbeID] = append(bySeries[r.ProbeID], r)
	}
	if len(bySeries) == 0 {
		return nil, nil
	}

	probeIDs := make([]uuid.UUID, 0, len(bySeries))
	for id := range bySeries {
		probeIDs = append(probeIDs, id)
	}
	// Deterministic lock order across concurrent ingest transactions.
	sort.Slice(probeIDs, func(i, j int) bool {
		return string(probeIDs[i][:]) < string(probeIDs[j][:])
	})

	seeded, err := ensureCurrent(ctx, db, agentID, probeIDs, bySeries)
	if err != nil {
		return nil, err
	}
	curs, err := lockCurrents(ctx, db, agentID, probeIDs)
	if err != nil {
		return nil, err
	}

	var pending []pendingEvent
	finals := make([]Run, 0, len(probeIDs))
	for _, probeID := range probeIDs {
		batch := bySeries[probeID]
		sort.Slice(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })

		cur := curs[probeID]
		if seeded[probeID] {
			cur = nil // this call inserted the row: no prior measurement
		}
		applied := false
		for _, r := range batch {
			act := decide(cur, r)
			if act == actionSkip {
				continue
			}
			if act == actionChange {
				pending = append(pending, pendingEvent{run: r, oldMTU: cur.largestOK, oldBlack: cur.blackHole})
			}
			cur = &current{updatedAt: r.Time, largestOK: r.LargestOK, blackHole: r.BlackHole}
			if applied {
				finals[len(finals)-1] = r // later run supersedes this series' final
			} else {
				finals = append(finals, r)
				applied = true
			}
		}
	}

	changes, err := bulkInsertEvents(ctx, db, agentID, pending)
	if err != nil {
		return nil, err
	}
	if err := bulkUpsertCurrent(ctx, db, agentID, finals); err != nil {
		return nil, err
	}
	return changes, nil
}

// ensureCurrent seeds a placeholder row for every series missing one, so
// the bulk FOR UPDATE in lockCurrents can lock it: FOR UPDATE cannot lock
// a missing row, and without the seed two concurrent first writers for a
// series would both fold from nil. The speculative insert makes the loser
// block until the winner commits, after which lockCurrents sees and locks
// the real committed row (outage.ensureStates' pattern). RETURNING reports
// exactly which rows THIS call inserted: those series fold from nil.
//
// Placeholder values can never survive to commit — every seeded series has
// at least one usable run decide applies (Apply's pre-grouping filter),
// and the sentinel updated_at (Go's zero time, which round-trips exactly
// through timestamptz) is strictly older than every grouped run's time
// (the pre-grouping filter drops runs at or before the sentinel), so the
// bulk upsert always overwrites the seed; bulkUpsertCurrent fails loudly
// if it ever does not.
func ensureCurrent(ctx context.Context, db DB, agentID uuid.UUID, probeIDs []uuid.UUID, bySeries map[uuid.UUID][]Run) (map[uuid.UUID]bool, error) {
	n := len(probeIDs)
	targetIDs := make([]uuid.UUID, n)
	for i, id := range probeIDs {
		targetIDs[i] = bySeries[id][0].TargetID
	}
	rows, err := db.Query(ctx, `
		INSERT INTO path_mtu_current (agent_id, probe_id, target_id, updated_at,
			largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes,
			ip_version, black_hole, local_constraint, rtt_us)
		SELECT $1, u.probe_id, u.target_id, '0001-01-01T00:00:00Z'::timestamptz,
			0, 0, 0, 0, false, false, NULL
		FROM unnest($2::uuid[], $3::uuid[]) AS u(probe_id, target_id)
		ON CONFLICT (agent_id, probe_id) DO NOTHING
		RETURNING probe_id`,
		agentID, probeIDs, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("seed path_mtu_current: %w", err)
	}
	defer rows.Close()
	seeded := make(map[uuid.UUID]bool, n)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("seed path_mtu_current: %w", err)
		}
		seeded[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seed path_mtu_current: %w", err)
	}
	return seeded, nil
}

// lockCurrents loads and locks every series' current row in one statement.
// After ensureCurrent every probe ID has a row, so a map miss cannot happen
// against a real database; tolerating one keeps test fakes simple.
func lockCurrents(ctx context.Context, db DB, agentID uuid.UUID, probeIDs []uuid.UUID) (map[uuid.UUID]*current, error) {
	// ORDER BY keeps lock acquisition in probe order across concurrent
	// transactions (the PK index serves the scan in order), matching the
	// sorted seed-insert order and the old per-series loop.
	rows, err := db.Query(ctx, `
		SELECT probe_id, updated_at, largest_ok_bytes, black_hole FROM path_mtu_current
		WHERE agent_id = $1 AND probe_id = ANY($2)
		ORDER BY probe_id
		FOR UPDATE`, agentID, probeIDs)
	if err != nil {
		return nil, fmt.Errorf("lock path_mtu_current: %w", err)
	}
	defer rows.Close()
	curs := make(map[uuid.UUID]*current, len(probeIDs))
	for rows.Next() {
		var id uuid.UUID
		var cur current
		if err := rows.Scan(&id, &cur.updatedAt, &cur.largestOK, &cur.blackHole); err != nil {
			return nil, fmt.Errorf("lock path_mtu_current: %w", err)
		}
		curs[id] = &cur
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock path_mtu_current: %w", err)
	}
	return curs, nil
}

// bulkInsertEvents records every pending change in one statement. Postgres
// emits INSERT ... SELECT unnest(...) RETURNING rows in insertion order,
// but rather than trust that, each returned probe_id is verified against
// its queue position — a mismatch is an error, never a mislabeled event.
func bulkInsertEvents(ctx context.Context, db DB, agentID uuid.UUID, pending []pendingEvent) ([]Change, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	n := len(pending)
	times := make([]time.Time, n)
	probeIDs := make([]uuid.UUID, n)
	targetIDs := make([]uuid.UUID, n)
	oldMTUs := make([]int32, n)
	newMTUs := make([]int32, n)
	oldBlacks := make([]bool, n)
	newBlacks := make([]bool, n)
	for i, p := range pending {
		times[i] = p.run.Time
		probeIDs[i] = p.run.ProbeID
		targetIDs[i] = p.run.TargetID
		oldMTUs[i] = p.oldMTU
		newMTUs[i] = p.run.LargestOK
		oldBlacks[i] = p.oldBlack
		newBlacks[i] = p.run.BlackHole
	}
	rows, err := db.Query(ctx, `
		INSERT INTO path_mtu_events (time, agent_id, probe_id, target_id,
			old_mtu_bytes, new_mtu_bytes, old_black_hole, new_black_hole)
		SELECT u.time, $1, u.probe_id, u.target_id,
			u.old_mtu_bytes, u.new_mtu_bytes, u.old_black_hole, u.new_black_hole
		FROM unnest($2::timestamptz[], $3::uuid[], $4::uuid[],
			$5::int[], $6::int[], $7::boolean[], $8::boolean[])
			AS u(time, probe_id, target_id, old_mtu_bytes, new_mtu_bytes, old_black_hole, new_black_hole)
		RETURNING id, probe_id`,
		agentID, times, probeIDs, targetIDs, oldMTUs, newMTUs, oldBlacks, newBlacks)
	if err != nil {
		return nil, fmt.Errorf("insert path MTU events: %w", err)
	}
	defer rows.Close()
	changes := make([]Change, 0, n)
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.EventID, &c.ProbeID); err != nil {
			return nil, fmt.Errorf("insert path MTU events: %w", err)
		}
		i := len(changes)
		if i >= n || c.ProbeID != pending[i].run.ProbeID {
			return nil, fmt.Errorf("insert path MTU events: RETURNING order diverged at row %d", i)
		}
		c.OldMTU = pending[i].oldMTU
		c.NewMTU = pending[i].run.LargestOK
		c.NewBlack = pending[i].run.BlackHole
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("insert path MTU events: %w", err)
	}
	if len(changes) != n {
		return nil, fmt.Errorf("insert path MTU events: %d rows returned, want %d", len(changes), n)
	}
	return changes, nil
}

// bulkUpsertCurrent persists every series' final applied run in one
// statement. Every target row is already locked by this transaction
// (ensureCurrent's speculative insert or lockCurrents' FOR UPDATE), so the
// statement's internal row order cannot introduce a new deadlock. The
// updated_at guard covers the concurrent-first-sighting race the seed
// already prevents structurally; while this transaction holds the row
// locks and the fold enforces run time > locked updated_at (with
// sentinel-time runs dropped before grouping), it never skips — a
// shortfall in RowsAffected is a logic bug and fails the push loudly
// rather than committing a placeholder as real state.
func bulkUpsertCurrent(ctx context.Context, db DB, agentID uuid.UUID, finals []Run) error {
	if len(finals) == 0 {
		return nil
	}
	n := len(finals)
	probeIDs := make([]uuid.UUID, n)
	targetIDs := make([]uuid.UUID, n)
	updatedAts := make([]time.Time, n)
	largestOKs := make([]int32, n)
	smallestFaileds := make([]int32, n)
	nextHopMTUs := make([]int32, n)
	ipVersions := make([]int16, n)
	blackHoles := make([]bool, n)
	localConstraints := make([]bool, n)
	rttUSs := make([]*int32, n)
	for i, r := range finals {
		probeIDs[i] = r.ProbeID
		targetIDs[i] = r.TargetID
		updatedAts[i] = r.Time
		largestOKs[i] = r.LargestOK
		smallestFaileds[i] = r.SmallestFailed
		nextHopMTUs[i] = r.NextHopMTU
		ipVersions[i] = r.IPVersion
		blackHoles[i] = r.BlackHole
		localConstraints[i] = r.LocalConstraint
		rttUSs[i] = r.RttUS
	}
	tag, err := db.Exec(ctx, `
		INSERT INTO path_mtu_current (agent_id, probe_id, target_id, updated_at,
			largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes,
			ip_version, black_hole, local_constraint, rtt_us)
		SELECT $1, u.probe_id, u.target_id, u.updated_at,
			u.largest_ok_bytes, u.smallest_failed_bytes, u.next_hop_mtu_bytes,
			u.ip_version, u.black_hole, u.local_constraint, u.rtt_us
		FROM unnest($2::uuid[], $3::uuid[], $4::timestamptz[],
			$5::int[], $6::int[], $7::int[], $8::smallint[],
			$9::boolean[], $10::boolean[], $11::int[])
			AS u(probe_id, target_id, updated_at,
				largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes,
				ip_version, black_hole, local_constraint, rtt_us)
		ON CONFLICT (agent_id, probe_id) DO UPDATE SET
			target_id = EXCLUDED.target_id,
			updated_at = EXCLUDED.updated_at,
			largest_ok_bytes = EXCLUDED.largest_ok_bytes,
			smallest_failed_bytes = EXCLUDED.smallest_failed_bytes,
			next_hop_mtu_bytes = EXCLUDED.next_hop_mtu_bytes,
			ip_version = EXCLUDED.ip_version,
			black_hole = EXCLUDED.black_hole,
			local_constraint = EXCLUDED.local_constraint,
			rtt_us = EXCLUDED.rtt_us
		WHERE path_mtu_current.updated_at < EXCLUDED.updated_at`,
		agentID, probeIDs, targetIDs, updatedAts, largestOKs, smallestFaileds,
		nextHopMTUs, ipVersions, blackHoles, localConstraints, rttUSs)
	if err != nil {
		return fmt.Errorf("upsert path_mtu_current: %w", err)
	}
	if int(tag.RowsAffected()) != n {
		return fmt.Errorf("upsert path_mtu_current: applied %d of %d rows despite held locks",
			tag.RowsAffected(), n)
	}
	return nil
}
