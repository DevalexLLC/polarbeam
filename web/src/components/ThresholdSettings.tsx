import { useState } from 'react'
import { apiPut } from '../api'
import { fmtAgo } from '../format'
import type { SettingsResponse, ThresholdSettings } from '../types'

// The form speaks milliseconds and percent; the wire speaks microseconds.
const usToMs = (us: number) => String(us / 1000)
const MAX_LATENCY_CRIT_MS = 60_000

interface Draft {
  latencyWarnMs: string
  latencyCritMs: string
  lossWarnPct: string
  lossCritPct: string
}

function draftFrom(t: ThresholdSettings): Draft {
  return {
    latencyWarnMs: usToMs(t.latency_warn_us),
    latencyCritMs: usToMs(t.latency_crit_us),
    lossWarnPct: String(t.loss_warn_pct),
    lossCritPct: String(t.loss_crit_pct),
  }
}

// Mirrors the server's validateThresholds so nearly every mistake is caught
// before the round-trip; server 400s render verbatim as a backstop.
function validate(d: Draft): { errors: string[]; parsed: ThresholdSettings | null } {
  const errors: string[] = []
  const num = (label: string, s: string): number => {
    const n = Number(s)
    if (s.trim() === '' || !Number.isFinite(n)) {
      errors.push(`${label} must be a number`)
      return NaN
    }
    return n
  }
  const warnMs = num('latency degraded', d.latencyWarnMs)
  const critMs = num('latency critical', d.latencyCritMs)
  const lossWarn = num('loss degraded', d.lossWarnPct)
  const lossCrit = num('loss critical', d.lossCritPct)
  if (errors.length === 0) {
    if (warnMs <= 0) errors.push('latency degraded must be positive')
    if (critMs <= warnMs) errors.push('latency critical must be greater than degraded')
    if (critMs > MAX_LATENCY_CRIT_MS) errors.push(`latency critical must be at most ${MAX_LATENCY_CRIT_MS} ms`)
    if (lossWarn < 0) errors.push('loss degraded must not be negative')
    if (lossCrit <= lossWarn) errors.push('loss critical must be greater than degraded')
    if (lossCrit > 100) errors.push('loss critical must be at most 100%')
  }
  if (errors.length > 0) return { errors, parsed: null }
  return {
    errors: [],
    parsed: {
      latency_warn_us: Math.round(warnMs * 1000),
      latency_crit_us: Math.round(critMs * 1000),
      loss_warn_pct: lossWarn,
      loss_crit_pct: lossCrit,
    },
  }
}

export default function ThresholdSettingsPanel({
  settings,
  isAdmin,
  onSaved,
  onAuthError,
  variant = 'popover',
}: {
  settings: SettingsResponse | null
  isAdmin: boolean
  onSaved: (s: SettingsResponse) => void
  onAuthError: (err: unknown) => void
  variant?: 'popover' | 'page'
}) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [errors, setErrors] = useState<string[]>([])
  const [warnings, setWarnings] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  if (!settings) return null

  const toggle = () => {
    setOpen((o) => !o)
    setDraft(draftFrom(settings.thresholds))
    setErrors([])
    setWarnings([])
    setSavedFlash(false)
  }

  const save = async () => {
    const current = draft ?? draftFrom(settings.thresholds)
    const { errors: errs, parsed } = validate(current)
    setErrors(errs)
    if (!parsed) return
    setSaving(true)
    try {
      const res = await apiPut<SettingsResponse>('/api/v1/settings', parsed)
      // Advisory: pair overrides the new globals left inconsistent.
      setWarnings(res.warnings ?? [])
      onSaved(res)
      // Clear the draft so the form resumes following server state: the
      // Settings page polls, and a lingering draft would hide another
      // admin's later change and let a stray Save overwrite it.
      setDraft(null)
      setSavedFlash(true)
    } catch (err) {
      onAuthError(err)
      setErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

  const field = (label: string, unit: string, key: keyof Draft) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          inputMode="decimal"
          value={(draft ?? draftFrom(settings.thresholds))[key]}
          disabled={!isAdmin || saving}
          onChange={(e) => {
            setSavedFlash(false)
            setDraft((d) => ({ ...(d ?? draftFrom(settings.thresholds)), [key]: e.target.value }))
          }}
        />
        <span className="hint">{unit}</span>
      </span>
    </label>
  )

  const currentDraft = draft ?? draftFrom(settings.thresholds)
  const savedDraft = draftFrom(settings.thresholds)
  const dirty = (Object.keys(currentDraft) as (keyof Draft)[]).some((key) => currentDraft[key] !== savedDraft[key])

  return (
    <div className={'threshold-settings threshold-settings-' + variant}>
      {variant === 'popover' && (
        <button className="linklike" aria-expanded={open} onClick={toggle}>
          {open ? 'Close thresholds' : 'Thresholds'}
        </button>
      )}
      {(open || variant === 'page') && (
        <div className="threshold-panel">
          <div className="threshold-grid">
            {field('Latency degraded', 'ms', 'latencyWarnMs')}
            {field('Latency critical', 'ms', 'latencyCritMs')}
            {field('Loss degraded', '%', 'lossWarnPct')}
            {field('Loss critical', '%', 'lossCritPct')}
          </div>
          {errors.length > 0 && (
            <ul className="error threshold-errors">
              {errors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          {warnings.length > 0 && (
            <div className="inline-alert" role="status">
              <strong>Saved, with a caveat.</strong>
              <ul>
                {warnings.map((w) => (
                  <li key={w}>{w}</li>
                ))}
              </ul>
              <button className="linklike" onClick={() => setWarnings([])}>
                Dismiss
              </button>
            </div>
          )}
          <div className="threshold-foot">
            <span className="hint">
              Shared by every dashboard user
              {settings.updated_by ? ` · last set by ${settings.updated_by} ${fmtAgo(settings.updated_at)}` : ''}
            </span>
            {isAdmin ? (
              <span className="threshold-actions">
                {savedFlash && <span className="hint">saved</span>}
                <button className="primary" onClick={save} disabled={saving || !dirty}>
                  {saving ? 'Saving…' : 'Save'}
                </button>
              </span>
            ) : (
              <span className="hint">admin role required to edit</span>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
