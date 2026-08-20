package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Target detail read queries. The target detail page is the per-target
// mirror of the pair drill-down: latency/loss series and summaries reuse
// PairSeries/PairSummary/PairLatencySource with dstTargets = [targetID]
// (the caggs already group by target_id), so only the stage breakdown and
// the health inventory need queries of their own.

// TargetSource is one (site, network) probing a target, with the agents
// that have series toward it. Series fold across a source's agents exactly
// like pair directions do; splitting by network keeps planes apart when a
// site probes the same target from more than one (single-network installs
// fold identically to the pre-networks site-only key).
type TargetSource struct {
	Site     string
	Network  string
	AgentIDs []uuid.UUID
}

// TargetEndpoints resolves a target row plus every site currently or
// recently probing it. DstSite is the owning agent's site name for
// agent-kind targets (nil for external targets and for agent targets whose
// agent row is gone). Sources come from series_state, which persists
// through probe DISABLE and spool replay — but probe DELETION drops the
// series rows (cleanupSeries), so a site whose last probe toward the
// target is deleted stops being listed even while cagg history survives.
// Known limitation: surfacing that orphaned history would need
// history-derived source discovery (a target-leading cagg index plus a
// union query); today the page shows sources only while a series exists.
type TargetEndpoints struct {
	ID      uuid.UUID
	Kind    string
	Name    string
	Address string
	Port    int32
	URL     string
	AgentID *uuid.UUID
	DstSite *string
	Sources []TargetSource
}

// StageBucket is one time_bucket of a target's stage-timing series: the
// per-stage successful averages in microseconds (nil = no probe measured
// that stage in the bucket) and the successful sample count.
type StageBucket struct {
	Bucket  time.Time
	DNSUS   *float64
	TCPUS   *float64
	TLSUS   *float64
	TTFBUS  *float64
	TotalUS *float64
	Samples int64
}

// TargetProbeHealthRow is one (probe series, bucket) row of a target's
// per-probe health detail — AgentProbeHealthRow's shape from the target's
// side, so source agent/site labels replace target labels. Bucket/Samples/OK
// are nil for a series with no samples in the window (the LEFT JOIN's
// single row), so configured-but-silent series still appear.
type TargetProbeHealthRow struct {
	AgentID    uuid.UUID
	SrcSite    string
	Network    string
	Hostname   string
	ProbeID    uuid.UUID
	ProbeType  int16
	LastStatus int16
	LastTime   time.Time
	// OpenedAt is the open probe_failing outage; nil = not failing. Open
	// probe_degraded events are excluded, same as AgentProbeHealth.
	OpenedAt  *time.Time
	OpenError *string
	Bucket    *time.Time
	Samples   *int64
	OK        *int64
}

// stageTable maps a Source to its stage-cagg relation. Separate from
// Source.table() — that whitelist is frozen with the 0002/0003 caggs —
// but the same rule applies: query text interpolates only these
// constants, never input.
func stageTable(source Source) string {
	switch source {
	case SourceHourly:
		return "probe_results_stage_hourly"
	case SourceDaily:
		return "probe_results_stage_daily"
	default:
		return ""
	}
}

