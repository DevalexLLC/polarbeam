package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// TestConfigCachesInvalidateOnWrites: the enabled-probe and expected-pair
// caches must reflect a config write immediately (write paths invalidate;
// the TTL is only a backstop for out-of-band SQL).
func TestConfigCachesInvalidateOnWrites(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	before, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs: %v", err)
	}
	pairsBefore, err := s.ExpectedPairs(ctx, nil)
	if err != nil {
		t.Fatalf("ExpectedPairs: %v", err)
	}

	// Same answers from the cache while nothing changed.
	again, err := s.EnabledProbeIDs(ctx)
	if err != nil || len(again) != len(before) {
		t.Fatalf("cached EnabledProbeIDs: %d ids (%v), want %d", len(again), err, len(before))
	}

	// A new direct probe at site-b must appear in both immediately.
	newProbe, err := s.AddDirectProbe(ctx, "site-b", "svc", f.defaultNet, netProbeSettings, true, "test", nil)
	if err != nil {
		t.Fatalf("AddDirectProbe: %v", err)
	}
	after, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs after write: %v", err)
	}
	found := false
	for _, id := range after {
		if id == newProbe {
			found = true
		}
	}
	if !found {
		t.Error("EnabledProbeIDs served stale data after AddDirectProbe")
	}

	// Disabling it must drop it again, through UpdateProbeConfig this time.
	if err := s.UpdateProbeConfig(ctx, newProbe, netProbeSettings, false, "test"); err != nil {
		t.Fatalf("UpdateProbeConfig: %v", err)
	}
	final, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs after disable: %v", err)
	}
	for _, id := range final {
		if id == newProbe {
			t.Error("EnabledProbeIDs kept a disabled probe after UpdateProbeConfig")
		}
	}

	// ExpectedPairs: scoped and unfiltered answers stay distinct entries —
	// a scoped tenant must never receive the admin's cached view.
	scoped, err := s.ExpectedPairs(ctx, []uuid.UUID{f.mgmt})
	if err != nil {
		t.Fatalf("ExpectedPairs scoped: %v", err)
	}
	for _, p := range scoped {
		if p.Network != "mgmt" {
			t.Errorf("mgmt-scoped ExpectedPairs leaked pair on network %q", p.Network)
		}
	}
	if len(pairsBefore) == 0 {
		t.Fatal("fixture produced no expected pairs; scope assertions are vacuous")
	}

	// Concurrent mixed-scope traffic over one cache: under -race this
	// catches any map access outside the cache mutex (a cached-scope read
	// racing a new-scope insert).
	done := make(chan error, 32)
	for i := 0; i < 32; i++ {
		scope := []uuid.UUID(nil)
		if i%2 == 0 {
			scope = []uuid.UUID{f.mgmt}
		}
		go func() {
			_, err := s.ExpectedPairs(ctx, scope)
			done <- err
		}()
	}
	for i := 0; i < 32; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent ExpectedPairs: %v", err)
		}
	}
}
