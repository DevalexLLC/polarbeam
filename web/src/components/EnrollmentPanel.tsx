import { useEffect, useState } from 'react'
import type { Caps } from '../caps'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { inheritRouteNetwork } from '../routeState'
import PlaneField from './PlaneField'
import RoleWall from './RoleWall'
import { apiDelete, apiGet, apiPost } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { JoinToken, SitesConfigResponse, TokenCreateResponse, TokensResponse } from '../types'
import ConfirmButton from './ConfirmButton'

const POLL_MS = 30_000

const TTL_OPTIONS: Array<{ label: string; ms: number }> = [
  { label: '1 hour', ms: 3_600_000 },
  { label: '6 hours', ms: 21_600_000 },
  { label: '24 hours', ms: 86_400_000 },
  { label: '3 days', ms: 259_200_000 },
  { label: '7 days', ms: 604_800_000 },
]

// Status is derived client-side (the API sends the raw fields): used wins
// over expired — a consumed token is history either way.
function tokenStatus(t: JoinToken): { label: string; kind: 'used' | 'expired' | 'active' } {
  if (t.used_at !== null) {
    return { label: t.used_by_hostname ? `Used by ${t.used_by_hostname}` : 'Used', kind: 'used' }
  }
  if (new Date(t.expires_at).getTime() < Date.now()) return { label: 'Expired', kind: 'expired' }
  return { label: 'Active', kind: 'active' }
}

