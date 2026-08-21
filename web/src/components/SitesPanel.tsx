import { useEffect, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import type { SitesConfigResponse, SiteConfig } from '../types'
import ConfirmButton from './ConfirmButton'

const POLL_MS = 30_000

interface Draft {
  name: string
  display_name: string
  location: string
  latitude: string
  longitude: string
}

const emptyDraft: Draft = { name: '', display_name: '', location: '', latitude: '', longitude: '' }

// Mirrors the server's siteadmin validation; server 400s render verbatim as
// a backstop. Coordinates are both-or-neither — clearing both unplaces the
// site (the PUT is full-state).
function validate(d: Draft): { errors: string[]; latitude: number | null; longitude: number | null } {
  const errors: string[] = []
  if (d.name.trim() === '') errors.push('name is required')
  const hasLat = d.latitude.trim() !== ''
  const hasLon = d.longitude.trim() !== ''
  let latitude: number | null = null
  let longitude: number | null = null
  if (hasLat !== hasLon) {
    errors.push('latitude and longitude must be set together (clear both to unplace the site)')
  } else if (hasLat) {
    latitude = Number(d.latitude)
    longitude = Number(d.longitude)
    if (!Number.isFinite(latitude) || latitude < -90 || latitude > 90) {
      errors.push('latitude must be between -90 and 90')
      latitude = null
    }
    if (!Number.isFinite(longitude) || longitude < -180 || longitude > 180) {
      errors.push('longitude must be between -180 and 180')
      longitude = null
    }
  }
  return { errors, latitude, longitude }
}

function refCount(s: SiteConfig): number {
  return s.agent_count + s.mesh_count + s.probe_count
}

function refSummary(s: SiteConfig): string {
  return `${s.agent_count} agent(s), ${s.mesh_count} mesh membership(s), ${s.probe_count} probe config(s)`
}

export default function SitesPanel({
  canWrite,
  onAuthError,
}: {
  canWrite: boolean
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<SitesConfigResponse | null>(null)
  const [error, setError] = useState('')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing site (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = () => {
      apiGet<SitesConfigResponse>('/api/v1/config/sites')
        .then((res) => {
          if (!cancelled) {
            setData(res)
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

  const reload = () => apiGet<SitesConfigResponse>('/api/v1/config/sites').then(setData).catch(onAuthError)

  const save = async () => {
    if (!draft) return
    const { errors, latitude, longitude } = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      const body = {
        name: draft.name.trim(),
        display_name: draft.display_name.trim(),
        location: draft.location.trim(),
        latitude,
        longitude,
      }
      if (editing) {
        await apiPut('/api/v1/config/sites/' + encodeURIComponent(body.name), body)
      } else {
        await apiPost('/api/v1/config/sites', body)
      }
      setDraft(null)
      setEditing(false)
      setSavedFlash(true)
      await reload()
    } catch (err) {
      onAuthError(err)
      setFormErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

  const remove = async (s: SiteConfig) => {
    try {
      await apiDelete('/api/v1/config/sites/' + encodeURIComponent(s.name))
      await reload()
    } catch (err) {
      onAuthError(err)
      setError(err instanceof Error ? err.message : String(err))
    }
  }

  const startEdit = (s: SiteConfig) => {
    setEditing(true)
    setSavedFlash(false)
    setFormErrors([])
    setDraft({
      name: s.name,
      display_name: s.display_name,
      location: s.location,
      // 0 is a real coordinate — check null, never truthiness.
      latitude: s.latitude !== null ? String(s.latitude) : '',
      longitude: s.longitude !== null ? String(s.longitude) : '',
    })
  }

  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Sites unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading sites…
      </div>
    )
  }

  const field = (label: string, key: keyof Draft, placeholder: string, locked = false) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          value={draft?.[key] ?? ''}
          placeholder={placeholder}
          disabled={saving || locked}
          onChange={(e) => {
            setSavedFlash(false)
            setDraft((d) => ({ ...(d ?? emptyDraft), [key]: e.target.value }))
          }}
        />
      </span>
    </label>
  )

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
            <span className="eyebrow">Locations</span>
            <h2>Sites</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Agents enroll into a site; meshes and direct probes are assigned by site. A site referenced by agents, meshes,
          or probes cannot be deleted until those references are removed.
        </p>
        {data.sites.length === 0 ? (
          <div className="empty-state">
            <strong>No sites</strong>
            <span>Add one below, then issue a join token from the Enrollment tab to enroll its first agent.</span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Display name</th>
                  <th>Location</th>
                  <th>Coordinates</th>
                  <th>Agents</th>
                  <th>Meshes</th>
                  <th>Probes</th>
                  <th>Created</th>
                  {canWrite && (
                    <th className="actions-col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {data.sites.map((s) => (
                  <tr key={s.id}>
                    <td data-label="Name" className="mono">
                      {s.name}
                    </td>
                    <td data-label="Display name">{s.display_name || '—'}</td>
                    <td data-label="Location">{s.location || '—'}</td>
                    <td data-label="Coordinates" className="mono">
                      {s.latitude !== null && s.longitude !== null
                        ? `${s.latitude.toFixed(4)}, ${s.longitude.toFixed(4)}`
                        : '—'}
                    </td>
                    <td data-label="Agents">{s.agent_count}</td>
                    <td data-label="Meshes">{s.mesh_count}</td>
                    <td data-label="Probes">{s.probe_count}</td>
                    <td data-label="Created">{fmtAgo(s.created_at)}</td>
                    {canWrite && (
                      <td data-label="Actions" className="config-actions">
                        <button type="button" className="secondary-button" onClick={() => startEdit(s)}>
                          Edit
                        </button>
                        <ConfirmButton
                          label="Delete"
                          confirmLabel="Confirm delete? Unused join tokens go with it"
                          disabled={refCount(s) > 0}
                          title={refCount(s) > 0 ? `In use by ${refSummary(s)} — remove those first` : undefined}
                          onConfirm={() => remove(s)}
                        />
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">{editing ? `Edit ${draft?.name}` : 'Add site'}</h3>
            <div className="config-form-grid">
              {field('Name', 'name', 'unique handle, e.g. nyc', editing)}
              {field('Display name', 'display_name', 'e.g. New York')}
              {field('Location', 'location', 'free text, e.g. New York, US')}
              {field('Latitude', 'latitude', '-90..90, with longitude')}
              {field('Longitude', 'longitude', '-180..180, with latitude')}
            </div>
            {formErrors.length > 0 && (
              <ul className="error threshold-errors">
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">
                Coordinates place the site on the Overview map. Clear both fields to remove it from the map.
              </span>
              <span className="threshold-actions">
                {savedFlash && <span className="hint">saved</span>}
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
                <button className="primary" onClick={save} disabled={saving || !draft}>
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add site'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