// TargetEndpoints resolves a target ID to its row and probing sites, or
// (nil, nil) when the target does not exist. Resolving agent IDs first
// lets the hypertable and cagg scans hit the (agent_id, target_id, ...)
// indexes with no joins — the SiteEndpoints design. networks is the
// caller's network scope (nil = unfiltered): sources are filtered to
// allowed planes, and an agent-kind target whose owning agent sits on a
// foreign plane resolves to (nil, nil) — byte-identical to an unknown id.
// External targets stay visible to every scope (operator-published probe
// destinations carry no plane yet).
func (s *Store) TargetEndpoints(ctx context.Context, targetID uuid.UUID, networks []uuid.UUID) (*TargetEndpoints, error) {
	var ep TargetEndpoints
	var dstNetworkID *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT t.id, t.kind, t.name, t.address, t.port, t.url, t.agent_id, ds.name, da.network_id
		   FROM targets t
		   LEFT JOIN agents da ON da.id = t.agent_id
		   LEFT JOIN sites ds ON ds.id = da.site_id
		  WHERE t.id = $1`, targetID).
		Scan(&ep.ID, &ep.Kind, &ep.Name, &ep.Address, &ep.Port, &ep.URL, &ep.AgentID, &ep.DstSite, &dstNetworkID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", targetID, err)
	}
	if networks != nil && ep.AgentID != nil &&
		(dstNetworkID == nil || !slices.Contains(networks, *dstNetworkID)) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.name, n.name, ss.agent_id
		   FROM series_state ss
		   JOIN agents a ON a.id = ss.agent_id
		   JOIN sites  s ON s.id = a.site_id
		   JOIN networks n ON n.id = a.network_id
		  WHERE ss.target_id = $1
		    AND ($2::uuid[] IS NULL OR a.network_id = ANY($2))
		  GROUP BY s.name, n.name, ss.agent_id
		  ORDER BY s.name, n.name, ss.agent_id`, targetID, networks)
	if err != nil {
		return nil, fmt.Errorf("target %s sources: %w", targetID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var site, network string
		var agentID uuid.UUID
		if err := rows.Scan(&site, &network, &agentID); err != nil {
			return nil, fmt.Errorf("target %s sources: %w", targetID, err)
		}
		if n := len(ep.Sources); n > 0 && ep.Sources[n-1].Site == site && ep.Sources[n-1].Network == network {
			ep.Sources[n-1].AgentIDs = append(ep.Sources[n-1].AgentIDs, agentID)
		} else {
			ep.Sources = append(ep.Sources, TargetSource{Site: site, Network: network, AgentIDs: []uuid.UUID{agentID}})
		}
	}
	return &ep, rows.Err()
}

