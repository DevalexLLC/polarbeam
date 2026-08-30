// Package outage detects connectivity problems from probe results: a
// series_state-backed hysteresis opens a probe_failing event after
// Threshold consecutive failures and closes it after Threshold consecutive
// successes, and opens a probe_degraded event after Threshold consecutive
// successes that breach the critical latency/loss thresholds (graded by
// ingest, see grpcapi's toOutageResults) closing it after Threshold
// consecutive clean successes — all updated inside the ingest transaction.
// A separate sweep (sweep.go) detects agents that have gone silent entirely.
//
// At most one probe event is open per series, and down supersedes degraded:
// a failure streak crossing the threshold while a probe_degraded event is
// open closes it and opens probe_failing at the same instant (escalation),
// and a success streak closing probe_failing opens probe_degraded in the
// same step when every closing success was breaching (de-escalation) —
// event history stays contiguous with no overlap.
//
// Degraded grading uses the thresholds effective when the result is
// ingested: threshold edits converge within the assignment cache's 30 s
// TTL, and spool-replayed history is graded at replay-time thresholds, not
// the values in force when the probe ran. Both are accepted — the hysteresis
// already trades instant precision for stability.
//
// The probe_failing and agent_offline kinds deliberately coexist without
// suppression: hysteresis is result-driven, so a silent agent never
// advances series counters, and an agent that keeps probing while unable to
// reach the server replays its real per-series history from spool on
// reconnect while the agent_offline event brackets the silent window.
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

// Threshold is the hysteresis width for both probe event kinds: consecutive
// failures (or breaching successes) to open, and consecutive successes (or
// clean successes) to close.
const Threshold = 3

// Probe event kinds this package opens and closes. agent_offline belongs to
// the sweep and never appears in a series' OpenKind.
const (
	KindProbeFailing  = "probe_failing"
	KindProbeDegraded = "probe_degraded"
)

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
	// Degraded marks an OK result whose latency or loss breached the
	// critical thresholds effective for its direction at ingest time.
	// Never set on failures. DegradedDetail is the stable display text
	// (probe_degraded's open_error); it must not embed measured values —
	// the dashboard correlates incidents by this text.
	Degraded       bool
	DegradedDetail string
	// LossPct/LatencyUS/LatencySource are the row's display measurements
	// (the ingest-side COALESCE ladder), persisted as series_state.last_*
	// so the matrix serves "latest per series" without scanning the
	// hypertable. Set for every result — failures included — mirroring the
	// raw latest-row query these columns replaced.
	LossPct       *float32
	LatencyUS     *int64
	LatencySource *string
}

// Transition reports one event opened or closed by Apply, for logging.
type Transition struct {
	EventID uuid.UUID
	ProbeID uuid.UUID
	Kind    string
	Opened  bool // false = closed
	At      time.Time
}

// State is one series' hysteresis state, mirroring a series_state row.
// OpenKind is the open event's kind, derived by lockStates from the event
// row (never persisted); "" when no event is open. ConsecOKs counts every
// success (it closes probe_failing); the degraded/clean pair are the two
// mutually exclusive sub-streaks within a success run.
type State struct {
	ConsecFails     int
	ConsecOKs       int
	ConsecDegraded  int
	ConsecClean     int
	FirstFailAt     time.Time
	FirstOKAt       time.Time
	FirstDegradedAt time.Time
	FirstCleanAt    time.Time
	LastStatus      int16
	LastTime        time.Time
	// Display mirrors of the newest result (see Result.LossPct et al.).
	LastLossPct       *float32
	LastLatencyUS     *int64
	LastLatencySource *string
	OpenEventID       *uuid.UUID
	OpenKind          string
}

type action int

const (
	actionOpen action = iota
	actionClose
)

// op is one event mutation step decided; Apply executes them in order.
// kind and detail are set only for actionOpen — actionClose closes whatever
// the series' OpenEventID points to.
type op struct {
	act    action
	kind   string
	at     time.Time
	detail string
}

