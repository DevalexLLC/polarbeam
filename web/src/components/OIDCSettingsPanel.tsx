import { useRef, useState } from 'react'
import type { Caps } from '../caps'
import RoleWall from './RoleWall'
import SettingsPageError from './SettingsPageError'
import { apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useErrorSummary } from '../formErrors'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import type { OIDCDiscoveryInfo, OIDCRoleRule, OIDCSettings, OIDCSettingsPut, UnmatchedRole } from '../types'
import { usePolledResource } from '../usePolledResource'
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
  // The tenant policy. Both are sent explicitly on every save: the server
  // treats an omitted field as "keep stored", so a form that could edit them
  // but stayed silent would make an unrelated save look like a no-op while
  // quietly preserving a mapping the operator just changed.
  roleRules: OIDCRoleRule[]
  unmatchedRole: UnmatchedRole
  caPem: string
}

// textField drives the plain string inputs only; Draft also carries the
// boolean toggle and the structured tenant policy, which have their own
// editors.
type StringKeys<T> = { [K in keyof T]: T[K] extends string ? K : never }[keyof T]

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
    roleRules: s.role_rules.map((r) => ({ ...r, networks: [...r.networks] })),
    unmatchedRole: s.unmatched_role,
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
  // Mirrors validateOIDCSettings' per-rule checks so a half-filled rule is
  // caught before the round trip.
  d.roleRules.forEach((r, i) => {
    if (r.value.trim() === '') errors.push(`role rule ${i + 1}: claim value is required`)
    if (r.networks.length === 0) errors.push(`role rule ${i + 1}: at least one network is required`)
  })
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
      role_rules: d.roleRules.map((r) => ({ ...r, value: r.value.trim() })),
      unmatched_role: d.unmatchedRole,
      ca_pem: d.caPem.trim() === '' ? '' : d.caPem,
    },
  }
}

