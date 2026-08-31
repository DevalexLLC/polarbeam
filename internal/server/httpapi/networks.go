package httpapi

// Network configuration endpoints. Reads are any-session — networks are
// topology vocabulary like sites and meshes, and the reference counts
// reveal nothing viewers can't already see; writes are admin-only.

import (
	"net/http"
	"strings"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/configadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// networkAPIFields makes shared validation errors name the JSON keys.
var networkAPIFields = configadmin.NetworkFields{Name: "name"}

type networkConfigJSON struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	AgentCount  int64     `json:"agent_count"`
	TokenCount  int64     `json:"token_count"`
	MeshCount   int64     `json:"mesh_count"`
	ProbeCount  int64     `json:"probe_count"`
	// Tenant-owned external targets; like the counts above, a non-zero
	// value blocks DELETE.
	TargetCount int64 `json:"target_count"`
}

func toNetworkConfigJSON(ni store.NetworkAdminInfo) networkConfigJSON {
	return networkConfigJSON{
		ID: ni.ID.String(), Name: ni.Name, DisplayName: ni.DisplayName,
		CreatedAt: ni.CreatedAt, AgentCount: ni.AgentCount, TokenCount: ni.TokenCount,
		MeshCount: ni.MeshCount, ProbeCount: ni.ProbeCount, TargetCount: ni.TargetCount,
	}
}

func (a *api) handleNetworksGet(w http.ResponseWriter, r *http.Request) {
	networks, err := a.db.ListNetworksConfig(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list networks config", err)
		return
	}
	out := make([]networkConfigJSON, 0, len(networks))
	for _, n := range networks {
		out = append(out, toNetworkConfigJSON(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

type networkRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

func (a *api) handleNetworkPost(w http.ResponseWriter, r *http.Request) {
	var in networkRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	if problems := configadmin.ValidateNetworkName(in.Name, networkAPIFields); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	id, err := a.db.CreateNetwork(r.Context(), in.Name, in.DisplayName)
	if err != nil {
		writeStoreError(w, "create network", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

func (a *api) handleNetworkPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in networkRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	// The name is immutable everywhere: it is shared operator vocabulary
	// (token minting, mesh binding), and a rename would silently repoint it.
	if in.Name != "" && in.Name != name {
		writeError(w, http.StatusBadRequest, "name cannot be changed in place (delete and re-create the network)")
		return
	}
	if err := a.db.UpdateNetwork(r.Context(), name, in.DisplayName); err != nil {
		writeStoreError(w, "update network", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (a *api) handleNetworkDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := a.db.DeleteNetwork(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, "delete network", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"tokens_deleted": deleted})
}
