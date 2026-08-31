// Probe-workload management: /api/v1/config/* — the HTTP face of the same
// store mutations the admin CLI performs. Reads are any-session. Writes are
// network-scoped: a global admin writes any plane, a network_admin only its
// own, and every handler here proves the touched resource's plane before it
// mutates (see requireNetworkScope in httpapi.go). Changes propagate to
// agents without further action: the gRPC StreamConfig tick rebuilds
// snapshots from the DB every ~30 s.
package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/configadmin"
	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// --- probe types (param registry) ---

type probeTypeJSON struct {
	Type       string                 `json:"type"`
	DirectOnly bool                   `json:"direct_only,omitempty"`
	Params     []probeadmin.ParamSpec `json:"params"`
}

func (a *api) handleProbeTypes(w http.ResponseWriter, r *http.Request) {
	names := probeadmin.Names()
	types := make([]probeTypeJSON, 0, len(names))
	for _, name := range names {
		t := probeadmin.TypeNames[name]
		params := probeadmin.Params(t)
		if params == nil {
			params = []probeadmin.ParamSpec{}
		}
		types = append(types, probeTypeJSON{Type: name, DirectOnly: probeadmin.DirectOnly(t), Params: params})
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": types})
}

// --- targets ---

type targetJSON struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	Port    int32  `json:"port,omitempty"`
	URL     string `json:"url,omitempty"`
	// Network is the owning plane, "" for a global (operator-owned) target
	// every plane may probe. Always present so the SPA can tell "global,
	// read-only to me" from "mine".
	Network    string    `json:"network"`
	ProbeCount int64     `json:"probe_count"`
	CreatedAt  time.Time `json:"created_at"`
}

func toTargetJSON(t store.TargetInfo) targetJSON {
	return targetJSON{
		ID: t.ID.String(), Kind: t.Kind, Name: t.Name, Address: t.Address,
		Port: t.Port, URL: t.URL, Network: t.Network,
		ProbeCount: t.ProbeCount, CreatedAt: t.CreatedAt,
	}
}

var targetConfigListSpec = listQuerySpec{
	Filters:      []listFilterSpec{{Name: "kind", Allowed: []string{"agent", "external"}}},
	Sorts:        []string{"name", "kind", "network", "probes", "created"},
	DefaultSort:  "name",
	DefaultOrder: "asc",
}

func (a *api) handleTargetsGet(w http.ResponseWriter, r *http.Request) {
	query, ok := readListQuery(w, r, targetConfigListSpec)
	if !ok {
		return
	}
	if !query.Mode {
		a.handleTargetsLegacy(w, r)
		return
	}
	scope, ok := a.listQueryScope(w, r, query)
	if !ok {
		return
	}
	targets, total, err := a.db.QueryTargetsConfig(r.Context(), store.TargetConfigFilter{
		Query: query.Query, Kind: query.Filters["kind"], Sort: query.Sort,
		Order: query.Order, Limit: query.Limit, Offset: query.Offset, Networks: scope,
	})
	if err != nil {
		internalError(w, "query targets config", err)
		return
	}
	out := make([]targetJSON, 0, len(targets))
	for _, t := range targets {
		out = append(out, toTargetJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out, "page": query.page(total)})
}

