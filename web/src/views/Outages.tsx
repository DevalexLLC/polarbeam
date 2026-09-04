import { useEffect, useMemo, useState } from 'react'
import IncidentTimeline, {
  bucketRangeLabel,
  gridWithYear,
  gridWithZone,
  overlapsBucket,
  timelineGrid,
} from '../components/IncidentTimeline'
import DisclosureChevron from '../components/DisclosureChevron'
import PageError from '../components/PageError'
import { fmtAgo, fmtTime } from '../format'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import {
  incidentAgentHref,
  incidentPairHref,
  incidentTargetHref,
  inheritRouteNetwork,
  routeEventHref,
  updateRouteParams,
} from '../routeState'
import { useTimezone } from '../timezone'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { OutageEvent, OutagesResponse, Window } from '../types'
import { WINDOWS } from '../types'

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
  const source = o.src_site || o.agent || `agent ${o.agent_id.slice(0, 8)}`
  if (o.kind === 'agent_offline') return `${source} · ${o.agent || 'deleted agent'}`
  return `${source} → ${o.dst_site ?? o.target ?? (o.target_id ? `target ${o.target_id.slice(0, 8)}` : '?')}`
}

function InvestigationLinks({ event, win }: { event: OutageEvent; win: Window }) {
  const agentLabel = event.agent || `Deleted agent ${event.agent_id.slice(0, 8)}`
  const agentHref = incidentAgentHref(event.agent_id, event.probe_id, event.agent)
  const agent = agentHref ? <a href={agentHref}>Agent {agentLabel}</a> : <span>Agent {agentLabel}</span>
  const pairHref = incidentPairHref(event.src_site, event.dst_site, win)
  const pair = pairHref ? (
    <a href={pairHref}>
      Pair {event.src_site} → {event.dst_site}
    </a>
  ) : event.src_site || event.dst_site ? (
    <span>
      Pair {event.src_site || '?'} → {event.dst_site || '?'}
    </span>
  ) : null
  const targetHref = incidentTargetHref(event.target_id, event.probe_id, event.target, win)
  const targetLink = event.target ? (
    targetHref ? (
      <a href={targetHref}>Target {event.target}</a>
    ) : (
      <span>Target {event.target}</span>
    )
  ) : event.target_id ? (
    <span>Deleted target {event.target_id.slice(0, 8)}</span>
  ) : null

  return (
    <div className="incident-investigation">
      <div className="incident-resource-links">
        {agent}
        {pair}
        {targetLink}
      </div>
      <div className="incident-route-links">
        <span className="label">Related route changes</span>
        {event.route_events.length === 0 ? (
          <span className="hint">No related route changes within 15 minutes of opening or resolution.</span>
        ) : (
          event.route_events.map((route) => (
            <a key={route.id} href={routeEventHref(route.id, win)} title={fmtTime(route.time)}>
              {route.src_site || route.agent || 'Unknown source'} → {route.dst_site ?? route.target ?? 'unknown target'}{' '}
              · {fmtAgo(route.time)}
            </a>
          ))
        )}
      </div>
    </div>
  )
}

interface IncidentGroup {
  id: string
  key: string
  open: boolean
  kind: OutageEvent['kind']
  probe: string
  error: string | null
  events: OutageEvent[]
}

function incidentSelectionID(key: string): string {
  let left = 2_166_136_261
  let right = 3_332_046_959
  for (let index = 0; index < key.length; index++) {
    const code = key.charCodeAt(index)
    left = Math.imul(left ^ code, 16_777_619)
    right = Math.imul(right ^ code, 2_246_822_519)
  }
  return `i${(left >>> 0).toString(36)}${(right >>> 0).toString(36)}`
}

