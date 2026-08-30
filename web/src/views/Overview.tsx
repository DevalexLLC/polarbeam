import { useCallback, useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import ConnectivityCard, { type ConnectivityMode } from '../components/ConnectivityCard'
import FleetAgentsCard from '../components/FleetAgentsCard'
import PageError from '../components/PageError'
import { fmtAgo } from '../format'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork } from '../routeState'
import { buildSiteTopology, topologyUrgentSites } from '../siteTopology'
import { buildThresholdResolver, cellSeverity } from '../severity'
import { resolveTopologyMode } from '../topologyMode'
import { useRouteParam } from '../useRouteState'
import type {
  AgentHealthResponse,
  AgentInfo,
  AgentsResponse,
  MatrixResponse,
  OutageEvent,
  OutagesResponse,
  SettingsResponse,
} from '../types'

const POLL_MS = 30_000
const NARROW_TOPOLOGY = '(max-width: 640px)'

function useNarrowTopology(): boolean {
  const [narrow, setNarrow] = useState(() => window.matchMedia(NARROW_TOPOLOGY).matches)
  useEffect(() => {
    const query = window.matchMedia(NARROW_TOPOLOGY)
    const update = () => setNarrow(query.matches)
    query.addEventListener('change', update)
    update()
    return () => query.removeEventListener('change', update)
  }, [])
  return narrow
}

// dropped_results is a lifetime running total that never resets, so it must
// not flag attention forever — only drops recent enough to still be
// actionable count. Ordered like the Agents view's health fold so the reason
// shown is the most severe one; must stay in lockstep with needs_attention
// in internal/server/store/inventory.go so the fleet card and the server-side
// Agents Attention filter agree.
const DROP_ATTENTION_MS = 24 * 60 * 60 * 1000
// Agents renew their cert at 2/3 lifetime (10 days left on the 30-day
// cert), so a healthy fleet always sits inside a 30-day window — the warn
// threshold must be BELOW the renewal point or it flags every agent from
// issuance. 7 days = renewal has been failing for 3+ days. Mirror of
// CERT_WARN_DAYS in Agents.tsx.
const CERT_WARN_DAYS = 7

function attentionReason(a: AgentInfo): string | null {
  if (a.cert_revoked_at) return 'Certificate revoked'
  if (a.cert_not_after != null && Date.parse(a.cert_not_after) < Date.now()) return 'Certificate expired'
  if (a.offline) return 'Offline'
  if (!a.last_seen_at) return 'Never connected'
  if (a.probes_failing > 0) return `${a.probes_failing} ${a.probes_failing === 1 ? 'probe' : 'probes'} failing`
  if (a.cert_not_after != null) {
    const days = Math.floor((Date.parse(a.cert_not_after) - Date.now()) / 86_400_000)
    if (days <= CERT_WARN_DAYS) return `Certificate expires in ${days}d`
  }
  if (a.last_dropped_at != null && Date.now() - Date.parse(a.last_dropped_at) < DROP_ATTENTION_MS) {
    return 'Spool drops in the last 24 h'
  }
  return null
}

// One key per rendered target: the API emits one event per failing series
// (probe × direction), so counts labeled "targets" must dedupe through this.
// NUL separator because site names are unrestricted text and NUL cannot
// appear in Postgres text (same convention as WorldMap's pairKey).
function targetKey(o: OutageEvent): string {
  return o.kind === 'agent_offline'
    ? `agent\u0000${o.src_site}\u0000${o.agent}`
    : `pair\u0000${o.src_site}\u0000${o.dst_site ?? o.target ?? '?'}`
}

function incidentCause(o: OutageEvent): string {
  if (o.kind === 'agent_offline') return 'Agent connection lost'
  // The degraded open_error is already a stable threshold description.
  if (o.kind === 'probe_degraded') return o.error || 'Critical threshold breached'
  const error = o.error?.toLowerCase() ?? ''
  if (error.includes('connection refused')) return 'Connection refused'
  if (error.includes('timeout') || error.includes('deadline exceeded')) return 'Timed out'
  if (error.includes('no such host')) return 'Host not found'
  return o.error || 'Probe failure'
}

function ratioStatus(value: number, total: number): string {
  if (total === 0) return ''
  if (total > 0 && value === total) return ' stat-good'
  if (total > 0 && value === 0) return ' stat-critical'
  return ' stat-warning'
}

