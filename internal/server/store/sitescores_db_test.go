package store_test

// DB-backed tests for SiteScores, the month-to-date availability and
// performance tallies behind the map's site card. The grading lives in SQL
// (per-hour rows are too many to ship to Go), so nothing but a real
// TimescaleDB can exercise it: the hourly cagg fold, the both-direction site
// attribution, and — most importantly — the in-query copy of the four-layer
// threshold merge, which TestSiteScoresMergeParity replays from the shared
// thresholds fixture against thresholds.Effective + thresholds.GradeWarn.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/thresholds"
)

const (
	probeICMP       int16 = 1
	probeTraceroute int16 = 6
)

// scoreSite seeds one staffed site: an agent on the named plane (created on
// demand; "" = default) plus the agent-kind target other agents probe.
type scoreSite struct {
	name   string
	agent  uuid.UUID
	target uuid.UUID
}

func seedScoreSite(t *testing.T, ctx context.Context, s *store.Store, siteName, hostname string, networkID *uuid.UUID) scoreSite {
	t.Helper()
	var agent uuid.UUID
	if networkID == nil {
		agent = seedAgent(t, ctx, s, siteName, hostname)
	} else {
		siteID, err := s.EnsureSite(ctx, siteName)
		if err != nil {
			t.Fatalf("EnsureSite %q: %v", siteName, err)
		}
		agent = uuid.New()
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO agents (id, site_id, network_id, hostname) VALUES ($1, $2, $3, $4)`,
			agent, siteID, *networkID, hostname); err != nil {
			t.Fatalf("insert agent %q: %v", hostname, err)
		}
	}
	return scoreSite{name: siteName, agent: agent, target: seedAgentTarget(t, ctx, s, agent)}
}

// hourBucket describes the rows one (series, hour) receives: ok successful
// rows at rttUS each, then failed rows (TIMEOUT, sent 1 / received 0), with
// the per-row train accounting overridable so loss can be dialed exactly.
type hourBucket struct {
	at     time.Time
	ok     int
	rttUS  int32
	failed int
	// sent/received per OK row; zero means the 1/1 default.
	sent, received int32
}

func seedBuckets(t *testing.T, ctx context.Context, s *store.Store, agent, target uuid.UUID, probeID uuid.UUID, probeType int16, buckets []hourBucket) {
	t.Helper()
	var rows []store.ResultRow
	for _, b := range buckets {
		sent, received := b.sent, b.received
		if sent == 0 {
			sent, received = 1, 1
		}
		for i := range b.ok {
			rtt := b.rttUS
			rows = append(rows, store.ResultRow{
				Time: b.at.Add(time.Duration(i) * time.Second), TargetID: target, ProbeID: probeID,
				ProbeType: probeType, Status: 1, Sent: sent, Received: received, RttAvgUS: &rtt,
			})
		}
		for i := range b.failed {
			rows = append(rows, store.ResultRow{
				Time: b.at.Add(time.Duration(1000+i) * time.Second), TargetID: target, ProbeID: probeID,
				ProbeType: probeType, Status: 2, Sent: 1, Received: 0,
			})
		}
	}
	insertResults(t, ctx, s, agent, rows)
	refreshHourly(t, ctx, s)
}

// refreshHourly materializes the hourly cagg up to now. The test database is
// cloned from the migrated template WITH its refresh policy, and that job
// fires soon after the clone exists: once it has run, the cagg's watermark
// sits at now - 1 h and a real-time query serves buckets below it from the
// materialization only — rows seeded afterwards into those hours stay
// invisible until the next scheduled refresh. Refreshing right after every
// seed makes the read deterministic whichever way that race goes. Must run
// outside a transaction, which a pool Exec is.
func refreshHourly(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx, `CALL refresh_continuous_aggregate('probe_results_hourly', NULL, NULL)`); err != nil {
		t.Fatalf("refresh probe_results_hourly: %v", err)
	}
}

func scoreOf(t *testing.T, scores []store.SiteScore, name string) store.SiteScore {
	t.Helper()
	for _, sc := range scores {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("site %q missing from %+v", name, scores)
	return store.SiteScore{}
}

func wantScore(t *testing.T, scores []store.SiteScore, name string, samples, ok, healthy int64) {
	t.Helper()
	got := scoreOf(t, scores, name)
	if got.Samples != samples || got.OK != ok || got.HealthyOK != healthy {
		t.Errorf("%s = samples %d / ok %d / healthy %d, want %d / %d / %d",
			name, got.Samples, got.OK, got.HealthyOK, samples, ok, healthy)
	}
}

// scoreT0 is an hour-aligned start three hours back: hour-aligned so each
// hourBucket lands in exactly one cagg bucket, in the past so the clock
// cannot move a row across a bucket edge mid-test, and well inside raw
// retention (14 d) for the un-refreshed tail the cagg serves live.
func scoreT0() time.Time { return time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour) }

func TestSiteScoresTallies(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	a := seedScoreSite(t, ctx, s, "score-a", "a1", nil)
	b := seedScoreSite(t, ctx, s, "score-b", "b1", nil)
	seedScoreSite(t, ctx, s, "score-empty", "e1", nil)
	t0 := scoreT0()
	// Global defaults: warn 100 ms / 1 % loss.
	seedBuckets(t, ctx, s, a.agent, b.target, uuid.New(), probeICMP, []hourBucket{
		{at: t0, ok: 8, rttUS: 50_000},                               // healthy
		{at: t0.Add(time.Hour), ok: 5, rttUS: 150_000},               // latency breaches warn
		{at: t0.Add(2 * time.Hour), ok: 3, rttUS: 50_000, failed: 2}, // 40 % loss: failed rows are loss
	})

	scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	// One series, attributed to the site that runs it AND the site it targets.
	wantScore(t, scores, "score-a", 18, 16, 8)
	wantScore(t, scores, "score-b", 18, 16, 8)
	// A staffed site with nothing this month is a row of zeros, not absent.
	wantScore(t, scores, "score-empty", 0, 0, 0)
	for i := 1; i < len(scores); i++ {
		if scores[i-1].Name >= scores[i].Name {
			t.Errorf("not ordered by name: %q before %q", scores[i-1].Name, scores[i].Name)
		}
	}

	// since is a lower bound on the bucket start, so a later since drops
	// the earlier hours entirely.
	later, err := s.SiteScores(ctx, t0.Add(2*time.Hour), probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores(since+2h): %v", err)
	}
	wantScore(t, later, "score-a", 5, 3, 0)
}

func TestSiteScoresLossGrading(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	a := seedScoreSite(t, ctx, s, "loss-a", "a1", nil)
	b := seedScoreSite(t, ctx, s, "loss-b", "b1", nil)
	t0 := scoreT0()
	// Two lossless hours, one at 2 % loss and one at 7 %, all fast.
	seedBuckets(t, ctx, s, a.agent, b.target, uuid.New(), probeICMP, []hourBucket{
		{at: t0, ok: 4, rttUS: 10_000, sent: 100, received: 100},
		{at: t0.Add(time.Hour), ok: 4, rttUS: 10_000, sent: 100, received: 100},
		{at: t0.Add(2 * time.Hour), ok: 4, rttUS: 10_000, sent: 100, received: 98},
		{at: t0.Add(3 * time.Hour), ok: 4, rttUS: 10_000, sent: 100, received: 93},
	})
	check := func(lossWarn float64, wantHealthy int64) {
		t.Helper()
		if _, err := s.UpdateSettings(ctx, store.ThresholdSettings{
			LatencyWarnUS: 100_000, LatencyCritUS: 250_000, LossWarnPct: lossWarn, LossCritPct: 50, UpdatedBy: "test",
		}); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
		if err != nil {
			t.Fatalf("SiteScores: %v", err)
		}
		wantScore(t, scores, "loss-a", 16, 16, wantHealthy)
	}
	// warn 1 %: both lossy hours breach.
	check(1, 8)
	// warn 3 %: only the 7 % hour breaches.
	check(3, 12)
	// warn 0 % ("warn on any loss"): the lossless hours stay healthy — zero
	// loss never breaches, even against a zero threshold — and only the
	// lossy hours flag.
	check(0, 8)
	// Exactly at the threshold breaches (>=), and the arithmetic must be
	// exact for that to hold: 1 - 93/100 is 0.06999… in binary floating
	// point, so a loss computed as 100 * (1 - received/sent) would read
	// 6.999… and slip under a 7 % warn. Lost packets are divided instead.
	check(7, 12)
	check(7.5, 16)
}

func TestSiteScoresExcludesTraceroute(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	a := seedScoreSite(t, ctx, s, "tr-a", "a1", nil)
	b := seedScoreSite(t, ctx, s, "tr-b", "b1", nil)
	t0 := scoreT0()
	seedBuckets(t, ctx, s, a.agent, b.target, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 4, rttUS: 10_000}})
	// Traceroute run-accounting rows: no timings, some failed.
	insertResults(t, ctx, s, a.agent, []store.ResultRow{
		{Time: t0.Add(5 * time.Minute), TargetID: b.target, ProbeID: uuid.New(), ProbeType: probeTraceroute, Status: 1, Sent: 1, Received: 1},
		{Time: t0.Add(6 * time.Minute), TargetID: b.target, ProbeID: uuid.New(), ProbeType: probeTraceroute, Status: 2, Sent: 1, Received: 0},
	})
	refreshHourly(t, ctx, s)
	scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	wantScore(t, scores, "tr-a", 4, 4, 4)
	// Anti-vacuity: with nothing excluded the traceroute rows do count —
	// and their failed row's loss would flag the whole hour.
	all, err := s.SiteScores(ctx, t0, -1, nil)
	if err != nil {
		t.Fatalf("SiteScores(no exclusion): %v", err)
	}
	wantScore(t, all, "tr-a", 6, 5, 4)
}

func TestSiteScoresSameSiteSeriesCountsOnce(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	x1 := seedScoreSite(t, ctx, s, "same-x", "x1", nil)
	x2 := seedScoreSite(t, ctx, s, "same-x", "x2", nil)
	t0 := scoreT0()
	seedBuckets(t, ctx, s, x1.agent, x2.target, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 6, rttUS: 10_000}})
	scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	wantScore(t, scores, "same-x", 6, 6, 6)
}

func TestSiteScoresExternalTarget(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	a := seedScoreSite(t, ctx, s, "ext-a", "a1", nil)
	seedScoreSite(t, ctx, s, "ext-b", "b1", nil)
	ext, err := s.UpsertExternalTarget(ctx, "svc", "svc.example", 443, "", nil, nil)
	if err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	t0 := scoreT0()
	seedBuckets(t, ctx, s, a.agent, ext, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 5, rttUS: 50_000}})

	scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	// Source site only; no other site inherits an external series.
	wantScore(t, scores, "ext-a", 5, 5, 5)
	wantScore(t, scores, "ext-b", 0, 0, 0)

	// The plane default still grades an external series (no pair layers
	// exist for it): warn at 10 ms flags the 50 ms hour.
	warn := int64(10_000)
	if _, err := s.UpsertNetworkThreshold(ctx, "default", store.NetworkThreshold{LatencyWarnUS: &warn, UpdatedBy: "test"}, nil); err != nil {
		t.Fatalf("UpsertNetworkThreshold: %v", err)
	}
	scores, err = s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	wantScore(t, scores, "ext-a", 5, 5, 0)
}

func TestSiteScoresScope(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	mgmt := createNetwork(t, ctx, s, "mgmt")
	// shared: staffed on both planes; only-default and only-mgmt: one each.
	sharedDef := seedScoreSite(t, ctx, s, "scope-shared", "s-def", nil)
	sharedMgmt := seedScoreSite(t, ctx, s, "scope-shared", "s-mgmt", &mgmt)
	onlyDef := seedScoreSite(t, ctx, s, "scope-only-default", "d1", nil)
	onlyMgmt := seedScoreSite(t, ctx, s, "scope-only-mgmt", "m1", &mgmt)
	t0 := scoreT0()
	seedBuckets(t, ctx, s, onlyDef.agent, sharedDef.target, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 3, rttUS: 10_000}})
	seedBuckets(t, ctx, s, onlyMgmt.agent, sharedMgmt.target, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 7, rttUS: 10_000}})

	all, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores(nil): %v", err)
	}
	wantScore(t, all, "scope-shared", 10, 10, 10)
	wantScore(t, all, "scope-only-default", 3, 3, 3)
	wantScore(t, all, "scope-only-mgmt", 7, 7, 7)

	scoped, err := s.SiteScores(ctx, t0, probeTraceroute, []uuid.UUID{mgmt})
	if err != nil {
		t.Fatalf("SiteScores(mgmt): %v", err)
	}
	// The shared site's tally holds only the tenant's plane, and the
	// other plane's site does not exist as far as the tenant can tell.
	wantScore(t, scoped, "scope-shared", 7, 7, 7)
	wantScore(t, scoped, "scope-only-mgmt", 7, 7, 7)
	for _, sc := range scoped {
		if sc.Name == "scope-only-default" {
			t.Errorf("scoped read leaks the other plane's site: %+v", sc)
		}
	}
	// An empty (non-nil) scope is a real filter: no planes, no sites.
	none, err := s.SiteScores(ctx, t0, probeTraceroute, []uuid.UUID{})
	if err != nil {
		t.Fatalf("SiteScores(empty): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty scope returned %+v", none)
	}
}

// TestSiteScoresGradingUnit pins the documented grain: the hourly cagg has
// no probe_id, so two same-type templates between the same endpoints blend
// into one series before grading — equal-weight 50 ms and 150 ms hours
// average to exactly the 100 ms warn threshold and the hour breaches. The
// user chose this unit over a probe-keyed aggregate; a per-template score
// would read 50 % here.
func TestSiteScoresGradingUnit(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	a := seedScoreSite(t, ctx, s, "unit-a", "a1", nil)
	b := seedScoreSite(t, ctx, s, "unit-b", "b1", nil)
	t0 := scoreT0()
	seedBuckets(t, ctx, s, a.agent, b.target, uuid.New(), probeICMP, []hourBucket{{at: t0, ok: 4, rttUS: 50_000}})
	seedBuckets(t, ctx, s, a.agent, b.target, uuid.New(), probeICMP, []hourBucket{{at: t0.Add(30 * time.Minute), ok: 4, rttUS: 150_000}})
	scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
	if err != nil {
		t.Fatalf("SiteScores: %v", err)
	}
	wantScore(t, scores, "unit-a", 8, 8, 0)
}

// parityFixture mirrors the shared case table's wire names (see
// thresholds/merge_parity_test.go for why T cannot decode it directly).
type parityFixture struct {
	Global struct {
		LatencyWarnUS int64   `json:"latency_warn_us"`
		LatencyCritUS int64   `json:"latency_crit_us"`
		LossWarnPct   float64 `json:"loss_warn_pct"`
		LossCritPct   float64 `json:"loss_crit_pct"`
	} `json:"global"`
	Cases []struct {
		Name        string       `json:"name"`
		PairNetwork *parityLayer `json:"pair_network"`
		PairAll     *parityLayer `json:"pair_all"`
		Network     *parityLayer `json:"network"`
	} `json:"cases"`
}

type parityLayer struct {
	LatencyWarnUS *int64   `json:"latency_warn_us"`
	LatencyCritUS *int64   `json:"latency_crit_us"`
	LossWarnPct   *float64 `json:"loss_warn_pct"`
	LossCritPct   *float64 `json:"loss_crit_pct"`
}

// warnOnly is the layer as this query sees it: the warn fields alone. The
// crit fields are dropped rather than copied because the fixture's
// deliberately odd values (equal warn and crit) violate the tables' CHECKs
// verbatim, and a crit value cannot influence a warn grade anyway. Nil when
// the layer sets no warn field: such a row would fail num_nonnulls > 0 and
// contributes nothing.
func (l *parityLayer) warnOnly() *thresholds.Override {
	if l == nil || (l.LatencyWarnUS == nil && l.LossWarnPct == nil) {
		return nil
	}
	return &thresholds.Override{LatencyWarnUS: l.LatencyWarnUS, LossWarnPct: l.LossWarnPct}
}

// TestSiteScoresMergeParity replays every case of the shared threshold
// fixture through the SQL merge: for each case the layers are written as
// real path_thresholds / network_thresholds rows and hours are seeded at
// every candidate warn value (and just under it), so a layer applied in the
// wrong order, or a boundary compared the wrong way, changes the tally.
// The expected tally is computed by thresholds.Effective + GradeWarn from
// the same rows — the SQL is a fourth copy of the merge, and this is its
// fence.
func TestSiteScoresMergeParity(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	raw, err := os.ReadFile("../thresholds/testdata/threshold-merge.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f parityFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Cases) == 0 || f.Global.LatencyWarnUS == 0 {
		t.Fatal("fixture decoded empty — wire names stopped matching")
	}
	global := thresholds.T{
		LatencyWarnUS: f.Global.LatencyWarnUS, LatencyCritUS: f.Global.LatencyCritUS,
		LossWarnPct: f.Global.LossWarnPct, LossCritPct: f.Global.LossCritPct,
	}
	if _, err := s.UpdateSettings(ctx, store.ThresholdSettings{
		LatencyWarnUS: global.LatencyWarnUS, LatencyCritUS: global.LatencyCritUS,
		LossWarnPct: global.LossWarnPct, LossCritPct: global.LossCritPct, UpdatedBy: "test",
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	// Start far enough back that every case's hours fit inside raw
	// retention with room; each case owns its own sites and plane so cases
	// never see each other's rows or layers.
	t0 := scoreT0().Add(-48 * time.Hour)
	for i, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			plane := createNetwork(t, ctx, s, "plane-"+suffix)
			src := seedScoreSite(t, ctx, s, "par-src-"+suffix, "src-"+suffix, &plane)
			// Both pair layers absent: this is the external-target shape,
			// so grade a genuinely external destination for that case.
			var dst uuid.UUID
			if c.PairNetwork == nil && c.PairAll == nil {
				ext, err := s.UpsertExternalTarget(ctx, "ext-"+suffix, "ext-"+suffix+".example", 443, "", nil, nil)
				if err != nil {
					t.Fatalf("UpsertExternalTarget: %v", err)
				}
				dst = ext
			} else {
				dst = seedScoreSite(t, ctx, s, "par-dst-"+suffix, "dst-"+suffix, &plane).target
			}

			var layers []thresholds.Override
			if o := c.PairNetwork.warnOnly(); o != nil {
				if _, err := s.UpsertPathThreshold(ctx, "par-src-"+suffix, "par-dst-"+suffix, &plane,
					store.PathThresholdOverride{LatencyWarnUS: o.LatencyWarnUS, LossWarnPct: o.LossWarnPct, UpdatedBy: "test"}); err != nil {
					t.Fatalf("UpsertPathThreshold(plane): %v", err)
				}
				layers = append(layers, *o)
			}
			if o := c.PairAll.warnOnly(); o != nil {
				if _, err := s.UpsertPathThreshold(ctx, "par-src-"+suffix, "par-dst-"+suffix, nil,
					store.PathThresholdOverride{LatencyWarnUS: o.LatencyWarnUS, LossWarnPct: o.LossWarnPct, UpdatedBy: "test"}); err != nil {
					t.Fatalf("UpsertPathThreshold(all): %v", err)
				}
				layers = append(layers, *o)
			}
			if o := c.Network.warnOnly(); o != nil {
				if _, err := s.UpsertNetworkThreshold(ctx, "plane-"+suffix,
					store.NetworkThreshold{LatencyWarnUS: o.LatencyWarnUS, LossWarnPct: o.LossWarnPct, UpdatedBy: "test"}, nil); err != nil {
					t.Fatalf("UpsertNetworkThreshold: %v", err)
				}
				layers = append(layers, *o)
			}
			effective := thresholds.Effective(global, layers...)

			// Candidate values: every warn value any layer or the global
			// row could contribute, probed at the value and just below it.
			latCandidates := []int64{global.LatencyWarnUS}
			lossCandidates := []float64{global.LossWarnPct}
			for _, l := range []*parityLayer{c.PairNetwork, c.PairAll, c.Network} {
				if l == nil {
					continue
				}
				if l.LatencyWarnUS != nil {
					latCandidates = append(latCandidates, *l.LatencyWarnUS)
				}
				if l.LossWarnPct != nil {
					lossCandidates = append(lossCandidates, *l.LossWarnPct)
				}
			}
			var buckets []hourBucket
			var wantOK, wantHealthy int64
			hour := 0
			add := func(b hourBucket, lat *int64, loss *float64) {
				b.at = t0.Add(time.Duration(hour) * time.Hour)
				hour++
				buckets = append(buckets, b)
				wantOK += int64(b.ok)
				if !thresholds.GradeWarn(effective, lat, loss) {
					wantHealthy += int64(b.ok)
				}
			}
			for _, lat := range latCandidates {
				for _, v := range []int64{lat, lat - 1} {
					vv := v
					add(hourBucket{ok: 3, rttUS: int32(v), sent: 100, received: 100}, &vv, nil)
				}
			}
			for _, loss := range lossCandidates {
				// Integer percentages in the fixture: received = 100 - loss.
				vv := loss
				one := int64(1)
				add(hourBucket{ok: 3, rttUS: 1, sent: 100, received: int32(100 - loss)}, &one, &vv)
			}
			seedBuckets(t, ctx, s, src.agent, dst, uuid.New(), probeICMP, buckets)

			scores, err := s.SiteScores(ctx, t0, probeTraceroute, nil)
			if err != nil {
				t.Fatalf("SiteScores: %v", err)
			}
			wantScore(t, scores, "par-src-"+suffix, wantOK, wantOK, wantHealthy)
			if wantHealthy == 0 || wantHealthy == wantOK {
				t.Logf("note: case grades uniformly (%d/%d healthy)", wantHealthy, wantOK)
			}
		})
	}
}
