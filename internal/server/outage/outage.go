// Package outage detects connectivity outages from probe results: a
// series_state-backed hysteresis opens a probe_failing event after
// Threshold consecutive failures and closes it after Threshold consecutive
// successes, updated inside the ingest transaction. A separate sweep
// (sweep.go) detects agents that have gone silent entirely.
//
// The two event kinds deliberately coexist without suppression: hysteresis
// is result-driven, so a silent agent never advances series counters, and
// an agent that keeps probing while unable to reach the server replays its
// real per-series history from spool on reconnect while the agent_offline
// event brackets the silent window.
package outage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Threshold is the hysteresis width: consecutive failures to open, and
// consecutive successes to close.
const Threshold = 3

// DB is the pgx surface Apply and Sweep need; satisfied by pgx.Tx and
// *pgxpool.Pool. Apply must run on the ingest transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Result is one genuinely inserted probe result. Dedupe-skipped rows must
// never reach Apply — the caller filters on what the insert actually added.
type Result struct {
	ProbeID   uuid.UUID
	TargetID  uuid.UUID
	ProbeType int16
	Time      time.Time
	// OK is status == PROBE_STATUS_OK. Everything else — including
	// UNSUPPORTED — counts as failure, deliberately: a probe an agent
	// cannot run is a real gap in coverage.
	OK bool
	// StatusCode is the wire ProbeStatus, stored as series_state.last_status.
	StatusCode int16
	Error      string
}

// Transition reports one event opened or closed by Apply, for logging.
type Transition struct {
	EventID uuid.UUID
	ProbeID uuid.UUID
	Opened  bool // false = closed
	At      time.Time
}

// State is one series' hysteresis state, mirroring a series_state row.
type State struct {
	ConsecFails int
	ConsecOKs   int
	FirstFailAt time.Time
	FirstOKAt   time.Time
	LastStatus  int16
	LastTime    time.Time
	OpenEventID *uuid.UUID
}

type action int

const (
	actionNone action = iota
	actionOpen
	actionClose
)

// step folds one result into the state. Pure. Results at or before
// LastTime are ignored (out-of-order spool replay). The returned action is
// what the caller must persist: actionOpen fires when the failure streak
// reaches Threshold with no open event (>=, not ==, so state drift heals),
// actionClose when the success streak reaches Threshold with one open.
func step(st State, r Result) (State, action) {
	if !r.Time.After(st.LastTime) {
		return st, actionNone
	}
	st.LastTime = r.Time
	st.LastStatus = r.StatusCode
	if r.OK {
		st.ConsecFails = 0
		st.FirstFailAt = time.Time{}
		st.ConsecOKs++
		if st.ConsecOKs == 1 {
			st.FirstOKAt = r.Time
		}
		if st.ConsecOKs >= Threshold && st.OpenEventID != nil {
			return st, actionClose
		}
		return st, actionNone
	}
	st.ConsecOKs = 0
	st.FirstOKAt = time.Time{}
	st.ConsecFails++
	if st.ConsecFails == 1 {
		st.FirstFailAt = r.Time
	}
	if st.ConsecFails >= Threshold && st.OpenEventID == nil {
		return st, actionOpen
	}
	return st, actionNone
}

// Apply folds a batch of inserted results into series_state inside the
// caller's transaction, opening and closing probe_failing events as streaks
// cross the threshold. Results may span series and arrive in any order;
// each series is folded in time order.
func Apply(ctx context.Context, db DB, agentID uuid.UUID, results []Result) ([]Transition, error) {
	if len(results) == 0 {
		return nil, nil
	}

	bySeries := make(map[uuid.UUID][]Result)
	for _, r := range results {
		bySeries[r.ProbeID] = append(bySeries[r.ProbeID], r)
	}
	probeIDs := make([]uuid.UUID, 0, len(bySeries))
	for id := range bySeries {
		probeIDs = append(probeIDs, id)
	}
	// Deterministic lock order across concurrent ingest transactions.
	sort.Slice(probeIDs, func(i, j int) bool {
		return string(probeIDs[i][:]) < string(probeIDs[j][:])
	})

	// FOR UPDATE cannot lock a row that does not exist yet, so two
	// concurrent first writers for the same series would each fold from
	// zero. Seed missing series with an empty state first — the
	// speculative insert makes the loser wait for the winner to commit,
	// after which the SELECT sees and locks a real row.
	if err := ensureStates(ctx, db, agentID, probeIDs, bySeries); err != nil {
		return nil, err
	}
	states, err := lockStates(ctx, db, agentID, probeIDs)
	if err != nil {
		return nil, err
	}

	var transitions []Transition
	for _, probeID := range probeIDs {
		batch := bySeries[probeID]
		sort.Slice(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })

		st := states[probeID] // zero State for a series never seen before
		for _, r := range batch {
			var act action
			st, act = step(st, r)
			switch act {
			case actionOpen:
				id, err := openEvent(ctx, db, agentID, r, st.FirstFailAt)
				if err != nil {
					return nil, err
				}
				st.OpenEventID = &id
				transitions = append(transitions, Transition{EventID: id, ProbeID: probeID, Opened: true, At: st.FirstFailAt})
			case actionClose:
				if _, err := db.Exec(ctx,
					`UPDATE outage_events SET closed_at = $2 WHERE id = $1 AND closed_at IS NULL`,
					*st.OpenEventID, st.FirstOKAt); err != nil {
					return nil, fmt.Errorf("close outage event: %w", err)
				}
				transitions = append(transitions, Transition{EventID: *st.OpenEventID, ProbeID: probeID, Opened: false, At: st.FirstOKAt})
				st.OpenEventID = nil
			}
		}

		last := batch[len(batch)-1]
		if err := upsertState(ctx, db, agentID, last.ProbeID, last.TargetID, last.ProbeType, st); err != nil {
			return nil, err
		}
	}
	return transitions, nil
}

