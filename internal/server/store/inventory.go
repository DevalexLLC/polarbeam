package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	AgentHealthOffline  = "offline"
	AgentHealthDegraded = "degraded"
	AgentHealthHealthy  = "healthy"
	AgentHealthNoData   = "no_data"
)

// AgentInventoryFilter is the query-mode contract for the operational
// agent inventory. Networks is nil for an unscoped caller.
type AgentInventoryFilter struct {
	Query    string
	Health   string
	Sort     string
	Order    string
	Limit    int
	Offset   int
	Networks []uuid.UUID
}

// AgentInventorySummary counts the complete filtered set before paging.
type AgentInventorySummary struct {
	Total    int64
	Offline  int64
	Degraded int64
	Healthy  int64
	NoData   int64
}

func agentInventoryOrder(sortName, order string) (string, error) {
	if order != "asc" && order != "desc" {
		return "", invalidf("agent inventory order must be asc or desc")
	}
	direction := " ASC"
	if order == "desc" {
		direction = " DESC"
	}
	var column string
	switch sortName {
	case "hostname":
		column = "lower(hostname)" + direction
	case "site":
		column = "lower(site)" + direction
	case "network":
		column = "lower(network)" + direction
	case "health":
		column = `CASE health
			WHEN 'offline' THEN 0 WHEN 'degraded' THEN 1
			WHEN 'no_data' THEN 2 ELSE 3 END` + direction
	case "last_seen":
		column = "last_seen_at" + direction + " NULLS LAST"
	case "version":
		column = "lower(version)" + direction
	default:
		return "", invalidf("unknown agent inventory sort %q", sortName)
	}
	return column + ", id" + direction, nil
}

const agentInventoryCTE = `
	WITH agent_rows AS MATERIALIZED (
		SELECT a.id, s.name AS site, n.name AS network, a.hostname,
		       a.probe_address, a.version, a.last_seen_at, a.created_at,
		       a.current_config_hash, a.dropped_results, a.last_dropped_at,
		       c.not_after, c.revoked_at,
		       COALESCE(o.offline, false) AS offline,
		       COALESCE(o.failing, 0) AS probes_failing,
		       COALESCE(ss.total, 0) AS probes_total,
		       CASE
		         WHEN c.revoked_at IS NOT NULL OR c.not_after < now()
		              OR COALESCE(o.offline, false) THEN 'offline'
		         WHEN a.last_seen_at IS NULL THEN 'no_data'
		         WHEN COALESCE(o.failing, 0) > 0 THEN 'degraded'
		         ELSE 'healthy'
		       END AS health
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
		              count(ep.probe_id) FILTER (WHERE oe.kind = 'probe_failing') AS failing
		         FROM outage_events oe
		         LEFT JOIN unnest($1::uuid[]) AS ep(probe_id)
		           ON oe.kind = 'probe_failing'
		          AND oe.probe_id = ep.probe_id
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
	), filtered AS MATERIALIZED (
		SELECT * FROM agent_rows
		 WHERE ($3 = ''
		        OR id::text ILIKE '%' || $3 || '%'
		        OR hostname ILIKE '%' || $3 || '%'
		        OR site ILIKE '%' || $3 || '%'
		        OR network ILIKE '%' || $3 || '%'
		        OR probe_address ILIKE '%' || $3 || '%'
		        OR version ILIKE '%' || $3 || '%')
		   AND ($4 = '' OR health = $4)
	)`

