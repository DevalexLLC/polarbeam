package store_test

// DB-backed tests for the target detail queries: source folding in
// TargetEndpoints, the stage averages' successful-only / stage-presence
// semantics on both the raw and cagg paths, and TargetProbeHealth's
// enabled-probe intersection and silent-series rows.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// seedAgent inserts a minimal agents row in siteName (created on demand)
// and returns the agent ID. probe_results has no agent FK, but
// TargetEndpoints/TargetProbeHealth join real agents+sites rows.
func seedAgent(t *testing.T, ctx context.Context, s *store.Store, siteName, hostname string) uuid.UUID {
	t.Helper()
	siteID, err := s.EnsureSite(ctx, siteName)
	if err != nil {
		t.Fatalf("EnsureSite %q: %v", siteName, err)
	}
	id := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO agents (id, site_id, network_id, hostname)
		 VALUES ($1, $2, (SELECT id FROM networks WHERE name = 'default'), $3)`,
		id, siteID, hostname); err != nil {
		t.Fatalf("insert agent %q: %v", hostname, err)
	}
	return id
}

func seedSeriesState(t *testing.T, ctx context.Context, s *store.Store, agentID, probeID, targetID uuid.UUID, probeType int16) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO series_state (agent_id, probe_id, target_id, probe_type, last_status, last_time)
		 VALUES ($1, $2, $3, $4, 1, now())`,
		agentID, probeID, targetID, probeType); err != nil {
		t.Fatalf("insert series_state: %v", err)
	}
}

func TestTargetEndpoints(t *testing.T) {
	ctx, s := newStore(t)

	if got, err := s.TargetEndpoints(ctx, uuid.New()); err != nil || got != nil {
		t.Fatalf("unknown target: got %+v, %v; want nil, nil", got, err)
	}

	a1 := seedAgent(t, ctx, s, "site-a", "a1")
	a2 := seedAgent(t, ctx, s, "site-a", "a2")
	b1 := seedAgent(t, ctx, s, "site-b", "b1")

	ext, err := s.UpsertExternalTarget(ctx, "svc", "svc.example", 443, "https://svc.example/")
	if err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	// Series toward the external target from both site-a agents and the
	// site-b agent; site order and per-site agent grouping must hold.
	seedSeriesState(t, ctx, s, a1, uuid.New(), ext, 4)
	seedSeriesState(t, ctx, s, a2, uuid.New(), ext, 4)
	seedSeriesState(t, ctx, s, b1, uuid.New(), ext, 4)

	got, err := s.TargetEndpoints(ctx, ext)
	if err != nil {
		t.Fatalf("TargetEndpoints(external): %v", err)
	}
	if got.Kind != "external" || got.Name != "svc" || got.URL != "https://svc.example/" ||
		got.AgentID != nil || got.DstSite != nil {
		t.Errorf("external target row = %+v", got)
	}
	if len(got.Sources) != 2 || got.Sources[0].Site != "site-a" || got.Sources[1].Site != "site-b" ||
		len(got.Sources[0].AgentIDs) != 2 || len(got.Sources[1].AgentIDs) != 1 {
		t.Errorf("sources = %+v, want site-a×2 agents then site-b×1", got.Sources)
	}
	if got.Sources[0].Network != "default" || got.Sources[1].Network != "default" {
		t.Errorf("sources = %+v, want every source on default", got.Sources)
	}

	// An off-plane agent at site-a splits the site into two source rows,
	// ordered site first, then network.
	mgmt := createNetwork(t, ctx, s, "mgmt")
	a3 := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO agents (id, site_id, network_id, hostname)
		 VALUES ($1, (SELECT site_id FROM agents WHERE id = $2), $3, 'a3')`,
		a3, a1, mgmt); err != nil {
		t.Fatalf("insert mgmt agent: %v", err)
	}
	seedSeriesState(t, ctx, s, a3, uuid.New(), ext, 4)
	got, err = s.TargetEndpoints(ctx, ext)
	if err != nil {
		t.Fatalf("TargetEndpoints(external, two planes): %v", err)
	}
	if len(got.Sources) != 3 ||
		got.Sources[0].Site != "site-a" || got.Sources[0].Network != "default" || len(got.Sources[0].AgentIDs) != 2 ||
		got.Sources[1].Site != "site-a" || got.Sources[1].Network != "mgmt" || len(got.Sources[1].AgentIDs) != 1 ||
		got.Sources[2].Site != "site-b" || got.Sources[2].Network != "default" {
		t.Errorf("sources = %+v, want site-a split by plane then site-b", got.Sources)
	}

	// Agent-kind target owned by b1: DstSite resolves through the owning
	// agent's site.
	agentTarget := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO targets (id, kind, name, agent_id) VALUES ($1, 'agent', $2, $3)`,
		agentTarget, "agent:"+b1.String(), b1); err != nil {
		t.Fatalf("insert agent target: %v", err)
	}
	seedSeriesState(t, ctx, s, a1, uuid.New(), agentTarget, 1)

	got, err = s.TargetEndpoints(ctx, agentTarget)
	if err != nil {
		t.Fatalf("TargetEndpoints(agent): %v", err)
	}
	if got.Kind != "agent" || got.AgentID == nil || *got.AgentID != b1 ||
		got.DstSite == nil || *got.DstSite != "site-b" {
		t.Errorf("agent target row = %+v, want owner b1 / dst site-b", got)
	}
	if len(got.Sources) != 1 || got.Sources[0].Site != "site-a" {
		t.Errorf("agent target sources = %+v, want site-a only", got.Sources)
	}
}

