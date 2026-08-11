package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OIDCSettings is the single oidc_settings row: the optional IdP
// configuration edited from Settings -> Authentication. ClientSecret is the
// stored cleartext; handlers must never echo it to clients.
type OIDCSettings struct {
	Enabled       bool
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	UsernameClaim string
	RoleClaim     string
	AdminValues   []string
	CAPEM         string
	UpdatedAt     time.Time
	UpdatedBy     string
}

const oidcSettingsColumns = `enabled, issuer, client_id, client_secret, redirect_url,
	scopes, username_claim, role_claim, admin_values, ca_pem, updated_at, updated_by`

func scanOIDCSettings(row pgx.Row) (*OIDCSettings, error) {
	var o OIDCSettings
	err := row.Scan(&o.Enabled, &o.Issuer, &o.ClientID, &o.ClientSecret, &o.RedirectURL,
		&o.Scopes, &o.UsernameClaim, &o.RoleClaim, &o.AdminValues, &o.CAPEM,
		&o.UpdatedAt, &o.UpdatedBy)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// GetOIDCSettings returns the OIDC configuration. The row is seeded by
// migration, so absence is a real error, not a default case.
func (s *Store) GetOIDCSettings(ctx context.Context) (*OIDCSettings, error) {
	o, err := scanOIDCSettings(s.pool.QueryRow(ctx,
		`SELECT `+oidcSettingsColumns+` FROM oidc_settings WHERE id`))
	if err != nil {
		return nil, fmt.Errorf("get oidc settings: %w", err)
	}
	return o, nil
}

// ErrConcurrentProviderChange is returned by UpdateOIDCSettings when a
// request that keeps the stored client_secret turns out — against the locked
// row — to be switching providers: its validation ran against a snapshot
// another write has since replaced. Keeping the secret would hand one
// provider's credential to another; the caller must re-read and resubmit.
var ErrConcurrentProviderChange = errors.New("oidc settings changed concurrently; a new client_secret is required")

// UpdateOIDCSettings replaces the OIDC configuration atomically and returns
// the stored row. With keepSecret the stored client_secret survives (the PUT
// convention: an empty secret in the request means "unchanged"); the handler
// validates first, so the table CHECK firing here is a bug and stays loud.
// A provider switch (issuer or client_id differing from the STORED row)
// additionally deletes every federated user's session in the same
// transaction. The comparison is made against the row locked here — never
// against the handler's earlier read, which can be stale: with two writes
// based on the same snapshot A, one switches A→B, a B login commits, and the
// other writes A back while its snapshot comparison sees "unchanged" — its
// skipped revocation would leave the B-issued session alive under an A
// configuration. The row lock pairs with CreateOIDCSession's share lock: a
// login racing the switch either commits first — and its session is deleted
// here — or blocks until this commits, re-reads the new provider identity,
// and fails. Either way no old-provider session survives. Returns the number
// of sessions revoked.
func (s *Store) UpdateOIDCSettings(ctx context.Context, o OIDCSettings, keepSecret bool) (*OIDCSettings, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock before deciding and deleting, not just at the UPDATE: otherwise
	// the delete could run while a racing CreateOIDCSession holds the share
	// lock, and that login's session — committed while our UPDATE waits —
	// would survive the revocation below.
	var curIssuer, curClientID, curSecret string
	if err := tx.QueryRow(ctx,
		`SELECT issuer, client_id, client_secret FROM oidc_settings WHERE id FOR UPDATE`).
		Scan(&curIssuer, &curClientID, &curSecret); err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	providerChanged := curIssuer != o.Issuer || curClientID != o.ClientID
	if providerChanged && keepSecret && curSecret != "" {
		// The validation-layer rule (a provider switch demands a fresh
		// secret), re-checked against the authoritative row.
		return nil, 0, ErrConcurrentProviderChange
	}
	var revoked int64
	if providerChanged {
		tag, err := tx.Exec(ctx, `
			DELETE FROM sessions USING users
			 WHERE sessions.user_id = users.id AND users.auth_source = 'oidc'`)
		if err != nil {
			return nil, 0, fmt.Errorf("update oidc settings: revoke sso sessions: %w", err)
		}
		revoked = tag.RowsAffected()
	}
	out, err := scanOIDCSettings(tx.QueryRow(ctx, `
		UPDATE oidc_settings
		   SET enabled = $1, issuer = $2, client_id = $3,
		       client_secret = CASE WHEN $12 THEN client_secret ELSE $4 END,
		       redirect_url = $5,
		       scopes = $6, username_claim = $7, role_claim = $8,
		       admin_values = $9, ca_pem = $10,
		       updated_at = now(), updated_by = $11
		 WHERE id
		 RETURNING `+oidcSettingsColumns,
		o.Enabled, o.Issuer, o.ClientID, o.ClientSecret, o.RedirectURL,
		o.Scopes, o.UsernameClaim, o.RoleClaim, o.AdminValues, o.CAPEM,
		o.UpdatedBy, keepSecret))
	if err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	return out, revoked, nil
}

// ErrProviderChanged is returned by CreateOIDCSession when the configured
// provider is no longer the (enabled) one that authenticated the user.
var ErrProviderChanged = errors.New("oidc provider configuration changed during login")

// CreateOIDCSession stores a session for a federated user, but only after
// re-checking — under a share lock on the settings row — that the provider
// that authenticated the user (issuer + client_id) is still the configured,
// enabled one. The callback holds its provider across the whole code
// exchange (seconds of IdP round-trips), so an admin can switch providers
// and revoke every SSO session inside that window; an unchecked insert would
// then resurrect old-provider access for a full session TTL. The share lock
// pairs with UpdateOIDCSettings's exclusive lock — see there.
func (s *Store) CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, issuer, clientID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	defer tx.Rollback(ctx)

	var enabled bool
	var curIssuer, curClientID string
	if err := tx.QueryRow(ctx,
		`SELECT enabled, issuer, client_id FROM oidc_settings WHERE id FOR SHARE`).
		Scan(&enabled, &curIssuer, &curClientID); err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	if !enabled || curIssuer != issuer || curClientID != clientID {
		return ErrProviderChanged
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, csrfToken, expiresAt); err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	return tx.Commit(ctx)
}

