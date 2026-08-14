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
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by"`
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
// settings handlers return the same complete shape.
func (a *api) settingsWithOverrides(r *http.Request, ts *store.ThresholdSettings) (settingsResponse, error) {
	overrides, err := a.db.ListPathThresholds(r.Context())
	if err != nil {
		return settingsResponse{}, err
	}
	resp := toSettingsResponse(ts)
	resp.Overrides = make([]overrideJSON, 0, len(overrides))
	for i := range overrides {
		resp.Overrides = append(resp.Overrides, toOverrideJSON(&overrides[i]))
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
	// A global change may invert a PARTIAL override's effective warn/crit
	// pair (validateOverride only guards the override's own write). The
	// evaluator checks crit before warn, so an inverted pair degrades
	// gracefully (the warn tier becomes unreachable) — the write goes
	// through, and the admin is warned instead of blocked.
	for _, o := range resp.Overrides {
		eff := effectiveThresholds(resp.Thresholds, o.pathThresholdFields)
		if eff.LatencyCritUS <= eff.LatencyWarnUS {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"override for %s and %s: effective latency_warn_us (%d) is no longer below latency_crit_us (%d); adjust or delete the override",
				o.A, o.B, eff.LatencyWarnUS, eff.LatencyCritUS))
		}
		if eff.LossCritPct <= eff.LossWarnPct {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"override for %s and %s: effective loss_warn_pct (%g) is no longer below loss_crit_pct (%g); adjust or delete the override",
				o.A, o.B, eff.LossWarnPct, eff.LossCritPct))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
