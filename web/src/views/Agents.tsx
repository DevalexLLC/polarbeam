import { useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import HealthStrip, { stripStats, UptimeValue } from '../components/HealthStrip'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { AgentInfo, AgentProbeHealth, AgentProbeHealthResponse, AgentsResponse } from '../types'

const POLL_MS = 30_000
// Agents renew their cert at 2/3 lifetime (10 days left on the 30-day
// cert), so the warn threshold must be BELOW the renewal point or every
// healthy agent is flagged from issuance. 7 days = renewal has been
// failing for 3+ days. Mirror of CERT_WARN_DAYS in Overview.tsx.
const CERT_WARN_DAYS = 7

type Health = 'ok' | 'degraded' | 'down' | 'stale'
type FleetFilter = 'all' | 'attention' | 'healthy'

// Health folds the server's signals in severity order: an unusable cert
// (revoked or expired) or an open agent_offline outage beats everything;
// failing probe series degrade; an agent that has never connected is
// stale, not broken. Expiry is checked here too because the offline sweep
// takes minutes to notice a cut-off agent — the row must not say ok while
// the certificate cell says expired.
function health(a: AgentInfo): { status: Health; label: string } {
  if (a.cert_revoked_at) return { status: 'down', label: 'revoked' }
  if (a.cert_not_after && certDaysLeft(a.cert_not_after) < 0) return { status: 'down', label: 'cert expired' }
  if (a.offline) return { status: 'down', label: 'offline' }
  if (!a.last_seen_at) return { status: 'stale', label: 'never seen' }
  if (a.probes_failing > 0) return { status: 'degraded', label: 'degraded' }
  return { status: 'ok', label: 'healthy' }
}

function certDaysLeft(notAfter: string): number {
  return Math.floor((new Date(notAfter).getTime() - Date.now()) / 86_400_000)
}

// Attention = not healthy OR spool drops in the last 24 h. Must stay in
// lockstep with Overview's attentionReason so the fleet card there and the
// Attention filter here always agree on which agents need a look. Drops
// stay out of health() — they don't make the agent's link unhealthy, they
// mean data was lost.
const DROP_ATTENTION_MS = 24 * 60 * 60 * 1000

function needsAttention(a: AgentInfo): boolean {
  if (health(a).status !== 'ok') return true
  // CertCell renders the ≤CERT_WARN_DAYS window in degraded styling; a row
  // carrying that warning must not sort under Healthy — renewal is
  // actionable now, not at expiry.
  if (a.cert_not_after && certDaysLeft(a.cert_not_after) <= CERT_WARN_DAYS) return true
  return a.last_dropped_at != null && Date.now() - Date.parse(a.last_dropped_at) < DROP_ATTENTION_MS
}

function CertCell({ a }: { a: AgentInfo }) {
  if (a.cert_revoked_at)
    return (
      <span className="status-text-down" title={fmtTime(a.cert_revoked_at)}>
        revoked {fmtAgo(a.cert_revoked_at)}
      </span>
    )
  if (!a.cert_not_after) return <span className="hint">—</span>
  const days = certDaysLeft(a.cert_not_after)
  if (days < 0)
    return (
      <span className="status-text-down" title={fmtTime(a.cert_not_after)}>
        expired
      </span>
    )
  if (days <= CERT_WARN_DAYS)
    return (
      <span className="status-text-degraded" title={fmtTime(a.cert_not_after)}>
        expires in {days}d
      </span>
    )
  return (
    <span className="muted" title={fmtTime(a.cert_not_after)}>
      {days}d left
    </span>
  )
}

// probeSortKey orders the expanded detail: failing series first, then by
// the same type + destination text the label shows.
function probeSortKey(p: AgentProbeHealth): string {
  return `${p.failing ? 0 : 1} ${p.type} ${p.dst_site ?? p.target ?? ''}`
}

function ProbeLabel({ p }: { p: AgentProbeHealth }) {
  return (
    <div className="probe-strip-label">
      <span className="mono">{p.type}</span>
      {p.target_kind === 'external' ? (
        <>
          <span>{p.target ?? 'deleted target'}</span>
          <span className="chip">external</span>
        </>
      ) : p.target_kind === 'agent' ? (
        <span>→ {p.dst_site ?? 'deleted site'}</span>
      ) : (
        <span className="hint">deleted target</span>
      )}
      {p.type === 'traceroute' && (
        <span className="hint" title="Traceroute status is destination reached; excluded from agent uptime ratios">
          path watch
        </span>
      )}
    </div>
  )
}

// The expanded per-probe detail: one 24 h strip per probe series, failing
// first, so the row's "N of M failing" is attributable to specific probes —
// an external target down for maintenance reads differently from a broken
// site link.
function ProbeDetail({ detail, error }: { detail: AgentProbeHealthResponse | null; error: string }) {
  if (!detail && error)
    return (
      <div className="inline-alert" role="status">
        Probe detail unavailable: {error}
      </div>
    )
  if (!detail)
    return (
      <div className="probe-strip-loading" role="status">
        <span className="state-spinner" /> Loading probe detail…
      </div>
    )
  if (detail.probes.length === 0)
    return (
      <div className="empty-state">
        <strong>No probe series yet</strong>
        <span>This agent has never reported a result.</span>
      </div>
    )
  const bucketS = detail.bucket_s || 1800
  const nowS = Date.now() / 1000
  // Sorting a freshly-spread copy, same as Outages' group sort (toSorted
  // needs a newer TS lib target than the build uses).
  // oxlint-disable-next-line unicorn/no-array-sort
  const probes = [...detail.probes].sort((x, y) => probeSortKey(x).localeCompare(probeSortKey(y)))
  return (
    <div className="probe-strip-list">
      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      {probes.map((p) => {
        const s = stripStats(p.buckets, bucketS, nowS)
        return (
          <div key={p.probe_id} className="probe-strip-row">
            <ProbeLabel p={p} />
            <HealthStrip buckets={s.inWindow} bucketS={bucketS} endS={s.endS} label={s.stripLabel} />
            <div className="probe-strip-uptime">
              <UptimeValue uptime={s.uptime} partial={s.partial} stripLabel={s.stripLabel} />
            </div>
            {p.failing && (
              <div className="probe-strip-error">
                <span className="status-text-down">failing since {fmtAgo(p.open_since)}</span>
                {p.error && (
                  <code className="probe-strip-error-text" title={p.error}>
                    {p.error}
                  </code>
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

function Row({
  a,
  expanded,
  onToggle,
  detail,
  detailError,
}: {
  a: AgentInfo
  expanded: boolean
  onToggle: () => void
  detail: AgentProbeHealthResponse | null
  detailError: string
}) {
  const h = health(a)
  return (
    <>
      <tr
        className="agent-row"
        onClick={(e) => {
          // The whole row is a convenience click target, but never steal
          // clicks meant for real controls inside it.
          if ((e.target as Element).closest('button, a')) return
          onToggle()
        }}
      >
        <td data-label="Status">
          <span className={'status-text-' + h.status}>
            <span className={'dot swatch status-' + h.status} /> {h.label}
          </span>
        </td>
        <td className="mono" data-label="Agent" title={`enrolled ${fmtTime(a.enrolled_at)} · ${a.id}`}>
          {a.site} · {a.hostname}
        </td>
        <td className="mono" data-label="Address">
          {a.probe_address || '—'}
        </td>
        <td className="mono" data-label="Version">
          {a.version || '—'}
        </td>
        <td data-label="Last seen" title={fmtTime(a.last_seen_at)}>
          {fmtAgo(a.last_seen_at)}
        </td>
        <td data-label="Probes">
          {a.probes_total === 0 ? (
            <span className="hint">none yet</span>
          ) : a.probes_failing > 0 ? (
            <span className="status-text-degraded">
              {a.probes_failing} of {a.probes_total} failing
            </span>
          ) : (
            <span className="muted">{a.probes_total} ok</span>
          )}
        </td>
        <td data-label="Spool drops">
          {a.dropped_results === 0 ? (
            <span className="muted">none</span>
          ) : (
            <span
              className="status-text-degraded"
              title={a.last_dropped_at ? `last ${fmtTime(a.last_dropped_at)}` : undefined}
            >
              {a.dropped_results.toLocaleString()} lost · {fmtAgo(a.last_dropped_at)}
            </span>
          )}
        </td>
        <td data-label="Certificate">
          <CertCell a={a} />
        </td>
        <td className="mono" data-label="Config" title={a.config_hash || undefined}>
          {a.config_hash ? a.config_hash.slice(0, 8) : '—'}
        </td>
        <td data-label="Detail">
          <button
            type="button"
            className="incident-toggle agent-detail-toggle"
            aria-expanded={expanded}
            onClick={onToggle}
          >
            {expanded ? 'Hide details' : 'View details'} <span aria-hidden="true">{expanded ? '−' : '+'}</span>
          </button>
        </td>
      </tr>
      {expanded && (
        <tr className="agent-detail-row">
          <td colSpan={10} data-label="24 h probes">
            <ProbeDetail detail={detail} error={detailError} />
          </td>
        </tr>
      )}
    </>
  )
}

export default function Agents({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [data, setData] = useState<AgentsResponse | null>(null)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState<FleetFilter>('all')
  const [query, setQuery] = useState('')
  // One agent's row may be expanded to its per-probe detail, fetched
  // lazily on expand and refreshed on the same cadence as the table.
  const [expanded, setExpanded] = useState<string | null>(null)
  const [detail, setDetail] = useState<AgentProbeHealthResponse | null>(null)
  const [detailError, setDetailError] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = () =>
      apiGet<AgentsResponse>('/api/v1/agents')
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
  }, [onAuthError])

  useEffect(() => {
    if (!expanded) {
      setDetail(null)
      setDetailError('')
      return
    }
    let cancelled = false
    setDetail(null)
    setDetailError('')
    const load = () =>
      apiGet<AgentProbeHealthResponse>(`/api/v1/agents/${expanded}/health?window=24h`)
        .then((res) => {
          if (!cancelled) {
            setDetail(res)
            setDetailError('')
          }
        })
        .catch((err) => {
          onAuthError(err)
          if (!cancelled) setDetailError(err instanceof Error ? err.message : String(err))
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [expanded, onAuthError])

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase()
    return (data?.agents ?? []).filter((agent) => {
      // The two filters partition the fleet on needsAttention — including
      // never-seen (stale) agents and recent spool drops — so the button
      // counts always match the rows they reveal.
      if (filter === 'attention' && !needsAttention(agent)) return false
      if (filter === 'healthy' && needsAttention(agent)) return false
      if (!needle) return true
      return [agent.site, agent.hostname, agent.probe_address, agent.version].some((value) =>
        value.toLowerCase().includes(needle),
      )
    })
  }, [data, filter, query])

  if (error && !data)
    return (
      <div className="state-panel state-error">
        <h1>Agents unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!data)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading agents…
      </div>
    )

  const down = data.agents.filter((a) => health(a).status === 'down').length
  const degraded = data.agents.filter((a) => health(a).status === 'degraded').length
  const dropsTotal = data.agents.reduce((sum, a) => sum + a.dropped_results, 0)
  const attention = data.agents.filter(needsAttention).length
  const healthy = data.agents.length - attention

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Operations</div>
          <h1>Agents</h1>
          <p>Connection, probe, certificate, configuration, and spool health.</p>
        </div>
        <div className="chips">
          <span className="chip">
            enrolled <span className="mono">{data.agents.length}</span>
          </span>
          <span className="chip">
            {down > 0 && <span className="dot swatch status-down" />}
            down <span className="mono">{down}</span>
          </span>
          <span className="chip">
            {degraded > 0 && <span className="dot swatch status-degraded" />}
            degraded <span className="mono">{degraded}</span>
          </span>
          <span className="chip">
            results lost <span className="mono">{dropsTotal.toLocaleString()}</span>
          </span>
        </div>
      </div>

      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar">
        <div className="control-group" role="group" aria-label="Agent health">
          <button
            className={filter === 'all' ? 'active' : ''}
            aria-pressed={filter === 'all'}
            onClick={() => setFilter('all')}
          >
            All {data.agents.length}
          </button>
          <button
            className={filter === 'attention' ? 'active' : ''}
            aria-pressed={filter === 'attention'}
            onClick={() => setFilter('attention')}
          >
            Attention {attention}
          </button>
          <button
            className={filter === 'healthy' ? 'active' : ''}
            aria-pressed={filter === 'healthy'}
            onClick={() => setFilter('healthy')}
          >
            Healthy {healthy}
          </button>
        </div>
        <label className="search-field">
          <span className="sr-only">Search agents</span>
          <input
            type="search"
            placeholder="Search site, host, or address"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <span className="freshness">Refreshes every {POLL_MS / 1000}s</span>
      </div>

      <div className="card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Fleet inventory</span>
            <h2>Enrolled agents</h2>
          </div>
          <span className="hint">
            Spool drops are lifetime totals{error ? ' · refresh failed, showing last data' : ''}
          </span>
        </div>
        {visible.length === 0 ? (
          <div className="empty-state">
            <strong>{data.agents.length === 0 ? 'No agents enrolled' : 'No matching agents'}</strong>
            <span>
              {data.agents.length === 0
                ? 'Enroll an agent to begin monitoring a site.'
                : 'Change the health filter or search query.'}
            </span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th className="eyebrow">status</th>
                  <th className="eyebrow">agent</th>
                  <th className="eyebrow">address</th>
                  <th className="eyebrow">version</th>
                  <th className="eyebrow">last seen</th>
                  <th className="eyebrow">probes</th>
                  <th className="eyebrow">spool drops</th>
                  <th className="eyebrow">certificate</th>
                  <th className="eyebrow">config</th>
                  <th className="actions-col">
                    <span className="sr-only">Detail</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {visible.map((a) => (
                  <Row
                    key={a.id}
                    a={a}
                    expanded={expanded === a.id}
                    onToggle={() => setExpanded((prev) => (prev === a.id ? null : a.id))}
                    detail={detail}
                    detailError={detailError}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
