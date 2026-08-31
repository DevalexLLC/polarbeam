package store_test

// Parity suite for the batched read path (dashboard_batch.go): every batch
// method must return exactly what its single-shot reference implementation
// returns, on the raw AND the cagg arms. This is also the first DB-backed
// coverage of PairSummary/PairSeries/PairLatencySource/DirectionLatest —
// the httpapi tests fake the store, so until now nothing executed the pair
// SQL against a real schema.
//
// The store connects with maxConns = 1 on purpose: a batch result that is
// not fully drained and closed before the next SendBatch would pin the
// pool's only connection and hang the test instead of passing by luck.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func newParityStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s, err := store.Connect(ctx, url, 10*time.Second, 1)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	t.Cleanup(s.Close)
	return ctx, s
}

// seedAgentTarget gives an agent its agent-kind target row (what
// SiteEndpoints' triples join requires) and returns the target ID.
func seedAgentTarget(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO targets (id, kind, name, agent_id) VALUES ($1, 'agent', $2, $3)`,
		id, "agent:"+agentID.String(), agentID); err != nil {
		t.Fatalf("insert agent target: %v", err)
	}
	return id
}

func TestPairBatchParity(t *testing.T) {
	t.Parallel()
	ctx, s := newParityStore(t)

	// Two staffed sites on the default plane plus one on mgmt only (the
	// scope-invisible case for a default-scoped caller).
	agentA := seedAgent(t, ctx, s, "pair-a", "a1")
	agentB := seedAgent(t, ctx, s, "pair-b", "b1")
	targetA := seedAgentTarget(t, ctx, s, agentA)
	targetB := seedAgentTarget(t, ctx, s, agentB)
	mgmt := createNetwork(t, ctx, s, "mgmt")
	siteCID, err := s.EnsureSite(ctx, "pair-c")
	if err != nil {
		t.Fatalf("EnsureSite pair-c: %v", err)
	}
	agentC := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO agents (id, site_id, network_id, hostname) VALUES ($1, $2, $3, 'c1')`,
		agentC, siteCID, mgmt); err != nil {
		t.Fatalf("insert mgmt agent: %v", err)
	}
	seedAgentTarget(t, ctx, s, agentC)

	// A→B skews heavily to tcp_connect with a lone rtt row under the 5%
	// coverage floor (rtt must lose despite higher priority); B→A is
	// rtt-dominant. Timestamps sit minutes in the past so now()-relative
	// window edges cannot flake between the two executions under compare.
	i32 := func(v int32) *int32 { return &v }
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-10 * time.Minute)
	probeAB, probeBA := uuid.New(), uuid.New()
	var abRows []store.ResultRow
	for i := range 24 {
		abRows = append(abRows, store.ResultRow{
			Time: t0.Add(time.Duration(i) * time.Second), TargetID: targetB, ProbeID: probeAB,
			ProbeType: 2, Status: 1, Sent: 1, Received: 1, TCPConnectUS: i32(400 + int32(i)),
		})
	}
	abRows = append(abRows,
		store.ResultRow{Time: t0.Add(30 * time.Second), TargetID: targetB, ProbeID: probeAB,
			ProbeType: 1, Status: 1, Sent: 1, Received: 1, RttAvgUS: i32(90)},
		store.ResultRow{Time: t0.Add(31 * time.Second), TargetID: targetB, ProbeID: probeAB,
			ProbeType: 2, Status: 2, Sent: 1, Received: 0},
	)
	insertResults(t, ctx, s, agentA, abRows)
	insertResults(t, ctx, s, agentB, []store.ResultRow{
		{Time: t0, TargetID: targetA, ProbeID: probeBA,
			ProbeType: 1, Status: 1, Sent: 3, Received: 2, RttAvgUS: i32(120), JitterUS: i32(7)},
		{Time: t0.Add(time.Second), TargetID: targetA, ProbeID: probeBA,
			ProbeType: 1, Status: 1, Sent: 3, Received: 3, RttAvgUS: i32(140), JitterUS: i32(9)},
	})

	// A 24h window, NOT 1h: the hourly cagg's rows sit on hour-aligned
	// buckets, so with a 1h window a t0 shortly past the top of the hour
	// lands in a bucket already outside `bucket > now() - window` and the
	// hourly sub-test flakes with the wall clock (caught by CI).
	window, bucket, horizon := 24*time.Hour, time.Hour, time.Hour
	dirs := []store.DirectionKey{
		{SrcAgents: []uuid.UUID{agentA}, DstTargets: []uuid.UUID{targetB}},
		{SrcAgents: []uuid.UUID{agentB}, DstTargets: []uuid.UUID{targetA}},
		// A plane filtered to nothing: still queried, NULL aggregates.
		{SrcAgents: []uuid.UUID{}, DstTargets: []uuid.UUID{targetB}},
	}

	checkSource := func(t *testing.T, source store.Source) {
		t.Helper()
		sums, err := s.PairDirectionSummaries(ctx, dirs, window, source, horizon)
		if err != nil {
			t.Fatalf("PairDirectionSummaries(%s): %v", source, err)
		}
		series, err := s.PairDirectionSeries(ctx, dirs, bucket, window, source)
		if err != nil {
			t.Fatalf("PairDirectionSeries(%s): %v", source, err)
		}
		if len(sums) != len(dirs) || len(series) != len(dirs) {
			t.Fatalf("(%s) got %d summaries / %d series, want %d", source, len(sums), len(series), len(dirs))
		}
		for i, d := range dirs {
			wantSum, err := s.PairSummary(ctx, d.SrcAgents, d.DstTargets, window, source)
			if err != nil {
				t.Fatalf("PairSummary(%s, dir %d): %v", source, i, err)
			}
			if !reflect.DeepEqual(sums[i].Summary, *wantSum) {
				t.Errorf("(%s, dir %d) batch summary = %+v\nwant %+v", source, i, sums[i].Summary, *wantSum)
			}
			wantLatest, err := s.DirectionLatest(ctx, d.SrcAgents, d.DstTargets, horizon)
			if err != nil {
				t.Fatalf("DirectionLatest(dir %d): %v", i, err)
			}
			if !reflect.DeepEqual(sums[i].Latest, wantLatest) {
				t.Errorf("(%s, dir %d) batch latest = %+v\nwant %+v", source, i, sums[i].Latest, wantLatest)
			}
			wantFamily, err := s.PairLatencySource(ctx, d.SrcAgents, d.DstTargets, window, source)
			if err != nil {
				t.Fatalf("PairLatencySource(%s, dir %d): %v", source, i, err)
			}
			if series[i].LatencySource != wantFamily || sums[i].Summary.LatencySource != wantFamily {
				t.Errorf("(%s, dir %d) families = summary %q / series %q, want %q",
					source, i, sums[i].Summary.LatencySource, series[i].LatencySource, wantFamily)
			}
			wantPoints, err := s.PairSeries(ctx, d.SrcAgents, d.DstTargets, bucket, window, source, wantFamily)
			if err != nil {
				t.Fatalf("PairSeries(%s, dir %d): %v", source, i, err)
			}
			if !reflect.DeepEqual(series[i].Points, wantPoints) {
				t.Errorf("(%s, dir %d) batch points = %+v\nwant %+v", source, i, series[i].Points, wantPoints)
			}
		}
		// The floor/fallback fold must actually be exercised: A→B's lone
		// rtt row is under 5% coverage, so tcp_connect wins.
		if got := sums[0].Summary.LatencySource; got != "tcp_connect" {
			t.Errorf("(%s) A→B family = %q, want tcp_connect (rtt under the coverage floor)", source, got)
		}
		if got := sums[1].Summary.LatencySource; got != "rtt" {
			t.Errorf("(%s) B→A family = %q, want rtt", source, got)
		}
	}
	t.Run("raw", func(t *testing.T) { checkSource(t, store.SourceRaw) })
	// materialized_only = false serves the un-refreshed window live, so this
	// executes the cagg SQL arms without a refresh_continuous_aggregate.
	t.Run("hourly", func(t *testing.T) { checkSource(t, store.SourceHourly) })
}

