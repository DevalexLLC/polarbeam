import { useEffect, useMemo, useRef, useState } from 'react'
import { apiGet } from '../api'
import DisclosureChevron from '../components/DisclosureChevron'
import HealthStrip, { stripStats, UptimeValue } from '../components/HealthStrip'
import PageError from '../components/PageError'
import { fmtAgo, fmtTime } from '../format'
import { matchesNetworkFilter, useNetworkFilter } from '../networkFilter'
import { pageFailure } from '../pageState'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type {
  AgentBucketFailuresResponse,
  AgentInfo,
  AgentProbeHealth,
  AgentProbeHealthResponse,
  AgentsResponse,
} from '../types'

const POLL_MS = 30_000
// Agents renew their cert at 2/3 lifetime (10 days left on the 30-day
// cert), so the warn threshold must be BELOW the renewal point or every
// healthy agent is flagged from issuance. 7 days = renewal has been
// failing for 3+ days. Mirror of CERT_WARN_DAYS in Overview.tsx.
const CERT_WARN_DAYS = 7
const AGENT_PAGE = 25

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
  // Target names link to the target detail page; a deleted target has no
  // id and stays plain text. The row's click guard already ignores clicks
  // that land on links, so linking here doesn't fight the row toggle.
  const link = (label: string) =>
    p.target_id ? (
      <a
        href={inheritRouteNetwork(
          '#/target/' + encodeURIComponent(p.target_id) + '?probe=' + encodeURIComponent(p.probe_id),
        )}
      >
        {label}
      </a>
    ) : (
      <span>{label}</span>
    )
  return (
    <div className="probe-strip-label">
      <span className="mono">{p.type}</span>
      {p.target_kind === 'external' ? (
        <>
          {p.target ? link(p.target) : <span>deleted target</span>}
          <span className="chip">external</span>
        </>
      ) : p.target_kind === 'agent' ? (
        <span>→ {p.dst_site ? link(p.dst_site) : 'deleted site'}</span>
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
function ProbeDetail({
  agentId,
  detail,
  error,
  selectedProbe,
  onSelectProbe,
}: {
  agentId: string
  detail: AgentProbeHealthResponse | null
  error: unknown
  selectedProbe: string
  onSelectProbe: (probe: string) => void
}) {
  if (!detail && error !== null)
    return (
      <div className="inline-alert" role="status">
        {pageFailure(error, 'probe detail').message}
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
      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      {probes.map((p) => {
        const s = stripStats(p.buckets, bucketS, nowS)
        return (
          <div
            key={p.probe_id}
            id={'agent-probe-' + p.probe_id}
            className={'probe-strip-row' + (selectedProbe === p.probe_id ? ' selected-row' : '')}
          >
            <div className="probe-select-cell">
              <ProbeLabel p={p} />
              <button
                type="button"
                className="probe-select-button linklike"
                aria-pressed={selectedProbe === p.probe_id}
                onClick={() => onSelectProbe(selectedProbe === p.probe_id ? '' : p.probe_id)}
              >
                {selectedProbe === p.probe_id ? 'Clear selection' : 'Select probe'}
              </button>
            </div>
            {/* probe_id scopes the breakdown to this series (and keeps
                traceroute in, matching this strip's own counts). */}
            <HealthStrip
              buckets={s.inWindow}
              bucketS={bucketS}
              endS={s.endS}
              label={s.stripLabel}
              fetchSlotDetail={(t) =>
                apiGet<AgentBucketFailuresResponse>(
                  `/api/v1/agents/${agentId}/health/bucket?t=${t}&probe_id=${p.probe_id}`,
                )
              }
            />
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
  multiNetwork,
  expanded,
  onToggle,
  detail,
  detailError,
  selectedProbe,
  onSelectProbe,
}: {
  a: AgentInfo
  multiNetwork: boolean
  expanded: boolean
  onToggle: () => void
  detail: AgentProbeHealthResponse | null
  detailError: unknown
  selectedProbe: string
  onSelectProbe: (probe: string) => void
}) {
  const h = health(a)
  const detailsID = `agent-detail-${a.id}`
  return (
    <>
      <tr
        id={'agent-' + a.id}
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
        {multiNetwork && (
          <td className="mono" data-label="Network">
            {a.network}
          </td>
        )}
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
            aria-controls={detailsID}
            onClick={onToggle}
          >
            {expanded ? 'Hide details' : 'View details'}
            <DisclosureChevron expanded={expanded} />
          </button>
        </td>
      </tr>
      {expanded && (
        <tr id={detailsID} className="agent-detail-row">
          <td colSpan={multiNetwork ? 11 : 10} data-label="24 h probes">
            <ProbeDetail
              agentId={a.id}
              detail={detail}
              error={detailError}
              selectedProbe={selectedProbe}
              onSelectProbe={onSelectProbe}
            />
          </td>
        </tr>
      )}
    </>
  )
}

export default function Agents({
  agent,
  onAuthError,
  onTitleChange,
}: {
  agent: string | null
  onAuthError: (err: unknown) => void
  onTitleChange: (title: string) => void
}) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [data, setData] = useState<AgentsResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [healthParam] = useRouteParam('health', 'all')
  const [query, setQuery] = useRouteSearch()
  const [sort] = useRouteParam('sort', 'status')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [selectedProbe, setSelectedProbe] = useRouteParam('probe')
  const scrolledAgent = useRef<string | null>(null)
  const scrolledProbe = useRef<string | null>(null)
  const filter = healthParam as FleetFilter
  // One agent's row may be expanded to its per-probe detail, fetched
  // lazily on expand and refreshed on the same cadence as the table.
  // The hash query's agent value is the single source of truth for which row
  // is open — not local state, which would go stale when the top-nav
  // Agents link resets the hash — so deep links from the Overview fleet
  // card, refreshes, and Back all restore the expansion for free.
  const expanded = agent
  const [detail, setDetail] = useState<AgentProbeHealthResponse | null>(null)
  const [detailError, setDetailError] = useState<unknown>(null)

  useEffect(() => {
    let cancelled = false
    const load = () =>
      apiGet<AgentsResponse>('/api/v1/agents')
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setError(null)
          }
        })
        .catch((err) => {
          onAuthError(err)
          console.error('agents request failed', err)
          if (!cancelled) setError(err)
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError, retryKey])

  useEffect(() => {
    if (!expanded) {
      setDetail(null)
      setDetailError(null)
      return
    }
    let cancelled = false
    setDetail(null)
    setDetailError(null)
    const load = () =>
      apiGet<AgentProbeHealthResponse>(`/api/v1/agents/${expanded}/health?window=24h`)
        .then((res) => {
          if (!cancelled) {
            setDetail(res)
            setDetailError(null)
          }
        })
        .catch((err) => {
          onAuthError(err)
          console.error('agent probe detail request failed', err)
          if (!cancelled) setDetailError(err)
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [expanded, onAuthError])

  const expandedAgent = data?.agents.find((row) => row.id === expanded)
  useEffect(() => {
    if (expanded) onTitleChange(expandedAgent?.hostname ?? 'Agent detail')
  }, [expanded, expandedAgent?.hostname, onTitleChange])

  useEffect(() => {
    if (!data || !expanded) return
    if (data.agents.some((row) => row.id === expanded)) return
    updateRouteParams({ agent: null, probe: null }, 'replace')
  }, [data, expanded])

  useEffect(() => {
    if (!selectedProbe) {
      scrolledProbe.current = null
      return
    }
    if (!detail) return
    if (detail.probes.some((probe) => probe.probe_id === selectedProbe)) {
      const key = `${expanded ?? ''}\u0000${selectedProbe}`
      if (scrolledProbe.current !== key) {
        const row = document.getElementById('agent-probe-' + selectedProbe)
        if (!row) return
        row.scrollIntoView({ block: 'nearest' })
        scrolledProbe.current = key
      }
      return
    }
    setSelectedProbe('', 'replace')
  }, [detail, expanded, page, selectedProbe, setSelectedProbe])

  // Bring a deep-linked row into view once the table exists. block:
  // 'nearest' is a no-op for a row already on screen, so expanding by
  // click never jumps — only arrivals from the Overview fleet card (or a
  // bookmark) scroll. An unknown id simply has no row to scroll to.
  useEffect(() => {
    if (!expanded) {
      scrolledAgent.current = null
      return
    }
    if (!data || scrolledAgent.current === expanded) return
    const row = document.getElementById('agent-' + expanded)
    if (!row) return
    row.scrollIntoView({ block: 'nearest' })
    scrolledAgent.current = expanded
  }, [expanded, data, page])

  // The global top-bar network filter scopes the whole view: rows, header
  // chips, and the health-filter button counts all derive from this subset.
  const { network } = useNetworkFilter()
  const fleet = useMemo(
    () => (data?.agents ?? []).filter((a) => matchesNetworkFilter(network, a.network)),
    [data, network],
  )
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const filtered = fleet.filter((row) => {
      // The two filters partition the fleet on needsAttention — including
      // never-seen (stale) agents and recent spool drops — so the button
      // counts always match the rows they reveal.
      if (filter === 'attention' && !needsAttention(row)) return false
      if (filter === 'healthy' && needsAttention(row)) return false
      if (!needle) return true
      return [row.site, row.network, row.hostname, row.probe_address, row.version].some((value) =>
        value.toLowerCase().includes(needle),
      )
    })
    // oxlint-disable-next-line unicorn/no-array-sort
    return [...filtered].sort((a, b) => {
      const value = (row: AgentInfo): string | number => {
        if (sort === 'site') return row.site
        if (sort === 'hostname') return row.hostname
        if (sort === 'last_seen') return row.last_seen_at ? Date.parse(row.last_seen_at) : 0
        const rank: Record<Health, number> = { down: 0, degraded: 1, stale: 2, ok: 3 }
        return rank[health(row).status]
      }
      const x = value(a)
      const y = value(b)
      const comparison = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      return order === 'desc' ? -comparison : comparison
    })
  }, [fleet, filter, order, query, sort])
  const pageCount = Math.max(1, Math.ceil(visible.length / AGENT_PAGE))
  const pageRows = visible.slice((page - 1) * AGENT_PAGE, page * AGENT_PAGE)
  const positionedAgent = useRef<string | null>(null)
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])
  useEffect(() => {
    if (!expanded) {
      positionedAgent.current = null
      return
    }
    if (positionedAgent.current === expanded) return
    const index = visible.findIndex((row) => row.id === expanded)
    if (index === -1) return
    positionedAgent.current = expanded
    const selectedPage = Math.floor(index / AGENT_PAGE) + 1
    setPage(selectedPage, 'replace')
  }, [expanded, setPage, visible])

  if (error && !data)
    return (
      <PageError
        title="Agents unavailable"
        subject="agents"
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
        Loading agents…
      </div>
    )

  // Column appears only when the fleet actually spans networks — derived
  // from the whole fleet, not the filtered subset, so the table shape stays
  // stable as the global filter changes; single-network installs keep the
  // exact pre-networks table.
  const multiNetwork = new Set(data.agents.map((a) => a.network)).size > 1

  const down = fleet.filter((a) => health(a).status === 'down').length
  const degraded = fleet.filter((a) => health(a).status === 'degraded').length
  const dropsTotal = fleet.reduce((sum, a) => sum + a.dropped_results, 0)
  const attention = fleet.filter(needsAttention).length
  const healthy = fleet.length - attention

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
            enrolled <span className="mono">{fleet.length}</span>
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

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar">
        <div className="control-group" role="group" aria-label="Agent health">
          <button
            className={filter === 'all' ? 'active' : ''}
            aria-pressed={filter === 'all'}
            onClick={() => updateRouteParams({ health: null, page: null, agent: null, probe: null })}
          >
            All {fleet.length}
          </button>
          <button
            className={filter === 'attention' ? 'active' : ''}
            aria-pressed={filter === 'attention'}
            onClick={() => updateRouteParams({ health: 'attention', page: null, agent: null, probe: null })}
          >
            Attention {attention}
          </button>
          <button
            className={filter === 'healthy' ? 'active' : ''}
            aria-pressed={filter === 'healthy'}
            onClick={() => updateRouteParams({ health: 'healthy', page: null, agent: null, probe: null })}
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
        <label className="compact-select">
          <span>Sort</span>
          <select value={sort} onChange={(event) => updateRouteParams({ sort: event.target.value, page: null })}>
            <option value="status">Status</option>
            <option value="site">Site</option>
            <option value="hostname">Hostname</option>
            <option value="last_seen">Last seen</option>
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
                : 'Change the health filter, search query, or top-bar network filter.'}
            </span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th className="eyebrow">status</th>
                  <th className="eyebrow">agent</th>
                  {multiNetwork && <th className="eyebrow">network</th>}
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
                {pageRows.map((a) => (
                  <Row
                    key={a.id}
                    a={a}
                    multiNetwork={multiNetwork}
                    expanded={expanded === a.id}
                    onToggle={() => {
                      updateRouteParams({ agent: expanded === a.id ? null : a.id, probe: null })
                    }}
                    detail={detail}
                    detailError={detailError}
                    selectedProbe={selectedProbe}
                    onSelectProbe={setSelectedProbe}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {pageCount > 1 && (
        <div className="progressive-footer">
          <span className="hint">
            Page {page} of {pageCount} · {visible.length} agents
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
