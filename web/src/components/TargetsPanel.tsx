import { useEffect, useState } from 'react'
import { ApiError, apiDelete, apiGet, apiPost } from '../api'
import { fmtAgo } from '../format'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { useErrorSummary } from '../formErrors'
import { useNetworkFilter } from '../networkFilter'
import type { Caps } from '../caps'
import { canWriteRow } from '../caps'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { inheritRouteNetwork, updateRouteParams } from '../routeState'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type { TargetsConfigResponse, TargetConfig } from '../types'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import PlaneField from './PlaneField'
import SettingsPageError from './SettingsPageError'

const TARGET_PAGE = 25

interface Draft {
  name: string
  address: string
  port: string
  url: string
  // The owning plane, '' for the global target every network may probe.
  // Fixed once created: the row's owner decides who may edit it.
  network: string
}

const emptyDraft: Draft = { name: '', address: '', port: '', url: '', network: '' }

const draftFrom = (target: TargetConfig): Draft => ({
  name: target.name,
  address: target.address ?? '',
  port: target.port ? String(target.port) : '',
  url: target.url ?? '',
  network: target.network,
})

// Mirrors the server's target validation; server 400s render verbatim as a
// backstop.
function validate(d: Draft): { errors: string[]; port: number } {
  const errors: string[] = []
  if (d.name.trim() === '') errors.push('name is required')
  if (d.address.trim() === '' && d.url.trim() === '') errors.push('address or URL is required')
  let port = 0
  if (d.port.trim() !== '') {
    port = Number(d.port)
    if (!Number.isInteger(port) || port < 0 || port > 65535) {
      errors.push('port must be an integer between 0 and 65535')
      port = 0
    }
  }
  return { errors, port }
}

