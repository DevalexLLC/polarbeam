package grpcapi

// DB-backed test for the four-layer threshold resolution ingest bakes into
// its assignment map. Gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest).
//
// This is the highest-blast-radius half of the merge: httpapi only DISPLAYS
// severities, while this one decides whether an outage event is opened. A
// resolver that agrees with the dashboard in unit tests but resolves the
// wrong layer here would split the live map from the incident history, which
// is exactly the failure internal/server/thresholds' package comment warns
// about.

import (
	"context"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

var ingestCertSerial atomic.Int64

func ingestStore(t testing.TB) (context.Context, *store.Store) {
	t.Helper()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s, err := store.Connect(ctx, url, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	t.Cleanup(s.Close)
	return ctx, s
}

func ingestEnroll(t *testing.T, ctx context.Context, s *store.Store, site, hostname string, network uuid.UUID) uuid.UUID {
	t.Helper()
	siteID, err := s.EnsureSite(ctx, site)
	if err != nil {
		t.Fatalf("EnsureSite %q: %v", site, err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, network, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	id, _, err := s.EnrollAgent(ctx, token, hostname, hostname+":9443", "v0", []byte(hostname),
		func(uuid.UUID) (store.IssuedCert, error) {
			return store.IssuedCert{
				Serial:    big.NewInt(ingestCertSerial.Add(1)),
				NotBefore: time.Now().Add(-time.Hour),
				NotAfter:  time.Now().Add(time.Hour),
			}, nil
		})
	if err != nil {
		t.Fatalf("EnrollAgent %q: %v", hostname, err)
	}
	return id
}

// TestAgentProbeMapResolvesFourLayers builds a real two-plane mesh and pins
// that each agent grades on ITS OWN plane's layers.
func TestAgentProbeMapResolvesFourLayers(t *testing.T) {
	ctx, s := ingestStore(t)
	srv := New(s, nil)

	defaultNet, err := s.NetworkIDByName(ctx, "default")
	if err != nil {
		t.Fatalf("NetworkIDByName: %v", err)
	}
	tenant, err := s.CreateNetwork(ctx, "tenant-a", "Tenant A")
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	// Same two sites on both planes — the shared-site case that made a
	// pair-only threshold key ambiguous in the first place.
	opsA := ingestEnroll(t, ctx, s, "site-a", "ops-a", defaultNet)
	ingestEnroll(t, ctx, s, "site-b", "ops-b", defaultNet)
	tenA := ingestEnroll(t, ctx, s, "site-a", "ten-a", tenant)
	ingestEnroll(t, ctx, s, "site-b", "ten-b", tenant)

	for _, plane := range []struct {
		name string
		id   uuid.UUID
	}{{"ops-mesh", defaultNet}, {"tenant-mesh", tenant}} {
		if _, err := s.UpsertMeshGroup(ctx, plane.name, &plane.id); err != nil {
			t.Fatalf("UpsertMeshGroup %s: %v", plane.name, err)
		}
		for _, site := range []string{"site-a", "site-b"} {
			if err := s.AddMeshMember(ctx, plane.name, site, nil); err != nil {
				t.Fatalf("AddMeshMember: %v", err)
			}
		}
		if _, err := s.AddMeshProbe(ctx, plane.name, store.ProbeSettings{
			ProbeType: 1, Interval: time.Minute, Timeout: 5 * time.Second,
			Params: map[string]string{},
		}, true, "test", nil); err != nil {
			t.Fatalf("AddMeshProbe %s: %v", plane.name, err)
		}
	}

	i64 := func(v int64) *int64 { return &v }
	f64 := func(v float64) *float64 { return &v }

	// Layer 2 (all planes): latency crit only.
	if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", nil,
		store.PathThresholdOverride{LatencyCritUS: i64(500_000), UpdatedBy: "op"}); err != nil {
		t.Fatalf("all-planes threshold: %v", err)
	}
	// Layer 1 (tenant plane only): a tighter latency crit, nothing else.
	if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", &tenant,
		store.PathThresholdOverride{LatencyCritUS: i64(90_000), UpdatedBy: "tenant"}); err != nil {
		t.Fatalf("plane threshold: %v", err)
	}
	// Layer 3 (tenant plane default): loss crit only.
	if _, err := s.UpsertNetworkThreshold(ctx, "tenant-a",
		store.NetworkThreshold{LossCritPct: f64(2), UpdatedBy: "tenant"}, nil); err != nil {
		t.Fatalf("network threshold: %v", err)
	}

	global, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	assignments := func(agent uuid.UUID) map[uuid.UUID]probeAssignment {
		m, err := srv.agentProbeMap(ctx, agent)
		if err != nil {
			t.Fatalf("agentProbeMap: %v", err)
		}
		if len(m) == 0 {
			t.Fatal("no probe assignments — the mesh did not expand")
		}
		return m
	}

	t.Run("tenant agent takes its plane's pair row and plane default", func(t *testing.T) {
		for id, a := range assignments(tenA) {
			// latency_crit: layer 1 wins over layer 2 and the global row.
			if a.Crit.LatencyCritUS != 90_000 {
				t.Errorf("probe %s latency_crit_us = %d, want 90000 (the tenant plane's pair row)", id, a.Crit.LatencyCritUS)
			}
			// loss_crit: no pair row sets it, so the plane default wins.
			if a.Crit.LossCritPct != 2 {
				t.Errorf("probe %s loss_crit_pct = %v, want 2 (the tenant plane default)", id, a.Crit.LossCritPct)
			}
			// latency_warn: no layer sets it — falls through to global.
			if a.Crit.LatencyWarnUS != global.LatencyWarnUS {
				t.Errorf("probe %s latency_warn_us = %d, want the global %d", id, a.Crit.LatencyWarnUS, global.LatencyWarnUS)
			}
		}
	})

	t.Run("ops agent sees the all-planes row and the global row only", func(t *testing.T) {
		for id, a := range assignments(opsA) {
			// The tenant's tighter row must NOT reach the operator's plane.
			if a.Crit.LatencyCritUS != 500_000 {
				t.Errorf("probe %s latency_crit_us = %d, want 500000 (the all-planes row)", id, a.Crit.LatencyCritUS)
			}
			// Nor its plane default.
			if a.Crit.LossCritPct != global.LossCritPct {
				t.Errorf("probe %s loss_crit_pct = %v, want the global %v — the tenant's plane default leaked",
					id, a.Crit.LossCritPct, global.LossCritPct)
			}
		}
	})
}
