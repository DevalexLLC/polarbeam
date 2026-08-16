package httpapi

import (
	"sort"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

type probeJSON struct {
	Type          string    `json:"type"`
	Status        string    `json:"status"`
	LatencyUS     *int64    `json:"latency_us"`
	LatencySource string    `json:"latency_source"`
	LossPct       *float32  `json:"loss_pct"`
	AsOf          time.Time `json:"as_of"`
}

type cellJSON struct {
	Src           string      `json:"src"`
	Dst           string      `json:"dst"`
	Status        string      `json:"status"`
	LatencyUS     *int64      `json:"latency_us"`
	LatencySource string      `json:"latency_source"`
	LossPct       *float32    `json:"loss_pct"`
	AsOf          time.Time   `json:"as_of"`
	Probes        []probeJSON `json:"probes"`
}

// foldMatrix reduces latest-per-series rows plus the configured pair list
// to one cell per ordered site pair. Pure function: offline-testable.
//
// Cell status: ok when every series' latest result is OK, down when none
// are, degraded when mixed, stale when configuration expects the pair but
// no series reported inside the horizon. Latency is the best (min) over OK
// series — the purest estimate of the path; loss is the worst.
func foldMatrix(rows []store.MatrixRow, expected []store.SitePair) []cellJSON {
	type agg struct {
		rows []store.MatrixRow
	}
	cells := map[store.SitePair]*agg{}
	for _, r := range rows {
		key := store.SitePair{Src: r.SrcSite, Dst: r.DstSite}
		if cells[key] == nil {
			cells[key] = &agg{}
		}
		cells[key].rows = append(cells[key].rows, r)
	}
	for _, p := range expected {
		if cells[p] == nil {
			cells[p] = &agg{} // no data in horizon → stale
		}
	}

	out := make([]cellJSON, 0, len(cells))
	for pair, a := range cells {
		c := cellJSON{Src: pair.Src, Dst: pair.Dst, Status: "stale", Probes: []probeJSON{}}
		okCount := 0
		for _, r := range a.rows {
			ok := r.Status == int16(pb.ProbeStatus_PROBE_STATUS_OK)
			if ok {
				okCount++
				if r.LatencyUS != nil && (c.LatencyUS == nil || *r.LatencyUS < *c.LatencyUS) {
					c.LatencyUS = r.LatencyUS
					c.LatencySource = r.LatencySource
				}
			}
			if r.LossPct != nil && (c.LossPct == nil || *r.LossPct > *c.LossPct) {
				c.LossPct = r.LossPct
			}
			if r.Time.After(c.AsOf) {
				c.AsOf = r.Time
			}
			c.Probes = append(c.Probes, toProbeJSON(r))
		}
		switch {
		case len(a.rows) == 0:
			c.Status = "stale"
		case okCount == len(a.rows):
			c.Status = "ok"
		case okCount == 0:
			c.Status = "down"
		default:
			c.Status = "degraded"
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out
}

func toProbeJSON(r store.MatrixRow) probeJSON {
	return probeJSON{
		Type:          probeTypeName(r.ProbeType),
		Status:        probeStatusName(r.Status),
		LatencyUS:     r.LatencyUS,
		LatencySource: r.LatencySource,
		LossPct:       r.LossPct,
		AsOf:          r.Time,
	}
}

// directionStatus is the same status rule applied to one direction's
// latest-per-series rows (pair detail header), so matrix cells and the
// pair page never disagree.
func directionStatus(rows []store.MatrixRow) string {
	if len(rows) == 0 {
		return "stale"
	}
	okCount := 0
	for _, r := range rows {
		if r.Status == int16(pb.ProbeStatus_PROBE_STATUS_OK) {
			okCount++
		}
	}
	switch okCount {
	case len(rows):
		return "ok"
	case 0:
		return "down"
	default:
		return "degraded"
	}
}

// probeTypeName delegates to probeadmin so a new probe type can never
// render as "unknown" here while the config API knows its name.
func probeTypeName(t int16) string {
	return probeadmin.TypeName(t)
}

func probeStatusName(s int16) string {
	switch pb.ProbeStatus(s) {
	case pb.ProbeStatus_PROBE_STATUS_OK:
		return "ok"
	case pb.ProbeStatus_PROBE_STATUS_TIMEOUT:
		return "timeout"
	case pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED:
		return "conn_refused"
	case pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE:
		return "tls_failure"
	case pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE:
		return "dns_failure"
	case pb.ProbeStatus_PROBE_STATUS_UNSUPPORTED:
		return "unsupported"
	case pb.ProbeStatus_PROBE_STATUS_ERROR:
		return "error"
	default:
		return "unknown"
	}
}
