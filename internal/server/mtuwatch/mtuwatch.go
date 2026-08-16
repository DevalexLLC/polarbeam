// Package mtuwatch detects path MTU changes: it keeps the latest usable
// measurement per series in path_mtu_current and records a
// path_mtu_events row whenever the measured MTU or black-hole state
// changes. Measurements live in these tables only, never in the
// probe_results hypertable. The skeleton deliberately mirrors pathwatch;
// the two stay separate packages because their run semantics (hash
// comparison vs. size/flag comparison) share no code worth abstracting.
package mtuwatch

import (
	"context"
	"fmt"
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

// Apply folds path MTU runs into path_mtu_current and path_mtu_events
// inside the caller's transaction. Runs may span series; each series is
// processed in time order.
func Apply(ctx context.Context, db DB, agentID uuid.UUID, runs []Run) ([]Change, error) {
	if len(runs) == 0 {
		return nil, nil
	}

	bySeries := make(map[uuid.UUID][]Run)
	for _, r := range runs {
		if !r.Usable {
			continue
		}
		bySeries[r.ProbeID] = append(bySeries[r.ProbeID], r)
	}

	probeIDs := make([]uuid.UUID, 0, len(bySeries))
	for id := range bySeries {
		probeIDs = append(probeIDs, id)
	}
	sort.Slice(probeIDs, func(i, j int) bool {
		return string(probeIDs[i][:]) < string(probeIDs[j][:])
	})

	var changes []Change
	for _, probeID := range probeIDs {
		batch := bySeries[probeID]
		sort.Slice(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })

		cur, err := lockCurrent(ctx, db, agentID, probeID)
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			act := decide(cur, r)
			if act == actionSkip {
				continue
			}
			// Upsert before recording anything: if the stale guard skipped
			// it, a concurrent transaction won a first-sighting race with a
			// newer run — reload (and this time lock) the real current row
			// so the rest of the batch folds against actual state.
			applied, err := upsertCurrent(ctx, db, agentID, r)
			if err != nil {
				return nil, err
			}
			if !applied {
				cur, err = lockCurrent(ctx, db, agentID, probeID)
				if err != nil {
					return nil, err
				}
				continue
			}
			if act == actionChange {
				var eventID uuid.UUID
				err := db.QueryRow(ctx, `
					INSERT INTO path_mtu_events (time, agent_id, probe_id, target_id,
						old_mtu_bytes, new_mtu_bytes, old_black_hole, new_black_hole)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					RETURNING id`,
					r.Time, agentID, r.ProbeID, r.TargetID,
					cur.largestOK, r.LargestOK, cur.blackHole, r.BlackHole).Scan(&eventID)
				if err != nil {
					return nil, fmt.Errorf("insert path MTU event: %w", err)
				}
				changes = append(changes, Change{
					EventID: eventID, ProbeID: probeID,
					OldMTU: cur.largestOK, NewMTU: r.LargestOK, NewBlack: r.BlackHole,
				})
			}
			cur = &current{updatedAt: r.Time, largestOK: r.LargestOK, blackHole: r.BlackHole}
		}
	}
	return changes, nil
}

func lockCurrent(ctx context.Context, db DB, agentID, probeID uuid.UUID) (*current, error) {
	var cur current
	err := db.QueryRow(ctx, `
		SELECT updated_at, largest_ok_bytes, black_hole FROM path_mtu_current
		WHERE agent_id = $1 AND probe_id = $2
		FOR UPDATE`, agentID, probeID).Scan(&cur.updatedAt, &cur.largestOK, &cur.blackHole)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock path_mtu_current: %w", err)
	}
	return &cur, nil
}

// upsertCurrent makes r the series' current measurement unless a newer one
// is already recorded; applied reports whether the write took effect. The
// stale guard covers concurrent first sightings: FOR UPDATE cannot lock a
// missing row, so two initial transactions may both decide to insert — the
// guard keeps the newer run current regardless of commit order. While the
// caller holds the row lock (cur was loaded), the guard never skips.
func upsertCurrent(ctx context.Context, db DB, agentID uuid.UUID, r Run) (applied bool, err error) {
	tag, err := db.Exec(ctx, `
		INSERT INTO path_mtu_current (agent_id, probe_id, target_id, updated_at,
			largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes,
			ip_version, black_hole, local_constraint, rtt_us)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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
		agentID, r.ProbeID, r.TargetID, r.Time, r.LargestOK, r.SmallestFailed,
		r.NextHopMTU, r.IPVersion, r.BlackHole, r.LocalConstraint, r.RttUS)
	if err != nil {
		return false, fmt.Errorf("upsert path_mtu_current: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
