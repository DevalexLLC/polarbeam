package migrate_test

// DB-backed migration tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). Shipped migrations are immutable, so a file that
// fails to apply on a fresh TimescaleDB can only be repaired with a follow-up
// migration forever — this is the one place that failure is caught before a
// release. External test package: dbtest imports migrate.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/migrate"
)

func connect(t *testing.T, url string) (context.Context, *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return ctx, conn
}

func TestApplyOnFreshDatabase(t *testing.T) {
	ctx, conn := connect(t, dbtest.Empty(t))

	pending, err := migrate.Pending(ctx, conn)
	if err != nil {
		t.Fatalf("Pending on fresh db: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("fresh database reports no pending migrations")
	}

	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply on fresh db: %v", err)
	}
	if pending, err = migrate.Pending(ctx, conn); err != nil {
		t.Fatalf("Pending after Apply: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("still pending after Apply: %v", pending)
	}

	// Apply runs on every dev startup; a second run over an up-to-date
	// schema must be a no-op.
	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply re-run: %v", err)
	}

	// The schema features later migrations and the server depend on
	// actually materialized: the hypertable, all five continuous
	// aggregates, and the toolkit extension the percentile columns need.
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM timescaledb_information.hypertables
		 WHERE hypertable_name = 'probe_results'`).Scan(&n); err != nil {
		t.Fatalf("hypertable check: %v", err)
	}
	if n != 1 {
		t.Errorf("probe_results hypertables = %d, want 1", n)
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM timescaledb_information.continuous_aggregates`).Scan(&n); err != nil {
		t.Fatalf("cagg check: %v", err)
	}
	if n != 5 {
		t.Errorf("continuous aggregates = %d, want 5 (hourly, daily, health, stage hourly, stage daily)", n)
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb_toolkit'`).Scan(&n); err != nil {
		t.Fatalf("toolkit check: %v", err)
	}
	if n != 1 {
		t.Error("timescaledb_toolkit extension missing after migrate")
	}
}

// A notx migration's DDL and its schema_migrations record are separate
// autocommit statements; a crash between the two leaves the DDL applied but
// unrecorded, and the next Apply re-executes the file. The package doc
// requires every notx file to converge under that re-run — simulate the
// crash for each one by deleting its record from a fully migrated database.
func TestNotxCrashBetweenDDLAndRecordConverges(t *testing.T) {
	ctx, conn := connect(t, dbtest.Empty(t))
	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	tag, err := conn.Exec(ctx,
		`DELETE FROM schema_migrations WHERE filename LIKE '%.notx.sql'`)
	if err != nil {
		t.Fatalf("unrecord notx migrations: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Skip("no notx migrations recorded")
	}

	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply after simulated crash re-runs notx files: %v", err)
	}
	if pending, err := migrate.Pending(ctx, conn); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("still pending after recovery: %v", pending)
	}
}
