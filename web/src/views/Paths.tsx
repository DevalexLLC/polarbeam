import { useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
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

function EventRow({ e }: { e: PathEvent }) {
  const [expanded, setExpanded] = useState(false)
  const count = changedHopCount(e.old_hops, e.new_hops)
  const detailsID = `path-event-${e.id}`
  return (
    <div className="path-event">
      <button
        className="path-event-head"
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
        aria-controls={detailsID}
      >
        <span className="mono">
          {e.src_site} → {e.dst_site ?? e.target ?? '?'}
        </span>
        <span className="path-summary">
          {count} {count === 1 ? 'hop' : 'hops'} changed
        </span>
        <span className="hint" title={fmtTime(e.time)}>
          {fmtAgo(e.time)}
        </span>
        <span className="incident-toggle path-toggle">
          {expanded ? 'Hide details' : 'View details'} <span aria-hidden="true">{expanded ? '−' : '+'}</span>
        </span>
      </button>
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
          <HopDiff oldHops={e.old_hops} newHops={e.new_hops} />
        </div>
      )}
    </div>
  )
}

export default function Paths({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [win, setWin] = useState<Window>('24h')
  const [data, setData] = useState<PathEventsResponse | null>(null)
  const [error, setError] = useState('')
  const [query, setQuery] = useState('')
  const [visibleLimit, setVisibleLimit] = useState(ROUTE_PAGE)

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

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const events = data?.events ?? []
    if (!needle) return events
    // "src -> dst" filters by direction; either side may be empty ("lon ->",
    // "-> ny"). "→" is accepted so a copied row header works as a query.
    const parts = needle.split(/->|→/)
    if (parts.length > 1) {
      const src = parts[0].trim()
      const dst = parts.slice(1).join('->').trim()
      return events.filter((event) => {
        const srcMatch = !src || event.src_site.toLowerCase().includes(src)
        const dstMatch =
          !dst ||
          [event.dst_site, event.target].filter(Boolean).some((value) => String(value).toLowerCase().includes(dst))
        return srcMatch && dstMatch
      })
    }
    return events.filter((event) =>
      [event.src_site, event.dst_site, event.target, event.agent]
        .filter(Boolean)
        .some((value) => String(value).toLowerCase().includes(needle)),
    )
  }, [data, query])

  useEffect(() => setVisibleLimit(ROUTE_PAGE), [query, win])

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
            in window <span className="mono">{data.events.length}</span>
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
        <div className="control-group" role="group" aria-label="Window">
          {WINDOWS.map((w) => (
            <button key={w} className={win === w ? 'active' : ''} aria-pressed={win === w} onClick={() => setWin(w)}>
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
            {visible.slice(0, visibleLimit).map((e) => (
              <EventRow key={e.id} e={e} />
            ))}
            {visibleLimit < visible.length && (
              <div className="progressive-footer">
                <span className="hint">
                  Showing {visibleLimit} of {visible.length} route changes
                </span>
                <button
                  className="secondary-button"
                  onClick={() => setVisibleLimit((limit) => Math.min(visible.length, limit + ROUTE_PAGE))}
                >
                  Show {Math.min(ROUTE_PAGE, visible.length - visibleLimit)} more
                </button>
              </div>
            )}
          </>
        )}
      </div>
    </>
  )
}
