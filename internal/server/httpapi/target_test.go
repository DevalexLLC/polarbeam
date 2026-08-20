package httpapi

// Handler tests for the target detail endpoints, run against fakeDB.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func targetFake() (*fakeDB, *store.TargetEndpoints) {
	f := newFakeDB()
	nycAgent, lonAgent, lonAgent2 := uuid.New(), uuid.New(), uuid.New()
	ep := &store.TargetEndpoints{
		ID: uuid.New(), Kind: "external", Name: "svc",
		Address: "svc.example", Port: 443, URL: "https://svc.example/",
		Sources: []store.TargetSource{
			{Site: "lon", Network: "default", AgentIDs: []uuid.UUID{lonAgent, lonAgent2}},
			{Site: "nyc", Network: "corp", AgentIDs: []uuid.UUID{nycAgent}},
		},
	}
	f.targetEndpoints = map[uuid.UUID]*store.TargetEndpoints{ep.ID: ep}
	f.latencySources = map[uuid.UUID]string{lonAgent: "ttfb", nycAgent: "rtt"}
	return f, ep
}

func getBody(t *testing.T, h http.Handler, cookie *http.Cookie, url string, wantCode int) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantCode {
		t.Fatalf("GET %s = %d %s, want %d", url, w.Code, w.Body, wantCode)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return body
}

func TestTargetEndpointsValidation(t *testing.T) {
	f, ep := targetFake()
	h := newTestAPI(t, f)

	// No session: 401 on every target route.
	for _, url := range []string{
		"/api/v1/targets/" + ep.ID.String(),
		"/api/v1/targets/" + ep.ID.String() + "/series",
		"/api/v1/targets/" + ep.ID.String() + "/stages",
		"/api/v1/targets/" + ep.ID.String() + "/health",
		"/api/v1/targets/" + ep.ID.String() + "/paths",
	} {
		getBody(t, h, nil, url, http.StatusUnauthorized)
	}

	cookie, _ := loginAndCookie(t, h, f)
	getBody(t, h, cookie, "/api/v1/targets/not-a-uuid", http.StatusBadRequest)
	getBody(t, h, cookie, "/api/v1/targets/"+uuid.New().String(), http.StatusNotFound)
	getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"?window=2h", http.StatusBadRequest)
	getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/series?metric=bogus", http.StatusBadRequest)
	getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/stages?window=1y", http.StatusBadRequest)
	// Health is fixed-window like the agent strips.
	getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/health?window=7d", http.StatusBadRequest)
}

func TestTargetSummary(t *testing.T) {
	f, ep := targetFake()
	fv := func(v float64) *float64 { return &v }
	f.pairSummary = &store.PairSummaryRow{AvgUS: fv(1500), LatencySource: "ttfb", Samples: 9}
	loss := float32(0)
	f.directionLatest = []store.MatrixRow{{
		ProbeType: int16(pb.ProbeType_PROBE_TYPE_HTTP),
		Status:    int16(pb.ProbeStatus_PROBE_STATUS_OK),
		Time:      time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC),
		LatencyUS: new(int64(1200)), LatencySource: "ttfb", LossPct: &loss,
	}}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"?window=30d", http.StatusOK)
	tgt := body["target"].(map[string]any)
	if tgt["id"] != ep.ID.String() || tgt["kind"] != "external" || tgt["name"] != "svc" ||
		tgt["url"] != "https://svc.example/" || tgt["dst_site"] != nil {
		t.Errorf("target = %#v", tgt)
	}
	if body["source"] != "hourly" || body["window"] != "30d" {
		t.Errorf("source/window = %v/%v, want hourly/30d", body["source"], body["window"])
	}
	sources := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	first := sources[0].(map[string]any)
	if first["site"] != "lon" || first["latency_source"] != "ttfb" || first["samples"] != 9.0 {
		t.Errorf("first source = %#v, want lon summary", first)
	}
	if first["network"] != "default" {
		t.Errorf("first source network = %v, want default", first["network"])
	}
	checks := first["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["type"] != "http" {
		t.Errorf("checks = %#v, want the latest http check", checks)
	}
	// omitempty: a row without a target id (this fixture) must drop the key
	// rather than render null — the matrix response relies on the same rule.
	if _, present := checks[0].(map[string]any)["target_id"]; present {
		t.Error("check without TargetID carries target_id; want key absent")
	}
	second := sources[1].(map[string]any)
	if second["site"] != "nyc" || second["network"] != "corp" {
		t.Errorf("second source = %#v, want nyc on corp", second)
	}
}

