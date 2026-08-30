import { useState } from 'react'
import { apiPost } from '../api'
import { useErrorSummary } from '../formErrors'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import type { ProbeTypesResponse } from '../types'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { newDraft, paramSpecsFor, type ProbeDraft, validate } from '../probeDraft'
import PlaneField from './PlaneField'
import ProbeDraftFields from './ProbeDraftFields'

// The "Add probe" form owns its draft, validation errors, and dirty-draft
// guard; the panel owns the shared busy flag and the advisory-warnings
// banner because both are also fed by the edit and enable/disable flows.
export default function ProbeCreateForm({
  plane,
  registry,
  meshes,
  sites,
  targets,
  busy,
  onBusyChange,
  onWarnings,
  onRefresh,
  onAuthError,
}: {
  plane: PlaneChoice
  registry: ProbeTypesResponse
  meshes: string[]
  sites: string[]
  targets: string[]
  busy: boolean
  onBusyChange: (busy: boolean) => void
  onWarnings: (warnings: string[]) => void
  onRefresh: () => Promise<void>
  onAuthError: (err: unknown) => void
}) {
  const [draft, setDraft] = useState<ProbeDraft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const createSummary = useErrorSummary(formErrors.length > 0)
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
    onBusyChange(true)
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
      onWarnings(res.warnings ?? [])
      setDraft(null)
      feedback.success(res.warnings?.length ? 'Probe added with warnings.' : 'Probe added.')
      await onRefresh()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      createSummary.request()
      feedback.error(`Probe was not added: ${message}`)
    } finally {
      onBusyChange(false)
    }
  }

  const setCreateDraft = (fn: (d: ProbeDraft) => ProbeDraft) => setDraft((d) => fn(d ?? newDraft(initialPlane(plane))))

  const createDraft = draft ?? newDraft(initialPlane(plane))

  return (
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
                  type: registry.types.find((t) => t.type === d.type)?.direct_only ? 'icmp' : d.type,
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
              {registry.types
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
      <ProbeDraftFields
        draft={createDraft}
        onChange={setCreateDraft}
        describedby={createSummary.describedby}
        busy={busy}
        registry={registry}
      />
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
            disabled={busy || !draft || !createGuard.dirty || (createDraft.mode === 'direct' && !planeReady(plane))}
            onClick={create}
          >
            {busy ? 'Saving…' : 'Add probe'}
          </button>
        </span>
      </div>
    </div>
  )
}
