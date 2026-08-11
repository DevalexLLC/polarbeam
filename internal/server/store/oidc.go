package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

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

// UpdateOIDCSettings replaces the OIDC configuration atomically and returns
// the stored row. With keepSecret the stored client_secret survives (the PUT
// convention: an empty secret in the request means "unchanged"); the handler
// validates first, so the table CHECK firing here is a bug and stays loud.
func (s *Store) UpdateOIDCSettings(ctx context.Context, o OIDCSettings, keepSecret bool) (*OIDCSettings, error) {
	out, err := scanOIDCSettings(s.pool.QueryRow(ctx, `
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
		return nil, fmt.Errorf("update oidc settings: %w", err)
	}
	return out, nil
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
