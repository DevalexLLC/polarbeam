package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Dashboard read queries. Latest-row queries (matrix, direction status) and
// short windows read the raw probe_results hypertable; long windows read the
// M5 continuous aggregates (probe_results_hourly/daily) selected by Source.
// The fixed-window agent health strips read the narrow probe_results_health_30m
// cagg (migration 0009) unconditionally — no Source branching.
//
// latencyExpr is the per-row latency estimate: real RTT when a prober
// measures it (ICMP, M4+), otherwise the purest available network timing.
// Windowed queries choose one latencySourceExpr value and aggregate only
// successful rows from that family, so charts never mix RTT with connect
// timings or turn a fast failure into apparent low latency.
//
// The same COALESCE ladder is frozen into the hourly cagg definition
// (migration 0002); once that migration ships, changing the order here
// requires a forward-only cagg rebuild or raw and aggregate windows disagree.
const latencyExpr = `COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us)`

const latencySourceExpr = `CASE
	WHEN rtt_avg_us       IS NOT NULL THEN 'rtt'
	WHEN tcp_connect_us   IS NOT NULL THEN 'tcp_connect'
	WHEN tls_handshake_us IS NOT NULL THEN 'tls_handshake'
	WHEN ttfb_us          IS NOT NULL THEN 'ttfb'
	WHEN total_us         IS NOT NULL THEN 'total'
	ELSE '' END`

// latencySourcePriority orders timing families purest-first for
// chooseLatencySource: real RTT beats connect time beats application
// timings. Must list every non-empty latencySourceExpr value.
var latencySourcePriority = []string{"rtt", "tcp_connect", "tls_handshake", "ttfb", "total"}

// chooseLatencySource picks the timing family a direction's window should
// chart: the purest family holding at least 5% of the window's successful
// latency samples. The coverage floor stops a just-enabled prober from
// hiding months of lower-priority history (one fresh RTT sample must not
// blank a 365d TCP chart); the fallback keeps the purest family when
// nothing clears the floor. "" means no successful samples at all.
func chooseLatencySource(counts map[string]int64) string {
	var total int64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return ""
	}
	fallback := ""
	for _, family := range latencySourcePriority {
		n := counts[family]
		if n <= 0 {
			continue
		}
		if n*20 >= total {
			return family
		}
		if fallback == "" {
			fallback = family
		}
	}
	return fallback
}

// Source names the table a windowed pair query reads from. Raw serves the
// short windows at full resolution; hourly/daily serve the long windows
// from the continuous aggregates.
type Source string

const (
	SourceRaw    Source = "raw"
	SourceHourly Source = "hourly"
	SourceDaily  Source = "daily"
)

// table maps a Source to its relation. The whitelist is the injection
// boundary: query text interpolates only these constants, never input.
func (s Source) table() string {
	switch s {
	case SourceHourly:
		return "probe_results_hourly"
	case SourceDaily:
		return "probe_results_daily"
	default:
		return ""
	}
}

// SiteInfo is a sites row as shown by the dashboard. Latitude/Longitude are
// nil until an operator places the site on the map (`site set`); the DB
// enforces both-or-neither.
type SiteInfo struct {
	ID          uuid.UUID
	Name        string
	DisplayName string
	Location    string
	Latitude    *float64
	Longitude   *float64
}

// AgentListInfo is an agents row joined with its site name plus the health
// signals the dashboard's Agents view shows: newest certificate, open
// outages, active series, and reported spool loss.
type AgentListInfo struct {
	ID           uuid.UUID
	Site         string
	Network      string
	Hostname     string
	ProbeAddress string
	Version      string
	LastSeenAt   *time.Time
	CreatedAt    time.Time
	ConfigHash   string
	// Newest certificate by issuance; nil only for an agent with no cert
	// row (never happens through real enrollment).
	CertNotAfter  *time.Time
	CertRevokedAt *time.Time
	Offline       bool   // an agent_offline outage is currently open
	ProbesFailing int64  // open probe_failing outages
	Health        string // query mode only: offline|degraded|healthy|no_data
	// Series this agent has reported results for whose probe is still
	// enabled. Disabling a probe keeps its series_state row (spool-replay
	// dedup) but removes it from this count immediately; re-enabling
	// restores it without waiting for a fresh result.
	ProbesTotal    int64
	DroppedResults int64
	LastDroppedAt  *time.Time
}

// AgentHealthBucket is one time bucket of one agent's probe success ratio.
type AgentHealthBucket struct {
	AgentID uuid.UUID
	Bucket  time.Time
	Samples int64
	OK      int64
}

// AgentProbeHealthRow is one (probe series, bucket) row of one agent's
// per-probe health detail. Label columns repeat on every bucket row of a
// series; Bucket/Samples/OK are nil for a series with no samples in the
// window (the LEFT JOIN's single row), so configured-but-silent series
// still appear. TargetID/TargetKind/TargetName are nil when the target row
// is gone (series_state carries no FK to targets); DstSite is nil for
// external targets.
type AgentProbeHealthRow struct {
	ProbeID    uuid.UUID
	ProbeType  int16
	TargetID   *uuid.UUID
	TargetKind *string
	TargetName *string
	DstSite    *string
	LastStatus int16
	LastTime   time.Time
	// OpenedAt is the open probe_failing outage; nil = not failing. An open
	// probe_degraded event is deliberately excluded — the drill-down must
	// agree with ListAgents' failing count, which only counts hard failures.
	OpenedAt  *time.Time
	OpenError *string
	Bucket    *time.Time
	Samples   *int64
	OK        *int64
}

