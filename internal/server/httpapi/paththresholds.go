package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// Per-site-pair threshold overrides. One row covers both directions of the
// unordered pair; each field is independently optional and inherits the
// global dashboard_settings value when null. Reads ride on GET /settings
// (settingsResponse.Overrides); only the writes live here.

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
	pathThresholdFields
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func toOverrideJSON(o *store.PathThresholdOverride) overrideJSON {
	return overrideJSON{
		A: o.A, B: o.B,
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

// effectiveThresholds merges an override over the global row per field —
// the single server-side definition of inheritance; the SPA resolver
// mirrors it.
func effectiveThresholds(global thresholdsJSON, o pathThresholdFields) thresholdsJSON {
	eff := global
	if o.LatencyWarnUS != nil {
		eff.LatencyWarnUS = *o.LatencyWarnUS
	}
	if o.LatencyCritUS != nil {
		eff.LatencyCritUS = *o.LatencyCritUS
	}
	if o.LossWarnPct != nil {
		eff.LossWarnPct = *o.LossWarnPct
	}
	if o.LossCritPct != nil {
		eff.LossCritPct = *o.LossCritPct
	}
	return eff
}

// validateOverride names every problem, like validateThresholds. Set fields
// are checked on their own, then cross-field rules run on the EFFECTIVE
// tuple (override merged over global) — an override that inverts warn/crit
// against an inherited value would make the severity bands nonsense, and
// the message must say which side came from the global settings.
func validateOverride(o pathThresholdFields, global thresholdsJSON) error {
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
				return ", inherited from global settings"
			}
			return ""
		}
		inheritedF := func(set *float64) string {
			if set == nil {
				return ", inherited from global settings"
			}
			return ""
		}
		eff := effectiveThresholds(global, o)
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
}

func pathThresholdSites(w http.ResponseWriter, r *http.Request) (siteA, siteB string, ok bool) {
	siteA, siteB = r.PathValue("a"), r.PathValue("b")
	if siteA == siteB {
		writeError(w, http.StatusBadRequest, "a site cannot pair with itself")
		return "", "", false
	}
	return siteA, siteB, true
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
	global, err := a.db.GetSettings(r.Context())
	if err != nil {
		internalError(w, "get settings", err)
		return
	}
	globalJSON := toSettingsResponse(global).Thresholds
	if err := validateOverride(in, globalJSON); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := sessionFrom(r.Context())
	out, err := a.db.UpsertPathThreshold(r.Context(), siteA, siteB, store.PathThresholdOverride{
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
	writeJSON(w, http.StatusOK, pathThresholdWriteResponse{
		overrideJSON: toOverrideJSON(out),
		Effective:    effectiveThresholds(globalJSON, in),
	})
}

func (a *api) handlePathThresholdDelete(w http.ResponseWriter, r *http.Request) {
	siteA, siteB, ok := pathThresholdSites(w, r)
	if !ok {
		return
	}
	if err := a.db.DeletePathThreshold(r.Context(), siteA, siteB); err != nil {
		writeStoreError(w, "delete path threshold", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
