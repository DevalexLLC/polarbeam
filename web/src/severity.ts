// Map severity: probe status folded with the shared warn/crit thresholds.
// Status comes first — thresholds only refine links that are demonstrably
// working; a down or silent pair must never look healthy because its last
// numbers were good.
import type { MatrixCell, ThresholdSettings } from './types'

export type Severity = 'ok' | 'warn' | 'crit' | 'stale' | 'down'

// Rank order is a judgment call, documented so it isn't "fixed" casually:
// stale sits above warn because "no data" must not read calmer than a link
// we can still see missing its thresholds, but below crit/down because those
// are confirmed-bad. The dotted rendering keeps stale honestly "unknown".
export const SEVERITY_RANK: Record<Severity, number> = {
  ok: 0,
  warn: 1,
  stale: 2,
  crit: 3,
  down: 4,
}

// Threshold tiers use the same public health vocabulary. `crit` remains an
// internal intensity tier, but it is still degraded connectivity rather than
// a competing health state.
export const SEVERITY_LABEL: Record<Severity, string> = {
  ok: 'Healthy',
  warn: 'Degraded',
  crit: 'Degraded',
  stale: 'Stale',
  down: 'Down',
}

export function worst(a: Severity, b: Severity): Severity {
  return SEVERITY_RANK[a] >= SEVERITY_RANK[b] ? a : b
}

// directionSeverity grades one direction's matrix cell. A missing cell
// (configured pair with no data at all) grades stale. Null thresholds
// (settings not yet loaded) grade on status alone.
export function directionSeverity(cell: MatrixCell | undefined, t: ThresholdSettings | null): Severity {
  if (!cell || cell.status === 'stale') return 'stale'
  if (cell.status === 'down') return 'down'
  // Degraded (some checks failing) floors at warn; thresholds may raise it.
  let sev: Severity = cell.status === 'degraded' ? 'warn' : 'ok'
  if (t) {
    if (cell.latency_us != null) {
      if (cell.latency_us >= t.latency_crit_us) sev = worst(sev, 'crit')
      else if (cell.latency_us >= t.latency_warn_us) sev = worst(sev, 'warn')
    }
    // Zero loss is never unhealthy: loss_warn_pct may legitimately be 0
    // ("warn on any loss"), and 0 >= 0 must not flag a lossless link.
    if (cell.loss_pct != null && cell.loss_pct > 0) {
      if (cell.loss_pct >= t.loss_crit_pct) sev = worst(sev, 'crit')
      else if (cell.loss_pct >= t.loss_warn_pct) sev = worst(sev, 'warn')
    }
  }
  return sev
}

// pairSeverity folds both directions of an unordered pair: the map draws one
// line per pair, and one dead direction dominates. Only configured
// directions count — foldMatrix emits an explicit stale cell for a
// configured-but-silent direction, so an ABSENT cell means "not probed that
// way", and a healthy one-way probe must not drag its pair to stale.
export function pairSeverity(
  ab: MatrixCell | undefined,
  ba: MatrixCell | undefined,
  t: ThresholdSettings | null,
): Severity {
  if (ab && ba) return worst(directionSeverity(ab, t), directionSeverity(ba, t))
  const only = ab ?? ba
  return only ? directionSeverity(only, t) : 'stale'
}
