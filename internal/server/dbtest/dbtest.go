// Package dbtest provisions throwaway databases for DB-backed tests.
//
// Tests are gated on POLARBEAM_TEST_DB_URL, a postgres:// superuser URL to a
// disposable TimescaleDB (the compose image, timescale/timescaledb-ha). When
// the variable is unset every DB-backed test skips, which keeps `make test`
// and the offline-build CI job network-free; the db-test CI job sets it
// against a service container. Locally:
//
//	docker run -d --rm -e POSTGRES_PASSWORD=t -p 54329:5432 timescale/timescaledb-ha:pg16-all
//	POLARBEAM_TEST_DB_URL=postgres://postgres:t@localhost:54329/postgres go test ./internal/server/...
//
// Each call creates a uniquely named database so tests never share state —
// both across the parallel processes `go test ./...` spawns and across
// t.Parallel() tests inside one package — and drops it on cleanup.
//
// Returned URLs carry pool_max_conns=4: DB tests run in parallel, so pool
// sizes multiply — store.Connect's automatic sizing (up to 64) times a few
// parallel tests would blow through a stock postgres max_connections of
// 100, while parallel tests × (4 pool conns + 1 admin conn) stays well
// inside it at go test's default -parallel. On a many-core box running the
// whole tree, cap the multiplier explicitly: `go test -p 4 -parallel 4`.
// Single-connection pgx clients must strip the parameter (only pgxpool
// consumes it; plain pgx would forward it to the server as a GUC).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/devalexllc/polarbeam/internal/server/migrate"
)

// EnvURL names the environment variable gating all DB-backed tests.
const EnvURL = "POLARBEAM_TEST_DB_URL"

// Empty creates a fresh, unmigrated database and returns a URL pointing at
// it. The test skips when EnvURL is unset; the database is dropped on
// cleanup.
func Empty(t testing.TB) string {
	t.Helper()
	return capped(t, provision(t, ""))
}

// Migrated returns a URL to a database with the schema a fresh production
// database has after `polarbeam-server migrate`. It clones a pre-migrated
// template (built once per schema hash and shared across processes) instead
// of replaying every migration per test.
func Migrated(t testing.TB) string {
	t.Helper()
	return capped(t, provision(t, ensureTemplate(t)))
}

// provision creates the throwaway database — cloned from template when one
// is named — and returns its plain URL (no pool parameters — internal
// single-connection use).
func provision(t testing.TB, template string) string {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skipf("set %s to a disposable TimescaleDB superuser URL to run DB-backed tests", EnvURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name, err := freshName()
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	url, err := withDatabase(admin, name)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("dbtest: connect to %s: %v", EnvURL, err)
	}
	defer conn.Close(ctx)
	// name is generated hex and template is dbtest's own naming, so quoting
	// them as identifiers is safe.
	create := fmt.Sprintf(`CREATE DATABASE %q`, name)
	if template != "" {
		create = fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, template)
	}
	if err := execRetry(ctx, conn, create); err != nil {
		t.Fatalf("dbtest: create %s: %v", name, err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("dbtest: reconnect for drop: %v", err)
			return
		}
		defer conn.Close(ctx)
		// FORCE terminates connections a test failed to close.
		if err := execRetry(ctx, conn, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, name)); err != nil {
			t.Errorf("dbtest: drop %s: %v", name, err)
		}
	})
	return url
}

var (
	tmplMu   sync.Mutex
	tmplName string
	tmplErr  error
)

// ensureTemplate builds (once per schema hash) a fully migrated template
// database and returns its name. The name embeds migrate.SchemaHash, so any
// migration edit — even to a not-yet-shipped file the schema_migrations
// ledger would not re-run — keys a fresh template. Cross-process
// coordination (go test runs packages as separate processes) uses a
// pg_advisory_lock; the build creates under a temporary name and renames
// into place, so a crashed half-built template can never be mistaken for a
// finished one. The finished template gets ALLOW_CONNECTIONS false, which
// keeps TimescaleDB's background workers from holding sessions on it —
// cloning fails with "being accessed by other users" while anyone is
// connected to the source.
func ensureTemplate(t testing.TB) string {
	t.Helper()
	tmplMu.Lock()
	defer tmplMu.Unlock()
	if tmplErr != nil {
		t.Fatalf("dbtest: template build failed earlier: %v", tmplErr)
	}
	if tmplName != "" {
		return tmplName
	}
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skipf("set %s to a disposable TimescaleDB superuser URL to run DB-backed tests", EnvURL)
	}
	name, err := buildTemplate(admin)
	if err != nil {
		tmplErr = err
		t.Fatalf("dbtest: build template: %v", err)
	}
	tmplName = name
	return tmplName
}

