package store_test

// DB-backed read-path benchmark for the dashboard queries, gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest) and run manually via
// `make bench` — never in CI. One run measures three things on one seeded
// 90-day fixture:
//
//  1. The real-time aggregate tail. Every cagg is materialized_only = false,
//     so a read unions the materialized chunks with raw rows above the
//     watermark; the tail length is set by the refresh policy's end_offset
//     and schedule. For each policy the harness establishes the shipped
//     cadence's worst-case watermark and the tightened candidate's, times
//     the reads at both, and prices one full bucket cycle of refresh calls
//     (no-op runs included) at each cadence on never-materialized buckets.
//  2. rowstore/ vs columnstore/: the same reads before and after the shipped
//     columnstore jobs (0026) have compressed every eligible chunk, with
//     hypertable sizes logged for both states.
//  3. Plans: with POLARBEAM_BENCH_EXPLAIN=1 every measured statement's
//     EXPLAIN (ANALYZE, BUFFERS) is written to the server log through
//     auto_explain (read it with `docker logs` on the throwaway container).
//
// Contexts: the fixture is large, so setup gets its own long deadline
// instead of newStore's two-minute test budget, and each timed call gets a
// fresh short one.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/seed"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

const (
	benchDays               = 90
	benchHTTPCadence        = 5 * time.Minute
	excludeTraceroute int16 = 6
)

type benchDB struct {
	b    *testing.B
	ctx  context.Context
	s    *store.Store
	dirs []seed.Direction
}

func (d *benchDB) exec(sql string, args ...any) {
	d.b.Helper()
	if _, err := d.s.Pool().Exec(d.ctx, sql, args...); err != nil {
		d.b.Fatalf("%s: %v", sql, err)
	}
}

// call runs a CALL statement (its own transactions, simple protocol).
func (d *benchDB) call(sql string, args ...any) {
	d.b.Helper()
	if _, err := d.s.Pool().Exec(d.ctx, sql, append([]any{pgx.QueryExecModeSimpleProtocol}, args...)...); err != nil {
		d.b.Fatalf("%s: %v", sql, err)
	}
}

// watermark returns a cagg's materialization watermark.
func (d *benchDB) watermark(view string) time.Time {
	d.b.Helper()
	var wm time.Time
	if err := d.s.Pool().QueryRow(d.ctx, `
		SELECT _timescaledb_functions.to_timestamp(_timescaledb_functions.cagg_watermark(h.id))
		  FROM timescaledb_information.continuous_aggregates ca
		  JOIN _timescaledb_catalog.hypertable h
		    ON h.schema_name = ca.materialization_hypertable_schema AND h.table_name = ca.materialization_hypertable_name
		 WHERE ca.view_name = $1`, view).Scan(&wm); err != nil {
		d.b.Fatalf("watermark %s: %v", view, err)
	}
	return wm
}

func (d *benchDB) refreshTo(view string, end time.Time) {
	d.b.Helper()
	if err := seed.Refresh(d.ctx, d.s.Pool(), end, view); err != nil {
		d.b.Fatal(err)
	}
}

// refreshWindow times one refresh call over [start, end) — a bounded window
// like the policy's, never a NULL start.
func (d *benchDB) refreshWindow(view string, start, end time.Time) time.Duration {
	d.b.Helper()
	t0 := time.Now()
	d.call(`CALL refresh_continuous_aggregate('`+view+`', $1::timestamptz, $2::timestamptz)`, start, end)
	return time.Since(t0)
}

func (d *benchDB) rawRowsIn(start, end time.Time) int64 {
	d.b.Helper()
	var n int64
	if err := d.s.Pool().QueryRow(d.ctx,
		`SELECT count(*) FROM probe_results WHERE time >= $1 AND time < $2`, start, end).Scan(&n); err != nil {
		d.b.Fatalf("count raw rows: %v", err)
	}
	return n
}

// relations lists the raw hypertable and every cagg's materialization
// hypertable (timescaledb_information.hypertables omits the latter).
const benchRelationsSQL = `
	SELECT hypertable_name AS name, hypertable_schema AS schema, hypertable_name AS table
	  FROM timescaledb_information.hypertables
	UNION ALL
	SELECT view_name, materialization_hypertable_schema, materialization_hypertable_name
	  FROM timescaledb_information.continuous_aggregates`

