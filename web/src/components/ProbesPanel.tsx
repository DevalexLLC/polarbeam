import { useEffect, useMemo, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import type {
  MeshesConfigResponse,
  ParamSpec,
  ProbeConfig,
  ProbesConfigResponse,
  ProbeTypesResponse,
  SitesResponse,
  TargetsConfigResponse,
} from '../types'
import ConfirmButton from './ConfirmButton'

const POLL_MS = 30_000
const PROBE_PAGE = 25
// Prober defaults, mirrored from the server's shared validation
// (internal/server/probeadmin): a zero train budgets as 10 × 200 ms.
const DEFAULT_TRAIN_COUNT = 10
const DEFAULT_TRAIN_SPACING_MS = 200
// Stable identity for the not-yet-loaded case: a fresh `[]` per render would
// change the memo key every render and defeat the pagination memo below.
const NO_PROBES: ProbeConfig[] = []

interface ProbeDraft {
  mode: 'mesh' | 'direct'
  mesh: string
  site: string
  target: string
  type: string
  intervalS: string
  timeoutS: string
  trainCount: string
  trainSpacingMs: string
  params: Record<string, string>
}

function newDraft(): ProbeDraft {
  return {
    mode: 'mesh',
    mesh: '',
    site: '',
    target: '',
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
    type: p.type,
    intervalS: String(p.interval_ms / 1000),
    timeoutS: String(p.timeout_ms / 1000),
    trainCount: p.train_count ? String(p.train_count) : '',
    trainSpacingMs: p.train_spacing_ms ? String(p.train_spacing_ms) : '',
    params: { ...p.params },
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
  isAdmin,
  onAuthError,
}: {
  isAdmin: boolean
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<ProbesConfigResponse | null>(null)
  const [registry, setRegistry] = useState<ProbeTypesResponse | null>(null)
  const [meshes, setMeshes] = useState<string[]>([])
  const [sites, setSites] = useState<string[]>([])
  const [targets, setTargets] = useState<string[]>([])
  const [error, setError] = useState('')
  const [rowError, setRowError] = useState('')
  const [visible, setVisible] = useState(PROBE_PAGE)
  const [busy, setBusy] = useState(false)

  // Create form
  const [draft, setDraft] = useState<ProbeDraft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [savedFlash, setSavedFlash] = useState(false)
  const [warnings, setWarnings] = useState<string[]>([])

  // Inline edit
  const [editID, setEditID] = useState<string | null>(null)
  const [editDraft, setEditDraft] = useState<ProbeDraft | null>(null)
  const [editErrors, setEditErrors] = useState<string[]>([])

  // The registry is static per server version: one fetch, no poll.
  useEffect(() => {
    apiGet<ProbeTypesResponse>('/api/v1/config/probe-types').then(setRegistry).catch(onAuthError)
  }, [onAuthError])

  useEffect(() => {
    let cancelled = false
    const load = () => {
      Promise.all([
        apiGet<ProbesConfigResponse>('/api/v1/config/probes'),
        apiGet<MeshesConfigResponse>('/api/v1/config/meshes'),
        apiGet<SitesResponse>('/api/v1/sites'),
        apiGet<TargetsConfigResponse>('/api/v1/config/targets'),
      ])
        .then(([probes, meshRes, sitesRes, targetsRes]) => {
          if (cancelled) return
          setData(probes)
          setMeshes(meshRes.meshes.map((m) => m.name))
          setSites(sitesRes.sites.map((s) => s.name))
          // Agent-kind targets are excluded: they carry no address/port/URL
          // (mesh expansion resolves peers), so the server rejects direct
          // probes against them.
          setTargets(targetsRes.targets.filter((t) => t.kind === 'external').map((t) => t.name))
          setError('')
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          setError(err instanceof Error ? err.message : String(err))
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError])

  const reload = () => apiGet<ProbesConfigResponse>('/api/v1/config/probes').then(setData).catch(onAuthError)

  const create = async () => {
    if (!draft) return
    const specs = paramSpecsFor(registry, draft.type, draft.mode)
    const { errors, body } = validate(draft, specs)
    setFormErrors(errors)
    if (!body) return
    setBusy(true)
    try {
      const assignment = draft.mode === 'mesh' ? { mesh: draft.mesh } : { site: draft.site, target: draft.target }
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
      setSavedFlash(true)
      await reload()
    } catch (err) {
      onAuthError(err)
      setFormErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setBusy(false)
    }
  }

  const saveEdit = async (p: ProbeConfig) => {
    if (!editDraft) return
    const specs = paramSpecsFor(registry, p.type, p.mesh ? 'mesh' : 'direct')
    const { errors, body } = validate(editDraft, specs)
    setEditErrors(errors)
    if (!body) return
    setBusy(true)
    try {
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
      await reload()
    } catch (err) {
      onAuthError(err)
      setEditErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setBusy(false)
    }
  }

  const setEnabled = async (p: ProbeConfig, enabled: boolean) => {
    setBusy(true)
    setRowError('')
    try {
      const res = await apiPut<{ warnings?: string[] }>('/api/v1/config/probes/' + p.id, {
        interval_ms: p.interval_ms,
        timeout_ms: p.timeout_ms,
        train_count: p.train_count,
        train_spacing_ms: p.train_spacing_ms,
        params: p.params,
        enabled,
      })
      // Re-enabling a probe configured before the advisory existed is the
      // one moment an upgraded installation hears about it, so this write
      // reports warnings like the others.
      setWarnings(res.warnings ?? [])
      await reload()
    } catch (err) {
      onAuthError(err)
      setRowError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const remove = async (p: ProbeConfig) => {
    setBusy(true)
    setRowError('')
    try {
      await apiDelete('/api/v1/config/probes/' + p.id)
      await reload()
    } catch (err) {
      onAuthError(err)
      setRowError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const probes = data?.probes ?? NO_PROBES
  const shown = useMemo(() => probes.slice(0, visible), [probes, visible])

  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Probes unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
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
            setSavedFlash(false)
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

  const setCreateDraft = (fn: (d: ProbeDraft) => ProbeDraft) => setDraft((d) => fn(d ?? newDraft()))
  const setEditDraftFn = (fn: (d: ProbeDraft) => ProbeDraft) => setEditDraft((d) => (d ? fn(d) : d))

  const createDraft = draft ?? newDraft()

  return (
    <>
      {error && (
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
          Direct probes run from every agent at a site against one target; mesh templates expand over every ordered pair
          of member sites. Type and assignment are fixed once created — cadence, params, and enabled state edit in place
          so history stays continuous.
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
        {probes.length === 0 ? (
          <div className="empty-state">
            <strong>No probes configured</strong>
            <span>Add one below; agents pick it up within ~30 seconds.</span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Type</th>
                  <th>Assignment</th>
                  <th>Interval</th>
                  <th>Timeout</th>
                  <th>Params</th>
                  <th>State</th>
                  <th>Updated</th>
                  {isAdmin && (
                    <th className="actions-col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {shown.map((p) => (
                  <>
                    <tr key={p.id} className={p.enabled ? '' : 'probe-disabled'}>
                      <td data-label="Type" className="mono">
                        {p.type}
                      </td>
                      <td data-label="Assignment" className="mono">
                        {assignmentLabel(p)}
                      </td>
                      <td data-label="Interval">{p.interval_ms / 1000}s</td>
                      <td data-label="Timeout">{p.timeout_ms / 1000}s</td>
                      <td data-label="Params" className="mono config-params-cell">
                        {paramsSummary(p)}
                      </td>
                      <td data-label="State">
                        <span className={'chip' + (p.enabled ? '' : ' chip-alert')}>
                          {p.enabled ? 'enabled' : 'disabled'}
                        </span>
                      </td>
                      <td data-label="Updated">
                        {fmtAgo(p.updated_at)}
                        {p.updated_by ? ` by ${p.updated_by}` : ''}
                      </td>
                      {isAdmin && (
                        <td data-label="Actions" className="config-actions">
                          <button
                            type="button"
                            className="secondary-button"
                            disabled={busy}
                            aria-expanded={editID === p.id}
                            onClick={() => {
                              if (editID === p.id) {
                                setEditID(null)
                                setEditDraft(null)
                              } else {
                                setEditID(p.id)
                                setEditDraft(draftFrom(p))
                                setEditErrors([])
                              }
                            }}
                          >
                            {editID === p.id ? 'Close' : 'Edit'}
                          </button>
                          <button
                            type="button"
                            className="secondary-button"
                            disabled={busy}
                            onClick={() => setEnabled(p, !p.enabled)}
                          >
                            {p.enabled ? 'Disable' : 'Enable'}
                          </button>
                          <ConfirmButton
                            label="Delete"
                            confirmLabel={p.mesh ? 'Confirm? Removes every expanded pair series' : 'Confirm delete?'}
                            disabled={busy}
                            onConfirm={() => remove(p)}
                          />
                        </td>
                      )}
                    </tr>
                    {editID === p.id && editDraft && (
                      <tr key={p.id + '-edit'} className="config-edit-row">
                        <td colSpan={isAdmin ? 8 : 7}>
                          <div className="config-form">
                            <h3 className="eyebrow">
                              Edit {p.type} · {assignmentLabel(p)}
                              <span className="hint">
                                {' '}
                                — type and assignment are fixed; delete and re-create to re-target
                              </span>
                            </h3>
                            {cadenceFields(editDraft, setEditDraftFn)}
                            {paramFields(editDraft, setEditDraftFn)}
                            {editErrors.length > 0 && (
                              <ul className="error threshold-errors">
                                {editErrors.map((e) => (
                                  <li key={e}>{e}</li>
                                ))}
                              </ul>
                            )}
                            <div className="threshold-foot">
                              <span className="hint">Edits keep the probe's series history and incident state.</span>
                              <button className="primary" disabled={busy} onClick={() => saveEdit(p)}>
                                {busy ? 'Saving…' : 'Save changes'}
                              </button>
                            </div>
                          </div>
                        </td>
                      </tr>
                    )}
                  </>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {probes.length > visible && (
          <div className="progressive-footer">
            <button type="button" className="secondary-button" onClick={() => setVisible((v) => v + PROBE_PAGE)}>
              Show {Math.min(PROBE_PAGE, probes.length - visible)} more
            </button>
          </div>
        )}
        {isAdmin && (
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
                {savedFlash && <span className="hint">saved</span>}
                <button className="primary" disabled={busy || !draft} onClick={create}>
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
