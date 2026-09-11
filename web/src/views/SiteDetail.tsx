import { useEffect, useMemo } from 'react'
import { apiGet } from '../api'
import type { Caps } from '../caps'
import FleetAgentsCard from '../components/FleetAgentsCard'
import IncidentGroupRow from '../components/IncidentGroupRow'
import IncidentTimeline, {
  WINDOW_MS,
  bucketRangeLabel,
  gridWithYear,
  gridWithZone,
  overlapsBucket,
  timelineGrid,
} from '../components/IncidentTimeline'
import { CLASS_LABEL, SEV_CLASS } from '../components/MatrixTable'
import PageError from '../components/PageError'
import { fmtAgo, fmtLatency } from '../format'
import { groupIncidents, incidentFreeRatio } from '../incidentGroups'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { incidentPairHref, inheritRouteNetwork, siteInvestigateHref, updateRouteParams } from '../routeState'
import {
  SEVERITY_LABEL,
  buildThresholdResolver,
  cellSeverity,
  directionSeverity,
  type ThresholdResolver,
} from '../severity'
import { agentIsLive, ratioStatus } from '../siteHealth'
import { fmtPercent, scoreTone } from '../siteScores'
import { buildSiteTopology, topologyUrgentSites } from '../siteTopology'
import { useTimezone } from '../timezone'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam } from '../useRouteState'
import type {
  AgentHealthResponse,
  AgentsResponse,
  MatrixCell,
  MatrixResponse,
  OutagesResponse,
  SettingsResponse,
  Window,
} from '../types'
import { WINDOWS } from '../types'

const scrollTo = (id: string) => document.getElementById(id)?.scrollIntoView({ block: 'nearest' })

// One direction of a peer row: the latest fold's status, latency, and loss,
// plus per-plane chips on multi-network installs.
function DirectionCell({
  cell,
  resolve,
  multiNetwork,
}: {
  cell: MatrixCell | undefined
  resolve: ThresholdResolver
  multiNetwork: boolean
}) {
  if (!cell) return <span className="muted">not probed</span>
  const cls = SEV_CLASS[cellSeverity(cell, resolve)]
  return (
    <span className="site-direction">
      <span className={'status-text-' + cls}>
        <span className={'dot swatch status-' + cls} /> {CLASS_LABEL[cls]}
      </span>
      <span className="site-direction-value">
        {cell.latency_us != null && (cell.status === 'ok' || cell.status === 'degraded')
          ? fmtLatency(cell.latency_us)
          : '—'}
        {cell.loss_pct != null && cell.loss_pct > 0 ? ` · ${cell.loss_pct.toFixed(1)}% loss` : ''}
      </span>
      {multiNetwork && cell.networks.length > 0 && (
        <span className="site-direction-planes">
          {cell.networks.map((sub) => (
            <span key={sub.network} className="chip">
              <span className="mono">{sub.network}</span> ·{' '}
              {SEVERITY_LABEL[directionSeverity({ ...cell, ...sub }, resolve(cell.src, cell.dst, sub.network))]}
            </span>
          ))}
        </span>
      )}
    </span>
  )
}