func TestTargetSeriesPerSourceFamilies(t *testing.T) {
	f, ep := targetFake()
	fv := func(v float64) *float64 { return &v }
	f.pairSeries = []store.SeriesBucket{
		{Bucket: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), AvgUS: fv(2000), Samples: 5},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/series?window=365d", http.StatusOK)
	if body["source"] != "daily" || body["metric"] != "latency" {
		t.Errorf("source/metric = %v/%v, want daily/latency", body["source"], body["metric"])
	}
	if f.lastSource != store.SourceDaily {
		t.Errorf("PairSeries source = %q, want daily", f.lastSource)
	}
	sources := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	lon := sources[0].(map[string]any)
	nyc := sources[1].(map[string]any)
	// Each source charts its own chosen family (fake keys on the site's
	// first agent id).
	if lon["site"] != "lon" || lon["latency_source"] != "ttfb" ||
		nyc["site"] != "nyc" || nyc["latency_source"] != "rtt" {
		t.Errorf("per-source families = %v/%v + %v/%v, want lon/ttfb + nyc/rtt",
			lon["site"], lon["latency_source"], nyc["site"], nyc["latency_source"])
	}
	if lon["network"] != "default" || nyc["network"] != "corp" {
		t.Errorf("source networks = %v/%v, want default/corp", lon["network"], nyc["network"])
	}
	if len(f.passedLatencySources) != 2 ||
		f.passedLatencySources[0] != "ttfb" || f.passedLatencySources[1] != "rtt" {
		t.Errorf("PairSeries families = %v, want [ttfb rtt]", f.passedLatencySources)
	}
	if pt := lon["points"].([]any)[0].(map[string]any); pt["avg_us"] != 2000.0 {
		t.Errorf("point = %#v, want avg 2000", pt)
	}
}

func TestTargetStages(t *testing.T) {
	f, ep := targetFake()
	fv := func(v float64) *float64 { return &v }
	f.stageBuckets = []store.StageBucket{
		{Bucket: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			DNSUS: fv(300), TCPUS: fv(800), TLSUS: fv(2100), TTFBUS: fv(9000), TotalUS: fv(9800),
			Samples: 7},
		// A bucket where only non-waterfall probes ran: all stages null.
		{Bucket: time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC), Samples: 3},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/stages?window=7d", http.StatusOK)
	if body["source"] != "raw" || body["resolution_s"] != 300.0 {
		t.Errorf("source/resolution = %v/%v, want raw/300", body["source"], body["resolution_s"])
	}
	// Every source agent, across sites, folds into one stage series.
	if f.lastStageTarget != ep.ID || len(f.lastStageAgents) != 3 {
		t.Errorf("TargetStageSeries called with target %s / %d agents, want %s / 3",
			f.lastStageTarget, len(f.lastStageAgents), ep.ID)
	}
	points := body["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2", len(points))
	}
	p0 := points[0].(map[string]any)
	for key, want := range map[string]float64{
		"dns_us": 300, "tcp_connect_us": 800, "tls_handshake_us": 2100,
		"ttfb_us": 9000, "total_us": 9800, "samples": 7,
	} {
		if p0[key] != want {
			t.Errorf("point[%s] = %v, want %v", key, p0[key], want)
		}
	}
	p1 := points[1].(map[string]any)
	if p1["dns_us"] != nil || p1["total_us"] != nil || p1["samples"] != 3.0 {
		t.Errorf("null-stage point = %#v, want null stages with 3 samples", p1)
	}
}