// QueryAgents returns a filtered, stably ordered page plus summary counts
// over the entire filtered set. It deliberately leaves ListAgents untouched
// so an unqueried /agents request retains its established JSON contract.
func (s *Store) QueryAgents(ctx context.Context, f AgentInventoryFilter) ([]AgentListInfo, AgentInventorySummary, error) {
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 {
		return nil, AgentInventorySummary{}, invalidf("invalid agent inventory page")
	}
	if f.Health != "" && f.Health != AgentHealthOffline && f.Health != AgentHealthDegraded &&
		f.Health != AgentHealthHealthy && f.Health != AgentHealthNoData {
		return nil, AgentInventorySummary{}, invalidf("unknown agent health %q", f.Health)
	}
	orderBy, err := agentInventoryOrder(f.Sort, f.Order)
	if err != nil {
		return nil, AgentInventorySummary{}, err
	}
	enabled, err := s.enabledProbeIDs(ctx)
	if err != nil {
		return nil, AgentInventorySummary{}, fmt.Errorf("query agents: %w", err)
	}
	args := []any{enabled, f.Networks, escapeLike(strings.TrimSpace(f.Query)), f.Health}

	var summary AgentInventorySummary
	rows, err := s.pool.Query(ctx, agentInventoryCTE+`
		SELECT id, site, network, hostname, probe_address, version, last_seen_at,
		       created_at, current_config_hash, dropped_results, last_dropped_at,
		       not_after, revoked_at, offline, probes_failing, probes_total, health,
		       count(*) OVER (),
		       count(*) FILTER (WHERE health = 'offline') OVER (),
		       count(*) FILTER (WHERE health = 'degraded') OVER (),
		       count(*) FILTER (WHERE health = 'healthy') OVER (),
		       count(*) FILTER (WHERE health = 'no_data') OVER ()
		  FROM filtered
		 ORDER BY `+orderBy+`
		 LIMIT $5 OFFSET $6`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, AgentInventorySummary{}, fmt.Errorf("query agents: %w", err)
	}
	defer rows.Close()
	list := make([]AgentListInfo, 0, f.Limit)
	for rows.Next() {
		var a AgentListInfo
		if err := rows.Scan(&a.ID, &a.Site, &a.Network, &a.Hostname, &a.ProbeAddress,
			&a.Version, &a.LastSeenAt, &a.CreatedAt, &a.ConfigHash, &a.DroppedResults,
			&a.LastDroppedAt, &a.CertNotAfter, &a.CertRevokedAt, &a.Offline,
			&a.ProbesFailing, &a.ProbesTotal, &a.Health, &summary.Total,
			&summary.Offline, &summary.Degraded, &summary.Healthy,
			&summary.NoData); err != nil {
			return nil, AgentInventorySummary{}, fmt.Errorf("scan queried agent: %w", err)
		}
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		return nil, AgentInventorySummary{}, err
	}
	if len(list) == 0 && f.Offset > 0 {
		err := s.pool.QueryRow(ctx, agentInventoryCTE+`
			SELECT count(*),
			       count(*) FILTER (WHERE health = 'offline'),
			       count(*) FILTER (WHERE health = 'degraded'),
			       count(*) FILTER (WHERE health = 'healthy'),
			       count(*) FILTER (WHERE health = 'no_data')
			  FROM filtered`, args...).Scan(&summary.Total, &summary.Offline,
			&summary.Degraded, &summary.Healthy, &summary.NoData)
		if err != nil {
			return nil, AgentInventorySummary{}, fmt.Errorf("summarize agents: %w", err)
		}
	}
	return list, summary, nil
}

const (
	TargetStatusIncident    = "incident"
	TargetStatusUnprobed    = "unprobed"
	TargetStatusNoIncidents = "no_incidents"
)

// OperationalTargetInfo is one row of the read-only Targets inventory.
type OperationalTargetInfo struct {
	ID                uuid.UUID
	Kind              string
	Name              string
	Address           string
	Port              int32
	URL               string
	Network           string
	CreatedAt         time.Time
	ProbeCount        int64
	EnabledProbeCount int64
	ProbingSites      []string
	OpenIncidents     int64
	Status            string
	AgentID           *uuid.UUID
	AgentSite         *string
	AgentHostname     *string
}

type TargetInventoryFilter struct {
	Query                 string
	Kind                  string
	Status                string
	Sort                  string
	Order                 string
	Limit                 int
	Offset                int
	Networks              []uuid.UUID
	RequireScopedActivity bool
}

