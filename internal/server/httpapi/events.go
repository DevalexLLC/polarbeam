// Outage, path-event and traceroute endpoints (M4).
package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

type outageJSON struct {
	ID          string              `json:"id"`
	Kind        string              `json:"kind"`
	AgentID     string              `json:"agent_id"`
	ProbeID     *string             `json:"probe_id"`
	TargetID    *string             `json:"target_id"`
	Agent       string              `json:"agent"`
	Network     string              `json:"network"` // "" once the agent row is deleted
	SrcSite     string              `json:"src_site"`
	DstSite     *string             `json:"dst_site"`
	Target      *string             `json:"target"`
	ProbeType   *string             `json:"probe_type"`
	OpenedAt    time.Time           `json:"opened_at"`
	ClosedAt    *time.Time          `json:"closed_at"`
	Error       *string             `json:"error"`
	RouteEvents []incidentRouteJSON `json:"route_events"`
}

type incidentRouteJSON struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	AgentID  string    `json:"agent_id"`
	ProbeID  string    `json:"probe_id"`
	TargetID *string   `json:"target_id"`
	Agent    string    `json:"agent"`
	Network  string    `json:"network"`
	SrcSite  string    `json:"src_site"`
	DstSite  *string   `json:"dst_site"`
	Target   *string   `json:"target"`
}

func optionalUUIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	value := id.String()
	return &value
}

func (a *api) handleOutages(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	includeRoutes := false
	switch value := r.URL.Query().Get("include_routes"); value {
	case "":
	case "true":
		includeRoutes = true
	case "false":
	default:
		writeError(w, http.StatusBadRequest, "include_routes must be true or false")
		return
	}
	outages, truncated, err := a.db.ListOutages(r.Context(), spec.Window, scopeIDs(r.Context()), includeRoutes)
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
		routes := make([]incidentRouteJSON, len(o.RelatedRoutes))
		for j, event := range o.RelatedRoutes {
			routes[j] = incidentRouteJSON{
				ID: event.ID.String(), Time: event.Time,
				AgentID: event.AgentID.String(), ProbeID: event.ProbeID.String(), TargetID: optionalUUIDString(event.TargetID),
				Agent: event.AgentHostname, Network: event.Network, SrcSite: event.SrcSite,
				DstSite: event.DstSite, Target: event.TargetName,
			}
		}
		out[i] = outageJSON{
			ID: o.ID.String(), Kind: o.Kind,
			AgentID: o.AgentID.String(), ProbeID: optionalUUIDString(o.ProbeID), TargetID: optionalUUIDString(o.TargetID),
			Agent: o.AgentHostname, Network: o.Network, SrcSite: o.SrcSite,
			DstSite: o.DstSite, Target: o.TargetName, ProbeType: probeType,
			OpenedAt: o.OpenedAt, ClosedAt: o.ClosedAt, Error: o.Error, RouteEvents: routes,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window": windowName(r),
		// The dashboard's incident timeline anchors its bucket grid here:
		// the window is evaluated against this clock, so a skewed browser
		// clock must not decide where "now" sits on the chart.
		"now":     time.Now().UTC(),
		"outages": out,
		// True when the oldest OPEN events were cut by the store's safety
		// cap — the dashboard should say the incident list is partial
		// rather than present it as complete.
		"truncated": truncated,
	})
}

type pathEventJSON struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	AgentID string    `json:"agent_id,omitempty"`
	ProbeID string    `json:"probe_id,omitempty"`
	Agent   string    `json:"agent"`
	Network string    `json:"network"` // "" once the agent row is deleted
	SrcSite string    `json:"src_site"`
	DstSite *string   `json:"dst_site"`
	Target  *string   `json:"target"`
	// Query mode retains the event's stable target ID after deletion;
	// legacy mode returns null because it selects the joined display row.
	TargetID    *string         `json:"target_id"`
	OldPathHash string          `json:"old_path_hash"`
	NewPathHash string          `json:"new_path_hash"`
	OldHops     json.RawMessage `json:"old_hops"`
	NewHops     json.RawMessage `json:"new_hops"`
	ChangedHops *int            `json:"changed_hops,omitempty"`
}

