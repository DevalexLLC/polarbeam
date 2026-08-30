// Package store is the control plane's PostgreSQL access layer (pgx).
package store

import (
	"context"
	"fmt"
	"log/slog"
	neturl "net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool

	// Config-derived caches. Both answer 30s-polled dashboard reads and the
	// outage sweep with data whose truth changes only on operator config
	// writes, so every write path that can alter probe expansion calls
	// invalidateConfigCaches and the TTL is only the backstop for writes
	// that bypass the store (manual SQL). Staleness is bounded well inside
	// the config stream's own ~30s convergence lag either way.
	enabled       configCache[[]uuid.UUID]
	expectedPairs configCache[map[string][]NetworkPair]
}

// noteConfigWrite is deferred by every write path that can alter probe
// expansion: it drops the in-process caches AND bumps the config_version
// row (migration 0024) so OTHER processes notice — the admin CLI writes
// from its own process, and the serving process's config streams gate
// their snapshot rebuilds on that row. The bump survives a cancelled
// request context and is log-only on failure: the streams' periodic
// forced rebuild bounds the damage of a missed bump.
func (s *Store) noteConfigWrite(ctx context.Context) {
	s.InvalidateConfigCaches()
	if _, err := s.pool.Exec(context.WithoutCancel(ctx),
		`UPDATE config_version SET version = version + 1`); err != nil {
		slog.Error("config version bump failed", "err", err)
	}
}

// ConfigDBVersion reads the cross-process config-write counter.
func (s *Store) ConfigDBVersion(ctx context.Context) (int64, error) {
	var v int64
	if err := s.pool.QueryRow(ctx, `SELECT version FROM config_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("config version: %w", err)
	}
	return v, nil
}

// configCacheTTL matches the dashboard poll and config-stream cadence.
const configCacheTTL = 30 * time.Second

// configCache is one mutex-guarded value with an expiry and an
// invalidation generation. A fill records the generation it started from
// (see generation/setIfCurrent): a config write that lands between a
// filler's DB read and its publish bumps the generation, and the stale
// fill is discarded instead of resurrecting pre-write data for a TTL.
type configCache[T any] struct {
	mu      sync.Mutex
	gen     uint64
	value   T
	expires time.Time
}

func (c *configCache[T]) get(now time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.After(c.expires) {
		var zero T
		return zero, false
	}
	return c.value, true
}

// generation snapshots the invalidation counter before a fill's DB read.
func (c *configCache[T]) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// setIfCurrent publishes a fill only if no invalidation raced it.
func (c *configCache[T]) setIfCurrent(v T, now time.Time, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	c.value = v
	c.expires = now.Add(configCacheTTL)
}

func (c *configCache[T]) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	var zero T
	c.value = zero
	c.expires = time.Time{}
}

// InvalidateConfigCaches drops the in-process config-derived caches (write
// paths go through noteConfigWrite, which also bumps the cross-process
// config_version row); exported for tests that mutate configuration with
// raw SQL.
func (s *Store) InvalidateConfigCaches() {
	s.enabled.invalidate()
	s.expectedPairs.invalidate()
}

// Connect opens a pool and verifies the database is reachable (preflight:
// a bad db.url fails startup here, loudly).
//
// maxConns caps the pool: an explicit value (db.max_conns) always wins; 0
// applies an automatic default UNLESS the URL carries its own
// pool_max_conns. The pgx default of max(4, NumCPU) starves a small host
// once agent config streams, the outage sweep, and dashboard polls all
// draw from the same pool, so automatic sizing raises the floor while
// staying beneath a stock postgres max_connections of 100.
func Connect(ctx context.Context, url string, timeout time.Duration, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db.url: %w", err)
	}
	switch {
	case maxConns > 0:
		cfg.MaxConns = int32(min(maxConns, 1<<31-1))
	case !urlSetsPoolMaxConns(url):
		cfg.MaxConns = int32(max(16, min(4*runtime.NumCPU(), 64)))
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db unreachable at configured db.url: %w", err)
	}
	return &Store{pool: pool}, nil
}

// urlSetsPoolMaxConns reports whether the connection string itself carries
// a pool_max_conns pgx pool option — parsed the way pgx does, not by
// substring, so a percent-encoded parameter is honored and the text
// appearing inside a password never counts. pgx accepts both URL and
// space-separated DSN keyword forms.
func urlSetsPoolMaxConns(raw string) bool {
	if strings.Contains(raw, "://") {
		u, err := neturl.Parse(raw)
		return err == nil && u.Query().Has("pool_max_conns")
	}
	return dsnPoolMaxConnsRe.MatchString(raw)
}

var dsnPoolMaxConnsRe = regexp.MustCompile(`(?:^|\s)pool_max_conns=`)

func (s *Store) Close() { s.pool.Close() }

// ToolkitInstalled reports whether the timescaledb_toolkit extension is
// present. Percentile queries depend on it, so serve preflight fails loud
// when it is missing instead of erroring on the first dashboard request.
func (s *Store) ToolkitInstalled(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'timescaledb_toolkit')`).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("toolkit check: %w", err)
	}
	return ok, nil
}

// Begin starts a transaction; the ingest path inserts results and advances
// outage/path state atomically in one.
func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) { return s.pool.Begin(ctx) }

// Pool exposes the underlying pool for packages that need transactions.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
