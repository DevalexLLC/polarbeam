package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OIDCRoleRule maps one role-claim value to a network-scoped role. Rules
// are evaluated after admin_values (which always wins): the strongest
// matched role wins (network_admin > network_viewer) and the networks of
// every rule granting it are unioned. Networks are names — the mapping is
// operator vocabulary, resolved to IDs at login time.
type OIDCRoleRule struct {
	Value    string   `json:"value"`
	Role     string   `json:"role"` // RoleNetworkAdmin | RoleNetworkViewer
	Networks []string `json:"networks"`
}

// OIDCSettings is the single oidc_settings row: the optional IdP
// configuration edited from Settings -> Authentication. ClientSecret is the
// stored cleartext; handlers must never echo it to clients. UnmatchedRole
// ("viewer" or "deny") decides what an authenticated user matching neither
// admin_values nor any rule becomes; multi-tenant installs set "deny",
// since a global viewer sees every plane.
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
	RoleRules     []OIDCRoleRule
	UnmatchedRole string
	CAPEM         string
	UpdatedAt     time.Time
	UpdatedBy     string
}

const oidcSettingsColumns = `enabled, issuer, client_id, client_secret, redirect_url,
	scopes, username_claim, role_claim, admin_values, role_rules, unmatched_role,
	ca_pem, updated_at, updated_by`

