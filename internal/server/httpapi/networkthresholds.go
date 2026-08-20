package httpapi

// Per-network threshold defaults: /api/v1/settings/network-thresholds/{network}.
//
// This is the layer between the global dashboard_settings singleton and the
// per-site-pair overrides. It exists because a tenant admin cannot write the
// global row — without it, a plane whose idea of "normal" differs from the
// operator's could only say so pair by pair.
//
// Reads ride on GET /settings (settingsResponse.NetworkDefaults), scoped
// there; only the writes live here.

import (
	"fmt"
	"net/http"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

type networkThresholdWriteResponse struct {
	networkThresholdJSON
	// Effective is this plane's defaults merged over the global row — what
	// a pair with no override of its own will actually grade against.
	Effective thresholdsJSON `json:"effective"`
	// Advisory only, same channel as PUT /settings: pair rows on this plane
	// whose EFFECTIVE tuple the new defaults just inverted.
	Warnings []string `json:"warnings,omitempty"`
}

// dependentPairWarnings names the plane's pair overrides that the new
// defaults leave inverted. A partial pair row inherits from this layer, so
// a default that is valid on its own can still push a dependent row's
// effective warn above its crit — the case validateOverride cannot see,
// because it only ever validates the row being written.
//
// Advisory, never blocking, exactly as PUT /settings treats the same class
// of change: the evaluator checks crit before warn, so an inverted pair
// degrades gracefully (the warn tier becomes unreachable) and the operator
// is told which rows to revisit rather than being stopped.
func dependentPairWarnings(overrides []overrideJSON, network string, base thresholdsJSON) []string {
	var out []string
	for _, o := range overrides {
		if o.Network != network {
			continue
		}
		eff := effectiveThresholds(base, o.pathThresholdFields)
		if eff.LatencyCritUS <= eff.LatencyWarnUS {
			out = append(out, fmt.Sprintf(
				"override for %s and %s on network %s: effective latency_warn_us (%d) is no longer below latency_crit_us (%d); adjust or delete the override",
				o.A, o.B, network, eff.LatencyWarnUS, eff.LatencyCritUS))
		}
		if eff.LossCritPct <= eff.LossWarnPct {
			out = append(out, fmt.Sprintf(
				"override for %s and %s on network %s: effective loss_warn_pct (%g) is no longer below loss_crit_pct (%g); adjust or delete the override",
				o.A, o.B, network, eff.LossWarnPct, eff.LossCritPct))
		}
	}
	return out
}

func (a *api) handleNetworkThresholdPut(w http.ResponseWriter, r *http.Request) {
	network := r.PathValue("network")
	var in pathThresholdFields
	if !decodeStrict(w, r, &in) {
		return
	}
	global, err := a.db.GetSettings(r.Context())
	if err != nil {
		internalError(w, "get settings", err)
		return
	}
	globalJSON := toSettingsResponse(global).Thresholds
	// A network default sits directly on the global row — nothing else is
	// less specific — so the global row IS its inherited base.
	if err := validateOverride(in, globalJSON, "global settings"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := sessionFrom(r.Context())
	out, err := a.db.UpsertNetworkThreshold(r.Context(), network, store.NetworkThreshold{
		LatencyWarnUS: in.LatencyWarnUS,
		LatencyCritUS: in.LatencyCritUS,
		LossWarnPct:   in.LossWarnPct,
		LossCritPct:   in.LossCritPct,
		UpdatedBy:     s.Username,
	}, scopeIDs(r.Context()))
	if err != nil {
		// The store answers ErrNotFound for a plane outside the caller's
		// scope with the same wording an unknown name gets, so a tenant
		// cannot enumerate other planes by writing to them.
		writeStoreError(w, "upsert network threshold", err)
		return
	}
	resp := networkThresholdWriteResponse{
		networkThresholdJSON: toNetworkThresholdJSON(out),
		Effective:            effectiveThresholds(globalJSON, in),
	}
	// The pair rows on this plane inherit what just changed; tell the caller
	// which of them the new defaults left inconsistent.
	if overrides, err := a.db.ListPathThresholds(r.Context(), scopeIDs(r.Context())); err == nil {
		rows := make([]overrideJSON, 0, len(overrides))
		for i := range overrides {
			rows = append(rows, toOverrideJSON(&overrides[i]))
		}
		resp.Warnings = dependentPairWarnings(rows, network, resp.Effective)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) handleNetworkThresholdDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.db.DeleteNetworkThreshold(r.Context(), r.PathValue("network"), scopeIDs(r.Context())); err != nil {
		writeStoreError(w, "delete network threshold", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