export default function EnrollmentPanel({
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
  useTimezone() // re-render fmtTime renders on UTC/local toggle
  const [data, setData] = useState<TokensResponse | null>(null)
  const [siteNames, setSiteNames] = useState<string[]>([])
  const [error, setError] = useState('')
  const [actionError, setActionError] = useState('')
  const [site, setSite] = useState('')
  const [networkDraft, setNetwork] = useState<string | null>(null)
  const network = networkDraft ?? initialPlane(plane)
  const [ttlMS, setTtlMS] = useState(86_400_000)
  const [creating, setCreating] = useState(false)
  // The freshly minted token lives in its own state so the 30 s poll can
  // never clear it — it is shown exactly once and cannot be recovered.
  const [minted, setMinted] = useState<TokenCreateResponse | null>(null)
  const [copied, setCopied] = useState(false)

  // Like the OIDC panel, this GET is admin-only (join tokens are enrollment
  // credentials), so viewers get a static explanation instead of a doomed
  // fetch.
  useEffect(() => {
    if (!canWrite) return
    let cancelled = false
    const load = () => {
      apiGet<TokensResponse>('/api/v1/config/tokens')
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
      apiGet<SitesConfigResponse>('/api/v1/config/sites')
        .then((res) => {
          if (!cancelled) setSiteNames(res.sites.map((s) => s.name))
        })
        .catch((err) => {
          if (!cancelled) onAuthError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [canWrite, onAuthError])

  if (!canWrite) {
    return <RoleWall need="networkWrite" what="Enrollment tokens" caps={caps} />
  }
  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Enrollment tokens unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading enrollment tokens…
      </div>
    )
  }

  const reload = () => apiGet<TokensResponse>('/api/v1/config/tokens').then(setData).catch(onAuthError)

  const create = async () => {
    if (!site) return
    setCreating(true)
    setActionError('')
    setCopied(false)
    try {
      // networkField decides whether to name the plane. A scoped caller
      // always does — omitting it would resolve to 'default', a plane the
      // tenant cannot see, and 404.
      const res = await apiPost<TokenCreateResponse>('/api/v1/config/tokens', {
        site,
        ttl_ms: ttlMS,
        ...networkField(network),
      })
      setMinted(res)
      await reload()
    } catch (err) {
      onAuthError(err)
      setActionError(err instanceof Error ? err.message : String(err))
    } finally {
      setCreating(false)
    }
  }

  const remove = async (t: JoinToken) => {
    setActionError('')
    try {
      await apiDelete('/api/v1/config/tokens/' + encodeURIComponent(t.id))
      await reload()
    } catch (err) {
      onAuthError(err)
      setActionError(err instanceof Error ? err.message : String(err))
    }
  }

  const copyMinted = () => {
    if (!minted) return
    navigator.clipboard.writeText(minted.token).then(
      () => setCopied(true),
      () => setCopied(false),
    )
  }

  // The column stays visible whenever the plane is not implied — a real
  // choice, or a tenant pinned to one plane that is not 'default' — and
  // also while any listed token names another plane.
  const showNetworkColumn = plane.kind !== 'implicit' || data.tokens.some((t) => t.network !== 'default')

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
            <span className="eyebrow">Agent enrollment</span>
            <h2>Join tokens</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Single-use tokens that enroll one agent into a site. Deleting an unused token revokes it immediately; used
          tokens are kept as the enrollment audit record.
        </p>
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">Issue token</h3>
            {siteNames.length === 0 ? (
              <div className="empty-state">
                <strong>No sites yet</strong>
                <span>
                  Create a site on the <a href={inheritRouteNetwork('#/settings?section=sites')}>Sites tab</a> first —
                  tokens are always issued for an existing site.
                </span>
              </div>
            ) : (
              <div className="config-form-grid">
                <label className="threshold-field">
                  <span className="eyebrow">Site</span>
                  <span className="threshold-input">
                    <select value={site} disabled={creating} onChange={(e) => setSite(e.target.value)}>
                      <option value="">choose a site…</option>
                      {siteNames.map((n) => (
                        <option key={n} value={n}>
                          {n}
                        </option>
                      ))}
                    </select>
                  </span>
                </label>
                <PlaneField choice={plane} value={network} onChange={setNetwork} disabled={creating} />
                <label className="threshold-field">
                  <span className="eyebrow">Valid for</span>
                  <span className="threshold-input">
                    <select value={ttlMS} disabled={creating} onChange={(e) => setTtlMS(Number(e.target.value))}>
                      {TTL_OPTIONS.map((o) => (
                        <option key={o.ms} value={o.ms}>
                          {o.label}
                        </option>
                      ))}
                    </select>
                  </span>
                </label>
              </div>
            )}
            {actionError && (
              <ul className="error threshold-errors">
                <li>{actionError}</li>
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">
                Use it with <code>polarbeam-agent enroll</code> — the install guide has the full command including the
                CA fingerprint.
                {plane.kind !== 'implicit' &&
                  ' The agent joins the token’s network permanently; move a box by re-enrolling with a token for the other network.'}
              </span>
              <span className="threshold-actions">
                {/* Issuance stays blocked while a token is displayed: minting
                    another would replace the only copy of the cleartext. */}
                <button
                  className="primary"
                  onClick={create}
                  disabled={creating || !site || minted !== null || !planeReady(plane)}
                  title={minted !== null ? 'Copy and dismiss the displayed token first' : undefined}
                >
                  {creating ? 'Issuing…' : 'Issue token'}
                </button>
              </span>
            </div>
          </div>
        )}
        {minted && (
          <div className="inline-alert" role="status">
            <div>
              <strong>
                Token for {minted.network !== 'default' ? `${minted.site} on ${minted.network}` : minted.site}, valid
                until {fmtTime(minted.expires_at)}.
              </strong>{' '}
              Copy it now — it is shown only once and cannot be recovered.
              <div className="mono token-reveal">{minted.token}</div>
            </div>
            <span className="threshold-actions">
              <button type="button" className="secondary-button" onClick={copyMinted}>
                {copied ? 'Copied' : 'Copy'}
              </button>
              <button type="button" className="linklike" onClick={() => setMinted(null)}>
                Dismiss
              </button>
            </span>
          </div>
        )}
        {data.tokens.length === 0 ? (
          <div className="empty-state">
            <strong>No join tokens</strong>
            <span>Issue one above to enroll a new agent.</span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Site</th>
                  {showNetworkColumn && <th>Network</th>}
                  <th>Created by</th>
                  <th>Created</th>
                  <th>Expires</th>
                  <th>Status</th>
                  {canWrite && (
                    <th className="actions-col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {data.tokens.map((t) => {
                  const status = tokenStatus(t)
                  return (
                    <tr key={t.id}>
                      <td data-label="Site" className="mono">
                        {t.site}
                      </td>
                      {showNetworkColumn && (
                        <td data-label="Network" className="mono">
                          {t.network}
                        </td>
                      )}
                      <td data-label="Created by">{t.created_by || '—'}</td>
                      <td data-label="Created">{fmtAgo(t.created_at)}</td>
                      <td data-label="Expires">{fmtTime(t.expires_at)}</td>
                      <td data-label="Status">{status.label}</td>
                      {canWrite && (
                        <td data-label="Actions" className="config-actions">
                          <ConfirmButton
                            label="Delete"
                            confirmLabel={status.kind === 'active' ? 'Confirm revoke?' : 'Confirm delete?'}
                            disabled={t.used_at !== null}
                            title={t.used_at !== null ? 'Used tokens are enrollment audit records' : undefined}
                            onConfirm={() => remove(t)}
                          />
                        </td>
                      )}
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  )
}
