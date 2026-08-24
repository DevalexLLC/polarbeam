import { useEffect, useState } from 'react'
import { apiDelete, apiGet, apiPost } from '../api'
import { fmtAgo } from '../format'
import type { Caps } from '../caps'
import { canWriteRow } from '../caps'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { inheritRouteNetwork } from '../routeState'
import type { TargetsConfigResponse, TargetConfig } from '../types'
import ConfirmButton from './ConfirmButton'
import PlaneField from './PlaneField'
import SettingsPageError from './SettingsPageError'

const POLL_MS = 30_000

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
  const [data, setData] = useState<TargetsConfigResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing target (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = () => {
      apiGet<TargetsConfigResponse>('/api/v1/config/targets')
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setError(null)
          }
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('target settings request failed', err)
          setError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError, retryKey])

  const reload = () => apiGet<TargetsConfigResponse>('/api/v1/config/targets').then(setData).catch(onAuthError)

  const save = async () => {
    if (!draft) return
    const { errors, port } = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      await apiPost('/api/v1/config/targets', {
        name: draft.name.trim(),
        address: draft.address.trim(),
        port,
        url: draft.url.trim(),
        ...networkField(draft.network),
      })
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

  const remove = async (t: TargetConfig) => {
    try {
      await apiDelete('/api/v1/config/targets/' + encodeURIComponent(t.name))
      await reload()
    } catch (err) {
      onAuthError(err)
      console.error('target delete failed', err)
      setError(err)
    }
  }

  const startEdit = (t: TargetConfig) => {
    setEditing(true)
    setSavedFlash(false)
    setFormErrors([])
    setDraft({
      name: t.name,
      address: t.address ?? '',
      port: t.port ? String(t.port) : '',
      url: t.url ?? '',
      network: t.network,
    })
  }

  if (error && !data) {
    return (
      <SettingsPageError
        title="Targets unavailable"
        subject="targets"
        error={error}
        onRetry={() => setRetryKey((key) => key + 1)}
      />
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

  const externals = data.targets.filter((t) => t.kind === 'external')
  const agents = data.targets.filter((t) => t.kind === 'agent')
  // Show the owner once there is anything to distinguish: a real choice of
  // plane, a tenant pinned to one, or any tenant-owned row in the list.
  const showNetworkColumn = plane.kind !== 'implicit' || externals.some((t) => t.network !== '')

  const blankDraft = (): Draft => ({ ...emptyDraft, network: initialPlane(plane) })

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
            <h2>External targets</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Hosts and URLs that site agents probe in addition to their mesh peers. A target in use by probes cannot be
          deleted until those probes are removed.
        </p>
        {externals.length === 0 ? (
          <div className="empty-state">
            <strong>No external targets</strong>
            <span>Add one below to probe infrastructure beyond the agent mesh.</span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Address</th>
                  {showNetworkColumn && <th>Network</th>}
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
                {externals.map((t) => (
                  <tr key={t.id}>
                    <td data-label="Name" className="mono">
                      <a href={inheritRouteNetwork('#/target/' + encodeURIComponent(t.id))}>{t.name}</a>
                    </td>
                    <td data-label="Address" className="mono">
                      {t.url ? t.url : t.port ? `${t.address}:${t.port}` : t.address}
                    </td>
                    {showNetworkColumn && (
                      <td data-label="Network">
                        {t.network === '' ? <span className="hint">all networks</span> : t.network}
                      </td>
                    )}
                    <td data-label="Probes">{t.probe_count}</td>
                    <td data-label="Created">{fmtAgo(t.created_at)}</td>
                    {canWrite && (
                      <td data-label="Actions" className="config-actions">
                        {canWriteRow(caps, t.network) ? (
                          <>
                            <button type="button" className="secondary-button" onClick={() => startEdit(t)}>
                              Edit
                            </button>
                            <ConfirmButton
                              label="Delete"
                              confirmLabel="Confirm delete?"
                              disabled={t.probe_count > 0}
                              title={
                                t.probe_count > 0
                                  ? `In use by ${t.probe_count} probe(s) — remove those first`
                                  : undefined
                              }
                              onConfirm={() => remove(t)}
                            />
                          </>
                        ) : (
                          // Published by the operator for every plane: yours
                          // to probe, not to change.
                          <span className="hint">operator-owned</span>
                        )}
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
              <ul className="error threshold-errors">
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">New probes can use this target as soon as it is saved.</span>
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
                <button
                  className="primary"
                  onClick={save}
                  disabled={saving || !draft || (!editing && !planeReady(plane))}
                >
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add target'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Enrollment-managed</span>
            <h2>Agent targets</h2>
          </div>
        </div>
        <p className="section-intro">
          Created automatically when an agent enrolls and removed with it; mesh probes resolve peers through these.
          Read-only here.
        </p>
        {agents.length === 0 ? (
          <div className="empty-state">
            <strong>No agent targets</strong>
            <span>Enroll an agent and its peer target appears here.</span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Probes</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {agents.map((t) => (
                  <tr key={t.id}>
                    <td data-label="Name" className="mono">
                      <a href={inheritRouteNetwork('#/target/' + encodeURIComponent(t.id))}>{t.name}</a>
                    </td>
                    <td data-label="Probes">{t.probe_count}</td>
                    <td data-label="Created">{fmtAgo(t.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  )
}
