package httpapi

import (
	"encoding/json"
	"errors"
	"io"
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
	UpdatedAt  time.Time      `json:"updated_at"`
	UpdatedBy  string         `json:"updated_by"`
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

func (a *api) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	ts, err := a.db.GetSettings(r.Context())
	if err != nil {
		internalError(w, "get settings", err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsResponse(ts))
}

func (a *api) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in thresholdsJSON
	dec := json.NewDecoder(r.Body)
	// An unknown key is a client bug (or a client newer than the server) —
	// reject it rather than silently dropping the field.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings body: "+err.Error())
		return
	}
	// Exactly one JSON value: trailing data after the object is a malformed
	// request, not something to silently ignore on a mutating endpoint.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid settings body: trailing data after JSON object")
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
	writeJSON(w, http.StatusOK, toSettingsResponse(out))
}