// ensureStates inserts an empty state row for every series missing one.
// last_time seeds as Go's zero time (year 1, round-trips exactly through
// timestamptz) — NOT the epoch, which would silently discard accepted
// results from unset host clocks — so folding proceeds exactly as from a
// zero State.
func ensureStates(ctx context.Context, db DB, agentID uuid.UUID, probeIDs []uuid.UUID, bySeries map[uuid.UUID][]Result) error {
	n := len(probeIDs)
	targetIDs := make([]uuid.UUID, n)
	probeTypes := make([]int16, n)
	for i, id := range probeIDs {
		first := bySeries[id][0]
		targetIDs[i], probeTypes[i] = first.TargetID, first.ProbeType
	}
	_, err := db.Exec(ctx, `
		INSERT INTO series_state (agent_id, probe_id, target_id, probe_type, last_status, last_time)
		SELECT $1, u.probe_id, u.target_id, u.probe_type, 0, '0001-01-01T00:00:00Z'::timestamptz
		FROM unnest($2::uuid[], $3::uuid[], $4::smallint[]) AS u(probe_id, target_id, probe_type)
		ON CONFLICT (agent_id, probe_id) DO NOTHING`,
		agentID, probeIDs, targetIDs, probeTypes)
	if err != nil {
		return fmt.Errorf("seed series_state: %w", err)
	}
	return nil
}

func lockStates(ctx context.Context, db DB, agentID uuid.UUID, probeIDs []uuid.UUID) (map[uuid.UUID]State, error) {
	rows, err := db.Query(ctx, `
		SELECT probe_id, consec_fails, consec_oks, first_fail_at, first_ok_at,
			last_status, last_time, open_event_id
		FROM series_state
		WHERE agent_id = $1 AND probe_id = ANY($2)
		FOR UPDATE`, agentID, probeIDs)
	if err != nil {
		return nil, fmt.Errorf("lock series_state: %w", err)
	}
	defer rows.Close()

	states := make(map[uuid.UUID]State)
	for rows.Next() {
		var (
			probeID            uuid.UUID
			st                 State
			firstFail, firstOK *time.Time
			openEventID        *uuid.UUID
		)
		if err := rows.Scan(&probeID, &st.ConsecFails, &st.ConsecOKs, &firstFail, &firstOK,
			&st.LastStatus, &st.LastTime, &openEventID); err != nil {
			return nil, fmt.Errorf("scan series_state: %w", err)
		}
		if firstFail != nil {
			st.FirstFailAt = *firstFail
		}
		if firstOK != nil {
			st.FirstOKAt = *firstOK
		}
		st.OpenEventID = openEventID
		states[probeID] = st
	}
	return states, rows.Err()
}

// openEvent inserts the probe_failing event, or adopts an existing open one
// if the partial unique index says the state drifted (crash between event
// insert and state upsert).
func openEvent(ctx context.Context, db DB, agentID uuid.UUID, r Result, openedAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `
		INSERT INTO outage_events (kind, agent_id, probe_id, target_id, probe_type, opened_at, open_error)
		VALUES ('probe_failing', $1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (agent_id, probe_id) WHERE kind = 'probe_failing' AND closed_at IS NULL
		DO NOTHING
		RETURNING id`,
		agentID, r.ProbeID, r.TargetID, r.ProbeType, openedAt, r.Error).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return uuid.Nil, fmt.Errorf("open outage event: %w", err)
	}
	err = db.QueryRow(ctx, `
		SELECT id FROM outage_events
		WHERE agent_id = $1 AND probe_id = $2 AND kind = 'probe_failing' AND closed_at IS NULL`,
		agentID, r.ProbeID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("adopt open outage event: %w", err)
	}
	return id, nil
}

func upsertState(ctx context.Context, db DB, agentID, probeID, targetID uuid.UUID, probeType int16, st State) error {
	_, err := db.Exec(ctx, `
		INSERT INTO series_state (agent_id, probe_id, target_id, probe_type,
			consec_fails, consec_oks, first_fail_at, first_ok_at,
			last_status, last_time, open_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, 'epoch'::timestamptz), NULLIF($8, 'epoch'::timestamptz), $9, $10, $11)
		ON CONFLICT (agent_id, probe_id) DO UPDATE SET
			target_id = EXCLUDED.target_id,
			probe_type = EXCLUDED.probe_type,
			consec_fails = EXCLUDED.consec_fails,
			consec_oks = EXCLUDED.consec_oks,
			first_fail_at = EXCLUDED.first_fail_at,
			first_ok_at = EXCLUDED.first_ok_at,
			last_status = EXCLUDED.last_status,
			last_time = EXCLUDED.last_time,
			open_event_id = EXCLUDED.open_event_id`,
		agentID, probeID, targetID, probeType,
		st.ConsecFails, st.ConsecOKs, zeroToEpoch(st.FirstFailAt), zeroToEpoch(st.FirstOKAt),
		st.LastStatus, st.LastTime, st.OpenEventID)
	if err != nil {
		return fmt.Errorf("upsert series_state: %w", err)
	}
	return nil
}

// zeroToEpoch maps Go's zero time to the epoch sentinel NULLIF strips to
// NULL, keeping the SQL free of *time.Time juggling.
func zeroToEpoch(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return t
}
