// Dashboard read queries for outage and path events (M4).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OutageInfo is one outage_events row joined with display names. Joins are
// LEFT so events survive deleted agents/targets; empty strings and nils
// mark the gaps rather than dropping history.
type OutageInfo struct {
	ID            uuid.UUID
	Kind          string
	AgentHostname string
	SrcSite       string
	DstSite       *string // nil for agent_offline and external targets
	TargetName    *string // nil for agent_offline
	ProbeType     *int16  // nil for agent_offline
	OpenedAt      time.Time
	ClosedAt      *time.Time // nil = still open
	Error         *string
}

// ListOutages returns open events (always, uncapped) plus up to 500
// events closed within the window, newest first. The two disjoint
// branches (open ∪ recently closed) replace a closed_at IS NULL OR
// closed_at > cutoff predicate no index could serve against
// forever-retained history: the open branch rides the partial open-event
// indexes, the closed branch range-scans outage_events_closed_idx. The
// LIMIT is parenthesized onto the closed branch only — attached to the
// union it would drop the oldest open outages during a >500-event
// incident, exactly when the dashboard must show them all.
func (s *Store) ListOutages(ctx context.Context, window time.Duration) ([]OutageInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT oe.id, oe.kind, COALESCE(a.hostname, ''), COALESCE(src.name, ''),
			dst.name, t.name, oe.probe_type, oe.opened_at, oe.closed_at, oe.open_error
		FROM (
			SELECT id, kind, agent_id, target_id, probe_type, opened_at, closed_at, open_error
			FROM outage_events
			WHERE closed_at IS NULL
			UNION ALL
			(SELECT id, kind, agent_id, target_id, probe_type, opened_at, closed_at, open_error
			FROM outage_events
			WHERE closed_at > now() - $1::interval
			ORDER BY opened_at DESC
			LIMIT 500)
		) oe
		LEFT JOIN agents a ON a.id = oe.agent_id
		LEFT JOIN sites src ON src.id = a.site_id
		LEFT JOIN targets t ON t.id = oe.target_id
		LEFT JOIN agents ta ON ta.id = t.agent_id
		LEFT JOIN sites dst ON dst.id = ta.site_id
		ORDER BY oe.opened_at DESC`, window)
	if err != nil {
		return nil, fmt.Errorf("list outages: %w", err)
	}
	defer rows.Close()

	var out []OutageInfo
	for rows.Next() {
		var o OutageInfo
		if err := rows.Scan(&o.ID, &o.Kind, &o.AgentHostname, &o.SrcSite,
			&o.DstSite, &o.TargetName, &o.ProbeType, &o.OpenedAt, &o.ClosedAt, &o.Error); err != nil {
			return nil, fmt.Errorf("scan outage: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PathEventInfo is one path_events row joined with display names. Hops are
// passed through as raw JSON — the API serves them verbatim.
type PathEventInfo struct {
	ID            uuid.UUID
	Time          time.Time
	AgentHostname string
	SrcSite       string
	DstSite       *string
	TargetName    *string
	OldPathHash   []byte
	NewPathHash   []byte
	OldHops       []byte
	NewHops       []byte
}

// ListPathEvents returns path change events within the window, newest first.
func (s *Store) ListPathEvents(ctx context.Context, window time.Duration) ([]PathEventInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pe.id, pe.time, COALESCE(a.hostname, ''), COALESCE(src.name, ''),
			dst.name, t.name, pe.old_path_hash, pe.new_path_hash, pe.old_hops, pe.new_hops
		FROM path_events pe
		LEFT JOIN agents a ON a.id = pe.agent_id
		LEFT JOIN sites src ON src.id = a.site_id
		LEFT JOIN targets t ON t.id = pe.target_id
		LEFT JOIN agents ta ON ta.id = t.agent_id
		LEFT JOIN sites dst ON dst.id = ta.site_id
		WHERE pe.time > now() - $1::interval
		ORDER BY pe.time DESC
		LIMIT 500`, window)
	if err != nil {
		return nil, fmt.Errorf("list path events: %w", err)
	}
	defer rows.Close()

	var out []PathEventInfo
	for rows.Next() {
		var e PathEventInfo
		if err := rows.Scan(&e.ID, &e.Time, &e.AgentHostname, &e.SrcSite,
			&e.DstSite, &e.TargetName, &e.OldPathHash, &e.NewPathHash, &e.OldHops, &e.NewHops); err != nil {
			return nil, fmt.Errorf("scan path event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CurrentPath is one traceroute_current row for a direction of a site pair.
type CurrentPath struct {
	AgentID       uuid.UUID
	AgentHostname string
	UpdatedAt     time.Time
	DestReached   bool
	PathHash      []byte
	Hops          []byte
}

// CurrentPaths returns the latest complete paths from any of srcAgents to
// any of dstTargets (a site can field several agents).
func (s *Store) CurrentPaths(ctx context.Context, srcAgents, dstTargets []uuid.UUID) ([]CurrentPath, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.agent_id, COALESCE(a.hostname, ''), tc.updated_at, tc.dest_reached,
			tc.path_hash, tc.hops
		FROM traceroute_current tc
		LEFT JOIN agents a ON a.id = tc.agent_id
		WHERE tc.agent_id = ANY($1) AND tc.target_id = ANY($2)
		ORDER BY a.hostname`, srcAgents, dstTargets)
	if err != nil {
		return nil, fmt.Errorf("current paths: %w", err)
	}
	defer rows.Close()

	var out []CurrentPath
	for rows.Next() {
		var p CurrentPath
		if err := rows.Scan(&p.AgentID, &p.AgentHostname, &p.UpdatedAt, &p.DestReached,
			&p.PathHash, &p.Hops); err != nil {
			return nil, fmt.Errorf("scan current path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CurrentPathMTU is one path_mtu_current row for a direction of a site
// pair. Sizes are IP-packet bytes including the IP header.
type CurrentPathMTU struct {
	AgentID         uuid.UUID
	AgentHostname   string
	UpdatedAt       time.Time
	LargestOK       int32
	SmallestFailed  int32
	NextHopMTU      int32
	IPVersion       int16
	BlackHole       bool
	LocalConstraint bool
	RttUS           *int32
}

// CurrentPathMTUs returns the latest usable path MTU measurements from any
// of srcAgents to any of dstTargets (a site can field several agents).
func (s *Store) CurrentPathMTUs(ctx context.Context, srcAgents, dstTargets []uuid.UUID) ([]CurrentPathMTU, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mc.agent_id, COALESCE(a.hostname, ''), mc.updated_at,
			mc.largest_ok_bytes, mc.smallest_failed_bytes, mc.next_hop_mtu_bytes,
			mc.ip_version, mc.black_hole, mc.local_constraint, mc.rtt_us
		FROM path_mtu_current mc
		LEFT JOIN agents a ON a.id = mc.agent_id
		WHERE mc.agent_id = ANY($1) AND mc.target_id = ANY($2)
		ORDER BY a.hostname`, srcAgents, dstTargets)
	if err != nil {
		return nil, fmt.Errorf("current path MTUs: %w", err)
	}
	defer rows.Close()

	var out []CurrentPathMTU
	for rows.Next() {
		var m CurrentPathMTU
		if err := rows.Scan(&m.AgentID, &m.AgentHostname, &m.UpdatedAt,
			&m.LargestOK, &m.SmallestFailed, &m.NextHopMTU,
			&m.IPVersion, &m.BlackHole, &m.LocalConstraint, &m.RttUS); err != nil {
			return nil, fmt.Errorf("scan current path MTU: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