export default function OIDCSettingsPanel({
  caps,
  canWrite,
  networks,
  onAuthError,
}: {
  caps: Caps
  canWrite: boolean
  // Every plane on the install; a rule may grant any of them.
  networks: string[]
  onAuthError: (err: unknown) => void
}) {
  // Unlike the other config tabs this GET is admin-only (issuer and claim
  // mapping are IdP topology), so viewers get a static explanation instead
  // of a doomed fetch.
  const { data, error, reload } = usePolledResource<OIDCSettings>('/api/v1/settings/oidc', {
    enabled: canWrite,
    onAuthError,
    logLabel: 'authentication settings',
  })
  const [draft, setDraft] = useState<Draft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const formSummary = useErrorSummary(formErrors.length > 0)
  const [saving, setSaving] = useState(false)
  const [warnings, setWarnings] = useState<string[]>([])
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null)
  const testSeq = useRef(0)
  const feedback = useSettingsMutation()
  const loadedDraft = data ? draftFrom(data) : null
  const guardCurrent = draft ?? loadedDraft
  const guard = useConcurrentSettingsDraft({
    id: 'authentication',
    label: 'OpenID Connect settings',
    loaded: loadedDraft,
    current: guardCurrent,
    editing: draft !== null,
    discard: () => {
      setDraft(null)
      setFormErrors([])
      setWarnings([])
    },
    reload: setDraft,
  })

  if (!canWrite) {
    return <RoleWall need="adminWrite" what="Single sign-on settings" caps={caps} />
  }
  if (error && !data) {
    return (
      <SettingsPageError
        title="Authentication settings unavailable"
        subject="authentication settings"
        error={error}
        onRetry={() => void reload()}
      />
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

  const current = guardCurrent ?? draftFrom(data)

  const updateRule = (i: number, patch: Partial<OIDCRoleRule>) => {
    update({ roleRules: current.roleRules.map((r, j) => (j === i ? { ...r, ...patch } : r)) })
  }

  const update = (patch: Partial<Draft>) => {
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
    if (errors.length > 0) {
      feedback.error(`OpenID Connect settings: ${errors.join('; ')}`)
      formSummary.request()
      return
    }
    setSaving(true)
    try {
      const currentServer = await guard.checkForConflict(async () =>
        draftFrom(await apiGet<OIDCSettings>('/api/v1/settings/oidc')),
      )
      if (!currentServer) return
      const res = await apiPut<OIDCSettings>('/api/v1/settings/oidc', body)
      setWarnings(res.warnings ?? [])
      await reload()
      // Clear the draft so the form resumes following server state (the
      // 30 s poll converges other admins' edits instead of shadowing them).
      setDraft(null)
      feedback.success(
        res.warnings?.length ? 'OpenID Connect settings saved with warnings.' : 'OpenID Connect settings saved.',
      )
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      formSummary.request()
      feedback.error(`OpenID Connect settings were not saved: ${message}`)
    } finally {
      setSaving(false)
    }
  }

  const test = async () => {
    const { errors, body } = validate({ ...current, enabled: false }, data, false)
    if (current.issuer.trim() === '') {
      setTestResult({ ok: false, text: 'Enter an issuer URL first.' })
      feedback.error('OpenID Connect test: enter an issuer URL first.')
      return
    }
    if (errors.length > 0) {
      setFormErrors(errors)
      formSummary.request()
      feedback.error(`OpenID Connect test: ${errors.join('; ')}`)
      return
    }
    const seq = ++testSeq.current
    setTesting(true)
    setTestResult(null)
    try {
      const info = await apiPost<OIDCDiscoveryInfo>('/api/v1/settings/oidc/test', body)
      if (testSeq.current === seq) {
        setTestResult({ ok: true, text: `Discovery OK — token endpoint ${info.token_endpoint}` })
        feedback.success('OpenID Connect connection succeeded.')
      }
    } catch (err) {
      onAuthError(err)
      if (testSeq.current === seq) {
        const message = err instanceof Error ? err.message : String(err)
        setTestResult({ ok: false, text: message })
        feedback.error(`OpenID Connect connection failed: ${message}`)
      }
    } finally {
      setTesting(false)
    }
  }

  const textField = (
    label: string,
    key: StringKeys<Draft>,
    placeholder: string,
    opts: { type?: string; hint?: string } = {},
  ) => (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <input
          type={opts.type ?? 'text'}
          value={current[key]}
          placeholder={placeholder}
          disabled={saving}
          autoComplete="off"
          aria-describedby={formSummary.describedby}
          onChange={(e) => update({ [key]: e.target.value })}
        />
        {opts.hint && <span className="hint">{opts.hint}</span>}
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
            <h2>OpenID Connect</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Delegate dashboard sign-in to your identity provider. Users are provisioned on first login, keyed by the
          provider&apos;s stable subject. Local accounts — including the seeded admin — always keep working as
          break-glass access, whatever the state of the provider.
        </p>
        {/* A real form, not a div: the client secret is a password field,
            and outside a form Chrome logs a DOM warning and password
            managers cannot associate the field. Submit = Save, so Enter in
            a text field saves once the form is dirty (the disabled default
            button blocks implicit submission until then). */}
        <form
          className="config-form"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <label className="oidc-enable">
            <input
              type="checkbox"
              role="switch"
              aria-checked={current.enabled}
              checked={current.enabled}
              disabled={saving}
              aria-describedby={formSummary.describedby}
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
            <span className="label">Admin values (one per line)</span>
            <textarea
              value={current.adminValues}
              placeholder={'polarbeam-admins'}
              rows={3}
              spellCheck={false}
              disabled={saving}
              aria-describedby={formSummary.describedby}
              onChange={(e) => update({ adminValues: e.target.value })}
            />
            <span className="hint">
              role-claim values granting global admin, matched exactly (commas allowed); this list always wins
            </span>
          </label>
          <div
            className="threshold-field"
            role="group"
            aria-label="Network role rules"
            aria-describedby={formSummary.describedby}
          >
            <span className="label">Network role rules</span>
            <span className="hint">
              Ordered mappings from the same role claim to a network-scoped role. Every matching rule contributes: the
              strongest role wins and its networks are unioned, so one user in two tenants' admin groups administers
              both. Only the scoped roles are mappable — admin has its list above, and a rule granting global viewer
              would be the unmatched fallback in disguise.
            </span>
            {current.roleRules.length === 0 ? (
              <span className="hint">No rules — every non-admin falls through to the setting below.</span>
            ) : (
              <div className="oidc-role-rules">
                {current.roleRules.map((rule, i) => (
                  // Index-keyed on purpose: rules are an ordered list with no
                  // identity of their own, and two rules may legitimately
                  // share a value while being edited.
                  // oxlint-disable-next-line react/no-array-index-key
                  <div className="oidc-role-rule" key={i}>
                    <input
                      type="text"
                      value={rule.value}
                      placeholder="tenant-a-admins"
                      spellCheck={false}
                      disabled={saving}
                      aria-label={`Rule ${i + 1} claim value`}
                      onChange={(e) => updateRule(i, { value: e.target.value })}
                    />
                    <select
                      value={rule.role}
                      disabled={saving}
                      aria-label={`Rule ${i + 1} role`}
                      onChange={(e) => updateRule(i, { role: e.target.value as OIDCRoleRule['role'] })}
                    >
                      <option value="network_admin">network admin</option>
                      <option value="network_viewer">network viewer</option>
                    </select>
                    <div className="chips" role="group" aria-label={`Rule ${i + 1} networks`}>
                      {/* A rule can outlive a network: the name stays in the
                        stored rule after the network is deleted, and the
                        server then refuses the whole settings save until it
                        is removed. Listing only live networks would hide the
                        one entry the admin has to clear, so selected-but-gone
                        names are shown too — checked, so unchecking removes
                        them. */}
                      {[...networks, ...rule.networks.filter((n) => !networks.includes(n))].map((n) => (
                        <label key={n} className="chip">
                          <input
                            type="checkbox"
                            checked={rule.networks.includes(n)}
                            disabled={saving}
                            onChange={(e) =>
                              updateRule(i, {
                                networks: e.target.checked
                                  ? [...rule.networks, n]
                                  : rule.networks.filter((v) => v !== n),
                              })
                            }
                          />
                          {n}
                          {!networks.includes(n) && <span className="hint"> (deleted)</span>}
                        </label>
                      ))}
                    </div>
                    <button
                      type="button"
                      className="secondary-button"
                      disabled={saving}
                      onClick={() => update({ roleRules: current.roleRules.filter((_, j) => j !== i) })}
                    >
                      Remove
                    </button>
                  </div>
                ))}
              </div>
            )}
            <button
              type="button"
              className="secondary-button"
              disabled={saving}
              onClick={() =>
                update({
                  roleRules: [...current.roleRules, { value: '', role: 'network_admin', networks: [] }],
                })
              }
            >
              Add rule
            </button>
          </div>
          <label className="threshold-field">
            <span className="label">Unmatched users</span>
            <span className="threshold-input">
              <select
                value={current.unmatchedRole}
                disabled={saving}
                aria-describedby={formSummary.describedby}
                onChange={(e) => update({ unmatchedRole: e.target.value as UnmatchedRole })}
              >
                <option value="viewer">become global viewers</option>
                <option value="deny">are denied sign-in</option>
              </select>
            </span>
            <span className="hint">
              What happens to a user the provider authenticates who matches no admin value and no rule. A global viewer
              sees every network, so any install carrying more than one tenant must deny.
            </span>
          </label>
          <label className="threshold-field oidc-capem">
            <span className="label">Identity provider CA (PEM, optional)</span>
            <textarea
              value={current.caPem}
              placeholder="-----BEGIN CERTIFICATE-----  (only for private PKI; leave empty to use system roots)"
              rows={5}
              spellCheck={false}
              disabled={saving}
              aria-describedby={formSummary.describedby}
              onChange={(e) => update({ caPem: e.target.value })}
            />
          </label>
          {formErrors.length > 0 && (
            <ul className="error threshold-errors" id={formSummary.id} ref={formSummary.ref} tabIndex={-1}>
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
              <button type="button" className="secondary-button" onClick={test} disabled={testing || saving}>
                {testing ? 'Testing…' : 'Test connection'}
              </button>
              <button type="submit" className="primary" disabled={saving || !guard.dirty}>
                {saving ? 'Saving…' : 'Save'}
              </button>
            </span>
          </div>
        </form>
      </section>
    </>
  )
}
