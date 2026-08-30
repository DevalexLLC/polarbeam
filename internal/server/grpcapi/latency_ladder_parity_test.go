package grpcapi

// rowLatencyUS grades results at ingest with the same purity order the read
// side's latencyExpr COALESCE ladder uses. This test drives it behaviorally
// from the shared fixture (store/testdata/latency-ladder.json) so a reorder
// on either side fails a test instead of grading and display silently
// judging different numbers.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestRowLatencyFollowsFixtureLadder(t *testing.T) {
	b, err := os.ReadFile("../store/testdata/latency-ladder.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f struct {
		Columns []string `json:"columns"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	set := map[string]func(*store.ResultRow, int32){
		"rtt_avg_us":       func(r *store.ResultRow, v int32) { r.RttAvgUS = &v },
		"tcp_connect_us":   func(r *store.ResultRow, v int32) { r.TCPConnectUS = &v },
		"tls_handshake_us": func(r *store.ResultRow, v int32) { r.TLSHandshakeUS = &v },
		"ttfb_us":          func(r *store.ResultRow, v int32) { r.TTFBUS = &v },
		"total_us":         func(r *store.ResultRow, v int32) { r.TotalUS = &v },
	}
	if len(set) != len(f.Columns) {
		t.Fatalf("fixture has %d columns, this test knows %d — extend the setter table", len(f.Columns), len(set))
	}

	// With columns[i:] all measured, the winner must be columns[i].
	for i, col := range f.Columns {
		if _, ok := set[col]; !ok {
			t.Fatalf("fixture column %q unknown to the setter table", col)
		}
		var row store.ResultRow
		for j := i; j < len(f.Columns); j++ {
			set[f.Columns[j]](&row, int32(1000+j))
		}
		got := rowLatencyUS(row)
		if got == nil || *got != int64(1000+i) {
			t.Errorf("row with %v measured: rowLatencyUS picked %v, want %s's value %d",
				f.Columns[i:], got, col, 1000+i)
		}
	}

	if got := rowLatencyUS(store.ResultRow{}); got != nil {
		t.Errorf("empty row: rowLatencyUS = %v, want nil", *got)
	}
}
