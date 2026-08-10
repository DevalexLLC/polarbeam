package outage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// offlineSeenGrace is how stale agents.last_seen_at must be before an agent
// counts as offline: 4× the 30 s config-stream touch, so one missed tick
// never trips it.
const offlineSeenGrace = 2 * time.Minute

// SweepConfig configures the silence sweep. Zero Interval means 30 s.
type SweepConfig struct {
	Interval time.Duration
	// AssignedProbeIDs returns the ID of every probe series agents are
	// currently expected to run (store.EnabledProbeIDs). When set, each
	// sweep also closes open probe_failing events whose probe is not in
	// the set — orphans nothing else can ever close, because their probe
	// no longer produces the successes the hysteresis close needs. Every
	// config mutation closes its own events (cleanupSeries), so a live
	// open event always has an enabled probe behind it; orphans arise
	// from straggler results accepted during the assignment cache's
	// staleness window, and from results an old server ingested under
	// the pre-0012 mesh probe identity during a live upgrade.
	AssignedProbeIDs func(context.Context) ([]uuid.UUID, error)
}

// Sweep periodically opens and closes agent_offline events until ctx is
// done. It is result- and stream-silence driven: an agent with no result in
// 3× its fastest probe interval AND a stale last_seen_at gets exactly one
// open event (the partial unique index resolves races); the event closes on
// the first sweep after either signal resumes. Agents that never connected
// are onboarding problems, not outages, and are skipped.
func Sweep(ctx context.Context, db DB, cfg SweepConfig) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sweepOnce(ctx, db, time.Now()); err != nil && ctx.Err() == nil {
				slog.Error("agent_offline sweep failed", "err", err)
			}
			if cfg.AssignedProbeIDs == nil {
				continue
			}
			if err := closeOrphanEvents(ctx, db, cfg.AssignedProbeIDs, time.Now()); err != nil && ctx.Err() == nil {
				slog.Error("orphan probe_failing sweep failed", "err", err)
			}
		}
	}
}

