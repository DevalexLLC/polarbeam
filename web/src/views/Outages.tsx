import { useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { OutageEvent, OutagesResponse, Window } from '../types'
import { WINDOWS } from '../types'

const POLL_MS = 30_000
const INCIDENT_DETAIL_PAGE = 10
type IncidentFilter = 'active' | 'all' | 'resolved'

function fmtDuration(openedAt: string, closedAt: string | null): string {
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
function normalizeError(error: string | null): string {
  if (!error) return 'No error detail'
  const lower = error.toLowerCase()
  if (lower.includes('connection refused')) return 'Connection refused'
  if (lower.includes('timeout') || lower.includes('deadline exceeded')) return 'Timed out'
  if (lower.includes('no such host')) return 'Host not found'
  return error
}

function errorSummary(error: string | null): string {
  const normalized = normalizeError(error)
  return normalized.length > 72 ? normalized.slice(0, 69) + '…' : normalized
}

function target(o: OutageEvent): string {
  return o.kind === 'agent_offline' ? `${o.src_site} · ${o.agent}` : `${o.src_site} → ${o.dst_site ?? o.target ?? '?'}`
}

interface IncidentGroup {
  key: string
  open: boolean
  kind: OutageEvent['kind']
  probe: string
  error: string | null
  events: OutageEvent[]
}

function groupIncidents(events: OutageEvent[]): IncidentGroup[] {
  const groups = new Map<string, IncidentGroup>()
  for (const event of events) {
    const open = event.closed_at == null
    const key = [open ? 'active' : 'resolved', event.kind, event.probe_type ?? '', normalizeError(event.error)].join(
      '\u0000',
    )
    const group = groups.get(key) ?? {
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

function IncidentGroupRow({ group }: { group: IncidentGroup }) {
  const [expanded, setExpanded] = useState(false)
  const [detailLimit, setDetailLimit] = useState(INCIDENT_DETAIL_PAGE)
  const firstOpened = group.events.reduce(
    (earliest, event) => (Date.parse(event.opened_at) < Date.parse(earliest) ? event.opened_at : earliest),
    group.events[0].opened_at,
  )
  const label = group.kind === 'agent_offline' ? 'Agents offline' : 'Probe failures'
  const affectedCount = new Set(group.events.map(target)).size
  let idHash = 0
  for (let i = 0; i < group.key.length; i++) idHash = (idHash * 31 + group.key.charCodeAt(i)) >>> 0
  const detailsID = `incident-${idHash}`

  return (
    <article className={'incident-group' + (group.open ? ' incident-active' : '')}>
      <button
        className="incident-summary"
        onClick={() => {
          setExpanded(!expanded)
          if (expanded) setDetailLimit(INCIDENT_DETAIL_PAGE)
        }}
        aria-expanded={expanded}
        aria-controls={detailsID}
      >
        <span className={'status-marker ' + (group.open ? 'status-marker-down' : 'status-marker-muted')} />
        <span className="incident-primary">
          <strong>{label}</strong>
          <small>{errorSummary(group.error)}</small>
        </span>
        <span className="incident-impact">
          <strong>{affectedCount}</strong>
          <small>{affectedCount === 1 ? 'affected target' : 'affected targets'}</small>
        </span>
        <span className="incident-meta">
          <strong>{group.probe}</strong>
          <small>{group.open ? `active for ${fmtDuration(firstOpened, null)}` : 'resolved'}</small>
        </span>
        <span className="incident-toggle">
          {expanded ? 'Hide details' : 'View details'} <span aria-hidden="true">{expanded ? '−' : '+'}</span>
        </span>
      </button>
      {expanded && (
        <div id={detailsID} className="incident-details">
          <div className="incident-detail-head">
            <span>Target</span>
            <span>Started</span>
            <span>Duration</span>
            <span>Detail</span>
          </div>
          {group.events.slice(0, detailLimit).map((event) => (
            <div className="incident-instance" key={event.id}>
              <strong className="mono">{target(event)}</strong>
              <span title={fmtTime(event.opened_at)}>{fmtAgo(event.opened_at)}</span>
              <span>{fmtDuration(event.opened_at, event.closed_at)}</span>
              <code title={event.error ?? undefined}>{event.error || 'No error detail'}</code>
            </div>
          ))}
          {detailLimit < group.events.length && (
            <div className="progressive-footer">
              <span className="hint">
                Showing {detailLimit} of {group.events.length} targets
              </span>
              <button
                className="secondary-button"
                onClick={() => setDetailLimit((limit) => Math.min(group.events.length, limit + INCIDENT_DETAIL_PAGE))}
              >
                Show {Math.min(INCIDENT_DETAIL_PAGE, group.events.length - detailLimit)} more
              </button>
            </div>
          )}
        </div>
      )}
    </article>
  )
}

export default function Outages({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [win, setWin] = useState<Window>('24h')
  const [filter, setFilter] = useState<IncidentFilter>('active')
  const [query, setQuery] = useState('')
  const [data, setData] = useState<OutagesResponse | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = () =>
      apiGet<OutagesResponse>(`/api/v1/outages?window=${win}`)
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

  const activeEvents = data?.outages.filter((o) => o.closed_at == null) ?? []
  const activeCount = activeEvents.length
  const resolvedCount = (data?.outages.length ?? 0) - activeCount
  // The API emits one event per failing series (probe × direction), so a
  // target failing on two probes is two events but one affected target.
  const activeTargetCount = new Set(activeEvents.map(target)).size
  const groups = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const filtered = (data?.outages ?? []).filter((event) => {
      const active = event.closed_at == null
      if (filter === 'active' && !active) return false
      if (filter === 'resolved' && active) return false
      if (!needle) return true
      return [target(event), event.kind, event.probe_type, event.error]
        .filter(Boolean)
        .some((value) => String(value).toLowerCase().includes(needle))
    })
    return groupIncidents(filtered)
  }, [data, filter, query])

  if (error && !data)
    return (
      <div className="state-panel state-error">
        <h1>Incidents unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!data)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading incidents…
      </div>
    )

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Operations</div>
          <h1>Incidents</h1>
          <p>Correlated failures requiring attention, followed by resolved history.</p>
        </div>
        <div className="chips">
          <span className={'chip' + (activeCount > 0 ? ' chip-alert' : '')}>
            {activeCount > 0 && <span className="dot swatch status-down" />}Active targets{' '}
            <span className="mono">{activeTargetCount}</span>
          </span>
          <span className="chip">
            In window <span className="mono">{data.outages.length}</span>
          </span>
        </div>
      </div>

      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar incident-toolbar">
        <div className="control-group" role="group" aria-label="Incident status">
          <button
            className={filter === 'active' ? 'active' : ''}
            aria-pressed={filter === 'active'}
            onClick={() => setFilter('active')}
          >
            Active {activeCount}
          </button>
          <button
            className={filter === 'all' ? 'active' : ''}
            aria-pressed={filter === 'all'}
            onClick={() => setFilter('all')}
          >
            All {data.outages.length}
          </button>
          <button
            className={filter === 'resolved' ? 'active' : ''}
            aria-pressed={filter === 'resolved'}
            onClick={() => setFilter('resolved')}
          >
            Resolved {resolvedCount}
          </button>
        </div>
        <label className="search-field">
          <span className="sr-only">Search incidents</span>
          <input
            type="search"
            placeholder="Search targets or errors"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <div className="control-group" role="group" aria-label="Time window">
          {WINDOWS.map((w) => (
            <button key={w} className={win === w ? 'active' : ''} aria-pressed={win === w} onClick={() => setWin(w)}>
              {w}
            </button>
          ))}
        </div>
      </div>

      <section className="card incident-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Correlated by state, probe, and error</span>
            <h2>
              {filter === 'active'
                ? 'Active incident groups'
                : filter === 'resolved'
                  ? 'Resolved incident groups'
                  : 'Incident groups'}
            </h2>
          </div>
          <span className="hint">Opens after 3 failures · resolves after 3 successes</span>
        </div>
        {groups.length === 0 ? (
          <div className="empty-state">
            <strong>
              {query ? 'No matching incidents' : filter === 'active' ? 'All clear' : 'No incident history'}
            </strong>
            <span>{query ? 'Try a different target, probe, or error.' : 'The network watch continues.'}</span>
          </div>
        ) : (
          groups.map((group) => <IncidentGroupRow key={group.key} group={group} />)
        )}
      </section>
    </>
  )
}
