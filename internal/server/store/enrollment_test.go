package store

import (
	"math"
	"testing"
)

func TestDroppedDelta(t *testing.T) {
	base := func(v int64) *int64 { return &v }
	tests := []struct {
		name           string
		last           *int64
		total, unacked int64
		delta          int64
		reset          bool
	}{
		// nil baseline = first total-bearing report: only the unacked
		// portion is new (earlier drops may be recorded via the legacy
		// path by a pre-v0.4 server).
		{"first report, fresh agent", nil, 5, 5, 5, false},
		{"first report, partly acked via old server", nil, 5, 3, 3, false},
		{"first report, fully acked via old server", nil, 5, 0, 0, false},
		{"retry of the same total", base(5), 5, 5, 0, false},
		{"new drops", base(5), 9, 4, 4, false},
		{"spool wiped, new drops", base(9), 3, 3, 3, true},
		{"spool wiped, no new drops", base(9), 0, 0, 0, true},
		{"near the bigint ceiling", base(math.MaxInt64 - 1), math.MaxInt64, 1, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delta, reset := droppedDelta(tt.last, tt.total, tt.unacked)
			if delta != tt.delta || reset != tt.reset {
				t.Errorf("droppedDelta(%v, %d, %d) = (%d, %v), want (%d, %v)",
					tt.last, tt.total, tt.unacked, delta, reset, tt.delta, tt.reset)
			}
		})
	}
}

func TestClampInt64(t *testing.T) {
	if got := clampInt64(math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("clampInt64(MaxUint64) = %d, want MaxInt64", got)
	}
	if got := clampInt64(42); got != 42 {
		t.Errorf("clampInt64(42) = %d", got)
	}
}
