// Package thresholds resolves and applies the dashboard's latency/loss
// severity thresholds on the server. The per-field merge and the grading
// comparisons deliberately mirror the SPA (web/src/severity.ts
// buildThresholdResolver + directionSeverity) and the httpapi validation
// merge (effectiveThresholds) — the three must agree or the live map and
// the incident history would disagree about the same measurements.
package thresholds

import (
	"fmt"
	"strconv"
)

// T is one direction's effective thresholds after any override merge.
type T struct {
	LatencyWarnUS int64
	LatencyCritUS int64
	LossWarnPct   float64
	LossCritPct   float64
}

// Override is one path_thresholds row's metric fields: nil inherits the
// global value for that field.
type Override struct {
	LatencyWarnUS *int64
	LatencyCritUS *int64
	LossWarnPct   *float64
	LossCritPct   *float64
}

// Effective merges an override over the global thresholds per field.
func Effective(global T, o Override) T {
	out := global
	if o.LatencyWarnUS != nil {
		out.LatencyWarnUS = *o.LatencyWarnUS
	}
	if o.LatencyCritUS != nil {
		out.LatencyCritUS = *o.LatencyCritUS
	}
	if o.LossWarnPct != nil {
		out.LossWarnPct = *o.LossWarnPct
	}
	if o.LossCritPct != nil {
		out.LossCritPct = *o.LossCritPct
	}
	return out
}

// GradeCrit reports whether a successful measurement breaches the critical
// tier, with a display string naming what breached. Nil metrics are "not
// measured" and never breach. Zero loss is never unhealthy — loss_crit_pct
// may not be 0 by CHECK, but the guard keeps the rule identical to the SPA's.
//
// The detail deliberately contains the threshold (stable per configuration)
// and NOT the measured value: the Incidents page correlates events by their
// error text, and a per-event measurement would split every incident into a
// group of one.
func GradeCrit(t T, latencyUS *int64, lossPct *float64) (bool, string) {
	latencyBreach := latencyUS != nil && *latencyUS >= t.LatencyCritUS
	lossBreach := lossPct != nil && *lossPct > 0 && *lossPct >= t.LossCritPct
	switch {
	case latencyBreach && lossBreach:
		return true, fmt.Sprintf("latency and loss at or above critical thresholds (%s, %s%%)",
			formatUS(t.LatencyCritUS), formatPct(t.LossCritPct))
	case latencyBreach:
		return true, fmt.Sprintf("latency at or above critical threshold (%s)", formatUS(t.LatencyCritUS))
	case lossBreach:
		return true, fmt.Sprintf("loss at or above critical threshold (%s%%)", formatPct(t.LossCritPct))
	default:
		return false, ""
	}
}

// formatUS renders a microsecond threshold as milliseconds, trimming
// trailing zeros ("100ms", "0.5ms").
func formatUS(us int64) string {
	return strconv.FormatFloat(float64(us)/1000, 'f', -1, 64) + "ms"
}

func formatPct(pct float64) string {
	return strconv.FormatFloat(pct, 'f', -1, 64)
}
