import { useEffect, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import type { NetworksConfigResponse, NetworkConfig } from '../types'
import ConfirmButton from './ConfirmButton'
import SettingsPageError from './SettingsPageError'

const POLL_MS = 30_000

interface Draft {
  name: string
  display_name: string
}

const emptyDraft: Draft = { name: '', display_name: '' }

// Mirrors the server's networkadmin validation (name required); server 400s
// render verbatim as a backstop.
function validate(d: Draft): string[] {
  return d.name.trim() === '' ? ['name is required'] : []
}

// Unused join tokens are swept with the network, so they never block a
// delete — everything else must be detached first.
function refCount(n: NetworkConfig): number {
  return n.agent_count + n.mesh_count + n.probe_count + n.target_count
}

function refSummary(n: NetworkConfig): string {
  return (
    `${n.agent_count} agent(s), ${n.mesh_count} mesh(es), ` +
    `${n.probe_count} probe config(s), ${n.target_count} target(s)`
  )
}

export default function NetworksPanel({
  canWrite,
  onAuthError,
}: {
  canWrite: boolean
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<NetworksConfigResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing network (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = () => {
      apiGet<NetworksConfigResponse>('/api/v1/config/networks')
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setError(null)
          }
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('network settings request failed', err)
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

  const reload = () => apiGet<NetworksConfigResponse>('/api/v1/config/networks').then(setData).catch(onAuthError)

  const save = async () => {
    if (!draft) return
    const errors = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      const body = { name: draft.name.trim(), display_name: draft.display_name.trim() }
      if (editing) {
        await apiPut('/api/v1/config/networks/' + encodeURIComponent(body.name), body)
      } else {
        await apiPost('/api/v1/config/networks', body)
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

  const remove = async (n: NetworkConfig) => {
    try {
      await apiDelete('/api/v1/config/networks/' + encodeURIComponent(n.name))
      await reload()
    } catch (err) {
      onAuthError(err)
      console.error('network delete failed', err)
      setError(err)
    }
  }

  const startEdit = (n: NetworkConfig) => {
    setEditing(true)
    setSavedFlash(false)
    setFormErrors([])
    setDraft({ name: n.name, display_name: n.display_name })
  }

  if (error && !data) {
    return (
      <SettingsPageError
        title="Networks unavailable"
        subject="networks"
        error={error}
        onRetry={() => setRetryKey((key) => key + 1)}
      />
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading networks…
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
      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Connectivity planes</span>
            <h2>Networks</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          A network asserts that its agents can reach one another. Agents join one permanently through their join token;
          each mesh and direct probe measures within exactly one, so planes that cannot reach each other are never
          paired. Deployments with a single flat network can ignore this tab — everything lives on{' '}
          <span className="mono">default</span>.
        </p>
        <div className="scroll-x">
          <table className="events">
            <thead>
              <tr>
                <th>Name</th>
                <th>Display name</th>
                <th>Agents</th>
                <th>Tokens</th>
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
              {data.networks.map((n) => (
                <tr key={n.id}>
                  <td data-label="Name" className="mono">
                    {n.name}
                  </td>
                  <td data-label="Display name">{n.display_name || '—'}</td>
                  <td data-label="Agents">{n.agent_count}</td>
                  <td data-label="Tokens">{n.token_count}</td>
                  <td data-label="Meshes">{n.mesh_count}</td>
                  <td data-label="Probes">{n.probe_count}</td>
                  <td data-label="Created">{fmtAgo(n.created_at)}</td>
                  {canWrite && (
                    <td data-label="Actions" className="config-actions">
                      <button type="button" className="secondary-button" onClick={() => startEdit(n)}>
                        Edit
                      </button>
                      <ConfirmButton
                        label="Delete"
                        confirmLabel="Confirm delete? Unused join tokens go with it"
                        disabled={n.name === 'default' || refCount(n) > 0}
                        title={
                          n.name === 'default'
                            ? 'The default network is the seeded fallback for enrollment and cannot be deleted'
                            : refCount(n) > 0
                              ? `In use by ${refSummary(n)} — remove those first`
                              : undefined
                        }
                        onConfirm={() => remove(n)}
                      />
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">{editing ? `Edit ${draft?.name}` : 'Add network'}</h3>
            <div className="config-form-grid">
              {field('Name', 'name', 'unique handle, e.g. mgmt', editing)}
              {field('Display name', 'display_name', 'e.g. Management')}
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
                The name is immutable once created — it is how tokens, meshes, and probes reference the plane. An agent
                moves between networks only by re-enrolling with a token for the other one.
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
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add network'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
