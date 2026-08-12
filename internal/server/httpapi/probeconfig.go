// Probe-workload management: /api/v1/config/* — the HTTP face of the same
// store mutations the admin CLI performs. Reads are any-session; writes are
// admin-only. Changes propagate to agents without further action: the gRPC
// StreamConfig tick rebuilds snapshots from the DB every ~30 s.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/targetadmin"
)

// decodeStrict decodes exactly one JSON object, rejecting unknown fields
// (a client bug or version skew — never silently dropped) and trailing
// data. It writes the 400/413 itself; callers bail on false.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if isBodyTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if isBodyTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid body: trailing data after JSON object")
		return false
	}
	return true
}

// isBodyTooLarge writes a 413 and reports true when err is the body-limit
// middleware's overflow (withBodyLimit); any other error is the caller's.
func isBodyTooLarge(w http.ResponseWriter, err error) bool {
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		return false
	}
	writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	return true
}

// writeStoreError maps the store's typed admin errors onto HTTP statuses;
// anything untyped is an internal error (logged, opaque 500).
func writeStoreError(w http.ResponseWriter, what string, err error) {
	var inUse store.InUseError
	switch {
	case errors.As(err, &inUse):
		writeError(w, http.StatusConflict, inUse.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		internalError(w, what, err)
	}
}

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
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Address    string    `json:"address,omitempty"`
	Port       int32     `json:"port,omitempty"`
	URL        string    `json:"url,omitempty"`
	ProbeCount int64     `json:"probe_count"`
	CreatedAt  time.Time `json:"created_at"`
}

func (a *api) handleTargetsGet(w http.ResponseWriter, r *http.Request) {
	targets, err := a.db.ListTargets(r.Context())
	if err != nil {
		internalError(w, "list targets", err)
		return
	}
	out := make([]targetJSON, 0, len(targets))
	for _, t := range targets {
		out = append(out, targetJSON{
			ID: t.ID.String(), Kind: t.Kind, Name: t.Name, Address: t.Address,
			Port: t.Port, URL: t.URL, ProbeCount: t.ProbeCount, CreatedAt: t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// targetAPIFields makes shared target validation errors name the JSON fields.
var targetAPIFields = targetadmin.FieldNames{Name: "name", Address: "address", URL: "url", Port: "port"}

type targetRequest struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int32  `json:"port"`
	URL     string `json:"url"`
}

func (a *api) handleTargetPost(w http.ResponseWriter, r *http.Request) {
	var in targetRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	problems := targetadmin.Validate(in.Name, in.Address, in.URL, int64(in.Port), targetAPIFields)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	id, err := a.db.UpsertExternalTarget(r.Context(), in.Name, in.Address, in.Port, in.URL)
	if err != nil {
		writeStoreError(w, "upsert target", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

func (a *api) handleTargetDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.db.DeleteTarget(r.Context(), r.PathValue("name")); err != nil {
		writeStoreError(w, "delete target", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// --- meshes ---

type meshJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Sites      []string `json:"sites"`
	ProbeCount int64    `json:"probe_count"`
}

func (a *api) handleMeshesGet(w http.ResponseWriter, r *http.Request) {
	meshes, err := a.db.ListMeshGroups(r.Context())
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
		out = append(out, meshJSON{ID: m.ID.String(), Name: m.Name, Sites: sites, ProbeCount: m.ProbeCount})
	}
	writeJSON(w, http.StatusOK, map[string]any{"meshes": out})
}

func (a *api) handleMeshPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := a.db.UpsertMeshGroup(r.Context(), in.Name)
	if err != nil {
		writeStoreError(w, "upsert mesh", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id.String()})
}

func (a *api) handleMeshDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := a.db.DeleteMeshGroup(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, "delete mesh", err)
		return
	}
	// The FK cascade is deliberate but never silent: the response carries
	// how many probe templates went with the mesh.
	writeJSON(w, http.StatusOK, map[string]int64{"probes_deleted": deleted})
}

func (a *api) handleMeshMemberPost(w http.ResponseWriter, r *http.Request) {
	if err := a.db.AddMeshMember(r.Context(), r.PathValue("name"), r.PathValue("site")); err != nil {
		writeStoreError(w, "add mesh member", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (a *api) handleMeshMemberDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.db.RemoveMeshMember(r.Context(), r.PathValue("name"), r.PathValue("site")); err != nil {
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
		ID: p.ID.String(), Site: p.Site, Target: p.Target, Mesh: p.Mesh,
		Type:       probeadmin.TypeName(p.ProbeType),
		IntervalMS: p.Interval.Milliseconds(), TimeoutMS: p.Timeout.Milliseconds(),
		TrainCount: p.TrainCount, TrainSpacingMS: p.TrainSpacing.Milliseconds(),
		Params: params, Enabled: p.Enabled,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, UpdatedBy: p.UpdatedBy,
	}
}

func (a *api) handleProbesGet(w http.ResponseWriter, r *http.Request) {
	probes, err := a.db.ListProbeConfigs(r.Context())
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

type probeRequest struct {
	Site           string            `json:"site"`
	Target         string            `json:"target"`
	Mesh           string            `json:"mesh"`
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
	return store.ProbeSettings{
		ProbeType:    int16(probeType),
		Interval:     time.Duration(in.IntervalMS) * time.Millisecond,
		Timeout:      time.Duration(in.TimeoutMS) * time.Millisecond,
		TrainCount:   in.TrainCount,
		TrainSpacing: time.Duration(in.TrainSpacingMS) * time.Millisecond,
		Params:       in.Params,
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
	if problems := validateProbeRequest(in, probeType, meshMode); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	enabled := in.Enabled == nil || *in.Enabled
	updatedBy := sessionFrom(r.Context()).Username
	var id uuid.UUID
	if meshMode {
		id, err = a.db.AddMeshProbe(r.Context(), in.Mesh, in.settings(probeType), enabled, updatedBy)
	} else {
		id, err = a.db.AddDirectProbe(r.Context(), in.Site, in.Target, in.settings(probeType), enabled, updatedBy)
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
		if warnings := probeadmin.Warnings(probeType, meshMode, time.Duration(in.IntervalMS)*time.Millisecond, in.Params); len(warnings) > 0 {
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
		out.Warnings = probeadmin.Warnings(probeType, current.Mesh != "", time.Duration(in.IntervalMS)*time.Millisecond, in.Params)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) handleProbeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "probe id must be a UUID")
		return
	}
	if err := a.db.DeleteProbeConfig(r.Context(), id); err != nil {
		writeStoreError(w, "delete probe", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
