// Package thresholds resolves and applies the dashboard's latency/loss
// severity thresholds on the server. The per-field merge and the grading
// comparisons deliberately mirror the SPA (web/src/severity.ts
// buildThresholdResolver + directionSeverity) and the httpapi validation
// merge (effectiveThresholds) — the three must agree or the live map and
// the incident history would disagree about the same measurements.
//
// Effective is the single definition of that merge: httpapi calls straight
// into it, and testdata/threshold-merge.json holds the shared case table
// that both this package's tests and the SPA's parity check read, so the Go
// and TypeScript resolvers cannot drift silently.
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

// Override is one layer's metric fields: nil inherits whatever the next,
// less specific layer supplies for that field. A path_thresholds row and a
// network_thresholds row are both Overrides — they differ only in where
// Effective's caller places them.
type Override struct {
	LatencyWarnUS *int64
	LatencyCritUS *int64
	LossWarnPct   *float64
	LossCritPct   *float64
}

// Effective merges override layers over the global thresholds, per field
// and independently: for each metric the FIRST layer that sets it wins, and
// a metric no layer sets falls through to global. Callers pass layers in
// specificity order, most specific first:
//
//	Effective(global, pairNetwork, pairAllPlanes, networkDefault)
//
// Absent layers are simply omitted (a zero Override is also inert), so the
// pre-tenancy one-layer call Effective(global, o) still means exactly what
// it did. Order is the caller's contract — this function never reorders,
// because "most specific wins" is a statement about the schema, not
// something a merge of anonymous tuples could infer.
func Effective(global T, layers ...Override) T {
	out := global
	var haveLatWarn, haveLatCrit, haveLossWarn, haveLossCrit bool
	for _, o := range layers {
		if o.LatencyWarnUS != nil && !haveLatWarn {
			out.LatencyWarnUS, haveLatWarn = *o.LatencyWarnUS, true
		}
		if o.LatencyCritUS != nil && !haveLatCrit {
			out.LatencyCritUS, haveLatCrit = *o.LatencyCritUS, true
		}
		if o.LossWarnPct != nil && !haveLossWarn {
			out.LossWarnPct, haveLossWarn = *o.LossWarnPct, true
		}
		if o.LossCritPct != nil && !haveLossCrit {
			out.LossCritPct, haveLossCrit = *o.LossCritPct, true
		}
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
