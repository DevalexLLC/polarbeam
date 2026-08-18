package thresholds

import "testing"

var global = T{LatencyWarnUS: 100_000, LatencyCritUS: 250_000, LossWarnPct: 1, LossCritPct: 5}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func TestEffectiveInheritsPerField(t *testing.T) {
	got := Effective(global, Override{LatencyCritUS: i64(40_000), LossCritPct: f64(2)})
	want := T{LatencyWarnUS: 100_000, LatencyCritUS: 40_000, LossWarnPct: 1, LossCritPct: 2}
	if got != want {
		t.Errorf("Effective = %+v, want %+v", got, want)
	}
	if got := Effective(global, Override{}); got != global {
		t.Errorf("empty override must inherit everything: %+v", got)
	}
}

func TestGradeCritLatency(t *testing.T) {
	// >= at the boundary, matching the SPA's directionSeverity.
	if ok, _ := GradeCrit(global, i64(250_000), nil); !ok {
		t.Error("latency at the crit threshold must breach")
	}
	if ok, _ := GradeCrit(global, i64(249_999), nil); ok {
		t.Error("latency below the crit threshold must not breach")
	}
	// Warn-tier values never open incidents.
	if ok, _ := GradeCrit(global, i64(100_000), nil); ok {
		t.Error("warn-tier latency must not breach")
	}
	if ok, _ := GradeCrit(global, nil, nil); ok {
		t.Error("unmeasured metrics must not breach")
	}
}

func TestGradeCritLoss(t *testing.T) {
	if ok, _ := GradeCrit(global, nil, f64(5)); !ok {
		t.Error("loss at the crit threshold must breach")
	}
	if ok, _ := GradeCrit(global, nil, f64(4.9)); ok {
		t.Error("loss below the crit threshold must not breach")
	}
	// Zero loss is never unhealthy, even against a zero threshold.
	if ok, _ := GradeCrit(T{LossCritPct: 0, LatencyCritUS: 1 << 40}, nil, f64(0)); ok {
		t.Error("zero loss must never breach")
	}
}

func TestGradeCritDetailIsStable(t *testing.T) {
	// The detail names the threshold, never the measurement — the dashboard
	// correlates incidents by this text.
	_, d1 := GradeCrit(global, i64(250_000), nil)
	_, d2 := GradeCrit(global, i64(900_000), nil)
	if d1 != d2 {
		t.Errorf("detail must not vary with the measured value: %q vs %q", d1, d2)
	}
	if d1 != "latency at or above critical threshold (250ms)" {
		t.Errorf("latency detail = %q", d1)
	}
	_, d3 := GradeCrit(global, nil, f64(7))
	if d3 != "loss at or above critical threshold (5%)" {
		t.Errorf("loss detail = %q", d3)
	}
	_, d4 := GradeCrit(global, i64(300_000), f64(7))
	if d4 != "latency and loss at or above critical thresholds (250ms, 5%)" {
		t.Errorf("combined detail = %q", d4)
	}
}