func (d *benchDB) logSizes(label string) {
	d.b.Helper()
	rows, err := d.s.Pool().Query(d.ctx, `
		WITH r AS (`+benchRelationsSQL+`)
		SELECT r.name, hypertable_size(format('%I.%I', r.schema, r.table)::regclass),
		       (SELECT count(*) FILTER (WHERE c.is_compressed) FROM timescaledb_information.chunks c
		         WHERE c.hypertable_schema = r.schema AND c.hypertable_name = r.table),
		       (SELECT count(*) FROM timescaledb_information.chunks c
		         WHERE c.hypertable_schema = r.schema AND c.hypertable_name = r.table)
		  FROM r ORDER BY 1`)
	if err != nil {
		d.b.Fatalf("sizes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var bytes, compressed, total int64
		if err := rows.Scan(&name, &bytes, &compressed, &total); err != nil {
			d.b.Fatalf("scan sizes: %v", err)
		}
		d.b.Logf("size[%s] %-27s %8.1f MB  chunks %d/%d compressed", label, name, float64(bytes)/1e6, compressed, total)
	}
}

func (d *benchDB) logColumnstoreStats() {
	d.b.Helper()
	rows, err := d.s.Pool().Query(d.ctx, `
		WITH r AS (`+benchRelationsSQL+`)
		SELECT r.name, s.before, s.after
		  FROM r
		  CROSS JOIN LATERAL (
		    SELECT sum(before_compression_total_bytes) AS before,
		           sum(after_compression_total_bytes)  AS after
		      FROM hypertable_columnstore_stats(format('%I.%I', r.schema, r.table)::regclass)) s
		 WHERE s.after IS NOT NULL
		 ORDER BY 1`)
	if err != nil {
		d.b.Fatalf("columnstore stats: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var before, after int64
		if err := rows.Scan(&name, &before, &after); err != nil {
			d.b.Fatalf("scan stats: %v", err)
		}
		d.b.Logf("columnstore %-27s compressed chunks %8.1f MB -> %8.1f MB  (%.1fx)", name,
			float64(before)/1e6, float64(after)/1e6, float64(before)/float64(max(after, 1)))
	}
}

// timed runs fn under a sub-benchmark with a fresh context per iteration.
func (d *benchDB) timed(name string, fn func(ctx context.Context) error) {
	d.b.Run(name, func(b *testing.B) {
		for b.Loop() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := fn(ctx); err != nil {
				cancel()
				b.Fatal(err)
			}
			cancel()
		}
	})
}

func BenchmarkDashboardReads(b *testing.B) {
	url := dbtest.Migrated(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s, err := store.Connect(ctx, url, 10*time.Second, 0)
	if err != nil {
		b.Fatalf("store.Connect: %v", err)
	}
	defer s.Close()
	d := &benchDB{b: b, ctx: ctx, s: s}
	dbtest.UnscheduleJobs(b, ctx, s.Pool())
	dbtest.LogServerFlags(b, ctx, s.Pool())

	if os.Getenv("POLARBEAM_BENCH_EXPLAIN") != "" {
		// Per-database session preload: every NEW session logs each
		// statement's EXPLAIN (ANALYZE, BUFFERS) to the server log. Reconnect
		// so the pool's sessions pick it up.
		var dbName string
		if err := s.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
			b.Fatal(err)
		}
		for _, set := range []string{
			`session_preload_libraries = 'auto_explain'`, `auto_explain.log_min_duration = 0`,
			`auto_explain.log_analyze = on`, `auto_explain.log_buffers = on`, `auto_explain.log_timing = on`,
			`auto_explain.log_nested_statements = off`,
		} {
			d.exec(fmt.Sprintf(`ALTER DATABASE %s SET %s`, pgx.Identifier{dbName}.Sanitize(), set))
		}
		s.Close()
		if s, err = store.Connect(ctx, url, 10*time.Second, 0); err != nil {
			b.Fatalf("store.Connect (auto_explain): %v", err)
		}
		defer s.Close()
		d.s = s
		b.Logf("auto_explain enabled on %s: plans are in the server log", dbName)
	}

	// Fixture: three sites, one agent each with its agent-kind target (six
	// directions), 90 d of ICMP at the seed cadence, 90 d of HTTP-shaped
	// rows with stage timings at 5 min, and an enabled direct probe config
	// per fixture probe (AgentProbeHealth intersects series_state with the
	// enabled set).
	agents := map[string]uuid.UUID{}
	for _, site := range []string{"bench-a", "bench-b", "bench-c"} {
		id := seedAgent(b, ctx, s, site, site+"-1")
		seedAgentTarget(b, ctx, s, id)
		agents[site] = id
	}
	dirs, err := seed.Load(ctx, s.Pool(), benchDays, io.Discard)
	if err != nil {
		b.Fatal(err)
	}
	if len(dirs) != 6 {
		b.Fatalf("seed.Load resolved %d directions, want 6", len(dirs))
	}
	d.dirs = dirs
	// Anchor the newest HTTP sample to the latest cadence boundary, like a
	// deployment probing every five minutes, so every watermark below has
	// a populated live tail (a fixture ending an hour short would leave
	// the candidate hourly watermark's tail empty and bias the comparison).
	httpPerDir := benchDays * 24 * 60 / int(benchHTTPCadence/time.Minute)
	httpStart := time.Now().Truncate(benchHTTPCadence).Add(-time.Duration(httpPerDir-1) * benchHTTPCadence)
	for _, dir := range dirs {
		httpProbe := uuid.NewSHA1(uuid.NameSpaceURL, []byte("polarbeam://bench/http/"+dir.Src+"|"+dir.Dst))
		const perCall = 2016 // one week at 5 min
		rows := httpRows(httpStart, httpPerDir, benchHTTPCadence, httpProbe, dir.TargetID)
		for i := 0; i < len(rows); i += perCall {
			insertResults(b, ctx, s, dir.AgentID, rows[i:min(i+perCall, len(rows))])
		}
		for _, p := range []struct {
			id        uuid.UUID
			probeType int16
			interval  time.Duration
		}{{dir.ProbeID, probeTypeICMP, seed.Interval}, {httpProbe, probeTypeHTTP, benchHTTPCadence}} {
			d.exec(`INSERT INTO probe_configs (id, site_id, target_id, network_id, probe_type, interval_ms, timeout_ms, enabled, updated_by)
			        SELECT $1, site_id, $3, network_id, $4, $5, 5000, true, 'bench' FROM agents WHERE id = $2`,
				p.id, dir.AgentID, dir.TargetID, p.probeType, p.interval.Milliseconds())
		}
	}
	s.InvalidateConfigCaches()
	b.Logf("fixture: %d raw rows", d.rawRowsIn(time.Time{}, time.Now().Add(time.Hour)))

	// --- 1. Real-time tail: refresh cadence experiment, per policy. ---
	type policy struct {
		view                  string
		bucket                time.Duration
		baseOffset, baseEvery time.Duration // shipped end_offset / schedule_interval
		candOffset, candEvery time.Duration // candidate
		reads                 func(ctx context.Context) error
		readName              string
	}
	firstDir := store.DirectionKey{SrcAgents: []uuid.UUID{dirs[0].AgentID}, DstTargets: []uuid.UUID{dirs[0].TargetID}}
	policies := []policy{
		{view: seed.ViewHourly, bucket: time.Hour, baseOffset: time.Hour, baseEvery: time.Hour, candOffset: 15 * time.Minute, candEvery: 15 * time.Minute,
			readName: "SiteScores/30d", reads: func(ctx context.Context) error {
				_, err := s.SiteScores(ctx, time.Now().Add(-30*24*time.Hour), excludeTraceroute, nil)
				return err
			}},
		{view: seed.ViewStageHourly, bucket: time.Hour, baseOffset: time.Hour, baseEvery: time.Hour, candOffset: 15 * time.Minute, candEvery: 15 * time.Minute,
			readName: "TargetStageSeries/90d-stage-hourly", reads: func(ctx context.Context) error {
				_, err := s.TargetStageSeries(ctx, firstDir.SrcAgents, dirs[0].TargetID, 3*time.Hour, 90*24*time.Hour, store.SourceHourly)
				return err
			}},
		{view: seed.ViewHealth30m, bucket: 30 * time.Minute, baseOffset: 30 * time.Minute, baseEvery: 30 * time.Minute, candOffset: 5 * time.Minute, candEvery: 5 * time.Minute,
			readName: "AgentHealthSeries+AgentProbeHealth/24h", reads: func(ctx context.Context) error {
				if _, err := s.AgentHealthSeries(ctx, 24*time.Hour, 30*time.Minute, excludeTraceroute, nil); err != nil {
					return err
				}
				_, err := s.AgentProbeHealth(ctx, dirs[0].AgentID, 24*time.Hour, 30*time.Minute, nil)
				return err
			}},
	}
	for _, p := range policies {
		// Cost side: untimed initial refresh to a bucket-aligned W0, then
		// one full bucket cycle of policy-shaped calls per cadence over
		// adjacent, never-materialized buckets with equal raw row counts.
		w0 := time.Now().Truncate(p.bucket).Add(-4 * p.bucket)
		d.refreshTo(p.view, w0)
		if wm := d.watermark(p.view); !wm.Equal(w0) {
			b.Fatalf("%s: watermark %s after initial refresh, want %s", p.view, wm, w0)
		}
		b1, b2 := w0, w0.Add(p.bucket)
		if n1, n2 := d.rawRowsIn(b1, b1.Add(p.bucket)), d.rawRowsIn(b2, b2.Add(p.bucket)); n1 != n2 {
			b.Fatalf("%s: cycle buckets hold %d and %d raw rows, want equal", p.view, n1, n2)
		}
		cycle := func(label string, from time.Time, offset, every time.Duration) {
			var total time.Duration
			runs := int(p.bucket / every)
			for k := 1; k <= runs; k++ {
				// Scheduled run k after the bucket closed refreshes up to
				// (invocation time - end_offset).
				end := from.Add(p.bucket).Add(time.Duration(k)*every - offset)
				total += d.refreshWindow(p.view, from.Add(-p.bucket), end)
				wm := d.watermark(p.view)
				if !wm.Equal(wm.Truncate(p.bucket)) {
					b.Fatalf("%s: watermark %s not bucket-aligned", p.view, wm)
				}
			}
			if wm := d.watermark(p.view); !wm.Equal(from.Add(p.bucket)) {
				b.Fatalf("%s %s cycle: watermark %s, want %s (exactly one bucket materialized)", p.view, label, wm, from.Add(p.bucket))
			}
			b.Logf("refresh-cost[%s] %-10s end_offset=%v every=%v: %d runs, %v per bucket cycle", p.view, label, offset, every, runs, total)
		}
		cycle("candidate", b1, p.candOffset, p.candEvery)
		cycle("shipped", b2, p.baseOffset, p.baseEvery)

		// Read side: the shipped cadence's worst-case watermark, then the
		// candidate's; both floor to the bucket.
		for _, st := range []struct {
			label string
			end   time.Time
		}{
			{"tail-shipped", time.Now().Add(-(p.baseOffset + p.baseEvery))},
			{"tail-candidate", time.Now().Add(-(p.candOffset + p.candEvery))},
		} {
			before := d.watermark(p.view)
			d.refreshTo(p.view, st.end)
			wm := d.watermark(p.view)
			if st.label == "tail-candidate" && !wm.After(before) {
				b.Fatalf("%s: candidate watermark %s did not move past %s", p.view, wm, before)
			}
			b.Logf("tail[%s] %s: watermark %s, tail %v", p.view, st.label, wm.Format(time.RFC3339), time.Since(wm).Round(time.Second))
			d.timed(st.label+"/"+p.readName, p.reads)
		}
	}

	// --- 2. Materialize everything (daily views fold from hourly) and
	// measure the rowstore. ---
	if err := seed.Refresh(ctx, s.Pool(), time.Now()); err != nil {
		b.Fatal(err)
	}
	for _, view := range seed.AllViews {
		var n int64
		if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM `+view+` WHERE bucket < now() - interval '13 days'`).Scan(&n); err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatalf("%s holds no historical materialization", view)
		}
	}
	// Production never holds 90 days of raw chunks or 90 days of health
	// strips: the shipped retention policies drop them past 14 d. Run every
	// retention job now (after the caggs have folded everything) so the
	// rowstore/columnstore comparison plans over the chunk set a real
	// deployment has — the raw-window and health queries exclude cold
	// chunks at runtime, and the cost of that grows with chunk count. The
	// 100/400 d rollup policies find nothing to drop in a 90 d fixture.
	retentionJobs, err := s.Pool().Query(ctx, `
		SELECT job_id FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' ORDER BY job_id`)
	if err != nil {
		b.Fatalf("retention jobs: %v", err)
	}
	var retention []int
	for retentionJobs.Next() {
		var id int
		if err := retentionJobs.Scan(&id); err != nil {
			b.Fatal(err)
		}
		retention = append(retention, id)
	}
	retentionJobs.Close()
	for _, id := range retention {
		d.call(fmt.Sprintf(`CALL run_job(%d)`, id))
	}
	d.logSizes("rowstore")
	reads := func(label string) {
		defer d.logStatements(label)
		d.resetStatements()
		allDirs := make([]store.DirectionKey, len(dirs))
		for i, dir := range dirs {
			allDirs[i] = store.DirectionKey{SrcAgents: []uuid.UUID{dir.AgentID}, DstTargets: []uuid.UUID{dir.TargetID}}
		}
		for _, w := range []struct {
			name   string
			window time.Duration
			bucket time.Duration
			source store.Source
		}{
			{"7d-raw", 7 * 24 * time.Hour, 5 * time.Minute, store.SourceRaw},
			{"30d-hourly", 30 * 24 * time.Hour, time.Hour, store.SourceHourly},
			{"90d-hourly", 90 * 24 * time.Hour, 3 * time.Hour, store.SourceHourly},
			{"365d-daily", 365 * 24 * time.Hour, 24 * time.Hour, store.SourceDaily},
		} {
			d.timed(label+"/PairDirectionSeries/"+w.name, func(ctx context.Context) error {
				_, err := s.PairDirectionSeries(ctx, allDirs[:2], w.bucket, w.window, w.source)
				return err
			})
		}
		d.timed(label+"/PairDirectionSummaries/30d", func(ctx context.Context) error {
			_, err := s.PairDirectionSummaries(ctx, allDirs[:2], 30*24*time.Hour, store.SourceHourly, 10*time.Minute)
			return err
		})
		d.timed(label+"/SiteScores/30d", func(ctx context.Context) error {
			_, err := s.SiteScores(ctx, time.Now().Add(-30*24*time.Hour), excludeTraceroute, nil)
			return err
		})
		d.timed(label+"/AgentHealthSeries/24h", func(ctx context.Context) error {
			_, err := s.AgentHealthSeries(ctx, 24*time.Hour, 30*time.Minute, excludeTraceroute, nil)
			return err
		})
		for _, w := range []struct {
			name   string
			window time.Duration
			bucket time.Duration
			source store.Source
		}{
			{"7d-raw", 7 * 24 * time.Hour, 5 * time.Minute, store.SourceRaw},
			{"90d-stage-hourly", 90 * 24 * time.Hour, 3 * time.Hour, store.SourceHourly},
			{"365d-stage-daily", 365 * 24 * time.Hour, 24 * time.Hour, store.SourceDaily},
		} {
			d.timed(label+"/TargetStageSeries/"+w.name, func(ctx context.Context) error {
				_, err := s.TargetStageSeries(ctx, firstDir.SrcAgents, dirs[0].TargetID, w.bucket, w.window, w.source)
				return err
			})
		}
	}
	reads("rowstore")

	// --- 3. Run the shipped columnstore jobs and measure again. ---
	jobs, err := s.Pool().Query(ctx, `
		SELECT job_id, hypertable_name, config->>'compress_after'
		  FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression' ORDER BY job_id`)
	if err != nil {
		b.Fatal(err)
	}
	type compressionJob struct {
		id    int
		table string
		after string
	}
	var cjobs []compressionJob
	for jobs.Next() {
		var j compressionJob
		if err := jobs.Scan(&j.id, &j.table, &j.after); err != nil {
			b.Fatal(err)
		}
		cjobs = append(cjobs, j)
	}
	jobs.Close()
	if len(cjobs) == 0 {
		b.Fatal("no policy_compression jobs; is migration 0026 applied?")
	}
	for _, j := range cjobs {
		t0 := time.Now()
		d.call(fmt.Sprintf(`CALL run_job(%d)`, j.id))
		var eligibleUncompressed, youngCompressed int
		if err := s.Pool().QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE NOT c.is_compressed AND c.range_end < now() - $2::interval),
			       count(*) FILTER (WHERE c.is_compressed AND c.range_end >= now() - $2::interval)
			  FROM timescaledb_information.chunks c
			 WHERE format('%I.%I', c.hypertable_schema, c.hypertable_name)::regclass = COALESCE(
			       (SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)::regclass
			          FROM timescaledb_information.continuous_aggregates WHERE view_name = $1),
			       $1::regclass)`, j.table, j.after).Scan(&eligibleUncompressed, &youngCompressed); err != nil {
			b.Fatal(err)
		}
		if eligibleUncompressed != 0 || youngCompressed != 0 {
			b.Fatalf("%s after run_job: %d eligible chunks still uncompressed, %d chunks younger than %s compressed",
				j.table, eligibleUncompressed, youngCompressed, j.after)
		}
		b.Logf("columnstore job %d (%s, compress_after %s) ran in %v", j.id, j.table, j.after, time.Since(t0).Round(time.Millisecond))
	}
	d.logSizes("columnstore")
	d.logColumnstoreStats()
	reads("columnstore")
}

// statementsAvailable reports whether pg_stat_statements is preloaded on
// the measured server (the compose file preloads it; a bare throwaway
// container does not).
func (d *benchDB) statementsAvailable() bool {
	var preload string
	if err := d.s.Pool().QueryRow(d.ctx, `SELECT current_setting('shared_preload_libraries')`).Scan(&preload); err != nil {
		d.b.Fatal(err)
	}
	return strings.Contains(preload, "pg_stat_statements")
}

func (d *benchDB) resetStatements() {
	if !d.statementsAvailable() {
		return
	}
	d.exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`)
	d.exec(`SELECT pg_stat_statements_reset()`)
}