// AgentBucketFailureGroup is one (probe series, status) failure group inside
// a single health-strip bucket, with a representative most-recent error.
// TargetKind/TargetName are nil when the target row is gone; DstSite is nil
// for external targets. LastError is the newest non-NULL error in the group
// (≤128 chars, truncated at ingest); nil when every row lacked one.
type AgentBucketFailureGroup struct {
	ProbeID    uuid.UUID
	ProbeType  int16
	TargetKind *string
	TargetName *string
	DstSite    *string
	Status     int16
	Count      int64
	LastError  *string
	LastTime   time.Time
}

// MatrixRow is the latest result of one (agent, agent-target, probe type)
// series, mapped to its ordered site pair. TargetID is set only by
// DirectionLatest — the pair page's check chips link to the target detail
// page — and stays nil from MatrixLatest, which folds to site pairs.
// Network is the source agent's network name, set only by MatrixLatest
// (mesh expansion pairs same-network agents, so it is the series' plane);
// DirectionLatest leaves it empty — the pair page filters by endpoint IDs.
type MatrixRow struct {
	SrcSite       string
	DstSite       string
	Network       string
	TargetID      *uuid.UUID
	ProbeType     int16
	Status        int16
	Time          time.Time
	LatencyUS     *int64
	LatencySource string
	LossPct       *float32
}

// SitePair is an ordered src→dst site pair that probe configuration says
// should be producing results.
type SitePair struct {
	Src string
	Dst string
}

// NetworkPair is a SitePair on one connectivity plane. Projecting the set
// to (Src, Dst) yields exactly the pre-networks expected-pair set.
type NetworkPair struct {
	Src     string
	Dst     string
	Network string
}

// SiteEndpoints are the probe-series endpoints belonging to one site: its
// agents (result senders) and those agents' targets (result destinations).
// Networks holds each agent's network name, parallel to AgentIDs and
// TargetIDs, so callers can filter all three in lockstep by plane.
type SiteEndpoints struct {
	SiteInfo
	AgentIDs  []uuid.UUID
	TargetIDs []uuid.UUID
	Networks  []string
}

// SeriesBucket is one time_bucket of a directional pair series. The
// percentile fields are populated only from aggregate sources; raw windows
// leave them nil.
type SeriesBucket struct {
	Bucket   time.Time
	MinUS    *float64
	AvgUS    *float64
	MaxUS    *float64
	LossPct  *float64
	Samples  int64
	Failures int64
	P50US    *float64
	P95US    *float64
	P99US    *float64
}

// PairSummaryRow aggregates one direction of a site pair over a window.
// Percentiles are nil from the raw source; jitter/tcp/tls averages come
// from every source.
type PairSummaryRow struct {
	MinUS             *float64
	AvgUS             *float64
	MaxUS             *float64
	LossPct           *float64
	Samples           int64
	LastOKAt          *time.Time
	LatencySource     string
	P50US             *float64
	P95US             *float64
	P99US             *float64
	JitterAvgUS       *float64
	TCPConnectAvgUS   *float64
	TLSHandshakeAvgUS *float64
}

// siteScopePredicate builds the scoped-site visibility rule shared by
// ListSites, ListSitesConfig, SiteEndpoints, and ListPathThresholds, as a
// SQL fragment over siteCol (a trusted identifier or placeholder, never
// input) and param (the uuid[] scope placeholder). With a nil scope every
// site is visible. With a scope, a site is visible when it participates in
// the tenant's probing surface: it hosts an agent on an allowed plane, is
// a member of a mesh on one, is the source site of an in-scope direct
// probe, or hosts the destination agent of one. The last three matter for
// UNSTAFFED sites: ExpectedPairs deliberately keeps a configured pair
// whose member site has no agents yet (it renders as stale), so the
// scoped matrix would otherwise reference sites absent from the scoped
// site list and pair detail would 404. Tenants never learn a foreign
// plane's site names — a shared site (co-located agents on several
// planes) stays visible, which is accepted: site names are shared
// operator vocabulary.
func siteScopePredicate(siteCol, param string) string {
	return fmt.Sprintf(`(%[2]s::uuid[] IS NULL
		OR EXISTS (SELECT 1 FROM agents sca WHERE sca.site_id = %[1]s AND sca.network_id = ANY(%[2]s))
		OR EXISTS (SELECT 1 FROM mesh_members scm JOIN mesh_groups scg ON scg.id = scm.mesh_id
		            WHERE scm.site_id = %[1]s AND scg.network_id = ANY(%[2]s))
		OR EXISTS (SELECT 1 FROM probe_configs scp
		            WHERE scp.site_id = %[1]s AND scp.network_id = ANY(%[2]s))
		OR EXISTS (SELECT 1 FROM probe_configs scd
		            JOIN targets sct ON sct.id = scd.target_id
		            JOIN agents scda ON scda.id = sct.agent_id
		            WHERE scda.site_id = %[1]s AND scd.network_id = ANY(%[2]s)))`, siteCol, param)
}

