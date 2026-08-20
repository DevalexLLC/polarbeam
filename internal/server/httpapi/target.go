package httpapi

// Target detail endpoints: the per-target mirror of the pair drill-down.
// Reads are any-session, like /config/targets. Latency/loss reuse the pair
// query machinery with dstTargets = [target]; only the stage series and the
// health inventory have target-specific store queries. The health strips'
// slot drill-down deliberately has no endpoint here — each probe row names
// its agent, so the SPA reuses /api/v1/agents/{id}/health/bucket.

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

type targetInfoJSON struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int32  `json:"port"`
	URL     string `json:"url"`
	// DstSite is the owning agent's site for agent-kind targets (the SPA
	// titles those pages by site — targets.name is a synthesized handle);
	// null for external targets.
	DstSite *string `json:"dst_site"`
}

func toTargetInfoJSON(ep *store.TargetEndpoints) targetInfoJSON {
	return targetInfoJSON{
		ID: ep.ID.String(), Kind: ep.Kind, Name: ep.Name,
		Address: ep.Address, Port: ep.Port, URL: ep.URL, DstSite: ep.DstSite,
	}
}

// targetEndpoints resolves the {id} path segment or answers the request
// itself and returns ok=false — the pairEndpoints idiom.
func (a *api) targetEndpoints(w http.ResponseWriter, r *http.Request) (*store.TargetEndpoints, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad target id")
		return nil, false
	}
	ep, err := a.db.TargetEndpoints(r.Context(), id)
	if err != nil {
		internalError(w, "target endpoints", err)
		return nil, false
	}
	if ep == nil {
		writeError(w, http.StatusNotFound, "unknown target")
		return nil, false
	}
	return ep, true
}

// handleTargetSummary serves the target row plus one directionJSON per
// source site (window aggregates + latest checks), the DirectionCard shape
// the pair page uses. Sources is empty (not null) when nothing has probed
// the target yet.
func (a *api) handleTargetSummary(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	ep, ok := a.targetEndpoints(w, r)
	if !ok {
		return
	}
	type sourceJSON struct {
		Site    string `json:"site"`
		Network string `json:"network"`
		directionJSON
	}
	sources := []sourceJSON{}
	for _, src := range ep.Sources {
		dir, err := a.directionFor(r, src.AgentIDs, []uuid.UUID{ep.ID}, spec)
		if err != nil {
			internalError(w, "target summary "+src.Site, err)
			return
		}
		sources = append(sources, sourceJSON{Site: src.Site, Network: src.Network, directionJSON: dir})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":  toTargetInfoJSON(ep),
		"window":  windowName(r),
		"source":  string(spec.Source),
		"sources": sources,
	})
}

// handleTargetSeries serves latency/loss chart series, one per source site,
// each with its own chosen timing family — handleSeries with sites on the
// source axis instead of directions.
func (a *api) handleTargetSeries(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "latency"
	}
	if metric != "latency" && metric != "loss" {
		writeError(w, http.StatusBadRequest, "unknown metric (want latency|loss)")
		return
	}
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	ep, ok := a.targetEndpoints(w, r)
	if !ok {
		return
	}
	type sourceSeriesJSON struct {
		Site          string      `json:"site"`
		Network       string      `json:"network"`
		LatencySource string      `json:"latency_source"`
		Points        []pointJSON `json:"points"`
	}
	dstTargets := []uuid.UUID{ep.ID}
	sources := []sourceSeriesJSON{}
	for _, src := range ep.Sources {
		family, err := a.db.PairLatencySource(r.Context(), src.AgentIDs, dstTargets, spec.Window, spec.Source)
		if err != nil {
			internalError(w, "target series source "+src.Site, err)
			return
		}
		points, err := a.db.PairSeries(r.Context(), src.AgentIDs, dstTargets, spec.Bucket, spec.Window, spec.Source, family)
		if err != nil {
			internalError(w, "target series "+src.Site, err)
			return
		}
		sources = append(sources, sourceSeriesJSON{
			Site: src.Site, Network: src.Network, LatencySource: family, Points: toPoints(points),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metric": metric, "window": windowName(r),
		"resolution_s": int(spec.Bucket.Seconds()),
		"source":       string(spec.Source),
		"sources":      sources,
	})
}

type stagePointJSON struct {
	T              int64    `json:"t"`
	DNSUS          *float64 `json:"dns_us"`
	TCPConnectUS   *float64 `json:"tcp_connect_us"`
	TLSHandshakeUS *float64 `json:"tls_handshake_us"`
	TTFBUS         *float64 `json:"ttfb_us"`
	TotalUS        *float64 `json:"total_us"`
	Samples        int64    `json:"samples"`
}