export default function TargetsPanel({
  caps,
  canWrite,
  plane,
  onAuthError,
}: {
  caps: Caps
  canWrite: boolean
  plane: PlaneChoice
  onAuthError: (err: unknown) => void
}) {
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing target (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const summary = useErrorSummary(formErrors.length > 0)
  const [saving, setSaving] = useState(false)
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [kind] = useRouteParam('kind', 'all')
  const [sort] = useRouteParam('sort', 'name')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const [actionRow, setActionRow] = useState<string | null>(null)
  const { network } = useNetworkFilter()
  const feedback = useSettingsMutation()

  const params = new URLSearchParams({
    limit: String(TARGET_PAGE),
    offset: String((page - 1) * TARGET_PAGE),
    sort,
    order,
  })
  if (queryParam.trim()) params.set('q', queryParam.trim())
  if (kind !== 'all') params.set('kind', kind)
  if (network) params.set('network', network)
  const requestURL = '/api/v1/config/targets?' + params.toString()

  const { data, error, reload } = usePolledResource<TargetsConfigResponse>(requestURL, {
    onAuthError,
    logLabel: 'target settings',
  })

  const blankDraft = (): Draft => ({ ...emptyDraft, network: initialPlane(plane) })
  const loadedTarget = editing && draft ? data?.targets.find((target) => target.name === draft.name) : undefined
  const loadedDraft = loadedTarget ? draftFrom(loadedTarget) : blankDraft()
  const guard = useConcurrentSettingsDraft({
    id: 'target-form',
    label: editing && draft ? `Target ${draft.name}` : 'New target',
    loaded: loadedDraft,
    current: draft ?? loadedDraft,
    editing: draft !== null,
    discard: () => {
      setDraft(null)
      setEditing(false)
      setFormErrors([])
    },
    reload: (latest) => {
      setDraft(latest.name ? latest : null)
      setEditing(latest.name !== '')
    },
  })

  const save = async () => {
    if (!draft) return
    const { errors, port } = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) {
      summary.request()
      feedback.error(`Target: ${errors.join('; ')}`)
      return
    }
    setSaving(true)
    try {
      const loadNamedTarget = async () => {
        try {
          return await apiGet<TargetConfig>('/api/v1/config/targets/' + encodeURIComponent(draft.name.trim()))
        } catch (requestError) {
          if (requestError instanceof ApiError && requestError.status === 404) return null
          throw requestError
        }
      }
      if (editing) {
        const currentServer = await guard.checkForConflict(async () => {
          const latest = await loadNamedTarget()
          return latest ? draftFrom(latest) : blankDraft()
        })
        if (!currentServer) return
      } else if (await loadNamedTarget()) {
        const message = `a target named ${draft.name.trim()} already exists — choose another name or edit that target`
        setFormErrors([message])
        summary.request()
        feedback.error(`Target was not added: ${message}`)
        return
      }
      await apiPost('/api/v1/config/targets', {
        name: draft.name.trim(),
        address: draft.address.trim(),
        port,
        url: draft.url.trim(),
        ...networkField(draft.network),
      })
      setDraft(null)
      setEditing(false)
      feedback.success(editing ? `Target ${draft.name.trim()} saved.` : `Target ${draft.name.trim()} added.`)
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      summary.request()
      feedback.error(`Target was not saved: ${message}`)
    } finally {
      setSaving(false)
    }
  }

  const remove = async (t: TargetConfig) => {
    try {
      await apiDelete('/api/v1/config/targets/' + encodeURIComponent(t.name))
      feedback.success(`Target ${t.name} deleted.`)
      await reload()
    } catch (err) {
      onAuthError(err)
      console.error('target delete failed', err)
      feedback.error(`Target ${t.name} was not deleted: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const startEdit = (t: TargetConfig) => {
    setEditing(true)
    setFormErrors([])
    setDraft(draftFrom(t))
  }

  const pageMeta = data?.page ?? { limit: TARGET_PAGE, offset: 0, total: data?.targets.length ?? 0, has_more: false }
  const pageCount = Math.max(1, Math.ceil(pageMeta.total / TARGET_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  if (error && !data) {
    return (
      <SettingsPageError title="Targets unavailable" subject="targets" error={error} onRetry={() => void reload()} />
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading targets…
      </div>
    )
  }

  const columns: DataTableColumn<TargetConfig>[] = [
    {
      key: 'name',
      label: 'Target',
      sortKey: 'name',
      priority: 'identity',
      className: 'mono',
      render: (target) => <a href={inheritRouteNetwork('#/target/' + encodeURIComponent(target.id))}>{target.name}</a>,
    },
    {
      key: 'kind',
      label: 'Kind',
      sortKey: 'kind',
      priority: 'status',
      render: (target) => (target.kind === 'agent' ? 'agent' : 'external'),
    },
    {
      key: 'endpoint',
      label: 'Endpoint',
      priority: 'primary',
      className: 'mono',
      render: (target) =>
        target.kind === 'agent' ? (
          <span className="hint">enrollment managed</span>
        ) : target.url ? (
          target.url
        ) : target.port ? (
          `${target.address}:${target.port}`
        ) : (
          target.address
        ),
    },
    {
      key: 'network',
      label: 'Network',
      sortKey: 'network',
      priority: 'secondary',
      render: (target) => (target.network === '' ? <span className="hint">all networks</span> : target.network),
    },
    { key: 'probes', label: 'Probes', sortKey: 'probes', priority: 'primary', render: (target) => target.probe_count },
    {
      key: 'created',
      label: 'Created',
      sortKey: 'created',
      priority: 'secondary',
      render: (target) => fmtAgo(target.created_at),
    },
  ]

  const field = (label: string, key: keyof Draft, placeholder: string, locked = false) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          value={draft?.[key] ?? ''}
          placeholder={placeholder}
          disabled={saving || locked}
          aria-describedby={summary.describedby}
          onChange={(e) => {
            setDraft((d) => ({ ...(d ?? blankDraft()), [key]: e.target.value }))
          }}
        />
      </span>
    </label>
  )

  return (
    <>
      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Probe destinations</span>
            <h2>Targets</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          External hosts and URLs share this inventory with enrollment-managed agent destinations. A target in use by
          probes cannot be deleted until those probes are removed.
        </p>
        <div className="view-toolbar data-table-toolbar">
          <label className="search-field">
            <span className="sr-only">Search target settings</span>
            <input
              type="search"
              placeholder="Search name or endpoint"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value)
                setExpandedRow(null)
              }}
            />
          </label>
          <label className="compact-select">
            <span>Kind</span>
            <select value={kind} onChange={(event) => updateRouteParams({ kind: event.target.value, page: null })}>
              <option value="all">All kinds</option>
              <option value="external">External</option>
              <option value="agent">Agent</option>
            </select>
          </label>
        </div>
        <DataTable
          label="Target settings"
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
          page={pageMeta}
          onPageChange={(next) => {
            setExpandedRow(null)
            setPage(next)
          }}
          resultLabel="targets"
          emptyTitle={query || kind !== 'all' ? 'No matching targets' : 'No targets'}
          emptyDescription={
            query || kind !== 'all'
              ? 'Change the search text or kind filter.'
              : 'Add an external target below, or enroll an agent.'
          }
          disclosure={{
            expandedKey: expandedRow,
            onExpandedKeyChange: setExpandedRow,
            label: (_target, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
            desktop: false,
          }}
          actions={
            canWrite
              ? {
                  openKey: actionRow,
                  onOpenKeyChange: setActionRow,
                  label: (target) => `Actions for ${target.name}`,
                  render: (target) =>
                    target.kind === 'external' && canWriteRow(caps, target.network) ? (
                      <>
                        <button
                          type="button"
                          className="secondary-button"
                          onClick={() => {
                            setActionRow(null)
                            startEdit(target)
                          }}
                        >
                          Edit
                        </button>
                        <ConfirmButton
                          label="Delete"
                          resource={`Target ${target.name}`}
                          consequence="This permanently removes the target."
                          disabled={target.probe_count > 0}
                          title={
                            target.probe_count > 0
                              ? `In use by ${target.probe_count} probe(s) — remove those first`
                              : undefined
                          }
                          onConfirm={() => remove(target)}
                        />
                      </>
                    ) : (
                      <span className="hint">{target.kind === 'agent' ? 'enrollment-managed' : 'operator-owned'}</span>
                    ),
                }
              : undefined
          }
        />
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">{editing ? `Edit ${draft?.name}` : 'Add target'}</h3>
            <div className="config-form-grid">
              {field('Name', 'name', 'unique handle, e.g. pg-primary', editing)}
              {field('Address', 'address', 'host or IP (tcp/tls/icmp/dns/ntp)')}
              {field('Port', 'port', 'for tcp/tls probes (ntp defaults to 123)')}
              {field('URL', 'url', 'full URL for http probes')}
              {/* Ownership is fixed at creation: it decides who may edit the
                row, so changing it on an existing target would be a
                privilege transfer rather than an edit. */}
              {!editing && (
                <PlaneField
                  choice={plane}
                  value={draft?.network ?? initialPlane(plane)}
                  onChange={(v) => setDraft((d) => ({ ...(d ?? blankDraft()), network: v }))}
                  disabled={saving}
                  label="Owner"
                  hint="all networks publishes it to every plane; a network makes it that tenant's"
                />
              )}
            </div>
            {formErrors.length > 0 && (
              <ul className="error threshold-errors" id={summary.id} ref={summary.ref} tabIndex={-1}>
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">New probes can use this target as soon as it is saved.</span>
              <span className="threshold-actions">
                {(editing || draft) && (
                  <button
                    type="button"
                    className="secondary-button"
                    disabled={saving}
                    onClick={() => {
                      setDraft(null)
                      setEditing(false)
                      setFormErrors([])
                    }}
                  >
                    Cancel
                  </button>
                )}
                <button
                  className="primary"
                  onClick={save}
                  disabled={saving || !draft || !guard.dirty || (!editing && !planeReady(plane))}
                >
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add target'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