// TestTargetStageSeries pins the stage averages on both paths: successful
// rows only, per-stage NULLs contribute nothing (an ICMP row must not drag
// an HTTP stage average), and the src-agent filter scopes the series. The
// hourly path is queried without a cagg refresh — materialized_only = false
// serves the whole window live from raw.
func TestTargetStageSeries(t *testing.T) {
	ctx, s := newStore(t)
	agent, other := uuid.New(), uuid.New()
	target := uuid.New()
	probe := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)

	i32 := func(v int32) *int32 { return &v }
	httpRow := func(at time.Time, status int16, base int32) store.ResultRow {
		return store.ResultRow{
			Time: at, TargetID: target, ProbeID: probe,
			ProbeType: 4, Status: status, Sent: 1, Received: 1,
			DNSUS: i32(base), TCPConnectUS: i32(2 * base), TLSHandshakeUS: i32(3 * base),
			TTFBUS: i32(4 * base), TotalUS: i32(5 * base),
		}
	}
	insertResults(t, ctx, s, agent, []store.ResultRow{
		httpRow(t0, 1, 100),
		httpRow(t0.Add(time.Second), 1, 300),
		// Failed row: enormous values that must not shift any average.
		httpRow(t0.Add(2*time.Second), 2, 1_000_000),
		// Successful ICMP row: NULL stages, counts as a sample only.
		{Time: t0.Add(3 * time.Second), TargetID: target, ProbeID: uuid.New(),
			ProbeType: 1, Status: 1, Sent: 1, Received: 1, RttAvgUS: i32(50)},
	})
	// Same target from an agent outside the filter: must not leak in.
	insertResults(t, ctx, s, other, []store.ResultRow{httpRow(t0, 1, 900)})

	check := func(source store.Source) {
		t.Helper()
		// A 24h bucket folds everything into one row regardless of where
		// the hour boundary falls relative to t0.
		got, err := s.TargetStageSeries(ctx, []uuid.UUID{agent}, target,
			24*time.Hour, 24*time.Hour, source)
		if err != nil {
			t.Fatalf("%s: TargetStageSeries: %v", source, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d buckets, want 1", source, len(got))
		}
		b := got[0]
		if b.Samples != 3 {
			t.Errorf("%s: samples = %d, want 3 successful", source, b.Samples)
		}
		wants := []struct {
			name string
			got  *float64
			want float64
		}{
			{"dns", b.DNSUS, 200}, {"tcp", b.TCPUS, 400}, {"tls", b.TLSUS, 600},
			{"ttfb", b.TTFBUS, 800}, {"total", b.TotalUS, 1000},
		}
		for _, w := range wants {
			if w.got == nil || *w.got != w.want {
				t.Errorf("%s: %s avg = %v, want %v", source, w.name, w.got, w.want)
			}
		}
	}
	check(store.SourceRaw)
	check(store.SourceHourly)

	// A window with no measured stages: buckets exist (the ICMP sample)
	// but every stage average is nil.
	icmpOnly := uuid.New()
	insertResults(t, ctx, s, agent, []store.ResultRow{
		{Time: t0, TargetID: icmpOnly, ProbeID: uuid.New(),
			ProbeType: 1, Status: 1, Sent: 1, Received: 1, RttAvgUS: i32(50)},
	})
	got, err := s.TargetStageSeries(ctx, []uuid.UUID{agent}, icmpOnly,
		24*time.Hour, 24*time.Hour, store.SourceRaw)
	if err != nil {
		t.Fatalf("icmp-only TargetStageSeries: %v", err)
	}
	if len(got) != 1 || got[0].DNSUS != nil || got[0].TotalUS != nil {
		t.Errorf("icmp-only stages = %+v, want one bucket with nil stages", got)
	}
}

