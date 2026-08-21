import { useEffect, useState } from 'react'
import { apiDelete, apiGet, apiPost } from '../api'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import type { MeshesConfigResponse, MeshConfig, SitesResponse } from '../types'
import ConfirmButton from './ConfirmButton'
import PlaneField from './PlaneField'

const POLL_MS = 30_000

export default function MeshesPanel({
  canWrite,
  plane,
  onAuthError,
}: {
  canWrite: boolean
  plane: PlaneChoice
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<MeshesConfigResponse | null>(null)
  const [sites, setSites] = useState<string[]>([])
  const [error, setError] = useState('')
  const [actionError, setActionError] = useState('')
  const [newName, setNewName] = useState('')
  const [newNetworkDraft, setNewNetwork] = useState<string | null>(null)
  const newNetwork = newNetworkDraft ?? initialPlane(plane)
  // site picked in each mesh's add-member select, keyed by mesh id
  const [memberPick, setMemberPick] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = () => {
      Promise.all([apiGet<MeshesConfigResponse>('/api/v1/config/meshes'), apiGet<SitesResponse>('/api/v1/sites')])
        .then(([meshes, sitesRes]) => {
          if (!cancelled) {
            setData(meshes)
            setSites(sitesRes.sites.map((s) => s.name))
            setError('')
          }
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

  const reload = () => apiGet<MeshesConfigResponse>('/api/v1/config/meshes').then(setData).catch(onAuthError)

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setActionError('')
    try {
      await fn()
      await reload()
    } catch (err) {
      onAuthError(err)
      setActionError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Meshes unavailable</h2>
        <p>{error}</p>
      </div>
    )
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
      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Full-mesh groups</span>
            <h2>Mesh groups</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Each mesh probe template expands over every ordered pair of member sites. Removing a member or deleting a mesh
          also retires the affected series and closes their open incidents.
        </p>
        {actionError && (
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
                      confirmLabel={`Confirm delete? Removes ${m.probe_count} probe template(s) and their series`}
                      disabled={busy}
                      onConfirm={() => run(() => apiDelete('/api/v1/config/meshes/' + encodeURIComponent(m.name)))}
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
                          label="×"
                          confirmLabel="remove?"
                          title={`Remove ${s} — retires this site's mesh series in both directions`}
                          disabled={busy}
                          onConfirm={() =>
                            run(() =>
                              apiDelete(
                                '/api/v1/config/meshes/' +
                                  encodeURIComponent(m.name) +
                                  '/members/' +
                                  encodeURIComponent(s),
                              ),
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
                          run(() =>
                            apiPost(
                              '/api/v1/config/meshes/' +
                                encodeURIComponent(m.name) +
                                '/members/' +
                                encodeURIComponent(memberPick[m.id]),
                            ),
                          ).then(() => setMemberPick((p) => ({ ...p, [m.id]: '' })))
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
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">Create mesh</h3>
            <div className="config-form-grid">
              <label className="threshold-field">
                <span className="eyebrow">Name</span>
                <span className="threshold-input">
                  <input
                    type="text"
                    value={newName}
                    placeholder="e.g. core"
                    disabled={busy}
                    onChange={(e) => setNewName(e.target.value)}
                  />
                </span>
              </label>
              <PlaneField choice={plane} value={newNetwork} onChange={setNewNetwork} disabled={busy} />
            </div>
            <div className="threshold-foot">
              <span className="hint">
                A new mesh has no members or probes until you add them.
                {multiNetwork &&
                  ' The network binding is permanent: templates expand only over that network’s agents at member sites.'}
              </span>
              <button
                className="primary"
                disabled={busy || newName.trim() === '' || !planeReady(plane)}
                onClick={() =>
                  // The mesh POST upserts by name with omitted network
                  // meaning "keep an existing mesh's binding", so the plane
                  // must be stated whenever we have one — otherwise an
                  // existing mesh on another plane comes back as a silent
                  // success instead of the server's 409. Only a global
                  // caller on a single-plane install omits it, keeping the
                  // pre-networks request shape.
                  run(() =>
                    apiPost('/api/v1/config/meshes', {
                      name: newName.trim(),
                      ...networkField(newNetwork),
                    }),
                  ).then(() => setNewName(''))
                }
              >
                Create
              </button>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
