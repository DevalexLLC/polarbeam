package store

import (
	"context"
	"errors"
	"fmt"
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
	CreatedBy      string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	UsedAt         *time.Time
	UsedByAgent    *uuid.UUID
	UsedByHostname *string
}

// ListJoinTokens lists every join token, newest first. Rows are never
// expired away automatically — used rows are the enrollment audit trail,
// and stale unused rows are the operator's to delete.
func (s *Store) ListJoinTokens(ctx context.Context) ([]JoinTokenInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, s.name, t.created_by, t.created_at, t.expires_at,
		       t.used_at, t.used_by_agent, a.hostname
		  FROM join_tokens t
		  JOIN sites s ON s.id = t.site_id
		  LEFT JOIN agents a ON a.id = t.used_by_agent
		 ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list join tokens: %w", err)
	}
	defer rows.Close()

	var tokens []JoinTokenInfo
	for rows.Next() {
		var t JoinTokenInfo
		if err := rows.Scan(&t.ID, &t.Site, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt,
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
func (s *Store) DeleteJoinToken(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	var usedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT used_at FROM join_tokens WHERE id = $1 FOR UPDATE`, id).Scan(&usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("join token %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	if usedAt != nil {
		return conflictf("join token %s was used to enroll an agent and is kept as an audit record", id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM join_tokens WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete join token %s: %w", id, err)
	}
	return tx.Commit(ctx)
}
