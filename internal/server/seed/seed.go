// Package seed loads deterministic synthetic probe history straight into
// probe_results so the aggregate/percentile pipeline can be exercised (and
// gated) without waiting 90 days. It bypasses the ingest RPC on purpose:
// rows land via COPY with seed-owned probe IDs, so ingest strictness and
// outage hysteresis are untouched, and a re-run converges (delete + reload).
package seed

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	probeTypeICMP = int16(1) // pb.ProbeType_PROBE_TYPE_ICMP
	statusOK      = int16(1) // pb.ProbeStatus_PROBE_STATUS_OK
	statusTimeout = int16(2) // pb.ProbeStatus_PROBE_STATUS_TIMEOUT

	// Interval is the synthetic probe cadence. 60 s over 90 d ≈ 130k rows
	// per direction — enough that a raw 90 d scan visibly loses to the
	// hourly aggregate, small enough that COPY loads it in seconds.
	Interval = time.Minute
)

// Row is one synthetic probe_results row (ICMP-shaped: only RTT/jitter
// timings, everything else NULL).
type Row struct {
	Time     time.Time
	Status   int16
	Sent     int32
	Received int32
	LossPct  *float32
	RTTMinUS *int32
	RTTAvgUS *int32
	RTTMaxUS *int32
	RTTStdUS *int32
	JitterUS *int32
}

// pair is one seeded direction resolved against live enrollment state.
type pair struct {
	src, dst string
	agentID  uuid.UUID
	targetID uuid.UUID
	probeID  uuid.UUID
}

// key is the deterministic identity of a direction; it seeds the RNG and
// derives the probe ID, so re-runs regenerate identical history.
func (p pair) key() string { return p.src + "|" + p.dst }

// probeID derives the seed-owned probe UUID for a direction. Stable across
// runs so a re-seed can delete exactly its own prior rows.
func probeID(src, dst string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("polarbeam://seed/"+src+"|"+dst))
}

// GeneratePair produces n rows at Interval cadence starting at start. Fully
// deterministic in (key, start, n): per-pair base latency, a diurnal swing,
// a long-tailed noise term (so p99 separates from p50), RFC 3550-style
// jitter smoothing, ~0.5% packet loss, and one long outage window in the
// aggregate-only past plus (for half the pairs) a short recent one that
// lands inside the raw window.
func GeneratePair(key string, start time.Time, n int) []Row {
	h := fnv.New64a()
	h.Write([]byte(key))
	sum := h.Sum64()
	rng := rand.New(rand.NewSource(int64(sum)))

	baseUS := 20_000 + float64(sum%180_000)                  // 20–200 ms per pair
	outage1 := time.Duration(25*24+int(sum%240)) * time.Hour // starts day 25–35
	outage1End := outage1 + time.Duration(2+sum%5)*time.Hour // lasts 2–6 h
	hasRecent := sum%2 == 0
	outage2 := time.Duration(n)*Interval - 48*time.Hour // 2 days before end
	outage2End := outage2 + time.Hour

	rows := make([]Row, 0, n)
	jitter := 0.0
	prevRTT := -1.0
	for i := range n {
		t := start.Add(time.Duration(i) * Interval)
		off := time.Duration(i) * Interval
		if (off >= outage1 && off < outage1End) || (hasRecent && off >= outage2 && off < outage2End) {
			loss := float32(100)
			rows = append(rows, Row{Time: t, Status: statusTimeout, Sent: 10, Received: 0, LossPct: &loss})
			continue
		}

		dayFrac := float64(t.Unix()%86400) / 86400
		diurnal := 1 + 0.1*math.Sin(2*math.Pi*dayFrac)
		rtt := baseUS * diurnal * (1 + rng.ExpFloat64()*0.15)
		std := baseUS * 0.02 * (0.5 + rng.Float64())

		if prevRTT >= 0 {
			jitter += (math.Abs(rtt-prevRTT) - jitter) / 16
		}
		prevRTT = rtt

		received := int32(10)
		for range 10 {
			if rng.Float64() < 0.005 {
				received--
			}
		}
		loss := float32(10-received) * 10

		avg := int32(rtt)
		min := int32(math.Max(rtt-2*std, 1))
		max := int32(rtt + 3*std)
		stdv := int32(std)
		jit := int32(jitter)
		rows = append(rows, Row{
			Time: t, Status: statusOK, Sent: 10, Received: received, LossPct: &loss,
			RTTMinUS: &min, RTTAvgUS: &avg, RTTMaxUS: &max, RTTStdUS: &stdv, JitterUS: &jit,
		})
	}
	return rows
}

