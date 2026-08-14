import { Fragment, useEffect, useState } from 'react'
import { apiDelete, apiGet } from '../api'
import { fmtAgo } from '../format'
import type { PathThresholdOverride, SettingsResponse, SitesResponse } from '../types'
import ConfirmButton from './ConfirmButton'
import PathThresholdEditor, { pathThresholdURL } from './PathThresholdEditor'

// Management table for the per-site-pair threshold overrides carried on
// GET /settings. Rows expand to the shared PathThresholdEditor; the add
// flow is two site selects feeding the same editor with no override yet.
const pairKey = (a: string, b: string) => (a < b ? a + '\u0000' + b : b + '\u0000' + a)

function valueCell(us: number | null, unit: 'ms' | '%', globalValue: number) {
  const scale = unit === 'ms' ? 1000 : 1
  if (us == null) {
    return <span className="hint">inherits {globalValue / scale + ' ' + unit}</span>
  }
  return <>{us / scale + ' ' + unit}</>
}

export default function PathThresholdsPanel({
  settings,
  isAdmin,
  onChanged,
  onAuthError,
}: {
  settings: SettingsResponse
  isAdmin: boolean
  onChanged: () => void
  onAuthError: (err: unknown) => void
}) {
  const [siteNames, setSiteNames] = useState<string[]>([])
  const [editKey, setEditKey] = useState<string | null>(null)
  const [addA, setAddA] = useState('')
  const [addB, setAddB] = useState('')
  const [adding, setAdding] = useState(false)
  const [rowError, setRowError] = useState('')

  // Site names feed the add-override selects only; admins are the only
  // audience, and a one-shot fetch is enough — a site created mid-visit
  // appears after a tab switch like every other config panel.
  useEffect(() => {
    if (!isAdmin) return
    apiGet<SitesResponse>('/api/v1/sites')
      // Sorting the fresh array .map just returned (toSorted needs the
      // ES2023 lib — same note as WorldMap's ordered sites).
      // oxlint-disable-next-line unicorn/no-array-sort
      .then((res) => setSiteNames(res.sites.map((s) => s.name).sort()))
      .catch(onAuthError)
  }, [isAdmin, onAuthError])

  const overrides = settings.overrides
  const global = settings.thresholds
  const overridden = new Set(overrides.map((o) => pairKey(o.a, o.b)))

  const remove = async (o: PathThresholdOverride) => {
    setRowError('')
    try {
      await apiDelete(pathThresholdURL(o.a, o.b))
      onChanged()
    } catch (err) {
      onAuthError(err)
      setRowError(err instanceof Error ? err.message : String(err))
    }
  }

  const addReady = addA !== '' && addB !== '' && addA !== addB && !overridden.has(pairKey(addA, addB))
  const addDuplicate = addA !== '' && addB !== '' && addA !== addB && overridden.has(pairKey(addA, addB))

  const siteSelect = (value: string, set: (v: string) => void, label: string) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <select value={value} onChange={(e) => set(e.target.value)}>
          <option value="">choose a site…</option>
          {siteNames.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </span>
    </label>
  )

  return (
    <section className="card settings-card config-card">
      <div className="card-head">
        <div>
          <span className="eyebrow">Per-pair exceptions</span>
          <h2>Path threshold overrides</h2>
        </div>
      </div>
      <p className="section-intro">
        Links have different baselines — an override tunes the thresholds for one site pair (both directions) while
        empty fields keep inheriting the global values above.
      </p>
      {rowError && (
        <ul className="error threshold-errors">
          <li>{rowError}</li>
        </ul>
      )}
      {overrides.length === 0 ? (
        <div className="empty-state">
          <strong>No overrides configured</strong>
          <span>Every pair grades against the global thresholds.</span>
        </div>
      ) : (
        <div className="scroll-x">
          <table className="events">
            <thead>
              <tr>
                <th>Pair</th>
                <th>Latency degraded</th>
                <th>Latency critical</th>
                <th>Loss degraded</th>
                <th>Loss critical</th>
                <th>Updated</th>
                {isAdmin && (
                  <th className="actions-col">
                    <span className="sr-only">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {overrides.map((o) => {
                const key = pairKey(o.a, o.b)
                return (
                  <Fragment key={key}>
                    <tr>
                      <td data-label="Pair" className="mono">
                        {o.a} ↔ {o.b}
                      </td>
                      <td data-label="Latency degraded">
                        {valueCell(o.latency_warn_us, 'ms', global.latency_warn_us)}
                      </td>
                      <td data-label="Latency critical">
                        {valueCell(o.latency_crit_us, 'ms', global.latency_crit_us)}
                      </td>
                      <td data-label="Loss degraded">{valueCell(o.loss_warn_pct, '%', global.loss_warn_pct)}</td>
                      <td data-label="Loss critical">{valueCell(o.loss_crit_pct, '%', global.loss_crit_pct)}</td>
                      <td data-label="Updated">
                        {fmtAgo(o.updated_at)}
                        {o.updated_by ? ` by ${o.updated_by}` : ''}
                      </td>
                      {isAdmin && (
                        <td data-label="Actions" className="config-actions">
                          <button
                            type="button"
                            className="secondary-button"
                            aria-expanded={editKey === key}
                            onClick={() => setEditKey(editKey === key ? null : key)}
                          >
                            {editKey === key ? 'Close' : 'Edit'}
                          </button>
                          <ConfirmButton
                            label="Delete"
                            confirmLabel="Confirm? Pair returns to global thresholds"
                            onConfirm={() => void remove(o)}
                          />
                        </td>
                      )}
                    </tr>
                    {editKey === key && (
                      <tr className="config-edit-row">
                        <td colSpan={isAdmin ? 7 : 6}>
                          <div className="config-form">
                            <h3 className="eyebrow">
                              Edit override · {o.a} ↔ {o.b}
                            </h3>
                            <PathThresholdEditor
                              a={o.a}
                              b={o.b}
                              override={o}
                              global={global}
                              isAdmin={isAdmin}
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
                )
              })}
            </tbody>
          </table>
        </div>
      )}
      {isAdmin && (
        <div className="config-form">
          {!adding ? (
            <button
              type="button"
              className="secondary-button"
              onClick={() => {
                setAdding(true)
                setAddA('')
                setAddB('')
              }}
            >
              Add override
            </button>
          ) : (
            <>
              <h3 className="eyebrow">
                New override
                <span className="hint"> — one override covers both directions of the pair</span>
              </h3>
              <div className="config-form-grid">
                {siteSelect(addA, setAddA, 'Site A')}
                {siteSelect(addB, setAddB, 'Site B')}
              </div>
              {addA !== '' && addA === addB && (
                <ul className="error threshold-errors">
                  <li>choose two different sites</li>
                </ul>
              )}
              {addDuplicate && (
                <ul className="error threshold-errors">
                  <li>this pair already has an override — edit it in the table above</li>
                </ul>
              )}
              {addReady && (
                <PathThresholdEditor
                  a={addA}
                  b={addB}
                  override={null}
                  global={global}
                  isAdmin={isAdmin}
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