// logStatements logs planning vs execution time per statement since the
// last reset — planning cost on a hypertable grows with the number of
// chunks the planner has to open, not the number a query reads, so this
// is where a compressed-chunk regression would show. Needs
// pg_stat_statements.track_planning=on for the plan columns.
func (d *benchDB) logStatements(label string) {
	if !d.statementsAvailable() {
		d.b.Logf("statements[%s]: pg_stat_statements not preloaded; plan/exec split unavailable", label)
		return
	}
	rows, err := d.s.Pool().Query(d.ctx, `
		SELECT left(regexp_replace(query, '\s+', ' ', 'g'), 70), calls,
		       round((total_plan_time / nullif(calls, 0))::numeric, 3),
		       round((total_exec_time / nullif(calls, 0))::numeric, 3)
		  FROM pg_stat_statements
		 WHERE calls >= 5 AND query NOT ILIKE '%pg_stat_statements%'
		 ORDER BY total_exec_time DESC LIMIT 12`)
	if err != nil {
		d.b.Fatalf("pg_stat_statements: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var q string
		var calls int64
		var planMS, execMS float64
		if err := rows.Scan(&q, &calls, &planMS, &execMS); err != nil {
			d.b.Fatal(err)
		}
		d.b.Logf("statements[%s] plan %7.3f ms  exec %8.3f ms  calls %4d  %s", label, planMS, execMS, calls, q)
	}
}
