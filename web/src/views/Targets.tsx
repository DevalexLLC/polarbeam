import { useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type {
  AgentInfo,
  AgentsResponse,
  OutageEvent,
  OutagesResponse,
  ProbeConfig,
  ProbesConfigResponse,
  TargetConfig,
  TargetsConfigResponse,
} from '../types'

const POLL_MS = 30_000
const TARGET_PAGE = 25

// The browseable, read-only counterpart to Settings → Targets: every
// authenticated user can reach the per-target drill-down (#/target/<id>)
// from here. All four feeds are any-session reads; the joins are by
// target NAME because probe configs and outages carry names, not ids.
// Raw rows are kept so the global network filter can re-derive the view
// without a refetch.
interface Feeds {
  targets: TargetConfig[]
  probes: ProbeConfig[]
  agentByID: Map<string, AgentInfo>
  outages: OutageEvent[]
}

// Agent-kind targets are enrollment-managed rows named agent:<agent-uuid>;
// resolve the agent for a human label (site + hostname).
function agentIDOf(t: TargetConfig): string | null {
  return t.kind === 'agent' && t.name.startsWith('agent:') ? t.name.slice('agent:'.length) : null
}

// active: the target has at least one ENABLED direct probe (external
// targets; probe_count can't tell — it counts disabled configs too).
// Agent targets pass true: mesh templates probe them without config rows.
function StatusCell({ t, troubled, active }: { t: TargetConfig; troubled: Set<string>; active: boolean }) {
  if (troubled.has(t.name)) {
    return (
      <span>
        <span className="dot swatch status-down" /> incident
      </span>
    )
  }
  if (!active) return <span className="hint">unprobed</span>
  // "no incidents" rather than "ok": this cell is fed by the incident
  // feed alone, which says nothing about targets that were never measured.
  return (
    <span>
      <span className="dot swatch status-ok" /> no incidents
    </span>
  )
}

export default function Targets({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [feeds, setFeeds] = useState<Feeds | null>(null)
  const [error, setError] = useState('')
  const [query, setQuery] = useRouteSearch()
  const [kind] = useRouteParam('kind', 'all')
  const [status] = useRouteParam('status', 'all')
  const [sort] = useRouteParam('sort', 'name')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)

  useEffect(() => {
    let cancelled = false
    const load = () =>
      Promise.all([
        apiGet<TargetsConfigResponse>('/api/v1/config/targets'),
        apiGet<ProbesConfigResponse>('/api/v1/config/probes'),
        apiGet<AgentsResponse>('/api/v1/agents'),
        apiGet<OutagesResponse>('/api/v1/outages?window=24h'),
      ])
        .then(([targets, probes, agents, outages]) => {
          if (cancelled) return
          setFeeds({
            targets: targets.targets,
            probes: probes.probes,
            agentByID: new Map(agents.agents.map((a) => [a.id, a])),
            outages: outages.outages,
          })
          setError('')
        })
        .catch((err) => {
          onAuthError(err)
          if (!cancelled) setError(err instanceof Error ? err.message : String(err))
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError])

  // The global top-bar network filter scopes the whole view before the
  // search does: probing sites, incident status, rows, and header chips.
  const { network } = useNetworkFilter()
  const scoped = useMemo(() => {
    if (!feeds) return null
    const probeSites = new Map<string, string[]>() // target name -> distinct probing sites
    // target name -> direct-config count on the plane; replaces the
    // server-wide probe_count under a filter (same semantics: disabled
    // configs count, only the plane changes).
    const probeCounts = new Map<string, number>()
    for (const p of feeds.probes) {
      // Direct probes carry a target name; mesh templates carry a mesh.
      if (!p.target || !matchesNetworkFilter(network, p.network)) continue
      probeCounts.set(p.target, (probeCounts.get(p.target) ?? 0) + 1)
      // Disabled configs are not probing anyone — they count above but
      // contribute no probing site.
      if (!p.site || !p.enabled) continue
      const sites = probeSites.get(p.target) ?? []
      if (!sites.includes(p.site)) sites.push(p.site)
      probeSites.set(p.target, sites)
    }
    // target names with an open incident on the plane
    const troubled = new Set(
      feeds.outages
        .filter((o) => !o.closed_at && o.target && matchesNetworkFilter(network, o.network))
        .map((o) => o.target as string),
    )
    // An external belongs to the planes that probe it: under a filter one
    // with no enabled probe on the plane is not part of it — hidden, not
    // shown as "unprobed". Site destinations follow their agent's plane;
    // deleted-agent rows stay visible under any filter (gap convention).
    const externals = feeds.targets.filter((t) => t.kind === 'external' && (network === '' || probeSites.has(t.name)))
    const sites = feeds.targets.filter((t) => {
      if (t.kind !== 'agent') return false
      const agent = agentIDOf(t) ? feeds.agentByID.get(agentIDOf(t) as string) : undefined
      return agent == null || matchesNetworkFilter(network, agent.network)
    })
    return { probeSites, probeCounts, troubled, externals, sites }
  }, [feeds, network])
  const visibleAll = useMemo(() => {
    if (!feeds || !scoped) return []
    const needle = query.trim().toLowerCase()
    const targetStatus = (target: TargetConfig) => {
      if (scoped.troubled.has(target.name)) return 'incident'
      if (target.kind === 'external' && !scoped.probeSites.has(target.name)) return 'unprobed'
      return 'healthy'
    }
    const matches = (t: TargetConfig) => {
      if (kind !== 'all' && t.kind !== kind) return false
      if (status !== 'all' && targetStatus(t) !== status) return false
      if (!needle) return true
      const agent = agentIDOf(t) ? feeds.agentByID.get(agentIDOf(t) as string) : undefined
      return [t.name, t.address, t.url, agent?.site, agent?.hostname, ...(scoped.probeSites.get(t.name) ?? [])]
        .filter(Boolean)
        .some((v) => String(v).toLowerCase().includes(needle))
    }
    // oxlint-disable-next-line unicorn/no-array-sort
    return [...scoped.externals, ...scoped.sites].filter(matches).sort((a, b) => {
      const value = (target: TargetConfig): string | number => {
        if (sort === 'status') {
          const rank = { incident: 0, unprobed: 1, healthy: 2 }
          return rank[targetStatus(target)]
        }
        if (sort === 'probes') return scoped.probeCounts.get(target.name) ?? 0
        if (sort === 'created') return Date.parse(target.created_at)
        return target.name
      }
      const x = value(a)
      const y = value(b)
      const comparison = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      return order === 'desc' ? -comparison : comparison
    })
  }, [feeds, kind, order, query, scoped, sort, status])
  const externalRows = visibleAll.filter((target) => target.kind === 'external')
  const siteRows = visibleAll.filter((target) => target.kind === 'agent')
  const pageCount = Math.max(1, Math.ceil(visibleAll.length / TARGET_PAGE))
  const pageRows = visibleAll.slice((page - 1) * TARGET_PAGE, page * TARGET_PAGE)
  const visible = {
    externals: pageRows.filter((target) => target.kind === 'external'),
    sites: pageRows.filter((target) => target.kind === 'agent'),
  }
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  if (error && !feeds)
    return (
      <div className="state-panel state-error">
        <h1>Targets unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!feeds || !scoped)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading targets…
      </div>
    )

  const externalCount = scoped.externals.length
  const siteCount = scoped.sites.length
  const troubledCount = [...scoped.externals, ...scoped.sites].filter((t) => scoped.troubled.has(t.name)).length

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Operations</div>
          <h1>Targets</h1>
          <p>Everything the fleet probes: external destinations and the sites' own agents.</p>
        </div>
        <div className="chips">
          <span className="chip">
            external <span className="mono">{externalCount}</span>
          </span>
          <span className="chip">
            site destinations <span className="mono">{siteCount}</span>
          </span>
          <span className="chip">
            {troubledCount > 0 && <span className="dot swatch status-down" />}
            with incidents <span className="mono">{troubledCount}</span>
          </span>
        </div>
      </div>

      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar">
        <label className="search-field">
          <span className="sr-only">Search targets</span>
          <input
            type="search"
            placeholder="Search name, endpoint, or probing site"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <label className="compact-select">
          <span>Kind</span>
          <select value={kind} onChange={(event) => updateRouteParams({ kind: event.target.value, page: null })}>
            <option value="all">All kinds</option>
            <option value="external">External</option>
            <option value="agent">Site destinations</option>
          </select>
        </label>
        <label className="compact-select">
          <span>Status</span>
          <select value={status} onChange={(event) => updateRouteParams({ status: event.target.value, page: null })}>
            <option value="all">All statuses</option>
            <option value="incident">Incident</option>
            <option value="healthy">No incidents</option>
            <option value="unprobed">Unprobed</option>
          </select>
        </label>
        <label className="compact-select">
          <span>Sort</span>
          <select value={sort} onChange={(event) => updateRouteParams({ sort: event.target.value, page: null })}>
            <option value="name">Name</option>
            <option value="status">Status</option>
            <option value="probes">Probes</option>
            <option value="created">Created</option>
          </select>
        </label>
        <button
          type="button"
          className="secondary-button"
          aria-label={`Change to ${order === 'asc' ? 'descending' : 'ascending'} order; currently ${order === 'asc' ? 'ascending' : 'descending'}`}
          onClick={() => updateRouteParams({ order: order === 'asc' ? 'desc' : null, page: null })}
        >
          {order === 'asc' ? 'Ascending' : 'Descending'}
        </button>
      </div>

      {kind !== 'agent' && (
        <div className="card">
          <div className="card-head">
            <div>
              <span className="eyebrow">Probe destinations</span>
              <h2>External targets</h2>
            </div>
            <span className="hint">status reflects open incidents · admins manage these in Settings → Targets</span>
          </div>
          {visible.externals.length === 0 ? (
            <div className="empty-state">
              <strong>
                {externalRows.length === 0
                  ? scoped.externals.length === 0
                    ? 'No external targets'
                    : 'No external targets match current filters'
                  : 'No external targets on this page'}
              </strong>
              <span>
                {scoped.externals.length === 0
                  ? 'An admin can add hosts and URLs to probe in Settings → Targets.'
                  : externalRows.length === 0
                    ? 'Change the status filter or search query.'
                    : 'This page contains site destinations; use the pagination controls to move through all targets.'}
              </span>
            </div>
          ) : (
            <div className="scroll-x">
              <table className="events">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Name</th>
                    <th>Endpoint</th>
                    <th>Probes</th>
                    <th>Probed from</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {visible.externals.map((t) => (
                    <tr key={t.id}>
                      <td data-label="Status">
                        <StatusCell t={t} troubled={scoped.troubled} active={scoped.probeSites.has(t.name)} />
                      </td>
                      <td data-label="Name" className="mono">
                        <a href={inheritRouteNetwork('#/target/' + encodeURIComponent(t.id))}>{t.name}</a>
                      </td>
                      <td data-label="Endpoint" className="mono">
                        {t.url ? t.url : t.port ? `${t.address}:${t.port}` : t.address}
                      </td>
                      <td data-label="Probes">{scoped.probeCounts.get(t.name) ?? 0}</td>
                      <td data-label="Probed from" className="mono">
                        {(scoped.probeSites.get(t.name) ?? []).join(', ') || '—'}
                      </td>
                      <td data-label="Created" title={fmtTime(t.created_at)}>
                        {fmtAgo(t.created_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}

      {kind !== 'external' && (
        <div className="card">
          <div className="card-head">
            <div>
              <span className="eyebrow">Agent mesh</span>
              <h2>Site destinations</h2>
            </div>
            <span className="hint">each site's agent as a probe destination — inbound metrics per source site</span>
          </div>
          {visible.sites.length === 0 ? (
            <div className="empty-state">
              <strong>
                {siteRows.length === 0
                  ? scoped.sites.length === 0
                    ? 'No agents enrolled yet'
                    : 'No site destinations match current filters'
                  : 'No site destinations on this page'}
              </strong>
              <span>
                {scoped.sites.length === 0
                  ? 'Enroll an agent and its destination appears here.'
                  : siteRows.length === 0
                    ? 'Change the status filter or search query.'
                    : 'This page contains external targets; use the pagination controls to move through all targets.'}
              </span>
            </div>
          ) : (
            <div className="scroll-x">
              <table className="events">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Site</th>
                    <th>Agent</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {visible.sites.map((t) => {
                    const agent = agentIDOf(t) ? feeds.agentByID.get(agentIDOf(t) as string) : undefined
                    return (
                      <tr key={t.id}>
                        <td data-label="Status">
                          <StatusCell t={t} troubled={scoped.troubled} active />
                        </td>
                        <td data-label="Site" className="mono">
                          <a href={inheritRouteNetwork('#/target/' + encodeURIComponent(t.id))}>
                            {agent ? agent.site : t.name}
                          </a>
                        </td>
                        <td data-label="Agent" className="mono">
                          {agent ? agent.hostname : <span className="hint">deleted agent</span>}
                        </td>
                        <td data-label="Created" title={fmtTime(t.created_at)}>
                          {fmtAgo(t.created_at)}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}

      {pageCount > 1 && (
        <div className="progressive-footer">
          <span className="hint">
            Page {page} of {pageCount} · {visibleAll.length} targets
          </span>
          <button className="secondary-button" disabled={page === 1} onClick={() => setPage(page - 1)}>
            Previous
          </button>
          <button className="secondary-button" disabled={page === pageCount} onClick={() => setPage(page + 1)}>
            Next
          </button>
        </div>
      )}
    </>
  )
}
