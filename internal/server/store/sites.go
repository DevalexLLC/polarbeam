package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SiteUpdate carries optional site field updates; nil = leave unchanged.
// Latitude and Longitude must be set together (the DB CHECK enforces
// both-or-neither, and callers validate before calling). ClearCoords resets
// both to NULL (unplaced) and cannot be combined with setting them.
type SiteUpdate struct {
	Latitude    *float64
	Longitude   *float64
	DisplayName *string
	Location    *string
	ClearCoords bool
}

// UpdateSite updates an existing site by name. Unknown names are an error —
// admin commands never auto-create sites (a typo'd --site must fail loudly),
// unlike token creation's EnsureSite.
func (s *Store) UpdateSite(ctx context.Context, name string, up SiteUpdate) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sites
		   SET latitude     = CASE WHEN $6 THEN NULL ELSE COALESCE($2, latitude) END,
		       longitude    = CASE WHEN $6 THEN NULL ELSE COALESCE($3, longitude) END,
		       display_name = COALESCE($4, display_name),
		       location     = COALESCE($5, location)
		 WHERE name = $1`,
		name, up.Latitude, up.Longitude, up.DisplayName, up.Location, up.ClearCoords)
	if err != nil {
		return fmt.Errorf("update site %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("site %q does not exist", name)
	}
	return nil
}

// CreateSite explicitly creates a site with optional metadata. Deliberately
// not EnsureSite: an admin create colliding with an existing name must fail
// loudly, not silently adopt the existing row.
func (s *Store) CreateSite(ctx context.Context, name string, up SiteUpdate) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sites (name, display_name, location, latitude, longitude)
		VALUES ($1, COALESCE($2, ''), COALESCE($3, ''), $4, $5)
		ON CONFLICT (name) DO NOTHING
		RETURNING id`,
		name, up.DisplayName, up.Location, up.Latitude, up.Longitude).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, conflictf("site %q already exists", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create site %q: %w", name, err)
	}
	return id, nil
}

// SiteAdminInfo is a sites row as shown by the admin config surface: the
// dashboard fields plus creation time and the reference counts that block a
// delete (and let the UI disable it with an explanation).
type SiteAdminInfo struct {
	SiteInfo
	CreatedAt  time.Time
	AgentCount int64
	MeshCount  int64
	ProbeCount int64
}

// ListSitesConfig lists all sites with per-site reference counts. The
// dashboard's ListSites stays separate so the map/matrix payload shape is
// stable. networks is the caller's network scope (nil = unfiltered):
// visibility follows siteScopePredicate, and every count describes only the
// caller's planes. Site NAMES are shared operator vocabulary, but the
// counts are tenant activity — a shared site reporting server-wide agent,
// mesh, and probe totals would tell one tenant how large another's
// footprint there is. Only direct probe rows carry site_id (mesh templates
// keep it NULL by CHECK), so the probe predicate needs no mesh join; mesh
// membership takes its plane from the owning group.
func (s *Store) ListSitesConfig(ctx context.Context, networks []uuid.UUID) ([]SiteAdminInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.name, s.display_name, s.location, s.latitude, s.longitude, s.created_at,
		       (SELECT count(*) FROM agents a
		         WHERE a.site_id = s.id
		           AND ($1::uuid[] IS NULL OR a.network_id = ANY($1))),
		       (SELECT count(*) FROM mesh_members mm
		          JOIN mesh_groups mg ON mg.id = mm.mesh_id
		         WHERE mm.site_id = s.id
		           AND ($1::uuid[] IS NULL OR mg.network_id = ANY($1))),
		       (SELECT count(*) FROM probe_configs pc
		         WHERE pc.site_id = s.id
		           AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1)))
		  FROM sites s
		 WHERE `+siteScopePredicate("s.id", "$1")+`
		 ORDER BY s.name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list sites config: %w", err)
	}
	defer rows.Close()

	var sites []SiteAdminInfo
	for rows.Next() {
		var si SiteAdminInfo
		if err := rows.Scan(&si.ID, &si.Name, &si.DisplayName, &si.Location,
			&si.Latitude, &si.Longitude, &si.CreatedAt,
			&si.AgentCount, &si.MeshCount, &si.ProbeCount); err != nil {
			return nil, fmt.Errorf("list sites config: %w", err)
		}
		sites = append(sites, si)
	}
	return sites, rows.Err()
}

// DeleteSite removes a site that nothing references, deleting its UNUSED
// join tokens along the way (a used token always implies a live agent —
// there is no agent-delete path — so agents block the delete first and
// used_by_agent can never be orphaned). Referenced sites return ErrConflict
// naming every blocking count. path_thresholds rows are deliberately NOT
// counted: they are presentation config and cascade with the site.
//
// Statement order is load-bearing: EnrollAgent locks the join-token row FOR
// UPDATE and then touches the site row (the agents insert takes FOR KEY
// SHARE on it via the FK), i.e. token → site. Deleting tokens BEFORE locking
// the site keeps this transaction in the same token → site order; locking
// the site first would deadlock against an in-flight enrollment. The site
// lock is plain FOR UPDATE (not NO KEY UPDATE) precisely so it conflicts
// with that FK key-share — no enrollment can slip an agent in between the
// count below and the delete.
func (s *Store) DeleteSite(ctx context.Context, name string) (tokensDeleted int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}
	defer tx.Rollback(ctx)

	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM sites WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, notFoundf("site %q does not exist", name)
	}
	if err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM join_tokens WHERE site_id = $1 AND used_at IS NULL`, id)
	if err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}
	tokensDeleted = tag.RowsAffected()

	if _, err := tx.Exec(ctx, `SELECT 1 FROM sites WHERE id = $1 FOR UPDATE`, id); err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}

	// Tokens are counted again here, AFTER the lock: a token committed
	// between the sweep above and the lock acquisition would otherwise
	// surface as an FK violation on the site delete (opaque 500). A plain
	// count takes no row locks — a second DELETE sweep here would invert
	// the token → site lock order and deadlock against EnrollAgent — and
	// READ COMMITTED gives this statement a fresh snapshot, so it sees
	// that raced commit. Any remaining token (raced unused, or used) makes
	// the delete a conflict; used ones imply agents, which block first.
	var agents, meshes, probes, tokens int64
	err = tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM agents a WHERE a.site_id = $1),
		       (SELECT count(*) FROM mesh_members mm WHERE mm.site_id = $1),
		       (SELECT count(*) FROM probe_configs pc WHERE pc.site_id = $1),
		       (SELECT count(*) FROM join_tokens t WHERE t.site_id = $1)`,
		id).Scan(&agents, &meshes, &probes, &tokens)
	if err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}
	if agents > 0 || meshes > 0 || probes > 0 || tokens > 0 {
		// Rollback restores the token rows deleted above.
		return 0, conflictf("site %q is referenced by %d agent(s), %d mesh membership(s), %d probe config(s), and %d join token(s)",
			name, agents, meshes, probes, tokens)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM sites WHERE id = $1`, id); err != nil {
		return 0, fmt.Errorf("delete site %q: %w", name, err)
	}
	return tokensDeleted, tx.Commit(ctx)
}