// closeOrphanEvents closes open probe_failing events whose probe is not in
// the assigned set, and clears the closed events' series_state pointers and
// hysteresis counters in the same statement — a dangling open_event_id
// would suppress every future open for a re-enabled, still-failing probe
// until three successes cleared it. An empty set is meaningful, not a
// guard condition: with no probes configured, any open probe_failing event
// is an orphan.
//
// The set read and the close are deliberately not one transaction: a probe
// re-enabled in the milliseconds between them could have a legitimate
// event closed, but the reset counters re-open it after Threshold further
// failures, so the race self-heals and is not worth cross-package locking.
func closeOrphanEvents(ctx context.Context, db DB, assigned func(context.Context) ([]uuid.UUID, error), now time.Time) error {
	ids, err := assigned(ctx)
	if err != nil {
		return fmt.Errorf("assigned probe ids: %w", err)
	}
	if ids == nil {
		// pgx encodes a nil slice as SQL NULL, and `!= ALL(NULL)` is
		// unknown for every row — the sweep would silently close nothing.
		ids = []uuid.UUID{}
	}
	// Two separate autocommit statements, NOT one transaction or CTE:
	// Apply locks series_state FOR UPDATE and then updates outage_events,
	// so any single transaction here touching both tables risks a lock
	// inversion deadlock against concurrent ingest. Each statement below
	// only ever locks one table, which makes deadlock impossible.
	//
	// Each statement is an independent reconciliation, so any state a
	// concurrent ingest creates between them is repaired on this or the
	// next tick rather than stranded:
	//
	// 1. Close orphan events (assignment-filtered). A legitimately open
	//    event always has an enabled probe behind it — every config
	//    mutation closes its own events via cleanupSeries.
	// 2. Repair the pointer invariant (deliberately NOT assignment-
	//    filtered): no series_state.open_event_id may reference a closed
	//    event. The legit close path clears the pointer in the ingest
	//    transaction, so a pointer to a closed event is always damage —
	//    from the close above, from a crash between these statements, or
	//    from a straggler batch — and a dangling one would suppress every
	//    future open for a still-failing probe until three successes.
	//    An ingest that opens a brand-new event between the statements is
	//    left intact (its event is open), and the next tick closes it if
	//    still unassigned, with the same tick's repair clearing the
	//    pointer.
	//
	//    Both steps are one single-row UPDATE per damaged row, never one
	//    bulk UPDATE: Apply locks a sorted batch of series_state rows FOR
	//    UPDATE and then closes their event rows in the same sorted order,
	//    and a concurrent multi-row UPDATE with a plan-dependent scan
	//    order could deadlock against either table's batch. A statement
	//    that locks at most one row cannot hold-and-wait, and each
	//    predicate re-checks its condition under the row lock, so a row
	//    ingest changed in the meantime is left alone. The damage sets
	//    are almost always empty.
	rows, err := db.Query(ctx, `
		SELECT id FROM outage_events
		WHERE kind = 'probe_failing' AND closed_at IS NULL AND probe_id != ALL($1)`, ids)
	if err != nil {
		return fmt.Errorf("find orphan events: %w", err)
	}
	var orphans []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("find orphan events: %w", err)
		}
		orphans = append(orphans, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("find orphan events: %w", err)
	}
	var closed int64
	for _, id := range orphans {
		tag, err := db.Exec(ctx, `
			UPDATE outage_events SET closed_at = $2
			WHERE id = $1 AND closed_at IS NULL`, id, now)
		if err != nil {
			return fmt.Errorf("close orphan event: %w", err)
		}
		closed += tag.RowsAffected()
	}
	rows, err = db.Query(ctx, `
		SELECT ss.agent_id, ss.probe_id FROM series_state ss
		JOIN outage_events oe ON oe.id = ss.open_event_id
		WHERE oe.closed_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("find dangling series pointers: %w", err)
	}
	type seriesKey struct{ agentID, probeID uuid.UUID }
	var damaged []seriesKey
	for rows.Next() {
		var k seriesKey
		if err := rows.Scan(&k.agentID, &k.probeID); err != nil {
			rows.Close()
			return fmt.Errorf("find dangling series pointers: %w", err)
		}
		damaged = append(damaged, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("find dangling series pointers: %w", err)
	}
	for _, k := range damaged {
		if _, err := db.Exec(ctx, `
			UPDATE series_state SET open_event_id = NULL, consec_fails = 0,
			    consec_oks = 0, first_fail_at = NULL, first_ok_at = NULL
			WHERE agent_id = $1 AND probe_id = $2 AND open_event_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM outage_events oe
			              WHERE oe.id = open_event_id AND oe.closed_at IS NOT NULL)`,
			k.agentID, k.probeID); err != nil {
			return fmt.Errorf("reset dangling series pointer: %w", err)
		}
	}
	if closed > 0 {
		slog.Warn("closed orphaned probe_failing events for probes no longer assigned",
			"count", closed)
	}
	return nil
}

// agentSignals is everything the offline decision needs for one agent.
type agentSignals struct {
	AgentID     uuid.UUID
	Hostname    string
	LastSeen    time.Time
	MinInterval time.Duration // fastest applicable probe interval; 0 = none configured
	LastResult  time.Time     // newest series_state.last_time; zero = none
	OpenEventID *uuid.UUID    // open agent_offline event, if any
}

// decideOffline is the pure decision: open when both signals are silent,
// close when either resumes.
func decideOffline(now time.Time, s agentSignals) (open, closeEvent bool) {
	// With no configured probes there is no result cadence to miss; the
	// stream heartbeat alone decides.
	resultSilent := s.LastResult.IsZero() || s.MinInterval <= 0 ||
		now.Sub(s.LastResult) > 3*s.MinInterval
	seenSilent := now.Sub(s.LastSeen) > offlineSeenGrace
	silent := resultSilent && seenSilent
	if s.OpenEventID == nil {
		return silent, false
	}
	return false, !silent
}

func sweepOnce(ctx context.Context, db DB, now time.Time) error {
	rows, err := db.Query(ctx, `
		SELECT a.id, a.hostname, a.last_seen_at,
			(SELECT min(pc.interval_ms) FROM probe_configs pc
			 WHERE pc.enabled AND (
				pc.site_id = a.site_id
				OR (pc.mesh_id IS NOT NULL
					AND EXISTS (SELECT 1 FROM mesh_members mm
						WHERE mm.mesh_id = pc.mesh_id AND mm.site_id = a.site_id)
					AND EXISTS (SELECT 1 FROM mesh_members mo
						WHERE mo.mesh_id = pc.mesh_id AND mo.site_id <> a.site_id)))),
			(SELECT max(ss.last_time) FROM series_state ss WHERE ss.agent_id = a.id),
			(SELECT oe.id FROM outage_events oe
			 WHERE oe.agent_id = a.id AND oe.kind = 'agent_offline' AND oe.closed_at IS NULL)
		FROM agents a
		WHERE a.last_seen_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("sweep query: %w", err)
	}
	defer rows.Close()

	var agents []agentSignals
	for rows.Next() {
		var (
			s          agentSignals
			intervalMS *int32
			lastResult *time.Time
		)
		if err := rows.Scan(&s.AgentID, &s.Hostname, &s.LastSeen, &intervalMS, &lastResult, &s.OpenEventID); err != nil {
			return fmt.Errorf("sweep scan: %w", err)
		}
		if intervalMS != nil {
			s.MinInterval = time.Duration(*intervalMS) * time.Millisecond
		}
		if lastResult != nil {
			s.LastResult = *lastResult
		}
		agents = append(agents, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweep query: %w", err)
	}

	for _, s := range agents {
		open, closeEvent := decideOffline(now, s)
		switch {
		case open:
			// opened_at is the last evidence of life, not sweep time.
			openedAt := s.LastSeen
			if s.LastResult.After(openedAt) {
				openedAt = s.LastResult
			}
			tag, err := db.Exec(ctx, `
				INSERT INTO outage_events (kind, agent_id, opened_at)
				VALUES ('agent_offline', $1, $2)
				ON CONFLICT (agent_id) WHERE kind = 'agent_offline' AND closed_at IS NULL
				DO NOTHING`, s.AgentID, openedAt)
			if err != nil {
				return fmt.Errorf("open agent_offline: %w", err)
			}
			if tag.RowsAffected() > 0 {
				slog.Warn("agent offline", "agent", s.AgentID, "hostname", s.Hostname, "since", openedAt)
			}
		case closeEvent:
			if _, err := db.Exec(ctx, `
				UPDATE outage_events SET closed_at = $2 WHERE id = $1 AND closed_at IS NULL`,
				*s.OpenEventID, now); err != nil {
				return fmt.Errorf("close agent_offline: %w", err)
			}
			slog.Info("agent back online", "agent", s.AgentID, "hostname", s.Hostname)
		}
	}
	return nil
}
