package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// UserInfo is a users row. PasswordHash is "" for federated (OIDC) users —
// their password_hash column is NULL by CHECK constraint. Networks holds
// the user's allowed network names for the scoped roles and stays nil for
// global roles (see SessionInfo for the semantics).
type UserInfo struct {
	ID           uuid.UUID
	Username     string
	PasswordHash string
	Role         string
	Disabled     bool
	CreatedAt    time.Time
	AuthSource   string // "local" or "oidc"
	Networks     []string
}

// NetworkRef names one network a scoped user may see.
type NetworkRef struct {
	ID   uuid.UUID
	Name string
}

// SessionInfo is a sessions row joined with its user, as needed by the
// session middleware. Networks is nil for the global roles (no filtering);
// for the network-scoped roles it holds the user's allowed planes, where a
// non-nil empty set means "sees nothing" — scope always fails closed.
type SessionInfo struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Username   string
	Role       string
	AuthSource string // "local" or "oidc"
	CSRFToken  string
	ExpiresAt  time.Time
	LastUsedAt time.Time
	Networks   []NetworkRef
}

// NetworkScope returns (nil, false) for global roles — no filtering — and
// (allowed planes, true) for the scoped roles. Every read-path enforcement
// point resolves the session's visibility through this one helper.
func (s *SessionInfo) NetworkScope() ([]NetworkRef, bool) {
	if !RoleIsNetworkScoped(s.Role) {
		return nil, false
	}
	// Non-nil even when empty: callers pass the result to `= ANY($n)`
	// predicates, where the empty set matches nothing (fails closed) while
	// nil means unfiltered.
	if s.Networks == nil {
		return []NetworkRef{}, true
	}
	return s.Networks, true
}

// CreateUser inserts a dashboard user and returns its ID. The password hash
// is produced by the caller (internal/server/auth). networks is the scope
// for the network-scoped roles (required non-empty — a scoped user with no
// planes sees nothing and is always a mistake at create time) and must be
// empty for global roles; callers resolve names to IDs first, so unknown
// networks fail loudly before this runs.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string, networks []uuid.UUID) (uuid.UUID, error) {
	if !ValidRole(role) {
		return uuid.Nil, invalidf("role %q is not a valid role", role)
	}
	if RoleIsNetworkScoped(role) && len(networks) == 0 {
		return uuid.Nil, invalidf("role %s requires at least one network", role)
	}
	if !RoleIsNetworkScoped(role) && len(networks) > 0 {
		return uuid.Nil, invalidf("role %s is global and cannot be limited to networks", role)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create user: %w", err)
	}
	defer tx.Rollback(ctx)

	var id uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role) VALUES ($1, $2, $3) RETURNING id`,
		username, passwordHash, role).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return uuid.Nil, conflictf("user %q already exists", username)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create user: %w", err)
	}
	if len(networks) > 0 {
		if err := insertUserNetworks(ctx, tx, id, networks); err != nil {
			return uuid.Nil, fmt.Errorf("create user: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// insertUserNetworks writes a user's scope rows. A deleted network surfaces
// as a typed 404 (the caller resolved the ID moments ago), never an opaque
// FK 500.
func insertUserNetworks(ctx context.Context, tx pgx.Tx, userID uuid.UUID, networks []uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO user_networks (user_id, network_id)
		SELECT $1, unnest($2::uuid[])
		ON CONFLICT DO NOTHING`, userID, networks)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return notFoundf("a referenced network no longer exists")
	}
	return err
}

