import { useEffect } from 'react'
import { apiGet } from '../api'
import DataTable, { type DataTableColumn } from '../components/DataTable'
import PageError from '../components/PageError'
import PathGraph from '../components/PathGraph'
import { fmtAgo, fmtTime } from '../format'
import { useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import { useStickyPin } from '../useStickyPin'
import type { Hop, PathEvent, PathEventsResponse, Window } from '../types'
import { WINDOWS } from '../types'

const ROUTE_PAGE = 25

export function hopLabel(h: Hop | undefined): string {
  if (!h || h.addrs.length === 0) return '*'
  return h.addrs.join(', ')
}

const byTTL = (hops: Hop[], ttl: number) => hops.find((h) => h.ttl === ttl)

// HopDiff renders old and new hop lists side by side, one row per TTL,
// highlighting rows whose responder set changed.
function HopDiff({ oldHops, newHops }: { oldHops: Hop[]; newHops: Hop[] }) {
  const rows = Math.max(0, ...oldHops.map((h) => h.ttl), ...newHops.map((h) => h.ttl))
  return (
    <div className="scroll-x">
      <table className="hop-diff">
        <thead>
          <tr>
            <th className="label">ttl</th>
            <th className="label">change</th>
            <th className="label">old path</th>
            <th className="label">new path</th>
          </tr>
        </thead>
        <tbody>
          {Array.from({ length: rows }, (_, i) => {
            const ttl = i + 1
            const o = byTTL(oldHops, ttl)
            const n = byTTL(newHops, ttl)
            const changed = hopLabel(o) !== hopLabel(n)
            const kind = !o && n ? 'added' : o && !n ? 'removed' : changed ? 'changed' : 'same'
            return (
              <tr key={ttl} className={changed ? `hop-changed hop-${kind}` : ''}>
                <td className="mono">{ttl}</td>
                <td>{changed ? <span className="change-badge">{kind}</span> : <span className="hint">—</span>}</td>
                <td className="mono">{hopLabel(o)}</td>
                <td className="mono">{hopLabel(n)}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function changedHopCount(oldHops: Hop[], newHops: Hop[]): number {
  const ttls = new Set([...oldHops.map((h) => h.ttl), ...newHops.map((h) => h.ttl)])
  let changed = 0
  for (const ttl of ttls) {
    if (hopLabel(oldHops.find((h) => h.ttl === ttl)) !== hopLabel(newHops.find((h) => h.ttl === ttl))) {
      changed++
    }
  }
  return changed
}

function DestinationCell({ e }: { e: PathEvent }) {
  // Destination links: site→site events go to the pair page, external
  // targets to their detail page. A deleted target has no id and stays
  // plain text.
  const dst = e.dst_site ? (
    e.src_site ? (
      <a href={inheritRouteNetwork(`#/pair/${encodeURIComponent(e.src_site)}/${encodeURIComponent(e.dst_site)}`)}>
        {e.dst_site}
      </a>
    ) : (
      e.dst_site
    )
  ) : e.target && e.target_id ? (
    <a href={inheritRouteNetwork(`#/target/${encodeURIComponent(e.target_id)}`)}>{e.target}</a>
  ) : (
    (e.target ?? '?')
  )
  return dst
}

function EventDetails({ e }: { e: PathEvent }) {
  return (
    <div className="path-event-details">
      <div className="path-hashes">
        <span className="hint">Path IDs</span>
        <span className="hash-chip" title={e.old_path_hash}>
          {e.old_path_hash}
        </span>
        <span aria-hidden="true">→</span>
        <span className="hash-chip" title={e.new_path_hash}>
          {e.new_path_hash}
        </span>
      </div>
      <PathGraph
        mode="diff"
        source={e.src_site}
        dest={e.dst_site ?? e.target ?? '?'}
        paths={[
          { key: 'old', hops: e.old_hops, destReached: true },
          { key: 'new', hops: e.new_hops, destReached: true },
        ]}
      />
      <details className="path-id">
        <summary>Table view</summary>
        <HopDiff oldHops={e.old_hops} newHops={e.new_hops} />
      </details>
    </div>
  )
}

export default function Paths({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const { network } = useNetworkFilter()
  const [windowParam] = useRouteParam('window', '24h')
  const [sort] = useRouteParam('sort', 'time')
  const [order] = useRouteParam('order', 'desc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [expandedEvent, setExpandedEvent] = useRouteParam('event')
  const win = windowParam as Window
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const { pinnedID: pinnedEventID, reconcile: reconcilePin } = useStickyPin(expandedEvent)

  const params = new URLSearchParams({
    window: win,
    limit: String(ROUTE_PAGE),
    offset: String(pinnedEventID ? 0 : (page - 1) * ROUTE_PAGE),
    sort,
    order,
  })
  if (network) params.set('network', network)
  // Investigation links carry a stable event ID. Querying that ID keeps
  // the linked row available even when it is not on the default first
  // page; the store's route search includes exact event identities.
  if (pinnedEventID) params.set('q', pinnedEventID)
  else if (queryParam.trim()) params.set('q', queryParam.trim())
  const requestURL = '/api/v1/path-events?' + params.toString()
  const scopeParams = new URLSearchParams({ window: win, limit: '1', offset: '0', sort: 'time', order: 'desc' })
  if (network) scopeParams.set('network', network)
  const scopeURL = '/api/v1/path-events?' + scopeParams.toString()
  const needsScopeRequest = Boolean(pinnedEventID || queryParam.trim())

  // scopeURL and needsScopeRequest derive from inputs already encoded in
  // requestURL (window, network, q), so the request URL alone is the key.
  const {
    data: snapshot,
    error,
    loadedKey,
    reload,
  } = usePolledResource(
    () => {
      const inventoryRequest = apiGet<PathEventsResponse>(requestURL)
      const scopeRequest = needsScopeRequest ? apiGet<PathEventsResponse>(scopeURL) : inventoryRequest
      return Promise.all([inventoryRequest, scopeRequest]).then(([res, scope]) => ({
        res,
        scopeTotal: scope.page?.total ?? scope.events.length,
      }))
    },
    { key: requestURL, onAuthError, logLabel: 'routes' },
  )
  const data = snapshot?.res ?? null
  reconcilePin(Boolean(data?.events.some((event) => event.id === expandedEvent)))
  const scopeTotal = snapshot?.scopeTotal ?? 0
  const loadedRequestURL = typeof loadedKey === 'string' ? loadedKey : ''

  const events = data?.events ?? []
  const pageMeta = data?.page ?? { limit: ROUTE_PAGE, offset: 0, total: events.length, has_more: false }
  const pageCount = Math.max(1, Math.ceil(pageMeta.total / ROUTE_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  const columns: DataTableColumn<PathEvent>[] = [
    {
      key: 'source',
      label: 'Source',
      sortKey: 'source',
      priority: 'identity',
      render: (event) => event.src_site || 'deleted source',
    },
    {
      key: 'destination',
      label: 'Destination',
      sortKey: 'destination',
      priority: 'identity',
      render: (event) => <DestinationCell e={event} />,
    },
    {
      key: 'changes',
      label: 'Changed hops',
      sortKey: 'changes',
      priority: 'primary',
      render: (event) => {
        const count = event.changed_hops ?? changedHopCount(event.old_hops, event.new_hops)
        return `${count} ${count === 1 ? 'hop' : 'hops'}`
      },
    },
    {
      key: 'time',
      label: 'Time',
      sortKey: 'time',
      priority: 'status',
      render: (event) => <span title={fmtTime(event.time)}>{fmtAgo(event.time)}</span>,
    },
    {
      key: 'agent',
      label: 'Agent',
      priority: 'secondary',
      className: 'mono',
      render: (event) => event.agent || 'deleted',
    },
    {
      key: 'network',
      label: 'Network',
      priority: 'secondary',
      render: (event) => event.network || 'unavailable',
    },
  ]

  if (error && !data)
    return (
      <PageError
        title="Routes unavailable"
        subject="routes"
        error={error}
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
        onRetry={() => void reload()}
      />
    )
  if (!data)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading route changes…
      </div>
    )

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <h1>Routes</h1>
        </div>
        <div className="chips">
          <span className="chip">
            In window <span className="mono">{scopeTotal}</span>
          </span>
        </div>
      </div>

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      {pinnedEventID && (
        <div className="data-table-context" role="status">
          <span>Showing the linked route change and its evidence.</span>
          <button type="button" className="secondary-button" onClick={() => setExpandedEvent('', 'replace')}>
            Show route inventory
          </button>
        </div>
      )}

      <div className="view-toolbar">
        <label className="search-field">
          <span className="sr-only">Search routes</span>
          <input
            type="search"
            placeholder={'Search source, destination, or agent — or "lon -> ny"'}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value)
              if (expandedEvent) setExpandedEvent('', 'replace')
            }}
          />
        </label>
        <label className="compact-select">
          <span>Sort</span>
          <select
            value={sort}
            onChange={(event) => updateRouteParams({ sort: event.target.value, page: null, event: null })}
          >
            <option value="time">Time</option>
            <option value="source">Source</option>
            <option value="destination">Destination</option>
            <option value="changes">Changed hops</option>
          </select>
        </label>
        <button
          type="button"
          className="secondary-button"
          aria-label={`Change to ${order === 'asc' ? 'descending' : 'ascending'} order; currently ${order === 'asc' ? 'ascending' : 'descending'}`}
          onClick={() => updateRouteParams({ order: order === 'asc' ? null : 'asc', page: null, event: null })}
        >
          {order === 'asc' ? 'Ascending' : 'Descending'}
        </button>
        <div className="control-group" role="group" aria-label="Window">
          {WINDOWS.map((w) => (
            <button
              key={w}
              className={win === w ? 'active' : ''}
              aria-pressed={win === w}
              onClick={() => updateRouteParams({ window: w === '24h' ? null : w, page: null, event: null })}
            >
              {w}
            </button>
          ))}
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <div>
            <h2>Path changes</h2>
          </div>
          {error ? <span className="hint">Refresh failed, showing last data</span> : null}
        </div>
        <DataTable
          label="Path changes"
          rows={events}
          rowKey={(event) => event.id}
          columns={columns}
          sort={{ key: sort, order: order === 'asc' ? 'asc' : 'desc' }}
          onSortChange={(next) =>
            updateRouteParams({
              sort: next.key === 'time' ? null : next.key,
              order: next.order === 'desc' ? null : next.order,
              page: null,
              event: null,
            })
          }
          page={pageMeta}
          onPageChange={(next) => {
            setExpandedEvent('', 'replace')
            setPage(next)
          }}
          resultLabel="route changes"
          emptyTitle={query ? 'No matching route changes' : 'Routes stable'}
          emptyDescription={
            query
              ? 'Try a different site or agent, or a direction pattern like "lon ->".'
              : 'No path changes in this window.'
          }
          disclosure={{
            expandedKey: expandedEvent || null,
            retainMissing: Boolean(pinnedEventID && loadedRequestURL !== requestURL),
            onExpandedKeyChange: (key) => setExpandedEvent(key ?? '', key === null ? 'replace' : 'push'),
            label: (_event, expanded) => (expanded ? 'Hide evidence' : 'Show evidence'),
            render: (event) => <EventDetails e={event} />,
          }}
        />
        <p className="card-foot">Traceroutes run on a slower cadence than other probes.</p>
      </div>
    </>
  )
}
