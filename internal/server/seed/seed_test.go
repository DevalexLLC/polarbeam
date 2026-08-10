package seed

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

func TestGeneratePairDeterministic(t *testing.T) {
	a := GeneratePair("nyc|lon", t0, 7*24*60)
	b := GeneratePair("nyc|lon", t0, 7*24*60)
	if len(a) != 7*24*60 {
		t.Fatalf("rows = %d, want %d", len(a), 7*24*60)
	}
	for i := range a {
		// DeepEqual follows the pointer fields; plain != would compare
		// addresses and always differ.
		if !reflect.DeepEqual(a[i], b[i]) {
			t.Fatalf("row %d differs between identical runs: %+v vs %+v", i, a[i], b[i])
		}
	}
	c := GeneratePair("lon|nyc", t0, 7*24*60)
	if *a[0].RTTAvgUS == *c[0].RTTAvgUS {
		t.Error("different pair keys produced identical first RTTs; RNG not keyed by pair")
	}
}

func TestGeneratePairCadenceAndShape(t *testing.T) {
	rows := GeneratePair("nyc|lon", t0, 60)
	for i, r := range rows {
		if want := t0.Add(time.Duration(i) * Interval); !r.Time.Equal(want) {
			t.Fatalf("row %d at %s, want %s", i, r.Time, want)
		}
		if r.Status == statusOK {
			if r.RTTAvgUS == nil || r.RTTMinUS == nil || r.RTTMaxUS == nil {
				t.Fatalf("row %d: OK row missing RTT fields: %+v", i, r)
			}
			if *r.RTTMinUS > *r.RTTAvgUS || *r.RTTAvgUS > *r.RTTMaxUS {
				t.Fatalf("row %d: min/avg/max out of order: %+v", i, r)
			}
		}
	}
}

// The long outage window (days 25–35) must produce TIMEOUT rows with no
// timings and 100% loss — those are what the aggregate gap and failure
// counts in the gate come from.
func TestGeneratePairOutageWindow(t *testing.T) {
	n := 40 * 24 * 60
	rows := GeneratePair("nyc|lon", t0, n)
	var failed int
	for _, r := range rows {
		if r.Status != statusOK {
			failed++
			if r.RTTAvgUS != nil || r.JitterUS != nil {
				t.Fatalf("outage row carries timings: %+v", r)
			}
			if r.Received != 0 || r.LossPct == nil || *r.LossPct != 100 {
				t.Fatalf("outage row not full loss: %+v", r)
			}
		}
	}
	// 2–6 h at 1/min = 120–360 rows (plus possibly a recent 1 h window).
	if failed < 120 || failed > 420 {
		t.Errorf("outage rows = %d, want a 2–6 h window's worth", failed)
	}
}

func TestPercentilesNearestRank(t *testing.T) {
	mk := func(vals ...int32) []Row {
		rows := make([]Row, len(vals))
		for i := range vals {
			rows[i] = Row{Status: statusOK, RTTAvgUS: &vals[i]}
		}
		return rows
	}
	// 1..100: nearest-rank on indices 0..99 → p50=idx 50 (=51), p95=idx 94
	// (=95), p99=idx 98 (=99).
	vals := make([]int32, 100)
	for i := range vals {
		vals[i] = int32(i + 1)
	}
	p50, p95, p99 := Percentiles(mk(vals...))
	if p50 != 51 || p95 != 95 || p99 != 99 {
		t.Errorf("percentiles = %v/%v/%v, want 51/95/99", p50, p95, p99)
	}
	// Outage rows (nil RTT) are excluded.
	rows := mk(10, 20, 30)
	rows = append(rows, Row{Status: statusTimeout})
	if p50, _, _ := Percentiles(rows); p50 != 20 {
		t.Errorf("p50 with nil rows = %v, want 20", p50)
	}
}
