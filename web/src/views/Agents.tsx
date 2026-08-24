import { useEffect, useRef, useState } from 'react'
import { apiGet } from '../api'
import DataTable, { type DataTableColumn } from '../components/DataTable'
import HealthStrip, { stripStats, UptimeValue } from '../components/HealthStrip'
import PageError from '../components/PageError'
import { fmtAgo, fmtTime } from '../format'
import { useNetworkFilter } from '../networkFilter'
import { pageFailure } from '../pageState'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type {
  AgentBucketFailuresResponse,
  AgentInfo,
  AgentInventorySummary,
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
  surface,
}: {
  agentId: string
  detail: AgentProbeHealthResponse | null
  error: unknown
  selectedProbe: string
  onSelectProbe: (probe: string) => void
  surface: 'desktop' | 'mobile'
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
            id={`agent-probe-${p.probe_id}-${surface}`}
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

function AgentDetails({
  a,
  detail,
  detailError,
  selectedProbe,
  onSelectProbe,
  surface,
}: {
  a: AgentInfo
  detail: AgentProbeHealthResponse | null
  detailError: unknown
  selectedProbe: string
  onSelectProbe: (probe: string) => void
  surface: 'desktop' | 'mobile'
}) {
  return (
    <ProbeDetail
      agentId={a.id}
      detail={detail}
      error={detailError}
      selectedProbe={selectedProbe}
      onSelectProbe={onSelectProbe}
      surface={surface}
    />
  )
}

export default function Agents({
  agent,
  networks,
  onAuthError,
  onTitleChange,
}: {
  agent: string | null
  networks: string[]
  onAuthError: (err: unknown) => void
  onTitleChange: (title: string) => void
}) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [data, setData] = useState<AgentsResponse | null>(null)
  const [scopeSummary, setScopeSummary] = useState<AgentInventorySummary | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [healthParam] = useRouteParam('health', 'all')
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [sort] = useRouteParam('sort', 'status')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [selectedProbe, setSelectedProbe] = useRouteParam('probe')
  const { network } = useNetworkFilter()
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
  const pinnedAgent = useRef<string | null>(expanded)
  const [detail, setDetail] = useState<AgentProbeHealthResponse | null>(null)
  const [detailError, setDetailError] = useState<unknown>(null)

  if (!expanded) pinnedAgent.current = null
  else if (pinnedAgent.current !== expanded) {
    pinnedAgent.current = data?.agents.some((row) => row.id === expanded) ? null : expanded
  }
  const pinnedAgentID = pinnedAgent.current === expanded ? expanded : null

  useEffect(() => {
    let cancelled = false
    const params = new URLSearchParams({
      limit: String(AGENT_PAGE),
      offset: String(pinnedAgentID ? 0 : (page - 1) * AGENT_PAGE),
      sort: sort === 'status' ? 'health' : sort,
      order,
    })
    if (network) params.set('network', network)
    if (pinnedAgentID) params.set('q', pinnedAgentID)
    else if (queryParam.trim()) params.set('q', queryParam.trim())
    if (filter !== 'all') params.set('health', filter === 'healthy' ? 'clear' : filter)
    const requestURL = '/api/v1/agents?' + params.toString()
    const scopeParams = new URLSearchParams({ limit: '1', offset: '0', sort: 'health', order: 'asc' })
    if (network) scopeParams.set('network', network)
    const needsScopeRequest = Boolean(pinnedAgentID || queryParam.trim() || filter !== 'all')
    const load = () => {
      const inventoryRequest = apiGet<AgentsResponse>(requestURL)
      const scopeRequest = needsScopeRequest
        ? apiGet<AgentsResponse>('/api/v1/agents?' + scopeParams.toString())
        : inventoryRequest
      Promise.all([inventoryRequest, scopeRequest])
        .then(([res, scope]) => {
          if (!cancelled) {
            setData(res)
            setScopeSummary(scope.summary ?? null)
            setError(null)
          }
        })
        .catch((err) => {
          onAuthError(err)
          console.error('agents request failed', err)
          if (!cancelled) setError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [filter, network, onAuthError, order, page, pinnedAgentID, queryParam, retryKey, sort])

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
    if (!selectedProbe) {
      scrolledProbe.current = null
      return
    }
    if (!detail) return
    if (detail.probes.some((probe) => probe.probe_id === selectedProbe)) {
      const key = `${expanded ?? ''}\u0000${selectedProbe}`
      if (scrolledProbe.current !== key) {
        const surface = window.matchMedia('(max-width: 760px)').matches ? 'mobile' : 'desktop'
        const row = document.getElementById(`agent-probe-${selectedProbe}-${surface}`)
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
    const surface = window.matchMedia('(max-width: 760px)').matches ? 'mobile' : 'desktop'
    const row = document.getElementById(`agent-${expanded}-${surface}`)
    if (!row) return
    row.scrollIntoView({ block: 'nearest' })
    scrolledAgent.current = expanded
  }, [expanded, data, page])

  const fleet = data?.agents ?? []
  const pageMeta = data?.page ?? { limit: AGENT_PAGE, offset: 0, total: fleet.length, has_more: false }
  const summary = data?.summary ?? {
    total: fleet.length,
    offline: 0,
    degraded: 0,
    healthy: 0,
    no_data: 0,
    attention: 0,
    dropped_results: 0,
  }
  const pageCount = Math.max(1, Math.ceil(pageMeta.total / AGENT_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

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

  const fleetSummary = scopeSummary ?? summary
  const down = fleetSummary.offline
  const degraded = fleetSummary.degraded
  const dropsTotal = fleetSummary.dropped_results
  const attention = fleetSummary.attention
  const healthy = fleetSummary.total - attention
  const multiNetwork = networks.length > 1

  const columns: DataTableColumn<AgentInfo>[] = [
    {
      key: 'status',
      label: 'Status',
      sortKey: 'status',
      priority: 'status',
      render: (row) => {
        const current = health(row)
        return (
          <span className={'status-text-' + current.status}>
            <span className={'dot swatch status-' + current.status} /> {current.label}
          </span>
        )
      },
    },
    {
      key: 'agent',
      label: 'Agent',
      sortKey: 'hostname',
      priority: 'identity',
      className: 'mono',
      render: (row) => (
        <span title={`enrolled ${fmtTime(row.enrolled_at)} · ${row.id}`}>
          {row.site} · {row.hostname}
        </span>
      ),
    },
    ...(multiNetwork
      ? [
          {
            key: 'network',
            label: 'Network',
            priority: 'secondary' as const,
            className: 'mono',
            render: (row: AgentInfo) => row.network,
          },
        ]
      : []),
    {
      key: 'address',
      label: 'Address',
      priority: 'primary',
      className: 'mono',
      render: (row) => row.probe_address || '—',
    },
    { key: 'version', label: 'Version', priority: 'secondary', className: 'mono', render: (row) => row.version || '—' },
    {
      key: 'last_seen',
      label: 'Last seen',
      sortKey: 'last_seen',
      priority: 'primary',
      render: (row) => <span title={fmtTime(row.last_seen_at)}>{fmtAgo(row.last_seen_at)}</span>,
    },
    {
      key: 'probes',
      label: 'Probes',
      priority: 'primary',
      render: (row) =>
        row.probes_total === 0 ? (
          <span className="hint">none yet</span>
        ) : row.probes_failing > 0 ? (
          <span className="status-text-degraded">
            {row.probes_failing} of {row.probes_total} failing
          </span>
        ) : (
          <span className="muted">{row.probes_total} ok</span>
        ),
    },
    {
      key: 'spool',
      label: 'Spool drops',
      priority: 'secondary',
      render: (row) =>
        row.dropped_results === 0 ? (
          <span className="muted">none</span>
        ) : (
          <span
            className="status-text-degraded"
            title={row.last_dropped_at ? `last ${fmtTime(row.last_dropped_at)}` : undefined}
          >
            {row.dropped_results.toLocaleString()} lost · {fmtAgo(row.last_dropped_at)}
          </span>
        ),
    },
    { key: 'certificate', label: 'Certificate', priority: 'secondary', render: (row) => <CertCell a={row} /> },
    {
      key: 'config',
      label: 'Config',
      priority: 'secondary',
      className: 'mono',
      render: (row) => (
        <span title={row.config_hash || undefined}>{row.config_hash ? row.config_hash.slice(0, 8) : '—'}</span>
      ),
    },
  ]

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
            enrolled <span className="mono">{fleetSummary.total}</span>
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

      {pinnedAgentID && (
        <div className="data-table-context" role="status">
          <span>Showing the linked agent and its probe evidence.</span>
          <button
            type="button"
            className="secondary-button"
            onClick={() => updateRouteParams({ agent: null, probe: null })}
          >
            Show fleet
          </button>
        </div>
      )}

      <div className="view-toolbar">
        <div className="control-group" role="group" aria-label="Agent health">
          <button
            className={filter === 'all' ? 'active' : ''}
            aria-pressed={filter === 'all'}
            onClick={() => updateRouteParams({ health: null, page: null, agent: null, probe: null })}
          >
            All {fleetSummary.total}
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
            onChange={(e) => {
              setQuery(e.target.value)
              if (expanded) updateRouteParams({ agent: null, probe: null }, 'replace')
            }}
          />
        </label>
        <label className="compact-select">
          <span>Sort</span>
          <select
            value={sort}
            onChange={(event) => updateRouteParams({ sort: event.target.value, page: null, agent: null, probe: null })}
          >
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
          onClick={() =>
            updateRouteParams({ order: order === 'asc' ? 'desc' : null, page: null, agent: null, probe: null })
          }
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
        <DataTable
          label="Enrolled agents"
          rows={fleet}
          rowKey={(row) => row.id}
          rowID={(row) => 'agent-' + row.id}
          columns={columns}
          sort={{ key: sort, order: order === 'desc' ? 'desc' : 'asc' }}
          onSortChange={(next) =>
            updateRouteParams({
              sort: next.key === 'status' ? null : next.key,
              order: next.order === 'asc' ? null : next.order,
              page: null,
              agent: null,
              probe: null,
            })
          }
          page={pageMeta}
          onPageChange={(next) => {
            updateRouteParams({ page: next === 1 ? null : next, agent: null, probe: null })
          }}
          resultLabel="agents"
          emptyTitle={summary.total === 0 && !query && filter === 'all' ? 'No agents enrolled' : 'No matching agents'}
          emptyDescription={
            summary.total === 0 && !query && filter === 'all'
              ? 'Enroll an agent to begin monitoring a site.'
              : 'Change the health filter, search query, or top-bar network filter.'
          }
          disclosure={{
            expandedKey: expanded,
            onExpandedKeyChange: (key) =>
              updateRouteParams({ agent: key, probe: null }, key === null ? 'replace' : 'push'),
            label: (_row, open) => (open ? 'Hide probe evidence' : 'Show probe evidence'),
            render: (row, surface) => (
              <AgentDetails
                a={row}
                detail={detail}
                detailError={detailError}
                selectedProbe={selectedProbe}
                onSelectProbe={setSelectedProbe}
                surface={surface}
              />
            ),
          }}
        />
      </div>
    </>
  )
}