// ListSites returns all sites ordered by name. networks is the caller's
// network scope (nil = unfiltered, the convention every scoped read here
// follows); visibility follows siteScopePredicate.
func (s *Store) ListSites(ctx context.Context, networks []uuid.UUID) ([]SiteInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, display_name, location, latitude, longitude FROM sites
		  WHERE `+siteScopePredicate("sites.id", "$1")+`
		  ORDER BY name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()
	var out []SiteInfo
	for rows.Next() {
		var si SiteInfo
		if err := rows.Scan(&si.ID, &si.Name, &si.DisplayName, &si.Location, &si.Latitude, &si.Longitude); err != nil {
			return nil, fmt.Errorf("list sites: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// enabledProbeIDs is the ID set of every probe series agents are currently
// expected to run: enabled direct configs by row ID, plus enabled mesh
// templates expanded over source sites × destination agent targets — the
// same derivation meshexpand ships to agents, so the set matches their
// working config exactly.
func (s *Store) enabledProbeIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, mesh_id FROM probe_configs WHERE enabled`)
	if err != nil {
		return nil, fmt.Errorf("enabled probes: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	meshTemplates := make(map[uuid.UUID][]uuid.UUID)
	for rows.Next() {
		var id uuid.UUID
		var meshID *uuid.UUID
		if err := rows.Scan(&id, &meshID); err != nil {
			return nil, fmt.Errorf("enabled probes: %w", err)
		}
		if meshID == nil {
			ids = append(ids, id)
		} else {
			meshTemplates[*meshID] = append(meshTemplates[*meshID], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enabled probes: %w", err)
	}
	if len(meshTemplates) == 0 {
		return ids, nil
	}
	mrows, err := s.pool.Query(ctx, `SELECT mesh_id, site_id FROM mesh_members`)
	if err != nil {
		return nil, fmt.Errorf("mesh members: %w", err)
	}
	defer mrows.Close()
	members := make(map[uuid.UUID][]uuid.UUID)
	for mrows.Next() {
		var meshID, siteID uuid.UUID
		if err := mrows.Scan(&meshID, &siteID); err != nil {
			return nil, fmt.Errorf("mesh members: %w", err)
		}
		members[meshID] = append(members[meshID], siteID)
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("mesh members: %w", err)
	}
	// Each mesh expands only over agents on its own network, matching
	// LoadAgentConfigInputs — a broader set here would leave orphaned events
	// the sweep can never close, a narrower one would close live events.
	nrows, err := s.pool.Query(ctx, `SELECT id, network_id FROM mesh_groups`)
	if err != nil {
		return nil, fmt.Errorf("mesh networks: %w", err)
	}
	defer nrows.Close()
	meshNet := make(map[uuid.UUID]uuid.UUID)
	for nrows.Next() {
		var meshID, networkID uuid.UUID
		if err := nrows.Scan(&meshID, &networkID); err != nil {
			return nil, fmt.Errorf("mesh networks: %w", err)
		}
		meshNet[meshID] = networkID
	}
	if err := nrows.Err(); err != nil {
		return nil, fmt.Errorf("mesh networks: %w", err)
	}
	// Network → site → agent-kind target IDs, mesh-independent (a site's
	// on-network agents are the same in every mesh on that network);
	// expansion looks up only member sites.
	trows, err := s.pool.Query(ctx, `
		SELECT a.network_id, a.site_id, t.id FROM agents a JOIN targets t ON t.agent_id = a.id`)
	if err != nil {
		return nil, fmt.Errorf("agent targets: %w", err)
	}
	defer trows.Close()
	siteTargetsByNet := make(map[uuid.UUID]map[uuid.UUID][]uuid.UUID)
	for trows.Next() {
		var networkID, siteID, targetID uuid.UUID
		if err := trows.Scan(&networkID, &siteID, &targetID); err != nil {
			return nil, fmt.Errorf("agent targets: %w", err)
		}
		st := siteTargetsByNet[networkID]
		if st == nil {
			st = make(map[uuid.UUID][]uuid.UUID)
			siteTargetsByNet[networkID] = st
		}
		st[siteID] = append(st[siteID], targetID)
	}
	if err := trows.Err(); err != nil {
		return nil, fmt.Errorf("agent targets: %w", err)
	}
	// Dedupe defensively — IDs are unique by construction since the
	// derivation includes template row and destination target, but ListAgents
	// joins this set against series rows, where a repeated ID would
	// double-count.
	seen := make(map[uuid.UUID]bool, len(ids))
	unique := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	for meshID, templates := range meshTemplates {
		for _, id := range expandMeshProbeIDs(templates, members[meshID], siteTargetsByNet[meshNet[meshID]]) {
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}
	}
	return unique, nil
}

// EnabledProbeIDs exposes the enabled-probe ID set for callers outside the
// store — the outage sweep uses it to close orphaned probe_failing events.
func (s *Store) EnabledProbeIDs(ctx context.Context) ([]uuid.UUID, error) {
	return s.enabledProbeIDs(ctx)
}

// ListAgents returns all agents with their site names and health signals.
// The open-outage fold hits the partial index on closed_at IS NULL;
// series_state is one small row per series, so the aggregate is cheap.
// The enabled set is joined via unnest, not `= ANY($1)`: a parameterized
// array can't be hashed, so ANY would test every element per row —
// quadratic in mesh size on an endpoint every session polls — while the
// unnest join hashes to O(rows + set).
// Both probe counts are intersected with enabledProbeIDs so they track
// the agent's active probe surface: a disabled probe's retained
// series_state row must not keep counting toward ProbesTotal, and a
// probe_failing event re-opened by straggler results ingested during the
// assignment cache's staleness window (nothing ever closes it — the
// disabled probe produces no successes) must not keep the agent degraded
// or make ProbesFailing exceed ProbesTotal. The outage sweep closes such
// stuck events within a tick; this rollup filters them in the meantime.
// networks is the caller's network scope (nil = unfiltered).
func (s *Store) ListAgents(ctx context.Context, networks []uuid.UUID) ([]AgentListInfo, error) {
	enabled, err := s.enabledProbeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, s.name, n.name, a.hostname, a.probe_address, a.version, a.last_seen_at,
		        a.created_at, a.current_config_hash, a.dropped_results, a.last_dropped_at,
		        c.not_after, c.revoked_at,
		        COALESCE(o.offline, false), COALESCE(o.failing, 0),
		        COALESCE(ss.total, 0)
		   FROM agents a
		   JOIN sites s ON s.id = a.site_id
		   JOIN networks n ON n.id = a.network_id
		   LEFT JOIN LATERAL (
		        SELECT not_after, revoked_at FROM certificates
		         WHERE agent_id = a.id ORDER BY created_at DESC LIMIT 1
		   ) c ON true
		   LEFT JOIN (
		        SELECT oe.agent_id,
		               bool_or(oe.kind = 'agent_offline') AS offline,
		               count(ep.probe_id) AS failing
		          FROM outage_events oe
		          LEFT JOIN unnest($1::uuid[]) AS ep(probe_id)
		            ON oe.kind = 'probe_failing' AND oe.probe_id = ep.probe_id
		         WHERE oe.closed_at IS NULL
		         GROUP BY oe.agent_id
		   ) o ON o.agent_id = a.id
		   LEFT JOIN (
		        SELECT ss.agent_id, count(*) AS total
		          FROM series_state ss
		          JOIN unnest($1::uuid[]) AS ep(probe_id) ON ep.probe_id = ss.probe_id
		         GROUP BY ss.agent_id
		   ) ss ON ss.agent_id = a.id
		  WHERE $2::uuid[] IS NULL OR a.network_id = ANY($2)
		  ORDER BY s.name, a.hostname`, enabled, networks)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []AgentListInfo
	for rows.Next() {
		var ai AgentListInfo
		if err := rows.Scan(&ai.ID, &ai.Site, &ai.Network, &ai.Hostname, &ai.ProbeAddress, &ai.Version, &ai.LastSeenAt,
			&ai.CreatedAt, &ai.ConfigHash, &ai.DroppedResults, &ai.LastDroppedAt,
			&ai.CertNotAfter, &ai.CertRevokedAt,
			&ai.Offline, &ai.ProbesFailing, &ai.ProbesTotal); err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		out = append(out, ai)
	}
	return out, rows.Err()
}

// AgentHealthSeries buckets every agent's probe results over the window into
// total/successful counts, ordered by agent then bucket. Rows of
// excludeProbeType are skipped: the caller passes traceroute, whose
// run-accounting rows (sent=1, all timings NULL) count as samples and would
// poison a success ratio (store deliberately does not import pb, so the
// constant travels as a parameter). status <> 1 — including UNSUPPORTED — is
// a failure. Reads the probe_results_health_30m cagg (migration 0009):
// bucket must be a multiple of 30 min and windows must stay inside the
// cagg's retention (14 d); materialized_only = false serves the un-refreshed
// tail live from raw, so the current half hour is never stale.
// networks is the caller's network scope (nil = unfiltered); the cagg has
// no plane column, so scope filters through the agents table.
func (s *Store) AgentHealthSeries(ctx context.Context, window, bucket time.Duration, excludeProbeType int16, networks []uuid.UUID) ([]AgentHealthBucket, error) {
	// Alias b, not bucket: GROUP BY resolves unqualified names against input
	// columns first, so "AS bucket ... GROUP BY bucket" would group by the
	// cagg's 30-min column and skip the re-bucket. sum(bigint) is numeric;
	// cast back for the int64 scans (same pattern as PairSeries).
	rows, err := s.pool.Query(ctx,
		`SELECT agent_id, time_bucket($1::interval, bucket) AS b,
		        sum(samples)::bigint, sum(ok_samples)::bigint
		   FROM probe_results_health_30m
		  WHERE bucket > now() - $2::interval
		    AND probe_type <> $3
		    AND ($4::uuid[] IS NULL
		         OR agent_id IN (SELECT id FROM agents WHERE network_id = ANY($4)))
		  GROUP BY agent_id, b
		  ORDER BY agent_id, b`, bucket, window, excludeProbeType, networks)
	if err != nil {
		return nil, fmt.Errorf("agent health series: %w", err)
	}
	defer rows.Close()
	var out []AgentHealthBucket
	for rows.Next() {
		var b AgentHealthBucket
		if err := rows.Scan(&b.AgentID, &b.Bucket, &b.Samples, &b.OK); err != nil {
			return nil, fmt.Errorf("agent health series: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AgentProbeHealth returns one agent's probe series with their bucketed
// success counts over the window, ordered by probe then bucket (the handler's
// run-length fold relies on that order). The inventory side is series_state
// intersected with enabledProbeIDs — exactly the rows ListAgents counts as
// probes_total, so the expanded detail always agrees with the row's counts:
// a disabled probe's retained series row (spool-replay dedup) stays out, as
// does the stuck probe_failing event the rollup filters for the same reason.
// A configured-and-enabled series with no results in the window still yields
// one nil-bucket row instead of vanishing. Traceroute series are included
// as-is: nothing here aggregates across probes, so their run-accounting rows
// can't poison a ratio the way they would in AgentHealthSeries. The bucket
// counts come from the probe_results_health_30m cagg (migration 0009): bucket
// must be a multiple of 30 min and windows must stay inside the cagg's
// retention (14 d); materialized_only = false keeps the current half hour
// live. The subquery's GROUP BY repeats the time_bucket expression instead of
// naming the alias — the alias must stay "bucket" for the outer ORDER BY, but
// an unqualified GROUP BY bucket would resolve to the cagg's input column.
// networks is the caller's network scope (nil = unfiltered): an
// out-of-scope agent yields no rows, indistinguishable from an unknown
// agent id (which is already an empty list, not an error).
func (s *Store) AgentProbeHealth(ctx context.Context, agentID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]AgentProbeHealthRow, error) {
	enabled, err := s.enabledProbeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent probe health: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT ss.probe_id, ss.probe_type, t.id, t.kind, t.name, dst.name,
		        ss.last_status, ss.last_time, oe.opened_at, oe.open_error,
		        b.bucket, b.samples, b.ok
		   FROM series_state ss
		   JOIN unnest($4::uuid[]) AS ep(probe_id) ON ep.probe_id = ss.probe_id
		   LEFT JOIN targets t ON t.id = ss.target_id
		   LEFT JOIN agents ta ON ta.id = t.agent_id
		   LEFT JOIN sites dst ON dst.id = ta.site_id
		   LEFT JOIN outage_events oe ON oe.id = ss.open_event_id AND oe.kind = 'probe_failing'
		   LEFT JOIN (
		        SELECT probe_id, time_bucket($2::interval, bucket) AS bucket,
		               sum(samples)::bigint AS samples, sum(ok_samples)::bigint AS ok
		          FROM probe_results_health_30m
		         WHERE agent_id = $1 AND bucket > now() - $3::interval
		         GROUP BY probe_id, time_bucket($2::interval, bucket)
		   ) b ON b.probe_id = ss.probe_id
		  WHERE ss.agent_id = $1
		    AND ($5::uuid[] IS NULL
		         OR EXISTS (SELECT 1 FROM agents ag WHERE ag.id = $1 AND ag.network_id = ANY($5)))
		  ORDER BY ss.probe_id, b.bucket`, agentID, bucket, window, enabled, networks)
	if err != nil {
		return nil, fmt.Errorf("agent probe health: %w", err)
	}
	defer rows.Close()
	var out []AgentProbeHealthRow
	for rows.Next() {
		var r AgentProbeHealthRow
		if err := rows.Scan(&r.ProbeID, &r.ProbeType, &r.TargetID, &r.TargetKind, &r.TargetName, &r.DstSite,
			&r.LastStatus, &r.LastTime, &r.OpenedAt, &r.OpenError,
			&r.Bucket, &r.Samples, &r.OK); err != nil {
			return nil, fmt.Errorf("agent probe health: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AgentBucketFailures returns the failure groups behind one health-strip
// bucket: every non-OK result (status <> 1, UNSUPPORTED included — same
// failure definition as AgentHealthSeries) the agent recorded in
// [bucketStart, bucketStart+bucket), grouped by probe series and status.
// Label joins go through pr.target_id — unlike AgentProbeHealth's
// series_state route — because only rows that actually exist are reported;
// a deleted target degrades to NULL kind/name exactly as there. probeID nil
// serves the fleet strip and excludes excludeProbeType so counts reconcile
// with AgentHealthSeries (the caller passes traceroute; store deliberately
// does not import pb). probeID non-nil serves a per-probe strip: it filters
// to that probe and ignores the exclusion, reconciling with
// AgentProbeHealth, which includes traceroute. The FILTER on array_agg
// keeps a NULL-error newest row from hiding an older real message. Bucket
// starts must sit inside raw retention (14 d); this reads probe_results
// directly.
// networks is the caller's network scope (nil = unfiltered); out-of-scope
// agents yield no rows, matching AgentProbeHealth.
func (s *Store) AgentBucketFailures(ctx context.Context, agentID uuid.UUID,
	bucketStart time.Time, bucket time.Duration,
	probeID *uuid.UUID, excludeProbeType int16, networks []uuid.UUID) ([]AgentBucketFailureGroup, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT pr.probe_id, pr.probe_type, t.kind, t.name, dst.name, pr.status,
		        count(*),
		        (array_agg(pr.error ORDER BY pr.time DESC)
		           FILTER (WHERE pr.error IS NOT NULL))[1],
		        max(pr.time)
		   FROM probe_results pr
		   LEFT JOIN targets t  ON t.id  = pr.target_id
		   LEFT JOIN agents ta  ON ta.id = t.agent_id
		   LEFT JOIN sites  dst ON dst.id = ta.site_id
		  WHERE pr.agent_id = $1
		    AND pr.time >= $2 AND pr.time < $2 + $3::interval
		    AND pr.status <> 1
		    AND ($4::uuid IS NULL OR pr.probe_id = $4)
		    AND ($4::uuid IS NOT NULL OR pr.probe_type <> $5)
		    AND ($6::uuid[] IS NULL
		         OR EXISTS (SELECT 1 FROM agents ag WHERE ag.id = $1 AND ag.network_id = ANY($6)))
		  GROUP BY pr.probe_id, pr.probe_type, t.kind, t.name, dst.name, pr.status
		  ORDER BY count(*) DESC, pr.probe_id, pr.status`,
		agentID, bucketStart, bucket, probeID, excludeProbeType, networks)
	if err != nil {
		return nil, fmt.Errorf("agent bucket failures: %w", err)
	}
	defer rows.Close()
	var out []AgentBucketFailureGroup
	for rows.Next() {
		var g AgentBucketFailureGroup
		if err := rows.Scan(&g.ProbeID, &g.ProbeType, &g.TargetKind, &g.TargetName, &g.DstSite,
			&g.Status, &g.Count, &g.LastError, &g.LastTime); err != nil {
			return nil, fmt.Errorf("agent bucket failures: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MatrixLatest returns the latest result per (agent, agent-target, probe
// type) series within the staleness horizon, mapped to ordered site pairs.
// External targets have no destination site and are excluded by the join.
// networks is the caller's network scope (nil = unfiltered), applied to the
// SOURCE agent's plane — the series' plane, since expansion pairs
// same-network agents.
func (s *Store) MatrixLatest(ctx context.Context, horizon time.Duration, networks []uuid.UUID) ([]MatrixRow, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`WITH latest AS (
		    SELECT DISTINCT ON (agent_id, target_id, probe_type)
		           agent_id, target_id, probe_type, time, status, loss_pct,
		           %s AS latency_us, %s AS latency_source
		      FROM probe_results
		     WHERE time > now() - $1::interval
		     ORDER BY agent_id, target_id, probe_type, time DESC
		)
		SELECT ss.name, ds.name, nw.name, l.probe_type, l.status, l.time,
		       l.latency_us, l.latency_source, l.loss_pct
		  FROM latest l
		  JOIN agents sa ON sa.id = l.agent_id
		  JOIN sites  ss ON ss.id = sa.site_id
		  JOIN networks nw ON nw.id = sa.network_id
		  JOIN targets t ON t.id = l.target_id AND t.agent_id IS NOT NULL
		  JOIN agents da ON da.id = t.agent_id
		  JOIN sites  ds ON ds.id = da.site_id
		 WHERE $2::uuid[] IS NULL OR sa.network_id = ANY($2)`, latencyExpr, latencySourceExpr),
		horizon, networks)
	if err != nil {
		return nil, fmt.Errorf("matrix latest: %w", err)
	}
	defer rows.Close()
	var out []MatrixRow
	for rows.Next() {
		var mr MatrixRow
		if err := rows.Scan(&mr.SrcSite, &mr.DstSite, &mr.Network, &mr.ProbeType, &mr.Status, &mr.Time,
			&mr.LatencyUS, &mr.LatencySource, &mr.LossPct); err != nil {
			return nil, fmt.Errorf("matrix latest: %w", err)
		}
		out = append(out, mr)
	}
	return out, rows.Err()
}

// ExpectedPairs returns the ordered site pairs, per network, that enabled
// probe configs should be producing results for — mesh templates expanded
// over member pairs plus direct probes whose target is an agent. Pairs
// present here but absent from MatrixLatest render as stale (per plane).
//
// Network scoping is deliberately conservative: a member site only drops
// out of a pair when it has agents but none on the probe's network (real
// cross-plane unreachability). A member site with no agents at all keeps
// its pairs — rendering stale for an unstaffed member is today's behavior
// and stays, so the matrix is bit-identical across the networks upgrade.
//
// networks is the caller's network scope (nil = unfiltered), applied to the
// pair's plane.
func (s *Store) ExpectedPairs(ctx context.Context, networks []uuid.UUID) ([]NetworkPair, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT s1.name, s2.name, n.name
		   FROM probe_configs pc
		   JOIN mesh_groups mg ON mg.id = pc.mesh_id
		   JOIN networks n ON n.id = mg.network_id
		   JOIN mesh_members m1 ON m1.mesh_id = pc.mesh_id
		   JOIN mesh_members m2 ON m2.mesh_id = pc.mesh_id AND m2.site_id <> m1.site_id
		   JOIN sites s1 ON s1.id = m1.site_id
		   JOIN sites s2 ON s2.id = m2.site_id
		  WHERE pc.enabled
		    AND ($1::uuid[] IS NULL OR mg.network_id = ANY($1))
		    AND (EXISTS (SELECT 1 FROM agents a WHERE a.site_id = m1.site_id AND a.network_id = mg.network_id)
		         OR NOT EXISTS (SELECT 1 FROM agents a WHERE a.site_id = m1.site_id))
		    AND (EXISTS (SELECT 1 FROM agents a WHERE a.site_id = m2.site_id AND a.network_id = mg.network_id)
		         OR NOT EXISTS (SELECT 1 FROM agents a WHERE a.site_id = m2.site_id))
		 UNION
		 SELECT DISTINCT s1.name, s2.name, n.name
		   FROM probe_configs pc
		   JOIN networks n ON n.id = pc.network_id
		   JOIN sites s1 ON s1.id = pc.site_id
		   JOIN targets t ON t.id = pc.target_id AND t.agent_id IS NOT NULL
		   JOIN agents da ON da.id = t.agent_id
		   JOIN sites s2 ON s2.id = da.site_id
		  WHERE pc.enabled AND s1.id <> s2.id
		    AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1))
		    AND (EXISTS (SELECT 1 FROM agents a WHERE a.site_id = pc.site_id AND a.network_id = pc.network_id)
		         OR NOT EXISTS (SELECT 1 FROM agents a WHERE a.site_id = pc.site_id))`, networks)
	if err != nil {
		return nil, fmt.Errorf("expected pairs: %w", err)
	}
	defer rows.Close()
	var out []NetworkPair
	for rows.Next() {
		var p NetworkPair
		if err := rows.Scan(&p.Src, &p.Dst, &p.Network); err != nil {
			return nil, fmt.Errorf("expected pairs: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SiteEndpoints resolves a site name to its agents and their agent-kind
// targets, or (nil, nil) when the site does not exist. Resolving IDs first
// lets the hypertable scans hit the (agent_id, target_id, ...) index with
// no joins. networks is the caller's network scope (nil = unfiltered): a
// scoped caller sees only its planes' endpoint IDs, and a site hosting no
// in-scope agents resolves to (nil, nil) — byte-identical to an unknown
// site, so a tenant cannot probe for other tenants' site names.
func (s *Store) SiteEndpoints(ctx context.Context, siteName string, networks []uuid.UUID) (*SiteEndpoints, error) {
	var ep SiteEndpoints
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, display_name, location, latitude, longitude FROM sites WHERE name = $1`, siteName).
		Scan(&ep.ID, &ep.Name, &ep.DisplayName, &ep.Location, &ep.Latitude, &ep.Longitude)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("site %q: %w", siteName, err)
	}
	if networks != nil {
		// Visibility follows siteScopePredicate (the ListSites rule),
		// checked on its own rather than against the endpoint rows below
		// so an in-scope agent with no targets yet — or an unstaffed
		// mesh-member site whose expected pairs render as stale — still
		// resolves for its tenant.
		var visible bool
		err := s.pool.QueryRow(ctx,
			`SELECT `+siteScopePredicate("$1::uuid", "$2"), ep.ID, networks).Scan(&visible)
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", siteName, err)
		}
		if !visible {
			return nil, nil
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, t.id, n.name
		   FROM agents a
		   JOIN targets t ON t.agent_id = a.id
		   JOIN networks n ON n.id = a.network_id
		  WHERE a.site_id = $1
		    AND ($2::uuid[] IS NULL OR a.network_id = ANY($2))`, ep.ID, networks)
	if err != nil {
		return nil, fmt.Errorf("site %q endpoints: %w", siteName, err)
	}
	defer rows.Close()
	for rows.Next() {
		var agentID, targetID uuid.UUID
		var network string
		if err := rows.Scan(&agentID, &targetID, &network); err != nil {
			return nil, fmt.Errorf("site %q endpoints: %w", siteName, err)
		}
		ep.AgentIDs = append(ep.AgentIDs, agentID)
		ep.TargetIDs = append(ep.TargetIDs, targetID)
		ep.Networks = append(ep.Networks, network)
	}
	return &ep, rows.Err()
}

// PairSeries buckets one direction (srcAgents → dstTargets) over the window,
// reading raw or a continuous aggregate per source. Aggregate sources add
// p50/p95/p99 from the rolled-up UddSketch; raw leaves them nil.
func (s *Store) PairSeries(ctx context.Context, srcAgents, dstTargets []uuid.UUID, bucket, window time.Duration, source Source, latencySource string) ([]SeriesBucket, error) {
	var sql string
	if table := source.table(); table == "" {
		sql = fmt.Sprintf(
			`SELECT time_bucket($1::interval, time) AS bucket,
			        min(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $5)::float8,
			        avg(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $5)::float8,
			        max(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $5)::float8,
			        100.0 * (1 - sum(received)::float8 / NULLIF(sum(sent), 0)),
			        count(*),
			        count(*) FILTER (WHERE status <> 1),
			        NULL::float8, NULL::float8, NULL::float8
			   FROM probe_results
			  WHERE agent_id = ANY($2) AND target_id = ANY($3)
			    AND time > now() - $4::interval
			  GROUP BY bucket ORDER BY bucket`, latencyExpr, latencySourceExpr)
	} else {
		// time_bucket over the cagg's bucket column regroups hourly rows
		// into wider chart buckets (identity when widths already match).
		// Averages come from the materialized sums/counts.
		sql = fmt.Sprintf(
			`SELECT time_bucket($1::interval, bucket) AS b,
			        min(lat_min_us) FILTER (WHERE latency_source = $5)::float8,
			        sum(lat_sum_us) FILTER (WHERE latency_source = $5)::float8
			            / NULLIF(sum(lat_count) FILTER (WHERE latency_source = $5), 0)::float8,
			        max(lat_max_us) FILTER (WHERE latency_source = $5)::float8,
			        100.0 * (1 - sum(received)::float8 / NULLIF(sum(sent), 0)::float8),
			        sum(samples)::bigint,
			        (sum(samples) - sum(ok_samples))::bigint,
			        approx_percentile(0.50, rollup(lat_pctl) FILTER (WHERE latency_source = $5)),
			        approx_percentile(0.95, rollup(lat_pctl) FILTER (WHERE latency_source = $5)),
			        approx_percentile(0.99, rollup(lat_pctl) FILTER (WHERE latency_source = $5))
			   FROM %s
			  WHERE agent_id = ANY($2) AND target_id = ANY($3)
			    AND bucket > now() - $4::interval
			  GROUP BY b ORDER BY b`, table)
	}
	rows, err := s.pool.Query(ctx, sql, bucket, srcAgents, dstTargets, window, latencySource)
	if err != nil {
		return nil, fmt.Errorf("pair series: %w", err)
	}
	defer rows.Close()
	var out []SeriesBucket
	for rows.Next() {
		var b SeriesBucket
		if err := rows.Scan(&b.Bucket, &b.MinUS, &b.AvgUS, &b.MaxUS, &b.LossPct, &b.Samples, &b.Failures,
			&b.P50US, &b.P95US, &b.P99US); err != nil {
			return nil, fmt.Errorf("pair series: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PairSummary aggregates one direction (srcAgents → dstTargets) over the
// window, reading raw or a continuous aggregate and selecting one successful
// timing family so charts and summaries describe the same measurement.
func (s *Store) PairSummary(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source Source) (*PairSummaryRow, error) {
	latencySource, err := s.PairLatencySource(ctx, srcAgents, dstTargets, window, source)
	if err != nil {
		return nil, err
	}
	var sql string
	if table := source.table(); table == "" {
		sql = fmt.Sprintf(
			`SELECT min(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $4)::float8,
			        avg(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $4)::float8,
			        max(%[1]s) FILTER (WHERE status = 1 AND %[2]s = $4)::float8,
			        100.0 * (1 - sum(received)::float8 / NULLIF(sum(sent), 0)),
			        count(*),
			        max(time) FILTER (WHERE status = 1),
			        NULL::float8, NULL::float8, NULL::float8,
			        avg(jitter_us) FILTER (WHERE status = 1)::float8,
			        avg(tcp_connect_us) FILTER (WHERE status = 1)::float8,
			        avg(tls_handshake_us) FILTER (WHERE status = 1)::float8
			   FROM probe_results
			  WHERE agent_id = ANY($1) AND target_id = ANY($2)
			    AND time > now() - $3::interval`, latencyExpr, latencySourceExpr)
	} else {
		sql = fmt.Sprintf(
			`SELECT min(lat_min_us) FILTER (WHERE latency_source = $4)::float8,
			        sum(lat_sum_us) FILTER (WHERE latency_source = $4)::float8
			            / NULLIF(sum(lat_count) FILTER (WHERE latency_source = $4), 0)::float8,
			        max(lat_max_us) FILTER (WHERE latency_source = $4)::float8,
			        100.0 * (1 - sum(received)::float8 / NULLIF(sum(sent), 0)::float8),
			        coalesce(sum(samples), 0)::bigint,
			        max(last_ok_at),
			        approx_percentile(0.50, rollup(lat_pctl) FILTER (WHERE latency_source = $4)),
			        approx_percentile(0.95, rollup(lat_pctl) FILTER (WHERE latency_source = $4)),
			        approx_percentile(0.99, rollup(lat_pctl) FILTER (WHERE latency_source = $4)),
			        sum(jitter_sum_us)::float8 / NULLIF(sum(jitter_count), 0)::float8,
			        sum(tcp_sum_us)::float8 / NULLIF(sum(tcp_count), 0)::float8,
			        sum(tls_sum_us)::float8 / NULLIF(sum(tls_count), 0)::float8
			   FROM %s
			  WHERE agent_id = ANY($1) AND target_id = ANY($2)
			    AND bucket > now() - $3::interval`, table)
	}
	var p PairSummaryRow
	err = s.pool.QueryRow(ctx, sql, srcAgents, dstTargets, window, latencySource).
		Scan(&p.MinUS, &p.AvgUS, &p.MaxUS, &p.LossPct, &p.Samples, &p.LastOKAt,
			&p.P50US, &p.P95US, &p.P99US,
			&p.JitterAvgUS, &p.TCPConnectAvgUS, &p.TLSHandshakeAvgUS)
	if err != nil {
		return nil, fmt.Errorf("pair summary: %w", err)
	}
	p.LatencySource = latencySource
	return &p, nil
}

// PairLatencySource chooses one successful timing family for a direction
// and window (see chooseLatencySource for the priority + coverage rule).
// Aggregate windows inspect their serving cagg, so source selection still
// works when the matching raw rows have aged out.
func (s *Store) PairLatencySource(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source Source) (string, error) {
	var sql string
	if table := source.table(); table == "" {
		sql = fmt.Sprintf(
			`SELECT %s AS latency_source, count(*)
			   FROM probe_results
			  WHERE agent_id = ANY($1) AND target_id = ANY($2)
			    AND time > now() - $3::interval
			    AND status = 1
			    AND %s IS NOT NULL
			  GROUP BY latency_source`, latencySourceExpr, latencyExpr)
	} else {
		sql = fmt.Sprintf(
			`SELECT latency_source, sum(lat_count)::bigint
			   FROM %s
			  WHERE agent_id = ANY($1) AND target_id = ANY($2)
			    AND bucket > now() - $3::interval
			    AND latency_source <> '' AND lat_count > 0
			  GROUP BY latency_source`, table)
	}
	rows, err := s.pool.Query(ctx, sql, srcAgents, dstTargets, window)
	if err != nil {
		return "", fmt.Errorf("pair latency source: %w", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var family string
		var n int64
		if err := rows.Scan(&family, &n); err != nil {
			return "", fmt.Errorf("pair latency source: %w", err)
		}
		counts[family] = n
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("pair latency source: %w", err)
	}
	return chooseLatencySource(counts), nil
}

// DirectionLatest returns the latest row per (agent, target, probe type)
// series for one direction within the horizon — the same shape the matrix
// uses, so cell and pair-detail status agree.
func (s *Store) DirectionLatest(ctx context.Context, srcAgents, dstTargets []uuid.UUID, horizon time.Duration) ([]MatrixRow, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`SELECT DISTINCT ON (agent_id, target_id, probe_type)
		        target_id, probe_type, status, time, %s AS latency_us, %s AS latency_source, loss_pct
		   FROM probe_results
		  WHERE agent_id = ANY($1) AND target_id = ANY($2)
		    AND time > now() - $3::interval
		  ORDER BY agent_id, target_id, probe_type, time DESC`,
		latencyExpr, latencySourceExpr),
		srcAgents, dstTargets, horizon)
	if err != nil {
		return nil, fmt.Errorf("direction latest: %w", err)
	}
	defer rows.Close()
	var out []MatrixRow
	for rows.Next() {
		var mr MatrixRow
		var targetID uuid.UUID
		if err := rows.Scan(&targetID, &mr.ProbeType, &mr.Status, &mr.Time, &mr.LatencyUS, &mr.LatencySource, &mr.LossPct); err != nil {
			return nil, fmt.Errorf("direction latest: %w", err)
		}
		mr.TargetID = &targetID
		out = append(out, mr)
	}
	return out, rows.Err()
}