// TestSiteEndpointsBatchParity interleaves known, unknown, and
// scope-invisible names — the cursor-alignment traps: an unknown site
// queues nothing in wave 2, an invisible one queues triples that must be
// drained and discarded without shifting later results.
func TestSiteEndpointsBatchParity(t *testing.T) {
	t.Parallel()
	ctx, s := newParityStore(t)

	agentA := seedAgent(t, ctx, s, "pair-a", "a1")
	agentA2 := seedAgent(t, ctx, s, "pair-a", "a2")
	agentB := seedAgent(t, ctx, s, "pair-b", "b1")
	seedAgentTarget(t, ctx, s, agentA)
	seedAgentTarget(t, ctx, s, agentA2)
	seedAgentTarget(t, ctx, s, agentB)
	mgmt := createNetwork(t, ctx, s, "mgmt")
	defaultNet := networkIDByName(t, ctx, s, "default")

	check := func(t *testing.T, names []string, scope []uuid.UUID) {
		t.Helper()
		got, err := s.SiteEndpointsBatch(ctx, names, scope)
		if err != nil {
			t.Fatalf("SiteEndpointsBatch(%v): %v", names, err)
		}
		if len(got) != len(names) {
			t.Fatalf("got %d entries, want %d", len(got), len(names))
		}
		for i, name := range names {
			want, err := s.SiteEndpoints(ctx, name, scope)
			if err != nil {
				t.Fatalf("SiteEndpoints(%q): %v", name, err)
			}
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("batch[%d] (%q) = %+v\nwant %+v", i, name, got[i], want)
			}
		}
	}

	t.Run("unscoped", func(t *testing.T) {
		check(t, []string{"pair-a", "nope", "pair-b"}, nil)
	})
	t.Run("scoped", func(t *testing.T) {
		// Under a mgmt-only scope both default-plane sites are invisible;
		// their queued triples must not desynchronize the batch cursor.
		check(t, []string{"pair-a", "nope", "pair-b"}, []uuid.UUID{mgmt})
		// Under the default scope both resolve, unknown stays nil.
		check(t, []string{"pair-b", "nope", "pair-a"}, []uuid.UUID{defaultNet})
	})
}

