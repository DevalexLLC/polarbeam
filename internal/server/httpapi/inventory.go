package httpapi

import (
	"net/http"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

type operationalTargetJSON struct {
	ID                string    `json:"id"`
	Kind              string    `json:"kind"`
	Name              string    `json:"name"`
	Address           string    `json:"address"`
	Port              int32     `json:"port"`
	URL               string    `json:"url"`
	Network           string    `json:"network"`
	CreatedAt         time.Time `json:"created_at"`
	ProbeCount        int64     `json:"probe_count"`
	EnabledProbeCount int64     `json:"enabled_probe_count"`
	ProbingSites      []string  `json:"probing_sites"`
	OpenIncidents     int64     `json:"open_incident_count"`
	Status            string    `json:"status"`
	AgentID           *string   `json:"agent_id"`
	AgentSite         *string   `json:"agent_site"`
	AgentHostname     *string   `json:"agent_hostname"`
}

type targetSummaryJSON struct {
	Total       int64 `json:"total"`
	External    int64 `json:"external"`
	Agent       int64 `json:"agent"`
	Incident    int64 `json:"incident"`
	Unprobed    int64 `json:"unprobed"`
	NoIncidents int64 `json:"no_incidents"`
}

var operationalTargetListSpec = listQuerySpec{
	Filters: []listFilterSpec{
		{Name: "kind", Allowed: []string{"agent", "external"}},
		{Name: "status", Allowed: []string{
			store.TargetStatusIncident, store.TargetStatusUnprobed,
			store.TargetStatusNoIncidents,
		}},
	},
	Sorts:        []string{"name", "kind", "status", "created", "probes"},
	DefaultSort:  "name",
	DefaultOrder: "asc",
}

func (a *api) handleOperationalTargets(w http.ResponseWriter, r *http.Request) {
	query, ok := readListQuery(w, r, operationalTargetListSpec)
	if !ok {
		return
	}
	// This endpoint is new and paginated from its first release, so a request
	// without explicit list parameters uses the endpoint defaults rather than
	// entering a legacy mode that never existed.
	if !query.Mode {
		query.Mode = true
		query.Sort = operationalTargetListSpec.DefaultSort
		query.Order = operationalTargetListSpec.DefaultOrder
		query.Limit = listDefaultLimit
	}
	scope, ok := a.listQueryScope(w, r, query)
	if !ok {
		return
	}
	targets, summary, err := a.db.QueryOperationalTargets(r.Context(), store.TargetInventoryFilter{
		Query: query.Query, Kind: query.Filters["kind"], Status: query.Filters["status"],
		Sort: query.Sort, Order: query.Order, Limit: query.Limit,
		Offset: query.Offset, Networks: scope,
	})
	if err != nil {
		internalError(w, "query operational targets", err)
		return
	}
	out := make([]operationalTargetJSON, 0, len(targets))
	for _, target := range targets {
		var agentID *string
		if target.AgentID != nil {
			id := target.AgentID.String()
			agentID = &id
		}
		out = append(out, operationalTargetJSON{
			ID: target.ID.String(), Kind: target.Kind, Name: target.Name,
			Address: target.Address, Port: target.Port, URL: target.URL,
			Network: target.Network, CreatedAt: target.CreatedAt,
			ProbeCount: target.ProbeCount, EnabledProbeCount: target.EnabledProbeCount,
			ProbingSites: target.ProbingSites, OpenIncidents: target.OpenIncidents,
			Status: target.Status, AgentID: agentID, AgentSite: target.AgentSite,
			AgentHostname: target.AgentHostname,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targets": out,
		"page":    query.page(summary.Total),
		"summary": targetSummaryJSON{
			Total: summary.Total, External: summary.External, Agent: summary.Agent,
			Incident: summary.Incident, Unprobed: summary.Unprobed,
			NoIncidents: summary.NoIncidents,
		},
	})
}