// step folds one result into the state. Pure. Results at or before
// LastTime are ignored (out-of-order spool replay). The returned ops are
// what the caller must persist, at most two: opens fire when a streak
// reaches Threshold with no open event (>=, not ==, so state drift heals),
// closes when the countering streak reaches Threshold with one open, and
// the escalation/de-escalation cases pair a close with an open at
// contiguous timestamps.
func step(st State, r Result) (State, []op) {
	if !r.Time.After(st.LastTime) {
		return st, nil
	}
	st.LastTime = r.Time
	st.LastStatus = r.StatusCode
	st.LastLossPct = r.LossPct
	st.LastLatencyUS = r.LatencyUS
	st.LastLatencySource = r.LatencySource
	if !r.OK {
		st.ConsecOKs = 0
		st.FirstOKAt = time.Time{}
		st.ConsecDegraded = 0
		st.FirstDegradedAt = time.Time{}
		st.ConsecClean = 0
		st.FirstCleanAt = time.Time{}
		st.ConsecFails++
		if st.ConsecFails == 1 {
			st.FirstFailAt = r.Time
		}
		if st.ConsecFails >= Threshold {
			switch st.OpenKind {
			case KindProbeDegraded:
				// Escalation: the degraded link went down. Close and open at
				// the same instant so the history is contiguous.
				return st, []op{
					{act: actionClose, at: st.FirstFailAt},
					{act: actionOpen, kind: KindProbeFailing, at: st.FirstFailAt, detail: r.Error},
				}
			case "":
				return st, []op{{act: actionOpen, kind: KindProbeFailing, at: st.FirstFailAt, detail: r.Error}}
			}
		}
		return st, nil
	}
	st.ConsecFails = 0
	st.FirstFailAt = time.Time{}
	st.ConsecOKs++
	if st.ConsecOKs == 1 {
		st.FirstOKAt = r.Time
	}
	if r.Degraded {
		st.ConsecClean = 0
		st.FirstCleanAt = time.Time{}
		st.ConsecDegraded++
		if st.ConsecDegraded == 1 {
			st.FirstDegradedAt = r.Time
		}
	} else {
		st.ConsecDegraded = 0
		st.FirstDegradedAt = time.Time{}
		st.ConsecClean++
		if st.ConsecClean == 1 {
			st.FirstCleanAt = r.Time
		}
	}
	switch {
	case st.OpenKind == KindProbeFailing && st.ConsecOKs >= Threshold:
		ops := []op{{act: actionClose, at: st.FirstOKAt}}
		if st.ConsecDegraded >= Threshold {
			// De-escalation: every success that closed the outage was
			// breaching, so the degraded window starts where the outage
			// ends (FirstDegradedAt == FirstOKAt). Deferring this open to
			// the next result would lose it entirely if that result is
			// clean.
			ops = append(ops, op{act: actionOpen, kind: KindProbeDegraded, at: st.FirstDegradedAt, detail: r.DegradedDetail})
		}
		return st, ops
	case st.OpenKind == KindProbeDegraded && st.ConsecClean >= Threshold:
		return st, []op{{act: actionClose, at: st.FirstCleanAt}}
	case st.OpenKind == "" && st.ConsecDegraded >= Threshold:
		return st, []op{{act: actionOpen, kind: KindProbeDegraded, at: st.FirstDegradedAt, detail: r.DegradedDetail}}
	}
	return st, nil
}