func TestCurrentPathsBatchParity(t *testing.T) {
	t.Parallel()
	ctx, s := newParityStore(t)

	agentA := seedAgent(t, ctx, s, "pair-a", "a1")
	agentB := seedAgent(t, ctx, s, "pair-b", "b1")
	targetA := seedAgentTarget(t, ctx, s, agentA)
	targetB := seedAgentTarget(t, ctx, s, agentB)

	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := make([]byte, 32)
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO traceroute_current (agent_id, probe_id, target_id, updated_at, dest_reached, path_hash, hops)
		 VALUES ($1, $2, $3, $4, true, $5, '[{"ttl": 1}]'::jsonb)`,
		agentA, uuid.New(), targetB, now, hash); err != nil {
		t.Fatalf("insert traceroute_current: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO path_mtu_current (agent_id, probe_id, target_id, updated_at,
		     largest_ok_bytes, smallest_failed_bytes, next_hop_mtu_bytes, ip_version,
		     black_hole, local_constraint, rtt_us)
		 VALUES ($1, $2, $3, $4, 1472, 1500, 0, 4, false, false, 250)`,
		agentB, uuid.New(), targetA, now); err != nil {
		t.Fatalf("insert path_mtu_current: %v", err)
	}

	dirs := []store.DirectionKey{
		{SrcAgents: []uuid.UUID{agentA}, DstTargets: []uuid.UUID{targetB}},
		{SrcAgents: []uuid.UUID{agentB}, DstTargets: []uuid.UUID{targetA}},
	}
	paths, err := s.CurrentPathsBatch(ctx, dirs)
	if err != nil {
		t.Fatalf("CurrentPathsBatch: %v", err)
	}
	mtus, err := s.CurrentPathMTUsBatch(ctx, dirs)
	if err != nil {
		t.Fatalf("CurrentPathMTUsBatch: %v", err)
	}
	for i, d := range dirs {
		wantPaths, err := s.CurrentPaths(ctx, d.SrcAgents, d.DstTargets)
		if err != nil {
			t.Fatalf("CurrentPaths(dir %d): %v", i, err)
		}
		if !reflect.DeepEqual(paths[i], wantPaths) {
			t.Errorf("paths[%d] = %+v\nwant %+v", i, paths[i], wantPaths)
		}
		wantMTUs, err := s.CurrentPathMTUs(ctx, d.SrcAgents, d.DstTargets)
		if err != nil {
			t.Fatalf("CurrentPathMTUs(dir %d): %v", i, err)
		}
		if !reflect.DeepEqual(mtus[i], wantMTUs) {
			t.Errorf("mtus[%d] = %+v\nwant %+v", i, mtus[i], wantMTUs)
		}
	}
	if len(paths[0]) != 1 || len(paths[1]) != 0 || len(mtus[0]) != 0 || len(mtus[1]) != 1 {
		t.Errorf("row spread = paths %d/%d, mtus %d/%d; want 1/0 and 0/1",
			len(paths[0]), len(paths[1]), len(mtus[0]), len(mtus[1]))
	}
}