// Percentiles returns nearest-rank p50/p95/p99 over the rows' RTT averages
// (outage rows carry no RTT and are excluded — matching what percentile_agg
// sees, since aggregates skip NULLs).
func Percentiles(rows []Row) (p50, p95, p99 float64) {
	var vals []float64
	for _, r := range rows {
		if r.RTTAvgUS != nil {
			vals = append(vals, float64(*r.RTTAvgUS))
		}
	}
	if len(vals) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(vals)
	at := func(q float64) float64 {
		return vals[int(math.Round(q*float64(len(vals)-1)))]
	}
	return at(0.50), at(0.95), at(0.99)
}

// Run resolves every ordered site pair from live enrollment state, loads
// `days` of synthetic history for each, refreshes both continuous
// aggregates, and prints per-direction row counts and exact empirical
// percentiles for the gate to compare against the API.
func Run(ctx context.Context, pool *pgxpool.Pool, days int, out io.Writer) error {
	pairs, err := resolvePairs(ctx, pool)
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		return fmt.Errorf("seed: no ordered site pairs with enrolled agents and agent targets — run `make up` and wait for agents to enroll")
	}

	n := days * 24 * 60
	// End 2 minutes in the past so live probes stay the newest rows per
	// series and the matrix/status views keep reflecting reality.
	start := time.Now().Add(-2 * time.Minute).Add(-time.Duration(n) * Interval).Truncate(time.Second)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("seed: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Idempotency: seed probe IDs are deterministic, so this deletes
	// exactly the previous seed run and nothing else. The delete is
	// pair-wise so the (agent_id, probe_id, time) dedupe index prefix
	// serves each pair (a bare probe_id filter would seq-scan the whole
	// hypertable), crossing ALL agents with the probe IDs in Go so rows a
	// rotated source agent wrote in an earlier run are still caught —
	// agents are never deleted, so every historical seed row's agent_id
	// is present in agents.
	var allAgents []uuid.UUID
	rows, err := tx.Query(ctx, `SELECT id FROM agents`)
	if err != nil {
		return fmt.Errorf("seed: list agents: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("seed: scan agent: %w", err)
		}
		allAgents = append(allAgents, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed: list agents: %w", err)
	}
	agentIDs := make([]uuid.UUID, 0, len(allAgents)*len(pairs))
	probeIDs := make([]uuid.UUID, 0, len(allAgents)*len(pairs))
	for _, a := range allAgents {
		for _, p := range pairs {
			agentIDs = append(agentIDs, a)
			probeIDs = append(probeIDs, p.probeID)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM probe_results pr
		USING unnest($1::uuid[], $2::uuid[]) AS u(agent_id, probe_id)
		WHERE pr.agent_id = u.agent_id AND pr.probe_id = u.probe_id`,
		agentIDs, probeIDs); err != nil {
		return fmt.Errorf("seed: delete prior seed rows: %w", err)
	}
	// Mirror the raw cleanup for series_state: a rerun can pick a different
	// source agent for the same deterministic probe ID (enrollment order),
	// and the prior agent's state row would otherwise linger as a phantom
	// matrix check until it ages past the horizon.
	if _, err := tx.Exec(ctx, `
		DELETE FROM series_state ss
		USING unnest($1::uuid[], $2::uuid[]) AS u(agent_id, probe_id)
		WHERE ss.agent_id = u.agent_id AND ss.probe_id = u.probe_id`,
		agentIDs, probeIDs); err != nil {
		return fmt.Errorf("seed: delete prior seed state: %w", err)
	}

	type summary struct {
		p             pair
		rows          int
		p50, p95, p99 float64
	}
	summaries := make([]summary, 0, len(pairs))
	cols := []string{"time", "agent_id", "target_id", "probe_id", "probe_type", "status",
		"sent", "received", "loss_pct", "rtt_min_us", "rtt_avg_us", "rtt_max_us", "rtt_stddev_us", "jitter_us"}
	for _, p := range pairs {
		rows := GeneratePair(p.key(), start, n)
		src := make([][]any, len(rows))
		for i, r := range rows {
			src[i] = []any{r.Time, p.agentID, p.targetID, p.probeID, probeTypeICMP, r.Status,
				r.Sent, r.Received, r.LossPct, r.RTTMinUS, r.RTTAvgUS, r.RTTMaxUS, r.RTTStdUS, r.JitterUS}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"probe_results"}, cols, pgx.CopyFromRows(src)); err != nil {
			return fmt.Errorf("seed: copy %s→%s: %w", p.src, p.dst, err)
		}
		// The matrix serves "latest per series" from series_state (0023),
		// which production ingest maintains — seeded history must land
		// there too or a freshly seeded dashboard shows an empty matrix
		// until live agents report. Only the newest row matters; live
		// ingest that has already advanced past it wins via the last_time
		// guard.
		last := rows[len(rows)-1]
		var lastLatency *int64
		var lastSource *string
		if last.RTTAvgUS != nil {
			us := int64(*last.RTTAvgUS)
			family := "rtt"
			lastLatency, lastSource = &us, &family
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO series_state (agent_id, probe_id, target_id, probe_type,
				last_status, last_time, last_loss_pct, last_latency_us, last_latency_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (agent_id, probe_id) DO UPDATE SET
				target_id = EXCLUDED.target_id,
				probe_type = EXCLUDED.probe_type,
				last_status = EXCLUDED.last_status,
				last_time = EXCLUDED.last_time,
				last_loss_pct = EXCLUDED.last_loss_pct,
				last_latency_us = EXCLUDED.last_latency_us,
				last_latency_source = EXCLUDED.last_latency_source
			WHERE EXCLUDED.last_time > series_state.last_time`,
			p.agentID, p.probeID, p.targetID, probeTypeICMP,
			last.Status, last.Time, last.LossPct, lastLatency, lastSource); err != nil {
			return fmt.Errorf("seed: series_state %s→%s: %w", p.src, p.dst, err)
		}
		p50, p95, p99 := Percentiles(rows)
		summaries = append(summaries, summary{p, len(rows), p50, p95, p99})
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed: commit: %w", err)
	}

	// Refresh the caggs over everything now, before the retention job can
	// drop the >14 d raw region the hourly aggregate must fold first. The
	// CALLs manage their own transactions, so they need the simple protocol
	// and must not run inside an explicit tx.
	//
	// The health cagg's refresh is bounded one bucket behind now: a full
	// refresh would materialize the current partial bucket and push the
	// watermark past it, and real-time aggregation only reads raw ABOVE the
	// watermark — rows live agents insert into that bucket afterwards would
	// stay invisible until a policy run catches up. Bounding keeps the live
	// edge served from raw, exactly as the 30-min end_offset policy does in
	// production. Hourly/daily keep the full refresh: their readers are
	// 30d+ windows where the current partial bucket is invisible anyway.
	for _, v := range []struct{ view, end string }{
		{"probe_results_hourly", "NULL"}, // before daily: daily folds FROM hourly
		{"probe_results_daily", "NULL"},
		{"probe_results_health_30m", "now() - interval '30 minutes'"},
	} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CALL refresh_continuous_aggregate('%s', NULL, %s)`, v.view, v.end),
			pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("seed: refresh %s: %w", v.view, err)
		}
	}

	fmt.Fprintf(out, "seeded %d days (%d rows/direction, %s cadence), window %s → now-2m\n",
		days, n, Interval, start.Format(time.RFC3339))
	fmt.Fprintf(out, "expected empirical percentiles (µs, compare API p50/p95/p99 within ~5%%):\n")
	for _, s := range summaries {
		fmt.Fprintf(out, "  %s → %s: rows=%d p50=%.0f p95=%.0f p99=%.0f\n",
			s.p.src, s.p.dst, s.rows, s.p50, s.p95, s.p99)
	}
	fmt.Fprintf(out, "note: raw rows older than 14 d are dropped by retention later; hourly/daily keep them — that is the design under test\n")
	return nil
}

// resolvePairs returns one seeded direction per ordered site pair: the
// first agent of the source site probing the first agent-target of the
// destination site (matches how the dashboard folds pairs).
func resolvePairs(ctx context.Context, pool *pgxpool.Pool) ([]pair, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (ss.name, ds.name) ss.name, ds.name, sa.id, t.id
		  FROM agents sa
		  JOIN sites ss ON ss.id = sa.site_id
		  JOIN targets t ON t.agent_id IS NOT NULL
		  JOIN agents da ON da.id = t.agent_id AND da.id <> sa.id
		  JOIN sites ds ON ds.id = da.site_id AND ds.id <> ss.id
		 ORDER BY ss.name, ds.name, sa.id, t.id`)
	if err != nil {
		return nil, fmt.Errorf("seed: resolve pairs: %w", err)
	}
	defer rows.Close()
	var out []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.src, &p.dst, &p.agentID, &p.targetID); err != nil {
			return nil, fmt.Errorf("seed: resolve pairs: %w", err)
		}
		p.probeID = probeID(p.src, p.dst)
		out = append(out, p)
	}
	return out, rows.Err()
}
