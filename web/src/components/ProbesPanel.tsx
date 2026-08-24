import { useEffect, useRef, useState } from 'react'
import { ApiError, apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useNetworkFilter } from '../networkFilter'
import { updateRouteParams } from '../routeState'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { serverSnapshotChanged } from '../settingsSnapshot'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import type {
  MeshesConfigResponse,
  ParamSpec,
  ProbeConfig,
  ProbesConfigResponse,
  ProbeTypesResponse,
  SitesResponse,
  TargetsConfigResponse,
} from '../types'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import PlaneField from './PlaneField'
import SettingsPageError from './SettingsPageError'

const POLL_MS = 30_000
const PROBE_PAGE = 25
// Prober defaults, mirrored from the server's shared validation
// (internal/server/probeadmin): a zero train budgets as 10 × 200 ms.
const DEFAULT_TRAIN_COUNT = 10
const DEFAULT_TRAIN_SPACING_MS = 200
// Path MTU prober defaults (internal/agent/probes/pathmtu.go), used only
// for the client-side mtu.min < mtu.max cross-check.
const DEFAULT_MTU_MIN = 1280
const DEFAULT_MTU_MAX = 1500

interface ProbeDraft {
  mode: 'mesh' | 'direct'
  mesh: string
  site: string
  target: string
  // Direct mode only; mesh templates inherit the mesh's network and the
  // server rejects the combination.
  network: string
  type: string
  intervalS: string
  timeoutS: string
  trainCount: string
  trainSpacingMs: string
  params: Record<string, string>
}

interface ProbeEditSnapshot {
  exists: boolean
  draft: ProbeDraft
  enabled: boolean
}

function newDraft(plane: string): ProbeDraft {
  return {
    mode: 'mesh',
    mesh: '',
    site: '',
    target: '',
    network: plane,
    type: 'icmp',
    intervalS: '30',
    timeoutS: '5',
    trainCount: '',
    trainSpacingMs: '',
    params: {},
  }
}

function draftFrom(p: ProbeConfig): ProbeDraft {
  return {
    mode: p.mesh ? 'mesh' : 'direct',
    mesh: p.mesh ?? '',
    site: p.site ?? '',
    target: p.target ?? '',
    network: p.network,
    type: p.type,
    intervalS: String(p.interval_ms / 1000),
    timeoutS: String(p.timeout_ms / 1000),
    trainCount: p.train_count ? String(p.train_count) : '',
    trainSpacingMs: p.train_spacing_ms ? String(p.train_spacing_ms) : '',
    params: { ...p.params },
  }
}

function mutationSnapshot(p: ProbeConfig) {
  return {
    interval_ms: p.interval_ms,
    timeout_ms: p.timeout_ms,
    train_count: p.train_count,
    train_spacing_ms: p.train_spacing_ms,
    params: p.params,
    enabled: p.enabled,
  }
}

// cleanParams drops empty values: an absent key means "prober default", and
// the server rejects empty strings for required keys with a clear message.
function cleanParams(params: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(params)) {
    if (v.trim() !== '') out[k] = v.trim()
  }
  return out
}

