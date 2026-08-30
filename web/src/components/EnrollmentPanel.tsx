import { useRef, useState } from 'react'
import type { Caps } from '../caps'
import type { PlaneChoice } from '../plane'
import { initialPlane, networkField, planeReady } from '../plane'
import { inheritRouteNetwork } from '../routeState'
import { useSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { useErrorSummary } from '../formErrors'
import PlaneField from './PlaneField'
import RoleWall from './RoleWall'
import { apiDelete, apiGet, apiPost } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { JoinToken, SitesConfigResponse, TokenCreateResponse, TokensResponse } from '../types'
import { usePolledResource } from '../usePolledResource'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import SettingsPageError from './SettingsPageError'

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

  // Like the OIDC panel, this GET is admin-only (join tokens are enrollment
  // credentials), so viewers get a static explanation instead of a doomed
  // fetch. Sites feed the site picker only: a sites failure keeps the last
  // list and stays out of the panel error — tokens are the panel's subject.
  const siteNamesRef = useRef<string[]>([])
  const {
    data: snapshot,
    error,
    reload,
  } = usePolledResource(
    () => {
      const sitesRequest = apiGet<SitesConfigResponse>('/api/v1/config/sites')
        .then((res) => res.sites.map((s) => s.name))
        .catch((err) => {
          onAuthError(err)
          return siteNamesRef.current
        })
      return Promise.all([apiGet<TokensResponse>('/api/v1/config/tokens'), sitesRequest]).then(
        ([tokens, siteNames]) => ({ tokens, siteNames }),
      )
    },
    { enabled: canWrite, onAuthError, logLabel: 'enrollment settings' },
  )
  const data = snapshot?.tokens ?? null
  const siteNames = snapshot?.siteNames ?? []
  siteNamesRef.current = siteNames

  const [actionError, setActionError] = useState('')
  // Token deletion reuses actionError's render slot but must not describe
  // the issue-token form's fields.
  const [errorScope, setErrorScope] = useState<'create' | 'row'>('create')
  const summary = useErrorSummary(Boolean(actionError) && errorScope === 'create')
  const [site, setSite] = useState('')
  const [networkDraft, setNetwork] = useState<string | null>(null)
  const network = networkDraft ?? initialPlane(plane)
  const [ttlMS, setTtlMS] = useState(86_400_000)
  const [creating, setCreating] = useState(false)
  // The freshly minted token lives in its own state so the 30 s poll can
  // never clear it — it is shown exactly once and cannot be recovered.
  const [minted, setMinted] = useState<TokenCreateResponse | null>(null)
  const [copied, setCopied] = useState(false)
  const [actionRow, setActionRow] = useState<string | null>(null)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const feedback = useSettingsMutation()
  useSettingsDraft(
    'enrollment-token-form',
    minted ? `One-time enrollment token for ${minted.site}` : 'New enrollment token',
    site !== '' || networkDraft !== null || ttlMS !== 86_400_000 || minted !== null,
    () => {
      setSite('')
      setNetwork(null)
      setTtlMS(86_400_000)
      setMinted(null)
      setActionError('')
    },
    minted
      ? 'This token is shown only once and cannot be recovered. Discarding it requires issuing a replacement token.'
      : undefined,
  )

  if (!canWrite) {
    return <RoleWall need="networkWrite" what="Enrollment tokens" caps={caps} />
  }
  if (error && !data) {
    return (
      <SettingsPageError
        title="Enrollment tokens unavailable"
        subject="enrollment tokens"
        error={error}
        onRetry={() => void reload()}
      />
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
      feedback.success('Enrollment token issued.')
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setErrorScope('create')
      setActionError(message)
      summary.request()
      feedback.error(`Enrollment token was not issued: ${message}`)
    } finally {
      setCreating(false)
    }
  }

  const remove = async (t: JoinToken) => {
    setActionError('')
    try {
      await apiDelete('/api/v1/config/tokens/' + encodeURIComponent(t.id))
      feedback.success(`Enrollment token for ${t.site} deleted.`)
      await reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setErrorScope('row')
      setActionError(message)
      feedback.error(`Enrollment token was not deleted: ${message}`)
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

  const columns: DataTableColumn<JoinToken>[] = [
    { key: 'site', label: 'Site', priority: 'identity', className: 'mono', render: (t) => t.site },
    ...(showNetworkColumn
      ? [
          {
            key: 'network',
            label: 'Network',
            priority: 'primary',
            className: 'mono',
            render: (t) => t.network,
          } satisfies DataTableColumn<JoinToken>,
        ]
      : []),
    { key: 'created-by', label: 'Created by', priority: 'secondary', render: (t) => t.created_by || '—' },
    { key: 'created', label: 'Created', priority: 'secondary', render: (t) => fmtAgo(t.created_at) },
    { key: 'expires', label: 'Expires', priority: 'primary', render: (t) => fmtTime(t.expires_at) },
    { key: 'status', label: 'Status', priority: 'status', render: (t) => tokenStatus(t).label },
  ]

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
                  Create a site on the{' '}
                  <a href={inheritRouteNetwork('#/settings?section=infrastructure&subsection=sites')}>Sites tab</a>{' '}
                  first — tokens are always issued for an existing site.
                </span>
              </div>
            ) : (
              <div className="config-form-grid">
                <label className="threshold-field">
                  <span className="eyebrow">Site</span>
                  <span className="threshold-input">
                    <select
                      value={site}
                      disabled={creating}
                      aria-describedby={summary.describedby}
                      onChange={(e) => setSite(e.target.value)}
                    >
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
                    <select
                      value={ttlMS}
                      disabled={creating}
                      aria-describedby={summary.describedby}
                      onChange={(e) => setTtlMS(Number(e.target.value))}
                    >
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
              <ul className="error threshold-errors" id={summary.id} ref={summary.ref} tabIndex={-1}>
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
        <DataTable
          label="Join tokens"
          rows={data.tokens}
          rowKey={(t) => t.id}
          columns={columns}
          emptyTitle="No join tokens"
          emptyDescription="Issue one above to enroll a new agent."
          disclosure={{
            expandedKey: expandedRow,
            onExpandedKeyChange: setExpandedRow,
            label: (_token, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
            desktop: false,
          }}
          actions={
            canWrite
              ? {
                  openKey: actionRow,
                  onOpenKeyChange: setActionRow,
                  label: (t) => `Actions for token ${t.id}`,
                  render: (t) => {
                    const status = tokenStatus(t)
                    return (
                      <ConfirmButton
                        label="Delete"
                        resource={`Enrollment token ${t.id}`}
                        consequence={
                          status.kind === 'active'
                            ? 'This revokes the token immediately, so it can no longer enroll an agent.'
                            : 'This permanently removes the expired token record.'
                        }
                        disabled={t.used_at !== null}
                        title={t.used_at !== null ? 'Used tokens are enrollment audit records' : undefined}
                        onConfirm={() => remove(t)}
                      />
                    )
                  },
                }
              : undefined
          }
        />
      </section>
    </>
  )
}
