package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrTokenInvalid covers every join-token failure (unknown, wrong secret,
// expired, already used). Deliberately indistinct: enrollment callers are
// unauthenticated.
var ErrTokenInvalid = errors.New("join token invalid, expired, or already used")

// EnsureSite returns the site's ID, creating it if it does not exist.
func (s *Store) EnsureSite(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sites (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, name).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ensure site %q: %w", name, err)
	}
	return id, nil
}

// CreateJoinToken mints a single-use enrollment token for a site, bound to
// the network the enrolling agent will join, and returns its cleartext
// "<id>.<secret>" form — shown exactly once, only the secret's sha256 is
// stored. Callers resolve the network name (empty input means 'default')
// via NetworkIDByName — networks are never auto-created.
func (s *Store) CreateJoinToken(ctx context.Context, siteID, networkID uuid.UUID, createdBy string, ttl time.Duration) (string, error) {
	id := uuid.New()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(secret))

	_, err := s.pool.Exec(ctx, `
		INSERT INTO join_tokens (id, secret_hash, site_id, network_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6)`,
		id, hash[:], siteID, networkID, createdBy, ttl)
	if isFKViolation(err) {
		// The site or network was deleted between the caller's resolve and
		// this insert — a 404, not a 500.
		return "", notFoundf("site %s or network %s no longer exists", siteID, networkID)
	}
	if err != nil {
		return "", fmt.Errorf("create join token: %w", err)
	}
	return id.String() + "." + secret, nil
}

// IssuedCert is what the issue callback passed to EnrollAgent reports back
// for recording.
type IssuedCert struct {
	Serial    *big.Int
	NotBefore time.Time
	NotAfter  time.Time
}

// EnrollAgent atomically consumes a join token, registers the agent, and
// records the certificate produced by issue — all in one transaction, so a
// failure at any step (including issuance) leaves the single-use token
// unconsumed and no orphaned agent behind.
//
// Retries are idempotent: a token already consumed by the SAME CSR (hash)
// means the enroll response was lost in transit — the original agent is
// returned with a freshly issued certificate instead of ErrTokenInvalid.
// Any other token problem returns ErrTokenInvalid.
func (s *Store) EnrollAgent(ctx context.Context, token, hostname, probeAddress, version string, csrHash []byte,
	issue func(agentID uuid.UUID) (IssuedCert, error)) (agentID, siteID uuid.UUID, err error) {
	// A new agent brings a new agent-kind target, changing mesh expansion.
	defer s.InvalidateConfigCaches()
	tokenID, secret, ok := splitToken(token)
	if !ok {
		return uuid.Nil, uuid.Nil, ErrTokenInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		storedHash  []byte
		expiresAt   time.Time
		usedAt      *time.Time
		usedByAgent *uuid.UUID
		usedCSRHash []byte
		networkID   uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT secret_hash, expires_at, used_at, used_by_agent, used_csr_hash, site_id, network_id
		FROM join_tokens WHERE id = $1 FOR UPDATE`,
		tokenID).Scan(&storedHash, &expiresAt, &usedAt, &usedByAgent, &usedCSRHash, &siteID, &networkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrTokenInvalid
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: %w", err)
	}
	gotHash := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(storedHash, gotHash[:]) != 1 || time.Now().After(expiresAt) {
		return uuid.Nil, uuid.Nil, ErrTokenInvalid
	}

	if usedAt != nil {
		// Idempotent replay only: same token AND same CSR.
		if usedByAgent == nil || len(usedCSRHash) == 0 || !bytes.Equal(usedCSRHash, csrHash) {
			return uuid.Nil, uuid.Nil, ErrTokenInvalid
		}
		agentID = *usedByAgent
	} else {
		agentID = uuid.New()
		// The agent inherits its token's network — assigned server-side when
		// the token was minted, never claimed by the agent, so a tenant token
		// cannot enroll into another plane. No agent update path exists, so
		// the network is immutable for the agent's lifetime (move a box by
		// re-enrolling), like probe_address.
		if _, err := tx.Exec(ctx, `
			INSERT INTO agents (id, site_id, network_id, hostname, probe_address, version)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			agentID, siteID, networkID, hostname, probeAddress, version); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: create agent: %w", err)
		}
		// Every agent is a probeable mesh target from the moment it exists.
		if _, err := tx.Exec(ctx, `
			INSERT INTO targets (kind, name, agent_id)
			VALUES ('agent', 'agent:' || $2, $1)`,
			agentID, agentID.String()); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: create agent target: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE join_tokens SET used_at = now(), used_by_agent = $2, used_csr_hash = $3
			WHERE id = $1`,
			tokenID, agentID, csrHash); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: consume token: %w", err)
		}
	}
	cert, err := issue(agentID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: issue certificate: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO certificates (serial, agent_id, not_before, not_after)
		VALUES ($1, $2, $3, $4)`,
		cert.Serial.String(), agentID, cert.NotBefore, cert.NotAfter); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: record certificate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("enroll: %w", err)
	}
	return agentID, siteID, nil
}

func splitToken(token string) (id uuid.UUID, secret string, ok bool) {
	idStr, secret, found := strings.Cut(token, ".")
	if !found || secret == "" {
		return uuid.Nil, "", false
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, "", false
	}
	return id, secret, true
}

