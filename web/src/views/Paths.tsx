import { useEffect, useMemo, useRef, useState } from 'react'
import { apiGet } from '../api'
import DisclosureChevron from '../components/DisclosureChevron'
import PathGraph from '../components/PathGraph'
import { fmtAgo, fmtTime } from '../format'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { Hop, PathEvent, PathEventsResponse, Window } from '../types'
import { WINDOWS } from '../types'

const POLL_MS = 30_000
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
            <th className="eyebrow">ttl</th>
            <th className="eyebrow">change</th>
            <th className="eyebrow">old path</th>
            <th className="eyebrow">new path</th>
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

function EventRow({ e, expanded, onToggle }: { e: PathEvent; expanded: boolean; onToggle: () => void }) {
  const count = changedHopCount(e.old_hops, e.new_hops)
  const detailsID = `path-event-${e.id}`
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
  return (
    <div className="path-event">
      {/* Like the Agents rows: the header is a convenience click target
          only, while the real disclosure semantics live on the View
          details button — a link nested in a button role would be
          flattened out of the accessibility tree. The a11y rules below
          are satisfied by that button; the row click just duplicates it. */}
      {/* oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-static-element-interactions */}
      <div
        className="path-event-head"
        onClick={(ev) => {
          if ((ev.target as Element).closest('button, a')) return
          onToggle()
        }}
      >
        <span className="mono">
          {e.src_site} → {dst}
        </span>
        <span className="path-summary">
          {count} {count === 1 ? 'hop' : 'hops'} changed
        </span>
        <span className="hint" title={fmtTime(e.time)}>
          {fmtAgo(e.time)}
        </span>
        <button
          type="button"
          className="incident-toggle path-toggle"
          aria-expanded={expanded}
          aria-controls={detailsID}
          onClick={onToggle}
        >
          {expanded ? 'Hide details' : 'View details'}
          <DisclosureChevron expanded={expanded} />
        </button>
      </div>
      {expanded && (
        <div id={detailsID} className="path-event-details">
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
      )}
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
  const [data, setData] = useState<PathEventsResponse | null>(null)
  const [error, setError] = useState('')
  const [query, setQuery] = useRouteSearch()

  useEffect(() => {
    let cancelled = false
    const load = () =>
      apiGet<PathEventsResponse>(`/api/v1/path-events?window=${win}`)
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setError('')
          }
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
  }, [win, onAuthError])

  // The global top-bar network filter narrows first (the header count reads
  // this subset too), then the search narrows the listed rows.
  const events = useMemo(
    () => (data?.events ?? []).filter((e) => matchesNetworkFilter(network, e.network)),
    [data, network],
  )
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase()
    let filtered = events
    // "src -> dst" filters by direction; either side may be empty ("lon ->",
    // "-> ny"). "→" is accepted so a copied row header works as a query.
    const parts = needle.split(/->|→/)
    if (needle && parts.length > 1) {
      const src = parts[0].trim()
      const dst = parts.slice(1).join('->').trim()
      filtered = events.filter((event) => {
        const srcMatch = !src || event.src_site.toLowerCase().includes(src)
        const dstMatch =
          !dst ||
          [event.dst_site, event.target].filter(Boolean).some((value) => String(value).toLowerCase().includes(dst))
        return srcMatch && dstMatch
      })
    } else if (needle) {
      filtered = events.filter((event) =>
        [event.src_site, event.dst_site, event.target, event.agent]
          .filter(Boolean)
          .some((value) => String(value).toLowerCase().includes(needle)),
      )
    }
    // oxlint-disable-next-line unicorn/no-array-sort
    return [...filtered].sort((a, b) => {
      const value = (event: PathEvent): string | number => {
        if (sort === 'source') return event.src_site
        if (sort === 'destination') return event.dst_site ?? event.target ?? ''
        if (sort === 'changes') return changedHopCount(event.old_hops, event.new_hops)
        return Date.parse(event.time)
      }
      const x = value(a)
      const y = value(b)
      const comparison = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      return order === 'asc' ? comparison : -comparison
    })
  }, [events, order, query, sort])

  const pageCount = Math.max(1, Math.ceil(visible.length / ROUTE_PAGE))
  const positionedEvent = useRef<string | null>(null)
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])
  useEffect(() => {
    if (!expandedEvent || !data) return
    if (events.some((event) => event.id === expandedEvent)) return
    setExpandedEvent('', 'replace')
  }, [data, events, expandedEvent, setExpandedEvent])
  useEffect(() => {
    if (!expandedEvent) {
      positionedEvent.current = null
      return
    }
    if (positionedEvent.current === expandedEvent) return
    const index = visible.findIndex((event) => event.id === expandedEvent)
    if (index === -1) return
    positionedEvent.current = expandedEvent
    setPage(Math.floor(index / ROUTE_PAGE) + 1, 'replace')
  }, [expandedEvent, setPage, visible])

  if (error && !data)
    return (
      <div className="state-panel state-error">
        <h1>Routes unavailable</h1>
        <p>{error}</p>
      </div>
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
          <div className="eyebrow">Operations</div>
          <h1>Routes</h1>
          <p>Traceroute changes that may explain latency shifts and connectivity failures.</p>
        </div>
        <div className="chips">
          <span className="chip">
            in window <span className="mono">{events.length}</span>
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
          <span className="sr-only">Search routes</span>
          <input
            type="search"
            placeholder={'Search source, destination, or agent — or "lon -> ny"'}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
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
          onClick={() => updateRouteParams({ order: order === 'asc' ? null : 'asc', page: null })}
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
            <span className="eyebrow">Change log</span>
            <h2>Path changes</h2>
          </div>
          <span className="hint">
            Traceroutes run on a slower cadence than other probes
            {error ? ' · refresh failed, showing last data' : ''}
          </span>
        </div>
        {visible.length === 0 ? (
          <div className="empty-state">
            <strong>{query ? 'No matching route changes' : 'Routes stable'}</strong>
            <span>
              {query
                ? 'Try a different site or agent, or a direction pattern like "lon ->".'
                : 'No path changes in this window.'}
            </span>
          </div>
        ) : (
          <>
            {visible.slice((page - 1) * ROUTE_PAGE, page * ROUTE_PAGE).map((e) => (
              <EventRow
                key={e.id}
                e={e}
                expanded={expandedEvent === e.id}
                onToggle={() => setExpandedEvent(expandedEvent === e.id ? '' : e.id)}
              />
            ))}
            {pageCount > 1 && (
              <div className="progressive-footer">
                <span className="hint">
                  Page {page} of {pageCount} · {visible.length} route changes
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
        )}
      </div>
    </>
  )
}