func (a *api) handleTargetsLegacy(w http.ResponseWriter, r *http.Request) {
	targets, err := a.db.ListTargets(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list targets", err)
		return
	}
	out := make([]targetJSON, 0, len(targets))
	for _, t := range targets {
		out = append(out, toTargetJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

func (a *api) handleTargetConfigGet(w http.ResponseWriter, r *http.Request) {
	target, err := a.db.GetTargetConfig(r.Context(), r.PathValue("name"), scopeIDs(r.Context()))
	if err != nil {
		writeStoreError(w, "get target config", err)
		return
	}
	writeJSON(w, http.StatusOK, toTargetJSON(*target))
}

// targetAPIFields makes shared target validation errors name the JSON fields.
var targetAPIFields = configadmin.TargetFields{Name: "name", Address: "address", URL: "url", Port: "port"}

type targetRequest struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int32  `json:"port"`
	URL     string `json:"url"`
	// Network claims the target for a plane. A global admin may omit it,
	// which creates the operator-owned target every plane can probe; a
	// tenant admin MUST name one of its own planes — it has no way to
	// express "global", and silently defaulting would hand it a write on a
	// row every other tenant reads.
	Network string `json:"network"`
}

func (a *api) handleTargetPost(w http.ResponseWriter, r *http.Request) {
	var in targetRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	problems := configadmin.ValidateTarget(in.Name, in.Address, in.URL, int64(in.Port), targetAPIFields)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	var networkID *uuid.UUID
	switch {
	case in.Network != "":
		id, ok := a.requireNetworkScopeName(w, r, in.Network)
		if !ok {
			return
		}
		networkID = &id
	case callerIsScoped(r.Context()):
		writeError(w, http.StatusBadRequest,
			"network is required: a network-scoped role cannot create the global targets that every plane shares")
		return
	}
	id, err := a.db.UpsertExternalTarget(r.Context(), in.Name, in.Address, in.Port, in.URL,
		networkID, scopeIDs(r.Context()))
	if err != nil {
		writeStoreError(w, "upsert target", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

func (a *api) handleTargetDelete(w http.ResponseWriter, r *http.Request) {
	// The store refuses out-of-scope rows with ErrNotFound, so a tenant
	// cannot tell a co-tenant's target from one that never existed.
	if err := a.db.DeleteTarget(r.Context(), r.PathValue("name"), scopeIDs(r.Context())); err != nil {
		writeStoreError(w, "delete target", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// --- meshes ---

type meshJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Network    string   `json:"network"`
	Sites      []string `json:"sites"`
	ProbeCount int64    `json:"probe_count"`
}

func (a *api) handleMeshesGet(w http.ResponseWriter, r *http.Request) {
	meshes, err := a.db.ListMeshGroups(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list meshes", err)
		return
	}
	out := make([]meshJSON, 0, len(meshes))
	for _, m := range meshes {
		sites := m.Sites
		if sites == nil {
			sites = []string{}
		}
		out = append(out, meshJSON{ID: m.ID.String(), Name: m.Name, Network: m.Network, Sites: sites, ProbeCount: m.ProbeCount})
	}
	writeJSON(w, http.StatusOK, map[string]any{"meshes": out})
}

func (a *api) handleMeshPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Network string `json:"network"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// An omitted network expresses no opinion (new meshes land on default,
	// existing ones keep theirs); a named one must exist and must match an
	// existing mesh's binding — the store refuses a mismatch.
	//
	// A scoped caller may not express "no opinion": the default it would
	// land on is the operator's plane. It must name one of its own, and
	// requireNetworkScopeName refuses anything else as a 404. That also
	// covers the re-upsert path — a tenant naming its own plane on a mesh
	// bound elsewhere gets the store's plain conflict, never a write.
	var networkID *uuid.UUID
	switch {
	case in.Network != "":
		id, ok := a.requireNetworkScopeName(w, r, in.Network)
		if !ok {
			return
		}
		networkID = &id
	case callerIsScoped(r.Context()):
		writeError(w, http.StatusBadRequest,
			"network is required: a network-scoped role cannot create meshes on the default network")
		return
	}
	id, err := a.db.UpsertMeshGroup(r.Context(), in.Name, networkID)
	if err != nil {
		writeStoreError(w, "upsert mesh", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

// Mesh writes authorize inside the store, under the mesh row lock (see
// lockMesh): mesh_groups.network_id is NOT NULL, so every mesh has exactly
// one plane, and membership and deletion both authorize through it rather
// than through the member sites, which are shared operator vocabulary.
//
// Deliberately NOT a handler-side name→network pre-check. Mesh names are
// globally unique but reusable, so a check here could be invalidated by a
// delete-and-recreate on another plane before the store re-resolved the
// name. Passing the scope down means the row that gets mutated is the row
// that got checked.

func (a *api) handleMeshDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := a.db.DeleteMeshGroup(r.Context(), r.PathValue("name"), scopeIDs(r.Context()))
	if err != nil {
		writeStoreError(w, "delete mesh", err)
		return
	}
	// The FK cascade is deliberate but never silent: the response carries
	// how many probe templates went with the mesh.
	writeJSON(w, http.StatusOK, map[string]int64{"probes_deleted": deleted})
}

func (a *api) handleMeshMemberPost(w http.ResponseWriter, r *http.Request) {
	// Authorization is the mesh's plane, not the site's: meshexpand pairs
	// only same-plane agents, so adding a shared site to a tenant's mesh
	// grants that tenant nothing it could not already measure.
	if err := a.db.AddMeshMember(r.Context(), r.PathValue("name"), r.PathValue("site"), scopeIDs(r.Context())); err != nil {
		writeStoreError(w, "add mesh member", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (a *api) handleMeshMemberDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.db.RemoveMeshMember(r.Context(), r.PathValue("name"), r.PathValue("site"), scopeIDs(r.Context())); err != nil {
		writeStoreError(w, "remove mesh member", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// --- probes ---

type probeCfgJSON struct {
	ID             string            `json:"id"`
	Site           string            `json:"site,omitempty"`
	Target         string            `json:"target,omitempty"`
	Mesh           string            `json:"mesh,omitempty"`
	Network        string            `json:"network"`
	Type           string            `json:"type"`
	IntervalMS     int64             `json:"interval_ms"`
	TimeoutMS      int64             `json:"timeout_ms"`
	TrainCount     int32             `json:"train_count"`
	TrainSpacingMS int64             `json:"train_spacing_ms"`
	Params         map[string]string `json:"params"`
	Enabled        bool              `json:"enabled"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	UpdatedBy      string            `json:"updated_by,omitempty"`
	// Warnings is advisory and set only on a write response; list payloads
	// omit it entirely.
	Warnings []string `json:"warnings,omitempty"`
}

func toProbeCfgJSON(p store.ProbeConfigInfo) probeCfgJSON {
	params := p.Params
	if params == nil {
		params = map[string]string{}
	}
	return probeCfgJSON{
		ID: p.ID.String(), Site: p.Site, Target: p.Target, Mesh: p.Mesh, Network: p.Network,
		Type:       probeadmin.TypeName(p.ProbeType),
		IntervalMS: p.Interval.Milliseconds(), TimeoutMS: p.Timeout.Milliseconds(),
		TrainCount: p.TrainCount, TrainSpacingMS: p.TrainSpacing.Milliseconds(),
		Params: params, Enabled: p.Enabled,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, UpdatedBy: p.UpdatedBy,
	}
}

var probeConfigListSpec = listQuerySpec{
	Filters: []listFilterSpec{
		{Name: "mode", Allowed: []string{"direct", "mesh"}},
		{Name: "enabled", Allowed: []string{"true", "false"}},
		{Name: "type", Allowed: probeadmin.Names()},
	},
	Sorts:        []string{"site", "target", "type", "enabled", "updated"},
	DefaultSort:  "site",
	DefaultOrder: "asc",
}

func (a *api) handleProbesGet(w http.ResponseWriter, r *http.Request) {
	query, ok := readListQuery(w, r, probeConfigListSpec)
	if !ok {
		return
	}
	if !query.Mode {
		a.handleProbesLegacy(w, r)
		return
	}
	scope, ok := a.listQueryScope(w, r, query)
	if !ok {
		return
	}
	var enabled *bool
	if value, present := query.Filters["enabled"]; present {
		parsed := value == "true"
		enabled = &parsed
	}
	var probeType int16
	if value, present := query.Filters["type"]; present {
		probeType = int16(probeadmin.TypeNames[value])
	}
	probes, total, err := a.db.QueryProbeConfigs(r.Context(), store.ProbeConfigFilter{
		Query: query.Query, Mode: query.Filters["mode"], Enabled: enabled,
		ProbeType: probeType, Sort: query.Sort, Order: query.Order,
		Limit: query.Limit, Offset: query.Offset, Networks: scope,
	})
	if err != nil {
		internalError(w, "query probes", err)
		return
	}
	out := make([]probeCfgJSON, 0, len(probes))
	for _, p := range probes {
		out = append(out, toProbeCfgJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"probes": out, "page": query.page(total)})
}

func (a *api) handleProbesLegacy(w http.ResponseWriter, r *http.Request) {
	probes, err := a.db.ListProbeConfigs(r.Context(), scopeIDs(r.Context()))
	if err != nil {
		internalError(w, "list probes", err)
		return
	}
	out := make([]probeCfgJSON, 0, len(probes))
	for _, p := range probes {
		out = append(out, toProbeCfgJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"probes": out})
}

func (a *api) handleProbeGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "probe id must be a UUID")
		return
	}
	probe, err := a.db.GetProbeConfigScoped(r.Context(), id, scopeIDs(r.Context()))
	if err != nil {
		writeStoreError(w, "get probe config", err)
		return
	}
	writeJSON(w, http.StatusOK, toProbeCfgJSON(*probe))
}

type probeRequest struct {
	Site           string            `json:"site"`
	Target         string            `json:"target"`
	Mesh           string            `json:"mesh"`
	Network        string            `json:"network"`
	Type           string            `json:"type"`
	IntervalMS     int64             `json:"interval_ms"`
	TimeoutMS      int64             `json:"timeout_ms"`
	TrainCount     int32             `json:"train_count"`
	TrainSpacingMS int64             `json:"train_spacing_ms"`
	Params         map[string]string `json:"params"`
	Enabled        *bool             `json:"enabled"`
}

// probeAPIFields makes shared validation errors name the JSON keys.
var probeAPIFields = probeadmin.FieldNames{
	Interval: "interval_ms", Timeout: "timeout_ms",
	TrainCount: "train_count", TrainSpacing: "train_spacing_ms",
}

// validateProbeRequest runs the shared cadence/param validation for an
// already-resolved type and assignment mode, returning every problem.
func validateProbeRequest(in probeRequest, probeType pb.ProbeType, mesh bool) []string {
	interval := time.Duration(in.IntervalMS) * time.Millisecond
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	spacing := time.Duration(in.TrainSpacingMS) * time.Millisecond
	problems := probeadmin.ValidateSettings(probeType, interval, timeout, int(in.TrainCount), spacing, probeAPIFields)
	return append(problems, probeadmin.ValidateParams(probeType, mesh, in.Params)...)
}

func (in probeRequest) settings(probeType pb.ProbeType) store.ProbeSettings {
	params := in.Params
	if params == nil {
		// An omitted params object means "no parameters", not NULL — the
		// probe_configs column is NOT NULL, and a PUT without params would
		// otherwise 500 at the database.
		params = map[string]string{}
	}
	return store.ProbeSettings{
		ProbeType:    int16(probeType),
		Interval:     time.Duration(in.IntervalMS) * time.Millisecond,
		Timeout:      time.Duration(in.TimeoutMS) * time.Millisecond,
		TrainCount:   in.TrainCount,
		TrainSpacing: time.Duration(in.TrainSpacingMS) * time.Millisecond,
		Params:       params,
	}
}

func (a *api) handleProbePost(w http.ResponseWriter, r *http.Request) {
	var in probeRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	probeType, err := probeadmin.ParseType(in.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meshMode := in.Mesh != ""
	directMode := in.Site != "" || in.Target != ""
	if meshMode == directMode {
		writeError(w, http.StatusBadRequest, "exactly one of mesh or site+target is required")
		return
	}
	if directMode && (in.Site == "" || in.Target == "") {
		writeError(w, http.StatusBadRequest, "site and target are both required for a direct probe")
		return
	}
	if meshMode && in.Network != "" {
		writeError(w, http.StatusBadRequest, "network cannot be combined with mesh (mesh probes inherit the mesh's network)")
		return
	}
	if problems := validateProbeRequest(in, probeType, meshMode); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	enabled := in.Enabled == nil || *in.Enabled
	updatedBy := sessionFrom(r.Context()).Username
	var id uuid.UUID
	if meshMode {
		// A mesh probe inherits the mesh's plane, so the mesh IS the
		// authorization subject — proved in the store under the row lock.
		id, err = a.db.AddMeshProbe(r.Context(), in.Mesh, in.settings(probeType), enabled, updatedBy,
			scopeIDs(r.Context()))
	} else {
		// Empty means the default network; anything else must already
		// exist — a typo'd network is a 404, never silently defaulted.
		// A scoped caller gets no default: 'default' is the operator's
		// plane unless it happens to be in scope, so it must say which.
		netName := in.Network
		if netName == "" {
			if callerIsScoped(r.Context()) {
				writeError(w, http.StatusBadRequest,
					"network is required: a network-scoped role cannot create probes on the default network")
				return
			}
			netName = "default"
		}
		networkID, ok := a.requireNetworkScopeName(w, r, netName)
		if !ok {
			return
		}
		// The target may be global (operator-published) or owned by a plane
		// in scope — AddDirectProbe enforces that against the caller's
		// scope, answering 404 for a co-tenant's target exactly as it does
		// for a name that does not exist.
		id, err = a.db.AddDirectProbe(r.Context(), in.Site, in.Target, networkID,
			in.settings(probeType), enabled, updatedBy, scopeIDs(r.Context()))
	}
	if err != nil {
		writeStoreError(w, "add probe", err)
		return
	}
	// Advisory only: the probe was created. Warnings ride the success
	// response so the UI can show what the configuration will actually
	// measure when that differs from the likely intent — unless it was
	// created disabled: a disabled probe measures nothing (same rationale
	// as the PUT path).
	out := map[string]any{"id": id.String()}
	if enabled {
		if warnings := probeadmin.Warnings(probeType, meshMode,
			time.Duration(in.IntervalMS)*time.Millisecond, time.Duration(in.TimeoutMS)*time.Millisecond, in.Params); len(warnings) > 0 {
			out["warnings"] = warnings
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) handleProbePut(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "probe id must be a UUID")
		return
	}
	var in probeRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	current, err := a.db.GetProbeConfig(r.Context(), id)
	if err != nil {
		writeStoreError(w, "get probe", err)
		return
	}
	// GetProbeConfig is deliberately scope-blind (ingest and the CLI use it
	// too), so the scope proof happens here, on the row it returned.
	// ProbeConfigInfo.Network already resolves a mesh template's plane to
	// the mesh's, so direct and mesh rows authorize identically.
	if !a.probeInScope(w, r, current) {
		return
	}

	// Identity is immutable in place: the probe ID is stored in
	// probe_results (and, for mesh templates, the type is baked into the
	// expanded UUIDv5 IDs), so changing type or assignment would splice
	// unrelated measurements under one series. Re-target = delete + create.
	var problems []string
	check := func(field, got, want string) {
		if got != "" && got != want {
			problems = append(problems, field+" cannot be changed in place (delete and re-create the probe)")
		}
	}
	check("type", in.Type, probeadmin.TypeName(current.ProbeType))
	check("site", in.Site, current.Site)
	check("target", in.Target, current.Target)
	check("mesh", in.Mesh, current.Mesh)
	check("network", in.Network, current.Network)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	probeType := pb.ProbeType(current.ProbeType)
	enabled := current.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if problems := validateProbeRequest(in, probeType, current.Mesh != ""); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	updatedBy := sessionFrom(r.Context()).Username
	if err := a.db.UpdateProbeConfig(r.Context(), id, in.settings(probeType), enabled, updatedBy); err != nil {
		writeStoreError(w, "update probe", err)
		return
	}
	updated, err := a.db.GetProbeConfig(r.Context(), id)
	if err != nil {
		writeStoreError(w, "get probe", err)
		return
	}
	// Edits reach the same configurations creates do — clearing
	// "dns.resolver" on a mesh dns probe is an edit, not a create — so the
	// advisory has to ride this response too or the warning is trivially
	// bypassed by the normal edit path. A disabled probe measures nothing,
	// so describing what it would query would be noise on the one write
	// that just stopped it.
	out := toProbeCfgJSON(*updated)
	if enabled {
		out.Warnings = probeadmin.Warnings(probeType, current.Mesh != "",
			time.Duration(in.IntervalMS)*time.Millisecond, time.Duration(in.TimeoutMS)*time.Millisecond, in.Params)
	}
	writeJSON(w, http.StatusOK, out)
}

// probeInScope proves the caller may write a probe row, answering the
// request itself when not. The refusal is the same 404 an unknown probe id
// produces, so a tenant cannot confirm that a UUID it guessed belongs to
// someone else's plane.
func (a *api) probeInScope(w http.ResponseWriter, r *http.Request, p *store.ProbeConfigInfo) bool {
	names := scopeNames(r.Context())
	if names == nil || slices.Contains(names, p.Network) {
		return true
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("probe config %s does not exist", p.ID))
	return false
}

func (a *api) handleProbeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "probe id must be a UUID")
		return
	}
	// Delete had no pre-fetch before tenancy; it needs one now, because the
	// row is the only place its plane is recorded.
	current, err := a.db.GetProbeConfig(r.Context(), id)
	if err != nil {
		writeStoreError(w, "get probe", err)
		return
	}
	if !a.probeInScope(w, r, current) {
		return
	}
	if err := a.db.DeleteProbeConfig(r.Context(), id); err != nil {
		writeStoreError(w, "delete probe", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