// handleTargetStages serves the stage-timing breakdown (per-stage successful
// averages) folded across every source agent. Stages a probe type does not
// measure are null; probes with no application timings at all (icmp,
// traceroute, path_mtu) yield buckets with every stage null, which the SPA
// renders as "no stage timings". Long windows read the stage caggs
// (migrations 0014/0015), which backfill only from raw surviving their
// deployment.
func (a *api) handleTargetStages(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	ep, ok := a.targetEndpoints(w, r)
	if !ok {
		return
	}
	var srcAgents []uuid.UUID
	for _, src := range ep.Sources {
		srcAgents = append(srcAgents, src.AgentIDs...)
	}
	buckets, err := a.db.TargetStageSeries(r.Context(), srcAgents, ep.ID, spec.Bucket, spec.Window, spec.Source)
	if err != nil {
		internalError(w, "target stages", err)
		return
	}
	points := make([]stagePointJSON, len(buckets))
	for i, b := range buckets {
		points[i] = stagePointJSON{
			T: b.Bucket.Unix(), DNSUS: b.DNSUS, TCPConnectUS: b.TCPUS,
			TLSHandshakeUS: b.TLSUS, TTFBUS: b.TTFBUS, TotalUS: b.TotalUS,
			Samples: b.Samples,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":       windowName(r),
		"resolution_s": int(spec.Bucket.Seconds()),
		"source":       string(spec.Source),
		"points":       points,
	})
}

// handleTargetPaths serves the latest complete traceroute per source site —
// handleTraceroute with sites on the source axis instead of a pair's two
// directions. Sites whose agents run no traceroute probe against the target
// yield an empty paths list, so the SPA can hide the card when nothing
// traces the target.
func (a *api) handleTargetPaths(w http.ResponseWriter, r *http.Request) {
	ep, ok := a.targetEndpoints(w, r)
	if !ok {
		return
	}
	type sourcePathsJSON struct {
		Site    string            `json:"site"`
		Network string            `json:"network"`
		Paths   []currentPathJSON `json:"paths"`
	}
	dstTargets := []uuid.UUID{ep.ID}
	sources := []sourcePathsJSON{}
	for _, src := range ep.Sources {
		paths, err := a.db.CurrentPaths(r.Context(), src.AgentIDs, dstTargets)
		if err != nil {
			internalError(w, "target paths "+src.Site, err)
			return
		}
		sources = append(sources, sourcePathsJSON{Site: src.Site, Network: src.Network, Paths: toCurrentPathJSON(paths)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":  toTargetInfoJSON(ep),
		"sources": sources,
	})
}

// targetProbeHealthJSON is agentProbeHealthJSON from the target's side:
// source agent/site labels replace the target labels, and agent_id is what
// lets the SPA reuse the per-agent bucket drill-down endpoint.
type targetProbeHealthJSON struct {
	AgentID    string                  `json:"agent_id"`
	Site       string                  `json:"site"`
	Network    string                  `json:"network"`
	Hostname   string                  `json:"hostname"`
	ProbeID    string                  `json:"probe_id"`
	Type       string                  `json:"type"`
	LastStatus string                  `json:"last_status"`
	LastTime   time.Time               `json:"last_time"`
	Failing    bool                    `json:"failing"`
	OpenSince  *time.Time              `json:"open_since"`
	Error      *string                 `json:"error"`
	Buckets    []agentHealthBucketJSON `json:"buckets"`
}

// handleTargetHealth serves the target's per-probe bucketed success counts
// for its health strips — handleAgentProbeHealth's contract (fixed 24h/30m
// window, silent series appear with empty buckets) keyed by target.
func (a *api) handleTargetHealth(w http.ResponseWriter, r *http.Request) {
	if win := r.URL.Query().Get("window"); win != "" && win != "24h" {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h)")
		return
	}
	ep, ok := a.targetEndpoints(w, r)
	if !ok {
		return
	}
	rows, err := a.db.TargetProbeHealth(r.Context(), ep.ID, agentHealthWindow, agentHealthBucket)
	if err != nil {
		internalError(w, "target probe health", err)
		return
	}
	probes := []targetProbeHealthJSON{}
	for _, row := range rows {
		pid := row.ProbeID.String()
		aid := row.AgentID.String()
		if n := len(probes); n == 0 || probes[n-1].ProbeID != pid || probes[n-1].AgentID != aid {
			probes = append(probes, targetProbeHealthJSON{
				AgentID:    aid,
				Site:       row.SrcSite,
				Network:    row.Network,
				Hostname:   row.Hostname,
				ProbeID:    pid,
				Type:       probeTypeName(row.ProbeType),
				LastStatus: probeStatusName(row.LastStatus),
				LastTime:   row.LastTime,
				Failing:    row.OpenedAt != nil,
				OpenSince:  row.OpenedAt,
				Error:      row.OpenError,
				Buckets:    []agentHealthBucketJSON{},
			})
		}
		if row.Bucket != nil {
			last := &probes[len(probes)-1]
			last.Buckets = append(last.Buckets,
				agentHealthBucketJSON{T: row.Bucket.Unix(), Samples: *row.Samples, OK: *row.OK})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":   "24h",
		"bucket_s": int(agentHealthBucket.Seconds()),
		"target":   toTargetInfoJSON(ep),
		"probes":   probes,
	})
}
