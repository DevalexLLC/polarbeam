package store_test

// Columnstore (migration 0026) contract: reads over compressed chunks —
// raw and the compressed hourly caggs, including the toolkit uddsketch state
// that TimescaleDB serializes as text — return byte-identical results to
// the rowstore, and the late-arrival path into a compressed raw chunk
// (never taken in steady state, see 0026's offset chain) still honors the
// dedupe index and ON CONFLICT DO NOTHING.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

const (
	probeTypeICMP int16 = 1
	probeTypeHTTP int16 = 4
)

// refreshView materializes one cagg over [start, end) via the pool (the
// CALL manages its own transactions, so it must not run inside one).
func refreshView(t testing.TB, ctx context.Context, s *store.Store, view string, start, end time.Time) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx,
		`CALL refresh_continuous_aggregate('`+view+`', $1::timestamptz, $2::timestamptz)`,
		pgx.QueryExecModeSimpleProtocol, start, end); err != nil {
		t.Fatalf("refresh %s: %v", view, err)
	}
}

// compressOlderThan compresses every chunk of the hypertable (or cagg)
// whose range ends before now()-age and returns how many it converted.
func compressOlderThan(t testing.TB, ctx context.Context, s *store.Store, relation string, age time.Duration) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(compress_chunk(c)) FROM show_chunks($1::regclass, older_than => $2::interval) c`,
		relation, age).Scan(&n); err != nil {
		t.Fatalf("compress %s: %v", relation, err)
	}
	return n
}

// compressedChunks counts (compressed, total) chunks of a hypertable or a
// cagg's materialization hypertable.
func compressedChunks(t testing.TB, ctx context.Context, s *store.Store, relation string) (compressed, total int) {
	t.Helper()
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE c.is_compressed), count(*)
		  FROM timescaledb_information.chunks c
		 WHERE format('%I.%I', c.hypertable_schema, c.hypertable_name)::regclass = COALESCE(
		       (SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)::regclass
		          FROM timescaledb_information.continuous_aggregates WHERE view_name = $1),
		       $1::regclass)`, relation).Scan(&compressed, &total); err != nil {
		t.Fatalf("chunks of %s: %v", relation, err)
	}
	return compressed, total
}

func icmpRows(start time.Time, n int, step time.Duration, probeID, targetID uuid.UUID) []store.ResultRow {
	rows := make([]store.ResultRow, n)
	for i := range rows {
		rtt := int32(20000 + (i%7)*1500)
		jit := int32(300 + (i%5)*40)
		loss := float32(0)
		rows[i] = store.ResultRow{
			Time: start.Add(time.Duration(i) * step), TargetID: targetID, ProbeID: probeID,
			ProbeType: probeTypeICMP, Status: 1, Sent: 10, Received: 10, LossPct: &loss,
			RttMinUS: &rtt, RttAvgUS: &rtt, RttMaxUS: &rtt, RttStddevUS: &jit, JitterUS: &jit,
		}
	}
	return rows
}

func httpRows(start time.Time, n int, step time.Duration, probeID, targetID uuid.UUID) []store.ResultRow {
	rows := make([]store.ResultRow, n)
	for i := range rows {
		dns, tcp, tls := int32(1500+(i%3)*100), int32(9000+(i%4)*500), int32(30000+(i%6)*700)
		ttfb, total := int32(60000+(i%5)*900), int32(90000+(i%8)*1100)
		rows[i] = store.ResultRow{
			Time: start.Add(time.Duration(i) * step), TargetID: targetID, ProbeID: probeID,
			ProbeType: probeTypeHTTP, Status: 1, Sent: 1, Received: 1,
			DNSUS: &dns, TCPConnectUS: &tcp, TLSHandshakeUS: &tls, TTFBUS: &ttfb, TotalUS: &total,
		}
	}
	return rows
}

