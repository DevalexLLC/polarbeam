package store

import (
	"context"
	"fmt"
	"time"
)

// ThresholdSettings is the shared dashboard severity configuration — the
// single dashboard_settings row. The map view colors dots and lines warn/crit
// when a pair's headline latency or loss crosses these values.
type ThresholdSettings struct {
	LatencyWarnUS int64
	LatencyCritUS int64
	LossWarnPct   float64
	LossCritPct   float64
	UpdatedAt     time.Time
	UpdatedBy     string
}

const settingsColumns = `latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct, updated_at, updated_by`

// GetSettings returns the shared dashboard settings. The row is seeded by
// migration, so absence is a real error, not a default case.
func (s *Store) GetSettings(ctx context.Context) (*ThresholdSettings, error) {
	var ts ThresholdSettings
	err := s.pool.QueryRow(ctx,
		`SELECT `+settingsColumns+` FROM dashboard_settings WHERE id`).
		Scan(&ts.LatencyWarnUS, &ts.LatencyCritUS, &ts.LossWarnPct, &ts.LossCritPct,
			&ts.UpdatedAt, &ts.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("get settings: %w", err)
	}
	return &ts, nil
}

// UpdateSettings replaces the thresholds atomically and returns the stored
// row (updated_at comes from the DB clock, not the caller's). The handler
// validates first; a CHECK violation surfacing here is a bug and stays loud.
func (s *Store) UpdateSettings(ctx context.Context, ts ThresholdSettings) (*ThresholdSettings, error) {
	var out ThresholdSettings
	err := s.pool.QueryRow(ctx, `
		UPDATE dashboard_settings
		   SET latency_warn_us = $1, latency_crit_us = $2,
		       loss_warn_pct = $3, loss_crit_pct = $4,
		       updated_at = now(), updated_by = $5
		 WHERE id
		 RETURNING `+settingsColumns,
		ts.LatencyWarnUS, ts.LatencyCritUS, ts.LossWarnPct, ts.LossCritPct, ts.UpdatedBy).
		Scan(&out.LatencyWarnUS, &out.LatencyCritUS, &out.LossWarnPct, &out.LossCritPct,
			&out.UpdatedAt, &out.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("update settings: %w", err)
	}
	return &out, nil
}