export default function SiteDetail({
  name,
  caps,
  onAuthError,
  onTitleChange,
}: {
  name: string
  caps: Caps | null
  onAuthError: (err: unknown) => void
  onTitleChange: (label: string) => void
}) {
  const { mode } = useTimezone() // re-render bucket labels on UTC/local toggle
  const [windowParam] = useRouteParam('window', '24h')
  const [selectedSlice, setSelectedSlice] = useRouteNumber('slice', 0)
  const [expandedIncident, setExpandedIncident] = useRouteParam('incident')
  const win = windowParam as Window
  const selectedBucket = selectedSlice || null
  // The global top-bar filter; every tile and card on this page honors it
  // the way the Overview does — client-side, over the same responses.
  const { network: netFilter } = useNetworkFilter()

  const { data, error, refreshing, lastLoadedAt, reload } = usePolledResource(
    () =>
      Promise.all([
        apiGet<MatrixResponse>('/api/v1/matrix'),
        apiGet<AgentsResponse>('/api/v1/agents'),
        apiGet<OutagesResponse>(`/api/v1/outages?window=${win}&site=${encodeURIComponent(name)}&include_routes=true`),
        apiGet<SettingsResponse>('/api/v1/settings'),
        apiGet<AgentHealthResponse>('/api/v1/agents/health?window=24h'),
      ]).then(([matrix, agents, outages, settings, health]) => ({ matrix, agents, outages, settings, health })),
    // NUL join: site names are unrestricted text.
    { key: [name, win].join('\u0000'), enabled: name !== '', onAuthError, logLabel: 'site detail' },
  )
  const matrix = data?.matrix ?? null
  const agents = data?.agents ?? null
  const outages = data?.outages ?? null
  const settings = data?.settings ?? null
  const health = data?.health ?? null

  // Identity resolves against the UNFILTERED site list: a site with no agent
  // on the selected plane is still this site, just unstaffed here.
  const site = useMemo(() => matrix?.sites.find((s) => s.name === name) ?? null, [matrix, name])
  useEffect(() => {
    if (site) onTitleChange(site.display_name || site.name)
  }, [site, onTitleChange])

  const resolveThresholds = useMemo(() => buildThresholdResolver(settings), [settings])
  const shownCells = useMemo(() => {
    if (!matrix || netFilter === '') return matrix?.cells ?? []
    return matrix.cells.flatMap((cell) => {
      const sub = cell.networks.find((n) => n.network === netFilter)
      return sub ? [{ ...cell, ...sub, src: cell.src, dst: cell.dst, networks: [sub] }] : []
    })
  }, [matrix, netFilter])
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
  const siteAgents = useMemo(() => shownAgents.filter((a) => a.site === name), [shownAgents, name])
  const sitePlanes = useMemo(
    () =>
      [...new Set(agents?.agents.filter((a) => a.site === name).map((a) => a.network) ?? [])]
        // oxlint-disable-next-line unicorn/no-array-sort -- fresh array from the spread
        .sort(),
    [agents, name],
  )
  const multiNetwork = useMemo(() => new Set(agents?.agents.map((a) => a.network) ?? []).size > 1, [agents])
  const events = useMemo(
    () => (outages?.outages ?? []).filter((o) => matchesNetworkFilter(netFilter, o.network)),
    [outages, netFilter],
  )
  const active = useMemo(() => events.filter((o) => o.closed_at == null), [events])
  // Under a plane filter the site can drop out of the staffed list; the
  // topology entry is then undefined and every direction figure reads 0/0.
  const topology = useMemo(() => {
    const offline = shownAgents.filter((a) => a.offline).map((a) => a.site)
    const urgent = topologyUrgentSites(offline, active)
    return buildSiteTopology(shownSites, shownCells, resolveThresholds, urgent).find((t) => t.site.name === name)
  }, [active, name, resolveThresholds, shownAgents, shownCells, shownSites])
  const cellFor = useMemo(() => {
    const map = new Map<string, MatrixCell>()
    for (const cell of shownCells) map.set(cell.src + '\u0000' + cell.dst, cell)
    return map
  }, [shownCells])

  // The chart's window and every derived history figure come from the
  // SNAPSHOT, not the selector: the previous response stays on screen while
  // a new window loads (or if the refresh fails), and a 365d denominator
  // over a 24h snapshot would misreport.
  const snapshotWin =
    outages && (WINDOWS as readonly string[]).includes(outages.window) ? (outages.window as Window) : win
  const fetchedAt = useMemo(() => {
    if (!outages) return 0
    const serverNow = Date.parse(outages.now)
    return Number.isFinite(serverNow) ? serverNow : (lastLoadedAt?.getTime() ?? 0)
  }, [outages, lastLoadedAt])
  const timeline = useMemo(() => {
    if (!fetchedAt || !outages) return null
    const grid = timelineGrid(snapshotWin, fetchedAt)
    const bucket =
      selectedBucket != null && selectedBucket >= grid.startMs && selectedBucket < grid.endMs ? selectedBucket : null
    return { grid, bucket, win: snapshotWin }
  }, [outages, snapshotWin, fetchedAt, selectedBucket])
  const bucket = timeline?.bucket ?? null
  const groups = useMemo(() => {
    const filtered =
      timeline?.bucket == null
        ? events
        : events.filter((event) => overlapsBucket(event, timeline.bucket!, timeline.grid.bucketMs, fetchedAt))
    return groupIncidents(filtered)
  }, [events, timeline, fetchedAt])
  const activeGroups = useMemo(() => groupIncidents(active).length, [active])
  const freeRatio = useMemo(
    () => (fetchedAt ? incidentFreeRatio(events, WINDOW_MS[snapshotWin], fetchedAt) : 1),
    [events, snapshotWin, fetchedAt],
  )
  const historyCapped = Boolean(outages?.truncated || outages?.history_truncated)

  useEffect(() => {
    if (selectedBucket != null && timeline && timeline.bucket == null) setSelectedSlice(0, 'replace')
  }, [selectedBucket, setSelectedSlice, timeline])
  useEffect(() => {
    if (!expandedIncident || !outages) return
    if (groupIncidents(events).some((group) => group.id === expandedIncident)) return
    setExpandedIncident('', 'replace')
  }, [outages, events, expandedIncident, setExpandedIncident])

  if (name === '')
    return (
      <PageError
        title="Site not found"
        subject="site"
        message="This address names no site."
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
      />
    )
  if (error && !data)
    return (
      <PageError
        title="Site unavailable"
        subject="site"
        error={error}
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
        onRetry={() => void reload()}
      />
    )
  if (!matrix || !agents || !outages)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading site…
      </div>
    )
  if (!site)
    return (
      <PageError
        title="Site not found"
        subject="site"
        message={`No site named ${name} is enrolled.`}
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
      />
    )

  const label = site.display_name || site.name
  const liveAgents = siteAgents.filter(agentIsLive).length
  const directions = topology?.stats.directions ?? 0
  const healthyDirections = topology?.stats.dirCounts.ok ?? 0
  const peers = topology?.stats.peers ?? []
  const subline = [
    site.display_name ? site.name : '',
    site.location,
    sitePlanes.length > 0 ? `${sitePlanes.length === 1 ? 'network' : 'networks'}: ${sitePlanes.join(', ')}` : '',
  ]
    .filter(Boolean)
    .join(' · ')
  const peerIncidents = (peer: string) =>
    events.filter((o) => (o.src_site === name && o.dst_site === peer) || (o.src_site === peer && o.dst_site === name))
      .length
  const filterEmpty = netFilter !== '' && siteAgents.length === 0

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="breadcrumb">
            <a href={inheritRouteNetwork('#/')}>Overview</a> / Site
          </div>
          <h1>{label}</h1>
          {subline && <p className="sub">{subline}</p>}
        </div>
        <div className="page-actions">
          <span className="freshness">Updated {fmtAgo(lastLoadedAt?.toISOString() ?? null)}</span>
          <button className="secondary-button" disabled={refreshing} onClick={() => void reload()}>
            {refreshing ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>
      </div>

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="controls">
        <div className="control-group" role="group" aria-label="Time window">
          {WINDOWS.map((w) => (
            <button
              key={w}
              className={win === w ? 'active' : ''}
              aria-pressed={win === w}
              onClick={() => updateRouteParams({ window: w === '24h' ? null : w, slice: null, incident: null })}
            >
              {w}
            </button>
          ))}
        </div>
      </div>

      <section className="stat-grid" aria-label="Site health summary">
        <button
          type="button"
          className={'stat-card' + ratioStatus(liveAgents, siteAgents.length)}
          onClick={() => scrollTo('site-agents')}
        >
          <span className="stat-label">Agents available</span>
          <strong>
            {liveAgents}
            <small> / {siteAgents.length}</small>
          </strong>
          <span className="stat-context">Agents at this site with a live session</span>
        </button>
        <button
          type="button"
          className={'stat-card' + ratioStatus(healthyDirections, directions)}
          onClick={() => scrollTo('site-peers')}
        >
          <span className="stat-label">Healthy directions</span>
          <strong>
            {healthyDirections}
            <small> / {directions}</small>
          </strong>
          <span className="stat-context">Latest probe horizon, both ways</span>
        </button>
        <button
          type="button"
          className={'stat-card ' + (activeGroups > 0 ? 'stat-critical' : 'stat-good')}
          onClick={() => scrollTo('site-incidents')}
        >
          <span className="stat-label">Active incident groups</span>
          <strong>{outages.truncated ? `${activeGroups}+` : activeGroups}</strong>
          <span className="stat-context">
            {active.length === 0
              ? 'No active incidents'
              : `${active.length}${outages.truncated ? '+' : ''} open ${active.length === 1 ? 'event' : 'events'}`}
          </span>
        </button>
        <button type="button" className={'stat-card' + scoreTone(freeRatio)} onClick={() => scrollTo('site-incidents')}>
          <span className="stat-label">Incident-free time</span>
          <strong>
            {historyCapped ? '≤ ' : ''}
            {fmtPercent(freeRatio)}
            <small> %</small>
          </strong>
          <span className="stat-context">
            {historyCapped ? `Upper bound: history over ${snapshotWin} is capped` : `Of the last ${snapshotWin}`}
          </span>
        </button>
      </section>

      <section className="card site-peers-card" id="site-peers">
        <div className="card-head">
          <div>
            <h2>Peers</h2>
          </div>
          <span className="freshness">Latest {Math.round(matrix.horizon_s / 60)}-minute probe horizon</span>
        </div>
        {peers.length === 0 ? (
          <div className="empty-state">
            <strong>{filterEmpty ? `No agents at this site on network ${netFilter}` : 'No monitored peers'}</strong>
            <span>
              {filterEmpty
                ? 'Clear the network filter or pick a plane this site is staffed on.'
                : 'Add this site to a mesh group and pair directions appear here.'}
            </span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="site-peers">
              <thead>
                <tr>
                  <th scope="col">Peer</th>
                  <th scope="col">Outbound</th>
                  <th scope="col">Inbound</th>
                  <th scope="col" className="site-peers-count">
                    Incidents in {snapshotWin}
                  </th>
                  <th scope="col" className="site-peers-link">
                    <span className="sr-only">Pair detail</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {peers.map((peer) => {
                  const href = incidentPairHref(name, peer, snapshotWin)
                  return (
                    <tr key={peer}>
                      <th scope="row" className="mono">
                        {peer}
                      </th>
                      <td>
                        <DirectionCell
                          cell={cellFor.get(name + '\u0000' + peer)}
                          resolve={resolveThresholds}
                          multiNetwork={multiNetwork}
                        />
                      </td>
                      <td>
                        <DirectionCell
                          cell={cellFor.get(peer + '\u0000' + name)}
                          resolve={resolveThresholds}
                          multiNetwork={multiNetwork}
                        />
                      </td>
                      <td className="site-peers-count">{peerIncidents(peer)}</td>
                      <td className="site-peers-link">
                        {href && (
                          <a href={href} aria-label={`Open pair detail for ${name} and ${peer}`}>
                            Pair detail
                          </a>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
        <p className="card-foot">
          Outbound is this site probing the peer, inbound the reverse. Incident counts include every event in the
          selected window whose two ends are this site and the peer.
        </p>
      </section>

      <div id="site-agents">
        {siteAgents.length === 0 ? (
          <section className="card">
            <div className="card-head">
              <div>
                <h2>Agents</h2>
              </div>
            </div>
            <div className="empty-state">
              <strong>
                {filterEmpty ? `No agents at this site on network ${netFilter}` : 'No agents at this site'}
              </strong>
              <span>
                {filterEmpty
                  ? 'Clear the network filter to see the whole site.'
                  : 'Enroll an agent here to start measuring.'}
              </span>
            </div>
          </section>
        ) : (
          <FleetAgentsCard agents={siteAgents} health={health} multiNetwork={multiNetwork} />
        )}
      </div>

      {timeline && (
        <section className="card chart-card incident-timeline-card" id="site-incidents">
          <div className="card-head">
            <div>
              <h2>Incident timeline</h2>
            </div>
          </div>
          <IncidentTimeline
            events={events}
            win={timeline.win}
            nowMs={fetchedAt}
            selected={bucket}
            onSelect={(value) => setSelectedSlice(value ?? 0)}
          />
          <p className="card-foot">
            Incidents whose source or destination is this site, colored by kind, with resolved ones muted. Click a bar
            to filter the list below.
            {outages.history_truncated ? ' Resolved incidents past the newest 500 are omitted.' : ''}
            {outages.truncated ? ' The oldest open incidents are omitted (server cap).' : ''}
          </p>
        </section>
      )}

      <section className="card incident-card">
        <div className="card-head">
          <div>
            <h2>Incident groups</h2>
          </div>
          {bucket != null && timeline ? (
            <button className="chip bucket-filter-chip" onClick={() => setSelectedSlice(0)}>
              {bucketRangeLabel(
                bucket,
                timeline.grid.bucketMs,
                timeline.win,
                mode === 'utc',
                gridWithZone(timeline.grid, timeline.win, mode === 'utc'),
                gridWithYear(timeline.grid),
              )}{' '}
              <span aria-hidden="true">×</span>
              <span className="sr-only">Clear time filter</span>
            </button>
          ) : null}
        </div>
        {groups.length === 0 ? (
          <div className="empty-state">
            <strong>{bucket != null ? 'No incidents in this slice' : `No incidents in the last ${snapshotWin}`}</strong>
            <span>
              {bucket != null ? 'Clear the time filter or pick another bar.' : 'The network watch continues.'}
            </span>
          </div>
        ) : (
          groups.map((group) => (
            <IncidentGroupRow
              key={group.key}
              group={group}
              win={snapshotWin}
              expanded={expandedIncident === group.id}
              onToggle={() => setExpandedIncident(expandedIncident === group.id ? '' : group.id)}
            />
          ))
        )}
        <p className="card-foot">
          Active groups come first, then resolved ones newest first. Open a group for its affected targets and
          investigation links.
        </p>
      </section>

      <section className="card site-investigate">
        <div className="card-head">
          <div>
            <h2>Investigate</h2>
          </div>
        </div>
        <div className="incident-resource-links">
          <a href={siteInvestigateHref('agents', name, win)}>Agents at {name}</a>
          <a href={siteInvestigateHref('routes', name, win)}>Route changes</a>
          <a href={siteInvestigateHref('targets', name, win)}>Targets probed from here</a>
          {caps?.adminWrite && <a href={siteInvestigateHref('settings-sites', name, win)}>Site settings</a>}
        </div>
      </section>
    </>
  )
}