func TestTargetProbeHealth(t *testing.T) {
	ctx, s := newStore(t)
	a1 := seedAgent(t, ctx, s, "site-a", "a1")
	b1 := seedAgent(t, ctx, s, "site-b", "b1")

	target, err := s.UpsertExternalTarget(ctx, "svc", "svc.example", 443, "")
	if err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	settings := store.ProbeSettings{ProbeType: 2, Interval: time.Minute, Timeout: 5 * time.Second, Params: map[string]string{}}
	defaultNet := networkIDByName(t, ctx, s, "default")
	enabledProbe, err := s.AddDirectProbe(ctx, "site-a", "svc", defaultNet, settings, true, "test")
	if err != nil {
		t.Fatalf("AddDirectProbe enabled: %v", err)
	}
	disabledProbe, err := s.AddDirectProbe(ctx, "site-b", "svc", defaultNet, settings, false, "test")
	if err != nil {
		t.Fatalf("AddDirectProbe disabled: %v", err)
	}
	// site-b direct probe for the silent-series case, enabled but no results.
	silentProbe, err := s.AddDirectProbe(ctx, "site-b", "svc", defaultNet, settings, true, "test")
	if err != nil {
		t.Fatalf("AddDirectProbe silent: %v", err)
	}

	seedSeriesState(t, ctx, s, a1, enabledProbe, target, 2)
	seedSeriesState(t, ctx, s, b1, disabledProbe, target, 2)
	seedSeriesState(t, ctx, s, b1, silentProbe, target, 2)

	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	i32 := func(v int32) *int32 { return &v }
	insertResults(t, ctx, s, a1, []store.ResultRow{
		{Time: t0, TargetID: target, ProbeID: enabledProbe,
			ProbeType: 2, Status: 1, Sent: 1, Received: 1, TCPConnectUS: i32(500)},
		{Time: t0.Add(time.Second), TargetID: target, ProbeID: enabledProbe,
			ProbeType: 2, Status: 2, Sent: 1, Received: 0},
	})

	rows, err := s.TargetProbeHealth(ctx, target, 24*time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatalf("TargetProbeHealth: %v", err)
	}
	// Expect: a1's series with ≥1 bucket row, b1's silent series with one
	// nil-bucket row — and the disabled series absent.
	byProbe := map[uuid.UUID][]store.TargetProbeHealthRow{}
	for _, r := range rows {
		byProbe[r.ProbeID] = append(byProbe[r.ProbeID], r)
	}
	if _, ok := byProbe[disabledProbe]; ok {
		t.Error("disabled probe's series leaked into TargetProbeHealth")
	}
	active := byProbe[enabledProbe]
	if len(active) == 0 {
		t.Fatal("enabled series missing")
	}
	if active[0].SrcSite != "site-a" || active[0].Hostname != "a1" || active[0].AgentID != a1 {
		t.Errorf("enabled series labels = %+v, want site-a/a1", active[0])
	}
	var samples, ok int64
	for _, r := range active {
		if r.Bucket == nil {
			t.Fatalf("enabled series has a nil bucket row despite results: %+v", r)
		}
		samples += *r.Samples
		ok += *r.OK
	}
	if samples != 2 || ok != 1 {
		t.Errorf("enabled series counted %d samples / %d ok, want 2 / 1", samples, ok)
	}
	silent := byProbe[silentProbe]
	if len(silent) != 1 || silent[0].Bucket != nil || silent[0].SrcSite != "site-b" {
		t.Errorf("silent series rows = %+v, want one nil-bucket site-b row", silent)
	}

	// Mesh probes share one probe_id across a site's agents, and hostnames
	// are not unique — a second agent with a1's hostname and probe id is a
	// distinct series whose rows must stay contiguous (the handler's
	// run-length fold splits series on any (agent, probe) change).
	a3 := seedAgent(t, ctx, s, "site-a", "a1")
	seedSeriesState(t, ctx, s, a3, enabledProbe, target, 2)
	insertResults(t, ctx, s, a3, []store.ResultRow{
		{Time: t0, TargetID: target, ProbeID: enabledProbe,
			ProbeType: 2, Status: 1, Sent: 1, Received: 1, TCPConnectUS: i32(700)},
		{Time: t0.Add(-40 * time.Minute), TargetID: target, ProbeID: enabledProbe,
			ProbeType: 2, Status: 1, Sent: 1, Received: 1, TCPConnectUS: i32(700)},
	})
	insertResults(t, ctx, s, a1, []store.ResultRow{
		{Time: t0.Add(-40 * time.Minute), TargetID: target, ProbeID: enabledProbe,
			ProbeType: 2, Status: 1, Sent: 1, Received: 1, TCPConnectUS: i32(500)},
	})
	rows, err = s.TargetProbeHealth(ctx, target, 24*time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatalf("TargetProbeHealth (shared probe id): %v", err)
	}
	type seriesKey struct {
		agent uuid.UUID
		probe uuid.UUID
	}
	var order []seriesKey
	for _, r := range rows {
		k := seriesKey{r.AgentID, r.ProbeID}
		if len(order) == 0 || order[len(order)-1] != k {
			order = append(order, k)
		}
	}
	seen := map[seriesKey]int{}
	for _, k := range order {
		seen[k]++
		if seen[k] > 1 {
			t.Fatalf("series %v appears non-contiguously in TargetProbeHealth rows", k)
		}
	}
	if seen[seriesKey{a1, enabledProbe}] != 1 || seen[seriesKey{a3, enabledProbe}] != 1 {
		t.Errorf("shared-probe series missing: saw %v", seen)
	}
}