// SetUserNetworks replaces a user's network scope. Only local users with a
// network-scoped role qualify: global roles have no scope by definition,
// and a federated user's scope is derived from the IdP's claims on every
// login — editing it here would be silently overwritten at the next
// sign-in, so it is refused instead.
func (s *Store) SetUserNetworks(ctx context.Context, id uuid.UUID, networks []uuid.UUID) error {
	if len(networks) == 0 {
		return invalidf("at least one network is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set user networks %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	var role, authSource string
	err = tx.QueryRow(ctx,
		`SELECT role, auth_source FROM users WHERE id = $1 FOR UPDATE`, id).
		Scan(&role, &authSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("user %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("set user networks %s: %w", id, err)
	}
	if !RoleIsNetworkScoped(role) {
		return conflictf("role %s is global and cannot be limited to networks", role)
	}
	if authSource != "local" {
		return conflictf("federated accounts derive their networks from the identity provider on every login")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_networks WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("set user networks %s: %w", id, err)
	}
	if err := insertUserNetworks(ctx, tx, id, networks); err != nil {
		return fmt.Errorf("set user networks %s: %w", id, err)
	}
	return tx.Commit(ctx)
}

// lockUserAndActiveAdmins locks the target row AND every enabled admin row
// FOR UPDATE inside tx, then reports the target's state and how many OTHER
// enabled admins exist. Locking the whole enabled-admin set (not just the
// target) is what makes the last-admin guard race-proof: two admins
// concurrently disabling each other would otherwise each count the
// still-uncommitted caller and both succeed (write skew under READ
// COMMITTED). The ORDER BY gives concurrent transactions one lock order,
// so they serialize instead of deadlocking.
func lockUserAndActiveAdmins(ctx context.Context, tx pgx.Tx, id uuid.UUID) (role string, disabled bool, otherAdmins int64, err error) {
	rows, err := tx.Query(ctx, `
		SELECT id, role, disabled FROM users
		 WHERE id = $1 OR (role = 'admin' AND NOT disabled)
		 ORDER BY id
		 FOR UPDATE`, id)
	if err != nil {
		return "", false, 0, err
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var rowID uuid.UUID
		var rowRole string
		var rowDisabled bool
		if err := rows.Scan(&rowID, &rowRole, &rowDisabled); err != nil {
			return "", false, 0, err
		}
		if rowID == id {
			found, role, disabled = true, rowRole, rowDisabled
		} else if rowRole == "admin" && !rowDisabled {
			otherAdmins++
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, 0, err
	}
	if !found {
		return "", false, 0, notFoundf("user %s does not exist", id)
	}
	return role, disabled, otherAdmins, nil
}

// SetUserDisabled flips a user's disabled flag. Disabling the last enabled
// admin is refused — recovery would need container CLI access. Disabled
// users lose their sessions on their next request (session lookups check
// the flag); enabling is always allowed.
func (s *Store) SetUserDisabled(ctx context.Context, id uuid.UUID, disabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set user disabled %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	role, cur, otherAdmins, err := lockUserAndActiveAdmins(ctx, tx, id)
	if err != nil {
		return err
	}
	if disabled && !cur && role == "admin" && otherAdmins == 0 {
		return conflictf("cannot disable the last enabled admin")
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET disabled = $2 WHERE id = $1`, id, disabled); err != nil {
		return fmt.Errorf("set user disabled %s: %w", id, err)
	}
	return tx.Commit(ctx)
}

// DeleteUser removes a user; their sessions cascade away immediately and
// their sign-in history remains as a deleted identity (login_events has no
// FK by design). Deleting the last enabled admin is refused, same as
// disabling it. Deleting an SSO user does NOT revoke IdP access — a still-
// authorized user is JIT-provisioned a fresh account on next login;
// disabling is the revocation lever.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	role, disabled, otherAdmins, err := lockUserAndActiveAdmins(ctx, tx, id)
	if err != nil {
		return err
	}
	if role == "admin" && !disabled && otherAdmins == 0 {
		return conflictf("cannot delete the last enabled admin")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return tx.Commit(ctx)
}

// userNetworkNames is the subquery loading a user's scope as sorted network
// names ('{}' when none). Cheap for the global roles (an empty index probe)
// and always consistent with the row it rides along with.
const userNetworkNames = `COALESCE((SELECT array_agg(n.name ORDER BY n.name)
	FROM user_networks un JOIN networks n ON n.id = un.network_id
	WHERE un.user_id = users.id), '{}')`

// scopeNames normalizes a loaded name array against the role: global roles
// carry nil (unfiltered), scoped roles keep the loaded set (possibly empty
// — which fails closed to "sees nothing").
func scopeNames(role string, names []string) []string {
	if !RoleIsNetworkScoped(role) {
		return nil
	}
	if names == nil {
		return []string{}
	}
	return names
}

// GetUserByUsername returns the user or (nil, nil) when the username is
// unknown, so login can burn a dummy hash verification without branching on
// an error.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*UserInfo, error) {
	var u UserInfo
	var networks []string
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, COALESCE(password_hash, ''), role, disabled, created_at, auth_source, `+userNetworkNames+`
		   FROM users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt, &u.AuthSource, &networks)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.Networks = scopeNames(u.Role, networks)
	return &u, nil
}

// GetUserByID returns the user or (nil, nil) when the id is unknown, mirroring
// GetUserByUsername. The self-service password change uses it to fetch the
// caller's current hash.
func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (*UserInfo, error) {
	var u UserInfo
	var networks []string
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, COALESCE(password_hash, ''), role, disabled, created_at, auth_source, `+userNetworkNames+`
		   FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt, &u.AuthSource, &networks)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.Networks = scopeNames(u.Role, networks)
	return &u, nil
}

// ResetLocalUserPassword replaces a local user's password hash and deletes
// ALL of their sessions (the old credential is presumed lost or leaked) in
// one transaction, returning the username and role for the reveal response.
// Federated accounts are refused explicitly — an unguarded UPDATE would trip
// the users_auth_shape constraint and surface as an opaque 500. A deleted
// identity (login_events row with no users row) lands on the not-found path.
// The disabled flag is deliberately untouched: enable is a separate lever.
func (s *Store) ResetLocalUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) (username, role string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("reset password %s: %w", id, err)
	}
	defer tx.Rollback(ctx)

	var authSource string
	err = tx.QueryRow(ctx,
		`SELECT username, role, auth_source FROM users WHERE id = $1 FOR UPDATE`, id).
		Scan(&username, &role, &authSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", notFoundf("user %s does not exist", id)
	}
	if err != nil {
		return "", "", fmt.Errorf("reset password %s: %w", id, err)
	}
	if authSource != "local" {
		return "", "", conflictf("federated accounts authenticate at the identity provider")
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, passwordHash); err != nil {
		return "", "", fmt.Errorf("reset password %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
		return "", "", fmt.Errorf("reset password %s: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("reset password %s: %w", id, err)
	}
	return username, role, nil
}

// UpdateOwnPassword replaces userID's password hash and deletes their OTHER
// sessions (keepSessionID survives — the one that just proved the current
// password) in one transaction. Current-password verification is the
// caller's job: an argon2 KDF must not run while holding the row lock.
// verifiedHash is the hash that verification ran against; the row must
// still carry it, or the update is refused — otherwise a change verified
// against the old credential could land after an admin reset and overwrite
// the fresh password, exactly the takeover the reset was revoking.
func (s *Store) UpdateOwnPassword(ctx context.Context, userID uuid.UUID, verifiedHash, passwordHash string, keepSessionID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update password %s: %w", userID, err)
	}
	defer tx.Rollback(ctx)

	var curHash string
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(password_hash, '') FROM users
		  WHERE id = $1 AND auth_source = 'local' FOR UPDATE`, userID).Scan(&curHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// The account was deleted (or is somehow federated) mid-request.
		return notFoundf("user %s does not exist", userID)
	}
	if err != nil {
		return fmt.Errorf("update password %s: %w", userID, err)
	}
	if curHash != verifiedHash {
		return conflictf("password was changed by another request; sign in with the current password and try again")
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, passwordHash); err != nil {
		return fmt.Errorf("update password %s: %w", userID, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, userID, keepSessionID); err != nil {
		return fmt.Errorf("update password %s: %w", userID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("update password %s: %w", userID, err)
	}
	return nil
}

// ErrPasswordChanged is returned by CreateLocalSession when the password (or
// enabled state) the login verified is no longer the row's current one.
var ErrPasswordChanged = errors.New("password changed during login")

// CreateLocalSession mints a password-login session, revalidating inside the
// insert that verifiedHash is still the user's password hash and the account
// is still enabled — the CreateOIDCSession pattern. Argon2 verification runs
// before this call; a reset or self-service change landing in that window
// revokes every session of the old credential, and an unchecked insert would
// hand the old (presumed leaked) password a fresh session that outlives the
// rotation.
func (s *Store) CreateLocalSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, verifiedHash string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at)
		SELECT $1, u.id, $3, $4 FROM users u
		 WHERE u.id = $2 AND u.auth_source = 'local'
		   AND u.password_hash = $5 AND NOT u.disabled`,
		tokenHash, userID, csrfToken, expiresAt, verifiedHash)
	if err != nil {
		return fmt.Errorf("create local session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPasswordChanged
	}
	return nil
}

// GetSessionByTokenHash returns the live session for a token hash, or
// (nil, nil) when it is unknown, expired, or its user is disabled. Role AND
// network scope are joined live from users/user_networks on every request,
// so a role or scope edit takes effect on the victim's next request with no
// session invalidation.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (*SessionInfo, error) {
	var si SessionInfo
	var netIDs []uuid.UUID
	var netNames []string
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.user_id, u.username, u.role, u.auth_source, s.csrf_token, s.expires_at, s.last_used_at,
		        COALESCE((SELECT array_agg(n.id ORDER BY n.name)
		                    FROM user_networks un JOIN networks n ON n.id = un.network_id
		                   WHERE un.user_id = u.id), '{}'),
		        COALESCE((SELECT array_agg(n.name ORDER BY n.name)
		                    FROM user_networks un JOIN networks n ON n.id = un.network_id
		                   WHERE un.user_id = u.id), '{}')
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.expires_at > now() AND NOT u.disabled`,
		tokenHash).
		Scan(&si.ID, &si.UserID, &si.Username, &si.Role, &si.AuthSource, &si.CSRFToken, &si.ExpiresAt, &si.LastUsedAt,
			&netIDs, &netNames)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if RoleIsNetworkScoped(si.Role) {
		si.Networks = make([]NetworkRef, len(netIDs))
		for i := range netIDs {
			si.Networks[i] = NetworkRef{ID: netIDs[i], Name: netNames[i]}
		}
	}
	return &si, nil
}

// TouchSession records session activity (rate-limited by the caller).
func (s *Store) TouchSession(ctx context.Context, id uuid.UUID) error {
	// One round trip: the session touch also upserts the user's activity
	// row for the current UTC day, so "active" survives logout and expiry.
	_, err := s.pool.Exec(ctx, `
		WITH s AS (
			UPDATE sessions SET last_used_at = now() WHERE id = $1
			RETURNING user_id
		)
		INSERT INTO user_activity_days (user_id, identity, day)
		SELECT u.id, `+identityExpr+`, (now() AT TIME ZONE 'UTC')::date
		  FROM users u JOIN s ON s.user_id = u.id
		ON CONFLICT (user_id, day) DO UPDATE SET last_seen_at = now()`, id)
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

// DeleteExpiredSessions is opportunistic cleanup run from the login handler;
// expired rows are already invisible to lookups either way.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// identityExpr is the SQL for a users row's stable per-person identity —
// issuer+subject for SSO, username for local — shared by login_events and
// user_activity_days so the two tables can never disagree on the key.
// Unit separator, not NUL: Postgres text cannot hold 0x00, and \x1f
// cannot appear in an issuer URL, so the join is unambiguous.
const identityExpr = `CASE WHEN auth_source = 'oidc' THEN oidc_issuer || E'\x1f' || oidc_subject
	                   ELSE username END`

// RecordLogin appends a login audit event, snapshotting everything from
// the just-authenticated user's row in one statement. The row deliberately
// has no FK to users, so the audit log survives user deletion; the
// identity snapshot (issuer+subject for SSO, username for local) is what
// keeps unique-user counts exact across deletion, JIT re-provisioning, and
// IdP-driven renames — user_id alone double-counts a deleted-then-
// reprovisioned SSO user.
func (s *Store) RecordLogin(ctx context.Context, userID uuid.UUID) error {
	// The login day is also an active day; a fresh session's last_used_at
	// is now(), so the touch path would not record it for touchInterval.
	tag, err := s.pool.Exec(ctx, `
		WITH ev AS (
			INSERT INTO login_events (user_id, identity, username, role, auth_source)
			SELECT id, `+identityExpr+`, username, role, auth_source
			  FROM users WHERE id = $1
			RETURNING user_id, identity
		)
		INSERT INTO user_activity_days (user_id, identity, day)
		SELECT user_id, identity, (now() AT TIME ZONE 'UTC')::date FROM ev
		ON CONFLICT (user_id, day) DO UPDATE SET last_seen_at = now()`, userID)
	if err != nil {
		return fmt.Errorf("record login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record login: user %s vanished before the event was written", userID)
	}
	return nil
}

// UserAccountInfo is one row of the admin Settings -> Users list: a live
// users row with its login-event aggregates, or a deleted identity
// reconstructed from its latest login-event snapshots.
type UserAccountInfo struct {
	ID          uuid.UUID
	Username    string
	Role        string
	AuthSource  string
	Status      string     // "active" | "disabled" | "deleted"
	CreatedAt   *time.Time // nil for deleted identities (not snapshotted)
	LoginCount  int64
	LastLoginAt *time.Time // nil = never logged in
	// LastActiveAt is the newest user_activity_days.last_seen_at for the
	// account, at the session-touch cadence's resolution; nil = never.
	LastActiveAt *time.Time
	// Networks is the scope of a live network-scoped account; nil for
	// global roles and for deleted identities (scope rows cascade away).
	Networks []string
}

// UserAccountFilter narrows and pages ListUserAccounts. Empty string means
// "any"; values are validated by the HTTP layer.
type UserAccountFilter struct {
	Query  string // case-insensitive username substring
	Role   string // one of Roles
	Status string // "active" | "disabled" | "deleted"
	Source string // "local" | "oidc"
	Limit  int
	Offset int
}

// escapeLike escapes LIKE/ILIKE metacharacters so a filter query matches
// them literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListUserAccounts lists dashboard accounts — every users row plus every
// deleted identity still present in login_events — filtered and paged
// server-side (OIDC JIT provisioning makes this the one config table that
// grows with people, not infrastructure). The second return is the total
// row count matching the filter, ignoring Limit/Offset.
func (s *Store) ListUserAccounts(ctx context.Context, f UserAccountFilter) ([]UserAccountInfo, int64, error) {
	rows, err := s.pool.Query(ctx, `
		WITH accounts AS (
			SELECT u.id, u.username, u.role, u.auth_source,
			       CASE WHEN u.disabled THEN 'disabled' ELSE 'active' END AS status,
			       u.created_at::timestamptz AS created_at,
			       count(e.id) AS login_count, max(e.occurred_at) AS last_login_at,
			       (SELECT max(a.last_seen_at) FROM user_activity_days a
			         WHERE a.user_id = u.id) AS last_active_at,
			       COALESCE((SELECT array_agg(n.name ORDER BY n.name)
			                   FROM user_networks un JOIN networks n ON n.id = un.network_id
			                  WHERE un.user_id = u.id), '{}') AS networks
			  FROM users u
			  LEFT JOIN login_events e ON e.user_id = u.id
			 GROUP BY u.id
			UNION ALL
			SELECT e.user_id,
			       (array_agg(e.username ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.role ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.auth_source ORDER BY e.occurred_at DESC))[1],
			       'deleted', NULL,
			       count(*), max(e.occurred_at),
			       (SELECT max(a.last_seen_at) FROM user_activity_days a
			         WHERE a.user_id = e.user_id),
			       '{}'
			  FROM login_events e
			 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id)
			 GROUP BY e.user_id
		)
		SELECT id, username, role, auth_source, status, created_at,
		       login_count, last_login_at, last_active_at, networks, count(*) OVER ()
		  FROM accounts
		 WHERE ($1 = '' OR username ILIKE '%' || $1 || '%')
		   AND ($2 = '' OR role = $2)
		   AND ($3 = '' OR status = $3)
		   AND ($4 = '' OR auth_source = $4)
		 ORDER BY lower(username), id
		 LIMIT $5 OFFSET $6`,
		escapeLike(f.Query), f.Role, f.Status, f.Source, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list user accounts: %w", err)
	}
	defer rows.Close()

	var accounts []UserAccountInfo
	var total int64
	for rows.Next() {
		var a UserAccountInfo
		var networks []string
		if err := rows.Scan(&a.ID, &a.Username, &a.Role, &a.AuthSource, &a.Status,
			&a.CreatedAt, &a.LoginCount, &a.LastLoginAt, &a.LastActiveAt, &networks, &total); err != nil {
			return nil, 0, fmt.Errorf("list user accounts: %w", err)
		}
		if a.Status != "deleted" {
			a.Networks = scopeNames(a.Role, networks)
		}
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// An offset past the end returns no rows, and with them no window
	// count; the caller still needs the real total to clamp its pager.
	if accounts == nil && f.Offset > 0 {
		err := s.pool.QueryRow(ctx, `
			WITH accounts AS (
				SELECT u.username, u.role, u.auth_source,
				       CASE WHEN u.disabled THEN 'disabled' ELSE 'active' END AS status
				  FROM users u
				UNION ALL
				-- Subquery: DISTINCT ON needs its own ORDER BY, which a
				-- bare UNION branch cannot carry.
				SELECT d.username, d.role, d.auth_source, 'deleted'
				  FROM (
					SELECT DISTINCT ON (e.user_id) e.username, e.role, e.auth_source
					  FROM login_events e
					 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id)
					 ORDER BY e.user_id, e.occurred_at DESC
				  ) d
			)
			SELECT count(*) FROM accounts
			 WHERE ($1 = '' OR username ILIKE '%' || $1 || '%')
			   AND ($2 = '' OR role = $2)
			   AND ($3 = '' OR status = $3)
			   AND ($4 = '' OR auth_source = $4)`,
			escapeLike(f.Query), f.Role, f.Status, f.Source).Scan(&total)
		if err != nil {
			return nil, 0, fmt.Errorf("count user accounts: %w", err)
		}
	}
	return accounts, total, nil
}

// LoginMonthStat is one UTC calendar month's login totals.
type LoginMonthStat struct {
	Month       time.Time // month start, UTC
	Total       int64
	Local       int64
	OIDC        int64
	UniqueUsers int64
	// ActiveUsers is the count of distinct identities with at least one
	// user_activity_days row in the month — people who used the dashboard,
	// whether or not they signed in that month.
	ActiveUsers int64
}

// MonthlyLoginStats returns zero-filled per-month login totals for the most
// recent `months` UTC calendar months, oldest first, ending with the current
// month. Buckets are UTC regardless of the connection's TimeZone; unique
// users count DISTINCT identity snapshots, so one person stays one count
// across renames, deletion, and SSO re-provisioning.
func (s *Store) MonthlyLoginStats(ctx context.Context, months int) ([]LoginMonthStat, error) {
	rows, err := s.pool.Query(ctx, `
		WITH months AS (
			SELECT generate_series(
			         date_trunc('month', now() AT TIME ZONE 'UTC') - ($1 - 1) * interval '1 month',
			         date_trunc('month', now() AT TIME ZONE 'UTC'),
			         interval '1 month') AS m
		)
		SELECT months.m,
		       count(e.id),
		       count(e.id) FILTER (WHERE e.auth_source = 'local'),
		       count(e.id) FILTER (WHERE e.auth_source = 'oidc'),
		       count(DISTINCT e.identity),
		       (SELECT count(DISTINCT a.identity) FROM user_activity_days a
		         WHERE a.day >= months.m::date
		           AND a.day <  (months.m + interval '1 month')::date)
		  FROM months
		  LEFT JOIN login_events e
		    ON e.occurred_at >= (months.m AT TIME ZONE 'UTC')
		   AND e.occurred_at <  ((months.m + interval '1 month') AT TIME ZONE 'UTC')
		 GROUP BY months.m
		 ORDER BY months.m`, months)
	if err != nil {
		return nil, fmt.Errorf("monthly login stats: %w", err)
	}
	defer rows.Close()

	var stats []LoginMonthStat
	for rows.Next() {
		var st LoginMonthStat
		if err := rows.Scan(&st.Month, &st.Total, &st.Local, &st.OIDC, &st.UniqueUsers, &st.ActiveUsers); err != nil {
			return nil, fmt.Errorf("monthly login stats: %w", err)
		}
		// generate_series over a naive UTC timestamp scans with an
		// unspecified zone; pin it so callers format the right month.
		st.Month = time.Date(st.Month.Year(), st.Month.Month(), 1, 0, 0, 0, 0, time.UTC)
		stats = append(stats, st)
	}
	return stats, rows.Err()
}
