import { useState } from 'react'
import DisclosureChevron from './DisclosureChevron'
import { fmtAgo, fmtTime } from '../format'
import { errorSummary, fmtDuration, incidentTarget, type IncidentGroup } from '../incidentGroups'
import { incidentAgentHref, incidentPairHref, incidentTargetHref, routeEventHref } from '../routeState'
import type { OutageEvent, Window } from '../types'

// One incident group — the expandable article the Incidents page and the
// site page both render. Detail rows page progressively.
export const INCIDENT_DETAIL_PAGE = 10

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

export default function IncidentGroupRow({
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
  const affectedCount = new Set(group.events.map(incidentTarget)).size
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
              <strong className="mono">{incidentTarget(event)}</strong>
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