// TestAgentProbeHealthTargetID pins the target link column: a live target's
// ID rides along; a deleted target degrades to nil like kind/name.
func TestAgentProbeHealthTargetID(t *testing.T) {
	ctx, s := newStore(t)
	a1 := seedAgent(t, ctx, s, "site-a", "a1")

	target, err := s.UpsertExternalTarget(ctx, "svc", "svc.example", 443, "")
	if err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	settings := store.ProbeSettings{ProbeType: 2, Interval: time.Minute, Timeout: 5 * time.Second, Params: map[string]string{}}
	probeLive, err := s.AddDirectProbe(ctx, "site-a", "svc", networkIDByName(t, ctx, s, "default"), settings, true, "test")
	if err != nil {
		t.Fatalf("AddDirectProbe: %v", err)
	}
	seedSeriesState(t, ctx, s, a1, probeLive, target, 2)
	// A series whose target row is gone (series_state has no FK to
	// targets, so it degrades to nil labels): the enabled probe config
	// still references a live target — only the series' snapshot points
	// at the vanished row.
	gone, goneProbe := uuid.New(), uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO probe_configs (id, site_id, target_id, network_id, probe_type, interval_ms, timeout_ms, enabled, updated_by)
		 SELECT $1, site_id, $3, network_id, 2, 60000, 5000, true, 'test' FROM agents WHERE id = $2`,
		goneProbe, a1, target); err != nil {
		t.Fatalf("insert probe config for gone-target series: %v", err)
	}
	seedSeriesState(t, ctx, s, a1, goneProbe, gone, 2)

	rows, err := s.AgentProbeHealth(ctx, a1, 24*time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatalf("AgentProbeHealth: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		seen[r.ProbeID] = true
		switch r.ProbeID {
		case probeLive:
			if r.TargetID == nil || *r.TargetID != target {
				t.Errorf("live series TargetID = %v, want %s", r.TargetID, target)
			}
		case goneProbe:
			if r.TargetID != nil {
				t.Errorf("gone-target series TargetID = %v, want nil", r.TargetID)
			}
		}
	}
	if !seen[probeLive] || !seen[goneProbe] {
		t.Fatalf("expected both series in AgentProbeHealth, saw %v", seen)
	}
}
