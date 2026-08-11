package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// UserInfo is a users row. PasswordHash is "" for federated (OIDC) users —
// their password_hash column is NULL by CHECK constraint.
type UserInfo struct {
	ID           uuid.UUID
	Username     string
	PasswordHash string
	Role         string
	Disabled     bool
	CreatedAt    time.Time
	AuthSource   string // "local" or "oidc"
}

// SessionInfo is a sessions row joined with its user, as needed by the
// session middleware.
type SessionInfo struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Username   string
	Role       string
	CSRFToken  string
	ExpiresAt  time.Time
	LastUsedAt time.Time
}

// CreateUser inserts a dashboard user and returns its ID. The password hash
// is produced by the caller (internal/server/auth); role is admin|viewer.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role) VALUES ($1, $2, $3) RETURNING id`,
		username, passwordHash, role).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return uuid.Nil, fmt.Errorf("user %q already exists", username)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// GetUserByUsername returns the user or (nil, nil) when the username is
// unknown, so login can burn a dummy hash verification without branching on
// an error.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*UserInfo, error) {
	var u UserInfo
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, COALESCE(password_hash, ''), role, disabled, created_at, auth_source
		   FROM users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt, &u.AuthSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return &u, nil
}

// CreateSession stores a new session for userID. tokenHash is the sha256 of
// the cookie token; the cleartext never reaches the database.
func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, csrfToken, expiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSessionByTokenHash returns the live session for a token hash, or
// (nil, nil) when it is unknown, expired, or its user is disabled.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (*SessionInfo, error) {
	var si SessionInfo
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.user_id, u.username, u.role, s.csrf_token, s.expires_at, s.last_used_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.expires_at > now() AND NOT u.disabled`,
		tokenHash).
		Scan(&si.ID, &si.UserID, &si.Username, &si.Role, &si.CSRFToken, &si.ExpiresAt, &si.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &si, nil
}

// TouchSession records session activity (rate-limited by the caller).
func (s *Store) TouchSession(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSessionByTokenHash removes a session (logout). Deleting an unknown
// token is not an error: logout is idempotent.
func (s *Store) DeleteSessionByTokenHash(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteOIDCSessions signs out every federated user. Called when the
// configured provider identity (issuer/client) changes: sessions issued
// under the previous provider must not carry over to the new one. Local
// (break-glass) sessions are never touched.
func (s *Store) DeleteOIDCSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM sessions USING users
		 WHERE sessions.user_id = users.id AND users.auth_source = 'oidc'`)
	if err != nil {
		return 0, fmt.Errorf("delete oidc sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredSessions is opportunistic cleanup run from the login handler;
// expired rows are already invisible to lookups either way.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
