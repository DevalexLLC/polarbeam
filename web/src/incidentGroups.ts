// Incident grouping shared by the Incidents page and the site page: the
// pure half (no JSX) so node tests can import it directly.
import type { OutageEvent } from './types'

export function fmtDuration(openedAt: string, closedAt: string | null): string {
  const end = closedAt ? new Date(closedAt).getTime() : Date.now()
  const s = Math.max(0, Math.round((end - new Date(openedAt).getTime()) / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${(s / 3600).toFixed(1)}h`
  return `${(s / 86400).toFixed(1)}d`
}

// Normalization (for grouping keys) keeps the FULL error text so two long
// errors sharing a prefix never merge into one incident; truncation is
// display-only in errorSummary.
export function normalizeError(error: string | null): string {
  if (!error) return 'No error detail'
  const lower = error.toLowerCase()
  if (lower.includes('connection refused')) return 'Connection refused'
  if (lower.includes('timeout') || lower.includes('deadline exceeded')) return 'Timed out'
  if (lower.includes('no such host')) return 'Host not found'
  return error
}

export function errorSummary(error: string | null): string {
  const normalized = normalizeError(error)
  return normalized.length > 72 ? normalized.slice(0, 69) + '…' : normalized
}

export function incidentTarget(o: OutageEvent): string {
  const source = o.src_site || o.agent || `agent ${o.agent_id.slice(0, 8)}`
  if (o.kind === 'agent_offline') return `${source} · ${o.agent || 'deleted agent'}`
  return `${source} → ${o.dst_site ?? o.target ?? (o.target_id ? `target ${o.target_id.slice(0, 8)}` : '?')}`
}

export interface IncidentGroup {
  id: string
  key: string
  open: boolean
  kind: OutageEvent['kind']
  probe: string
  error: string | null
  events: OutageEvent[]
}

export function incidentSelectionID(key: string): string {
  let left = 2_166_136_261
  let right = 3_332_046_959
  for (let index = 0; index < key.length; index++) {
    const code = key.charCodeAt(index)
    left = Math.imul(left ^ code, 16_777_619)
    right = Math.imul(right ^ code, 2_246_822_519)
  }
  return `i${(left >>> 0).toString(36)}${(right >>> 0).toString(36)}`
}

export function groupIncidents(events: OutageEvent[]): IncidentGroup[] {
  const groups = new Map<string, IncidentGroup>()
  for (const event of events) {
    const open = event.closed_at == null
    const key = [open ? 'active' : 'resolved', event.kind, event.probe_type ?? '', normalizeError(event.error)].join(
      '\u0000',
    )
    const group = groups.get(key) ?? {
      id: incidentSelectionID(key),
      key,
      open,
      kind: event.kind,
      probe: event.probe_type ?? 'agent',
      error: event.error,
      events: [],
    }
    group.events.push(event)
    groups.set(key, group)
  }
  // The spread already copies, so this sorts a fresh array and mutates
  // nothing shared.
  // oxlint-disable-next-line unicorn/no-array-sort
  return [...groups.values()].sort((a, b) => {
    if (a.open !== b.open) return a.open ? -1 : 1
    return Date.parse(b.events[0].opened_at) - Date.parse(a.events[0].opened_at)
  })
}

// Fraction of [nowMs - windowMs, nowMs] with no incident open: the union of
// every event's [opened, closed ?? now] clipped to the window. An event
// opened before the window counts from the window start; overlapping
// events never double-count. 1 when nothing was open; the caller decides
// whether a server cap makes that an upper bound.
export function incidentFreeRatio(events: OutageEvent[], windowMs: number, nowMs: number): number {
  if (!(windowMs > 0)) return 1
  const startMs = nowMs - windowMs
  const spans: [number, number][] = []
  for (const event of events) {
    const opened = Date.parse(event.opened_at)
    if (!Number.isFinite(opened)) continue
    const closedRaw = event.closed_at == null ? nowMs : Date.parse(event.closed_at)
    const closed = Number.isFinite(closedRaw) ? closedRaw : nowMs
    const from = Math.max(opened, startMs)
    const to = Math.min(closed, nowMs)
    if (to > from) spans.push([from, to])
  }
  // oxlint-disable-next-line unicorn/no-array-sort -- fresh local array
  spans.sort((a, b) => a[0] - b[0])
  let covered = 0
  let cursor = -Infinity
  for (const [from, to] of spans) {
    const start = Math.max(from, cursor)
    if (to > start) {
      covered += to - start
      cursor = to
    }
  }
  return Math.max(0, Math.min(1, 1 - covered / windowMs))
}
