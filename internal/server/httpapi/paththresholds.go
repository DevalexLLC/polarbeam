package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/thresholds"
)

// Per-site-pair threshold overrides. One row covers both directions of the
// unordered pair on one plane; each field is independently optional and
// inherits the next layer out when null. Reads ride on GET /settings
// (settingsResponse.Overrides); only the writes live here.
//
// Resolution is four layers deep, per metric, most specific first:
//
//	pair+network → pair (all planes) → network default → global default
//
// and this file resolves it through internal/server/thresholds.Effective —
// the same function ingest grades with — so the dashboard and the outage
// detector can never disagree about a measurement.

// pathThresholdFields is the nullable override tuple — the PUT body, and
// the metric half of every override in responses. Wire units match the
// global thresholds (µs / percent).
type pathThresholdFields struct {
	LatencyWarnUS *int64   `json:"latency_warn_us"`
	LatencyCritUS *int64   `json:"latency_crit_us"`
	LossWarnPct   *float64 `json:"loss_warn_pct"`
	LossCritPct   *float64 `json:"loss_crit_pct"`
}

type overrideJSON struct {
	A string `json:"a"`
	B string `json:"b"`
	// Network is the plane this row applies to, "" for the all-planes row.
	// The SPA resolver keys on it, so it is always present.
	Network string `json:"network"`
	pathThresholdFields
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func toOverrideJSON(o *store.PathThresholdOverride) overrideJSON {
	return overrideJSON{
		A: o.A, B: o.B, Network: o.Network,
		pathThresholdFields: pathThresholdFields{
			LatencyWarnUS: o.LatencyWarnUS,
			LatencyCritUS: o.LatencyCritUS,
			LossWarnPct:   o.LossWarnPct,
			LossCritPct:   o.LossCritPct,
		},
		UpdatedAt: o.UpdatedAt,
		UpdatedBy: o.UpdatedBy,
	}
}

// networkThresholdJSON is one network_thresholds row: the per-plane overlay
// between the global defaults and the pair overrides. Same nullable tuple,
// one layer out.
type networkThresholdJSON struct {
	Network string `json:"network"`
	pathThresholdFields
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func toNetworkThresholdJSON(t *store.NetworkThreshold) networkThresholdJSON {
	return networkThresholdJSON{
		Network: t.Network,
		pathThresholdFields: pathThresholdFields{
			LatencyWarnUS: t.LatencyWarnUS,
			LatencyCritUS: t.LatencyCritUS,
			LossWarnPct:   t.LossWarnPct,
			LossCritPct:   t.LossCritPct,
		},
		UpdatedAt: t.UpdatedAt,
		UpdatedBy: t.UpdatedBy,
	}
}

// layer converts the nullable tuple into a merge layer.
func (o pathThresholdFields) layer() thresholds.Override {
	return thresholds.Override{
		LatencyWarnUS: o.LatencyWarnUS,
		LatencyCritUS: o.LatencyCritUS,
		LossWarnPct:   o.LossWarnPct,
		LossCritPct:   o.LossCritPct,
	}
}

func (t thresholdsJSON) base() thresholds.T {
	return thresholds.T{
		LatencyWarnUS: t.LatencyWarnUS,
		LatencyCritUS: t.LatencyCritUS,
		LossWarnPct:   t.LossWarnPct,
		LossCritPct:   t.LossCritPct,
	}
}

func fromBase(t thresholds.T) thresholdsJSON {
	return thresholdsJSON{
		LatencyWarnUS: t.LatencyWarnUS,
		LatencyCritUS: t.LatencyCritUS,
		LossWarnPct:   t.LossWarnPct,
		LossCritPct:   t.LossCritPct,
	}
}

// effectiveThresholds merges override layers over the global row per field,
// most specific first. It delegates to internal/server/thresholds.Effective
// rather than repeating the fold: ingest grades with that function, so a
// second copy here could only ever drift away from what the outage detector
// actually does. The SPA resolver mirrors the same order, and
// testdata/threshold-merge.json is the shared case table that keeps it
// honest.
func effectiveThresholds(global thresholdsJSON, layers ...pathThresholdFields) thresholdsJSON {
	ls := make([]thresholds.Override, 0, len(layers))
	for _, l := range layers {
		ls = append(ls, l.layer())
	}
	return fromBase(thresholds.Effective(global.base(), ls...))
}

// validateOverride names every problem, like validateThresholds. Set fields
// are checked on their own, then cross-field rules run on the EFFECTIVE
// tuple — an override that inverts warn/crit against an inherited value
// would make the severity bands nonsense, and the message must say which
// side was inherited and from where.
//
// base is what this row actually inherits: the merge of every LESS specific
// layer that applies to it. For a plane-qualified pair row that is the
// all-planes row over the plane's default over the global row, not the
// global row alone — validating against the global alone would accept a row
// whose real effective tuple is inverted, and would name the wrong source
// in the message. baseDesc names those layers for the operator.
func validateOverride(o pathThresholdFields, base thresholdsJSON, baseDesc string) error {
	var problems []string
	if o.LatencyWarnUS == nil && o.LatencyCritUS == nil && o.LossWarnPct == nil && o.LossCritPct == nil {
		return errors.New("at least one field must be set; clear the override with DELETE instead")
	}
	if o.LatencyWarnUS != nil && *o.LatencyWarnUS <= 0 {
		problems = append(problems, "latency_warn_us must be positive")
	}
	if o.LatencyCritUS != nil {
		if *o.LatencyCritUS <= 0 {
			problems = append(problems, "latency_crit_us must be positive")
		}
		if *o.LatencyCritUS > maxLatencyCritUS {
			problems = append(problems, "latency_crit_us must be at most 60000000 (60s)")
		}
	}
	if o.LossWarnPct != nil && (*o.LossWarnPct < 0 || *o.LossWarnPct > 100) {
		problems = append(problems, "loss_warn_pct must be between 0 and 100")
	}
	if o.LossCritPct != nil && (*o.LossCritPct <= 0 || *o.LossCritPct > 100) {
		problems = append(problems, "loss_crit_pct must be positive and at most 100")
	}

	// Cross-field rules on the effective values, but only when the set
	// fields themselves are sane — a negative warn already reported above
	// would otherwise produce a misleading second message.
	if len(problems) == 0 {
		inherited := func(set *int64) string {
			if set == nil {
				return ", inherited from " + baseDesc
			}
			return ""
		}
		inheritedF := func(set *float64) string {
			if set == nil {
				return ", inherited from " + baseDesc
			}
			return ""
		}
		eff := effectiveThresholds(base, o)
		if eff.LatencyCritUS <= eff.LatencyWarnUS {
			problems = append(problems, fmt.Sprintf(
				"latency_crit_us (%d%s) must be greater than latency_warn_us (%d%s)",
				eff.LatencyCritUS, inherited(o.LatencyCritUS),
				eff.LatencyWarnUS, inherited(o.LatencyWarnUS)))
		}
		if eff.LossCritPct <= eff.LossWarnPct {
			problems = append(problems, fmt.Sprintf(
				"loss_crit_pct (%g%s) must be greater than loss_warn_pct (%g%s)",
				eff.LossCritPct, inheritedF(o.LossCritPct),
				eff.LossWarnPct, inheritedF(o.LossWarnPct)))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// pathThresholdWriteResponse echoes the stored override plus the effective
// tuple so the SPA can show the merged result without re-deriving it.
type pathThresholdWriteResponse struct {
	overrideJSON
	Effective thresholdsJSON `json:"effective"`
	// Advisory only, same channel as the probe and OIDC writes: the write
	// succeeded, but something about it will not do what it looks like.
	Warnings []string `json:"warnings,omitempty"`
}

func pathThresholdSites(w http.ResponseWriter, r *http.Request) (siteA, siteB string, ok bool) {
	siteA, siteB = r.PathValue("a"), r.PathValue("b")
	if siteA == siteB {
		writeError(w, http.StatusBadRequest, "a site cannot pair with itself")
		return "", "", false
	}
	return siteA, siteB, true
}

// pathThresholdPlane resolves the ?network= plane a threshold write
// addresses, answering the request itself on refusal.
//
//	global admin, no ?network=  → nil, the all-planes row (pre-tenancy
//	                              behavior, unchanged)
//	?network=<name>             → that plane, which must be in scope
//	scoped caller, no ?network= → refused: the all-planes row decides every
//	                              tenant's severities, so it is the
//	                              operator's alone
func (a *api) pathThresholdPlane(w http.ResponseWriter, r *http.Request) (*uuid.UUID, string, bool) {
	name := r.URL.Query().Get("network")
	if name == "" {
		if callerIsScoped(r.Context()) {
			writeError(w, http.StatusBadRequest,
				"network is required: the all-planes threshold override applies to every tenant and is reserved to a global admin")
			return nil, "", false
		}
		return nil, "", true
	}
	id, ok := a.requireNetworkScopeName(w, r, name)
	if !ok {
		return nil, "", false
	}
	return &id, name, true
}

// pathThresholdBase resolves what a row on this pair and plane INHERITS —
// every less specific layer, merged in precedence order — plus a phrase
// naming those layers for validateOverride's messages.
//
// A plane-qualified row inherits the pair's all-planes row, then the plane's
// default, then the global row. The all-planes row itself inherits only the
// global row: it applies to every plane at once, so no single network
// default can sit beneath it.
func (a *api) pathThresholdBase(r *http.Request, global thresholdsJSON, siteA, siteB, plane string) (thresholdsJSON, string, error) {
	if plane == "" {
		return global, "global settings", nil
	}
	scope := scopeIDs(r.Context())
	overrides, err := a.db.ListPathThresholds(r.Context(), scope)
	if err != nil {
		return thresholdsJSON{}, "", err
	}
	var layers []pathThresholdFields
	var named []string
	for i := range overrides {
		o := &overrides[i]
		if o.Network == "" && samePair(o.A, o.B, siteA, siteB) {
			layers = append(layers, toOverrideJSON(o).pathThresholdFields)
			named = append(named, "the all-planes override")
			break
		}
	}
	nets, err := a.db.ListNetworkThresholds(r.Context(), scope)
	if err != nil {
		return thresholdsJSON{}, "", err
	}
	for i := range nets {
		if nets[i].Network == plane {
			layers = append(layers, toNetworkThresholdJSON(&nets[i]).pathThresholdFields)
			named = append(named, fmt.Sprintf("the %s network default", plane))
			break
		}
	}
	named = append(named, "the global settings")
	return effectiveThresholds(global, layers...), strings.Join(named, ", "), nil
}

// samePair compares two unordered site pairs by name.
func samePair(a1, b1, a2, b2 string) bool {
	return (a1 == a2 && b1 == b2) || (a1 == b2 && b1 == a2)
}

// pathThresholdSitesVisible proves a scoped caller can actually SEE both
// sites of the pair, answering the request itself when it cannot, and
// returns the planes present at both (nil when no check ran).
//
// Enforced only for scoped callers, and deliberately so. pairEndpoints
// resolves a site through its endpoints, so a site with no agents yet
// resolves to nothing — running it for everyone would stop an operator
// configuring thresholds BEFORE enrollment, a normal order of operations
// that works today. A scoped caller, though, must be able to see both
// sites: GET /sites already hides sites it has no presence at, and without
// this the threshold routes would answer "unknown site" for a name that does
// not exist and something else for one that does, turning either verb into a
// site-name oracle.
//
// BOTH verbs call this. PUT alone would leave DELETE as that oracle, and
// would also let a scoped caller delete a row for a pair it cannot see.
func (a *api) pathThresholdSitesVisible(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	if !callerIsScoped(r.Context()) {
		return nil, true
	}
	_, _, planes, ok := a.pairEndpoints(w, r)
	return planes, ok
}

func (a *api) handlePathThresholdPut(w http.ResponseWriter, r *http.Request) {
	siteA, siteB, ok := pathThresholdSites(w, r)
	if !ok {
		return
	}
	var in pathThresholdFields
	if !decodeStrict(w, r, &in) {
		return
	}
	networkID, plane, ok := a.pathThresholdPlane(w, r)
	if !ok {
		return
	}
	planes, ok := a.pathThresholdSitesVisible(w, r)
	if !ok {
		return
	}
	global, err := a.db.GetSettings(r.Context())
	if err != nil {
		internalError(w, "get settings", err)
		return
	}
	globalJSON := toSettingsResponse(global).Thresholds
	base, baseDesc, err := a.pathThresholdBase(r, globalJSON, siteA, siteB, plane)
	if err != nil {
		internalError(w, "resolve threshold layers", err)
		return
	}
	if err := validateOverride(in, base, baseDesc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := sessionFrom(r.Context())
	out, err := a.db.UpsertPathThreshold(r.Context(), siteA, siteB, networkID, store.PathThresholdOverride{
		LatencyWarnUS: in.LatencyWarnUS,
		LatencyCritUS: in.LatencyCritUS,
		LossWarnPct:   in.LossWarnPct,
		LossCritPct:   in.LossCritPct,
		UpdatedBy:     s.Username,
	})
	if err != nil {
		writeStoreError(w, "upsert path threshold", err)
		return
	}
	resp := pathThresholdWriteResponse{
		overrideJSON: toOverrideJSON(out),
		Effective:    effectiveThresholds(base, in),
	}
	// Advisory, not a refusal, and only where planes were computed at all
	// (see above): a row for a plane neither site currently carries is dead
	// config, but blocking it would stop a tenant configuring thresholds
	// BEFORE its agents enroll. Isolation does not rest on this check; it
	// rests on pairEndpoints' site scoping and requireNetworkScopeName.
	if plane != "" && planes != nil && !slices.Contains(planes, plane) {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"no agents at both %s and %s are on network %q yet, so this override has no effect until they enroll",
			siteA, siteB, plane))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) handlePathThresholdDelete(w http.ResponseWriter, r *http.Request) {
	siteA, siteB, ok := pathThresholdSites(w, r)
	if !ok {
		return
	}
	networkID, _, ok := a.pathThresholdPlane(w, r)
	if !ok {
		return
	}
	if _, ok := a.pathThresholdSitesVisible(w, r); !ok {
		return
	}
	// Deleting one plane's row never touches another plane's, nor the
	// all-planes row; the store keys on network_id IS NOT DISTINCT FROM.
	if err := a.db.DeletePathThreshold(r.Context(), siteA, siteB, networkID); err != nil {
		writeStoreError(w, "delete path threshold", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
