// Map severity: probe status folded with the shared warn/crit thresholds.
// Status comes first — thresholds only refine links that are demonstrably
// working; a down or silent pair must never look healthy because its last
// numbers were good.
import type {
  MatrixCell,
  NetworkThreshold,
  PathThresholdOverride,
  SettingsResponse,
  ThresholdOverrideFields,
  ThresholdSettings,
} from './types'

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
  stale: 'No data',
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

// ThresholdResolver answers "which thresholds grade this direction" — the
// global settings with every applicable override layer merged over them.
// Pair overrides are keyed on the unordered pair, so both directions of a
// link resolve the same values (they still grade independently). `network`
// is the plane being graded: matrix sub-cells, pair series, and target
// sources all know theirs. Omit it (or pass '') where no plane applies and
// only the all-planes and global layers are used.
//
// A null `dst` means "no site pair" — an external target, which ingest
// grades on the plane default over the global row and nothing else. Null
// RESULT means settings are still loading, matching the old `thresholds`
// prop contract.
export type ThresholdResolver = (src: string, dst: string | null, network?: string) => ThresholdSettings | null

function pairKey(a: string, b: string, network: string): string {
  const [x, y] = a < b ? [a, b] : [b, a]
  return x + '\u0000' + y + '\u0000' + network
}

// mergeLayers folds override layers over the global row per field, the
// FIRST layer that sets a metric winning — layers are passed most specific
// first. This mirrors internal/server/thresholds.Effective exactly, and
// testdata/threshold-merge.json is the shared case table both are checked
// against (web/tools/check-threshold-merge.ts runs this side). If the two
// ever disagree, the live map and the incident history disagree about the
// same measurement.
//
// Note `??`, not `||`: 0 is a legitimate loss_warn_pct and must not fall
// through to the next layer.
export function mergeLayers(
  global: ThresholdSettings,
  ...layers: (ThresholdOverrideFields | undefined)[]
): ThresholdSettings {
  const pick = <K extends keyof ThresholdOverrideFields>(key: K): number => {
    for (const l of layers) {
      const v = l?.[key]
      if (v != null) return v
    }
    return global[key]
  }
  return {
    latency_warn_us: pick('latency_warn_us'),
    latency_crit_us: pick('latency_crit_us'),
    loss_warn_pct: pick('loss_warn_pct'),
    loss_crit_pct: pick('loss_crit_pct'),
  }
}

// cellNetwork names the plane a matrix cell represents, or '' when it folds
// several. A cell carries one sub-cell per (src, dst, network); single-plane
// installs and any view under a network filter therefore have exactly one,
// and its plane-qualified thresholds apply.
export function cellNetwork(cell: Pick<MatrixCell, 'networks'>): string {
  return cell.networks?.length === 1 ? cell.networks[0].network : ''
}

// cellSeverity grades a matrix cell the way ingest does: PER PLANE, then
// folded to the worst.
//
// Grading a multi-plane cell as one unit with no network would drop every
// network default and plane-qualified override, so a sub-cell could breach
// the very threshold that opened an incident while the aggregate rendered
// healthy — the dashboard contradicting the Incidents page about the same
// measurement. Each sub-cell carries its own numbers (latency_us, loss_pct,
// status) as well as its plane, so the fold is over real per-plane
// severities rather than a re-grade of already-folded values.
//
// Single-plane cells take the fast path and are identical to grading the
// cell directly.
export function cellSeverity(cell: MatrixCell, resolve: ThresholdResolver): Severity {
  const subs = cell.networks
  if (!subs || subs.length <= 1) {
    return directionSeverity(cell, resolve(cell.src, cell.dst, cellNetwork(cell)))
  }
  let sev: Severity = 'ok'
  for (const sub of subs) {
    sev = worst(sev, directionSeverity({ ...cell, ...sub }, resolve(cell.src, cell.dst, sub.network)))
  }
  return sev
}

// buildThresholdResolver merges the four layers ONCE per (pair, plane) so
// every consumer — matrix, map, overview stat, pair detail — resolves
// identical effective values:
//
//   pair+network -> pair (all planes) -> network default -> global
//
// The `loss_pct > 0` guard in directionSeverity applies unchanged to
// overridden values: a pair override of loss_warn_pct 0 still never flags a
// lossless link.
export function buildThresholdResolver(settings: SettingsResponse | null): ThresholdResolver {
  if (!settings) return () => null
  const global = settings.thresholds
  const pairLayers = new Map<string, PathThresholdOverride>()
  for (const o of settings.overrides) pairLayers.set(pairKey(o.a, o.b, o.network), o)
  const networkLayers = new Map<string, NetworkThreshold>()
  for (const d of settings.network_defaults ?? []) networkLayers.set(d.network, d)

  // Resolution is per (pair, plane) and the set of planes is small, so cache
  // the folds rather than redoing them for every cell on every render.
  const resolved = new Map<string, ThresholdSettings>()
  return (src, dst, network = '') => {
    // '\u0000' cannot occur in a site name, so this key can never collide
    // with a real pair's.
    const key = dst === null ? '\u0000external\u0000' + network : pairKey(src, dst, network)
    const hit = resolved.get(key)
    if (hit) return hit
    const eff =
      dst === null
        ? mergeLayers(global, network ? networkLayers.get(network) : undefined)
        : mergeLayers(
            global,
            network ? pairLayers.get(key) : undefined,
            pairLayers.get(pairKey(src, dst, '')),
            network ? networkLayers.get(network) : undefined,
          )
    resolved.set(key, eff)
    return eff
  }
}