// InsertCertificate records an issued certificate.
func (s *Store) InsertCertificate(ctx context.Context, serial *big.Int, agentID uuid.UUID, notBefore, notAfter time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO certificates (serial, agent_id, not_before, not_after)
		VALUES ($1, $2, $3, $4)`,
		serial.String(), agentID, notBefore, notAfter)
	if err != nil {
		return fmt.Errorf("insert certificate: %w", err)
	}
	return nil
}

// CertValid reports whether serial belongs to agentID, is unrevoked, and is
// within its validity window. Unknown serials are invalid — the database is
// the sole revocation authority.
func (s *Store) CertValid(ctx context.Context, serial *big.Int, agentID uuid.UUID) (bool, error) {
	var valid bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM certificates
			WHERE serial = $1 AND agent_id = $2
			  AND revoked_at IS NULL
			  AND now() BETWEEN not_before AND not_after)`,
		serial.String(), agentID).Scan(&valid)
	if err != nil {
		return false, fmt.Errorf("certificate check: %w", err)
	}
	return valid, nil
}

// RevokeCertificate marks a certificate revoked. Takes effect on the
// holder's next connection (plus the sweep for live streams).
func (s *Store) RevokeCertificate(ctx context.Context, serial *big.Int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE certificates SET revoked_at = now()
		WHERE serial = $1 AND revoked_at IS NULL`, serial.String())
	if err != nil {
		return fmt.Errorf("revoke certificate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("certificate serial %s not found or already revoked", serial)
	}
	return nil
}

// TouchAgent updates liveness metadata for a connected agent.
func (s *Store) TouchAgent(ctx context.Context, agentID uuid.UUID, version, configHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE agents SET last_seen_at = now(), version = $2, current_config_hash = $3
		WHERE id = $1`, agentID, version, configHash)
	if err != nil {
		return fmt.Errorf("touch agent: %w", err)
	}
	return nil
}

// RecordDroppedResultsTx accumulates a legacy dropped_since_last_push delta
// inside the ingest transaction (pre-v0.4 agents only). Running in the same
// commit as the batch means a server-side failure can no longer re-add the
// delta on retry; a push whose response is lost after commit still can — the
// wire delta carries no idempotency key.
func RecordDroppedResultsTx(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, n uint64) error {
	// LEAST against the remaining headroom: saturate at the bigint ceiling
	// instead of failing the RPC forever on an absurd counter.
	_, err := tx.Exec(ctx, `
		UPDATE agents SET
			dropped_results = dropped_results + LEAST($2, 9223372036854775807 - dropped_results),
			last_dropped_at = now()
		WHERE id = $1`, agentID, clampInt64(n))
	if err != nil {
		return fmt.Errorf("record dropped results: %w", err)
	}
	return nil
}

// RecordDroppedTotalTx folds an agent's lifetime dropped_total into
// dropped_results idempotently: only the portion beyond the last total this
// agent reported counts, so a retried push (same total) adds nothing.
// unacked is the agent's legacy dropped_since_last_push value, needed for
// the very first total-bearing report (see droppedDelta). Returns the
// applied delta and whether a counter reset (backwards total, i.e. wiped
// agent spool state) was assumed.
func RecordDroppedTotalTx(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, total, unacked uint64) (delta int64, reset bool, err error) {
	t := clampInt64(total)
	var last *int64
	// FOR UPDATE serializes concurrent pushes from the same agent; a missing
	// row is an error — an authenticated agent must have one.
	if err := tx.QueryRow(ctx, `
		SELECT dropped_last_total FROM agents WHERE id = $1 FOR UPDATE`,
		agentID).Scan(&last); err != nil {
		return 0, false, fmt.Errorf("load dropped_last_total: %w", err)
	}
	delta, reset = droppedDelta(last, t, clampInt64(min(unacked, total)))
	if last != nil && delta == 0 && !reset {
		// Pure retry or no new drops: skip the write so steady-state pushes
		// from an agent that ever dropped don't churn its row. A nil last
		// must still write to initialize the baseline.
		return 0, false, nil
	}
	// LEAST against the remaining headroom: saturate at the bigint ceiling
	// instead of failing the RPC forever on an absurd counter.
	if _, err := tx.Exec(ctx, `
		UPDATE agents SET
			dropped_results = dropped_results + LEAST($2, 9223372036854775807 - dropped_results),
			dropped_last_total = $3,
			last_dropped_at = CASE WHEN $2 > 0 THEN now() ELSE last_dropped_at END
		WHERE id = $1`, agentID, delta, t); err != nil {
		return 0, false, fmt.Errorf("record dropped total: %w", err)
	}
	return delta, reset, nil
}

// droppedDelta computes how much of a reported lifetime total is new.
//
// A nil last means this is the agent's first total-bearing report. Drops
// before it may already be in dropped_results via the legacy delta path (an
// agent upgraded before the server reports through an old server, which
// records and acknowledges the legacy field), so only the agent's still
// unacknowledged portion is new — never the whole lifetime total.
//
// total < last means the agent's spool state was reset (the pusher is
// strictly sequential, so totals never go backwards otherwise): the whole
// new total is fresh loss and last is rebaselined. Clamping the delta to 0
// instead would silently uncount every post-wipe drop.
func droppedDelta(last *int64, total, unacked int64) (delta int64, reset bool) {
	if last == nil {
		return unacked, false
	}
	if total >= *last {
		return total - *last, false
	}
	return total, true
}

// clampInt64 guards the uint64 wire counters against bigint overflow.
func clampInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
