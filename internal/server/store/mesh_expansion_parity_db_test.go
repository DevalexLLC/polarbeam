package store_test

// The mesh expansion is written twice: meshexpand.BuildSnapshot derives each
// agent's probe specs for the config stream, and store.enabledProbeIDs
// re-derives the fleet-wide enabled set for dashboards and the outage sweep.
// Comments on both assert they must match; a drift silently orphans outage
// events (opened for IDs the sweep no longer expects) and skews probe
// counts. This test pins them together against a real database: the union
// of every agent's snapshot IDs must equal EnabledProbeIDs exactly.

import (
	"testing"

	"github.com/google/uuid"
)

func TestEnabledProbeIDsMatchSnapshotExpansion(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	// Widen the fixture beyond the default plane: a second mesh on the mgmt
	// network (network-scoped pairing) and a disabled direct probe that must
	// appear on NEITHER side.
	if _, err := s.UpsertMeshGroup(ctx, "m2", &f.mgmt); err != nil {
		t.Fatalf("UpsertMeshGroup m2: %v", err)
	}
	for _, site := range []string{"site-a", "site-b"} {
		if err := s.AddMeshMember(ctx, "m2", site, nil); err != nil {
			t.Fatalf("AddMeshMember m2 %s: %v", site, err)
		}
	}
	if _, err := s.AddMeshProbe(ctx, "m2", netProbeSettings, true, "test", nil); err != nil {
		t.Fatalf("AddMeshProbe m2: %v", err)
	}
	if _, err := s.AddDirectProbe(ctx, "site-b", "svc", f.defaultNet, netProbeSettings, false, "test", nil); err != nil {
		t.Fatalf("AddDirectProbe disabled: %v", err)
	}

	want := map[string]bool{}
	for _, agent := range []uuid.UUID{f.aDef, f.aMgmt, f.bDef, f.bMgmt} {
		for id := range snapshotProbeIDs(t, ctx, s, agent) {
			want[id] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("fixture produced no snapshot probes; the parity check is vacuous")
	}

	ids, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id.String()] = true
	}

	for id := range want {
		if !got[id] {
			t.Errorf("snapshot expansion has %s but EnabledProbeIDs does not — the sweep would orphan its events", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("EnabledProbeIDs has %s but no agent's snapshot does — probe counts would include a phantom series", id)
		}
	}
}
