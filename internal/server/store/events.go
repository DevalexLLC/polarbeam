// Dashboard read queries for outage and path events (M4).
package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OutageInfo is one outage_events row joined with display names. Joins are
// LEFT so events survive deleted agents/targets; empty strings and nils
// mark the gaps rather than dropping history.
type OutageInfo struct {
	ID            uuid.UUID
	Kind          string
	AgentID       uuid.UUID
	ProbeID       *uuid.UUID // nil for agent_offline and legacy rows
	TargetID      *uuid.UUID // nil for agent_offline and legacy rows
	AgentHostname string
	Network       string // "" once the agent row is deleted
	SrcSite       string
	DstSite       *string // nil for agent_offline and external targets
	TargetName    *string // nil for agent_offline
	ProbeType     *int16  // nil for agent_offline
	OpenedAt      time.Time
	ClosedAt      *time.Time // nil = still open
	Error         *string
	RelatedRoutes []PathEventInfo
}

const incidentRouteWindow = 15 * time.Minute

// ListOutages returns open events (always, uncapped) plus up to 500
// events closed within the window, newest first. The two disjoint
// branches (open ∪ recently closed) replace a closed_at IS NULL OR
// closed_at > cutoff predicate no index could serve against
// forever-retained history: the open branch rides the partial open-event
// indexes, the closed branch range-scans outage_events_closed_idx. The
// LIMIT is parenthesized onto the closed branch only — attached to the
// union it would drop the oldest open outages during a >500-event
// incident, exactly when the dashboard must show them all.
// networks is the caller's network scope (nil = unfiltered). Scoped callers
// also lose events whose agent row is gone (plane unknowable): attribution
// fails closed rather than leaking a possibly foreign event. The scope
// predicate lives INSIDE both union branches, not on the outer query: the
// closed branch's LIMIT must count only in-scope events, or a noisy foreign
// tenant's 500 newest closed outages would crowd a scoped tenant's own
// history out of the response entirely.
func (s *Store) ListOutages(ctx context.Context, window time.Duration, networks []uuid.UUID, includeRoutes bool) ([]OutageInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT oe.id, oe.kind, oe.agent_id, oe.probe_id, oe.target_id,
			COALESCE(a.hostname, ''), COALESCE(n.name, ''),
			COALESCE(src.name, ''),
			dst.name, t.name, oe.probe_type, oe.opened_at, oe.closed_at, oe.open_error
		FROM (
			SELECT id, kind, agent_id, probe_id, target_id, probe_type, opened_at, closed_at, open_error
			FROM outage_events
			WHERE closed_at IS NULL
			  AND ($2::uuid[] IS NULL
			       OR agent_id IN (SELECT id FROM agents WHERE network_id = ANY($2)))
			UNION ALL
			(SELECT id, kind, agent_id, probe_id, target_id, probe_type, opened_at, closed_at, open_error
			FROM outage_events
			WHERE closed_at > now() - $1::interval
			  AND ($2::uuid[] IS NULL
			       OR agent_id IN (SELECT id FROM agents WHERE network_id = ANY($2)))
			ORDER BY opened_at DESC
			LIMIT 500)
		) oe
		LEFT JOIN agents a ON a.id = oe.agent_id
		LEFT JOIN networks n ON n.id = a.network_id
		LEFT JOIN sites src ON src.id = a.site_id
		LEFT JOIN targets t ON t.id = oe.target_id
		LEFT JOIN agents ta ON ta.id = t.agent_id
		LEFT JOIN sites dst ON dst.id = ta.site_id
		ORDER BY oe.opened_at DESC`, window, networks)
	if err != nil {
		return nil, fmt.Errorf("list outages: %w", err)
	}
	defer rows.Close()

	var out []OutageInfo
	for rows.Next() {
		var o OutageInfo
		if err := rows.Scan(&o.ID, &o.Kind, &o.AgentID, &o.ProbeID, &o.TargetID,
			&o.AgentHostname, &o.Network, &o.SrcSite,
			&o.DstSite, &o.TargetName, &o.ProbeType, &o.OpenedAt, &o.ClosedAt, &o.Error); err != nil {
			return nil, fmt.Errorf("scan outage: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(out) == 0 || !includeRoutes {
		return out, nil
	}
	candidates, err := s.listIncidentRouteCandidates(ctx, window, networks)
	if err != nil {
		return nil, err
	}
	correlateIncidentRoutes(out, candidates)
	return out, nil
}

// PathEventInfo is one path_events row joined with display names. Hops are
// passed through as raw JSON — the API serves them verbatim.
type PathEventInfo struct {
	ID            uuid.UUID
	Time          time.Time
	AgentID       uuid.UUID
	ProbeID       uuid.UUID
	AgentHostname string
	Network       string // "" once the agent row is deleted
	SrcSite       string
	DstSite       *string
	TargetName    *string
	// Query mode reads the event's stable ID after target deletion; legacy
	// mode reads the joined target row and returns nil.
	TargetID    *uuid.UUID
	OldPathHash []byte
	NewPathHash []byte
	OldHops     []byte
	NewHops     []byte
	ChangedHops int
}

// listIncidentRouteCandidates uses the Routes page's same stable newest-500
// window, but skips hop payloads that incident cards never render. A related
// route link therefore names an event available in the preserved window.
func (s *Store) listIncidentRouteCandidates(ctx context.Context, window time.Duration, networks []uuid.UUID) ([]PathEventInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pe.id, pe.time, pe.agent_id, pe.probe_id, pe.target_id,
		       COALESCE(a.hostname, ''), COALESCE(n.name, ''),
		       COALESCE(src.name, ''), dst.name, t.name
		  FROM path_events pe
		  LEFT JOIN agents a ON a.id = pe.agent_id
		  LEFT JOIN networks n ON n.id = a.network_id
		  LEFT JOIN sites src ON src.id = a.site_id
		  LEFT JOIN targets t ON t.id = pe.target_id
		  LEFT JOIN agents ta ON ta.id = t.agent_id
		  LEFT JOIN sites dst ON dst.id = ta.site_id
		 WHERE pe.time > now() - $1::interval
		   AND ($2::uuid[] IS NULL OR a.network_id = ANY($2))
		 ORDER BY pe.time DESC, pe.id DESC
		 LIMIT 500`, window, networks)
	if err != nil {
		return nil, fmt.Errorf("list incident route candidates: %w", err)
	}
	defer rows.Close()
	var out []PathEventInfo
	for rows.Next() {
		var e PathEventInfo
		if err := rows.Scan(&e.ID, &e.Time, &e.AgentID, &e.ProbeID, &e.TargetID,
			&e.AgentHostname, &e.Network, &e.SrcSite, &e.DstSite, &e.TargetName); err != nil {
			return nil, fmt.Errorf("scan incident route candidate: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func usableUUID(id uuid.UUID) bool { return id != uuid.Nil }

func usableUUIDPtr(id *uuid.UUID) bool { return id != nil && usableUUID(*id) }

func sameIncidentLabel(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func sameIncidentLabelPtr(a, b *string) bool {
	return a != nil && b != nil && sameIncidentLabel(*a, *b)
}

// incidentRouteMatches gives usable stable IDs absolute precedence: an ID
// mismatch never degrades into a same-label match. Labels are consulted only
// when a legacy row lacks one of the identities needed for an exact match.
func incidentRouteMatches(o OutageInfo, e PathEventInfo) bool {
	agentExact := usableUUID(o.AgentID) && usableUUID(e.AgentID)
	if agentExact && o.AgentID != e.AgentID {
		return false
	}
	if o.Kind == "agent_offline" {
		if agentExact {
			return true
		}
		return sameIncidentLabel(o.AgentHostname, e.AgentHostname) || sameIncidentLabel(o.SrcSite, e.SrcSite)
	}

	targetExact := usableUUIDPtr(o.TargetID) && usableUUIDPtr(e.TargetID)
	if targetExact && *o.TargetID != *e.TargetID {
		return false
	}
	// A failure probe and a traceroute probe are different configurations,
	// so their probe IDs normally differ. Exact agent+target identity is the
	// cross-probe series key; probe ID becomes decisive only for legacy rows
	// whose target identity is unavailable.
	if agentExact && targetExact {
		return true
	}
	probeExact := usableUUIDPtr(o.ProbeID) && usableUUID(e.ProbeID)
	if probeExact && *o.ProbeID != e.ProbeID {
		return false
	}
	if agentExact && probeExact {
		return true
	}

	sameSource := sameIncidentLabel(o.AgentHostname, e.AgentHostname) || sameIncidentLabel(o.SrcSite, e.SrcSite)
	sameDestination := sameIncidentLabelPtr(o.DstSite, e.DstSite) || sameIncidentLabelPtr(o.TargetName, e.TargetName)
	return sameSource && sameDestination
}

func incidentRouteDistance(o OutageInfo, at time.Time) (time.Duration, bool) {
	best := absDuration(at.Sub(o.OpenedAt))
	matched := best <= incidentRouteWindow
	if o.ClosedAt != nil {
		closedDistance := absDuration(at.Sub(*o.ClosedAt))
		if closedDistance <= incidentRouteWindow && (!matched || closedDistance < best) {
			best, matched = closedDistance, true
		}
	}
	return best, matched
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func incidentRouteKey(e PathEventInfo) string {
	if usableUUID(e.ID) {
		return e.ID.String()
	}
	return fmt.Sprintf("legacy:%d:%s:%s:%s:%s", e.Time.UnixNano(), e.AgentHostname,
		e.SrcSite, pointerString(e.DstSite), pointerString(e.TargetName))
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func correlateIncidentRoutes(outages []OutageInfo, candidates []PathEventInfo) {
	type rankedRoute struct {
		path     PathEventInfo
		distance time.Duration
	}
	for i := range outages {
		seen := make(map[string]struct{})
		matches := make([]rankedRoute, 0, 3)
		for _, candidate := range candidates {
			distance, inWindow := incidentRouteDistance(outages[i], candidate.Time)
			if !inWindow || !incidentRouteMatches(outages[i], candidate) {
				continue
			}
			key := incidentRouteKey(candidate)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			matches = append(matches, rankedRoute{path: candidate, distance: distance})
		}
		sort.Slice(matches, func(a, b int) bool {
			if matches[a].distance != matches[b].distance {
				return matches[a].distance < matches[b].distance
			}
			if !matches[a].path.Time.Equal(matches[b].path.Time) {
				return matches[a].path.Time.After(matches[b].path.Time)
			}
			return matches[a].path.ID.String() > matches[b].path.ID.String()
		})
		limit := min(3, len(matches))
		outages[i].RelatedRoutes = make([]PathEventInfo, limit)
		for j := range limit {
			outages[i].RelatedRoutes[j] = matches[j].path
		}
	}
}

// PathEventFilter is the validated query-mode contract for route changes.
// Query matches display evidence case-insensitively. Sort and Order are
// revalidated here before they become an ORDER BY clause; Networks is nil
// for an unscoped caller and non-nil for a narrowed authenticated scope.
type PathEventFilter struct {
	Query    string
	Sort     string
	Order    string
	Limit    int
	Offset   int
	Networks []uuid.UUID
}

// ListPathEvents returns path change events within the window, newest first.
// networks is the caller's network scope (nil = unfiltered); deleted-agent
// events fail closed for scoped callers, as in ListOutages.
func (s *Store) ListPathEvents(ctx context.Context, window time.Duration, networks []uuid.UUID) ([]PathEventInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pe.id, pe.time, COALESCE(a.hostname, ''), COALESCE(n.name, ''),
			COALESCE(src.name, ''),
			dst.name, t.name, t.id, pe.old_path_hash, pe.new_path_hash, pe.old_hops, pe.new_hops
		FROM path_events pe
		LEFT JOIN agents a ON a.id = pe.agent_id
		LEFT JOIN networks n ON n.id = a.network_id
		LEFT JOIN sites src ON src.id = a.site_id
		LEFT JOIN targets t ON t.id = pe.target_id
		LEFT JOIN agents ta ON ta.id = t.agent_id
		LEFT JOIN sites dst ON dst.id = ta.site_id
		WHERE pe.time > now() - $1::interval
		  AND ($2::uuid[] IS NULL OR a.network_id = ANY($2))
		ORDER BY pe.time DESC, pe.id DESC
		LIMIT 500`, window, networks)
	if err != nil {
		return nil, fmt.Errorf("list path events: %w", err)
	}
	defer rows.Close()

	var out []PathEventInfo
	for rows.Next() {
		var e PathEventInfo
		if err := rows.Scan(&e.ID, &e.Time, &e.AgentHostname, &e.Network, &e.SrcSite,
			&e.DstSite, &e.TargetName, &e.TargetID, &e.OldPathHash, &e.NewPathHash, &e.OldHops, &e.NewHops); err != nil {
			return nil, fmt.Errorf("scan path event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// pathEventMatchingSQL returns the newest matching rows before query-mode
// presentation sorting. countProbe selects only the columns needed to count
// up to 501 rows; the page query takes 500 complete rows. The safety cap
// therefore applies after search/scope and before an alternate sort.
func pathEventMatchingSQL(countProbe bool) string {
	limit := "500"
	projection := `pe.id, pe.time, pe.agent_id, pe.probe_id, pe.target_id,
		       COALESCE(a.hostname, '') AS agent_hostname,
		       COALESCE(n.name, '') AS network,
		       COALESCE(src.name, '') AS src_site,
		       dst.name AS dst_site, t.name AS target_name,
		       pe.old_path_hash, pe.new_path_hash, pe.old_hops, pe.new_hops`
	if countProbe {
		limit = "501"
		projection = "pe.id"
	}
	return `
		SELECT ` + projection + `
		  FROM path_events pe
		  LEFT JOIN agents a ON a.id = pe.agent_id
		  LEFT JOIN networks n ON n.id = a.network_id
		  LEFT JOIN sites src ON src.id = a.site_id
		  LEFT JOIN targets t ON t.id = pe.target_id
		  LEFT JOIN agents ta ON ta.id = t.agent_id
		  LEFT JOIN sites dst ON dst.id = ta.site_id
		 WHERE pe.time > now() - $1::interval
		   AND ($2::uuid[] IS NULL OR a.network_id = ANY($2))
		   AND ($3 = ''
		        OR COALESCE(a.hostname, '') ILIKE '%' || $3 || '%'
		        OR COALESCE(src.name, '') ILIKE '%' || $3 || '%'
		        OR COALESCE(dst.name, '') ILIKE '%' || $3 || '%'
		        OR COALESCE(t.name, '') ILIKE '%' || $3 || '%')
		 ORDER BY pe.time DESC, pe.id DESC
		 LIMIT ` + limit
}

func pathEventOrder(sortName, order string) (string, error) {
	if order != "asc" && order != "desc" {
		return "", invalidf("path event order must be asc or desc")
	}
	direction := " ASC"
	if order == "desc" {
		direction = " DESC"
	}
	var column string
	switch sortName {
	case "time":
		column = "e.time"
	case "agent":
		column = "lower(e.agent_hostname)"
	case "source":
		column = "lower(e.src_site)"
	case "destination":
		column = "lower(COALESCE(e.dst_site, e.target_name, ''))"
	case "changes":
		column = "e.changed_hops"
	default:
		return "", invalidf("unknown path event sort %q", sortName)
	}
	return column + direction + ", e.id" + direction, nil
}

// QueryPathEvents filters, counts, sorts, and pages route changes in SQL.
// total describes the capped result set; truncated reports a 501st match.
// ChangedHops compares one deduplicated address set per TTL, deliberately
// ignoring RTTs and counting added/removed TTLs through a FULL JOIN. TTL
// identity stays text because the wire's uint32 range exceeds SQL integer.
func (s *Store) QueryPathEvents(ctx context.Context, window time.Duration, f PathEventFilter) ([]PathEventInfo, int64, bool, error) {
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 {
		return nil, 0, false, invalidf("invalid path event page")
	}
	orderBy, err := pathEventOrder(f.Sort, f.Order)
	if err != nil {
		return nil, 0, false, err
	}
	query := escapeLike(strings.TrimSpace(f.Query))

	countSQL := `WITH matching AS MATERIALIZED (` + pathEventMatchingSQL(true) + `)
		SELECT LEAST(count(*), 500), count(*) > 500 FROM matching`
	var total int64
	var truncated bool
	if err := s.pool.QueryRow(ctx, countSQL, window, f.Networks, query).Scan(&total, &truncated); err != nil {
		return nil, 0, false, fmt.Errorf("count path events: %w", err)
	}
	if int64(f.Offset) >= total {
		return []PathEventInfo{}, total, truncated, nil
	}

	listSQL := `WITH matching AS MATERIALIZED (` + pathEventMatchingSQL(false) + `),
		enriched AS (
			SELECT m.*, changed.changed_hops
			  FROM matching m
			 CROSS JOIN LATERAL (
				WITH old_ttls AS (
					SELECT hop->>'ttl' AS ttl,
					       COALESCE(array_agg(DISTINCT address.value ORDER BY address.value)
					         FILTER (WHERE address.value IS NOT NULL), ARRAY[]::text[]) AS addresses
					  FROM jsonb_array_elements(m.old_hops) AS hop
					  LEFT JOIN LATERAL jsonb_array_elements_text(
					       COALESCE(hop->'addrs', '[]'::jsonb)) AS address(value) ON true
					 GROUP BY hop->>'ttl'
				), new_ttls AS (
					SELECT hop->>'ttl' AS ttl,
					       COALESCE(array_agg(DISTINCT address.value ORDER BY address.value)
					         FILTER (WHERE address.value IS NOT NULL), ARRAY[]::text[]) AS addresses
					  FROM jsonb_array_elements(m.new_hops) AS hop
					  LEFT JOIN LATERAL jsonb_array_elements_text(
					       COALESCE(hop->'addrs', '[]'::jsonb)) AS address(value) ON true
					 GROUP BY hop->>'ttl'
				)
				SELECT count(*)::integer AS changed_hops
				  FROM old_ttls o FULL JOIN new_ttls n USING (ttl)
				 WHERE o.addresses IS DISTINCT FROM n.addresses
			) changed
		)
		SELECT e.id, e.time, e.agent_id, e.probe_id, e.target_id,
		       e.agent_hostname, e.network, e.src_site, e.dst_site, e.target_name,
		       e.old_path_hash, e.new_path_hash, e.old_hops, e.new_hops, e.changed_hops
		  FROM enriched e
		 ORDER BY ` + orderBy + `
		 LIMIT $4 OFFSET $5`
	rows, err := s.pool.Query(ctx, listSQL, window, f.Networks, query, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, false, fmt.Errorf("query path events: %w", err)
	}
	defer rows.Close()

	out := make([]PathEventInfo, 0, f.Limit)
	for rows.Next() {
		var e PathEventInfo
		if err := rows.Scan(&e.ID, &e.Time, &e.AgentID, &e.ProbeID, &e.TargetID,
			&e.AgentHostname, &e.Network, &e.SrcSite, &e.DstSite, &e.TargetName,
			&e.OldPathHash, &e.NewPathHash, &e.OldHops, &e.NewHops, &e.ChangedHops); err != nil {
			return nil, 0, false, fmt.Errorf("scan queried path event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return out, total, truncated, nil
}

// CurrentPath is one traceroute_current row for a direction of a site
// pair. (AgentID, ProbeID) is the series identity, mirroring the table's
// primary key: several destination agents or templates put one source
// hostname in multiple rows with distinct probe IDs, while several
// source agents at one site share a probe ID (mesh IDs are derived per
// source site, direct probes are assigned site-wide) and differ only by
// agent ID.
type CurrentPath struct {
	AgentID       uuid.UUID
	ProbeID       uuid.UUID
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
		SELECT tc.agent_id, tc.probe_id, COALESCE(a.hostname, ''), tc.updated_at,
			tc.dest_reached, tc.path_hash, tc.hops
		FROM traceroute_current tc
		LEFT JOIN agents a ON a.id = tc.agent_id
		WHERE tc.agent_id = ANY($1) AND tc.target_id = ANY($2)
		ORDER BY a.hostname, tc.agent_id, tc.probe_id`, srcAgents, dstTargets)
	if err != nil {
		return nil, fmt.Errorf("current paths: %w", err)
	}
	defer rows.Close()

	var out []CurrentPath
	for rows.Next() {
		var p CurrentPath
		if err := rows.Scan(&p.AgentID, &p.ProbeID, &p.AgentHostname, &p.UpdatedAt,
			&p.DestReached, &p.PathHash, &p.Hops); err != nil {
			return nil, fmt.Errorf("scan current path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CurrentPathMTU is one path_mtu_current row for a direction of a site
// pair. Sizes are IP-packet bytes including the IP header. (AgentID,
// ProbeID) is the series identity, mirroring the table's primary key:
// several destination agents or templates put one source hostname in
// multiple rows with distinct probe IDs, while several source agents at
// one site share a probe ID (mesh IDs are derived per source site,
// direct probes are assigned site-wide) and differ only by agent ID.
type CurrentPathMTU struct {
	AgentID         uuid.UUID
	ProbeID         uuid.UUID
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
		SELECT mc.agent_id, mc.probe_id, COALESCE(a.hostname, ''), mc.updated_at,
			mc.largest_ok_bytes, mc.smallest_failed_bytes, mc.next_hop_mtu_bytes,
			mc.ip_version, mc.black_hole, mc.local_constraint, mc.rtt_us
		FROM path_mtu_current mc
		LEFT JOIN agents a ON a.id = mc.agent_id
		WHERE mc.agent_id = ANY($1) AND mc.target_id = ANY($2)
		ORDER BY a.hostname, mc.agent_id, mc.probe_id`, srcAgents, dstTargets)
	if err != nil {
		return nil, fmt.Errorf("current path MTUs: %w", err)
	}
	defer rows.Close()

	var out []CurrentPathMTU
	for rows.Next() {
		var m CurrentPathMTU
		if err := rows.Scan(&m.AgentID, &m.ProbeID, &m.AgentHostname, &m.UpdatedAt,
			&m.LargestOK, &m.SmallestFailed, &m.NextHopMTU,
			&m.IPVersion, &m.BlackHole, &m.LocalConstraint, &m.RttUS); err != nil {
			return nil, fmt.Errorf("scan current path MTU: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
