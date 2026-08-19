package httpapi

import (
	"sort"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

type probeJSON struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	// TargetID links a pair-detail check chip to its target's detail page.
	// omitempty: matrix cells fold to site pairs and never carry it, so
	// the matrix response shape is unchanged.
	TargetID *string `json:"target_id,omitempty"`
	// Network is the series' plane. omitempty: set only on matrix probes
	// (MatrixLatest); pair-detail chips (DirectionLatest) leave it empty
	// because the pair page already filtered by endpoint IDs.
	Network       string    `json:"network,omitempty"`
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
	// Networks breaks the same rows out per plane (length 1 on
	// single-network installs). The top-level fields stay the all-plane
	// fold so pre-networks consumers see an unchanged cell.
	Networks []netCellJSON `json:"networks"`
}

// netCellJSON is one (src, dst, network) fold inside a cell — the same
// aggregate rule at plane granularity, so the SPA's network filter and the
// map's per-plane rollup never re-derive fold logic client-side.
type netCellJSON struct {
	Network       string      `json:"network"`
	Status        string      `json:"status"`
	LatencyUS     *int64      `json:"latency_us"`
	LatencySource string      `json:"latency_source"`
	LossPct       *float32    `json:"loss_pct"`
	AsOf          time.Time   `json:"as_of"`
	Probes        []probeJSON `json:"probes"`
}

// foldMatrix reduces latest-per-series rows plus the configured pair list
// to one cell per ordered site pair, with a per-network breakdown inside
// each cell. Pure function: offline-testable.
//
// Cell status: ok when every series' latest result is OK, down when none
// are, degraded when mixed, stale when configuration expects the pair but
// no series reported inside the horizon. Latency is the best (min) over OK
// series — the purest estimate of the path; loss is the worst. The same
// rule folds each network's rows into its sub-cell; a plane expected by
// configuration but silent in the horizon gets a stale sub-cell, so the
// SPA's filter distinguishes "stale on this plane" from "not probed".
func foldMatrix(rows []store.MatrixRow, expected []store.NetworkPair) []cellJSON {
	type agg struct {
		rows []store.MatrixRow
		nets map[string][]store.MatrixRow
	}
	cells := map[store.SitePair]*agg{}
	get := func(key store.SitePair) *agg {
		a := cells[key]
		if a == nil {
			a = &agg{nets: map[string][]store.MatrixRow{}}
			cells[key] = a
		}
		return a
	}
	for _, r := range rows {
		a := get(store.SitePair{Src: r.SrcSite, Dst: r.DstSite})
		a.rows = append(a.rows, r)
		a.nets[r.Network] = append(a.nets[r.Network], r)
	}
	for _, p := range expected {
		a := get(store.SitePair{Src: p.Src, Dst: p.Dst}) // no data in horizon → stale
		if _, seen := a.nets[p.Network]; !seen {
			a.nets[p.Network] = nil // expected but silent on this plane → stale sub-cell
		}
	}

	out := make([]cellJSON, 0, len(cells))
	for pair, a := range cells {
		c := cellJSON{Src: pair.Src, Dst: pair.Dst, Networks: make([]netCellJSON, 0, len(a.nets))}
		c.Status, c.LatencyUS, c.LatencySource, c.LossPct, c.AsOf, c.Probes = foldGroup(a.rows)
		names := make([]string, 0, len(a.nets))
		for name := range a.nets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			n := netCellJSON{Network: name}
			n.Status, n.LatencyUS, n.LatencySource, n.LossPct, n.AsOf, n.Probes = foldGroup(a.nets[name])
			c.Networks = append(c.Networks, n)
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

// foldGroup applies the cell aggregate rule to one group of rows: status
// from the ok/total split (stale when empty), best (min) OK latency, worst
// loss, latest as_of. Shared by the site-pair and per-network folds so the
// two levels can never disagree.
func foldGroup(rows []store.MatrixRow) (status string, latencyUS *int64, latencySource string, lossPct *float32, asOf time.Time, probes []probeJSON) {
	probes = []probeJSON{}
	okCount := 0
	for _, r := range rows {
		if r.Status == int16(pb.ProbeStatus_PROBE_STATUS_OK) {
			okCount++
			if r.LatencyUS != nil && (latencyUS == nil || *r.LatencyUS < *latencyUS) {
				latencyUS = r.LatencyUS
				latencySource = r.LatencySource
			}
		}
		if r.LossPct != nil && (lossPct == nil || *r.LossPct > *lossPct) {
			lossPct = r.LossPct
		}
		if r.Time.After(asOf) {
			asOf = r.Time
		}
		probes = append(probes, toProbeJSON(r))
	}
	switch {
	case len(rows) == 0:
		status = "stale"
	case okCount == len(rows):
		status = "ok"
	case okCount == 0:
		status = "down"
	default:
		status = "degraded"
	}
	return status, latencyUS, latencySource, lossPct, asOf, probes
}

func toProbeJSON(r store.MatrixRow) probeJSON {
	p := probeJSON{
		Type:          probeTypeName(r.ProbeType),
		Status:        probeStatusName(r.Status),
		Network:       r.Network,
		LatencyUS:     r.LatencyUS,
		LatencySource: r.LatencySource,
		LossPct:       r.LossPct,
		AsOf:          r.Time,
	}
	if r.TargetID != nil {
		tid := r.TargetID.String()
		p.TargetID = &tid
	}
	return p
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
