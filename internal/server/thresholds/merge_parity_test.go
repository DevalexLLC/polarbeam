package thresholds

// The Go half of the cross-language merge fence. testdata/threshold-merge.json
// is read here AND by web/tools/check-threshold-merge.ts, so a change to
// either resolver that is not matched in the other fails a CI job: this one
// in `make test` / offline-build, the TypeScript one in web-lint.
//
// Before this file the three-way agreement documented in the package comment
// was enforced only by that comment.

import (
	"encoding/json"
	"os"
	"testing"
)

type parityLayer struct {
	LatencyWarnUS *int64   `json:"latency_warn_us"`
	LatencyCritUS *int64   `json:"latency_crit_us"`
	LossWarnPct   *float64 `json:"loss_warn_pct"`
	LossCritPct   *float64 `json:"loss_crit_pct"`
}

func (l *parityLayer) override() Override {
	if l == nil {
		return Override{}
	}
	return Override{
		LatencyWarnUS: l.LatencyWarnUS,
		LatencyCritUS: l.LatencyCritUS,
		LossWarnPct:   l.LossWarnPct,
		LossCritPct:   l.LossCritPct,
	}
}

// parityT mirrors T with the fixture's wire names. T itself carries no JSON
// tags, and encoding/json will NOT match "latency_warn_us" to LatencyWarnUS
// (case-insensitive matching does not cross underscores), so decoding
// straight into T silently yields a zero value — and a fixture of zeros
// would compare equal to nothing useful while looking like it ran.
type parityT struct {
	LatencyWarnUS int64   `json:"latency_warn_us"`
	LatencyCritUS int64   `json:"latency_crit_us"`
	LossWarnPct   float64 `json:"loss_warn_pct"`
	LossCritPct   float64 `json:"loss_crit_pct"`
}

func (p parityT) T() T {
	return T{
		LatencyWarnUS: p.LatencyWarnUS,
		LatencyCritUS: p.LatencyCritUS,
		LossWarnPct:   p.LossWarnPct,
		LossCritPct:   p.LossCritPct,
	}
}

type parityFile struct {
	Global parityT `json:"global"`
	Cases  []struct {
		Name        string       `json:"name"`
		PairNetwork *parityLayer `json:"pair_network"`
		PairAll     *parityLayer `json:"pair_all"`
		Network     *parityLayer `json:"network"`
		Expect      parityT      `json:"expect"`
	} `json:"cases"`
}

func TestEffectiveMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/threshold-merge.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f parityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture has no cases — a silently empty table would pass forever")
	}
	if (f.Global == parityT{}) {
		t.Fatal("fixture global decoded to zeros — the wire names stopped matching parityT")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// Specificity order, most specific first — the same order
			// ingest and httpapi pass, and the same the SPA folds.
			got := Effective(f.Global.T(),
				c.PairNetwork.override(),
				c.PairAll.override(),
				c.Network.override())
			if got != c.Expect.T() {
				t.Errorf("Effective = %+v, want %+v", got, c.Expect.T())
			}
		})
	}
}

// TestEffectiveGlobalUnchangedByLayers pins that the merge never mutates its
// inputs — Effective takes T by value and Override by value, and an earlier
// draft that cleared consumed fields would have made the second call in a
// loop return something different.
func TestEffectiveGlobalUnchangedByLayers(t *testing.T) {
	global := T{LatencyWarnUS: 1, LatencyCritUS: 2, LossWarnPct: 3, LossCritPct: 4}
	warn := int64(99)
	layer := Override{LatencyWarnUS: &warn}
	first := Effective(global, layer)
	second := Effective(global, layer)
	if first != second {
		t.Errorf("Effective is not idempotent: %+v then %+v", first, second)
	}
	if global != (T{LatencyWarnUS: 1, LatencyCritUS: 2, LossWarnPct: 3, LossCritPct: 4}) {
		t.Errorf("Effective mutated its global argument: %+v", global)
	}
	if layer.LatencyWarnUS == nil || *layer.LatencyWarnUS != 99 {
		t.Error("Effective mutated its layer argument")
	}
}