// Apply folds a batch of inserted results into series_state inside the
// caller's transaction, opening and closing probe events as streaks cross
// the threshold. Results may span series and arrive in any order; each
// series is folded in time order.
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
	finals := make([]finalSeries, 0, len(probeIDs))
	for _, probeID := range probeIDs {
		batch := bySeries[probeID]
		sort.Slice(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })

		st := states[probeID] // zero State for a series never seen before
		for _, r := range batch {
			var ops []op
			st, ops = step(st, r)
			for _, o := range ops {
				switch o.act {
				case actionOpen:
					id, err := openEvent(ctx, db, agentID, r, o.kind, o.at, o.detail)
					if err != nil {
						return nil, err
					}
					st.OpenEventID = &id
					st.OpenKind = o.kind
					transitions = append(transitions, Transition{EventID: id, ProbeID: probeID, Kind: o.kind, Opened: true, At: o.at})
				case actionClose:
					if _, err := db.Exec(ctx,
						`UPDATE outage_events SET closed_at = $2 WHERE id = $1 AND closed_at IS NULL`,
						*st.OpenEventID, o.at); err != nil {
						return nil, fmt.Errorf("close outage event: %w", err)
					}
					transitions = append(transitions, Transition{EventID: *st.OpenEventID, ProbeID: probeID, Kind: st.OpenKind, Opened: false, At: o.at})
					st.OpenEventID = nil
					st.OpenKind = ""
				}
			}
		}

		last := batch[len(batch)-1]
		finals = append(finals, finalSeries{probeID: last.ProbeID, targetID: last.TargetID, probeType: last.ProbeType, st: st})
	}
	if err := bulkUpsertStates(ctx, db, agentID, finals); err != nil {
		return nil, err
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

// lockStates loads and locks the series states, deriving OpenKind from the
// open event row. FOR UPDATE OF ss, not plain FOR UPDATE: locking the
// nullable side of an outer join is a Postgres error, and outage_events
// must stay unlocked here to preserve the deadlock analysis in
// bulkUpsertStates. The join deliberately does not filter on closed_at — a
// dangling pointer to a closed event keeps behaving exactly as before (the
// eventual close is a no-op UPDATE and the sweep repairs the pointer).
func lockStates(ctx context.Context, db DB, agentID uuid.UUID, probeIDs []uuid.UUID) (map[uuid.UUID]State, error) {
	rows, err := db.Query(ctx, `
		SELECT ss.probe_id, ss.consec_fails, ss.consec_oks, ss.consec_degraded, ss.consec_clean,
			ss.first_fail_at, ss.first_ok_at, ss.first_degraded_at, ss.first_clean_at,
			ss.last_status, ss.last_time,
			ss.last_loss_pct, ss.last_latency_us, ss.last_latency_source,
			ss.open_event_id, oe.kind
		FROM series_state ss
		LEFT JOIN outage_events oe ON oe.id = ss.open_event_id
		WHERE ss.agent_id = $1 AND ss.probe_id = ANY($2)
		FOR UPDATE OF ss`, agentID, probeIDs)
	if err != nil {
		return nil, fmt.Errorf("lock series_state: %w", err)
	}
	defer rows.Close()

	states := make(map[uuid.UUID]State)
	for rows.Next() {
		var (
			probeID                                       uuid.UUID
			st                                            State
			firstFail, firstOK, firstDegraded, firstClean *time.Time
			openEventID                                   *uuid.UUID
			openKind                                      *string
		)
		if err := rows.Scan(&probeID, &st.ConsecFails, &st.ConsecOKs, &st.ConsecDegraded, &st.ConsecClean,
			&firstFail, &firstOK, &firstDegraded, &firstClean,
			&st.LastStatus, &st.LastTime,
			&st.LastLossPct, &st.LastLatencyUS, &st.LastLatencySource,
			&openEventID, &openKind); err != nil {
			return nil, fmt.Errorf("scan series_state: %w", err)
		}
		if firstFail != nil {
			st.FirstFailAt = *firstFail
		}
		if firstOK != nil {
			st.FirstOKAt = *firstOK
		}
		if firstDegraded != nil {
			st.FirstDegradedAt = *firstDegraded
		}
		if firstClean != nil {
			st.FirstCleanAt = *firstClean
		}
		st.OpenEventID = openEventID
		if openEventID != nil && openKind != nil {
			st.OpenKind = *openKind
		}
		states[probeID] = st
	}
	return states, rows.Err()
}

// The conflict predicate must be a literal per kind: ON CONFLICT infers its
// arbiter index from a constant predicate, and each kind has its own
// partial unique index (outage_events_probe_open_uidx,
// outage_events_degraded_open_uidx).
const (
	openFailingSQL = `
		INSERT INTO outage_events (kind, agent_id, probe_id, target_id, probe_type, opened_at, open_error)
		VALUES ('probe_failing', $1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (agent_id, probe_id) WHERE kind = 'probe_failing' AND closed_at IS NULL
		DO NOTHING
		RETURNING id`
	openDegradedSQL = `
		INSERT INTO outage_events (kind, agent_id, probe_id, target_id, probe_type, opened_at, open_error)
		VALUES ('probe_degraded', $1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (agent_id, probe_id) WHERE kind = 'probe_degraded' AND closed_at IS NULL
		DO NOTHING
		RETURNING id`
)

// openEvent inserts the probe event, or adopts an existing open one of the
// same kind if the partial unique index says the state drifted (crash
// between event insert and state upsert).
func openEvent(ctx context.Context, db DB, agentID uuid.UUID, r Result, kind string, openedAt time.Time, detail string) (uuid.UUID, error) {
	sql := openFailingSQL
	if kind == KindProbeDegraded {
		sql = openDegradedSQL
	}
	var id uuid.UUID
	err := db.QueryRow(ctx, sql,
		agentID, r.ProbeID, r.TargetID, r.ProbeType, openedAt, detail).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return uuid.Nil, fmt.Errorf("open outage event: %w", err)
	}
	err = db.QueryRow(ctx, `
		SELECT id FROM outage_events
		WHERE agent_id = $1 AND probe_id = $2 AND kind = $3 AND closed_at IS NULL`,
		agentID, r.ProbeID, kind).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("adopt open outage event: %w", err)
	}
	return id, nil
}

