package meshexpand

import (
	"testing"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeid"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

var (
	meshID   = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	tmplID   = uuid.MustParse("00000000-0000-0000-0000-0000000000f1")
	siteA    = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	siteB    = uuid.MustParse("00000000-0000-0000-0000-0000000000b1")
	siteC    = uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
	agentA   = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	agentB   = uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	agentC   = uuid.MustParse("00000000-0000-0000-0000-0000000000c2")
	targetB  = uuid.MustParse("00000000-0000-0000-0000-0000000000b3")
	targetC  = uuid.MustParse("00000000-0000-0000-0000-0000000000c3")
	directID = uuid.MustParse("00000000-0000-0000-0000-0000000000d1")
)

func tcpSettings() store.ProbeSettings {
	return store.ProbeSettings{
		ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
		Interval:  30 * time.Second,
		Timeout:   5 * time.Second,
	}
}

func inputsForA() store.AgentConfigInputs {
	return store.AgentConfigInputs{
		AgentID: agentA,
		SiteID:  siteA,
		Direct: []store.DirectProbeRow{{
			ID:       directID,
			Settings: tcpSettings(),
			TargetID: uuid.MustParse("00000000-0000-0000-0000-0000000000e1"),
			Kind:     "external",
			Address:  "db.example",
			Port:     5432,
		}},
		Mesh: []store.MeshProbeRow{{
			ConfigID: tmplID,
			MeshID:   meshID,
			Settings: store.ProbeSettings{
				ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
				Interval:  time.Minute,
				Timeout:   5 * time.Second,
				Params:    map[string]string{"port": "9"},
			},
		}},
		Peers: []store.PeerRow{
			{MeshID: meshID, AgentID: agentB, SiteID: siteB, TargetID: targetB, ProbeAddress: "10.0.0.2"},
			{MeshID: meshID, AgentID: agentC, SiteID: siteC, TargetID: targetC, ProbeAddress: "10.0.0.3"},
		},
	}
}

func mustBuild(t *testing.T, in store.AgentConfigInputs) *pb.ConfigSnapshot {
	t.Helper()
	snap, err := BuildSnapshot(in)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	return snap
}

func TestBuildSnapshotExpandsDirectAndMesh(t *testing.T) {
	snap := mustBuild(t, inputsForA())
	if len(snap.Probes) != 3 {
		t.Fatalf("got %d probes, want 3 (1 direct + 2 mesh peers)", len(snap.Probes))
	}
	var direct, mesh int
	for _, p := range snap.Probes {
		if p.ProbeId == directID.String() {
			direct++
			if p.Target.Kind != pb.TargetKind_TARGET_KIND_EXTERNAL || p.Target.Port != 5432 {
				t.Errorf("direct target wrong: %+v", p.Target)
			}
		} else {
			mesh++
			if p.Target.Kind != pb.TargetKind_TARGET_KIND_AGENT_PEER {
				t.Errorf("mesh target kind = %v", p.Target.Kind)
			}
			if p.Target.Port != 9 {
				t.Errorf("mesh port = %d, want 9 (from params)", p.Target.Port)
			}
		}
	}
	if direct != 1 || mesh != 2 {
		t.Errorf("direct=%d mesh=%d", direct, mesh)
	}
}

// TestMultipleAgentsPerSiteDistinct is the collision regression: a second
// agent at siteB must yield its own spec with its own probe_id and its own
// address, never collapse onto the first agent's series.
func TestMultipleAgentsPerSiteDistinct(t *testing.T) {
	in := inputsForA()
	agentB2 := uuid.MustParse("00000000-0000-0000-0000-0000000000b4")
	targetB2 := uuid.MustParse("00000000-0000-0000-0000-0000000000b5")
	in.Peers = append(in.Peers, store.PeerRow{
		MeshID: meshID, AgentID: agentB2, SiteID: siteB, TargetID: targetB2, ProbeAddress: "10.0.0.4",
	})

	snap := mustBuild(t, in)
	if len(snap.Probes) != 4 {
		t.Fatalf("got %d probes, want 4 (1 direct + 3 mesh peers)", len(snap.Probes))
	}
	seen := make(map[string]string) // probe_id → address
	for _, p := range snap.Probes {
		if prev, dup := seen[p.ProbeId]; dup {
			t.Fatalf("duplicate probe_id %s (addresses %s and %s)", p.ProbeId, prev, p.Target.Address)
		}
		seen[p.ProbeId] = p.Target.Address
	}
	wantAddr := map[string]string{
		probeid.MeshProbeID(tmplID, siteA, targetB).String():  "10.0.0.2",
		probeid.MeshProbeID(tmplID, siteA, targetB2).String(): "10.0.0.4",
		probeid.MeshProbeID(tmplID, siteA, targetC).String():  "10.0.0.3",
	}
	for id, addr := range wantAddr {
		if seen[id] != addr {
			t.Errorf("probe %s has address %q, want %q", id, seen[id], addr)
		}
	}
}

// TestDuplicateTemplatesDistinct: two same-type templates on one mesh are
// distinct series — the template row ID is the derivation namespace.
func TestDuplicateTemplatesDistinct(t *testing.T) {
	in := inputsForA()
	second := in.Mesh[0]
	second.ConfigID = uuid.MustParse("00000000-0000-0000-0000-0000000000f2")
	in.Mesh = append(in.Mesh, second)

	snap := mustBuild(t, in)
	if len(snap.Probes) != 5 {
		t.Fatalf("got %d probes, want 5 (1 direct + 2 templates × 2 peers)", len(snap.Probes))
	}
	seen := make(map[string]bool)
	for _, p := range snap.Probes {
		if seen[p.ProbeId] {
			t.Fatalf("duplicate probe_id %s", p.ProbeId)
		}
		seen[p.ProbeId] = true
	}
}

// TestDuplicateExpansionFailsLoud: identical (template, target) expansions
// must be an error, never a silently collapsed snapshot.
func TestDuplicateExpansionFailsLoud(t *testing.T) {
	in := inputsForA()
	in.Mesh = append(in.Mesh, in.Mesh[0]) // same ConfigID twice
	if _, err := BuildSnapshot(in); err == nil {
		t.Fatal("BuildSnapshot accepted a duplicate probe_id expansion")
	}
}

func TestHashDeterministicAcrossInputOrder(t *testing.T) {
	in1 := inputsForA()
	in2 := inputsForA()
	// Reverse peer order; the snapshot sorts by probe_id, so the hash must
	// not depend on database row order.
	in2.Peers[0], in2.Peers[1] = in2.Peers[1], in2.Peers[0]

	h1 := mustBuild(t, in1).ConfigHash
	h2 := mustBuild(t, in2).ConfigHash
	if h1 != h2 {
		t.Errorf("hash depends on input order: %s vs %s", h1, h2)
	}
	if h1 == "" {
		t.Error("empty hash")
	}
}

func TestHashChangesWithConfig(t *testing.T) {
	base := mustBuild(t, inputsForA()).ConfigHash
	changed := inputsForA()
	changed.Direct[0].Settings.Interval = time.Hour
	if mustBuild(t, changed).ConfigHash == base {
		t.Error("interval change must change the hash")
	}
}

// TestMeshProbeIDDirectionality: A→B and B→A expand to different targets and
// therefore different probe_ids. (The derivation itself is pinned to a
// golden literal in the probeid package.)
func TestMeshProbeIDDirectionality(t *testing.T) {
	targetA := uuid.MustParse("00000000-0000-0000-0000-0000000000a3")
	ab := probeid.MeshProbeID(tmplID, siteA, targetB)
	ba := probeid.MeshProbeID(tmplID, siteB, targetA)
	if ab == ba {
		t.Error("A→B and B→A must have distinct probe ids")
	}
	if ab.Version() != 5 {
		t.Errorf("uuid version = %d, want 5 (UUIDv5/SHA-1)", ab.Version())
	}
}

func TestPeersOfOtherMeshesExcluded(t *testing.T) {
	in := inputsForA()
	otherMesh := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	in.Peers = append(in.Peers, store.PeerRow{
		MeshID: otherMesh, AgentID: agentB, SiteID: siteB, TargetID: targetB, ProbeAddress: "10.9.9.9",
	})
	snap := mustBuild(t, in)
	if len(snap.Probes) != 3 {
		t.Errorf("peer of an unrelated mesh leaked into expansion: %d probes", len(snap.Probes))
	}
}

func TestEmptyInputsStableHash(t *testing.T) {
	empty := store.AgentConfigInputs{AgentID: agentA, SiteID: siteA}
	h1 := mustBuild(t, empty).ConfigHash
	h2 := mustBuild(t, empty).ConfigHash
	if h1 != h2 || h1 == "" {
		t.Errorf("empty snapshot hash unstable: %q vs %q", h1, h2)
	}
}
