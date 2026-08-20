package store

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PathThresholdOverride is one path_thresholds row: per-site-pair threshold
// overrides keyed on the unordered pair. A and B are site names, A < B
// lexically (the DB canonicalizes by uuid order; names are sorted here only
// for display). Nil metric fields inherit the global dashboard_settings
// value — merging happens in httpapi, the SPA, and
// internal/server/thresholds, never here.
type PathThresholdOverride struct {
	A             string
	B             string
	LatencyWarnUS *int64
	LatencyCritUS *int64
	LossWarnPct   *float64
	LossCritPct   *float64
	UpdatedAt     time.Time
	UpdatedBy     string
}

const pathThresholdValueColumns = `latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct, updated_at, updated_by`

func scanPathThreshold(row interface{ Scan(...any) error }) (*PathThresholdOverride, error) {
	var o PathThresholdOverride
	err := row.Scan(&o.A, &o.B,
		&o.LatencyWarnUS, &o.LatencyCritUS, &o.LossWarnPct, &o.LossCritPct,
		&o.UpdatedAt, &o.UpdatedBy)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ListPathThresholds returns every override, pair names lexically ordered
// within each row and rows sorted by pair. networks is the caller's network
// scope (nil = unfiltered): a scoped caller sees only overrides whose BOTH
// sites are visible under siteScopePredicate — override rows carry site
// names, which must never leak across tenants.
func (s *Store) ListPathThresholds(ctx context.Context, networks []uuid.UUID) ([]PathThresholdOverride, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT least(sa.name, sb.name), greatest(sa.name, sb.name), `+pathThresholdValueColumns+`
		  FROM path_thresholds pt
		  JOIN sites sa ON sa.id = pt.site_a_id
		  JOIN sites sb ON sb.id = pt.site_b_id
		 WHERE `+siteScopePredicate("pt.site_a_id", "$1")+`
		   AND `+siteScopePredicate("pt.site_b_id", "$1")+`
		 ORDER BY 1, 2`, networks)
	if err != nil {
		return nil, fmt.Errorf("list path thresholds: %w", err)
	}
	defer rows.Close()

	var out []PathThresholdOverride
	for rows.Next() {
		o, err := scanPathThreshold(rows)
		if err != nil {
			return nil, fmt.Errorf("list path thresholds: %w", err)
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// PathThresholdPair is one path_thresholds row keyed by site IDs in the
// table's canonical (uuid bytewise) order — the ingest-side consumer needs
// no names and must not pay the sites joins.
type PathThresholdPair struct {
	SiteAID       uuid.UUID
	SiteBID       uuid.UUID
	LatencyWarnUS *int64
	LatencyCritUS *int64
	LossWarnPct   *float64
	LossCritPct   *float64
}

// PathThresholdPairs returns the overrides involving one site, keyed by
// canonical site-ID pair. Ingest resolves thresholds per source agent, and
// only pairs containing the agent's site can apply — an unfiltered load
// would be O(agents × all overrides) every cache refresh. The PK prefix
// serves the a-side arm, path_thresholds_b_idx the b-side.
func (s *Store) PathThresholdPairs(ctx context.Context, siteID uuid.UUID) ([]PathThresholdPair, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT site_a_id, site_b_id, latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct
		  FROM path_thresholds
		 WHERE site_a_id = $1 OR site_b_id = $1`, siteID)
	if err != nil {
		return nil, fmt.Errorf("path threshold pairs: %w", err)
	}
	defer rows.Close()

	var out []PathThresholdPair
	for rows.Next() {
		var p PathThresholdPair
		if err := rows.Scan(&p.SiteAID, &p.SiteBID,
			&p.LatencyWarnUS, &p.LatencyCritUS, &p.LossWarnPct, &p.LossCritPct); err != nil {
			return nil, fmt.Errorf("path threshold pairs: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pathThresholdKey resolves both site names and returns their ids in the
// table's canonical (uuid bytewise) order. Same-site pairs are rejected in
// httpapi before this runs; an equal pair here would hit the CHECK loudly.
func (s *Store) pathThresholdKey(ctx context.Context, siteA, siteB string) (uuid.UUID, uuid.UUID, error) {
	idA, err := s.SiteIDByName(ctx, siteA)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	idB, err := s.SiteIDByName(ctx, siteB)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if bytes.Compare(idA[:], idB[:]) > 0 {
		idA, idB = idB, idA
	}
	return idA, idB, nil
}

// UpsertPathThreshold stores the override for the unordered pair
// (siteA, siteB), replacing all four metric fields (the SPA form always
// submits the full set; there is no partial-update path). The handler
// validates first; a CHECK violation surfacing here is a bug and stays loud.
func (s *Store) UpsertPathThreshold(ctx context.Context, siteA, siteB string, o PathThresholdOverride) (*PathThresholdOverride, error) {
	idA, idB, err := s.pathThresholdKey(ctx, siteA, siteB)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO path_thresholds
		       (site_a_id, site_b_id, latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (site_a_id, site_b_id) DO UPDATE
		   SET latency_warn_us = EXCLUDED.latency_warn_us,
		       latency_crit_us = EXCLUDED.latency_crit_us,
		       loss_warn_pct = EXCLUDED.loss_warn_pct,
		       loss_crit_pct = EXCLUDED.loss_crit_pct,
		       updated_at = now(),
		       updated_by = EXCLUDED.updated_by
		RETURNING least($8::text, $9::text), greatest($8::text, $9::text), `+pathThresholdValueColumns,
		idA, idB, o.LatencyWarnUS, o.LatencyCritUS, o.LossWarnPct, o.LossCritPct, o.UpdatedBy,
		siteA, siteB)
	out, err := scanPathThreshold(row)
	if err != nil {
		return nil, fmt.Errorf("upsert path threshold %s/%s: %w", siteA, siteB, err)
	}
	return out, nil
}

// DeletePathThreshold removes the override for the unordered pair; absence
// is ErrNotFound so httpapi answers 404, matching the other config deletes.
func (s *Store) DeletePathThreshold(ctx context.Context, siteA, siteB string) error {
	idA, idB, err := s.pathThresholdKey(ctx, siteA, siteB)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM path_thresholds WHERE site_a_id = $1 AND site_b_id = $2`, idA, idB)
	if err != nil {
		return fmt.Errorf("delete path threshold %s/%s: %w", siteA, siteB, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("no threshold override for %s and %s", siteA, siteB)
	}
	return nil
}
