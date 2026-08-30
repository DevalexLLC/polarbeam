import type { ParamSpec, ProbeConfig, ProbeTypesResponse } from './types'

// Prober defaults, mirrored from the server's shared validation
// (internal/server/probeadmin): a zero train budgets as 10 × 200 ms.
const DEFAULT_TRAIN_COUNT = 10
const DEFAULT_TRAIN_SPACING_MS = 200
// Path MTU prober defaults (internal/agent/probes/pathmtu.go), used only
// for the client-side mtu.min < mtu.max cross-check.
const DEFAULT_MTU_MIN = 1280
const DEFAULT_MTU_MAX = 1500

export interface ProbeDraft {
  mode: 'mesh' | 'direct'
  mesh: string
  site: string
  target: string
  // Direct mode only; mesh templates inherit the mesh's network and the
  // server rejects the combination.
  network: string
  type: string
  intervalS: string
  timeoutS: string
  trainCount: string
  trainSpacingMs: string
  params: Record<string, string>
}

export interface ProbeEditSnapshot {
  exists: boolean
  draft: ProbeDraft
  enabled: boolean
}

export function newDraft(plane: string): ProbeDraft {
  return {
    mode: 'mesh',
    mesh: '',
    site: '',
    target: '',
    network: plane,
    type: 'icmp',
    intervalS: '30',
    timeoutS: '5',
    trainCount: '',
    trainSpacingMs: '',
    params: {},
  }
}

export function draftFrom(p: ProbeConfig): ProbeDraft {
  return {
    mode: p.mesh ? 'mesh' : 'direct',
    mesh: p.mesh ?? '',
    site: p.site ?? '',
    target: p.target ?? '',
    network: p.network,
    type: p.type,
    intervalS: String(p.interval_ms / 1000),
    timeoutS: String(p.timeout_ms / 1000),
    trainCount: p.train_count ? String(p.train_count) : '',
    trainSpacingMs: p.train_spacing_ms ? String(p.train_spacing_ms) : '',
    params: { ...p.params },
  }
}

export function mutationSnapshot(p: ProbeConfig) {
  return {
    interval_ms: p.interval_ms,
    timeout_ms: p.timeout_ms,
    train_count: p.train_count,
    train_spacing_ms: p.train_spacing_ms,
    params: p.params,
    enabled: p.enabled,
  }
}

// cleanParams drops empty values: an absent key means "prober default", and
// the server rejects empty strings for required keys with a clear message.
function cleanParams(params: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(params)) {
    if (v.trim() !== '') out[k] = v.trim()
  }
  return out
}

// Mirrors the server's shared validation (probeadmin.ValidateSettings and
// the param registry rules) so nearly every mistake is caught before the
// round-trip; server 400s render verbatim as a backstop.
export function validate(d: ProbeDraft, specs: ParamSpec[]): { errors: string[]; body: object | null } {
  const errors: string[] = []
  const num = (label: string, s: string, empty: number): number => {
    if (s.trim() === '') return empty
    const n = Number(s)
    if (!Number.isFinite(n)) {
      errors.push(`${label} must be a number`)
      return NaN
    }
    return n
  }
  const intervalS = num('interval', d.intervalS, NaN)
  const timeoutS = num('timeout', d.timeoutS, NaN)
  const trainCount = num('train count', d.trainCount, 0)
  const trainSpacing = num('train spacing', d.trainSpacingMs, 0)

  if (d.mode === 'mesh' && d.mesh === '') errors.push('pick a mesh group')
  if (d.mode === 'direct' && (d.site === '' || d.target === '')) errors.push('pick a site and a target')

  if (Number.isFinite(intervalS) && Number.isFinite(timeoutS)) {
    if (intervalS <= 0 || timeoutS <= 0) errors.push('interval and timeout must be positive')
    else if (timeoutS >= intervalS) errors.push('timeout must be shorter than interval')
  } else if (Number.isNaN(intervalS) || Number.isNaN(timeoutS)) {
    if (d.intervalS.trim() === '') errors.push('interval is required')
    if (d.timeoutS.trim() === '') errors.push('timeout is required')
  }
  if (trainCount < 0 || trainSpacing < 0) errors.push('train count and spacing must not be negative')
  else if (trainCount === 0 && trainSpacing > 0) errors.push('train spacing requires a train count')
  else if (trainCount > 0 && Number.isFinite(timeoutS)) {
    const spacing = trainSpacing || DEFAULT_TRAIN_SPACING_MS
    if ((trainCount || DEFAULT_TRAIN_COUNT) * spacing >= timeoutS * 1000) {
      errors.push('the probe train must fit inside the timeout')
    }
  }

  const params = cleanParams(d.params)
  for (const spec of specs) {
    const required = d.mode === 'mesh' ? spec.required_mesh : spec.required_direct
    const v = params[spec.key]
    if (required && v === undefined) errors.push(`${spec.key} is required`)
    if (v === undefined) continue
    if (spec.kind === 'port') {
      const n = Number(v)
      if (!Number.isInteger(n) || n < 1 || n > 65535) errors.push(`${spec.key} must be an integer between 1 and 65535`)
    }
    if (spec.kind === 'status' && !/^[1-5](xx|[0-9]{2})$/.test(v)) {
      errors.push(`${spec.key} must be an exact status ("200") or a class ("2xx")`)
    }
    if (spec.kind === 'int') {
      const n = Number(v)
      const min = spec.min ?? Number.MIN_SAFE_INTEGER
      const max = spec.max ?? Number.MAX_SAFE_INTEGER
      if (!Number.isInteger(n) || n < min || n > max) {
        errors.push(`${spec.key} must be an integer between ${min} and ${max}`)
      }
    }
  }

  // Mirrors probeadmin's path_mtu cross-check: effective values with the
  // prober defaults substituted, so a lone out-of-range bound is caught.
  if (d.type === 'path_mtu') {
    const effMin = Number(params['mtu.min'] ?? DEFAULT_MTU_MIN)
    const effMax = Number(params['mtu.max'] ?? DEFAULT_MTU_MAX)
    if (Number.isInteger(effMin) && Number.isInteger(effMax) && effMin >= effMax) {
      errors.push(`mtu.min (${effMin}) must be less than mtu.max (${effMax})`)
    }
  }

  if (errors.length > 0) return { errors, body: null }
  return {
    errors: [],
    body: {
      interval_ms: Math.round(intervalS * 1000),
      timeout_ms: Math.round(timeoutS * 1000),
      train_count: trainCount,
      train_spacing_ms: trainSpacing,
      params,
    },
  }
}

// The registry drives which param fields exist for a type + assignment
// mode; a key the agent would silently ignore simply cannot be entered.
export function paramSpecsFor(registry: ProbeTypesResponse | null, type: string, mode: 'mesh' | 'direct'): ParamSpec[] {
  const specs = registry?.types.find((t) => t.type === type)?.params ?? []
  return specs.filter((s) => !(s.mesh_only && mode === 'direct'))
}

export function assignmentLabel(p: ProbeConfig): string {
  return p.mesh ? `mesh:${p.mesh}` : `${p.site} → ${p.target}`
}

export function paramsSummary(p: ProbeConfig): string {
  const entries = Object.entries(p.params)
  if (entries.length === 0) return '—'
  return entries.map(([k, v]) => `${k}=${v}`).join(', ')
}