// UpsertOIDCUser JIT-provisions (or refreshes) a federated user keyed on the
// pair (issuer, subject) — subjects are unique only within an issuer, so the
// bare subject would let a re-pointed provider merge unrelated accounts.
// Username and role track the IdP on every login; disabled is deliberately
// never touched — it is the operator's revocation lever and must survive
// re-login attempts. A username squatted by another row gets one retry under
// a deterministic identity-derived suffix, so a colliding local name can
// never deny a federated login.
func (s *Store) UpsertOIDCUser(ctx context.Context, issuer, subject, username, role string) (*UserInfo, error) {
	if issuer == "" || subject == "" {
		return nil, errors.New("upsert oidc user: empty issuer or subject")
	}
	u, err := s.upsertOIDCUser(ctx, issuer, subject, username, role)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_key" {
		sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
		u, err = s.upsertOIDCUser(ctx, issuer, subject, username+"-"+hex.EncodeToString(sum[:4]), role)
	}
	if err != nil {
		return nil, fmt.Errorf("upsert oidc user: %w", err)
	}
	return u, nil
}

func (s *Store) upsertOIDCUser(ctx context.Context, issuer, subject, username, role string) (*UserInfo, error) {
	var u UserInfo
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, role, auth_source, oidc_issuer, oidc_subject)
		VALUES ($1, NULL, $2, 'oidc', $3, $4)
		ON CONFLICT (oidc_issuer, oidc_subject) WHERE oidc_subject IS NOT NULL
		DO UPDATE SET username = EXCLUDED.username, role = EXCLUDED.role
		RETURNING id, username, COALESCE(password_hash, ''), role, disabled, created_at, auth_source`,
		username, role, issuer, subject).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt, &u.AuthSource)
	if err != nil {
		return nil, err
	}
	return &u, nil
}
