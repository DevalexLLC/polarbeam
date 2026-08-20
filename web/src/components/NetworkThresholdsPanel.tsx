import { Fragment, useState } from 'react'
import { apiDelete } from '../api'
import type { Caps } from '../caps'
import { fmtAgo } from '../format'
import type { PlaneChoice } from '../plane'
import { initialPlane } from '../plane'
import type { NetworkThreshold, SettingsResponse } from '../types'
import ConfirmButton from './ConfirmButton'
import PlaneField from './PlaneField'
import ThresholdOverrideForm from './ThresholdOverrideForm'

// The layer between the deployment-wide row and the per-pair overrides: a
// network states its own idea of "normal" without touching the global
// settings every other tenant grades against. Resolution is per metric,
// most specific first — pair+network, pair, network, global — so an empty
// field here still falls through to the global row.
//
// Unlike the pair endpoints the plane is a path segment, not a query param.
const networkThresholdURL = (network: string) => `/api/v1/settings/network-thresholds/${encodeURIComponent(network)}`

function valueCell(us: number | null, unit: 'ms' | '%', inheritedValue: number) {
  const scale = unit === 'ms' ? 1000 : 1
  if (us == null) {
    return <span className="hint">inherits {inheritedValue / scale + ' ' + unit}</span>
  }
  return <>{us / scale + ' ' + unit}</>
}

export default function NetworkThresholdsPanel({
  settings,
  caps,
  canWrite,
  plane,
  onChanged,
  onAuthError,
}: {
  settings: SettingsResponse
  caps: Caps
  canWrite: boolean
  plane: PlaneChoice
  onChanged: () => void
  onAuthError: (err: unknown) => void
}) {
  const [editKey, setEditKey] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [addNetworkDraft, setAddNetwork] = useState<string | null>(null)
  const [rowError, setRowError] = useState('')

  const global = settings.thresholds
  const defaults = settings.network_defaults
  const configured = new Set(defaults.map((d) => d.network))

  // The all-planes option this shared control offers elsewhere has no
  // meaning here — a per-network row must name a network — so drop it.
  const addChoice: PlaneChoice =
    plane.kind === 'choice'
      ? {
          kind: 'choice',
          options: plane.options.filter((n) => n !== ''),
          initial: plane.options.find((n) => n !== '') ?? '',
        }
      : plane
  const addNetwork = addNetworkDraft ?? initialPlane(addChoice)

  const remove = async (d: NetworkThreshold) => {
    setRowError('')
    try {
      await apiDelete(networkThresholdURL(d.network))
      onChanged()
    } catch (err) {
      onAuthError(err)
      setRowError(err instanceof Error ? err.message : String(err))
    }
  }

  const addable = addNetwork !== '' && !configured.has(addNetwork)

  return (
    <section className="card settings-card config-card">
      <div className="card-head">
        <div>
          <span className="eyebrow">Per-network defaults</span>
          <h2>Network thresholds</h2>
        </div>
      </div>
      <p className="section-intro">
        One network's baseline, applied to every pair on that plane that has no override of its own. Empty fields keep
        inheriting the global values above, and a per-pair override still wins over anything set here.
      </p>
      {rowError && (
        <ul className="error threshold-errors">
          <li>{rowError}</li>
        </ul>
      )}
      {defaults.length === 0 ? (
        <div className="empty-state">
          <strong>No network defaults configured</strong>
          <span>Every network grades against the global thresholds.</span>
        </div>
      ) : (
        <div className="scroll-x">
          <table className="events">
            <thead>
              <tr>
                <th>Network</th>
                <th>Latency degraded</th>
                <th>Latency critical</th>
                <th>Loss degraded</th>
                <th>Loss critical</th>
                <th>Updated</th>
                {canWrite && (
                  <th className="actions-col">
                    <span className="sr-only">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {defaults.map((d) => (
                <Fragment key={d.network}>
                  <tr>
                    <td data-label="Network" className="mono">
                      {d.network}
                    </td>
                    <td data-label="Latency degraded">{valueCell(d.latency_warn_us, 'ms', global.latency_warn_us)}</td>
                    <td data-label="Latency critical">{valueCell(d.latency_crit_us, 'ms', global.latency_crit_us)}</td>
                    <td data-label="Loss degraded">{valueCell(d.loss_warn_pct, '%', global.loss_warn_pct)}</td>
                    <td data-label="Loss critical">{valueCell(d.loss_crit_pct, '%', global.loss_crit_pct)}</td>
                    <td data-label="Updated">
                      {fmtAgo(d.updated_at)}
                      {d.updated_by ? ` by ${d.updated_by}` : ''}
                    </td>
                    {canWrite && (
                      <td data-label="Actions" className="config-actions">
                        <button
                          type="button"
                          className="secondary-button"
                          aria-expanded={editKey === d.network}
                          onClick={() => setEditKey(editKey === d.network ? null : d.network)}
                        >
                          {editKey === d.network ? 'Close' : 'Edit'}
                        </button>
                        <ConfirmButton
                          label="Delete"
                          confirmLabel="Confirm? Network returns to global thresholds"
                          onConfirm={() => void remove(d)}
                        />
                      </td>
                    )}
                  </tr>
                  {editKey === d.network && (
                    <tr className="config-edit-row">
                      <td colSpan={canWrite ? 7 : 6}>
                        <div className="config-form">
                          <h3 className="eyebrow">Edit defaults · {d.network}</h3>
                          <ThresholdOverrideForm
                            url={networkThresholdURL(d.network)}
                            override={d}
                            inherited={global}
                            canWrite={canWrite}
                            emptyHint="the global thresholds"
                            onChanged={() => {
                              setEditKey(null)
                              onChanged()
                            }}
                            onAuthError={onAuthError}
                          />
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {canWrite && caps.networks?.length !== 0 && (
        <div className="config-form">
          {!adding ? (
            <button
              type="button"
              className="secondary-button"
              onClick={() => {
                setAdding(true)
                setAddNetwork(null)
              }}
            >
              Add network defaults
            </button>
          ) : (
            <>
              <h3 className="eyebrow">
                New network defaults
                <span className="hint"> — applies to every pair on that plane without its own override</span>
              </h3>
              <div className="config-form-grid">
                <PlaneField choice={addChoice} value={addNetwork} onChange={setAddNetwork} label="Network" />
              </div>
              {addNetwork !== '' && configured.has(addNetwork) && (
                <ul className="error threshold-errors">
                  <li>{addNetwork} already has defaults — edit them in the table above</li>
                </ul>
              )}
              {addable && (
                <ThresholdOverrideForm
                  url={networkThresholdURL(addNetwork)}
                  override={null}
                  inherited={global}
                  canWrite={canWrite}
                  emptyHint="the global thresholds"
                  onChanged={() => {
                    setAdding(false)
                    onChanged()
                  }}
                  onAuthError={onAuthError}
                />
              )}
              <div className="threshold-foot">
                <span className="hint" />
                <button type="button" className="secondary-button" onClick={() => setAdding(false)}>
                  Cancel
                </button>
              </div>
            </>
          )}
        </div>
      )}
    </section>
  )
}
