package probeid

import (
	"testing"

	"github.com/google/uuid"
)

var (
	tmpl = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	src  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	tgt  = uuid.MustParse("33333333-3333-4333-8333-333333333333")
)

// TestMeshProbeIDGolden pins the derivation to a literal. The IDs are stored
// in probe_results — if this test fails, the change orphans all mesh history
// and needs its own state-cleanup migration.
func TestMeshProbeIDGolden(t *testing.T) {
	got := MeshProbeID(tmpl, src, tgt)
	const want = "f1381e41-e8c4-5231-b964-d3b6453311fd"
	if got.String() != want {
		t.Fatalf("MeshProbeID derivation changed: got %s, want %s", got, want)
	}
	if got.Version() != 5 {
		t.Fatalf("expected UUIDv5, got version %d", got.Version())
	}
}

// TestMeshProbeIDAxes verifies every input axis changes the ID: template
// (duplicate same-type templates stay distinct), source site, and
// destination target (multiple agents at one site stay distinct).
func TestMeshProbeIDAxes(t *testing.T) {
	base := MeshProbeID(tmpl, src, tgt)
	other := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	if MeshProbeID(other, src, tgt) == base {
		t.Error("template ID does not affect derivation")
	}
	if MeshProbeID(tmpl, other, tgt) == base {
		t.Error("source site does not affect derivation")
	}
	if MeshProbeID(tmpl, src, other) == base {
		t.Error("target ID does not affect derivation")
	}
}
