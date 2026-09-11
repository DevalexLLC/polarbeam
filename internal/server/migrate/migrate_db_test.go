package migrate_test

// DB-backed migration tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). Shipped migrations are immutable, so a file that
// fails to apply on a fresh TimescaleDB can only be repaired with a follow-up
// migration forever — this is the one place that failure is caught before a
// release. External test package: dbtest imports migrate.

import (
	"context"
	"maps"
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
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	// dbtest URLs cap pool_max_conns for pooled clients; a single pgx
	// connection must not forward it to the server as a GUC.
	delete(cfg.RuntimeParams, "pool_max_conns")
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return ctx, conn
}

func TestApplyOnFreshDatabase(t *testing.T) {
	t.Parallel()
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

	// 0026: columnstore settings on the raw hypertable and on the two
	// hourly caggs' materialization hypertables (health_30m and the daily
	// caggs stay rowstore), each with a compression policy at the
	// documented offset.
	// Settings are keyed by the materialization hypertable, so resolve
	// view names through continuous_aggregates.
	wantSettings := map[string][2]string{
		"probe_results":              {"agent_id,target_id,probe_id", `"time" DESC`},
		"probe_results_hourly":       {"agent_id,target_id,probe_type,latency_source", "bucket DESC"},
		"probe_results_stage_hourly": {"agent_id,target_id,probe_type", "bucket DESC"},
	}
	// The view lists every hypertable; those without columnstore settings
	// have NULL segmentby/orderby.
	rows, err := conn.Query(ctx, `
		SELECT COALESCE(ca.view_name, s.hypertable::text), s.segmentby, s.orderby
		  FROM timescaledb_information.hypertable_columnstore_settings s
		  LEFT JOIN timescaledb_information.continuous_aggregates ca
		    ON format('%I.%I', ca.materialization_hypertable_schema, ca.materialization_hypertable_name)::regclass = s.hypertable
		 WHERE s.segmentby IS NOT NULL OR s.orderby IS NOT NULL`)
	if err != nil {
		t.Fatalf("columnstore settings: %v", err)
	}
	gotSettings := map[string][2]string{}
	for rows.Next() {
		var name, segmentby, orderby string
		if err := rows.Scan(&name, &segmentby, &orderby); err != nil {
			t.Fatalf("scan settings: %v", err)
		}
		gotSettings[name] = [2]string{segmentby, orderby}
	}
	rows.Close()
	if !maps.Equal(gotSettings, wantSettings) {
		t.Errorf("columnstore settings = %v, want %v", gotSettings, wantSettings)
	}

	wantAfter := map[string]string{
		"probe_results":              "8 days",
		"probe_results_hourly":       "10 days",
		"probe_results_stage_hourly": "10 days",
	}
	rows, err = conn.Query(ctx, `
		SELECT hypertable_name, config->>'compress_after'
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_compression'`)
	if err != nil {
		t.Fatalf("compression jobs: %v", err)
	}
	gotAfter := map[string]string{}
	for rows.Next() {
		var name, after string
		if err := rows.Scan(&name, &after); err != nil {
			t.Fatalf("scan jobs: %v", err)
		}
		gotAfter[name] = after
	}
	rows.Close()
	if !maps.Equal(gotAfter, wantAfter) {
		t.Errorf("compression policies = %v, want %v", gotAfter, wantAfter)
	}
}

// A notx migration's DDL and its schema_migrations record are separate
// autocommit statements; a crash between the two leaves the DDL applied but
// unrecorded, and the next Apply re-executes the file. The package doc
// requires every notx file to converge under that re-run — simulate the
// crash for each one by deleting its record from a fully migrated database.
func TestNotxCrashBetweenDDLAndRecordConverges(t *testing.T) {
	t.Parallel()
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
