import { Fragment, useEffect, useState } from 'react'
import { apiDelete, apiGet } from '../api'
import { fmtAgo } from '../format'
import { mergeLayers } from '../severity'
import type { Caps } from '../caps'
import { canWriteRow } from '../caps'
import type { PlaneChoice } from '../plane'
import { initialPlane } from '../plane'
import type { PathThresholdOverride, SettingsResponse, SitesResponse } from '../types'
import ConfirmButton from './ConfirmButton'
import PathThresholdEditor, { pathThresholdURL } from './PathThresholdEditor'
import PlaneField from './PlaneField'

// Management table for the per-site-pair threshold overrides carried on
// GET /settings. Rows expand to the shared PathThresholdEditor; the add
// flow is two site selects feeding the same editor with no override yet.
// Row identity is the unordered pair PLUS the plane: one pair can carry an
// all-planes row and a row per network at the same time, so keying on the
// pair alone would collide React keys and point every edit at whichever row
// happened to render first.
const pairKey = (a: string, b: string, network = '') =>
  (a < b ? a + '\u0000' + b : b + '\u0000' + a) + '\u0000' + network

// An empty field inherits the next layer out, which since the per-network
// defaults shipped is no longer always the global row: a pair override on a
// plane that states its own defaults inherits THOSE. Reporting the global
// value here would tell the operator the pair grades against a number it
// does not actually use.
function valueCell(us: number | null, unit: 'ms' | '%', inheritedValue: number) {
  const scale = unit === 'ms' ? 1000 : 1
  if (us == null) {
    return <span className="hint">inherits {inheritedValue / scale + ' ' + unit}</span>
  }
  return <>{us / scale + ' ' + unit}</>
}

export default function PathThresholdsPanel({
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
  const [siteNames, setSiteNames] = useState<string[]>([])
  const [editKey, setEditKey] = useState<string | null>(null)
  const [addA, setAddA] = useState('')
  const [addB, setAddB] = useState('')
  const [addNetworkDraft, setAddNetwork] = useState<string | null>(null)
  const addNetwork = addNetworkDraft ?? initialPlane(plane)
  const [adding, setAdding] = useState(false)
  const [rowError, setRowError] = useState('')

  // Site names feed the add-override selects only; whoever may write an
  // override is the audience, and a one-shot fetch is enough — a site
  // created mid-visit appears after a tab switch like every other config
  // panel. /api/v1/sites is any-session and already scope-filtered, so a
  // network admin sees exactly the sites its own planes reach.
  useEffect(() => {
    if (!canWrite) return
    apiGet<SitesResponse>('/api/v1/sites')
      // Sorting the fresh array .map just returned (toSorted needs the
      // ES2023 lib — same note as WorldMap's ordered sites).
      // oxlint-disable-next-line unicorn/no-array-sort
      .then((res) => setSiteNames(res.sites.map((s) => s.name).sort()))
      .catch(onAuthError)
  }, [canWrite, onAuthError])

  const overrides = settings.overrides
  const global = settings.thresholds
  // What a row's empty fields fall through to: its plane's defaults folded
  // over the global row, mirroring the resolver's pair -> network -> global
  // order with the pair layer itself left out.
  const networkDefaults = new Map(settings.network_defaults.map((d) => [d.network, d]))
  const inherited = (network: string) => (network === '' ? global : mergeLayers(global, networkDefaults.get(network)))
  const overridden = new Set(overrides.map((o) => pairKey(o.a, o.b, o.network)))

  const remove = async (o: PathThresholdOverride) => {
    setRowError('')
    try {
      await apiDelete(pathThresholdURL(o.a, o.b, o.network))
      onChanged()
    } catch (err) {
      onAuthError(err)
      setRowError(err instanceof Error ? err.message : String(err))
    }
  }

  const addPicked = addA !== '' && addB !== '' && addA !== addB
  // A scoped caller cannot write the all-planes row, so an unresolved plane
  // ('' with no global rights) is not a valid target for the add flow.
  const addPlaneOk = canWriteRow(caps, addNetwork) && plane.kind !== 'none'
  const addDuplicate = addPicked && overridden.has(pairKey(addA, addB, addNetwork))
  const addReady = addPicked && addPlaneOk && !addDuplicate

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
                {canWrite && (
                  <th className="actions-col">
                    <span className="sr-only">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {overrides.map((o) => {
                const key = pairKey(o.a, o.b, o.network)
                const base = inherited(o.network)
                return (
                  <Fragment key={key}>
                    <tr>
                      <td data-label="Pair" className="mono">
                        {o.a} ↔ {o.b}
                        {o.network !== '' && <span className="hint"> · {o.network}</span>}
                      </td>
                      <td data-label="Latency degraded">{valueCell(o.latency_warn_us, 'ms', base.latency_warn_us)}</td>
                      <td data-label="Latency critical">{valueCell(o.latency_crit_us, 'ms', base.latency_crit_us)}</td>
                      <td data-label="Loss degraded">{valueCell(o.loss_warn_pct, '%', base.loss_warn_pct)}</td>
                      <td data-label="Loss critical">{valueCell(o.loss_crit_pct, '%', base.loss_crit_pct)}</td>
                      <td data-label="Updated">
                        {fmtAgo(o.updated_at)}
                        {o.updated_by ? ` by ${o.updated_by}` : ''}
                      </td>
                      {canWrite && (
                        <td data-label="Actions" className="config-actions">
                          {canWriteRow(caps, o.network) ? (
                            <>
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
                                confirmLabel="Confirm? Pair returns to inherited thresholds"
                                onConfirm={() => void remove(o)}
                              />
                            </>
                          ) : (
                            // The all-planes row grades every tenant, so it
                            // belongs to a global admin.
                            <span className="hint">all networks</span>
                          )}
                        </td>
                      )}
                    </tr>
                    {editKey === key && (
                      <tr className="config-edit-row">
                        <td colSpan={canWrite ? 7 : 6}>
                          <div className="config-form">
                            <h3 className="eyebrow">
                              Edit override · {o.a} ↔ {o.b}
                              {o.network !== '' ? ` · network ${o.network}` : ''}
                            </h3>
                            <PathThresholdEditor
                              a={o.a}
                              b={o.b}
                              network={o.network}
                              override={o}
                              global={base}
                              canWrite={canWriteRow(caps, o.network)}
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
      {canWrite && (
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
                {/* Which plane the override grades. The all-planes row
                  applies to every tenant, so the server reserves it to a
                  global admin and offers it here only to one. */}
                <PlaneField
                  choice={plane}
                  value={addNetwork}
                  onChange={setAddNetwork}
                  label="Applies to"
                  hint="all networks grades every plane at this pair"
                />
              </div>
              {addA !== '' && addA === addB && (
                <ul className="error threshold-errors">
                  <li>choose two different sites</li>
                </ul>
              )}
              {addDuplicate && (
                <ul className="error threshold-errors">
                  <li>
                    this pair already has an override
                    {addNetwork === '' ? ' for all networks' : ` on ${addNetwork}`} — edit it in the table above
                  </li>
                </ul>
              )}
              {addReady && (
                <PathThresholdEditor
                  a={addA}
                  b={addB}
                  network={addNetwork}
                  override={null}
                  global={inherited(addNetwork)}
                  canWrite={canWrite}
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
