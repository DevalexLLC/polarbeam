// Site + enrollment-token management: /api/v1/config/sites and
// /api/v1/config/tokens. Site reads are any-session (viewers already see
// sites everywhere); everything token-shaped is admin-only INCLUDING the
// list — enrollment credentials and their audit metadata are not viewer
// material (same reasoning as GET /settings/oidc).
package httpapi

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/siteadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// siteAPIFields makes shared site validation errors name the JSON fields.
var siteAPIFields = siteadmin.FieldNames{Name: "name", Lat: "latitude", Lon: "longitude"}

// --- sites ---

type siteConfigJSON struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	Location    string    `json:"location"`
	Latitude    *float64  `json:"latitude"`  // null = unplaced
	Longitude   *float64  `json:"longitude"` // null = unplaced
	CreatedAt   time.Time `json:"created_at"`
	AgentCount  int64     `json:"agent_count"`
	MeshCount   int64     `json:"mesh_count"`
	ProbeCount  int64     `json:"probe_count"`
}

// siteRequest is shared by POST (create) and PUT (update). PUT is
// full-state: the SPA form always carries every field, so the request is
// the complete desired site and latitude/longitude null (or omitted — same
// thing after decoding) means "unplaced". Hand-written clients beware:
// omitting the coordinates clears them.
type siteRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Location    string   `json:"location"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
}

// validateSiteCoords applies the both-or-neither and range rules shared by
// POST and PUT.
func validateSiteCoords(in siteRequest) []string {
	var problems []string
	if (in.Latitude == nil) != (in.Longitude == nil) {
		problems = append(problems, "latitude and longitude must be set together")
	} else if in.Latitude != nil {
		problems = append(problems, siteadmin.ValidateCoords(*in.Latitude, *in.Longitude, siteAPIFields)...)
	}
	return problems
}

func toSiteConfigJSON(si store.SiteAdminInfo) siteConfigJSON {
	return siteConfigJSON{
		ID: si.ID.String(), Name: si.Name, DisplayName: si.DisplayName,
		Location: si.Location, Latitude: si.Latitude, Longitude: si.Longitude,
		CreatedAt: si.CreatedAt, AgentCount: si.AgentCount,
		MeshCount: si.MeshCount, ProbeCount: si.ProbeCount,
	}
}

func (a *api) handleSitesConfigGet(w http.ResponseWriter, r *http.Request) {
	sites, err := a.db.ListSitesConfig(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list sites config", err)
		return
	}
	out := make([]siteConfigJSON, 0, len(sites))
	for _, si := range sites {
		out = append(out, toSiteConfigJSON(si))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (a *api) handleSiteConfigPost(w http.ResponseWriter, r *http.Request) {
	var in siteRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	problems := siteadmin.ValidateName(in.Name, siteAPIFields)
	problems = append(problems, validateSiteCoords(in)...)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	up := store.SiteUpdate{
		DisplayName: &in.DisplayName, Location: &in.Location,
		Latitude: in.Latitude, Longitude: in.Longitude,
	}
	id, err := a.db.CreateSite(r.Context(), in.Name, up)
	if err != nil {
		writeStoreError(w, "create site", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

func (a *api) handleSiteConfigPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in siteRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	var problems []string
	// Name is identity, like probe type/site/target on probe PUT.
	if in.Name != "" && in.Name != name {
		problems = append(problems, "name cannot be changed in place (delete and re-create the site)")
	}
	problems = append(problems, validateSiteCoords(in)...)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	up := store.SiteUpdate{DisplayName: &in.DisplayName, Location: &in.Location}
	if in.Latitude == nil {
		up.ClearCoords = true
	} else {
		up.Latitude, up.Longitude = in.Latitude, in.Longitude
	}
	if err := a.db.UpdateSite(r.Context(), name, up); err != nil {
		writeStoreError(w, "update site", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (a *api) handleSiteConfigDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := a.db.DeleteSite(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, "delete site", err)
		return
	}
	// Unused tokens go with the site — deliberate but never silent.
	writeJSON(w, http.StatusOK, map[string]int64{"tokens_deleted": deleted})
}

// --- enrollment tokens ---

type joinTokenJSON struct {
	ID             string     `json:"id"`
	Site           string     `json:"site"`
	Network        string     `json:"network"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	UsedAt         *time.Time `json:"used_at"`
	UsedByAgent    *string    `json:"used_by_agent"`
	UsedByHostname *string    `json:"used_by_hostname"`
}

func (a *api) handleTokensGet(w http.ResponseWriter, r *http.Request) {
	tokens, err := a.db.ListJoinTokens(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list join tokens", err)
		return
	}
	out := make([]joinTokenJSON, 0, len(tokens))
	for _, t := range tokens {
		j := joinTokenJSON{
			ID: t.ID.String(), Site: t.Site, Network: t.Network, CreatedBy: t.CreatedBy,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, UsedAt: t.UsedAt,
			UsedByHostname: t.UsedByHostname,
		}
		if t.UsedByAgent != nil {
			s := t.UsedByAgent.String()
			j.UsedByAgent = &s
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *api) handleTokenPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Site    string `json:"site"`
		Network string `json:"network"`
		TTLMS   int64  `json:"ttl_ms"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	var problems []string
	if in.Site == "" {
		problems = append(problems, "site is required")
	}
	// The upper bound keeps the ms→Duration multiplication from wrapping
	// (a wrapped TTL would 200 while minting an already-expired token).
	if in.TTLMS <= 0 || in.TTLMS > math.MaxInt64/int64(time.Millisecond) {
		problems = append(problems, "ttl_ms must be positive and within range")
	}
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	// SiteIDByName, never EnsureSite: the UI only offers existing sites, so
	// an unknown name here is a bug or a typo'd script — fail loudly.
	siteID, err := a.db.SiteIDByName(r.Context(), in.Site)
	if err != nil {
		writeStoreError(w, "resolve site", err)
		return
	}
	// Empty means the default network; anything else must already exist —
	// a typo'd network is a 404, never silently defaulted (the enrolling
	// agent's plane is a trust decision).
	//
	// A scoped caller has no default to fall back on: enrollment inherits
	// the token's network, so minting one on 'default' would drop an agent
	// onto the operator's plane. It must name a plane it owns, and
	// requireNetworkScopeName refuses the rest as a 404.
	netName := in.Network
	if netName == "" {
		if callerIsScoped(r.Context()) {
			writeError(w, http.StatusBadRequest,
				"network is required: a network-scoped role cannot mint tokens for the default network")
			return
		}
		netName = "default"
	}
	networkID, ok := a.requireNetworkScopeName(w, r, netName)
	if !ok {
		return
	}
	ttl := time.Duration(in.TTLMS) * time.Millisecond
	token, err := a.db.CreateJoinToken(r.Context(), siteID, networkID, sessionFrom(r.Context()).Username, ttl)
	if err != nil {
		// Typed 404 when the site or network was deleted between resolve
		// and insert.
		writeStoreError(w, "create join token", err)
		return
	}
	// The cleartext token exists exactly here, once — only the secret's hash
	// is stored, so it can never be shown again. expires_at is a display
	// convenience; the row's now()+ttl is authoritative.
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"site":       in.Site,
		"network":    netName,
		"expires_at": time.Now().Add(ttl),
	})
}

func (a *api) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "token id must be a UUID")
		return
	}
	// The store refuses out-of-scope tokens with ErrNotFound, so a
	// co-tenant's pending enrollment is not discoverable by id.
	if err := a.db.DeleteJoinToken(r.Context(), id, scopeIDs(r.Context())); err != nil {
		writeStoreError(w, "delete join token", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
