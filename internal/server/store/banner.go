package store

import (
	"context"
	"fmt"
	"time"
)

// BannerSettings is the single banner_settings row: an optional shared
// marking text (e.g. "PROPRIETARY") the SPA renders in bands at the top and
// bottom of every screen, sign-in included.
type BannerSettings struct {
	Enabled   bool
	Text      string
	UpdatedAt time.Time
	UpdatedBy string
}

const bannerColumns = `enabled, text, updated_at, updated_by`

// GetBannerSettings returns the banner configuration. The row is seeded by
// migration, so absence is a real error, not a default case.
func (s *Store) GetBannerSettings(ctx context.Context) (*BannerSettings, error) {
	var b BannerSettings
	err := s.pool.QueryRow(ctx,
		`SELECT `+bannerColumns+` FROM banner_settings WHERE id`).
		Scan(&b.Enabled, &b.Text, &b.UpdatedAt, &b.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("get banner settings: %w", err)
	}
	return &b, nil
}

// UpdateBannerSettings replaces the banner configuration atomically and
// returns the stored row (updated_at comes from the DB clock, not the
// caller's). The handler validates first; a CHECK violation surfacing here
// is a bug and stays loud.
func (s *Store) UpdateBannerSettings(ctx context.Context, b BannerSettings) (*BannerSettings, error) {
	var out BannerSettings
	err := s.pool.QueryRow(ctx, `
		UPDATE banner_settings
		   SET enabled = $1, text = $2, updated_at = now(), updated_by = $3
		 WHERE id
		 RETURNING `+bannerColumns,
		b.Enabled, b.Text, b.UpdatedBy).
		Scan(&out.Enabled, &out.Text, &out.UpdatedAt, &out.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("update banner settings: %w", err)
	}
	return &out, nil
}
