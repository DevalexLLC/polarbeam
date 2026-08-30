import { useEffect, useRef, useState } from 'react'
import { ApiError, apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useErrorSummary } from '../formErrors'
import { useNetworkFilter } from '../networkFilter'
import { updateRouteParams } from '../routeState'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { serverSnapshotChanged } from '../settingsSnapshot'
import { usePolledResource } from '../usePolledResource'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import { useStickyPin } from '../useStickyPin'
import type {
  MeshesConfigResponse,
  ProbeConfig,
  ProbesConfigResponse,
  ProbeTypesResponse,
  SitesResponse,
  TargetsConfigResponse,
} from '../types'
import {
  assignmentLabel,
  draftFrom,
  mutationSnapshot,
  newDraft,
  paramsSummary,
  paramSpecsFor,
  type ProbeDraft,
  type ProbeEditSnapshot,
  validate,
} from '../probeDraft'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import PlaneField from './PlaneField'
import SettingsPageError from './SettingsPageError'

const PROBE_PAGE = 25

// DataTable keeps both surfaces mounted; CSS shows exactly one (the same
// 760px breakpoint the stylesheet uses), so DOM lookups must pick it.
const visibleSurface = () => (window.matchMedia('(max-width: 760px)').matches ? 'mobile' : 'desktop')