// Mirrors the server's shared validation (probeadmin.ValidateSettings and
// the param registry rules) so nearly every mistake is caught before the
// round-trip; server 400s render verbatim as a backstop.
function validate(d: ProbeDraft, specs: ParamSpec[]): { errors: string[]; body: object | null } {
  const errors: string[] = []
  const num = (label: string, s: string, empty: number): number => {
    if (s.trim() === '') return empty
    const n = Number(s)
    if (!Number.isFinite(n)) {
      errors.push(`${label} must be a number`)
      return NaN
    }
    return n
  }
  const intervalS = num('interval', d.intervalS, NaN)
  const timeoutS = num('timeout', d.timeoutS, NaN)
  const trainCount = num('train count', d.trainCount, 0)
  const trainSpacing = num('train spacing', d.trainSpacingMs, 0)

  if (d.mode === 'mesh' && d.mesh === '') errors.push('pick a mesh group')
  if (d.mode === 'direct' && (d.site === '' || d.target === '')) errors.push('pick a site and a target')

  if (Number.isFinite(intervalS) && Number.isFinite(timeoutS)) {
    if (intervalS <= 0 || timeoutS <= 0) errors.push('interval and timeout must be positive')
    else if (timeoutS >= intervalS) errors.push('timeout must be shorter than interval')
  } else if (Number.isNaN(intervalS) || Number.isNaN(timeoutS)) {
    if (d.intervalS.trim() === '') errors.push('interval is required')
    if (d.timeoutS.trim() === '') errors.push('timeout is required')
  }
  if (trainCount < 0 || trainSpacing < 0) errors.push('train count and spacing must not be negative')
  else if (trainCount === 0 && trainSpacing > 0) errors.push('train spacing requires a train count')
  else if (trainCount > 0 && Number.isFinite(timeoutS)) {
    const spacing = trainSpacing || DEFAULT_TRAIN_SPACING_MS
    if ((trainCount || DEFAULT_TRAIN_COUNT) * spacing >= timeoutS * 1000) {
      errors.push('the probe train must fit inside the timeout')
    }
  }

  const params = cleanParams(d.params)
  for (const spec of specs) {
    const required = d.mode === 'mesh' ? spec.required_mesh : spec.required_direct
    const v = params[spec.key]
    if (required && v === undefined) errors.push(`${spec.key} is required`)
    if (v === undefined) continue
    if (spec.kind === 'port') {
      const n = Number(v)
      if (!Number.isInteger(n) || n < 1 || n > 65535) errors.push(`${spec.key} must be an integer between 1 and 65535`)
    }
    if (spec.kind === 'status' && !/^[1-5](xx|[0-9]{2})$/.test(v)) {
      errors.push(`${spec.key} must be an exact status ("200") or a class ("2xx")`)
    }
    if (spec.kind === 'int') {
      const n = Number(v)
      const min = spec.min ?? Number.MIN_SAFE_INTEGER
      const max = spec.max ?? Number.MAX_SAFE_INTEGER
      if (!Number.isInteger(n) || n < min || n > max) {
        errors.push(`${spec.key} must be an integer between ${min} and ${max}`)
      }
    }
  }

  // Mirrors probeadmin's path_mtu cross-check: effective values with the
  // prober defaults substituted, so a lone out-of-range bound is caught.
  if (d.type === 'path_mtu') {
    const effMin = Number(params['mtu.min'] ?? DEFAULT_MTU_MIN)
    const effMax = Number(params['mtu.max'] ?? DEFAULT_MTU_MAX)
    if (Number.isInteger(effMin) && Number.isInteger(effMax) && effMin >= effMax) {
      errors.push(`mtu.min (${effMin}) must be less than mtu.max (${effMax})`)
    }
  }

  if (errors.length > 0) return { errors, body: null }
  return {
    errors: [],
    body: {
      interval_ms: Math.round(intervalS * 1000),
      timeout_ms: Math.round(timeoutS * 1000),
      train_count: trainCount,
      train_spacing_ms: trainSpacing,
      params,
    },
  }
}

// The registry drives which param fields exist for a type + assignment
// mode; a key the agent would silently ignore simply cannot be entered.
function paramSpecsFor(registry: ProbeTypesResponse | null, type: string, mode: 'mesh' | 'direct'): ParamSpec[] {
  const specs = registry?.types.find((t) => t.type === type)?.params ?? []
  return specs.filter((s) => !(s.mesh_only && mode === 'direct'))
}

function assignmentLabel(p: ProbeConfig): string {
  return p.mesh ? `mesh:${p.mesh}` : `${p.site} → ${p.target}`
}

