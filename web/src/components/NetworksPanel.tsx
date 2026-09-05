import { useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { useErrorSummary } from '../formErrors'
import type { NetworksConfigResponse, NetworkConfig } from '../types'
import { usePolledResource } from '../usePolledResource'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import SettingsFormDialog from './SettingsFormDialog'
import SettingsPageError from './SettingsPageError'

interface Draft {
  name: string
  display_name: string
}

const emptyDraft: Draft = { name: '', display_name: '' }

// Mirrors the server's configadmin network validation (name required); server 400s
// render verbatim as a backstop.
function validate(d: Draft): string[] {
  return d.name.trim() === '' ? ['name is required'] : []
}

const draftFrom = (network: NetworkConfig): Draft => ({ name: network.name, display_name: network.display_name })

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
  const { data, error, reload } = usePolledResource<NetworksConfigResponse>('/api/v1/config/networks', {
    onAuthError,
    logLabel: 'network settings',
  })
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing network (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const summary = useErrorSummary(formErrors.length > 0)
  const [saving, setSaving] = useState(false)
  const [actionRow, setActionRow] = useState<string | null>(null)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const feedback = useSettingsMutation()
  const loadedNetwork = editing && draft ? data?.networks.find((network) => network.name === draft.name) : undefined
  const loadedDraft = loadedNetwork ? draftFrom(loadedNetwork) : emptyDraft
  const guard = useConcurrentSettingsDraft({
    id: 'network-form',
    label: editing && draft ? `Network ${draft.name}` : 'New network',
    loaded: loadedDraft,
    current: draft ?? loadedDraft,
    editing: draft !== null,
    discard: () => {
      setDraft(null)
      setEditing(false)
      setFormErrors([])
    },
    reload: (latest) => {
      setDraft(latest.name ? latest : null)
      if (!latest.name) setEditing(false)
    },
  })

  const save = async () => {
    if (!draft) return
    const errors = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) {
      summary.request()
      feedback.error(`Network: ${errors.join('; ')}`)
      return
    }
    setSaving(true)
    try {
      const body = { name: draft.name.trim(), display_name: draft.display_name.trim() }
      if (editing) {
        const currentServer = await guard.checkForConflict(async () => {
          const latest = await apiGet<NetworksConfigResponse>('/api/v1/config/networks')
          const network = latest.networks.find((item) => item.name === body.name)
          return network ? draftFrom(network) : emptyDraft
        })
        if (!currentServer) return
        await apiPut('/api/v1/config/networks/' + encodeURIComponent(body.name), body)
      } else {
        await apiPost('/api/v1/config/networks', body)
      }
      setDraft(null)
      setEditing(false)
      feedback.success(editing ? `Network ${body.name} saved.` : `Network ${body.name} added.`)
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      summary.request()
      feedback.error(`Network was not saved: ${message}`)
    } finally {
      setSaving(false)
    }
  }

  const remove = async (n: NetworkConfig) => {
    try {
      await apiDelete('/api/v1/config/networks/' + encodeURIComponent(n.name))
      feedback.success(`Network ${n.name} deleted.`)
      await reload()
    } catch (err) {
      onAuthError(err)
      console.error('network delete failed', err)
      feedback.error(`Network ${n.name} was not deleted: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const startEdit = (n: NetworkConfig) => {
    setEditing(true)
    setFormErrors([])
    setDraft({ name: n.name, display_name: n.display_name })
  }

  // The dialog is open exactly while a draft exists, so opening, Cancel, a
  // route-guard discard, and a conflict reload all go through the draft.
  const openCreate = () => {
    setEditing(false)
    setFormErrors([])
    setDraft(emptyDraft)
  }

  const cancel = () => {
    setDraft(null)
    setEditing(false)
    setFormErrors([])
  }

  if (error && !data) {
    return (
      <SettingsPageError title="Networks unavailable" subject="networks" error={error} onRetry={() => void reload()} />
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

  const columns: DataTableColumn<NetworkConfig>[] = [
    { key: 'name', label: 'Name', priority: 'identity', className: 'mono', render: (n) => n.name },
    { key: 'display', label: 'Display name', priority: 'primary', render: (n) => n.display_name || '—' },
    { key: 'agents', label: 'Agents', priority: 'secondary', render: (n) => n.agent_count },
    { key: 'tokens', label: 'Tokens', priority: 'secondary', render: (n) => n.token_count },
    { key: 'meshes', label: 'Meshes', priority: 'secondary', render: (n) => n.mesh_count },
    { key: 'probes', label: 'Probes', priority: 'secondary', render: (n) => n.probe_count },
    { key: 'created', label: 'Created', priority: 'secondary', render: (n) => fmtAgo(n.created_at) },
  ]

  const field = (label: string, key: keyof Draft, placeholder: string, locked = false) => (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          value={draft?.[key] ?? ''}
          placeholder={placeholder}
          disabled={saving || locked}
          aria-describedby={summary.describedby}
          onChange={(e) => {
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
            <h2>Networks</h2>
          </div>
          <div className="card-head-actions">
            <span className="hint">Refreshes every 30s</span>
            {canWrite && (
              <button type="button" className="primary" onClick={openCreate}>
                Add network
              </button>
            )}
          </div>
        </div>
        <SettingsFormDialog
          open={draft !== null}
          title={editing ? `Edit ${draft?.name}` : 'Add network'}
          busy={saving}
          onClose={cancel}
        >
          <div className="config-form">
            <div className="config-form-grid">
              {field('Name', 'name', 'e.g. mgmt', editing)}
              {field('Display name', 'display_name', 'e.g. Management')}
            </div>
            {formErrors.length > 0 && (
              <ul className="error threshold-errors" id={summary.id} ref={summary.ref} tabIndex={-1}>
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            {guard.conflict && (
              <div className="inline-alert" role="alert">
                <strong>{guard.conflict.message}</strong>{' '}
                <button type="button" className="linklike" disabled={saving} onClick={guard.conflict.reload}>
                  Reload server version
                </button>
              </div>
            )}
            <div className="threshold-foot">
              <span className="hint">
                The name is immutable once created — it is how tokens, meshes, and probes reference the plane. An agent
                moves between networks only by re-enrolling with a token for the other one.
              </span>
              <span className="threshold-actions">
                <button type="button" className="secondary-button" disabled={saving} onClick={cancel}>
                  Cancel
                </button>
                <button className="primary" onClick={save} disabled={saving || !draft || !guard.dirty}>
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add network'}
                </button>
              </span>
            </div>
          </div>
        </SettingsFormDialog>
        <p className="section-intro">
          A network asserts that its agents can reach one another. Agents join one permanently through their join token;
          each mesh and direct probe measures within exactly one, so planes that cannot reach each other are never
          paired. Deployments with a single flat network can ignore this tab — everything lives on{' '}
          <span className="mono">default</span>.
        </p>
        <DataTable
          label="Networks"
          rows={data.networks}
          rowKey={(n) => n.id}
          columns={columns}
          emptyTitle="No networks"
          emptyDescription="Initialization seeds the default network."
          disclosure={{
            expandedKey: expandedRow,
            onExpandedKeyChange: setExpandedRow,
            label: (_network, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
            desktop: false,
          }}
          actions={
            canWrite
              ? {
                  openKey: actionRow,
                  onOpenKeyChange: setActionRow,
                  label: (n) => `Actions for ${n.name}`,
                  render: (n) => (
                    <>
                      {/* The row menu stays mounted behind the modal (as it
                          does for Delete's confirm) so closing the dialog
                          returns focus to this item. */}
                      <button type="button" className="secondary-button" onClick={() => startEdit(n)}>
                        Edit
                      </button>
                      <ConfirmButton
                        label="Delete"
                        resource={`Network ${n.name}`}
                        consequence="This permanently removes the network and its unused enrollment tokens."
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
                    </>
                  ),
                }
              : undefined
          }
        />
      </section>
    </>
  )
}