export default function ProbesPanel({
  canWrite,
  plane,
  selectedProbe,
  onSelectedProbe,
  onAuthError,
}: {
  canWrite: boolean
  plane: PlaneChoice
  selectedProbe: string
  onSelectedProbe: (probe: string, mode?: 'push' | 'replace') => void
  onAuthError: (err: unknown) => void
}) {
  const [rowError, setRowError] = useState('')
  const [busy, setBusy] = useState(false)
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [mode] = useRouteParam('mode', 'all')
  const [enabled] = useRouteParam('enabled', 'all')
  const [typeFilter] = useRouteParam('type', 'all')
  const [sort] = useRouteParam('sort', 'site')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const [actionRow, setActionRow] = useState<string | null>(null)
  const { network } = useNetworkFilter()

  // Create form
  const [draft, setDraft] = useState<ProbeDraft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const createSummary = useErrorSummary(formErrors.length > 0)
  const [warnings, setWarnings] = useState<string[]>([])

  // Inline edit
  const [editID, setEditID] = useState<string | null>(null)
  const [editDraft, setEditDraft] = useState<ProbeDraft | null>(null)
  const [editErrors, setEditErrors] = useState<string[]>([])
  const editSummary = useErrorSummary(editErrors.length > 0)
  // Actions → Edit unmounts the floating menu with focus inside it, so the
  // opened editor must take focus itself or keyboard users land back at the
  // document root with the expansion unannounced. The request is keyed to
  // the target probe and survives until its editor mounts — switching away
  // from another editor takes extra commits (a confirmed discard even lands
  // one with editID null), and the route still naming the target is what
  // distinguishes that from an abandoned request. A rejected switch (Stay)
  // leaves the route elsewhere, so the next transition drops the request
  // instead of stealing focus from the active input on a later keystroke.
  // Deep-linked opens never set it — no focus stealing on load.
  const focusEditor = useRef<string | null>(null)
  useEffect(() => {
    if (focusEditor.current === null) return
    const target = focusEditor.current
    if (target === editID && editDraft !== null) {
      focusEditor.current = null
      document.getElementById(`probe-editor-${editID}-${visibleSurface()}`)?.focus()
      return
    }
    if (selectedProbe !== target) focusEditor.current = null
  }, [editID, editDraft, selectedProbe])
  const scrolledProbe = useRef<string | null>(null)
  const feedback = useSettingsMutation()
  const blankProbe = newDraft(initialPlane(plane))
  const createGuard = useConcurrentSettingsDraft({
    id: 'new-probe',
    label: 'New probe',
    loaded: blankProbe,
    current: draft ?? blankProbe,
    editing: draft !== null,
    discard: () => {
      setDraft(null)
      setFormErrors([])
    },
    reload: setDraft,
  })
  const { pinnedID: pinnedProbeID, reconcile: reconcilePin } = useStickyPin(selectedProbe)

  const probeParams = new URLSearchParams({
    limit: String(PROBE_PAGE),
    offset: String(pinnedProbeID ? 0 : (page - 1) * PROBE_PAGE),
    sort,
    order,
  })
  if (pinnedProbeID) probeParams.set('q', pinnedProbeID)
  else if (queryParam.trim()) probeParams.set('q', queryParam.trim())
  if (mode !== 'all') probeParams.set('mode', mode)
  if (enabled !== 'all') probeParams.set('enabled', enabled)
  if (typeFilter !== 'all') probeParams.set('type', typeFilter)
  if (network) probeParams.set('network', network)
  const requestURL = '/api/v1/config/probes?' + probeParams.toString()

  // Static per server version: fetch once on entry (and on an explicit
  // retry), independently from the 30-second configuration poll.
  const {
    data: registry,
    error: registryError,
    reload: reloadRegistry,
  } = usePolledResource<ProbeTypesResponse>('/api/v1/config/probe-types', {
    pollMs: null,
    onAuthError,
    logLabel: 'probe type registry',
  })

  const {
    data,
    error: listError,
    loadedKey,
    reload,
  } = usePolledResource<ProbesConfigResponse>(requestURL, { onAuthError, logLabel: 'probe settings' })
  reconcilePin(Boolean(data?.probes.some((probe) => probe.id === selectedProbe)))
  const loadedRequestURL = typeof loadedKey === 'string' ? loadedKey : ''

  const {
    data: formOptions,
    error: optionsError,
    reload: reloadOptions,
  } = usePolledResource(
    () =>
      Promise.all([
        apiGet<MeshesConfigResponse>('/api/v1/config/meshes'),
        apiGet<SitesResponse>('/api/v1/sites'),
        apiGet<TargetsConfigResponse>('/api/v1/config/targets'),
      ]).then(([meshRes, sitesRes, targetsRes]) => ({
        meshes: meshRes.meshes.map((m) => m.name),
        sites: sitesRes.sites.map((s) => s.name),
        // Agent-kind targets are excluded: they carry no address/port/URL
        // (mesh expansion resolves peers), so the server rejects direct
        // probes against them.
        targets: targetsRes.targets.filter((t) => t.kind === 'external').map((t) => t.name),
      })),
    { onAuthError, logLabel: 'probe form options' },
  )
  const meshes = formOptions?.meshes ?? []
  const sites = formOptions?.sites ?? []
  const targets = formOptions?.targets ?? []
  // The list and options pollers used to race on one error slot; they keep
  // sharing one render slot, with the list's failure taking precedence.
  const error = listError ?? optionsError

  const loadedEditProbe = editID ? data?.probes.find((probe) => probe.id === editID) : undefined
  const loadedEdit: ProbeEditSnapshot | null = loadedEditProbe
    ? { exists: true, draft: draftFrom(loadedEditProbe), enabled: loadedEditProbe.enabled }
    : null
  const currentEdit: ProbeEditSnapshot | null =
    editDraft && loadedEdit ? { ...loadedEdit, draft: editDraft } : loadedEdit
  const editGuard = useConcurrentSettingsDraft({
    id: `probe-edit:${editID ?? 'none'}`,
    label: loadedEditProbe ? `Probe ${assignmentLabel(loadedEditProbe)}` : 'Probe',
    loaded: loadedEdit,
    current: currentEdit,
    editing: editDraft !== null,
    discard: () => {
      setEditID(null)
      setEditDraft(null)
      setEditErrors([])
    },
    reload: (latest) => {
      if (latest.exists) setEditDraft(latest.draft)
      else {
        setEditID(null)
        setEditDraft(null)
      }
    },
  })

  const create = async () => {
    if (!draft) return
    const specs = paramSpecsFor(registry, draft.type, draft.mode)
    const { errors, body } = validate(draft, specs)
    setFormErrors(errors)
    if (!body) {
      feedback.error(`New probe: ${errors.join('; ')}`)
      createSummary.request()
      return
    }
    setBusy(true)
    try {
      // A mesh template inherits its mesh's plane and the server rejects
      // the combination, so only a direct probe names one — and a scoped
      // caller always must, or the write resolves to a plane it cannot see.
      const assignment =
        draft.mode === 'mesh'
          ? { mesh: draft.mesh }
          : {
              site: draft.site,
              target: draft.target,
              ...networkField(draft.network),
            }
      const res = await apiPost<{ warnings?: string[] }>('/api/v1/config/probes', {
        ...assignment,
        type: draft.type,
        ...body,
      })
      // The probe was created; warnings describe what it will actually
      // measure when that is unlikely to match the intent. They persist
      // until the next create, since the form closes on success.
      setWarnings(res.warnings ?? [])
      setDraft(null)
      feedback.success(res.warnings?.length ? 'Probe added with warnings.' : 'Probe added.')
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      createSummary.request()
      feedback.error(`Probe was not added: ${message}`)
    } finally {
      setBusy(false)
    }
  }

  const saveEdit = async (p: ProbeConfig) => {
    if (!editDraft) return
    const specs = paramSpecsFor(registry, p.type, p.mesh ? 'mesh' : 'direct')
    const { errors, body } = validate(editDraft, specs)
    setEditErrors(errors)
    if (!body) {
      feedback.error(`Probe ${assignmentLabel(p)}: ${errors.join('; ')}`)
      editSummary.request()
      return
    }
    setBusy(true)
    try {
      const currentServer = await editGuard.checkForConflict(async () => {
        try {
          const latest = await apiGet<ProbeConfig>('/api/v1/config/probes/' + p.id)
          return { exists: true, draft: draftFrom(latest), enabled: latest.enabled }
        } catch (requestError) {
          if (requestError instanceof ApiError && requestError.status === 404) {
            return { exists: false, draft: newDraft(initialPlane(plane)), enabled: false }
          }
          throw requestError
        }
      })
      if (!currentServer) return
      const res = await apiPut<{ warnings?: string[] }>('/api/v1/config/probes/' + p.id, {
        ...body,
        enabled: p.enabled,
      })
      // An edit can reach the same questionable configuration a create can
      // (clearing dns.resolver on a mesh dns probe, say), so it reports the
      // same advisory rather than saving silently.
      setWarnings(res.warnings ?? [])
      setEditID(null)
      setEditDraft(null)
      editGuard.release()
      onSelectedProbe('', 'replace')
      feedback.success(res.warnings?.length ? 'Probe saved with warnings.' : 'Probe saved.')
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setEditErrors([message])
      editSummary.request()
      feedback.error(`Probe was not saved: ${message}`)
    } finally {
      setBusy(false)
    }
  }

  const setEnabled = async (p: ProbeConfig, nextEnabled: boolean) => {
    setBusy(true)
    setRowError('')
    try {
      let latest: ProbeConfig
      try {
        latest = await apiGet<ProbeConfig>('/api/v1/config/probes/' + p.id)
      } catch (requestError) {
        if (requestError instanceof ApiError && requestError.status === 404) {
          feedback.conflict(`Probe ${assignmentLabel(p)}`, () => void reload())
          return
        }
        throw requestError
      }
      if (serverSnapshotChanged(mutationSnapshot(p), mutationSnapshot(latest))) {
        feedback.conflict(`Probe ${assignmentLabel(p)}`, () => void reload())
        return
      }
      const res = await apiPut<{ warnings?: string[] }>('/api/v1/config/probes/' + p.id, {
        ...mutationSnapshot(latest),
        enabled: nextEnabled,
      })
      // Re-enabling a probe configured before the advisory existed is the
      // one moment an upgraded installation hears about it, so this write
      // reports warnings like the others.
      setWarnings(res.warnings ?? [])
      feedback.success(nextEnabled ? 'Probe enabled.' : 'Probe disabled.')
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setRowError(message)
      feedback.error(`Probe state was not changed: ${message}`)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (p: ProbeConfig) => {
    setBusy(true)
    setRowError('')
    try {
      await apiDelete('/api/v1/config/probes/' + p.id)
      if (selectedProbe === p.id) {
        editGuard.release()
        setEditID(null)
        setEditDraft(null)
        onSelectedProbe('', 'replace')
      }
      feedback.success('Probe deleted.')
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setRowError(message)
      feedback.error(`Probe was not deleted: ${message}`)
    } finally {
      setBusy(false)
    }
  }

  const probes = data?.probes ?? []
  // Single-network installs never see the network picker or labels.
  const multiNetwork = plane.kind !== 'implicit'

  useEffect(() => {
    if (!selectedProbe) {
      scrolledProbe.current = null
      if (editID !== null) {
        setEditID(null)
        setEditDraft(null)
      }
      return
    }
    if (!data) return
    const selected = data.probes.find((probe) => probe.id === selectedProbe)
    if (!selected) {
      if (pinnedProbeID && loadedRequestURL !== requestURL) return
      onSelectedProbe('', 'replace')
      return
    }
    if (editID !== selectedProbe) {
      setEditID(selectedProbe)
      setEditDraft(draftFrom(selected))
      setEditErrors([])
    }
    if (scrolledProbe.current !== selectedProbe) {
      const row = document.getElementById(`settings-probe-${selectedProbe}-${visibleSurface()}`)
      if (!row) return
      row.scrollIntoView({ block: 'nearest' })
      scrolledProbe.current = selectedProbe
    }
  }, [data, editID, loadedRequestURL, onSelectedProbe, pinnedProbeID, requestURL, selectedProbe])

  const pageMeta = data?.page ?? { limit: PROBE_PAGE, offset: 0, total: probes.length, has_more: false }
  const pageCount = Math.max(1, Math.ceil(pageMeta.total / PROBE_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  const initialError = !registry ? registryError : !data ? error : null
  if (initialError !== null && (!data || !registry)) {
    return (
      <SettingsPageError
        title="Probes unavailable"
        subject="probes"
        error={initialError}
        onRetry={() => {
          void reloadRegistry()
          void reload()
          void reloadOptions()
        }}
      />
    )
  }
  if (!data || !registry) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading probes…
      </div>
    )
  }

  const numField = (
    d: ProbeDraft,
    set: (fn: (d: ProbeDraft) => ProbeDraft) => void,
    summary: ReturnType<typeof useErrorSummary>,
    label: string,
    unit: string,
    key: 'intervalS' | 'timeoutS' | 'trainCount' | 'trainSpacingMs',
    placeholder = '',
  ) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          inputMode="decimal"
          value={d[key]}
          placeholder={placeholder}
          disabled={busy}
          aria-describedby={summary.describedby}
          onChange={(e) => {
            set((prev) => ({ ...prev, [key]: e.target.value }))
          }}
        />
        <span className="hint">{unit}</span>
      </span>
    </label>
  )

  const paramFields = (
    d: ProbeDraft,
    set: (fn: (d: ProbeDraft) => ProbeDraft) => void,
    summary: ReturnType<typeof useErrorSummary>,
  ) => {
    const specs = paramSpecsFor(registry, d.type, d.mode)
    if (specs.length === 0) return null
    const setParam = (key: string, value: string) =>
      set((prev) => ({ ...prev, params: { ...prev.params, [key]: value } }))
    return (
      <div className="config-form-grid">
        {specs.map((spec) => {
          const required = d.mode === 'mesh' ? spec.required_mesh : spec.required_direct
          const label = spec.key + (required ? ' (required)' : '')
          if (spec.kind === 'bool') {
            return (
              <label key={spec.key} className="config-param-check">
                <input
                  type="checkbox"
                  checked={d.params[spec.key] === 'true'}
                  disabled={busy}
                  aria-describedby={summary.describedby}
                  onChange={(e) => setParam(spec.key, e.target.checked ? 'true' : '')}
                />
                <span>{spec.key}</span>
                <span className="hint">{spec.hint}</span>
              </label>
            )
          }
          if (spec.kind === 'enum') {
            return (
              <label key={spec.key} className="threshold-field">
                <span className="eyebrow">{label}</span>
                <span className="threshold-input">
                  <select
                    value={d.params[spec.key] ?? ''}
                    disabled={busy}
                    aria-describedby={summary.describedby}
                    onChange={(e) => setParam(spec.key, e.target.value)}
                  >
                    <option value="">default</option>
                    {(spec.enum ?? []).map((v) => (
                      <option key={v} value={v}>
                        {v}
                      </option>
                    ))}
                  </select>
                  <span className="hint">{spec.hint}</span>
                </span>
              </label>
            )
          }
          if (spec.kind === 'int') {
            return (
              <label key={spec.key} className="threshold-field">
                <span className="eyebrow">{label}</span>
                <span className="threshold-input">
                  <input
                    type="text"
                    inputMode="numeric"
                    value={d.params[spec.key] ?? ''}
                    placeholder={spec.hint}
                    disabled={busy}
                    aria-describedby={summary.describedby}
                    onChange={(e) => setParam(spec.key, e.target.value)}
                  />
                  {spec.min !== undefined && spec.max !== undefined && (
                    <span className="hint">
                      {spec.min}–{spec.max}
                    </span>
                  )}
                </span>
              </label>
            )
          }
          return (
            <label key={spec.key} className="threshold-field">
              <span className="eyebrow">{label}</span>
              <span className="threshold-input">
                <input
                  type="text"
                  value={d.params[spec.key] ?? ''}
                  placeholder={spec.hint}
                  disabled={busy}
                  aria-describedby={summary.describedby}
                  onChange={(e) => setParam(spec.key, e.target.value)}
                />
              </span>
            </label>
          )
        })}
      </div>
    )
  }

  const cadenceFields = (
    d: ProbeDraft,
    set: (fn: (d: ProbeDraft) => ProbeDraft) => void,
    summary: ReturnType<typeof useErrorSummary>,
  ) => (
    <div className="config-form-grid">
      {numField(d, set, summary, 'Interval', 's', 'intervalS')}
      {numField(d, set, summary, 'Timeout', 's', 'timeoutS')}
      {numField(d, set, summary, 'Train count', 'pkts', 'trainCount', 'default 10 (icmp)')}
      {numField(d, set, summary, 'Train spacing', 'ms', 'trainSpacingMs', 'default 200')}
    </div>
  )

  const setCreateDraft = (fn: (d: ProbeDraft) => ProbeDraft) => setDraft((d) => fn(d ?? newDraft(initialPlane(plane))))
  const setEditDraftFn = (fn: (d: ProbeDraft) => ProbeDraft) => setEditDraft((d) => (d ? fn(d) : d))

  const createDraft = draft ?? newDraft(initialPlane(plane))

  const columns: DataTableColumn<ProbeConfig>[] = [
    {
      key: 'type',
      label: 'Type',
      sortKey: 'type',
      priority: 'status',
      className: 'mono',
      render: (probe) => probe.type,
    },
    {
      key: 'assignment',
      label: 'Assignment',
      sortKey: 'site',
      priority: 'identity',
      className: 'mono',
      render: (probe) => (
        <>
          {assignmentLabel(probe)}
          {multiNetwork && <span className="chip">{probe.network}</span>}
        </>
      ),
    },
    {
      key: 'state',
      label: 'State',
      sortKey: 'enabled',
      priority: 'primary',
      render: (probe) => (
        <span className={'chip' + (probe.enabled ? '' : ' chip-alert')}>{probe.enabled ? 'enabled' : 'disabled'}</span>
      ),
    },
    { key: 'interval', label: 'Interval', priority: 'primary', render: (probe) => `${probe.interval_ms / 1000}s` },
    { key: 'timeout', label: 'Timeout', priority: 'secondary', render: (probe) => `${probe.timeout_ms / 1000}s` },
    {
      key: 'params',
      label: 'Params',
      priority: 'secondary',
      className: 'mono config-params-cell',
      render: paramsSummary,
    },
    {
      key: 'updated',
      label: 'Updated',
      sortKey: 'updated',
      priority: 'secondary',
      render: (probe) => (
        <>
          {fmtAgo(probe.updated_at)}
          {probe.updated_by ? ` by ${probe.updated_by}` : ''}
        </>
      ),
    },
  ]

  const editPanel = (probe: ProbeConfig, surface: 'desktop' | 'mobile') => {
    if (editID !== probe.id || !editDraft) return null
    return (
      <div className="config-form" id={`probe-editor-${probe.id}-${surface}`} tabIndex={-1}>
        <h3 className="eyebrow">
          Edit {probe.type} · {assignmentLabel(probe)}
          <span className="hint"> — type, assignment, and network are fixed; delete and re-create to re-target</span>
        </h3>
        {cadenceFields(editDraft, setEditDraftFn, editSummary)}
        {paramFields(editDraft, setEditDraftFn, editSummary)}
        {editErrors.length > 0 && (
          <ul className="error threshold-errors" id={editSummary.id} ref={editSummary.ref} tabIndex={-1}>
            {editErrors.map((message) => (
              <li key={message}>{message}</li>
            ))}
          </ul>
        )}
        <div className="threshold-foot">
          <span className="hint">Edits keep the probe's series history and incident state.</span>
          <span className="threshold-actions">
            <button type="button" className="secondary-button" disabled={busy} onClick={() => closeEditor(probe)}>
              Cancel
            </button>
            <button className="primary" disabled={busy || !editGuard.dirty} onClick={() => saveEdit(probe)}>
              {busy ? 'Saving…' : 'Save changes'}
            </button>
          </span>
        </div>
      </div>
    )
  }

  const selectProbe = (key: string | null) => {
    onSelectedProbe(key ?? '', key === null ? 'replace' : 'push')
  }

  // Cancel unmounts itself with the editor; hand focus back to the row's
  // Actions toggle instead of dropping it on the document.
  const closeEditor = (probe: ProbeConfig) => {
    document
      .getElementById(`settings-probe-${probe.id}-${visibleSurface()}`)
      ?.querySelector<HTMLElement>('.data-table-actions-toggle')
      ?.focus()
    selectProbe(null)
  }

  return (
    <>
      {(error !== null || registryError !== null) && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Measurement workload</span>
            <h2>Probes</h2>
          </div>
          <span className="hint">Changes reach agents within ~30s · refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Direct probes run from every agent on the probe's network at a site against one target; mesh templates expand
          over every ordered pair of member sites, pairing only agents on the mesh's network. Type, assignment, and
          network are fixed once created — cadence, params, and enabled state edit in place so history stays continuous.
        </p>
        {rowError && (
          <ul className="error threshold-errors">
            <li>{rowError}</li>
          </ul>
        )}
        {warnings.length > 0 && (
          <div className="inline-alert" role="status">
            <strong>Saved, with a caveat.</strong>
            <ul>
              {warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
            <button className="linklike" onClick={() => setWarnings([])}>
              Dismiss
            </button>
          </div>
        )}
        <div className="view-toolbar data-table-toolbar">
          <label className="search-field">
            <span className="sr-only">Search probes</span>
            <input
              type="search"
              placeholder="Search assignment or type"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value)
                if (selectedProbe) onSelectedProbe('', 'replace')
              }}
            />
          </label>
          <label className="compact-select">
            <span>Mode</span>
            <select
              value={mode}
              onChange={(event) => updateRouteParams({ mode: event.target.value, page: null, probe: null })}
            >
              <option value="all">All modes</option>
              <option value="direct">Direct</option>
              <option value="mesh">Mesh</option>
            </select>
          </label>
          <label className="compact-select">
            <span>State</span>
            <select
              value={enabled}
              onChange={(event) => updateRouteParams({ enabled: event.target.value, page: null, probe: null })}
            >
              <option value="all">All states</option>
              <option value="true">Enabled</option>
              <option value="false">Disabled</option>
            </select>
          </label>
          <label className="compact-select">
            <span>Type</span>
            <select
              value={typeFilter}
              onChange={(event) => updateRouteParams({ type: event.target.value, page: null, probe: null })}
            >
              <option value="all">All types</option>
              {registry.types.map((item) => (
                <option key={item.type} value={item.type}>
                  {item.type}
                </option>
              ))}
            </select>
          </label>
        </div>
        <DataTable
          label="Probe settings"
          rows={probes}
          rowKey={(probe) => probe.id}
          rowID={(probe) => 'settings-probe-' + probe.id}
          rowClassName={(probe) =>
            `${probe.enabled ? '' : 'probe-disabled'}${selectedProbe === probe.id ? ' selected-row' : ''}`
          }
          columns={columns}
          sort={{ key: sort, order: order === 'desc' ? 'desc' : 'asc' }}
          onSortChange={(next) =>
            updateRouteParams({
              sort: next.key === 'site' ? null : next.key,
              order: next.order === 'asc' ? null : next.order,
              page: null,
              probe: null,
            })
          }
          page={pageMeta}
          onPageChange={(next) => updateRouteParams({ page: next === 1 ? null : next, probe: null })}
          resultLabel="probes"
          emptyTitle={
            query || mode !== 'all' || enabled !== 'all' || typeFilter !== 'all'
              ? 'No matching probes'
              : 'No probes configured'
          }
          emptyDescription={
            query || mode !== 'all' || enabled !== 'all' || typeFilter !== 'all'
              ? 'Change the search text or filters.'
              : 'Add one below; agents pick it up within ~30 seconds.'
          }
          disclosure={
            canWrite
              ? {
                  expandedKey: selectedProbe || null,
                  retainMissing: Boolean(pinnedProbeID && loadedRequestURL !== requestURL),
                  onExpandedKeyChange: selectProbe,
                  label: (_probe, expanded) => (expanded ? 'Close editor' : 'Edit probe'),
                  render: editPanel,
                  // Desktop rows open the editor via Actions → Edit; the
                  // panel's Cancel button closes it.
                  desktopTrigger: false,
                }
              : {
                  expandedKey: expandedRow,
                  onExpandedKeyChange: setExpandedRow,
                  label: (_probe, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
                  desktop: false,
                }
          }
          actions={
            canWrite
              ? {
                  openKey: actionRow,
                  onOpenKeyChange: setActionRow,
                  label: (probe) => `Actions for ${assignmentLabel(probe)}`,
                  render: (probe) => (
                    <>
                      <button
                        type="button"
                        className="secondary-button"
                        disabled={busy}
                        onClick={() => {
                          setActionRow(null)
                          // Already open: no state transition will fire the
                          // effect, and the editor is mounted — focus it now.
                          if (editID === probe.id) {
                            document.getElementById(`probe-editor-${probe.id}-${visibleSurface()}`)?.focus()
                          } else {
                            focusEditor.current = probe.id
                          }
                          selectProbe(probe.id)
                        }}
                      >
                        Edit
                      </button>
                      {probe.enabled ? (
                        <ConfirmButton
                          label="Disable"
                          resource={`Probe ${assignmentLabel(probe)}`}
                          consequence="This stops new measurements and may close incidents that depend on this probe."
                          disabled={busy}
                          onConfirm={() => setEnabled(probe, false)}
                        />
                      ) : (
                        <button
                          type="button"
                          className="secondary-button"
                          disabled={busy}
                          onClick={() => setEnabled(probe, true)}
                        >
                          Enable
                        </button>
                      )}
                      <ConfirmButton
                        label="Delete"
                        resource={`Probe ${assignmentLabel(probe)}`}
                        consequence={
                          probe.mesh
                            ? 'This removes every expanded pair workload and retires their measurement series.'
                            : 'This permanently removes the probe and retires its measurement series.'
                        }
                        disabled={busy}
                        onConfirm={() => remove(probe)}
                      />
                    </>
                  ),
                }
              : undefined
          }
        />
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">Add probe</h3>
            <div className="config-form-grid">
              {/* A <label> would be wrong here: this field holds a button
                  group, not a form control, so there is nothing for the
                  label to name. The group carries its own accessible name. */}
              <div className="threshold-field">
                <span className="eyebrow">Assignment</span>
                <span
                  className="control-group config-mode"
                  role="group"
                  aria-label="Probe assignment"
                  aria-describedby={createSummary.describedby}
                >
                  <button
                    type="button"
                    className={createDraft.mode === 'mesh' ? 'active' : ''}
                    aria-pressed={createDraft.mode === 'mesh'}
                    disabled={busy}
                    onClick={() =>
                      setCreateDraft((d) => ({
                        ...d,
                        mode: 'mesh',
                        params: {},
                        // A direct-only type (http, ntp) cannot be a mesh template.
                        type: registry?.types.find((t) => t.type === d.type)?.direct_only ? 'icmp' : d.type,
                      }))
                    }
                  >
                    Mesh
                  </button>
                  <button
                    type="button"
                    className={createDraft.mode === 'direct' ? 'active' : ''}
                    aria-pressed={createDraft.mode === 'direct'}
                    disabled={busy}
                    onClick={() => setCreateDraft((d) => ({ ...d, mode: 'direct', params: {} }))}
                  >
                    Direct
                  </button>
                </span>
              </div>
              <label className="threshold-field">
                <span className="eyebrow">Type</span>
                <span className="threshold-input">
                  <select
                    value={createDraft.type}
                    disabled={busy}
                    aria-describedby={createSummary.describedby}
                    onChange={(e) => setCreateDraft((d) => ({ ...d, type: e.target.value, params: {} }))}
                  >
                    {(registry?.types ?? [])
                      .filter((t) => !(createDraft.mode === 'mesh' && t.direct_only))
                      .map((t) => (
                        <option key={t.type} value={t.type}>
                          {t.type}
                        </option>
                      ))}
                  </select>
                </span>
              </label>
              {createDraft.mode === 'mesh' ? (
                <label className="threshold-field">
                  <span className="eyebrow">Mesh group</span>
                  <span className="threshold-input">
                    <select
                      value={createDraft.mesh}
                      disabled={busy}
                      aria-describedby={createSummary.describedby}
                      onChange={(e) => setCreateDraft((d) => ({ ...d, mesh: e.target.value }))}
                    >
                      <option value="">pick…</option>
                      {meshes.map((m) => (
                        <option key={m} value={m}>
                          {m}
                        </option>
                      ))}
                    </select>
                  </span>
                </label>
              ) : (
                <>
                  <label className="threshold-field">
                    <span className="eyebrow">Site</span>
                    <span className="threshold-input">
                      <select
                        value={createDraft.site}
                        disabled={busy}
                        aria-describedby={createSummary.describedby}
                        onChange={(e) => setCreateDraft((d) => ({ ...d, site: e.target.value }))}
                      >
                        <option value="">pick…</option>
                        {sites.map((s) => (
                          <option key={s} value={s}>
                            {s}
                          </option>
                        ))}
                      </select>
                    </span>
                  </label>
                  <label className="threshold-field">
                    <span className="eyebrow">Target</span>
                    <span className="threshold-input">
                      <select
                        value={createDraft.target}
                        disabled={busy}
                        aria-describedby={createSummary.describedby}
                        onChange={(e) => setCreateDraft((d) => ({ ...d, target: e.target.value }))}
                      >
                        <option value="">pick…</option>
                        {targets.map((t) => (
                          <option key={t} value={t}>
                            {t}
                          </option>
                        ))}
                      </select>
                    </span>
                  </label>
                  <PlaneField
                    choice={plane}
                    value={createDraft.network}
                    onChange={(v) => setCreateDraft((d) => ({ ...d, network: v }))}
                    disabled={busy}
                    hint="only this network's agents at the site run it"
                  />
                </>
              )}
            </div>
            {cadenceFields(createDraft, setCreateDraft, createSummary)}
            {paramFields(createDraft, setCreateDraft, createSummary)}
            {formErrors.length > 0 && (
              <ul className="error threshold-errors" id={createSummary.id} ref={createSummary.ref} tabIndex={-1}>
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">Agents at the affected sites start probing within ~30 seconds.</span>
              <span className="threshold-actions">
                <button
                  className="primary"
                  disabled={
                    busy || !draft || !createGuard.dirty || (createDraft.mode === 'direct' && !planeReady(plane))
                  }
                  onClick={create}
                >
                  {busy ? 'Saving…' : 'Add probe'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
