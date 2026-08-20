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

// JoinTokenInfo is a join_tokens row joined with its site (and, when used,
// the consuming agent's hostname) for the admin token list. The secret is
// never readable back — only its hash is stored.
type JoinTokenInfo struct {
	ID             uuid.UUID
	Site           string
	Network        string
	CreatedBy      string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	UsedAt         *time.Time
	UsedByAgent    *uuid.UUID
	UsedByHostname *string
}

// ListJoinTokens lists join tokens, newest first. Rows are never expired
// away automatically — used rows are the enrollment audit trail, and stale
// unused rows are the operator's to delete.
//
// networks is the caller's network scope (nil = unfiltered). A token states
// the plane its agent will land on, so scoping is a plain predicate on the
// token's own network_id — a tenant admin must see the tokens it may
// create, and must not see a co-tenant's pending enrollments.
func (s *Store) ListJoinTokens(ctx context.Context, networks []uuid.UUID) ([]JoinTokenInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, s.name, n.name, t.created_by, t.created_at, t.expires_at,
		       t.used_at, t.used_by_agent, a.hostname
		  FROM join_tokens t
		  JOIN sites s ON s.id = t.site_id
		  JOIN networks n ON n.id = t.network_id
		  LEFT JOIN agents a ON a.id = t.used_by_agent
		 WHERE $1::uuid[] IS NULL OR t.network_id = ANY($1)
		 ORDER BY t.created_at DESC`, networks)
	if err != nil {
		return nil, fmt.Errorf("list join tokens: %w", err)
	}
	defer rows.Close()

	var tokens []JoinTokenInfo
	for rows.Next() {
		var t JoinTokenInfo
		if err := rows.Scan(&t.ID, &t.Site, &t.Network, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt,
			&t.UsedAt, &t.UsedByAgent, &t.UsedByHostname); err != nil {
			return nil, fmt.Errorf("list join tokens: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// DeleteJoinToken hard-deletes an UNUSED join token. Delete IS revocation:
// EnrollAgent selects the row by id, so a missing row is ErrTokenInvalid.
// Used tokens are refused — they are the enrollment audit record, back the
// idempotent-replay path for lost enroll responses, and their used_by_agent
// FK pins the agent row. The FOR UPDATE serializes against EnrollAgent's
// own FOR UPDATE on the same row, so a delete racing an enrollment resolves
// to either a clean conflict or a clean revocation, never a half state.
//
// scope is the caller's network scope (nil = unscoped): a token on another
// plane is ErrNotFound, indistinguishable from an id that never existed.
// The scope check reads the row under the same FOR UPDATE as the used_at
// check, so it cannot be raced by a concurrent enrollment.
func (s *Store) DeleteJoinToken(ctx context.Context, id uuid.UUID, scope []uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	var (
		usedAt    *time.Time
		networkID uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT used_at, network_id FROM join_tokens WHERE id = $1 FOR UPDATE`,
		id).Scan(&usedAt, &networkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("join token %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	if scope != nil && !slices.Contains(scope, networkID) {
		return notFoundf("join token %s does not exist", id)
	}
	if usedAt != nil {
		return conflictf("join token %s was used to enroll an agent and is kept as an audit record", id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM join_tokens WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	return tx.Commit(ctx)
}