type TargetInventorySummary struct {
	Total       int64
	External    int64
	Agent       int64
	Incident    int64
	Unprobed    int64
	NoIncidents int64
}

func targetInventoryOrder(sortName, order string) (string, error) {
	if order != "asc" && order != "desc" {
		return "", invalidf("target inventory order must be asc or desc")
	}
	direction := " ASC"
	if order == "desc" {
		direction = " DESC"
	}
	var column string
	switch sortName {
	case "name":
		column = "lower(name)"
	case "kind":
		column = "kind"
	case "status":
		column = `CASE status WHEN 'incident' THEN 0 WHEN 'unprobed' THEN 1 ELSE 2 END`
	case "created":
		column = "created_at"
	case "probes":
		column = "probe_count"
	default:
		return "", invalidf("unknown target inventory sort %q", sortName)
	}
	return column + direction + ", id" + direction, nil
}

const targetInventoryCTE = `
	WITH visible_targets AS MATERIALIZED (
		SELECT t.id, t.kind, t.name, t.address, t.port, t.url, t.created_at,
		       COALESCE(tn.name, an.name, '') AS network,
		       a.id AS agent_id, s.name AS agent_site, a.hostname AS agent_hostname
		  FROM targets t
		  LEFT JOIN networks tn ON tn.id = t.network_id
		  LEFT JOIN agents a ON a.id = t.agent_id
		  LEFT JOIN networks an ON an.id = a.network_id
		  LEFT JOIN sites s ON s.id = a.site_id
		 WHERE ($1::uuid[] IS NULL
		    OR (t.kind = 'external'
		        AND (t.network_id IS NULL OR t.network_id = ANY($1)))
		    OR (t.kind = 'agent' AND a.network_id = ANY($1)))
		   AND (NOT $5::boolean OR t.kind = 'agent' OR EXISTS (
		       SELECT 1 FROM probe_configs visible_probe
		        WHERE visible_probe.target_id = t.id
		          AND visible_probe.enabled
		          AND visible_probe.mesh_id IS NULL
		          AND visible_probe.network_id = ANY($1)
		   ))
	), probe_rows AS MATERIALIZED (
		SELECT vt.id AS target_id, pc.id AS config_id, pc.enabled,
		       source_site.name AS probing_site
		  FROM visible_targets vt
		  JOIN probe_configs pc ON pc.target_id = vt.id
		  JOIN sites source_site ON source_site.id = pc.site_id
		 WHERE pc.mesh_id IS NULL
		   AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1))
		UNION ALL
		SELECT vt.id, pc.id, pc.enabled, source_site.name
		  FROM visible_targets vt
		  JOIN agents destination ON destination.id = vt.agent_id
		  JOIN mesh_members destination_member
		    ON destination_member.site_id = destination.site_id
		  JOIN mesh_groups mesh
		    ON mesh.id = destination_member.mesh_id
		   AND mesh.network_id = destination.network_id
		  JOIN probe_configs pc ON pc.mesh_id = mesh.id
		  JOIN mesh_members source_member
		    ON source_member.mesh_id = mesh.id
		   AND source_member.site_id <> destination.site_id
		  JOIN sites source_site ON source_site.id = source_member.site_id
		 WHERE vt.kind = 'agent'
		   AND ($1::uuid[] IS NULL OR mesh.network_id = ANY($1))
		   AND (EXISTS (
		       SELECT 1 FROM agents source_agent
		        WHERE source_agent.site_id = source_member.site_id
		          AND source_agent.network_id = mesh.network_id
		   ) OR NOT EXISTS (
		       SELECT 1 FROM agents source_agent
		        WHERE source_agent.site_id = source_member.site_id
		   ))
	), probe_aggregates AS (
		SELECT target_id, count(DISTINCT config_id) AS probe_count,
		       count(DISTINCT config_id) FILTER (WHERE enabled) AS enabled_probe_count,
		       COALESCE(array_agg(DISTINCT probing_site ORDER BY probing_site)
		         FILTER (WHERE enabled), ARRAY[]::text[]) AS probing_sites
		  FROM probe_rows
		 GROUP BY target_id
	), incident_aggregates AS (
		SELECT oe.target_id, count(*) AS open_incidents
		  FROM outage_events oe
		  JOIN agents source_agent ON source_agent.id = oe.agent_id
		 WHERE oe.closed_at IS NULL AND oe.target_id IS NOT NULL
		   AND ($1::uuid[] IS NULL OR source_agent.network_id = ANY($1))
		 GROUP BY oe.target_id
	), target_rows AS MATERIALIZED (
		SELECT vt.*, COALESCE(pa.probe_count, 0) AS probe_count,
		       COALESCE(pa.enabled_probe_count, 0) AS enabled_probe_count,
		       COALESCE(pa.probing_sites, ARRAY[]::text[]) AS probing_sites,
		       COALESCE(ia.open_incidents, 0) AS open_incidents,
		       CASE WHEN COALESCE(ia.open_incidents, 0) > 0 THEN 'incident'
		            WHEN COALESCE(pa.enabled_probe_count, 0) = 0 THEN 'unprobed'
		            ELSE 'no_incidents' END AS status
		  FROM visible_targets vt
		  LEFT JOIN probe_aggregates pa ON pa.target_id = vt.id
		  LEFT JOIN incident_aggregates ia ON ia.target_id = vt.id
	), scope_summary AS MATERIALIZED (
		SELECT count(*) AS total,
		       count(*) FILTER (WHERE kind = 'external') AS external,
		       count(*) FILTER (WHERE kind = 'agent') AS agent,
		       count(*) FILTER (WHERE status = 'incident') AS incident,
		       count(*) FILTER (WHERE status = 'unprobed') AS unprobed,
		       count(*) FILTER (WHERE status = 'no_incidents') AS no_incidents
		  FROM target_rows
	), filtered AS MATERIALIZED (
		SELECT * FROM target_rows
		 WHERE ($2 = ''
		        OR id::text ILIKE '%' || $2 || '%'
		        OR COALESCE(agent_id::text, '') ILIKE '%' || $2 || '%'
		        OR name ILIKE '%' || $2 || '%'
		        OR address ILIKE '%' || $2 || '%'
		        OR port::text ILIKE '%' || $2 || '%'
		        OR url ILIKE '%' || $2 || '%'
		        OR network ILIKE '%' || $2 || '%'
		        OR COALESCE(agent_site, '') ILIKE '%' || $2 || '%'
		        OR COALESCE(agent_hostname, '') ILIKE '%' || $2 || '%'
		        OR array_to_string(probing_sites, ' ') ILIKE '%' || $2 || '%')
		   AND ($3 = '' OR kind = $3)
		   AND ($4 = '' OR status = $4)
	)`

