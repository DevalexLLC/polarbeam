import { useEffect, useMemo } from 'react'
import IncidentTimeline, {
  bucketRangeLabel,
  gridWithYear,
  gridWithZone,
  overlapsBucket,
  timelineGrid,
} from '../components/IncidentTimeline'
import IncidentGroupRow from '../components/IncidentGroupRow'
import PageError from '../components/PageError'
import { groupIncidents, incidentTarget } from '../incidentGroups'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { OutagesResponse, Window } from '../types'
import { WINDOWS } from '../types'

type IncidentFilter = 'active' | 'all' | 'resolved'

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
      return [incidentTarget(event), event.kind, event.probe_type, event.error]
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
            {/* Both caps are applied server-side BEFORE the network
                filter, so the notes follow the server flags. */}
            Each bar counts the incidents in its slice, colored by kind, with resolved ones muted. Click a bar to filter
            the list below.
            {data.history_truncated ? ' Resolved incidents past the newest 500 are omitted.' : ''}
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
