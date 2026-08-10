import { useEffect, useRef, useState } from 'react'
import { apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import type { OIDCDiscoveryInfo, OIDCSettings, OIDCSettingsPut } from '../types'

const POLL_MS = 30_000
const CALLBACK_PATH = '/api/v1/auth/oidc/callback'

interface Draft {
  enabled: boolean
  issuer: string
  clientId: string
  clientSecret: string // always starts empty; empty = keep stored
  redirectUrl: string
  scopes: string
  usernameClaim: string
  roleClaim: string
  adminValues: string
  caPem: string
}

function draftFrom(s: OIDCSettings): Draft {
  return {
    enabled: s.enabled,
    issuer: s.issuer,
    clientId: s.client_id,
    clientSecret: '',
    redirectUrl: s.redirect_url,
    scopes: s.scopes.join(', '),
    usernameClaim: s.username_claim,
    roleClaim: s.role_claim,
    adminValues: s.admin_values.join('\n'),
    caPem: s.ca_pem,
  }
}

const splitList = (s: string) =>
  s
    .split(',')
    .map((v) => v.trim())
    .filter((v) => v !== '')

// Admin values split on newlines, not commas: claim values like an
// LDAP-style "CN=Admins,OU=Groups" legitimately contain commas, and the
// server matches them exactly. (Scope tokens cannot contain commas, so the
// comma list stays fine there.)
const splitLines = (s: string) =>
  s
    .split(/\r?\n/)
    .map((v) => v.trim())
    .filter((v) => v !== '')

// Mirrors the server's validateOIDCSettings for the cheap checks; server
// 400s render verbatim as a backstop. forSave additionally enforces the
// secret-scoping rule — the test action only runs discovery, which never
// transmits the secret, so it may probe a new issuer with the field blank.
function validate(d: Draft, stored: OIDCSettings, forSave: boolean): { errors: string[]; body: OIDCSettingsPut } {
  const secretStored = stored.client_secret_set
  const errors: string[] = []
  const checkUrl = (label: string, raw: string): URL | null => {
    if (raw === '') return null
    try {
      const u = new URL(raw)
      if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error()
      if (u.hash !== '') errors.push(`${label} must not contain a fragment`)
      return u
    } catch {
      errors.push(`${label} must be an absolute http(s) URL`)
      return null
    }
  }
  checkUrl('issuer', d.issuer.trim())
  const redirect = checkUrl('redirect URL', d.redirectUrl.trim())
  if (redirect) {
    if (redirect.search !== '') errors.push('redirect URL must not contain a query string')
    if (redirect.pathname !== CALLBACK_PATH) errors.push(`redirect URL path must be exactly ${CALLBACK_PATH}`)
  }
  const scopes = splitList(d.scopes)
  if (!scopes.includes('openid')) errors.push('scopes must include "openid"')
  if (d.usernameClaim.trim() === '') errors.push('username claim is required')
  if (d.roleClaim.trim() === '') errors.push('role claim is required')
  if (
    forSave &&
    d.clientSecret === '' &&
    secretStored &&
    (d.issuer.trim() !== stored.issuer || d.clientId.trim() !== stored.client_id)
  ) {
    errors.push(
      'changing the issuer or client ID requires entering a new client secret (the stored one belongs to the previous provider)',
    )
  }
  if (d.enabled) {
    if (d.issuer.trim() === '' || d.clientId.trim() === '' || d.redirectUrl.trim() === '') {
      errors.push('enabling requires issuer, client ID, and redirect URL')
    }
    if (d.clientSecret === '' && !secretStored) errors.push('enabling requires a client secret')
  }
  return {
    errors,
    body: {
      enabled: d.enabled,
      issuer: d.issuer.trim(),
      client_id: d.clientId.trim(),
      client_secret: d.clientSecret,
      redirect_url: d.redirectUrl.trim(),
      scopes,
      username_claim: d.usernameClaim.trim(),
      role_claim: d.roleClaim.trim(),
      admin_values: splitLines(d.adminValues),
      ca_pem: d.caPem.trim() === '' ? '' : d.caPem,
    },
  }
}

export default function OIDCSettingsPanel({
  isAdmin,
  onAuthError,
}: {
  isAdmin: boolean
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<OIDCSettings | null>(null)
  const [error, setError] = useState('')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)
  const [warnings, setWarnings] = useState<string[]>([])
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null)
  const testSeq = useRef(0)

  // Unlike the other config tabs this GET is admin-only (issuer and claim
  // mapping are IdP topology), so viewers get a static explanation instead
  // of a doomed fetch.
  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    const load = () => {
      apiGet<OIDCSettings>('/api/v1/settings/oidc')
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
  }, [isAdmin, onAuthError])

  if (!isAdmin) {
    return (
      <div className="state-panel">
        <h2>Admin role required</h2>
        <p>Single sign-on configuration is visible to administrators only.</p>
      </div>
    )
  }
  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Authentication settings unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading authentication settings…
      </div>
    )
  }

  const current = draft ?? draftFrom(data)
  const saved = draftFrom(data)
  const dirty = (Object.keys(current) as (keyof Draft)[]).some((k) => current[k] !== saved[k])

  const update = (patch: Partial<Draft>) => {
    setSavedFlash(false)
    // A discovery result describes the values it was run against — any
    // edit invalidates it, and bumping the sequence drops an in-flight
    // test's late response.
    testSeq.current++
    setTestResult(null)
    setDraft((d) => ({ ...(d ?? draftFrom(data)), ...patch }))
  }

  const save = async () => {
    const { errors, body } = validate(current, data, true)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      const res = await apiPut<OIDCSettings>('/api/v1/settings/oidc', body)
      setWarnings(res.warnings ?? [])
      setData(res)
      // Clear the draft so the form resumes following server state (the
      // 30 s poll converges other admins' edits instead of shadowing them).
      setDraft(null)
      setSavedFlash(true)
    } catch (err) {
      onAuthError(err)
      setFormErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

  const test = async () => {
    const { errors, body } = validate({ ...current, enabled: false }, data, false)
    if (current.issuer.trim() === '') {
      setTestResult({ ok: false, text: 'Enter an issuer URL first.' })
      return
    }
    if (errors.length > 0) {
      setFormErrors(errors)
      return
    }
    const seq = ++testSeq.current
    setTesting(true)
    setTestResult(null)
    try {
      const info = await apiPost<OIDCDiscoveryInfo>('/api/v1/settings/oidc/test', body)
      if (testSeq.current === seq) {
        setTestResult({ ok: true, text: `Discovery OK — token endpoint ${info.token_endpoint}` })
      }
    } catch (err) {
      onAuthError(err)
      if (testSeq.current === seq) {
        setTestResult({ ok: false, text: err instanceof Error ? err.message : String(err) })
      }
    } finally {
      setTesting(false)
    }
  }

  const textField = (
    label: string,
    key: keyof Omit<Draft, 'enabled'>,
    placeholder: string,
    opts: { type?: string; hint?: string } = {},
  ) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type={opts.type ?? 'text'}
          value={current[key]}
          placeholder={placeholder}
          disabled={saving}
          autoComplete="off"
          onChange={(e) => update({ [key]: e.target.value })}
        />
        {opts.hint && <span className="hint">{opts.hint}</span>}
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
      {warnings.length > 0 && (
        <div className="inline-alert" role="status">
          <strong>Saved, with a caveat.</strong>{' '}
          {warnings.map((w) => (
            <span key={w}>{w} </span>
          ))}
          <button type="button" className="linklike" onClick={() => setWarnings([])}>
            Dismiss
          </button>
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Single sign-on</span>
            <h2>OpenID Connect</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Delegate dashboard sign-in to your identity provider. Users are provisioned on first login, keyed by the
          provider&apos;s stable subject. Local accounts — including the seeded admin — always keep working as
          break-glass access, whatever the state of the provider.
        </p>
        <div className="config-form">
          <label className="oidc-enable">
            <input
              type="checkbox"
              role="switch"
              aria-checked={current.enabled}
              checked={current.enabled}
              disabled={saving}
              onChange={(e) => update({ enabled: e.target.checked })}
            />
            <span className="oidc-enable-copy">
              <span className="oidc-enable-title">Enable single sign-on</span>
              <span className="hint">
                {current.enabled
                  ? 'The login page offers sign-in through the provider below.'
                  : 'Only local accounts can sign in.'}
              </span>
            </span>
            <span className={current.enabled ? 'oidc-enable-state is-on' : 'oidc-enable-state'}>
              {current.enabled ? 'Enabled' : 'Disabled'}
            </span>
          </label>
          <div className="config-form-grid">
            {textField('Issuer URL', 'issuer', 'https://keycloak.example/realms/main')}
            {textField('Client ID', 'clientId', 'polarbeam')}
            {textField('Client secret', 'clientSecret', data.client_secret_set ? '(unchanged)' : 'required to enable', {
              type: 'password',
            })}
            {textField('Redirect URL', 'redirectUrl', `https://${location.host}${CALLBACK_PATH}`, {
              hint: 'must end in ' + CALLBACK_PATH,
            })}
          </div>
          <div className="config-form-grid">
            {textField('Scopes', 'scopes', 'openid, profile, email', { hint: 'comma-separated' })}
            {textField('Username claim', 'usernameClaim', 'preferred_username')}
            {textField('Role claim', 'roleClaim', 'groups')}
          </div>
          <label className="threshold-field oidc-admin-values">
            <span className="eyebrow">Admin values (one per line)</span>
            <textarea
              value={current.adminValues}
              placeholder={'polarbeam-admins'}
              rows={3}
              spellCheck={false}
              disabled={saving}
              onChange={(e) => update({ adminValues: e.target.value })}
            />
            <span className="hint">
              role-claim values granting admin, matched exactly (commas allowed); everyone else gets viewer
            </span>
          </label>
          <label className="threshold-field oidc-capem">
            <span className="eyebrow">Identity provider CA (PEM, optional)</span>
            <textarea
              value={current.caPem}
              placeholder="-----BEGIN CERTIFICATE-----  (only for private PKI; leave empty to use system roots)"
              rows={5}
              spellCheck={false}
              disabled={saving}
              onChange={(e) => update({ caPem: e.target.value })}
            />
          </label>
          {formErrors.length > 0 && (
            <ul className="error threshold-errors">
              {formErrors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          {testResult && (
            <p className={testResult.ok ? 'hint oidc-test-result' : 'error oidc-test-result'} role="status">
              {testResult.text}
            </p>
          )}
          <div className="threshold-foot">
            <span className="hint">
              Applies within seconds of saving — no restart
              {data.updated_by ? ` · last set by ${data.updated_by} ${fmtAgo(data.updated_at)}` : ''}
            </span>
            <span className="threshold-actions">
              {savedFlash && <span className="hint">saved</span>}
              <button type="button" className="secondary-button" onClick={test} disabled={testing || saving}>
                {testing ? 'Testing…' : 'Test connection'}
              </button>
              <button className="primary" onClick={save} disabled={saving || !dirty}>
                {saving ? 'Saving…' : 'Save'}
              </button>
            </span>
          </div>
        </div>
      </section>
    </>
  )
}