func TestTargetHealthFold(t *testing.T) {
	f, ep := targetFake()
	agentA, agentB := uuid.New(), uuid.New()
	probe1, probe2 := uuid.New(), uuid.New()
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	i64 := func(v int64) *int64 { return &v }
	openSince := t0.Add(-time.Hour)
	errText := "connect timeout"
	f.targetHealth = []store.TargetProbeHealthRow{
		// probe1: two buckets, failing.
		{AgentID: agentA, SrcSite: "lon", Network: "default", Hostname: "lon-1", ProbeID: probe1,
			ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP), LastStatus: 2, LastTime: t0,
			OpenedAt: &openSince, OpenError: &errText,
			Bucket: &t0, Samples: i64(4), OK: i64(1)},
		{AgentID: agentA, SrcSite: "lon", Network: "default", Hostname: "lon-1", ProbeID: probe1,
			ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP), LastStatus: 2, LastTime: t0,
			OpenedAt: &openSince, OpenError: &errText,
			Bucket:  func() *time.Time { b := t0.Add(30 * time.Minute); return &b }(),
			Samples: i64(6), OK: i64(6)},
		// probe2: silent series, single nil-bucket row.
		{AgentID: agentB, SrcSite: "nyc", Network: "corp", Hostname: "nyc-1", ProbeID: probe2,
			ProbeType: int16(pb.ProbeType_PROBE_TYPE_HTTP), LastStatus: 1, LastTime: t0},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/health", http.StatusOK)
	if f.lastTargetHealth != ep.ID {
		t.Errorf("TargetProbeHealth target = %s, want %s", f.lastTargetHealth, ep.ID)
	}
	if body["window"] != "24h" || body["bucket_s"] != 1800.0 {
		t.Errorf("window/bucket_s = %v/%v, want 24h/1800", body["window"], body["bucket_s"])
	}
	probes := body["probes"].([]any)
	if len(probes) != 2 {
		t.Fatalf("probes = %d, want 2 folded series", len(probes))
	}
	p0 := probes[0].(map[string]any)
	if p0["agent_id"] != agentA.String() || p0["site"] != "lon" || p0["network"] != "default" ||
		p0["hostname"] != "lon-1" ||
		p0["type"] != "tcp" || p0["failing"] != true || p0["error"] != errText {
		t.Errorf("first probe = %#v", p0)
	}
	if buckets := p0["buckets"].([]any); len(buckets) != 2 ||
		buckets[0].(map[string]any)["samples"] != 4.0 {
		t.Errorf("first probe buckets = %#v, want 2 buckets", p0["buckets"])
	}
	p1 := probes[1].(map[string]any)
	if p1["agent_id"] != agentB.String() || p1["failing"] != false ||
		len(p1["buckets"].([]any)) != 0 {
		t.Errorf("silent probe = %#v, want empty buckets", p1)
	}
}

// TestAgentProbeHealthCarriesTargetID pins the Agents-page link column.
func TestTargetPathsPerSource(t *testing.T) {
	f, ep := targetFake()
	lonAgent := ep.Sources[0].AgentIDs[0]
	f.paths = []store.CurrentPath{
		{AgentID: lonAgent, ProbeID: uuid.New(), AgentHostname: "lon-1", UpdatedAt: time.Now(),
			DestReached: true, PathHash: []byte{0xab, 0xcd},
			Hops: []byte(`[{"ttl":1,"addrs":["10.0.0.1"],"rtt_us":[500]}]`)},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/targets/"+ep.ID.String()+"/paths", http.StatusOK)
	if got := body["target"].(map[string]any)["name"]; got != "svc" {
		t.Errorf("target name = %v, want svc", got)
	}
	sources := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	lon := sources[0].(map[string]any)
	if lon["site"] != "lon" || lon["network"] != "default" {
		t.Errorf("first source = %#v, want lon on default", lon)
	}
	paths := lon["paths"].([]any)
	if len(paths) != 1 {
		t.Fatalf("lon paths = %d, want 1", len(paths))
	}
	p := paths[0].(map[string]any)
	if p["agent"] != "lon-1" || p["dest_reached"] != true || p["path_hash"] != "abcd" {
		t.Errorf("lon path = %#v", p)
	}
	if hops := p["hops"].([]any); len(hops) != 1 {
		t.Errorf("hops = %#v, want the seeded hop", p["hops"])
	}
	// A site with no traceroute against the target reports an empty list,
	// never null — the SPA hides the card on all-empty sources.
	nyc := sources[1].(map[string]any)
	if nyc["site"] != "nyc" {
		t.Errorf("second source = %#v, want nyc", nyc)
	}
	if paths, ok := nyc["paths"].([]any); !ok || len(paths) != 0 {
		t.Errorf("nyc paths = %#v, want []", nyc["paths"])
	}
}

func TestAgentProbeHealthCarriesTargetID(t *testing.T) {
	f := newFakeDB()
	agent := uuid.New()
	target := uuid.New()
	kind, name := "external", "svc"
	probeLive, probeGone := uuid.New(), uuid.New()
	f.probeHealth = []store.AgentProbeHealthRow{
		{ProbeID: probeLive, ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
			TargetID: &target, TargetKind: &kind, TargetName: &name, LastStatus: 1, LastTime: time.Now()},
		{ProbeID: probeGone, ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
			LastStatus: 1, LastTime: time.Now()},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	body := getBody(t, h, cookie, "/api/v1/agents/"+agent.String()+"/health", http.StatusOK)
	probes := body["probes"].([]any)
	if len(probes) != 2 {
		t.Fatalf("probes = %d, want 2", len(probes))
	}
	if got := probes[0].(map[string]any)["target_id"]; got != target.String() {
		t.Errorf("live series target_id = %v, want %s", got, target)
	}
	if got := probes[1].(map[string]any)["target_id"]; got != nil {
		t.Errorf("gone-target series target_id = %v, want null", got)
	}
}