// tmplLockKey is the single advisory-lock key serializing ALL template
// building and build-leftover reaping across processes — deliberately not
// hash-derived, so two checkouts with different migration revisions sharing
// one server cannot reap or race each other's in-flight builds. Finished
// templates for other hashes are never dropped here: a concurrent run may
// be cloning from one right now. They cost only disk until the server is
// recreated (`make reset` locally; CI containers are ephemeral).
const tmplLockKey = int64(0x706f6c6172_746d70) // "polar tmp"

func buildTemplate(admin string) (string, error) {
	hash, err := migrate.SchemaHash()
	if err != nil {
		return "", err
	}
	name := "polarbeam_test_tmpl_" + hash[:12]

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", EnvURL, err)
	}
	defer conn.Close(ctx)

	// The session lock releases on disconnect even if this process dies
	// mid-build.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, tmplLockKey); err != nil {
		return "", fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, tmplLockKey)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", fmt.Errorf("check template: %w", err)
	}
	if exists {
		return name, nil
	}

	// Reap leftovers of crashed or failed builds first. The lock guarantees
	// no build is in flight; underscores are escaped so LIKE cannot match
	// unrelated databases (_ is a single-character wildcard).
	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE 'polarbeam\_test\_tmplbuild\_%'`)
	if err != nil {
		return "", fmt.Errorf("list stale template builds: %w", err)
	}
	var stale []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", fmt.Errorf("list stale template builds: %w", err)
		}
		stale = append(stale, n)
	}
	if rows.Err() != nil {
		return "", fmt.Errorf("list stale template builds: %w", rows.Err())
	}
	for _, n := range stale {
		if err := execRetry(ctx, conn, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, n)); err != nil {
			return "", fmt.Errorf("drop stale template build %s: %w", n, err)
		}
	}

	build := "polarbeam_test_tmplbuild_" + hash[:12]
	if err := execRetry(ctx, conn, fmt.Sprintf(`CREATE DATABASE %q`, build)); err != nil {
		return "", fmt.Errorf("create template build db: %w", err)
	}
	if err := migrateInto(ctx, admin, build); err != nil {
		// Best effort: the next builder's reap sweep gets it otherwise.
		_ = execRetry(ctx, conn, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, build))
		return "", err
	}
	// Seal before renaming: no NEW sessions may land, then kick the
	// TimescaleDB background workers that attached while the migration
	// registered cagg jobs — RENAME needs the database session-free.
	if _, err := conn.Exec(ctx,
		fmt.Sprintf(`ALTER DATABASE %q ALLOW_CONNECTIONS false`, build)); err != nil {
		return "", fmt.Errorf("seal template: %w", err)
	}
	if _, err := conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, build); err != nil {
		return "", fmt.Errorf("clear template sessions: %w", err)
	}
	if err := execRetry(ctx, conn,
		fmt.Sprintf(`ALTER DATABASE %q RENAME TO %q`, build, name)); err != nil {
		return "", fmt.Errorf("finalize template: %w", err)
	}
	return name, nil
}

func migrateInto(ctx context.Context, admin, name string) error {
	url, err := withDatabase(admin, name)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect for migrate: %w", err)
	}
	defer conn.Close(ctx)
	if err := migrate.Apply(ctx, conn); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

// execRetry runs a CREATE/DROP DATABASE statement, retrying the transient
// contention parallel tests provoke: concurrent CREATE DATABASE calls copy
// template1 and fail with SQLSTATE 55006 ("source database is being
// accessed by other users") while another copy is in flight.
func execRetry(ctx context.Context, conn *pgx.Conn, sql string) error {
	var err error
	for attempt, delay := 0, 100*time.Millisecond; attempt < 5; attempt, delay = attempt+1, delay*2 {
		if _, err = conn.Exec(ctx, sql); err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return err
}

func freshName() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random db name: %w", err)
	}
	return "polarbeam_test_" + hex.EncodeToString(raw), nil
}

func withDatabase(admin, name string) (string, error) {
	u, err := neturl.Parse(admin)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvURL, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("%s must be a postgres:// URL", EnvURL)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// capped adds the pool_max_conns cap to a returned URL — see the package
// comment for the connection budget.
func capped(t testing.TB, url string) string {
	t.Helper()
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "4")
	u.RawQuery = q.Encode()
	return u.String()
}
