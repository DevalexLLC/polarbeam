import { useEffect, useRef, useState } from 'react'
import { ApiError, apiGet, apiPut } from '../api'
import { useErrorSummary } from '../formErrors'
import { useNetworkFilter } from '../networkFilter'
import { updateRouteParams } from '../routeState'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
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
  newDraft,
  paramSpecsFor,
  type ProbeDraft,
  type ProbeEditSnapshot,
  validate,
} from '../probeDraft'
import type { PlaneChoice } from '../plane'
import { initialPlane } from '../plane'
import DataTable from './DataTable'
import ProbeCreateForm from './ProbeCreateForm'
import ProbeDraftFields from './ProbeDraftFields'
import ProbeRowActions, { probeColumns } from './ProbeRowActions'
import ProbesToolbar from './ProbesToolbar'
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

  // Advisory warnings share one banner across create, edit, and
  // enable/disable — every probe write can report the same caveats.
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

  // ProbeRowActions owns the enable/disable and delete mutations; deleting
  // the probe whose editor is open must also release the edit guard and
  // clear the route selection, which only this panel can do.
  const removedProbe = (p: ProbeConfig) => {
    if (selectedProbe === p.id) {
      editGuard.release()
      setEditID(null)
      setEditDraft(null)
      onSelectedProbe('', 'replace')
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

  const setEditDraftFn = (fn: (d: ProbeDraft) => ProbeDraft) => setEditDraft((d) => (d ? fn(d) : d))

  const columns = probeColumns(multiNetwork)

  const editPanel = (probe: ProbeConfig, surface: 'desktop' | 'mobile') => {
    if (editID !== probe.id || !editDraft) return null
    return (
      <div className="config-form" id={`probe-editor-${probe.id}-${surface}`} tabIndex={-1}>
        <h3 className="eyebrow">
          Edit {probe.type} · {assignmentLabel(probe)}
          <span className="hint"> — type, assignment, and network are fixed; delete and re-create to re-target</span>
        </h3>
        <ProbeDraftFields
          draft={editDraft}
          onChange={setEditDraftFn}
          describedby={editSummary.describedby}
          busy={busy}
          registry={registry}
        />
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
        <ProbesToolbar
          query={query}
          onQueryChange={(value) => {
            setQuery(value)
            if (selectedProbe) onSelectedProbe('', 'replace')
          }}
          mode={mode}
          enabled={enabled}
          typeFilter={typeFilter}
          types={registry.types.map((item) => item.type)}
        />
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
                    <ProbeRowActions
                      probe={probe}
                      busy={busy}
                      onBusyChange={setBusy}
                      onRowError={setRowError}
                      onWarnings={setWarnings}
                      onEdit={() => {
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
                      onRemoved={removedProbe}
                      onRefresh={reload}
                      onAuthError={onAuthError}
                    />
                  ),
                }
              : undefined
          }
        />
        {canWrite && (
          <ProbeCreateForm
            plane={plane}
            registry={registry}
            meshes={meshes}
            sites={sites}
            targets={targets}
            busy={busy}
            onBusyChange={setBusy}
            onWarnings={setWarnings}
            onRefresh={reload}
            onAuthError={onAuthError}
          />
        )}
      </section>
    </>
  )
}
