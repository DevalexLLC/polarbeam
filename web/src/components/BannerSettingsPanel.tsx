import { useEffect, useState } from 'react'
import type { Caps } from '../caps'
import RoleWall from './RoleWall'
import { apiGet, apiPut } from '../api'
import { fmtAgo } from '../format'
import type { BannerSettings, BannerSettingsPut, UIBanner } from '../types'

const POLL_MS = 30_000
const MAX_TEXT_CHARS = 300

interface Draft {
  enabled: boolean
  text: string
}

function draftFrom(s: BannerSettings): Draft {
  return { enabled: s.enabled, text: s.text }
}

// Mirrors the server's validateBannerSettings; server 400s render verbatim
// as a backstop.
function validate(d: Draft): { errors: string[]; body: BannerSettingsPut } {
  const text = d.text.trim()
  const errors: string[] = []
  if (/\p{Cc}/u.test(text)) errors.push('text must not contain control characters')
  if ([...text].length > MAX_TEXT_CHARS) errors.push(`text must be at most ${MAX_TEXT_CHARS} characters`)
  if (d.enabled && text === '') errors.push('text is required when the banner is enabled')
  return { errors, body: { enabled: d.enabled, text } }
}

export default function BannerSettingsPanel({
  caps,
  canWrite,
  onAuthError,
  onSaved,
}: {
  caps: Caps
  canWrite: boolean
  onAuthError: (err: unknown) => void
  onSaved: (b: UIBanner) => void
}) {
  const [data, setData] = useState<BannerSettings | null>(null)
  const [error, setError] = useState('')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  // Admin-only GET (updated_by usernames), so viewers get a static
  // explanation instead of a doomed fetch.
  useEffect(() => {
    if (!canWrite) return
    let cancelled = false
    const load = () => {
      apiGet<BannerSettings>('/api/v1/settings/ui-banner')
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
  }, [canWrite, onAuthError])

  if (!canWrite) {
    return <RoleWall need="adminWrite" what="Banner settings" caps={caps} />
  }
  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>Banner settings unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading banner settings…
      </div>
    )
  }

  const current = draft ?? draftFrom(data)
  const saved = draftFrom(data)
  const dirty = current.enabled !== saved.enabled || current.text !== saved.text

  const update = (patch: Partial<Draft>) => {
    setSavedFlash(false)
    setDraft((d) => ({ ...(d ?? draftFrom(data)), ...patch }))
  }

  const save = async () => {
    const { errors, body } = validate(current)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      const res = await apiPut<BannerSettings>('/api/v1/settings/ui-banner', body)
      setData(res)
      // Clear the draft so the form resumes following server state (the
      // 30 s poll converges other admins' edits instead of shadowing them).
      setDraft(null)
      setSavedFlash(true)
      // The bands update instantly for the editing admin; everyone else
      // converges through the app-level 30 s poll. Mirror the open
      // endpoint's redaction: a disabled banner carries no text.
      onSaved({ enabled: res.enabled, text: res.enabled ? res.text : '' })
    } catch (err) {
      onAuthError(err)
      setFormErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

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
            <span className="eyebrow">Marking</span>
            <h2>Screen banner</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Text shown centered in slim bands at the top and bottom of every screen, the sign-in page included — for
          deployments that must carry a marking such as &ldquo;PROPRIETARY&rdquo;. The text is kept when the banner is
          disabled, but only shown while it is enabled.
        </p>
        <form
          className="config-form"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          {/* The oidc-enable classes are the shared switch treatment, not
              OIDC-specific styling. */}
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
              <span className="oidc-enable-title">Show the banner</span>
              <span className="hint">
                {current.enabled
                  ? 'Every screen carries the text below, sign-in included.'
                  : 'No banner is shown anywhere.'}
              </span>
            </span>
            <span className={current.enabled ? 'oidc-enable-state is-on' : 'oidc-enable-state'}>
              {current.enabled ? 'Enabled' : 'Disabled'}
            </span>
          </label>
          <label className="threshold-field">
            <span className="eyebrow">Banner text</span>
            <span className="threshold-input">
              <input
                type="text"
                value={current.text}
                placeholder="PROPRIETARY"
                maxLength={MAX_TEXT_CHARS}
                disabled={saving}
                autoComplete="off"
                onChange={(e) => update({ text: e.target.value })}
              />
              <span className="hint">a single line, up to {MAX_TEXT_CHARS} characters</span>
            </span>
          </label>
          {formErrors.length > 0 && (
            <ul className="error threshold-errors">
              {formErrors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          <div className="threshold-foot">
            <span className="hint">
              Applies within seconds of saving — no restart
              {data.updated_by ? ` · last set by ${data.updated_by} ${fmtAgo(data.updated_at)}` : ''}
            </span>
            <span className="threshold-actions">
              {savedFlash && <span className="hint">saved</span>}
              <button type="submit" className="primary" disabled={saving || !dirty}>
                {saving ? 'Saving…' : 'Save'}
              </button>
            </span>
          </div>
        </form>
      </section>
    </>
  )
}