// finalSeries is one series' folded end state, queued for the single bulk
// upsert at the end of Apply.
type finalSeries struct {
	probeID   uuid.UUID
	targetID  uuid.UUID
	probeType int16
	st        State
}

// bulkUpsertStates persists every folded series state in one statement —
// one round trip per push instead of one per series. Every target row is
// already locked by this transaction (ensureStates' speculative insert or
// lockStates' FOR UPDATE), so the statement's internal row order cannot
// introduce a new deadlock. Nullable columns ride sentinels stripped back
// to NULL by NULLIF: zero time → 'epoch' (zeroToEpoch) and nil
// open_event_id → the nil uuid, which gen_random_uuid() can never mint.
// last_time passes through raw: it is NOT NULL, and the year-1 seed value
// must survive (see ensureStates). OpenKind is derived, never stored.
func bulkUpsertStates(ctx context.Context, db DB, agentID uuid.UUID, finals []finalSeries) error {
	n := len(finals)
	probeIDs := make([]uuid.UUID, n)
	targetIDs := make([]uuid.UUID, n)
	probeTypes := make([]int16, n)
	consecFails := make([]int32, n)
	consecOKs := make([]int32, n)
	consecDegraded := make([]int32, n)
	consecClean := make([]int32, n)
	firstFails := make([]time.Time, n)
	firstOKs := make([]time.Time, n)
	firstDegradeds := make([]time.Time, n)
	firstCleans := make([]time.Time, n)
	lastStatuses := make([]int16, n)
	lastTimes := make([]time.Time, n)
	lastLossPcts := make([]*float32, n)
	lastLatencies := make([]*int64, n)
	lastSources := make([]*string, n)
	openEventIDs := make([]uuid.UUID, n)
	for i, f := range finals {
		probeIDs[i] = f.probeID
		targetIDs[i] = f.targetID
		probeTypes[i] = f.probeType
		consecFails[i] = int32(f.st.ConsecFails)
		consecOKs[i] = int32(f.st.ConsecOKs)
		consecDegraded[i] = int32(f.st.ConsecDegraded)
		consecClean[i] = int32(f.st.ConsecClean)
		firstFails[i] = zeroToEpoch(f.st.FirstFailAt)
		firstOKs[i] = zeroToEpoch(f.st.FirstOKAt)
		firstDegradeds[i] = zeroToEpoch(f.st.FirstDegradedAt)
		firstCleans[i] = zeroToEpoch(f.st.FirstCleanAt)
		lastStatuses[i] = f.st.LastStatus
		lastTimes[i] = f.st.LastTime
		lastLossPcts[i] = f.st.LastLossPct
		lastLatencies[i] = f.st.LastLatencyUS
		lastSources[i] = f.st.LastLatencySource
		if f.st.OpenEventID != nil {
			openEventIDs[i] = *f.st.OpenEventID
		}
	}
	_, err := db.Exec(ctx, `
		INSERT INTO series_state (agent_id, probe_id, target_id, probe_type,
			consec_fails, consec_oks, consec_degraded, consec_clean,
			first_fail_at, first_ok_at, first_degraded_at, first_clean_at,
			last_status, last_time,
			last_loss_pct, last_latency_us, last_latency_source, open_event_id)
		SELECT $1, u.probe_id, u.target_id, u.probe_type,
			u.consec_fails, u.consec_oks, u.consec_degraded, u.consec_clean,
			NULLIF(u.first_fail_at, 'epoch'::timestamptz),
			NULLIF(u.first_ok_at, 'epoch'::timestamptz),
			NULLIF(u.first_degraded_at, 'epoch'::timestamptz),
			NULLIF(u.first_clean_at, 'epoch'::timestamptz),
			u.last_status, u.last_time,
			u.last_loss_pct, u.last_latency_us, u.last_latency_source,
			NULLIF(u.open_event_id, '00000000-0000-0000-0000-000000000000'::uuid)
		FROM unnest($2::uuid[], $3::uuid[], $4::smallint[], $5::int[], $6::int[], $7::int[], $8::int[],
			$9::timestamptz[], $10::timestamptz[], $11::timestamptz[], $12::timestamptz[],
			$13::smallint[], $14::timestamptz[], $15::real[], $16::bigint[], $17::text[], $18::uuid[])
			AS u(probe_id, target_id, probe_type, consec_fails, consec_oks, consec_degraded, consec_clean,
				first_fail_at, first_ok_at, first_degraded_at, first_clean_at,
				last_status, last_time, last_loss_pct, last_latency_us, last_latency_source, open_event_id)
		ON CONFLICT (agent_id, probe_id) DO UPDATE SET
			target_id = EXCLUDED.target_id,
			probe_type = EXCLUDED.probe_type,
			consec_fails = EXCLUDED.consec_fails,
			consec_oks = EXCLUDED.consec_oks,
			consec_degraded = EXCLUDED.consec_degraded,
			consec_clean = EXCLUDED.consec_clean,
			first_fail_at = EXCLUDED.first_fail_at,
			first_ok_at = EXCLUDED.first_ok_at,
			first_degraded_at = EXCLUDED.first_degraded_at,
			first_clean_at = EXCLUDED.first_clean_at,
			last_status = EXCLUDED.last_status,
			last_time = EXCLUDED.last_time,
			last_loss_pct = EXCLUDED.last_loss_pct,
			last_latency_us = EXCLUDED.last_latency_us,
			last_latency_source = EXCLUDED.last_latency_source,
			open_event_id = EXCLUDED.open_event_id`,
		agentID, probeIDs, targetIDs, probeTypes, consecFails, consecOKs, consecDegraded, consecClean,
		firstFails, firstOKs, firstDegradeds, firstCleans, lastStatuses, lastTimes,
		lastLossPcts, lastLatencies, lastSources, openEventIDs)
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