var pathEventListSpec = listQuerySpec{
	Sorts:        []string{"time", "agent", "source", "destination", "changes"},
	DefaultSort:  "time",
	DefaultOrder: "desc",
}

func (a *api) handlePathEvents(w http.ResponseWriter, r *http.Request) {
	spec, ok := parseWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want 24h|7d|30d|90d|365d)")
		return
	}
	query, ok := readListQuery(w, r, pathEventListSpec)
	if !ok {
		return
	}
	if !query.Mode {
		events, err := a.db.ListPathEvents(r.Context(), spec.Window, scopeIDs(r.Context()))
		if err != nil {
			internalError(w, "list path events", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"window": windowName(r),
			"events": toPathEventJSON(events, false),
		})
		return
	}
	scope, ok := a.listQueryScope(w, r, query)
	if !ok {
		return
	}
	events, total, truncated, err := a.db.QueryPathEvents(r.Context(), spec.Window, store.PathEventFilter{
		Query: query.Query, Sort: query.Sort, Order: query.Order,
		Limit: query.Limit, Offset: query.Offset, Networks: scope,
	})
	if err != nil {
		internalError(w, "query path events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":    windowName(r),
		"events":    toPathEventJSON(events, true),
		"page":      query.page(total),
		"truncated": truncated,
	})
}

func toPathEventJSON(events []store.PathEventInfo, queryMode bool) []pathEventJSON {
	out := make([]pathEventJSON, len(events))
	for i, e := range events {
		row := pathEventJSON{
			ID: e.ID.String(), Time: e.Time, Agent: e.AgentHostname, Network: e.Network, SrcSite: e.SrcSite,
			DstSite: e.DstSite, Target: e.TargetName, TargetID: optionalUUIDString(e.TargetID),
			OldPathHash: hex.EncodeToString(e.OldPathHash),
			NewPathHash: hex.EncodeToString(e.NewPathHash),
			OldHops:     json.RawMessage(e.OldHops),
			NewHops:     json.RawMessage(e.NewHops),
		}
		if queryMode {
			row.AgentID = e.AgentID.String()
			row.ProbeID = e.ProbeID.String()
			changed := e.ChangedHops
			row.ChangedHops = &changed
		}
		out[i] = row
	}
	return out
}

type currentPathJSON struct {
	// (agent_id, probe_id) is the series identity: several destination
	// agents or templates give one source hostname several probe IDs,
	// while several source agents at one site share a probe ID and
	// differ only by agent ID.
	AgentID     string          `json:"agent_id"`
	ProbeID     string          `json:"probe_id"`
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
			AgentID: p.AgentID.String(), ProbeID: p.ProbeID.String(),
			Agent: p.AgentHostname, UpdatedAt: p.UpdatedAt,
			DestReached: p.DestReached, PathHash: hex.EncodeToString(p.PathHash),
			Hops: json.RawMessage(p.Hops),
		}
	}
	return out
}

func (a *api) handleTraceroute(w http.ResponseWriter, r *http.Request) {
	ea, eb, _, ok := a.pairEndpoints(w, r)
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
	// (agent_id, probe_id) is the series identity: several destination
	// agents or templates give one source hostname several probe IDs,
	// while several source agents at one site share a probe ID and
	// differ only by agent ID.
	AgentID         string    `json:"agent_id"`
	ProbeID         string    `json:"probe_id"`
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
			AgentID: m.AgentID.String(), ProbeID: m.ProbeID.String(),
			Agent: m.AgentHostname, UpdatedAt: m.UpdatedAt,
			LargestOKBytes: m.LargestOK, SmallestFailed: m.SmallestFailed,
			NextHopMTUBytes: m.NextHopMTU, IPVersion: m.IPVersion,
			BlackHole: m.BlackHole, LocalConstraint: m.LocalConstraint, RttUS: m.RttUS,
		}
	}
	return out
}

func (a *api) handlePathMTU(w http.ResponseWriter, r *http.Request) {
	ea, eb, _, ok := a.pairEndpoints(w, r)
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
