package httpapi

import (
	"testing"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func i64(v int64) *int64     { return new(int64(v)) }
func f32(v float32) *float32 { return new(float32(v)) }

func row(src, dst string, status pb.ProbeStatus, latency *int64, loss *float32, at time.Time) store.MatrixRow {
	return store.MatrixRow{
		SrcSite: src, DstSite: dst, ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
		Status: int16(status), Time: at, LatencyUS: latency, LatencySource: "tcp_connect", LossPct: loss,
	}
}

func cellByPair(t *testing.T, cells []cellJSON, src, dst string) cellJSON {
	t.Helper()
	for _, c := range cells {
		if c.Src == src && c.Dst == dst {
			return c
		}
	}
	t.Fatalf("no cell %s→%s in %+v", src, dst, cells)
	return cellJSON{}
}

func TestFoldMatrixStatuses(t *testing.T) {
	now := time.Now()
	rows := []store.MatrixRow{
		// nyc→lon: both series OK → ok, latency = min of the two.
		row("nyc", "lon", pb.ProbeStatus_PROBE_STATUS_OK, i64(1800), f32(0), now),
		row("nyc", "lon", pb.ProbeStatus_PROBE_STATUS_OK, i64(1500), f32(0), now.Add(-time.Minute)),
		// lon→nyc: mixed → degraded, loss = worst.
		row("lon", "nyc", pb.ProbeStatus_PROBE_STATUS_OK, i64(2000), f32(1), now),
		row("lon", "nyc", pb.ProbeStatus_PROBE_STATUS_TIMEOUT, nil, f32(100), now),
		// nyc→syd: all failing → down.
		row("nyc", "syd", pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED, nil, f32(100), now),
	}
	expected := []store.SitePair{
		{Src: "nyc", Dst: "lon"}, {Src: "lon", Dst: "nyc"},
		{Src: "nyc", Dst: "syd"},
		{Src: "syd", Dst: "nyc"}, // configured but silent → stale
	}
	cells := foldMatrix(rows, expected)
	if len(cells) != 4 {
		t.Fatalf("cells = %d, want 4", len(cells))
	}

	ok := cellByPair(t, cells, "nyc", "lon")
	if ok.Status != "ok" || ok.LatencyUS == nil || *ok.LatencyUS != 1500 || ok.LatencySource != "tcp_connect" {
		t.Errorf("nyc→lon = %+v, want ok/1500/tcp_connect", ok)
	}
	if ok.AsOf != now.UTC() && !ok.AsOf.Equal(now) {
		t.Errorf("as_of should be newest row time, got %v", ok.AsOf)
	}

	deg := cellByPair(t, cells, "lon", "nyc")
	if deg.Status != "degraded" || deg.LossPct == nil || *deg.LossPct != 100 {
		t.Errorf("lon→nyc = %+v, want degraded with worst loss 100", deg)
	}
	if len(deg.Probes) != 2 || deg.Probes[1].LossPct == nil || *deg.Probes[1].LossPct != 100 {
		t.Errorf("lon→nyc probes = %+v, want per-probe loss detail", deg.Probes)
	}

	if down := cellByPair(t, cells, "nyc", "syd"); down.Status != "down" {
		t.Errorf("nyc→syd = %+v, want down", down)
	}

	stale := cellByPair(t, cells, "syd", "nyc")
	if stale.Status != "stale" || len(stale.Probes) != 0 {
		t.Errorf("syd→nyc = %+v, want stale with no probes", stale)
	}
}

func TestFoldMatrixEmpty(t *testing.T) {
	if cells := foldMatrix(nil, nil); len(cells) != 0 {
		t.Errorf("empty fold = %+v, want none", cells)
	}
}

func TestDirectionStatus(t *testing.T) {
	now := time.Now()
	okRow := row("a", "b", pb.ProbeStatus_PROBE_STATUS_OK, i64(1), nil, now)
	badRow := row("a", "b", pb.ProbeStatus_PROBE_STATUS_TIMEOUT, nil, nil, now)
	for name, tc := range map[string]struct {
		rows []store.MatrixRow
		want string
	}{
		"empty":  {nil, "stale"},
		"all ok": {[]store.MatrixRow{okRow, okRow}, "ok"},
		"none":   {[]store.MatrixRow{badRow}, "down"},
		"mixed":  {[]store.MatrixRow{okRow, badRow}, "degraded"},
	} {
		if got := directionStatus(tc.rows); got != tc.want {
			t.Errorf("%s: directionStatus = %q, want %q", name, got, tc.want)
		}
	}
}

func TestWindows(t *testing.T) {
	for name, want := range map[string]struct {
		bucket time.Duration
		source store.Source
	}{
		"24h":  {time.Minute, store.SourceRaw},
		"7d":   {5 * time.Minute, store.SourceRaw},
		"30d":  {time.Hour, store.SourceHourly},
		"90d":  {3 * time.Hour, store.SourceHourly},
		"365d": {24 * time.Hour, store.SourceDaily},
	} {
		spec, ok := parseWindow(name)
		if !ok || spec.Bucket != want.bucket || spec.Source != want.source {
			t.Errorf("parseWindow(%q) = %+v %v, want bucket %v source %q", name, spec, ok, want.bucket, want.source)
		}
	}
	if spec, ok := parseWindow(""); !ok || spec.Bucket != time.Minute || spec.Source != store.SourceRaw {
		t.Errorf("default window should be 24h/raw, got %+v %v", spec, ok)
	}
	for _, bad := range []string{"6d", "1y", "never", "0"} {
		if _, ok := parseWindow(bad); ok {
			t.Errorf("parseWindow(%q) accepted", bad)
		}
	}
}