// TargetStageSeries buckets a target's stage timings (DNS, TCP connect,
// TLS handshake, TTFB, total) over the window, reading raw or a stage
// continuous aggregate per source. Averages cover successful probes only;
// per-stage counts already exclude rows that did not measure a stage, so
// probe types without application timings contribute nothing. The stage
// caggs backfill only from raw surviving their deployment (≤14d), so long
// windows may be sparse right after an upgrade.
func (s *Store) TargetStageSeries(ctx context.Context, srcAgents []uuid.UUID, targetID uuid.UUID, bucket, window time.Duration, source Source) ([]StageBucket, error) {
	var sql string
	if table := stageTable(source); table == "" {
		sql = `SELECT time_bucket($1::interval, time) AS bucket,
		        avg(dns_us) FILTER (WHERE status = 1)::float8,
		        avg(tcp_connect_us) FILTER (WHERE status = 1)::float8,
		        avg(tls_handshake_us) FILTER (WHERE status = 1)::float8,
		        avg(ttfb_us) FILTER (WHERE status = 1)::float8,
		        avg(total_us) FILTER (WHERE status = 1)::float8,
		        count(*) FILTER (WHERE status = 1)
		   FROM probe_results
		  WHERE agent_id = ANY($2) AND target_id = $3
		    AND time > now() - $4::interval
		  GROUP BY bucket ORDER BY bucket`
	} else {
		// time_bucket over the cagg's bucket column regroups rows into
		// wider chart buckets (identity when widths already match).
		// Averages come from the materialized sums/counts.
		sql = fmt.Sprintf(
			`SELECT time_bucket($1::interval, bucket) AS b,
			        sum(dns_sum_us)::float8   / NULLIF(sum(dns_count), 0)::float8,
			        sum(tcp_sum_us)::float8   / NULLIF(sum(tcp_count), 0)::float8,
			        sum(tls_sum_us)::float8   / NULLIF(sum(tls_count), 0)::float8,
			        sum(ttfb_sum_us)::float8  / NULLIF(sum(ttfb_count), 0)::float8,
			        sum(total_sum_us)::float8 / NULLIF(sum(total_count), 0)::float8,
			        coalesce(sum(ok_samples), 0)::bigint
			   FROM %s
			  WHERE agent_id = ANY($2) AND target_id = $3
			    AND bucket > now() - $4::interval
			  GROUP BY b ORDER BY b`, table)
	}
	rows, err := s.pool.Query(ctx, sql, bucket, srcAgents, targetID, window)
	if err != nil {
		return nil, fmt.Errorf("target stage series: %w", err)
	}
	defer rows.Close()
	var out []StageBucket
	for rows.Next() {
		var b StageBucket
		if err := rows.Scan(&b.Bucket, &b.DNSUS, &b.TCPUS, &b.TLSUS, &b.TTFBUS, &b.TotalUS, &b.Samples); err != nil {
			return nil, fmt.Errorf("target stage series: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TargetProbeHealth returns a target's probe series with their bucketed
// success counts over the window, ordered by source site, hostname,
// agent id, probe, bucket (the handler's run-length fold relies on every
// (agent, probe) series being contiguous — agent_id is in the ORDER BY
// because mesh probes share one probe_id across a site's agents, and
// hostnames are not unique, so neither alone keeps series apart). It is
// AgentProbeHealth from the target's side and keeps its contract: the
// inventory is series_state intersected with enabledProbeIDs so counts
// agree with the Agents view; a configured-and-enabled series with no
// results still yields one nil-bucket row; traceroute series are included
// as-is (nothing aggregates across probes); buckets come from
// probe_results_health_30m, so bucket must be a multiple of 30 min and
// windows must stay inside its 14 d retention. The subquery's GROUP BY
// repeats the time_bucket expression instead of naming the alias for the
// same input-column-resolution reason as AgentProbeHealth.
// networks is the caller's network scope (nil = unfiltered), applied to the
// source agents' planes.
func (s *Store) TargetProbeHealth(ctx context.Context, targetID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]TargetProbeHealthRow, error) {
	enabled, err := s.enabledProbeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("target probe health: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT ss.agent_id, s.name, n.name, a.hostname, ss.probe_id, ss.probe_type,
		        ss.last_status, ss.last_time, oe.opened_at, oe.open_error,
		        b.bucket, b.samples, b.ok
		   FROM series_state ss
		   JOIN unnest($4::uuid[]) AS ep(probe_id) ON ep.probe_id = ss.probe_id
		   JOIN agents a ON a.id = ss.agent_id
		   JOIN sites  s ON s.id = a.site_id
		   JOIN networks n ON n.id = a.network_id
		   LEFT JOIN outage_events oe ON oe.id = ss.open_event_id AND oe.kind = 'probe_failing'
		   LEFT JOIN (
		        SELECT h.agent_id, h.probe_id, time_bucket($2::interval, h.bucket) AS bucket,
		               sum(h.samples)::bigint AS samples, sum(h.ok_samples)::bigint AS ok
		          FROM probe_results_health_30m h
		          JOIN series_state hs ON hs.agent_id = h.agent_id
		               AND hs.probe_id = h.probe_id AND hs.target_id = $1
		         WHERE h.bucket > now() - $3::interval
		         GROUP BY h.agent_id, h.probe_id, time_bucket($2::interval, h.bucket)
		   ) b ON b.agent_id = ss.agent_id AND b.probe_id = ss.probe_id
		  WHERE ss.target_id = $1
		    AND ($5::uuid[] IS NULL OR a.network_id = ANY($5))
		  ORDER BY s.name, a.hostname, ss.agent_id, ss.probe_id, b.bucket`, targetID, bucket, window, enabled, networks)
	if err != nil {
		return nil, fmt.Errorf("target probe health: %w", err)
	}
	defer rows.Close()
	var out []TargetProbeHealthRow
	for rows.Next() {
		var r TargetProbeHealthRow
		if err := rows.Scan(&r.AgentID, &r.SrcSite, &r.Network, &r.Hostname, &r.ProbeID, &r.ProbeType,
			&r.LastStatus, &r.LastTime, &r.OpenedAt, &r.OpenError,
			&r.Bucket, &r.Samples, &r.OK); err != nil {
			return nil, fmt.Errorf("target probe health: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
