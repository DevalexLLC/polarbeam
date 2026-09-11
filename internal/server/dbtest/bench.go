package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Measurement helpers shared by the DB-backed benchmarks. They act on the
// throwaway database (and, for WAL counters, the throwaway cluster) that
// EnvURL points at — never on a deployment.

// UnscheduleJobs turns off every TimescaleDB policy job in the connected
// database so refresh, retention, and columnstore jobs cannot move
// watermarks, drop chunks, or compress behind a measurement. Test databases
// are clones, so a real deployment's schedule is never touched.
func UnscheduleJobs(tb testing.TB, ctx context.Context, pool *pgxpool.Pool) {
	tb.Helper()
	if _, err := pool.Exec(ctx, `
		SELECT alter_job(job_id, scheduled => false)
		  FROM timescaledb_information.jobs
		 WHERE job_id >= 1000 AND scheduled`); err != nil {
		tb.Fatalf("unschedule jobs: %v", err)
	}
}

// LogServerFlags records the server settings a measurement depends on, so
// the benchmark log proves which configuration it ran against rather than
// assuming the compose flags reached the disposable server.
func LogServerFlags(tb testing.TB, ctx context.Context, pool *pgxpool.Pool) {
	tb.Helper()
	var wal, preload, io, ver string
	if err := pool.QueryRow(ctx, `
		SELECT current_setting('wal_compression'), current_setting('shared_preload_libraries'),
		       current_setting('track_io_timing'), current_setting('server_version')`).
		Scan(&wal, &preload, &io, &ver); err != nil {
		tb.Fatalf("server flags: %v", err)
	}
	tb.Logf("server %s: wal_compression=%s shared_preload_libraries=%s track_io_timing=%s", ver, wal, preload, io)
}

// WALMeter brackets a benchmark's timed loop with the cluster-wide WAL
// counters so the reported walB/op and fpi/op cover only the measured
// workload — not the template migration, database clone, or fixture load
// that precede it. The caller must be the only writer on the cluster while
// the bracket is open (run one package at a time).
type WALMeter struct {
	pool         *pgxpool.Pool
	bytes0, fpi0 int64
}

// StartWALMeter unschedules the database's jobs, forces a checkpoint (so
// the first touch of each page inside the bracket pays its full-page image
// the same way on every run), flushes the statistics, and records the
// counters. Call before the timed loop; Report after it.
func StartWALMeter(b *testing.B, ctx context.Context, pool *pgxpool.Pool) *WALMeter {
	b.Helper()
	UnscheduleJobs(b, ctx, pool)
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		b.Fatalf("checkpoint: %v", err)
	}
	m := &WALMeter{pool: pool}
	m.bytes0, m.fpi0 = m.read(b, ctx)
	return m
}

func (m *WALMeter) read(b *testing.B, ctx context.Context) (bytes, fpi int64) {
	b.Helper()
	if _, err := m.pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		b.Fatalf("flush stats: %v", err)
	}
	if err := m.pool.QueryRow(ctx, `SELECT wal_bytes::bigint, wal_fpi::bigint FROM pg_stat_wal`).Scan(&bytes, &fpi); err != nil {
		b.Fatalf("pg_stat_wal: %v", err)
	}
	return bytes, fpi
}

// Report stops the timer and attaches walB/op and fpi/op to the benchmark.
func (m *WALMeter) Report(b *testing.B, ctx context.Context) {
	b.Helper()
	b.StopTimer()
	bytes, fpi := m.read(b, ctx)
	if n := b.N; n > 0 {
		b.ReportMetric(float64(bytes-m.bytes0)/float64(n), "walB/op")
		b.ReportMetric(float64(fpi-m.fpi0)/float64(n), "fpi/op")
	}
}
