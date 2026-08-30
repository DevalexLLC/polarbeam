package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
)

type SiteConfigFilter struct {
	Query    string
	Sort     string
	Order    string
	Limit    int
	Offset   int
	Networks []uuid.UUID
}

type TargetConfigFilter struct {
	Query    string
	Kind     string
	Sort     string
	Order    string
	Limit    int
	Offset   int
	Networks []uuid.UUID
}

type ProbeConfigFilter struct {
	Query     string
	Mode      string
	Enabled   *bool
	ProbeType int16
	Sort      string
	Order     string
	Limit     int
	Offset    int
	Networks  []uuid.UUID
}

func inventoryDirection(order, what string) (string, error) {
	switch order {
	case "asc":
		return " ASC", nil
	case "desc":
		return " DESC", nil
	default:
		return "", invalidf("%s order must be asc or desc", what)
	}
}

var siteConfigSort = orderAllowlist{
	name:    "site config",
	tieExpr: "id",
	columns: map[string]orderColumn{
		"name":         {expr: "lower(name)"},
		"display_name": {expr: "lower(display_name)"},
		"created":      {expr: "created_at"},
		"agents":       {expr: "agent_count"},
		"meshes":       {expr: "mesh_count"},
		"probes":       {expr: "probe_count"},
	},
}

var siteConfigRowsSQL = `
	SELECT s.id, s.name, s.display_name, s.location, s.latitude, s.longitude, s.created_at,
	       (SELECT count(*) FROM agents a
	         WHERE a.site_id = s.id
	           AND ($1::uuid[] IS NULL OR a.network_id = ANY($1))) AS agent_count,
	       (SELECT count(*) FROM mesh_members mm
	          JOIN mesh_groups mg ON mg.id = mm.mesh_id
	         WHERE mm.site_id = s.id
	           AND ($1::uuid[] IS NULL OR mg.network_id = ANY($1))) AS mesh_count,
	       (SELECT count(*) FROM probe_configs pc
	         WHERE pc.site_id = s.id
	           AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1))) AS probe_count
	  FROM sites s
	 WHERE ` + siteScopePredicate("s.id", "$1")

func scanSiteConfig(row rowScanner, extra ...any) (SiteAdminInfo, error) {
	var si SiteAdminInfo
	dest := []any{&si.ID, &si.Name, &si.DisplayName, &si.Location,
		&si.Latitude, &si.Longitude, &si.CreatedAt,
		&si.AgentCount, &si.MeshCount, &si.ProbeCount}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return si, err
	}
	return si, nil
}