func TestCompressedChunksStayQueryable(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	dbtest.UnscheduleJobs(t, ctx, s.Pool())

	agentA := seedAgent(t, ctx, s, "cs-a", "cs-a1")
	agentB := seedAgent(t, ctx, s, "cs-b", "cs-b1")
	targetA := seedAgentTarget(t, ctx, s, agentA)
	targetB := seedAgentTarget(t, ctx, s, agentB)

	// 25-day-old history: older than the raw policy's 8 d and the caggs'
	// 10/12 d, inside every retention horizon. Ten minutes of ICMP at 30 s
	// plus HTTP at 1 min per direction, spread over two hours so the hourly
	// caggs get more than one bucket.
	old := time.Now().UTC().Add(-25 * 24 * time.Hour).Truncate(time.Hour)
	icmpAB, icmpBA, httpAB, httpBA := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	insertResults(t, ctx, s, agentA, icmpRows(old, 240, 30*time.Second, icmpAB, targetB))
	insertResults(t, ctx, s, agentB, icmpRows(old, 240, 30*time.Second, icmpBA, targetA))
	insertResults(t, ctx, s, agentA, httpRows(old, 120, time.Minute, httpAB, targetB))
	insertResults(t, ctx, s, agentB, httpRows(old, 120, time.Minute, httpBA, targetA))

	// Materialize the hourly views over the whole seeded window, but the
	// daily views only up to the start of the seeded day: the seeded day
	// then sits above the daily watermarks, so every daily read below is
	// served by the real-time tail folding FROM the hourly views — which
	// are compressed further down. The hourly views are compressed too,
	// so their own reads go through columnar chunks.
	winStart, winEnd := old.Add(-24*time.Hour), old.Add(48*time.Hour)
	dayStart := old.Truncate(24 * time.Hour)
	refreshView(t, ctx, s, "probe_results_hourly", winStart, winEnd)
	refreshView(t, ctx, s, "probe_results_stage_hourly", winStart, winEnd)
	// A refresh window must span at least one bucket: two whole days
	// ending where the seeded day begins.
	refreshView(t, ctx, s, "probe_results_daily", dayStart.Add(-48*time.Hour), dayStart)
	refreshView(t, ctx, s, "probe_results_stage_daily", dayStart.Add(-48*time.Hour), dayStart)

	type snapshot struct {
		rawSeries, hourlySeries, dailySeries []store.SeriesBucket
		rawSummary, hourlySummary, dailySum  *store.PairSummaryRow
		stageRaw, stageHourly, stageDaily    []store.StageBucket
		scores                               []store.SiteScore
	}
	capture := func() snapshot {
		t.Helper()
		var snap snapshot
		var err error
		src, dst := []uuid.UUID{agentA}, []uuid.UUID{targetB}
		// Raw-source reads over a window that reaches the 25-day-old rows:
		// after compression these scan the columnar raw chunk directly.
		if snap.rawSeries, err = s.PairSeries(ctx, src, dst, 5*time.Minute, 40*24*time.Hour, store.SourceRaw, "rtt"); err != nil {
			t.Fatalf("PairSeries raw: %v", err)
		}
		if snap.rawSummary, err = s.PairSummary(ctx, src, dst, 40*24*time.Hour, store.SourceRaw); err != nil {
			t.Fatalf("PairSummary raw: %v", err)
		}
		if snap.stageRaw, err = s.TargetStageSeries(ctx, src, targetB, 5*time.Minute, 40*24*time.Hour, store.SourceRaw); err != nil {
			t.Fatalf("TargetStageSeries raw: %v", err)
		}
		if snap.hourlySeries, err = s.PairSeries(ctx, src, dst, time.Hour, 40*24*time.Hour, store.SourceHourly, "rtt"); err != nil {
			t.Fatalf("PairSeries hourly: %v", err)
		}
		if snap.dailySeries, err = s.PairSeries(ctx, src, dst, 24*time.Hour, 400*24*time.Hour, store.SourceDaily, "rtt"); err != nil {
			t.Fatalf("PairSeries daily: %v", err)
		}
		if snap.hourlySummary, err = s.PairSummary(ctx, src, dst, 40*24*time.Hour, store.SourceHourly); err != nil {
			t.Fatalf("PairSummary hourly: %v", err)
		}
		if snap.dailySum, err = s.PairSummary(ctx, src, dst, 400*24*time.Hour, store.SourceDaily); err != nil {
			t.Fatalf("PairSummary daily: %v", err)
		}
		if snap.stageHourly, err = s.TargetStageSeries(ctx, src, targetB, time.Hour, 40*24*time.Hour, store.SourceHourly); err != nil {
			t.Fatalf("TargetStageSeries hourly: %v", err)
		}
		if snap.stageDaily, err = s.TargetStageSeries(ctx, src, targetB, 24*time.Hour, 400*24*time.Hour, store.SourceDaily); err != nil {
			t.Fatalf("TargetStageSeries daily: %v", err)
		}
		if snap.scores, err = s.SiteScores(ctx, time.Now().Add(-30*24*time.Hour), 6, nil); err != nil {
			t.Fatalf("SiteScores: %v", err)
		}
		return snap
	}
	before := capture()
	if len(before.rawSeries) == 0 || len(before.hourlySeries) == 0 || len(before.dailySeries) == 0 ||
		len(before.stageRaw) == 0 || len(before.stageHourly) == 0 || len(before.stageDaily) == 0 {
		t.Fatalf("fixture produced empty series: raw=%d hourly=%d daily=%d stageRaw=%d stageHourly=%d stageDaily=%d",
			len(before.rawSeries), len(before.hourlySeries), len(before.dailySeries),
			len(before.stageRaw), len(before.stageHourly), len(before.stageDaily))
	}
	if before.rawSummary.Samples == 0 {
		t.Fatal("raw summary saw no samples")
	}
	if before.hourlySummary.P95US == nil || before.dailySum.P95US == nil {
		t.Fatal("fixture produced no percentiles; the uddsketch round trip is untested")
	}
	// The daily views must really be serving the seeded day from their
	// real-time tail, or the hourly-fold contract below is vacuous.
	for _, view := range []string{"probe_results_daily", "probe_results_stage_daily"} {
		var visible, materialized int
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM `+view+` WHERE bucket >= $1 AND bucket < $2 AND agent_id = $3`,
			dayStart, dayStart.Add(24*time.Hour), agentA).Scan(&visible); err != nil {
			t.Fatal(err)
		}
		var matSchema, matTable string
		if err := s.Pool().QueryRow(ctx,
			`SELECT materialization_hypertable_schema, materialization_hypertable_name
			   FROM timescaledb_information.continuous_aggregates WHERE view_name = $1`, view).Scan(&matSchema, &matTable); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM `+pgx.Identifier{matSchema, matTable}.Sanitize()+` WHERE bucket >= $1 AND bucket < $2 AND agent_id = $3`,
			dayStart, dayStart.Add(24*time.Hour), agentA).Scan(&materialized); err != nil {
			t.Fatal(err)
		}
		if visible == 0 || materialized != 0 {
			t.Fatalf("%s: seeded day visible=%d materialized=%d, want visible rows served only by the real-time fold from hourly", view, visible, materialized)
		}
	}

	// Compress exactly what the shipped policies would: raw chunks whose
	// range ended more than 8 d ago, hourly cagg chunks more than 10 d ago.
	// The daily caggs stay rowstore (0026), but their reads are captured
	// too: they fold from the now-compressed hourly views' live tails.
	for relation, age := range map[string]time.Duration{
		"probe_results":              8 * 24 * time.Hour,
		"probe_results_hourly":       10 * 24 * time.Hour,
		"probe_results_stage_hourly": 10 * 24 * time.Hour,
	} {
		if n := compressOlderThan(t, ctx, s, relation, age); n == 0 {
			t.Errorf("%s: no chunk was eligible for compression", relation)
		}
		if c, total := compressedChunks(t, ctx, s, relation); c == 0 || c != total {
			t.Errorf("%s: %d of %d chunks compressed, want all (the fixture is entirely older than the policy offset)", relation, c, total)
		}
	}

	after := capture()
	if !reflect.DeepEqual(before, after) {
		t.Errorf("reads differ after compression:\n before %+v\n after  %+v", before, after)
	}

	// Late arrival into a compressed raw chunk: a duplicate of an existing
	// row is rejected by the dedupe index (0 inserted), a genuinely new row
	// lands (1 inserted), and a seed-style delete by (agent_id, probe_id)
	// still works. All three go through TimescaleDB's compressed-DML path.
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	dup := icmpRows(old, 1, time.Second, icmpAB, targetB)
	fresh := icmpRows(old.Add(90*time.Minute+15*time.Second), 1, time.Second, icmpAB, targetB) // off the 30 s grid
	inserted, err := store.InsertResultsTx(ctx, tx, agentA, append(dup, fresh...))
	if err != nil {
		t.Fatalf("late InsertResultsTx into compressed chunk: %v", err)
	}
	if len(inserted) != 1 || !inserted[0].Time.Equal(fresh[0].Time) {
		t.Errorf("late insert returned %d rows, want exactly the fresh one", len(inserted))
	}
	tag, err := tx.Exec(ctx, `DELETE FROM probe_results WHERE agent_id = $1 AND probe_id = $2`, agentA, icmpAB)
	if err != nil {
		t.Fatalf("delete from compressed chunk: %v", err)
	}
	if tag.RowsAffected() != 241 {
		t.Errorf("delete removed %d rows, want 241 (240 seeded + 1 late)", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
