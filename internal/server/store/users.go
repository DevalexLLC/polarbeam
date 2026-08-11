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
		return uuid.Nil, conflictf("user %q already exists", username)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create user: %w", err)
	}
	return id, nil
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

// DeleteExpiredSessions is opportunistic cleanup run from the login handler;
// expired rows are already invisible to lookups either way.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RecordLogin appends a login audit event, snapshotting everything from
// the just-authenticated user's row in one statement. The row deliberately
// has no FK to users, so the audit log survives user deletion; the
// identity snapshot (issuer+subject for SSO, username for local) is what
// keeps unique-user counts exact across deletion, JIT re-provisioning, and
// IdP-driven renames — user_id alone double-counts a deleted-then-
// reprovisioned SSO user.
func (s *Store) RecordLogin(ctx context.Context, userID uuid.UUID) error {
	// Unit separator, not NUL: Postgres text cannot hold 0x00, and \x1f
	// cannot appear in an issuer URL, so the join is unambiguous.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO login_events (user_id, identity, username, role, auth_source)
		SELECT id,
		       CASE WHEN auth_source = 'oidc' THEN oidc_issuer || E'\x1f' || oidc_subject
		            ELSE username END,
		       username, role, auth_source
		  FROM users WHERE id = $1`, userID)
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
}

// UserAccountFilter narrows and pages ListUserAccounts. Empty string means
// "any"; values are validated by the HTTP layer.
type UserAccountFilter struct {
	Query  string // case-insensitive username substring
	Role   string // "admin" | "viewer"
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
			       count(e.id) AS login_count, max(e.occurred_at) AS last_login_at
			  FROM users u
			  LEFT JOIN login_events e ON e.user_id = u.id
			 GROUP BY u.id
			UNION ALL
			SELECT e.user_id,
			       (array_agg(e.username ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.role ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.auth_source ORDER BY e.occurred_at DESC))[1],
			       'deleted', NULL,
			       count(*), max(e.occurred_at)
			  FROM login_events e
			 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id)
			 GROUP BY e.user_id
		)
		SELECT id, username, role, auth_source, status, created_at,
		       login_count, last_login_at, count(*) OVER ()
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
		if err := rows.Scan(&a.ID, &a.Username, &a.Role, &a.AuthSource, &a.Status,
			&a.CreatedAt, &a.LoginCount, &a.LastLoginAt, &total); err != nil {
			return nil, 0, fmt.Errorf("list user accounts: %w", err)
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
		       count(DISTINCT e.identity)
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
		if err := rows.Scan(&st.Month, &st.Total, &st.Local, &st.OIDC, &st.UniqueUsers); err != nil {
			return nil, fmt.Errorf("monthly login stats: %w", err)
		}
		// generate_series over a naive UTC timestamp scans with an
		// unspecified zone; pin it so callers format the right month.
		st.Month = time.Date(st.Month.Year(), st.Month.Month(), 1, 0, 0, 0, 0, time.UTC)
		stats = append(stats, st)
	}
	return stats, rows.Err()
}
