import { useRef, useState } from 'react'
import type { Caps } from '../caps'
import RoleWall from './RoleWall'
import SettingsPageError from './SettingsPageError'
import { apiGet, apiPost, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useErrorSummary } from '../formErrors'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import type {
  SyslogContent,
  SyslogFraming,
  SyslogLevel,
  SyslogOnFailure,
  SyslogSettings,
  SyslogSettingsPut,
  SyslogStatus,
  SyslogTestResult,
  SyslogTransport,
} from '../types'
import { usePolledResource } from '../usePolledResource'

// Every RFC 5424 facility the server accepts (syslogfwd.FacilityCode),
// locals first because they are what a collector is usually told to route.
const FACILITIES = [
  'local0',
  'local1',
  'local2',
  'local3',
  'local4',
  'local5',
  'local6',
  'local7',
  'auth',
  'authpriv',
  'audit',
  'user',
  'daemon',
  'syslog',
  'kern',
  'mail',
  'lpr',
  'news',
  'uucp',
  'cron',
  'ftp',
  'ntp',
  'alert',
  'clock',
] as const

interface Draft {
  enabled: boolean
  transport: SyslogTransport
  host: string
  port: string
  framing: SyslogFraming
  facility: string
  hostname: string
  content: SyslogContent
  minLevel: SyslogLevel
  onFailure: SyslogOnFailure
  failureTimeoutMin: string
  caPem: string
  clientCertPem: string
  clientKeyPem: string // always starts empty; empty = keep stored (while a certificate is set)
  serverName: string
}

type StringKeys<T> = { [K in keyof T]: T[K] extends string ? K : never }[keyof T]

function draftFrom(s: SyslogSettings): Draft {
  return {
    enabled: s.enabled,
    transport: s.transport,
    host: s.host,
    port: String(s.port),
    framing: s.framing,
    facility: s.facility,
    hostname: s.hostname,
    content: s.content,
    minLevel: s.min_level,
    onFailure: s.on_failure,
    // Exact, not rounded: a 90 s timeout must survive an unrelated save and
    // still differ from 119 s in the conflict snapshot.
    failureTimeoutMin: String(s.failure_timeout_ms / 60000),
    caPem: s.tls_ca_pem,
    clientCertPem: s.tls_client_cert_pem,
    clientKeyPem: '',
    serverName: s.tls_server_name,
  }
}

// Mirrors the server's Config.Problems for the cheap checks; server 400s
// render verbatim as a backstop. The certificate/key pairing is the
// server's (it holds the stored key), so only the shape is checked here.
function validate(d: Draft, stored: SyslogSettings, forSave: boolean): { errors: string[]; body: SyslogSettingsPut } {
  const errors: string[] = []
  const host = d.host.trim()
  const port = Number(d.port)
  const timeoutMin = Number(d.failureTimeoutMin)
  if ((d.enabled || !forSave) && host === '') errors.push('host is required')
  if (/[\s/]/.test(host)) errors.push('host must be a hostname or IP address')
  if (!Number.isInteger(port) || port < 1 || port > 65535) errors.push('port must be between 1 and 65535')
  if (d.transport === 'tls' && d.framing === 'non-transparent') errors.push('TLS is always octet-counted (RFC 5425)')
  if (d.hostname.length > 255 || /[^\x21-\x7e]/.test(d.hostname))
    errors.push('hostname must be printable ASCII without spaces, at most 255 characters')
  if (!Number.isFinite(timeoutMin) || timeoutMin < 1) errors.push('failure timeout must be at least 1 minute')
  if (d.clientCertPem.trim() === '' && d.clientKeyPem.trim() !== '')
    errors.push('a client key needs a client certificate')
  if (d.clientCertPem.trim() !== '' && d.clientKeyPem.trim() === '' && !stored.tls_client_key_stored)
    errors.push('a client certificate needs its private key')
  return {
    errors,
    body: {
      enabled: d.enabled,
      transport: d.transport,
      host,
      port,
      framing: d.transport === 'tls' ? 'octet-counted' : d.framing,
      facility: d.facility,
      hostname: d.hostname.trim(),
      content: d.content,
      min_level: d.minLevel,
      on_failure: d.onFailure,
      failure_timeout_ms: Math.round(timeoutMin * 60000),
      tls_ca_pem: d.caPem.trim() === '' ? '' : d.caPem,
      tls_client_cert_pem: d.clientCertPem.trim() === '' ? '' : d.clientCertPem,
      tls_client_key_pem: d.clientKeyPem,
      tls_server_name: d.serverName.trim(),
    },
  }
}

