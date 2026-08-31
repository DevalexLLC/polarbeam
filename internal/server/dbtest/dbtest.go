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
	return capped(t, provision(t))
}

// Migrated is Empty plus a full migrate.Apply, i.e. the schema a fresh
// production database has after `polarbeam-server migrate`.
func Migrated(t testing.TB) string {
	t.Helper()
	url := provision(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("dbtest: connect for migrate: %v", err)
	}
	defer conn.Close(ctx)
	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("dbtest: migrate: %v", err)
	}
	return capped(t, url)
}

// provision creates the throwaway database and returns its plain URL
// (no pool parameters — internal single-connection use).
func provision(t testing.TB) string {
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
	// name is generated hex, so quoting it as an identifier is safe.
	if err := execRetry(ctx, conn, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
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