export default function Overview({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  const [matrix, setMatrix] = useState<MatrixResponse | null>(null)
  const [agents, setAgents] = useState<AgentsResponse | null>(null)
  const [outages, setOutages] = useState<OutagesResponse | null>(null)
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [health, setHealth] = useState<AgentHealthResponse | null>(null)
  const [explicitTopology, setTopology] = useRouteParam('topology')
  const narrowTopology = useNarrowTopology()
  const connMode: ConnectivityMode = resolveTopologyMode(explicitTopology, narrowTopology)
  // The global top-bar filter; '' = all planes folded together — the
  // pre-networks view. Every stat tile and card on this page honors it.
  const { network: netFilter } = useNetworkFilter()
  const [error, setError] = useState<unknown>(null)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const [refreshing, setRefreshing] = useState(false)

  const load = useCallback(() => {
    setRefreshing(true)
    return Promise.all([
      apiGet<MatrixResponse>('/api/v1/matrix'),
      apiGet<AgentsResponse>('/api/v1/agents'),
      apiGet<OutagesResponse>('/api/v1/outages?window=24h'),
      apiGet<SettingsResponse>('/api/v1/settings'),
      apiGet<AgentHealthResponse>('/api/v1/agents/health?window=24h'),
    ])
      .then(([m, a, o, s, h]) => {
        setMatrix(m)
        setAgents(a)
        setOutages(o)
        setSettings(s)
        setHealth(h)
        setUpdatedAt(new Date())
        setError(null)
      })
      .catch((err) => {
        onAuthError(err)
        console.error('overview request failed', err)
        setError(err)
      })
      .finally(() => setRefreshing(false))
  }, [onAuthError])

  useEffect(() => {
    let cancelled = false
    const run = () => {
      if (!cancelled) void load()
    }
    run()
    const id = setInterval(run, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [load])

  const resolveThresholds = useMemo(() => buildThresholdResolver(settings), [settings])
  // With a plane selected, each cell narrows to its sub-cell (same fields,
  // folded server-side at (src, dst, network)). A pair with no sub-cell on
  // the plane drops out and renders "not probed"; an expected-but-silent
  // plane keeps a stale sub-cell — both honest under the filter.
  const shownCells = useMemo(() => {
    if (!matrix || netFilter === '') return matrix?.cells ?? []
    return matrix.cells.flatMap((cell) => {
      const sub = cell.networks.find((n) => n.network === netFilter)
      return sub ? [{ ...cell, ...sub, src: cell.src, dst: cell.dst, networks: [sub] }] : []
    })
  }, [matrix, netFilter])
  // Sites are network-agnostic rows; under a filter the card and the
  // sites-available tile scope to sites staffed by at least one agent on
  // the plane, so off-plane sites don't render as all-"not probed" noise.
  const shownSites = useMemo(() => {
    if (!matrix) return []
    if (netFilter === '' || !agents) return matrix.sites
    const staffed = new Set(agents.agents.filter((a) => a.network === netFilter).map((a) => a.site))
    return matrix.sites.filter((s) => staffed.has(s.name))
  }, [matrix, agents, netFilter])
  const shownAgents = useMemo(
    () => agents?.agents.filter((a) => matchesNetworkFilter(netFilter, a.network)) ?? [],
    [agents, netFilter],
  )
  const active = useMemo(
    () => outages?.outages.filter((o) => o.closed_at == null && matchesNetworkFilter(netFilter, o.network)) ?? [],
    [outages, netFilter],
  )
  const urgentSites = useMemo(() => {
    const offline = shownAgents.filter((agent) => agent.offline).map((agent) => agent.site)
    return topologyUrgentSites(offline, active)
  }, [active, shownAgents])
  const siteTopology = useMemo(
    () => buildSiteTopology(shownSites, shownCells, resolveThresholds, urgentSites),
    [resolveThresholds, shownCells, shownSites, urgentSites],
  )
  const activeGroups = useMemo(() => {
    const groups = new Map<string, { key: string; cause: string; probe: string; events: OutageEvent[] }>()
    for (const event of active) {
      const cause = incidentCause(event)
      const probe = event.probe_type ?? 'agent'
      const key = `${event.kind}\u0000${probe}\u0000${cause}`
      const group = groups.get(key) ?? { key, cause, probe, events: [] }
      group.events.push(event)
      groups.set(key, group)
    }
    return [...groups.values()]
  }, [active])
  const activeTargetCount = new Set(active.map(targetKey)).size
  const attention = shownAgents.filter((a) => attentionReason(a) != null)
  const healthyDirections = shownCells.filter((cell) => cellSeverity(cell, resolveThresholds) === 'ok').length
  const totalDirections = shownCells.length
  const availableSites = shownSites.filter((site) => {
    const own = shownAgents.filter((a) => a.site === site.name)
    return own.some(
      (a) =>
        a.last_seen_at != null &&
        !a.offline &&
        !a.cert_revoked_at &&
        (a.cert_not_after == null || Date.parse(a.cert_not_after) >= Date.now()),
    )
  }).length

  if (error && !matrix)
    return <PageError title="Overview unavailable" subject="overview" error={error} onRetry={() => void load()} />
  if (!matrix || !agents || !outages)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading network overview…
      </div>
    )

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Operations</div>
          <h1>Overview</h1>
          <p>Current health across sites, monitored directions, and the agent fleet.</p>
        </div>
        <div className="page-actions">
          <span className="freshness">Updated {fmtAgo(updatedAt?.toISOString() ?? null)}</span>
          <button className="secondary-button" disabled={refreshing} onClick={() => void load()}>
            {refreshing ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>
      </div>

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <section className="stat-grid" aria-label="Network health summary">
        <a
          className={'stat-card' + ratioStatus(availableSites, shownSites.length)}
          href={inheritRouteNetwork('#/agents')}
        >
          <span className="stat-label">Sites available</span>
          <strong>
            {availableSites}
            <small> / {shownSites.length}</small>
          </strong>
          <span className="stat-context">
            <span className="stat-badge">
              {shownSites.length === 0
                ? 'No sites'
                : availableSites === shownSites.length
                  ? 'All live'
                  : `${shownSites.length - availableSites} unavailable`}
            </span>
            Sites with a live agent
          </span>
        </a>
        <button
          type="button"
          className={'stat-card' + ratioStatus(healthyDirections, totalDirections)}
          onClick={() => {
            setTopology('matrix')
            document.getElementById('connectivity')?.scrollIntoView({ block: 'nearest' })
          }}
        >
          <span className="stat-label">Healthy directions</span>
          <strong>
            {healthyDirections}
            <small> / {totalDirections}</small>
          </strong>
          <span className="stat-context">
            <span className="stat-badge">
              {totalDirections === 0
                ? 'No probes'
                : healthyDirections === totalDirections
                  ? 'All healthy'
                  : `${totalDirections - healthyDirections} not healthy`}
            </span>
            Latest probe horizon
          </span>
        </button>
        <a
          className={'stat-card ' + (activeGroups.length > 0 ? 'stat-critical' : 'stat-good')}
          href={inheritRouteNetwork('#/incidents')}
        >
          <span className="stat-label">Active incident groups</span>
          {/* The server caps the open-event list during pathological
              incidents; a "+" marks every derived figure as a floor. */}
          <strong>{outages.truncated ? `${activeGroups.length}+` : activeGroups.length}</strong>
          <span className="stat-context">
            <span className="stat-badge">{activeGroups.length > 0 ? 'Active' : 'Clear'}</span>
            {active.length === 0
              ? 'No active incidents'
              : `${activeTargetCount}${outages.truncated ? '+' : ''} affected ${activeTargetCount === 1 ? 'target' : 'targets'}`}
          </span>
        </a>
        <a
          className={'stat-card ' + (attention.length > 0 ? 'stat-warning' : 'stat-good')}
          href={inheritRouteNetwork('#/agents')}
        >
          <span className="stat-label">Agents needing attention</span>
          <strong>{attention.length}</strong>
          <span className="stat-context">
            <span className="stat-badge">{attention.length > 0 ? 'Attention' : 'All healthy'}</span>
            Probe, certificate, or spool health
          </span>
        </a>
      </section>

      <div className="overview-main-row">
        <ConnectivityCard
          matrix={matrix}
          sites={shownSites}
          cells={shownCells}
          thresholds={resolveThresholds}
          topology={siteTopology}
          mode={connMode}
          onModeChange={setTopology}
        />
        <FleetAgentsCard
          agents={shownAgents}
          health={health}
          // Derived from the whole fleet, not the filtered subset, so the
          // chips don't appear and disappear as the filter changes.
          multiNetwork={new Set(agents.agents.map((a) => a.network)).size > 1}
        />
      </div>
    </>
  )
}