// QueryOperationalTargets returns the read-only target inventory with
// counts and evidence joined by stable IDs. Every aggregate is scoped before
// filtering so a query or summary cannot reveal another tenant's activity.
func (s *Store) QueryOperationalTargets(ctx context.Context, f TargetInventoryFilter) ([]OperationalTargetInfo, TargetInventorySummary, TargetInventorySummary, error) {
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, invalidf("invalid target inventory page")
	}
	if f.RequireScopedActivity && f.Networks == nil {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, invalidf("scoped target activity requires networks")
	}
	if f.Kind != "" && f.Kind != "agent" && f.Kind != "external" {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, invalidf("unknown target kind %q", f.Kind)
	}
	if f.Status != "" && f.Status != TargetStatusIncident && f.Status != TargetStatusUnprobed &&
		f.Status != TargetStatusNoIncidents {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, invalidf("unknown target status %q", f.Status)
	}
	orderBy, err := targetInventoryOrder(f.Sort, f.Order)
	if err != nil {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, err
	}
	args := []any{f.Networks, escapeLike(strings.TrimSpace(f.Query)), f.Kind, f.Status, f.RequireScopedActivity}
	var summary, scopeSummary TargetInventorySummary
	rows, err := s.pool.Query(ctx, targetInventoryCTE+`
		SELECT id, kind, name, address, port, url, network, created_at,
		       probe_count, enabled_probe_count, probing_sites, open_incidents,
		       status, agent_id, agent_site, agent_hostname,
		       count(*) OVER (),
		       count(*) FILTER (WHERE kind = 'external') OVER (),
		       count(*) FILTER (WHERE kind = 'agent') OVER (),
		       count(*) FILTER (WHERE status = 'incident') OVER (),
		       count(*) FILTER (WHERE status = 'unprobed') OVER (),
		       count(*) FILTER (WHERE status = 'no_incidents') OVER (),
		       scope.total, scope.external, scope.agent,
		       scope.incident, scope.unprobed, scope.no_incidents
		  FROM filtered CROSS JOIN scope_summary scope
		 ORDER BY `+orderBy+`
		 LIMIT $6 OFFSET $7`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, fmt.Errorf("query operational targets: %w", err)
	}
	defer rows.Close()
	list := make([]OperationalTargetInfo, 0, f.Limit)
	for rows.Next() {
		var target OperationalTargetInfo
		if err := rows.Scan(&target.ID, &target.Kind, &target.Name, &target.Address,
			&target.Port, &target.URL, &target.Network, &target.CreatedAt,
			&target.ProbeCount, &target.EnabledProbeCount, &target.ProbingSites,
			&target.OpenIncidents, &target.Status, &target.AgentID,
			&target.AgentSite, &target.AgentHostname, &summary.Total,
			&summary.External, &summary.Agent, &summary.Incident,
			&summary.Unprobed, &summary.NoIncidents, &scopeSummary.Total,
			&scopeSummary.External, &scopeSummary.Agent, &scopeSummary.Incident,
			&scopeSummary.Unprobed, &scopeSummary.NoIncidents); err != nil {
			return nil, TargetInventorySummary{}, TargetInventorySummary{}, fmt.Errorf("scan operational target: %w", err)
		}
		list = append(list, target)
	}
	if err := rows.Err(); err != nil {
		return nil, TargetInventorySummary{}, TargetInventorySummary{}, err
	}
	if len(list) == 0 {
		err := s.pool.QueryRow(ctx, targetInventoryCTE+`
			SELECT filtered.total, filtered.external, filtered.agent,
			       filtered.incident, filtered.unprobed, filtered.no_incidents,
			       scope.total, scope.external, scope.agent,
			       scope.incident, scope.unprobed, scope.no_incidents
			  FROM (
				SELECT count(*) AS total,
				       count(*) FILTER (WHERE kind = 'external') AS external,
				       count(*) FILTER (WHERE kind = 'agent') AS agent,
				       count(*) FILTER (WHERE status = 'incident') AS incident,
				       count(*) FILTER (WHERE status = 'unprobed') AS unprobed,
				       count(*) FILTER (WHERE status = 'no_incidents') AS no_incidents
				  FROM filtered
			  ) filtered CROSS JOIN scope_summary scope`, args...).Scan(
			&summary.Total, &summary.External,
			&summary.Agent, &summary.Incident, &summary.Unprobed,
			&summary.NoIncidents, &scopeSummary.Total, &scopeSummary.External,
			&scopeSummary.Agent, &scopeSummary.Incident, &scopeSummary.Unprobed,
			&scopeSummary.NoIncidents)
		if err != nil {
			return nil, TargetInventorySummary{}, TargetInventorySummary{}, fmt.Errorf("summarize targets: %w", err)
		}
	}
	return list, summary, scopeSummary, nil
}
