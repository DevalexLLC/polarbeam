import { useEffect, useState } from 'react'
import DataTable, { type DataTableColumn } from '../components/DataTable'
import PageError from '../components/PageError'
import { fmtAgo, fmtTime } from '../format'
import { useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork, targetDetailHref, updateRouteParams } from '../routeState'
import { useTimezone } from '../timezone'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { OperationalTarget, OperationalTargetsResponse } from '../types'

const TARGET_PAGE = 25

function StatusCell({ t }: { t: OperationalTarget }) {
  if (t.status === 'incident') {
    return (
      <span>
        <span className="dot swatch status-down" /> incident
      </span>
    )
  }
  if (t.status === 'unprobed') return <span className="hint">unprobed</span>
  // "no incidents" rather than "ok": this cell is fed by the incident
  // feed alone, which says nothing about targets that were never measured.
  return (
    <span>
      <span className="dot swatch status-ok" /> no incidents
    </span>
  )
}

export default function Targets({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [kind] = useRouteParam('kind', 'all')
  const [status] = useRouteParam('status', 'all')
  const [sort] = useRouteParam('sort', 'name')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const { network } = useNetworkFilter()

  const params = new URLSearchParams({
    limit: String(TARGET_PAGE),
    offset: String((page - 1) * TARGET_PAGE),
    sort,
    order,
  })
  if (network) params.set('network', network)
  if (queryParam.trim()) params.set('q', queryParam.trim())
  if (kind !== 'all') params.set('kind', kind)
  if (status !== 'all') params.set('status', status === 'healthy' ? 'no_incidents' : status)
  const { data, error, reload } = usePolledResource<OperationalTargetsResponse>(
    '/api/v1/targets?' + params.toString(),
    { onAuthError, logLabel: 'targets' },
  )

  const pageCount = Math.max(1, Math.ceil((data?.page.total ?? 0) / TARGET_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  if (error && !data)
    return (
      <PageError
        title="Targets unavailable"
        subject="targets"
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
        Loading targets…
      </div>
    )

  const externalCount = data.scope_summary.external
  const siteCount = data.scope_summary.agent
  const troubledCount = data.scope_summary.incident
  const columns: DataTableColumn<OperationalTarget>[] = [
    {
      key: 'status',
      label: 'Status',
      sortKey: 'status',
      priority: 'status',
      render: (target) => <StatusCell t={target} />,
    },
    {
      key: 'name',
      label: 'Target',
      sortKey: 'name',
      priority: 'identity',
      render: (target) => (
        <a href={targetDetailHref(target.id)}>
          {target.kind === 'agent' ? (target.agent_site ?? target.name) : target.name}
        </a>
      ),
    },
    {
      key: 'kind',
      label: 'Kind',
      priority: 'primary',
      render: (target) => (target.kind === 'agent' ? 'site destination' : 'external'),
    },
    {
      key: 'evidence',
      label: 'Primary evidence',
      priority: 'primary',
      render: (target) =>
        target.kind === 'agent' ? (
          (target.agent_hostname ?? <span className="hint">deleted agent</span>)
        ) : (
          <span className="mono">
            {target.url ? target.url : target.port ? `${target.address}:${target.port}` : target.address}
          </span>
        ),
    },
    {
      key: 'probes',
      label: 'Probes',
      sortKey: 'probes',
      priority: 'secondary',
      render: (target) =>
        target.enabled_probe_count === target.probe_count
          ? target.probe_count
          : `${target.enabled_probe_count} of ${target.probe_count} enabled`,
    },
    {
      key: 'from',
      label: 'Probed from',
      priority: 'secondary',
      render: (target) => target.probing_sites.join(', ') || '—',
    },
    {
      key: 'network',
      label: 'Network',
      priority: 'secondary',
      render: (target) => target.network || 'all networks',
    },
    {
      key: 'created',
      label: 'Created',
      sortKey: 'created',
      priority: 'secondary',
      render: (target) => <span title={fmtTime(target.created_at)}>{fmtAgo(target.created_at)}</span>,
    },
  ]

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <h1>Targets</h1>
        </div>
      </div>

      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar">
        <label className="search-field">
          <span className="sr-only">Search targets</span>
          <input
            type="search"
            placeholder="Search name, endpoint, or probing site"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <label className="compact-select">
          <span>Kind</span>
          <select value={kind} onChange={(event) => updateRouteParams({ kind: event.target.value, page: null })}>
            <option value="all">All kinds</option>
            <option value="external">External</option>
            <option value="agent">Site destinations</option>
          </select>
        </label>
        <label className="compact-select">
          <span>Status</span>
          <select value={status} onChange={(event) => updateRouteParams({ status: event.target.value, page: null })}>
            <option value="all">All statuses</option>
            <option value="incident">Incident</option>
            <option value="healthy">No incidents</option>
            <option value="unprobed">Unprobed</option>
          </select>
        </label>
        <label className="compact-select">
          <span>Sort</span>
          <select value={sort} onChange={(event) => updateRouteParams({ sort: event.target.value, page: null })}>
            <option value="name">Name</option>
            <option value="status">Status</option>
            <option value="probes">Probes</option>
            <option value="created">Created</option>
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
      </div>

      <div className="card">
        <div className="card-head">
          <div>
            <h2>Target inventory</h2>
          </div>
          <span className="hint">
            {externalCount} external, {siteCount} site {siteCount === 1 ? 'destination' : 'destinations'},{' '}
            {troubledCount} with {troubledCount === 1 ? 'an incident' : 'incidents'}
          </span>
        </div>
        <DataTable
          label="Operational targets"
          rows={data.targets}
          rowKey={(target) => target.id}
          columns={columns}
          sort={{ key: sort, order: order === 'desc' ? 'desc' : 'asc' }}
          onSortChange={(next) =>
            updateRouteParams({
              sort: next.key === 'name' ? null : next.key,
              order: next.order === 'asc' ? null : next.order,
              page: null,
            })
          }
          page={data.page}
          onPageChange={(next) => {
            setExpandedRow(null)
            setPage(next)
          }}
          resultLabel="targets"
          emptyTitle={externalCount === 0 && siteCount === 0 ? 'No targets yet' : 'No matching targets'}
          emptyDescription={
            externalCount === 0 && siteCount === 0
              ? 'An admin can add hosts and URLs to probe in Settings → Targets. Enroll an agent and its destination appears here.'
              : 'Change the kind, status, search, or network filter.'
          }
          disclosure={{
            expandedKey: expandedRow,
            onExpandedKeyChange: setExpandedRow,
            label: (_target, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
            desktop: false,
          }}
        />
        <p className="card-foot">Status reflects open incidents. Admins manage external targets in Settings.</p>
      </div>
    </>
  )
}
