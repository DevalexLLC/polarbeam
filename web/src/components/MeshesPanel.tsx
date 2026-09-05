import { useState } from 'react'
import { apiDelete, apiGet, apiPost } from '../api'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { useSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { useErrorSummary } from '../formErrors'
import type { MeshesConfigResponse, MeshConfig, SitesResponse } from '../types'
import { usePolledResource } from '../usePolledResource'
import ConfirmButton from './ConfirmButton'
import PlaneField from './PlaneField'
import SettingsFormDialog from './SettingsFormDialog'
import SettingsPageError from './SettingsPageError'

export default function MeshesPanel({
  canWrite,
  plane,
  onAuthError,
}: {
  canWrite: boolean
  plane: PlaneChoice
  onAuthError: (err: unknown) => void
}) {
  const {
    data: snapshot,
    error,
    reload,
  } = usePolledResource(
    () =>
      Promise.all([apiGet<MeshesConfigResponse>('/api/v1/config/meshes'), apiGet<SitesResponse>('/api/v1/sites')]).then(
        ([meshes, sitesRes]) => ({ meshes, sites: sitesRes.sites.map((s) => s.name) }),
      ),
    { onAuthError, logLabel: 'mesh settings' },
  )
  const data = snapshot?.meshes ?? null
  const sites = snapshot?.sites ?? []
  const [actionError, setActionError] = useState('')
  // Row actions (delete, add/remove member) share actionError's render slot
  // but must not describe the create form's fields.
  const [errorScope, setErrorScope] = useState<'create' | 'row'>('create')
  const summary = useErrorSummary(Boolean(actionError) && errorScope === 'create')
  const [newName, setNewName] = useState('')
  const [newNetworkDraft, setNewNetwork] = useState<string | null>(null)
  const newNetwork = newNetworkDraft ?? initialPlane(plane)
  // site picked in each mesh's add-member select, keyed by mesh id
  const [memberPick, setMemberPick] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  // A blank create draft is indistinguishable from "closed", so the dialog
  // carries its own open flag; every close path also clears the draft so no
  // dirty state survives behind a closed dialog.
  const [createOpen, setCreateOpen] = useState(false)
  const feedback = useSettingsMutation()
  const newMeshDirty = newName !== '' || newNetworkDraft !== null
  const closeCreate = () => {
    setNewName('')
    setNewNetwork(null)
    setActionError('')
    setCreateOpen(false)
  }
  useSettingsDraft('new-mesh', 'New mesh', newMeshDirty, closeCreate)
  useSettingsDraft('mesh-members', 'Mesh membership selection', Object.values(memberPick).some(Boolean), () => {
    setMemberPick({})
    setActionError('')
  })

  const run = async (fn: () => Promise<unknown>, successMessage: string, scope: 'create' | 'row' = 'row') => {
    setBusy(true)
    setActionError('')
    try {
      const result = await fn()
      if (result === false) return false
      feedback.success(successMessage)
      await reload()
      return true
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setErrorScope(scope)
      setActionError(message)
      feedback.error(`Mesh change failed: ${message}`)
      return false
    } finally {
      setBusy(false)
    }
  }

  if (error && !data) {
    return <SettingsPageError title="Meshes unavailable" subject="meshes" error={error} onRetry={() => void reload()} />
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading meshes…
      </div>
    )
  }

  const addable = (m: MeshConfig) => sites.filter((s) => !m.sites.includes(s))
  // The plane is worth showing whenever it is not implied: a real choice,
  // or a tenant pinned to a single plane that is not 'default'.
  const multiNetwork = plane.kind !== 'implicit'

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
            <h2>Mesh groups</h2>
          </div>
          <div className="card-head-actions">
            <span className="hint">Refreshes every 30s</span>
            {canWrite && (
              <button type="button" className="primary" onClick={() => setCreateOpen(true)}>
                Create mesh
              </button>
            )}
          </div>
        </div>
        <SettingsFormDialog open={createOpen} title="Create mesh" size="compact" busy={busy} onClose={closeCreate}>
          <div className="config-form">
            <div className="config-form-grid">
              <label className="threshold-field">
                <span className="label">Name</span>
                <span className="threshold-input">
                  <input
                    type="text"
                    value={newName}
                    placeholder="e.g. core"
                    disabled={busy}
                    aria-describedby={summary.describedby}
                    onChange={(e) => setNewName(e.target.value)}
                  />
                </span>
              </label>
              <PlaneField choice={plane} value={newNetwork} onChange={setNewNetwork} disabled={busy} />
            </div>
            {actionError && errorScope === 'create' && (
              <ul className="error threshold-errors" id={summary.id} ref={summary.ref} tabIndex={-1}>
                <li>{actionError}</li>
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">
                A new mesh has no members or probes until you add them.
                {multiNetwork &&
                  ' The network binding is permanent: templates expand only over that network’s agents at member sites.'}
              </span>
              <span className="threshold-actions">
                <button type="button" className="secondary-button" disabled={busy} onClick={closeCreate}>
                  Cancel
                </button>
                <button
                  className="primary"
                  disabled={busy || newName.trim() === '' || !newMeshDirty || !planeReady(plane)}
                  onClick={() =>
                    // The mesh POST upserts by name with omitted network
                    // meaning "keep an existing mesh's binding", so the plane
                    // must be stated whenever we have one — otherwise an
                    // existing mesh on another plane comes back as a silent
                    // success instead of the server's 409. Only a global
                    // caller on a single-plane install omits it, keeping the
                    // pre-networks request shape.
                    run(
                      () =>
                        apiPost('/api/v1/config/meshes', {
                          name: newName.trim(),
                          ...networkField(newNetwork),
                        }),
                      `Mesh ${newName.trim()} created.`,
                      'create',
                    ).then((saved) => {
                      if (saved) closeCreate()
                      else summary.request()
                    })
                  }
                >
                  Create
                </button>
              </span>
            </div>
          </div>
        </SettingsFormDialog>
        <p className="section-intro">
          Each mesh probe template expands over every ordered pair of member sites. Removing a member or deleting a mesh
          also retires the affected series and closes their open incidents.
        </p>
        {actionError && errorScope === 'row' && (
          <ul className="error threshold-errors">
            <li>{actionError}</li>
          </ul>
        )}
        {data.meshes.length === 0 ? (
          <div className="empty-state">
            <strong>No mesh groups</strong>
            <span>Create one to run the same probes between every pair of member sites.</span>
          </div>
        ) : (
          <ul className="mesh-list">
            {data.meshes.map((m) => (
              <li key={m.id} className="mesh-row">
                <div className="mesh-row-head">
                  <span className="mono">{m.name}</span>
                  {multiNetwork && <span className="chip">{m.network}</span>}
                  <span className="hint">
                    {m.sites.length} site(s) · {m.probe_count} probe template(s)
                  </span>
                  {canWrite && (
                    <ConfirmButton
                      label="Delete mesh"
                      resource={`Mesh ${m.name}`}
                      consequence={`This removes ${m.probe_count} probe template(s) and retires their measurement series.`}
                      disabled={busy}
                      onConfirm={() =>
                        run(
                          () => apiDelete('/api/v1/config/meshes/' + encodeURIComponent(m.name)),
                          `Mesh ${m.name} deleted.`,
                        )
                      }
                    />
                  )}
                </div>
                <div className="mesh-members">
                  {m.sites.length === 0 && <span className="hint">no member sites yet</span>}
                  {m.sites.map((s) => (
                    <span key={s} className="chip">
                      {s}
                      {canWrite && (
                        <ConfirmButton
                          label="Remove"
                          resource={`Site ${s} in mesh ${m.name}`}
                          consequence="This removes the site and retires its mesh series in both directions."
                          title={`Remove ${s} — retires this site's mesh series in both directions`}
                          disabled={busy}
                          onConfirm={() =>
                            run(
                              () =>
                                apiDelete(
                                  '/api/v1/config/meshes/' +
                                    encodeURIComponent(m.name) +
                                    '/members/' +
                                    encodeURIComponent(s),
                                ),
                              `Site ${s} removed from mesh ${m.name}.`,
                            )
                          }
                        />
                      )}
                    </span>
                  ))}
                  {canWrite && addable(m).length > 0 && (
                    <span className="mesh-add">
                      <label>
                        <span className="sr-only">Site to add to {m.name}</span>
                        <select
                          value={memberPick[m.id] ?? ''}
                          disabled={busy}
                          onChange={(e) => setMemberPick((p) => ({ ...p, [m.id]: e.target.value }))}
                        >
                          <option value="">add site…</option>
                          {addable(m).map((s) => (
                            <option key={s} value={s}>
                              {s}
                            </option>
                          ))}
                        </select>
                      </label>
                      <button
                        type="button"
                        className="secondary-button"
                        disabled={busy || !memberPick[m.id]}
                        onClick={() =>
                          run(
                            () =>
                              apiPost(
                                '/api/v1/config/meshes/' +
                                  encodeURIComponent(m.name) +
                                  '/members/' +
                                  encodeURIComponent(memberPick[m.id]),
                              ),
                            `Site ${memberPick[m.id]} added to mesh ${m.name}.`,
                          ).then((saved) => {
                            if (saved) setMemberPick((p) => ({ ...p, [m.id]: '' }))
                          })
                        }
                      >
                        Add
                      </button>
                    </span>
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  )
}
