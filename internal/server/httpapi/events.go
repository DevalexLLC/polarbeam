// Outage, path-event and traceroute endpoints (M4).
package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

type outageJSON struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Agent     string     `json:"agent"`
	SrcSite   string     `json:"src_site"`
	DstSite   *string    `json:"dst_site"`
	Target    *string    `json:"target"`
	ProbeType *string    `json:"probe_type"`
	OpenedAt  time.Time  `json:"opened_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	Error     *string    `json:"error"`
}

func (a *api) handleOutages(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	outages, err := a.db.ListOutages(r.Context(), spec.Window)
	if err != nil {
		internalError(w, "list outages", err)
		return
	}
	out := make([]outageJSON, len(outages))
	for i, o := range outages {
		var probeType *string
		if o.ProbeType != nil {
			name := probeTypeName(*o.ProbeType)
			probeType = &name
		}
		out[i] = outageJSON{
			ID: o.ID.String(), Kind: o.Kind, Agent: o.AgentHostname, SrcSite: o.SrcSite,
			DstSite: o.DstSite, Target: o.TargetName, ProbeType: probeType,
			OpenedAt: o.OpenedAt, ClosedAt: o.ClosedAt, Error: o.Error,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":  windowName(r),
		"outages": out,
	})
}

type pathEventJSON struct {
	ID          string          `json:"id"`
	Time        time.Time       `json:"time"`
	Agent       string          `json:"agent"`
	SrcSite     string          `json:"src_site"`
	DstSite     *string         `json:"dst_site"`
	Target      *string         `json:"target"`
	OldPathHash string          `json:"old_path_hash"`
	NewPathHash string          `json:"new_path_hash"`
	OldHops     json.RawMessage `json:"old_hops"`
	NewHops     json.RawMessage `json:"new_hops"`
}

func (a *api) handlePathEvents(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	events, err := a.db.ListPathEvents(r.Context(), spec.Window)
	if err != nil {
		internalError(w, "list path events", err)
		return
	}
	out := make([]pathEventJSON, len(events))
	for i, e := range events {
		out[i] = pathEventJSON{
			ID: e.ID.String(), Time: e.Time, Agent: e.AgentHostname, SrcSite: e.SrcSite,
			DstSite: e.DstSite, Target: e.TargetName,
			OldPathHash: hex.EncodeToString(e.OldPathHash),
			NewPathHash: hex.EncodeToString(e.NewPathHash),
			OldHops:     json.RawMessage(e.OldHops),
			NewHops:     json.RawMessage(e.NewHops),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window": windowName(r),
		"events": out,
	})
}

type currentPathJSON struct {
	Agent       string          `json:"agent"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DestReached bool            `json:"dest_reached"`
	PathHash    string          `json:"path_hash"`
	Hops        json.RawMessage `json:"hops"`
}

func toCurrentPathJSON(paths []store.CurrentPath) []currentPathJSON {
	out := make([]currentPathJSON, len(paths))
	for i, p := range paths {
		out[i] = currentPathJSON{
			Agent: p.AgentHostname, UpdatedAt: p.UpdatedAt, DestReached: p.DestReached,
			PathHash: hex.EncodeToString(p.PathHash), Hops: json.RawMessage(p.Hops),
		}
	}
	return out
}

func (a *api) handleTraceroute(w http.ResponseWriter, r *http.Request) {
	ea, eb, ok := a.pairEndpoints(w, r)
	if !ok {
		return
	}
	aToB, err := a.db.CurrentPaths(r.Context(), ea.AgentIDs, eb.TargetIDs)
	if err != nil {
		internalError(w, "traceroute a→b", err)
		return
	}
	bToA, err := a.db.CurrentPaths(r.Context(), eb.AgentIDs, ea.TargetIDs)
	if err != nil {
		internalError(w, "traceroute b→a", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"a": ea.Name, "b": eb.Name,
		"a_to_b": map[string]any{"paths": toCurrentPathJSON(aToB)},
		"b_to_a": map[string]any{"paths": toCurrentPathJSON(bToA)},
	})
}

type currentMTUJSON struct {
	Agent           string    `json:"agent"`
	UpdatedAt       time.Time `json:"updated_at"`
	LargestOKBytes  int32     `json:"largest_ok_bytes"`
	SmallestFailed  int32     `json:"smallest_failed_bytes"`
	NextHopMTUBytes int32     `json:"next_hop_mtu_bytes"`
	IPVersion       int16     `json:"ip_version"`
	BlackHole       bool      `json:"black_hole"`
	LocalConstraint bool      `json:"local_constraint"`
	RttUS           *int32    `json:"rtt_us"`
}

func toCurrentMTUJSON(mtus []store.CurrentPathMTU) []currentMTUJSON {
	out := make([]currentMTUJSON, len(mtus))
	for i, m := range mtus {
		out[i] = currentMTUJSON{
			Agent: m.AgentHostname, UpdatedAt: m.UpdatedAt,
			LargestOKBytes: m.LargestOK, SmallestFailed: m.SmallestFailed,
			NextHopMTUBytes: m.NextHopMTU, IPVersion: m.IPVersion,
			BlackHole: m.BlackHole, LocalConstraint: m.LocalConstraint, RttUS: m.RttUS,
		}
	}
	return out
}

func (a *api) handlePathMTU(w http.ResponseWriter, r *http.Request) {
	ea, eb, ok := a.pairEndpoints(w, r)
	if !ok {
		return
	}
	aToB, err := a.db.CurrentPathMTUs(r.Context(), ea.AgentIDs, eb.TargetIDs)
	if err != nil {
		internalError(w, "path MTU a→b", err)
		return
	}
	bToA, err := a.db.CurrentPathMTUs(r.Context(), eb.AgentIDs, ea.TargetIDs)
	if err != nil {
		internalError(w, "path MTU b→a", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"a": ea.Name, "b": eb.Name,
		"a_to_b": map[string]any{"mtus": toCurrentMTUJSON(aToB)},
		"b_to_a": map[string]any{"mtus": toCurrentMTUJSON(bToA)},
	})
}