function paramsSummary(p: ProbeConfig): string {
  const entries = Object.entries(p.params)
  if (entries.length === 0) return '—'
  return entries.map(([k, v]) => `${k}=${v}`).join(', ')
}

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
  const [data, setData] = useState<ProbesConfigResponse | null>(null)
  const [loadedRequestURL, setLoadedRequestURL] = useState('')
  const [registry, setRegistry] = useState<ProbeTypesResponse | null>(null)
  const [registryError, setRegistryError] = useState<unknown>(null)
  const [meshes, setMeshes] = useState<string[]>([])
  const [sites, setSites] = useState<string[]>([])
  const [targets, setTargets] = useState<string[]>([])
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
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
  const [warnings, setWarnings] = useState<string[]>([])

  // Inline edit
  const [editID, setEditID] = useState<string | null>(null)
  const [editDraft, setEditDraft] = useState<ProbeDraft | null>(null)
  const [editErrors, setEditErrors] = useState<string[]>([])
  const scrolledProbe = useRef<string | null>(null)
  const pinnedProbe = useRef<string | null>(selectedProbe)
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

  if (!selectedProbe) pinnedProbe.current = null
  else if (pinnedProbe.current !== selectedProbe) {
    pinnedProbe.current = data?.probes.some((probe) => probe.id === selectedProbe) ? null : selectedProbe
  }
  const pinnedProbeID = pinnedProbe.current === selectedProbe ? selectedProbe : null

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
  useEffect(() => {
    let cancelled = false
    apiGet<ProbeTypesResponse>('/api/v1/config/probe-types')
      .then((res) => {
        if (!cancelled) {
          setRegistry(res)
          setRegistryError(null)
        }
      })
      .catch((err) => {
        if (cancelled) return
        onAuthError(err)
        console.error('probe type registry request failed', err)
        setRegistryError(err)
      })
    return () => {
      cancelled = true
    }
  }, [onAuthError, retryKey])

  useEffect(() => {
    let cancelled = false
    const load = () => {
      apiGet<ProbesConfigResponse>(requestURL)
        .then((probes) => {
          if (cancelled) return
          setData(probes)
          setLoadedRequestURL(requestURL)
          setError(null)
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('probe settings request failed', err)
          setError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError, requestURL, retryKey])

  useEffect(() => {
    let cancelled = false
    const loadOptions = () => {
      Promise.all([
        apiGet<MeshesConfigResponse>('/api/v1/config/meshes'),
        apiGet<SitesResponse>('/api/v1/sites'),
        apiGet<TargetsConfigResponse>('/api/v1/config/targets'),
      ])
        .then(([meshRes, sitesRes, targetsRes]) => {
          if (cancelled) return
          setMeshes(meshRes.meshes.map((m) => m.name))
          setSites(sitesRes.sites.map((s) => s.name))
          // Agent-kind targets are excluded: they carry no address/port/URL
          // (mesh expansion resolves peers), so the server rejects direct
          // probes against them.
          setTargets(targetsRes.targets.filter((t) => t.kind === 'external').map((t) => t.name))
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('probe form options request failed', err)
          setError(err)
        })
    }
    loadOptions()
    const id = setInterval(loadOptions, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError, retryKey])

  const reload = () =>
    apiGet<ProbesConfigResponse>(requestURL)
      .then((probes) => {
        setData(probes)
        setLoadedRequestURL(requestURL)
      })
      .catch(onAuthError)

  const create = async () => {
    if (!draft) return
    const specs = paramSpecsFor(registry, draft.type, draft.mode)
    const { errors, body } = validate(draft, specs)
    setFormErrors(errors)
    if (!body) {
      feedback.error(`New probe: ${errors.join('; ')}`)
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
        feedback.conflict(`Probe ${assignmentLabel(p)}`, () => {
          setData((current) =>
            current
              ? { ...current, probes: current.probes.map((probe) => (probe.id === latest.id ? latest : probe)) }
              : current,
          )
        })
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
      const surface = window.matchMedia('(max-width: 760px)').matches ? 'mobile' : 'desktop'
      const row = document.getElementById(`settings-probe-${selectedProbe}-${surface}`)
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
        onRetry={() => setRetryKey((key) => key + 1)}
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
          onChange={(e) => {
            set((prev) => ({ ...prev, [key]: e.target.value }))
          }}
        />
        <span className="hint">{unit}</span>
      </span>
    </label>
  )

  const paramFields = (d: ProbeDraft, set: (fn: (d: ProbeDraft) => ProbeDraft) => void) => {
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
                  onChange={(e) => setParam(spec.key, e.target.value)}
                />
              </span>
            </label>
          )
        })}
      </div>
    )
  }

  const cadenceFields = (d: ProbeDraft, set: (fn: (d: ProbeDraft) => ProbeDraft) => void) => (
    <div className="config-form-grid">
      {numField(d, set, 'Interval', 's', 'intervalS')}
      {numField(d, set, 'Timeout', 's', 'timeoutS')}
      {numField(d, set, 'Train count', 'pkts', 'trainCount', 'default 10 (icmp)')}
      {numField(d, set, 'Train spacing', 'ms', 'trainSpacingMs', 'default 200')}
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

  const editPanel = (probe: ProbeConfig) => {
    if (editID !== probe.id || !editDraft) return null
    return (
      <div className="config-form">
        <h3 className="eyebrow">
          Edit {probe.type} · {assignmentLabel(probe)}
          <span className="hint"> — type, assignment, and network are fixed; delete and re-create to re-target</span>
        </h3>
        {cadenceFields(editDraft, setEditDraftFn)}
        {paramFields(editDraft, setEditDraftFn)}
        {editErrors.length > 0 && (
          <ul className="error threshold-errors">
            {editErrors.map((message) => (
              <li key={message}>{message}</li>
            ))}
          </ul>
        )}
        <div className="threshold-foot">
          <span className="hint">Edits keep the probe's series history and incident state.</span>
          <button className="primary" disabled={busy || !editGuard.dirty} onClick={() => saveEdit(probe)}>
            {busy ? 'Saving…' : 'Save changes'}
          </button>
        </div>
      </div>
    )
  }

  const selectProbe = (key: string | null) => {
    onSelectedProbe(key ?? '', key === null ? 'replace' : 'push')
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
                <span className="control-group config-mode" role="group" aria-label="Probe assignment">
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
            {cadenceFields(createDraft, setCreateDraft)}
            {paramFields(createDraft, setCreateDraft)}
            {formErrors.length > 0 && (
              <ul className="error threshold-errors">
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
