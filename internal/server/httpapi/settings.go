package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// maxLatencyCritUS bounds threshold latency at 60 s — beyond any per-run
// probe timeout, so anything larger is a unit mistake (ms vs µs), not a
// policy.
const maxLatencyCritUS = 60_000_000

type thresholdsJSON struct {
	LatencyWarnUS int64   `json:"latency_warn_us"`
	LatencyCritUS int64   `json:"latency_crit_us"`
	LossWarnPct   float64 `json:"loss_warn_pct"`
	LossCritPct   float64 `json:"loss_crit_pct"`
}

type settingsResponse struct {
	Thresholds thresholdsJSON `json:"thresholds"`
	// Per-site-pair overrides ride along so every consumer of the global
	// thresholds (matrix, map, pair detail) resolves severity from one
	// fetch. Always [] in JSON, never null.
	Overrides []overrideJSON `json:"overrides"`
	// NetworkDefaults is the per-plane layer between the global row and the
	// pair overrides. It ships in the same payload for the same reason: the
	// SPA resolver needs all four layers to reproduce what ingest graded.
	// Always [] in JSON, never null.
	NetworkDefaults []networkThresholdJSON `json:"network_defaults"`
	UpdatedAt       time.Time              `json:"updated_at"`
	UpdatedBy       string                 `json:"updated_by"`
	// Advisory only, set on PUT: a global change is never blocked by
	// partial overrides it leaves inconsistent, but the admin is told
	// which pairs need attention (same channel as probe/OIDC writes).
	Warnings []string `json:"warnings,omitempty"`
}

func toSettingsResponse(ts *store.ThresholdSettings) settingsResponse {
	return settingsResponse{
		Thresholds: thresholdsJSON{
			LatencyWarnUS: ts.LatencyWarnUS,
			LatencyCritUS: ts.LatencyCritUS,
			LossWarnPct:   ts.LossWarnPct,
			LossCritPct:   ts.LossCritPct,
		},
		UpdatedAt: ts.UpdatedAt,
		UpdatedBy: ts.UpdatedBy,
	}
}

// validateThresholds names every problem, not just the first — the SPA form
// mirrors these rules, so a hand-crafted request failing several ways gets
// the full list at once.
func validateThresholds(t thresholdsJSON) error {
	var problems []string
	if t.LatencyWarnUS <= 0 {
		problems = append(problems, "latency_warn_us must be positive")
	}
	if t.LatencyCritUS <= t.LatencyWarnUS {
		problems = append(problems, "latency_crit_us must be greater than latency_warn_us")
	}
	if t.LatencyCritUS > maxLatencyCritUS {
		problems = append(problems, "latency_crit_us must be at most 60000000 (60s)")
	}
	if t.LossWarnPct < 0 {
		problems = append(problems, "loss_warn_pct must not be negative")
	}
	if t.LossCritPct <= t.LossWarnPct {
		problems = append(problems, "loss_crit_pct must be greater than loss_warn_pct")
	}
	if t.LossCritPct > 100 {
		problems = append(problems, "loss_crit_pct must be at most 100")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// settingsWithOverrides folds the override list into the response; both
// settings handlers return the same complete shape. Scoped sessions see
// only overrides whose site pair is visible to them — the global defaults
// themselves stay readable (they decide the tenant's severities too).
func (a *api) settingsWithOverrides(r *http.Request, ts *store.ThresholdSettings) (settingsResponse, error) {
	scope := scopeIDs(r.Context())
	overrides, err := a.db.ListPathThresholds(r.Context(), scope)
	if err != nil {
		return settingsResponse{}, err
	}
	defaults, err := a.db.ListNetworkThresholds(r.Context(), scope)
	if err != nil {
		return settingsResponse{}, err
	}
	resp := toSettingsResponse(ts)
	resp.Overrides = make([]overrideJSON, 0, len(overrides))
	for i := range overrides {
		resp.Overrides = append(resp.Overrides, toOverrideJSON(&overrides[i]))
	}
	resp.NetworkDefaults = make([]networkThresholdJSON, 0, len(defaults))
	for i := range defaults {
		resp.NetworkDefaults = append(resp.NetworkDefaults, toNetworkThresholdJSON(&defaults[i]))
	}
	return resp, nil
}

func (a *api) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	ts, err := a.db.GetSettings(r.Context())
	if err != nil {
		internalError(w, "get settings", err)
		return
	}
	resp, err := a.settingsWithOverrides(r, ts)
	if err != nil {
		internalError(w, "list path thresholds", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in thresholdsJSON
	if !decodeStrict(w, r, &in) {
		return
	}
	if err := validateThresholds(in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := sessionFrom(r.Context())
	out, err := a.db.UpdateSettings(r.Context(), store.ThresholdSettings{
		LatencyWarnUS: in.LatencyWarnUS,
		LatencyCritUS: in.LatencyCritUS,
		LossWarnPct:   in.LossWarnPct,
		LossCritPct:   in.LossCritPct,
		UpdatedBy:     s.Username,
	})
	if err != nil {
		internalError(w, "update settings", err)
		return
	}
	resp, err := a.settingsWithOverrides(r, out)
	if err != nil {
		internalError(w, "list path thresholds", err)
		return
	}
	// A global change may invert a PARTIAL layer's effective warn/crit pair
	// (validateOverride only guards that layer's own write). The evaluator
	// checks crit before warn, so an inverted pair degrades gracefully (the
	// warn tier becomes unreachable) — the write goes through, and the
	// admin is warned instead of blocked.
	//
	// Every layer below the pair rows is checked, because a global change
	// can invert a network default just as easily. Pair rows are checked
	// against the global row alone rather than their full stack: this is an
	// advisory sweep over potentially many rows, and a partial layer that
	// only looks inverted here still gets named, which is the point.
	for _, d := range resp.NetworkDefaults {
		eff := effectiveThresholds(resp.Thresholds, d.pathThresholdFields)
		if eff.LatencyCritUS <= eff.LatencyWarnUS {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"network default for %s: effective latency_warn_us (%d) is no longer below latency_crit_us (%d); adjust or delete it",
				d.Network, eff.LatencyWarnUS, eff.LatencyCritUS))
		}
		if eff.LossCritPct <= eff.LossWarnPct {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"network default for %s: effective loss_warn_pct (%g) is no longer below loss_crit_pct (%g); adjust or delete it",
				d.Network, eff.LossWarnPct, eff.LossCritPct))
		}
	}
	for _, o := range resp.Overrides {
		eff := effectiveThresholds(resp.Thresholds, o.pathThresholdFields)
		where := fmt.Sprintf("%s and %s", o.A, o.B)
		if o.Network != "" {
			where += " on network " + o.Network
		}
		if eff.LatencyCritUS <= eff.LatencyWarnUS {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"override for %s: effective latency_warn_us (%d) is no longer below latency_crit_us (%d); adjust or delete the override",
				where, eff.LatencyWarnUS, eff.LatencyCritUS))
		}
		if eff.LossCritPct <= eff.LossWarnPct {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"override for %s: effective loss_warn_pct (%g) is no longer below loss_crit_pct (%g); adjust or delete the override",
				where, eff.LossWarnPct, eff.LossCritPct))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