func scanOIDCSettings(row pgx.Row) (*OIDCSettings, error) {
	var o OIDCSettings
	err := row.Scan(&o.Enabled, &o.Issuer, &o.ClientID, &o.ClientSecret, &o.RedirectURL,
		&o.Scopes, &o.UsernameClaim, &o.RoleClaim, &o.AdminValues, &o.RoleRules,
		&o.UnmatchedRole, &o.CAPEM, &o.UpdatedAt, &o.UpdatedBy)
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

// roleRulesEqual compares two rule lists structurally (order-sensitive —
// the list is ordered configuration).
func roleRulesEqual(a, b []OIDCRoleRule) bool {
	return slices.EqualFunc(a, b, func(x, y OIDCRoleRule) bool {
		return x.Value == y.Value && x.Role == y.Role && slices.Equal(x.Networks, y.Networks)
	})
}

// UpdateOIDCSettings replaces the OIDC configuration atomically and returns
// the stored row. The keep flags implement the PUT "omitted means
// unchanged" conventions AGAINST THE LOCKED ROW: keepSecret preserves the
// stored client_secret (an empty secret in the request means "unchanged"),
// keepRoleRules/keepUnmatchedRole preserve the stored tenant policy fields
// (a pre-tenancy client never sends them). Resolving the kept values from
// the handler's earlier read instead would reintroduce the classic
// lost-update race: a stale request would write a superseded policy back —
// deny reverting to viewer — after a stricter one committed. The handler
// validates first, so the table CHECK firing here is a bug and stays loud.
//
// A provider switch (issuer or client_id differing from the STORED row) OR
// an authorization-policy change (role_claim, admin_values, role_rules,
// unmatched_role — every input of the claim→role mapping) additionally
// deletes every federated user's session in the same transaction: sessions
// join their role and network scope live from the users row, which only a
// LOGIN remaps, so without revocation an already signed-in global viewer
// would keep seeing every network for the session TTL after the operator
// turned on tenant isolation. The comparison is made against the row locked
// here — never against the handler's earlier read, which can be stale: with
// two writes based on the same snapshot A, one switches A→B, a B login
// commits, and the other writes A back while its snapshot comparison sees
// "unchanged" — its skipped revocation would leave the B-issued session
// alive under an A configuration. The row lock pairs with the share locks in
// CreateOIDCSession and UpsertOIDCUser: a login racing the change either
// commits first — and its session is deleted here — or blocks until this
// commits, re-reads the new updated_at, and fails. Either way no
// stale-policy session survives. Returns the number of sessions revoked.
func (s *Store) UpdateOIDCSettings(ctx context.Context, o OIDCSettings, keepSecret, keepRoleRules, keepUnmatchedRole bool) (*OIDCSettings, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock before deciding and deleting, not just at the UPDATE: otherwise
	// the delete could run while a racing CreateOIDCSession holds the share
	// lock, and that login's session — committed while our UPDATE waits —
	// would survive the revocation below.
	var curIssuer, curClientID, curSecret, curRoleClaim, curUnmatched string
	var curAdmins []string
	var curRules []OIDCRoleRule
	if err := tx.QueryRow(ctx,
		`SELECT issuer, client_id, client_secret, role_claim, admin_values, role_rules, unmatched_role
		   FROM oidc_settings WHERE id FOR UPDATE`).
		Scan(&curIssuer, &curClientID, &curSecret, &curRoleClaim, &curAdmins, &curRules, &curUnmatched); err != nil {
		return nil, 0, fmt.Errorf("update oidc settings: %w", err)
	}
	providerChanged := curIssuer != o.Issuer || curClientID != o.ClientID
	effRules := o.RoleRules
	if keepRoleRules {
		effRules = curRules
	}
	effUnmatched := o.UnmatchedRole
	if keepUnmatchedRole {
		effUnmatched = curUnmatched
	}
	policyChanged := curRoleClaim != o.RoleClaim ||
		!slices.Equal(curAdmins, o.AdminValues) ||
		!roleRulesEqual(curRules, effRules) || curUnmatched != effUnmatched
	if providerChanged && keepSecret && curSecret != "" {
		// The validation-layer rule (a provider switch demands a fresh
		// secret), re-checked against the authoritative row.
		return nil, 0, ErrConcurrentProviderChange
	}
	var revoked int64
	if providerChanged || policyChanged {
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
		       client_secret = CASE WHEN $14 THEN client_secret ELSE $4 END,
		       redirect_url = $5,
		       scopes = $6, username_claim = $7, role_claim = $8,
		       admin_values = $9,
		       role_rules = CASE WHEN $15 THEN role_rules ELSE $10 END,
		       unmatched_role = CASE WHEN $16 THEN unmatched_role ELSE $11 END,
		       ca_pem = $12,
		       updated_at = now(), updated_by = $13
		 WHERE id
		 RETURNING `+oidcSettingsColumns,
		o.Enabled, o.Issuer, o.ClientID, o.ClientSecret, o.RedirectURL,
		o.Scopes, o.UsernameClaim, o.RoleClaim, o.AdminValues, o.RoleRules,
		o.UnmatchedRole, o.CAPEM, o.UpdatedBy, keepSecret, keepRoleRules, keepUnmatchedRole))
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

// ErrOIDCPolicyChanged is returned by UpsertOIDCUser and CreateOIDCSession
// when the oidc_settings row has changed since the callback's claim mapping
// ran (updated_at mismatch): the mapped role and scope were derived from a
// superseded policy and must not be applied — the retried login remaps
// under the current one.
var ErrOIDCPolicyChanged = errors.New("oidc settings changed during login")

// CreateOIDCSession stores a session for a federated user, but only after
// re-checking — under a share lock on the settings row — that the provider
// that authenticated the user (issuer + client_id) is still the configured,
// enabled one AND that the settings revision the claims were mapped under
// (policyUpdatedAt) is still current. The callback holds its provider
// across the whole code exchange (seconds of IdP round-trips), so an admin
// can switch providers or rewrite the role mapping — revoking every SSO
// session — inside that window; an unchecked insert would then resurrect
// old-provider or old-policy access for a full session TTL. The share lock
// pairs with UpdateOIDCSettings's exclusive lock — see there.
func (s *Store) CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, issuer, clientID string, policyUpdatedAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	defer tx.Rollback(ctx)

	var enabled bool
	var curIssuer, curClientID string
	var curUpdatedAt time.Time
	if err := tx.QueryRow(ctx,
		`SELECT enabled, issuer, client_id, updated_at FROM oidc_settings WHERE id FOR SHARE`).
		Scan(&enabled, &curIssuer, &curClientID, &curUpdatedAt); err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	if !enabled || curIssuer != issuer || curClientID != clientID {
		return ErrProviderChanged
	}
	if !curUpdatedAt.Equal(policyUpdatedAt) {
		return ErrOIDCPolicyChanged
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
// Username, role, AND network scope track the IdP on every login (networks
// is the resolved scope for the network-scoped roles and must be empty for
// global ones — the claim mapping is the single source of truth, so the
// user_networks rows are replaced wholesale in the same transaction);
// disabled is deliberately never touched — it is the operator's revocation
// lever and must survive re-login attempts. A username squatted by another
// row gets one retry under a deterministic identity-derived suffix, so a
// colliding local name can never deny a federated login.
//
// policyUpdatedAt is the oidc_settings revision the role/networks were
// mapped under; the write is refused with ErrOIDCPolicyChanged when the row
// has moved on. The check runs under a share lock INSIDE the same
// transaction as the user write, so a callback that mapped claims under a
// superseded policy can never overwrite the role or scope a newer login
// (or the policy change's revocation) just established.
func (s *Store) UpsertOIDCUser(ctx context.Context, issuer, subject, username, role string, networks []uuid.UUID, policyUpdatedAt time.Time) (*UserInfo, error) {
	if issuer == "" || subject == "" {
		return nil, errors.New("upsert oidc user: empty issuer or subject")
	}
	if RoleIsNetworkScoped(role) == (len(networks) == 0) {
		// The callback validated the mapping; reaching here is a server bug.
		return nil, fmt.Errorf("upsert oidc user: role %s with %d networks", role, len(networks))
	}
	u, err := s.upsertOIDCUser(ctx, issuer, subject, username, role, networks, policyUpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_key" {
		sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
		u, err = s.upsertOIDCUser(ctx, issuer, subject, username+"-"+hex.EncodeToString(sum[:4]), role, networks, policyUpdatedAt)
	}
	if errors.Is(err, ErrOIDCPolicyChanged) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("upsert oidc user: %w", err)
	}
	return u, nil
}

func (s *Store) upsertOIDCUser(ctx context.Context, issuer, subject, username, role string, networks []uuid.UUID, policyUpdatedAt time.Time) (*UserInfo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var curUpdatedAt time.Time
	if err := tx.QueryRow(ctx,
		`SELECT updated_at FROM oidc_settings WHERE id FOR SHARE`).Scan(&curUpdatedAt); err != nil {
		return nil, err
	}
	if !curUpdatedAt.Equal(policyUpdatedAt) {
		return nil, ErrOIDCPolicyChanged
	}

	var u UserInfo
	err = tx.QueryRow(ctx, `
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
	// Replace, not merge: a group removed at the IdP must lose its plane
	// here on the next login, and a role change to a global one must clear
	// the scope entirely.
	if _, err := tx.Exec(ctx, `DELETE FROM user_networks WHERE user_id = $1`, u.ID); err != nil {
		return nil, err
	}
	if len(networks) > 0 {
		if err := insertUserNetworks(ctx, tx, u.ID, networks); err != nil {
			return nil, err
		}
	}
	return &u, tx.Commit(ctx)
}
