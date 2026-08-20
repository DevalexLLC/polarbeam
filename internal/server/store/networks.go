package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NetworkAdminInfo is a networks row as shown by the admin config surface:
// the row itself plus the reference counts that block a delete (and let the
// UI disable it with an explanation). ProbeCount covers direct probe rows
// only — mesh templates carry no network of their own and count via the
// mesh group.
type NetworkAdminInfo struct {
	ID          uuid.UUID
	Name        string
	DisplayName string
	CreatedAt   time.Time
	AgentCount  int64
	TokenCount  int64
	MeshCount   int64
	ProbeCount  int64
	// TargetCount is tenant-owned external targets. It joined the list with
	// 0019: targets.network_id is ON DELETE RESTRICT, so these block a
	// delete exactly as agents and meshes do, and a listing that omitted
	// them would let the UI offer a delete guaranteed to 409.
	TargetCount int64
}

// NetworkIDByName resolves a network name WITHOUT creating it. Networks are
// a trust statement ("agents on this name can reach each other") and are
// NEVER auto-created — a typo'd network on any surface must fail loudly,
// with no EnsureSite-style counterpart.
func (s *Store) NetworkIDByName(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM networks WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, notFoundf("network %q does not exist", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve network %q: %w", name, err)
	}
	return id, nil
}

// CreateNetwork explicitly creates a network. A collision with an existing
// name must fail loudly, not silently adopt the existing row.
func (s *Store) CreateNetwork(ctx context.Context, name, displayName string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO networks (name, display_name)
		VALUES ($1, $2)
		ON CONFLICT (name) DO NOTHING
		RETURNING id`,
		name, displayName).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, conflictf("network %q already exists", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create network %q: %w", name, err)
	}
	return id, nil
}

// UpdateNetwork sets a network's display name. The name itself is immutable
// everywhere (like a mesh's network): agents, tokens, and meshes reference
// networks by ID, but operators and configs reference them by name, and a
// rename would silently repoint that shared vocabulary.
func (s *Store) UpdateNetwork(ctx context.Context, name, displayName string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE networks SET display_name = $2 WHERE name = $1`, name, displayName)
	if err != nil {
		return fmt.Errorf("update network %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("network %q does not exist", name)
	}
	return nil
}

// ListNetworksConfig lists all networks with per-network reference counts.
// networks is the caller's network scope (nil = unfiltered): a scoped
// caller sees only its own planes — the network inventory must never
// enumerate other tenants.
func (s *Store) ListNetworksConfig(ctx context.Context, networks []uuid.UUID) ([]NetworkAdminInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT n.id, n.name, n.display_name, n.created_at,
		       (SELECT count(*) FROM agents a WHERE a.network_id = n.id),
		       (SELECT count(*) FROM join_tokens t WHERE t.network_id = n.id),
		       (SELECT count(*) FROM mesh_groups g WHERE g.network_id = n.id),
		       (SELECT count(*) FROM probe_configs pc WHERE pc.network_id = n.id),
		       (SELECT count(*) FROM targets tg WHERE tg.network_id = n.id)
		  FROM networks n
		 WHERE $1::uuid[] IS NULL OR n.id = ANY($1)
		 ORDER BY n.name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list networks config: %w", err)
	}
	defer rows.Close()

	var out []NetworkAdminInfo
	for rows.Next() {
		var ni NetworkAdminInfo
		if err := rows.Scan(&ni.ID, &ni.Name, &ni.DisplayName, &ni.CreatedAt,
			&ni.AgentCount, &ni.TokenCount, &ni.MeshCount, &ni.ProbeCount, &ni.TargetCount); err != nil {
			return nil, fmt.Errorf("list networks config: %w", err)
		}
		out = append(out, ni)
	}
	return out, rows.Err()
}

// DeleteNetwork removes a network that nothing references, deleting its
// UNUSED join tokens along the way (a used token always implies a live
// agent — there is no agent-delete path — so agents block the delete first
// and used_by_agent can never be orphaned). Referenced networks return
// ErrConflict naming every blocking count. The seeded 'default' network is
// never deletable: it is the fallback every empty --network input resolves
// to, and enrollment of pre-networks tokens depends on it existing.
//
// Statement order is load-bearing, exactly as in DeleteSite: EnrollAgent
// locks the join-token row FOR UPDATE and then touches the networks row
// (the agents insert takes FOR KEY SHARE on it via the FK), i.e. token →
// network. Deleting tokens BEFORE locking the network keeps this
// transaction in the same order; the network lock is plain FOR UPDATE
// precisely so it conflicts with that FK key-share — no enrollment can slip
// an agent in between the count below and the delete.
func (s *Store) DeleteNetwork(ctx context.Context, name string) (tokensDeleted int64, err error) {
	if name == "default" {
		return 0, invalidf("the 'default' network cannot be deleted (it is the seeded fallback for enrollment and admin writes)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}
	defer tx.Rollback(ctx)

	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM networks WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, notFoundf("network %q does not exist", name)
	}
	if err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM join_tokens WHERE network_id = $1 AND used_at IS NULL`, id)
	if err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}
	tokensDeleted = tag.RowsAffected()

	if _, err := tx.Exec(ctx, `SELECT 1 FROM networks WHERE id = $1 FOR UPDATE`, id); err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}

	// Tokens are counted again here, AFTER the lock: a token committed
	// between the sweep above and the lock acquisition would otherwise
	// surface as an FK violation on the network delete (opaque 500). A
	// plain count takes no row locks — a second DELETE sweep here would
	// invert the token → network lock order and deadlock against
	// EnrollAgent — and READ COMMITTED gives this statement a fresh
	// snapshot, so it sees that raced commit.
	//
	// targets joined the list in 0019: targets.network_id is ON DELETE
	// RESTRICT (a target is probe workload, not presentation config), so
	// without counting it here a tenant-owned target would surface as an
	// opaque FK violation instead of a refusal naming what blocks. Contrast
	// path_thresholds and network_thresholds, which cascade by design and
	// must NOT be counted — user_networks likewise (user scope must never
	// block operator topology changes).
	var agents, meshes, probes, tokens, targets int64
	err = tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM agents a WHERE a.network_id = $1),
		       (SELECT count(*) FROM mesh_groups g WHERE g.network_id = $1),
		       (SELECT count(*) FROM probe_configs pc WHERE pc.network_id = $1),
		       (SELECT count(*) FROM join_tokens t WHERE t.network_id = $1),
		       (SELECT count(*) FROM targets tg WHERE tg.network_id = $1)`,
		id).Scan(&agents, &meshes, &probes, &tokens, &targets)
	if err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}
	if agents > 0 || meshes > 0 || probes > 0 || tokens > 0 || targets > 0 {
		// Rollback restores the token rows deleted above.
		return 0, conflictf("network %q is referenced by %d agent(s), %d mesh group(s), %d probe config(s), %d join token(s), and %d target(s)",
			name, agents, meshes, probes, tokens, targets)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM networks WHERE id = $1`, id); err != nil {
		return 0, fmt.Errorf("delete network %q: %w", name, err)
	}
	return tokensDeleted, tx.Commit(ctx)
}