function describeStatus(s: SyslogStatus | undefined): { text: string; tone: 'ok' | 'warn' | 'muted' } {
  if (!s || s.state === 'disabled') return { text: 'Forwarding is off.', tone: 'muted' }
  const since = s.since ? ` since ${fmtAgo(s.since)}` : ''
  const tail = ` · ${s.buffered} buffered · ${s.dropped_total} dropped`
  switch (s.state) {
    case 'connected':
      return { text: `Connected${since}${tail}`, tone: 'ok' }
    case 'connecting':
      return { text: `Connecting${tail}`, tone: 'muted' }
    default:
      return { text: `Disconnected${since}${tail}${s.last_error ? ` — ${s.last_error}` : ''}`, tone: 'warn' }
  }
}

export default function SyslogSettingsPanel({
  caps,
  canWrite,
  onAuthError,
}: {
  caps: Caps
  canWrite: boolean
  onAuthError: (err: unknown) => void
}) {
  // Admin-only GET (collector address, trust anchors, forwarder status), so
  // viewers get a static explanation instead of a doomed fetch.
  const { data, error, reload } = usePolledResource<SyslogSettings>('/api/v1/settings/syslog', {
    enabled: canWrite,
    onAuthError,
    logLabel: 'log forwarding settings',
  })
  const [draft, setDraft] = useState<Draft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const {
    request: formSummaryRequest,
    describedby: formSummaryDescribedby,
    id: formSummaryId,
    ref: formSummaryRef,
  } = useErrorSummary(formErrors.length > 0)
  const [saving, setSaving] = useState(false)
  const [warnings, setWarnings] = useState<string[]>([])
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null)
  const testSeq = useRef(0)
  const feedback = useSettingsMutation()
  const loadedDraft = data ? draftFrom(data) : null
  const guardCurrent = draft ?? loadedDraft
  const guard = useConcurrentSettingsDraft({
    id: 'syslog',
    label: 'Log forwarding settings',
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
    return <RoleWall need="adminWrite" what="Log forwarding settings" caps={caps} />
  }
  if (error && !data) {
    return (
      <SettingsPageError
        title="Log forwarding settings unavailable"
        subject="log forwarding settings"
        error={error}
        onRetry={() => void reload()}
      />
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading log forwarding settings…
      </div>
    )
  }

  const current = guardCurrent ?? draftFrom(data)
  const status = describeStatus(data.status)

  const update = (patch: Partial<Draft>) => {
    // A test result describes the values it was run against — any edit
    // invalidates it, and bumping the sequence drops an in-flight test's
    // late response.
    testSeq.current++
    setTestResult(null)
    setDraft((d) => ({ ...(d ?? draftFrom(data)), ...patch }))
  }

  const save = async () => {
    const { errors, body } = validate(current, data, true)
    setFormErrors(errors)
    if (errors.length > 0) {
      feedback.error(`Log forwarding settings: ${errors.join('; ')}`)
      formSummaryRequest()
      return
    }
    setSaving(true)
    try {
      const currentServer = await guard.checkForConflict(async () =>
        draftFrom(await apiGet<SyslogSettings>('/api/v1/settings/syslog')),
      )
      if (!currentServer) return
      const res = await apiPut<SyslogSettings>('/api/v1/settings/syslog', body)
      setWarnings(res.warnings ?? [])
      await reload()
      // Clear the draft so the form resumes following server state (the
      // 30 s poll converges other admins' edits instead of shadowing them).
      setDraft(null)
      feedback.success(res.warnings?.length ? 'Log forwarding saved with warnings.' : 'Log forwarding saved.')
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setFormErrors([message])
      formSummaryRequest()
      feedback.error(`Log forwarding settings were not saved: ${message}`)
    } finally {
      setSaving(false)
    }
  }

  const test = async () => {
    const { errors, body } = validate(current, data, false)
    if (errors.length > 0) {
      setFormErrors(errors)
      formSummaryRequest()
      feedback.error(`Log forwarding test: ${errors.join('; ')}`)
      return
    }
    const seq = ++testSeq.current
    setTesting(true)
    setTestResult(null)
    try {
      const res = await apiPost<SyslogTestResult>('/api/v1/settings/syslog/test', body)
      if (testSeq.current === seq) {
        const peer = res.peer_subject
          ? ` · collector certificate ${res.peer_subject}${res.peer_not_after ? `, expires ${new Date(res.peer_not_after).toLocaleDateString()}` : ''}`
          : ''
        setTestResult({
          ok: true,
          text: `Sent a test record to ${res.addr}${res.tls_version ? ` over ${res.tls_version}` : ''}${peer}.`,
        })
        feedback.success('Log forwarding test succeeded.')
      }
    } catch (err) {
      onAuthError(err)
      if (testSeq.current === seq) {
        const message = err instanceof Error ? err.message : String(err)
        setTestResult({ ok: false, text: message })
        feedback.error(`Log forwarding test failed: ${message}`)
      }
    } finally {
      setTesting(false)
    }
  }

  const textField = (
    label: string,
    key: StringKeys<Draft>,
    placeholder: string,
    opts: { type?: string; hint?: string; inputMode?: 'numeric' } = {},
  ) => (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <input
          type={opts.type ?? 'text'}
          inputMode={opts.inputMode}
          value={current[key]}
          placeholder={placeholder}
          disabled={saving}
          autoComplete="off"
          spellCheck={false}
          aria-describedby={formSummaryDescribedby}
          onChange={(e) => update({ [key]: e.target.value })}
        />
        {opts.hint && <span className="hint">{opts.hint}</span>}
      </span>
    </label>
  )

  const selectField = <K extends 'transport' | 'framing' | 'facility' | 'content' | 'minLevel' | 'onFailure'>(
    label: string,
    key: K,
    options: readonly { value: Draft[K]; label: string }[],
    hint?: string,
  ) => (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <select
          value={current[key]}
          disabled={saving}
          aria-describedby={formSummaryDescribedby}
          onChange={(e) => {
            const patch = { [key]: e.target.value } as Partial<Draft>
            // TLS is octet-counted by definition (RFC 5425); a framing left
            // over from a TCP draft would otherwise fail validation behind
            // a selector that can no longer show it.
            if (key === 'transport' && e.target.value === 'tls') patch.framing = 'octet-counted'
            update(patch)
          }}
        >
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        {hint && <span className="hint">{hint}</span>}
      </span>
    </label>
  )

  const pemField = (label: string, key: 'caPem' | 'clientCertPem' | 'clientKeyPem', placeholder: string) => (
    <label className="threshold-field oidc-capem">
      <span className="label">{label}</span>
      <textarea
        value={current[key]}
        placeholder={placeholder}
        rows={4}
        spellCheck={false}
        disabled={saving}
        aria-describedby={formSummaryDescribedby}
        onChange={(e) => update({ [key]: e.target.value })}
      />
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
            <h2>Log forwarding</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Send every audit record — sign-ins, session ends, account and configuration changes, agent credential
          decisions, server start and stop — and optionally the operational log to a syslog collector as RFC 5424 over
          TLS. Records also stay in the container log. The catalog of records, the wire format, and receiver examples
          are in docs/audit-logging.md.
        </p>
        <p className={status.tone === 'warn' ? 'error' : 'hint'} role="status" aria-live="polite">
          <strong>Status:</strong> {status.text}
        </p>
        {/* A real form: the client key is a secret field, Submit = Save. */}
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
              aria-describedby={formSummaryDescribedby}
              onChange={(e) => update({ enabled: e.target.checked })}
            />
            <span className="oidc-enable-copy">
              <span className="oidc-enable-title">Forward logs to a syslog collector</span>
              <span className="hint">
                {current.enabled
                  ? 'Records are sent to the collector below as they happen.'
                  : 'Nothing is forwarded; the settings are kept.'}
              </span>
            </span>
            <span className={current.enabled ? 'oidc-enable-state is-on' : 'oidc-enable-state'}>
              {current.enabled ? 'Enabled' : 'Disabled'}
            </span>
          </label>
          <div className="config-form-grid">
            {selectField(
              'Transport',
              'transport',
              [
                { value: 'tls', label: 'TLS (RFC 5425, recommended)' },
                { value: 'tcp', label: 'TCP (RFC 6587, unencrypted)' },
                { value: 'udp', label: 'UDP (RFC 5426, unencrypted, lossy)' },
              ],
              current.transport === 'tls' ? undefined : 'records cross the network in clear text',
            )}
            {textField('Host', 'host', 'collector.example')}
            {textField('Port', 'port', current.transport === 'tls' ? '6514' : '514', { inputMode: 'numeric' })}
            {selectField(
              'Framing',
              'framing',
              current.transport === 'tls'
                ? [{ value: 'octet-counted', label: 'octet-counted (LEN SP MSG)' }]
                : [
                    { value: 'octet-counted', label: 'octet-counted (rsyslog, syslog-ng)' },
                    { value: 'non-transparent', label: 'non-transparent (LF; Splunk TCP input)' },
                  ],
            )}
          </div>
          <div className="config-form-grid">
            {selectField(
              'Facility',
              'facility',
              FACILITIES.map((f) => ({ value: f, label: f })),
            )}
            {textField('Hostname', 'hostname', 'polarbeam.example', {
              hint: 'the HOSTNAME field of every record; empty uses the container hostname',
            })}
            {selectField('Content', 'content', [
              { value: 'all', label: 'audit records and the operational log' },
              { value: 'audit', label: 'audit records only' },
            ])}
            {selectField(
              'Minimum level',
              'minLevel',
              [
                { value: 'debug', label: 'debug' },
                { value: 'info', label: 'info' },
                { value: 'warn', label: 'warn' },
                { value: 'error', label: 'error' },
              ],
              'for operational records; audit records are always forwarded',
            )}
          </div>
          <div className="config-form-grid">
            {selectField(
              'On failure',
              'onFailure',
              [
                { value: 'warn', label: 'warn — buffer, alert, keep running' },
                { value: 'halt', label: 'halt — stop the server after the timeout' },
              ],
              current.onFailure === 'halt'
                ? 'the server stops without a reachable collector and refuses to start while it is unreachable (ASD STIG V-222486)'
                : undefined,
            )}
            {textField('Failure timeout (minutes)', 'failureTimeoutMin', '5', {
              inputMode: 'numeric',
              hint: 'for halt: how long the collector may be unreachable before the server stops (minimum 1)',
            })}
            {textField('Server name', 'serverName', '', {
              hint: 'the name expected in the collector certificate, when it differs from the host',
            })}
          </div>
          {pemField(
            'Collector CA (PEM, optional)',
            'caPem',
            '-----BEGIN CERTIFICATE-----  (the collector certificate or its CA; leave empty to use system roots)',
          )}
          {pemField(
            'Client certificate (PEM, optional — mutual TLS)',
            'clientCertPem',
            '-----BEGIN CERTIFICATE-----  (an identity this server presents to the collector)',
          )}
          {pemField(
            data.tls_client_key_stored
              ? 'Client private key (PEM, stored — leave empty to keep)'
              : 'Client private key (PEM)',
            'clientKeyPem',
            data.tls_client_key_stored ? '(unchanged)' : '-----BEGIN PRIVATE KEY-----',
          )}
          {formErrors.length > 0 && (
            <ul className="error threshold-errors" id={formSummaryId} ref={formSummaryRef} tabIndex={-1}>
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
