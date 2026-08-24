import { cellSeverity, directionSeverity, worst, type Severity, type ThresholdResolver } from './severity.ts'
import type { MatrixCell, OutageEvent, Site } from './types'

const pairKey = (a: string, b: string) => a + '\u0000' + b

const HEALTH_RANK: Record<Severity, number> = {
  down: 0,
  crit: 1,
  warn: 1,
  stale: 2,
  ok: 3,
}

const SITE_SEVERITY_RANK: Record<Severity, number> = {
  ok: 0,
  stale: 1,
  warn: 2,
  crit: 2,
  down: 3,
}

function worseSiteSeverity(a: Severity, b: Severity): Severity {
  const aRank = SITE_SEVERITY_RANK[a]
  const bRank = SITE_SEVERITY_RANK[b]
  if (aRank === bRank) return worst(a, b)
  return aRank > bRank ? a : b
}

export interface SiteTopologyStats {
  degree: number
  bestLatencyUs: number | null
  directions: number
  dirCounts: Record<Severity, number>
  netCounts: Map<string, { ok: number; total: number }>
  peers: string[]
}

export interface SiteTopology {
  site: Site
  severity: Severity
  stats: SiteTopologyStats
}

export function topologyUrgentSites(
  offlineSites: Iterable<string>,
  incidents: Pick<OutageEvent, 'kind' | 'src_site' | 'dst_site'>[],
): Set<string> {
  const urgent = new Set(offlineSites)
  for (const incident of incidents) {
    // Individual probe failures, including an inter-site partial failure,
    // are already represented by the matrix fold as degraded or down. Only
    // an agent-offline incident independently establishes site urgency.
    if (incident.kind === 'agent_offline') urgent.add(incident.src_site)
  }
  return urgent
}

function newStats(): SiteTopologyStats {
  return {
    degree: 0,
    bestLatencyUs: null,
    directions: 0,
    dirCounts: { ok: 0, warn: 0, crit: 0, down: 0, stale: 0 },
    netCounts: new Map(),
    peers: [],
  }
}

export function buildSiteTopology(
  sites: Site[],
  cells: MatrixCell[],
  thresholds: ThresholdResolver,
  urgentSites: ReadonlySet<string> = new Set(),
): SiteTopology[] {
  const severities = new Map<string, Severity>()
  const statsBySite = new Map<string, SiteTopologyStats>()
  for (const site of sites) statsBySite.set(site.name, newStats())

  for (const cell of cells) {
    const severity = cellSeverity(cell, thresholds)
    for (const name of [cell.src, cell.dst]) {
      const previous = severities.get(name)
      severities.set(name, previous === undefined ? severity : worseSiteSeverity(previous, severity))
      const stats = statsBySite.get(name)
      if (stats) {
        stats.directions++
        stats.dirCounts[severity]++
      }
    }
    for (const sub of cell.networks) {
      const subSeverity = directionSeverity({ ...cell, ...sub }, thresholds(cell.src, cell.dst, sub.network))
      for (const name of [cell.src, cell.dst]) {
        const stats = statsBySite.get(name)
        if (!stats) continue
        const entry = stats.netCounts.get(sub.network) ?? { ok: 0, total: 0 }
        entry.total++
        if (subSeverity === 'ok') entry.ok++
        stats.netCounts.set(sub.network, entry)
      }
    }
  }

  const byPair = new Map<string, { x: string; y: string; cells: MatrixCell[] }>()
  for (const cell of cells) {
    const [x, y] = cell.src < cell.dst ? [cell.src, cell.dst] : [cell.dst, cell.src]
    const key = pairKey(x, y)
    const entry = byPair.get(key) ?? { x, y, cells: [] }
    entry.cells.push(cell)
    byPair.set(key, entry)
  }
  for (const { x, y, cells: pairCells } of byPair.values()) {
    const liveLatencies = pairCells
      .filter((cell) => cell.status === 'ok' || cell.status === 'degraded')
      .map((cell) => cell.latency_us)
      .filter((latency): latency is number => latency != null)
    for (const [name, peer] of [
      [x, y],
      [y, x],
    ]) {
      const stats = statsBySite.get(name)
      if (!stats) continue
      stats.degree++
      stats.peers.push(peer)
      if (liveLatencies.length > 0) {
        const best = Math.min(...liveLatencies)
        stats.bestLatencyUs = stats.bestLatencyUs == null ? best : Math.min(stats.bestLatencyUs, best)
      }
    }
  }
  for (const stats of statsBySite.values()) {
    // oxlint-disable-next-line unicorn/no-array-sort -- ES2022 build target
    stats.peers.sort()
  }

  return sites.map((site) => ({
    site,
    severity: urgentSites.has(site.name) ? 'down' : (severities.get(site.name) ?? 'stale'),
    stats: statsBySite.get(site.name)!,
  }))
}

export function rankSiteTopology(topology: SiteTopology[]): SiteTopology[] {
  // oxlint-disable-next-line unicorn/no-array-sort -- sorting a fresh copy is non-mutating to callers
  return [...topology].sort((a, b) => {
    const rank = HEALTH_RANK[a.severity] - HEALTH_RANK[b.severity]
    if (rank !== 0) return rank
    const aLabel = a.site.display_name || a.site.name
    const bLabel = b.site.display_name || b.site.name
    if (aLabel !== bLabel) return aLabel < bLabel ? -1 : 1
    return a.site.name < b.site.name ? -1 : a.site.name > b.site.name ? 1 : 0
  })
}