// QuerySitesConfig filters, sorts, and pages the settings site inventory.
// Tenant visibility and every reference count are scoped before search.
func (s *Store) QuerySitesConfig(ctx context.Context, f SiteConfigFilter) ([]SiteAdminInfo, int64, error) {
	if err := pageBounds("site config", f.Limit, f.Offset); err != nil {
		return nil, 0, err
	}
	orderBy, err := siteConfigSort.clause(f.Sort, f.Order)
	if err != nil {
		return nil, 0, err
	}
	cte := `WITH site_rows AS MATERIALIZED (` + siteConfigRowsSQL + `),
		filtered AS MATERIALIZED (
			SELECT * FROM site_rows
			 WHERE $2 = '' OR id::text = $2
			    OR name ILIKE '%' || $2 || '%'
			    OR display_name ILIKE '%' || $2 || '%'
			    OR location ILIKE '%' || $2 || '%'
		)`
	var total int64
	out, err := runPaged(ctx, s.pool, f.Limit, f.Offset, pagedSpec[SiteAdminInfo]{
		queryErr:    "query sites config",
		scanErr:     "scan site config",
		fallbackErr: "count sites config",
		pageSQL: cte + `
			SELECT id, name, display_name, location, latitude, longitude, created_at,
			       agent_count, mesh_count, probe_count, count(*) OVER ()
			  FROM filtered ORDER BY ` + orderBy + ` LIMIT $3 OFFSET $4`,
		args: []any{f.Networks, escapeLike(strings.TrimSpace(f.Query))},
		scan: func(row rowScanner) (SiteAdminInfo, error) {
			return scanSiteConfig(row, &total)
		},
		fallbackSQL:  cte + ` SELECT count(*) FROM filtered`,
		fallbackDest: []any{&total},
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetSiteConfig returns one settings site row with caller-scoped counts.
func (s *Store) GetSiteConfig(ctx context.Context, name string, networks []uuid.UUID) (*SiteAdminInfo, error) {
	si, err := scanSiteConfig(s.pool.QueryRow(ctx,
		`WITH site_rows AS (`+siteConfigRowsSQL+`)
		 SELECT id, name, display_name, location, latitude, longitude, created_at,
		        agent_count, mesh_count, probe_count
		   FROM site_rows WHERE name = $2`,
		networks, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFoundf("site %q does not exist", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get site config: %w", err)
	}
	return &si, nil
}

var targetConfigSort = orderAllowlist{
	name:    "target config",
	tieExpr: "id",
	columns: map[string]orderColumn{
		"name":    {expr: "lower(name)"},
		"kind":    {expr: "kind"},
		"network": {expr: "lower(network)"},
		"probes":  {expr: "probe_count"},
		"created": {expr: "created_at"},
	},
}

const targetConfigRowsSQL = `
	SELECT t.id, t.kind, t.name, t.agent_id, t.address, t.port, t.url, t.created_at,
	       COALESCE(owner_network.name, '') AS network,
	       (SELECT count(*) FROM probe_configs pc
	         WHERE pc.target_id = t.id
	           AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1))) AS probe_count
	  FROM targets t
	  LEFT JOIN networks owner_network ON owner_network.id = t.network_id
	  LEFT JOIN agents target_agent ON target_agent.id = t.agent_id
	 WHERE $1::uuid[] IS NULL
	    OR (t.agent_id IS NULL
	        AND (t.network_id IS NULL OR t.network_id = ANY($1)))
	    OR target_agent.network_id = ANY($1)`

func scanTargetConfig(row rowScanner, extra ...any) (TargetInfo, error) {
	var t TargetInfo
	dest := []any{&t.ID, &t.Kind, &t.Name, &t.AgentID, &t.Address, &t.Port,
		&t.URL, &t.CreatedAt, &t.Network, &t.ProbeCount}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return t, err
	}
	return t, nil
}

// QueryTargetsConfig filters, sorts, and pages the settings target inventory.
func (s *Store) QueryTargetsConfig(ctx context.Context, f TargetConfigFilter) ([]TargetInfo, int64, error) {
	if err := pageBounds("target config", f.Limit, f.Offset); err != nil {
		return nil, 0, err
	}
	if f.Kind != "" && f.Kind != "agent" && f.Kind != "external" {
		return nil, 0, invalidf("unknown target config kind %q", f.Kind)
	}
	orderBy, err := targetConfigSort.clause(f.Sort, f.Order)
	if err != nil {
		return nil, 0, err
	}
	cte := `WITH target_rows AS MATERIALIZED (` + targetConfigRowsSQL + `),
		filtered AS MATERIALIZED (
			SELECT * FROM target_rows
			 WHERE ($2 = '' OR id::text = $2
			        OR name ILIKE '%' || $2 || '%'
			        OR address ILIKE '%' || $2 || '%'
			        OR port::text ILIKE '%' || $2 || '%'
			        OR url ILIKE '%' || $2 || '%')
			   AND ($3 = '' OR kind = $3)
		)`
	var total int64
	out, err := runPaged(ctx, s.pool, f.Limit, f.Offset, pagedSpec[TargetInfo]{
		queryErr:    "query targets config",
		scanErr:     "scan target config",
		fallbackErr: "count targets config",
		pageSQL: cte + `
			SELECT id, kind, name, agent_id, address, port, url, created_at,
			       network, probe_count, count(*) OVER ()
			  FROM filtered ORDER BY ` + orderBy + ` LIMIT $4 OFFSET $5`,
		args: []any{f.Networks, escapeLike(strings.TrimSpace(f.Query)), f.Kind},
		scan: func(row rowScanner) (TargetInfo, error) {
			return scanTargetConfig(row, &total)
		},
		fallbackSQL:  cte + ` SELECT count(*) FROM filtered`,
		fallbackDest: []any{&total},
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetTargetConfig returns one visible target using the settings response row.
func (s *Store) GetTargetConfig(ctx context.Context, name string, networks []uuid.UUID) (*TargetInfo, error) {
	t, err := scanTargetConfig(s.pool.QueryRow(ctx,
		`WITH target_rows AS (`+targetConfigRowsSQL+`)
		 SELECT id, kind, name, agent_id, address, port, url, created_at,
		        network, probe_count
		   FROM target_rows WHERE name = $2`,
		networks, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFoundf("target %q does not exist", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get target config: %w", err)
	}
	return &t, nil
}

const probeConfigRowsSQL = `
	SELECT pc.id, COALESCE(s.name, '') AS site, COALESCE(t.name, '') AS target,
	       COALESCE(g.name, '') AS mesh,
	       pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms,
	       pc.params, pc.enabled, pc.created_at, pc.updated_at, pc.updated_by,
	       COALESCE(nd.name, ng.name, '') AS network,
	       CASE WHEN pc.mesh_id IS NULL THEN 'direct' ELSE 'mesh' END AS mode,
	       COALESCE(t.name, g.name, '') AS assignment,
	       COALESCE(type_names.name, 'type-' || pc.probe_type::text) AS type_name
	  FROM probe_configs pc
	  LEFT JOIN sites s ON s.id = pc.site_id
	  LEFT JOIN targets t ON t.id = pc.target_id
	  LEFT JOIN mesh_groups g ON g.id = pc.mesh_id
	  LEFT JOIN networks nd ON nd.id = pc.network_id
	  LEFT JOIN networks ng ON ng.id = g.network_id
	  LEFT JOIN unnest($2::smallint[], $3::text[]) AS type_names(id, name)
	    ON type_names.id = pc.probe_type
	 WHERE $1::uuid[] IS NULL OR COALESCE(pc.network_id, g.network_id) = ANY($1)`

func probeTypeLookup() ([]int16, []string) {
	names := probeadmin.Names()
	ids := make([]int16, len(names))
	for i, name := range names {
		ids[i] = int16(probeadmin.TypeNames[name])
	}
	return ids, names
}

func probeConfigOrder(sortName, order string) (string, error) {
	direction, err := inventoryDirection(order, "probe config")
	if err != nil {
		return "", err
	}
	columns := map[string]string{
		"site":    "lower(site)",
		"target":  "lower(assignment)",
		"type":    "type_name",
		"enabled": "enabled",
		"updated": "updated_at",
	}
	column, ok := columns[sortName]
	if !ok {
		return "", invalidf("unknown probe config sort %q", sortName)
	}
	return column + direction + ", id" + direction, nil
}

func scanProbeConfigInventory(row rowScanner, extra ...any) (ProbeConfigInfo, error) {
	var (
		p                                     ProbeConfigInfo
		intervalMS, timeoutMS, trainSpacingMS int64
		mode, assignment, typeName            string
	)
	dest := []any{&p.ID, &p.Site, &p.Target, &p.Mesh, &p.ProbeType,
		&intervalMS, &timeoutMS, &p.TrainCount, &trainSpacingMS, &p.Params,
		&p.Enabled, &p.CreatedAt, &p.UpdatedAt, &p.UpdatedBy, &p.Network,
		&mode, &assignment, &typeName}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return p, err
	}
	p.Interval = time.Duration(intervalMS) * time.Millisecond
	p.Timeout = time.Duration(timeoutMS) * time.Millisecond
	p.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
	return p, nil
}

// QueryProbeConfigs filters, sorts, and pages direct and mesh assignments.
func (s *Store) QueryProbeConfigs(ctx context.Context, f ProbeConfigFilter) ([]ProbeConfigInfo, int64, error) {
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 {
		return nil, 0, invalidf("invalid probe config page")
	}
	if f.Mode != "" && f.Mode != "direct" && f.Mode != "mesh" {
		return nil, 0, invalidf("unknown probe config mode %q", f.Mode)
	}
	if f.ProbeType < 0 {
		return nil, 0, invalidf("unknown probe config type %d", f.ProbeType)
	}
	orderBy, err := probeConfigOrder(f.Sort, f.Order)
	if err != nil {
		return nil, 0, err
	}
	typeIDs, typeNames := probeTypeLookup()
	args := []any{f.Networks, typeIDs, typeNames, escapeLike(strings.TrimSpace(f.Query)), f.Mode, f.Enabled, f.ProbeType}
	cte := `WITH probe_rows AS MATERIALIZED (` + probeConfigRowsSQL + `),
		filtered AS MATERIALIZED (
			SELECT * FROM probe_rows
			 WHERE ($4 = '' OR id::text = $4
			        OR site ILIKE '%' || $4 || '%'
			        OR assignment ILIKE '%' || $4 || '%'
			        OR type_name ILIKE '%' || $4 || '%')
			   AND ($5 = '' OR mode = $5)
			   AND ($6::boolean IS NULL OR enabled = $6)
			   AND ($7::smallint = 0 OR probe_type = $7)
		)`
	rows, err := s.pool.Query(ctx, cte+`
		SELECT id, site, target, mesh, probe_type, interval_ms, timeout_ms,
		       train_count, train_spacing_ms, params, enabled, created_at,
		       updated_at, updated_by, network, mode, assignment, type_name,
		       count(*) OVER ()
		  FROM filtered ORDER BY `+orderBy+` LIMIT $8 OFFSET $9`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query probe configs: %w", err)
	}
	defer rows.Close()
	out := make([]ProbeConfigInfo, 0, f.Limit)
	var total int64
	for rows.Next() {
		p, err := scanProbeConfigInventory(rows, &total)
		if err != nil {
			return nil, 0, fmt.Errorf("scan probe config: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) == 0 && f.Offset > 0 {
		if err := s.pool.QueryRow(ctx, cte+` SELECT count(*) FROM filtered`, args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count probe configs: %w", err)
		}
	}
	return out, total, nil
}

// GetProbeConfigScoped returns one probe only when its plane is visible.
func (s *Store) GetProbeConfigScoped(ctx context.Context, id uuid.UUID, networks []uuid.UUID) (*ProbeConfigInfo, error) {
	typeIDs, typeNames := probeTypeLookup()
	p, err := scanProbeConfigInventory(s.pool.QueryRow(ctx,
		`WITH probe_rows AS (`+probeConfigRowsSQL+`)
		 SELECT id, site, target, mesh, probe_type, interval_ms, timeout_ms,
		        train_count, train_spacing_ms, params, enabled, created_at,
		        updated_at, updated_by, network, mode, assignment, type_name
		   FROM probe_rows WHERE id = $4`,
		networks, typeIDs, typeNames, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFoundf("probe config %s does not exist", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped probe config: %w", err)
	}
	return &p, nil
}
