package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NetworkThreshold is one network_thresholds row: a per-plane overlay
// between the global dashboard_settings singleton and the per-site-pair
// overrides. Network is the plane's name. Nil metric fields inherit the
// global value — merging happens in httpapi, the SPA, and
// internal/server/thresholds, never here.
type NetworkThreshold struct {
	Network       string
	LatencyWarnUS *int64
	LatencyCritUS *int64
	LossWarnPct   *float64
	LossCritPct   *float64
	UpdatedAt     time.Time
	UpdatedBy     string
}

const networkThresholdValueColumns = `latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct, updated_at, updated_by`

func scanNetworkThreshold(row interface{ Scan(...any) error }) (*NetworkThreshold, error) {
	var t NetworkThreshold
	err := row.Scan(&t.Network,
		&t.LatencyWarnUS, &t.LatencyCritUS, &t.LossWarnPct, &t.LossCritPct,
		&t.UpdatedAt, &t.UpdatedBy)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListNetworkThresholds returns the per-network overlays, ordered by plane
// name. networks is the caller's network scope (nil = unfiltered): a scoped
// caller sees only its own planes' overlays — another tenant's idea of
// "normal" is not its business, and the row would name a plane it must not
// learn exists.
func (s *Store) ListNetworkThresholds(ctx context.Context, networks []uuid.UUID) ([]NetworkThreshold, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT n.name, `+networkThresholdValueColumns+`
		  FROM network_thresholds nt
		  JOIN networks n ON n.id = nt.network_id
		 WHERE $1::uuid[] IS NULL OR nt.network_id = ANY($1)
		 ORDER BY n.name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list network thresholds: %w", err)
	}
	defer rows.Close()

	var out []NetworkThreshold
	for rows.Next() {
		t, err := scanNetworkThreshold(rows)
		if err != nil {
			return nil, fmt.Errorf("list network thresholds: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// NetworkThresholdByID returns one plane's overlay, or nil when the plane
// has none. Ingest calls this once per assignment-cache refresh for the
// measuring agent's own network, so it stays a point query on the PK.
func (s *Store) NetworkThresholdByID(ctx context.Context, networkID uuid.UUID) (*NetworkThreshold, error) {
	t, err := scanNetworkThreshold(s.pool.QueryRow(ctx, `
		SELECT n.name, `+networkThresholdValueColumns+`
		  FROM network_thresholds nt
		  JOIN networks n ON n.id = nt.network_id
		 WHERE nt.network_id = $1`, networkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("network threshold %s: %w", networkID, err)
	}
	return t, nil
}

// UpsertNetworkThreshold stores one plane's overlay, replacing all four
// metric fields. scope is the caller's network scope (nil = unscoped): a
// plane outside it is ErrNotFound, byte-identical to a name that does not
// exist, so a tenant cannot probe for another's planes. The handler
// validates the effective tuple first; a CHECK violation here is a bug.
func (s *Store) UpsertNetworkThreshold(ctx context.Context, network string, t NetworkThreshold, scope []uuid.UUID) (*NetworkThreshold, error) {
	id, err := s.networkThresholdKey(ctx, network, scope)
	if err != nil {
		return nil, err
	}
	out, err := scanNetworkThreshold(s.pool.QueryRow(ctx, `
		WITH up AS (
		INSERT INTO network_thresholds
		       (network_id, latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (network_id) DO UPDATE
		   SET latency_warn_us = EXCLUDED.latency_warn_us,
		       latency_crit_us = EXCLUDED.latency_crit_us,
		       loss_warn_pct = EXCLUDED.loss_warn_pct,
		       loss_crit_pct = EXCLUDED.loss_crit_pct,
		       updated_at = now(),
		       updated_by = EXCLUDED.updated_by
		RETURNING network_id, `+networkThresholdValueColumns+`)
		SELECT n.name, `+networkThresholdValueColumns+`
		  FROM up JOIN networks n ON n.id = up.network_id`,
		id, t.LatencyWarnUS, t.LatencyCritUS, t.LossWarnPct, t.LossCritPct, t.UpdatedBy))
	if err != nil {
		return nil, fmt.Errorf("upsert network threshold %q: %w", network, err)
	}
	return out, nil
}

// DeleteNetworkThreshold clears one plane's overlay; absence is ErrNotFound
// so httpapi answers 404, matching the other config deletes.
func (s *Store) DeleteNetworkThreshold(ctx context.Context, network string, scope []uuid.UUID) error {
	id, err := s.networkThresholdKey(ctx, network, scope)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM network_thresholds WHERE network_id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete network threshold %q: %w", network, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("no threshold overlay for network %q", network)
	}
	return nil
}

// networkThresholdKey resolves a plane name to its id, refusing planes
// outside the caller's scope with the SAME error a nonexistent name
// produces. Scope is checked against the resolved id rather than the name,
// so the two failure modes cannot be told apart by timing either.
func (s *Store) networkThresholdKey(ctx context.Context, network string, scope []uuid.UUID) (uuid.UUID, error) {
	id, err := s.NetworkIDByName(ctx, network)
	if err != nil {
		return uuid.Nil, err
	}
	if scope != nil && !slices.Contains(scope, id) {
		return uuid.Nil, notFoundf("network %q does not exist", network)
	}
	return id, nil
}