function groupIncidents(events: OutageEvent[]): IncidentGroup[] {
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

function IncidentGroupRow({
  group,
  win,
  expanded,
  onToggle,
}: {
  group: IncidentGroup
  win: Window
  expanded: boolean
  onToggle: () => void
}) {
  const [detailLimit, setDetailLimit] = useState(INCIDENT_DETAIL_PAGE)
  const firstOpened = group.events.reduce(
    (earliest, event) => (Date.parse(event.opened_at) < Date.parse(earliest) ? event.opened_at : earliest),
    group.events[0].opened_at,
  )
  const label =
    group.kind === 'agent_offline'
      ? 'Agents offline'
      : group.kind === 'probe_degraded'
        ? 'Probes degraded'
        : 'Probe failures'
  const affectedCount = new Set(group.events.map(target)).size
  const detailsID = `incident-${group.id}`

  return (
    <article className={'incident-group' + (group.open ? ' incident-active' : '')}>
      <button
        className="incident-summary"
        onClick={() => {
          onToggle()
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
          {expanded ? 'Hide details' : 'View details'}
          <DisclosureChevron expanded={expanded} />
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
              <InvestigationLinks event={event} win={win} />
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
  const { mode } = useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [windowParam] = useRouteParam('window', '24h')
  const [statusParam] = useRouteParam('status', 'active')
  const [query, setQuery] = useRouteSearch()
  const [selectedSlice, setSelectedSlice] = useRouteNumber('slice', 0)
  const [expandedIncident, setExpandedIncident] = useRouteParam('incident')
  const win = windowParam as Window
  const filter = statusParam as IncidentFilter
  const selectedBucket = selectedSlice || null
  const { data, error, lastLoadedAt, reload } = usePolledResource<OutagesResponse>(
    `/api/v1/outages?window=${win}&include_routes=true`,
    { onAuthError, logLabel: 'incidents' },
  )
  const snapshotWin = data && (WINDOWS as readonly string[]).includes(data.window) ? (data.window as Window) : win
  // The timeline's "now" is fetch time, so its bucket grid only shifts on
  // the 30s poll, never on a re-render (hover, expand, timezone toggle).
  // Anchor it at the server clock the window was evaluated against — a
  // skewed browser clock would shift the grid and hide returned incidents
  // off either edge.
  const fetchedAt = useMemo(() => {
    if (!data) return 0
    const serverNow = Date.parse(data.now)
    return Number.isFinite(serverNow) ? serverNow : (lastLoadedAt?.getTime() ?? 0)
  }, [data, lastLoadedAt])

  // The global top-bar network filter scopes everything on this view —
  // groups, timeline, chips, and button counts all derive from this subset.
  const { network } = useNetworkFilter()
  const events = useMemo(
    () => (data?.outages ?? []).filter((o) => matchesNetworkFilter(network, o.network)),
    [data, network],
  )
  const activeEvents = events.filter((o) => o.closed_at == null)
  const activeCount = activeEvents.length
  const resolvedCount = events.length - activeCount
  // Selection is time-addressed (bucket start ms) so a poll that shifts the
  // grid keeps filtering the same slice; it only clears once the bucket
  // leaves the window entirely — the HealthStrip pinned-slot reasoning.
  // The chart's window comes from the SNAPSHOT, not the selector: on a
  // window switch the stale response keeps its own range and label until
  // the new one arrives (or the fetch fails), instead of 24h of data being
  // spread across a chart claiming a year with everything else zero.
  const timeline = useMemo(() => {
    if (!fetchedAt || !data) return null
    const grid = timelineGrid(snapshotWin, fetchedAt)
    const bucket =
      selectedBucket != null && selectedBucket >= grid.startMs && selectedBucket < grid.endMs ? selectedBucket : null
    return { grid, bucket, win: snapshotWin }
  }, [data, snapshotWin, fetchedAt, selectedBucket])
  const bucket = timeline?.bucket ?? null
  const groups = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const filtered = events.filter((event) => {
      const active = event.closed_at == null
      if (filter === 'active' && !active) return false
      if (filter === 'resolved' && active) return false
      if (timeline?.bucket != null && !overlapsBucket(event, timeline.bucket, timeline.grid.bucketMs, fetchedAt))
        return false
      if (!needle) return true
      return [target(event), event.kind, event.probe_type, event.error]
        .filter(Boolean)
        .some((value) => String(value).toLowerCase().includes(needle))
    })
    return groupIncidents(filtered)
  }, [events, filter, query, timeline, fetchedAt])

  useEffect(() => {
    if (selectedBucket != null && timeline && timeline.bucket == null) setSelectedSlice(0, 'replace')
  }, [selectedBucket, setSelectedSlice, timeline])

  useEffect(() => {
    if (!expandedIncident || !data) return
    if (groupIncidents(events).some((group) => group.id === expandedIncident)) return
    setExpandedIncident('', 'replace')
  }, [data, events, expandedIncident, setExpandedIncident])
  const sliceHasHiddenIncidents =
    groups.length === 0 &&
    bucket != null &&
    timeline != null &&
    events.some((event) => overlapsBucket(event, bucket, timeline.grid.bucketMs, fetchedAt))

  if (error && !data)
    return (
      <PageError
        title="Incidents unavailable"
        subject="incidents"
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
        Loading incidents…
      </div>
    )

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <h1>Incidents</h1>
        </div>
      </div>

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar incident-toolbar">
        <div className="control-group" role="group" aria-label="Incident status">
          <button
            className={filter === 'active' ? 'active' : ''}
            aria-pressed={filter === 'active'}
            onClick={() => updateRouteParams({ status: null, page: null, incident: null })}
          >
            Active {activeCount}
          </button>
          <button
            className={filter === 'all' ? 'active' : ''}
            aria-pressed={filter === 'all'}
            onClick={() => updateRouteParams({ status: 'all', page: null, incident: null })}
          >
            All {events.length}
          </button>
          <button
            className={filter === 'resolved' ? 'active' : ''}
            aria-pressed={filter === 'resolved'}
            onClick={() => updateRouteParams({ status: 'resolved', page: null, incident: null })}
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
            <button
              key={w}
              className={win === w ? 'active' : ''}
              aria-pressed={win === w}
              onClick={() => {
                updateRouteParams({ window: w === '24h' ? null : w, slice: null, incident: null })
              }}
            >
              {w}
            </button>
          ))}
        </div>
      </div>

      {timeline && (
        <section className="card chart-card incident-timeline-card">
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
            {/* The 500 cap is applied server-side BEFORE the network
                filter, so the note watches the unfiltered count. */}
            Each bar counts the incidents in its slice, colored by kind, with resolved ones muted. Click a bar to filter
            the list below.
            {data.outages.filter((o) => o.closed_at != null).length >= 500
              ? ' Resolved incidents past the newest 500 are omitted.'
              : ''}
            {data.truncated ? ' The oldest open incidents are omitted (server cap).' : ''}
          </p>
        </section>
      )}

      <section className="card incident-card">
        <div className="card-head">
          <div>
            <h2>
              {filter === 'active'
                ? 'Active incident groups'
                : filter === 'resolved'
                  ? 'Resolved incident groups'
                  : 'Incident groups'}
            </h2>
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
            {/* The timeline deliberately charts the whole window, so a
                selected slice can hold only incidents the status filter or
                search hides — name the real culprit instead of blaming the
                time filter. */}
            <strong>
              {bucket != null
                ? sliceHasHiddenIncidents
                  ? 'Incidents in this slice are filtered out'
                  : 'No incidents in this slice'
                : query
                  ? 'No matching incidents'
                  : filter === 'active'
                    ? 'All clear'
                    : 'No incident history'}
            </strong>
            <span>
              {bucket != null
                ? sliceHasHiddenIncidents
                  ? 'The status filter or search hides them — switch to All or clear the search.'
                  : 'Clear the time filter or pick another bar.'
                : query
                  ? 'Try a different target, probe, or error.'
                  : 'The network watch continues.'}
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
          A failing group opens after 3 consecutive failures of one probe series and a degraded group after 3
          consecutive critical-threshold breaches. Both resolve after 3 consecutive clean results.
        </p>
      </section>
    </>
  )
}
