import { useEffect, useState } from 'react'
import { apiGet } from '../api'
import PageError from '../components/PageError'
import { fmtAgo, fmtTime } from '../format'
import { useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, targetDetailHref, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { OperationalTarget, OperationalTargetsResponse } from '../types'

const POLL_MS = 30_000
const TARGET_PAGE = 25

function StatusCell({ t }: { t: OperationalTarget }) {
  if (t.status === 'incident') {
    return (
      <span>
        <span className="dot swatch status-down" /> incident
      </span>
    )
  }
  if (t.status === 'unprobed') return <span className="hint">unprobed</span>
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
  const [data, setData] = useState<OperationalTargetsResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [kind] = useRouteParam('kind', 'all')
  const [status] = useRouteParam('status', 'all')
  const [sort] = useRouteParam('sort', 'name')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const { network } = useNetworkFilter()

  useEffect(() => {
    let cancelled = false
    const params = new URLSearchParams({
      limit: String(TARGET_PAGE),
      offset: String((page - 1) * TARGET_PAGE),
      sort,
      order,
    })
    if (network) params.set('network', network)
    if (queryParam.trim()) params.set('q', queryParam.trim())
    if (kind !== 'all') params.set('kind', kind)
    if (status !== 'all') params.set('status', status === 'healthy' ? 'no_incidents' : status)
    const load = () =>
      apiGet<OperationalTargetsResponse>('/api/v1/targets?' + params.toString())
        .then((response) => {
          if (cancelled) return
          setData(response)
          setError(null)
        })
        .catch((err) => {
          onAuthError(err)
          console.error('targets request failed', err)
          if (!cancelled) setError(err)
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [kind, network, onAuthError, order, page, queryParam, retryKey, sort, status])

  const pageCount = Math.max(1, Math.ceil((data?.page.total ?? 0) / TARGET_PAGE))
  const visible = {
    externals: (data?.targets ?? []).filter((target) => target.kind === 'external'),
    sites: (data?.targets ?? []).filter((target) => target.kind === 'agent'),
  }
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  if (error && !data)
    return (
      <PageError
        title="Targets unavailable"
        subject="targets"
        error={error}
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
        onRetry={() => setRetryKey((key) => key + 1)}
      />
    )
  if (!data)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading targets…
      </div>
    )

  const externalCount = data.summary.external
  const siteCount = data.summary.agent
  const troubledCount = data.summary.incident

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

      {error !== null && (
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
                {externalCount === 0 ? 'No external targets match current filters' : 'No external targets on this page'}
              </strong>
              <span>
                {externalCount === 0
                  ? 'Change the kind, status, network, or search query.'
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
                        <StatusCell t={t} />
                      </td>
                      <td data-label="Name" className="mono">
                        <a href={targetDetailHref(t.id)}>{t.name}</a>
                      </td>
                      <td data-label="Endpoint" className="mono">
                        {t.url ? t.url : t.port ? `${t.address}:${t.port}` : t.address}
                      </td>
                      <td data-label="Probes">
                        {t.enabled_probe_count === t.probe_count
                          ? t.probe_count
                          : `${t.enabled_probe_count} of ${t.probe_count} enabled`}
                      </td>
                      <td data-label="Probed from" className="mono">
                        {t.probing_sites.join(', ') || '—'}
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
                {siteCount === 0 ? 'No site destinations match current filters' : 'No site destinations on this page'}
              </strong>
              <span>
                {siteCount === 0
                  ? 'Change the kind, status, network, or search query.'
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
                    return (
                      <tr key={t.id}>
                        <td data-label="Status">
                          <StatusCell t={t} />
                        </td>
                        <td data-label="Site" className="mono">
                          <a href={targetDetailHref(t.id)}>{t.agent_site ?? t.name}</a>
                        </td>
                        <td data-label="Agent" className="mono">
                          {t.agent_hostname ?? <span className="hint">deleted agent</span>}
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
            Page {page} of {pageCount} · {data.page.total} targets
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
