import { useState } from 'react'
import { apiDelete, apiPut } from '../api'
import type { PathThresholdOverride, ThresholdSettings } from '../types'

// Editor for one pair's threshold override, hosted by the Settings →
// Thresholds overrides table (edit rows and the add flow). Fields speak
// ms/percent (wire is µs, like the global thresholds form); an EMPTY field
// means "inherit the global value", so the placeholders show what would
// apply. Saving with every field empty clears the override (DELETE) — the
// server rejects all-null rows by design.
const usToMs = (us: number) => String(us / 1000)
const MAX_LATENCY_CRIT_MS = 60_000

interface Draft {
  latencyWarnMs: string
  latencyCritMs: string
  lossWarnPct: string
  lossCritPct: string
}

// The nullable metric tuple of a PUT body (PathThresholdOverride minus the
// server-owned identity/audit fields).
type OverrideBody = Pick<
  PathThresholdOverride,
  'latency_warn_us' | 'latency_crit_us' | 'loss_warn_pct' | 'loss_crit_pct'
>

function draftFrom(o: PathThresholdOverride | null): Draft {
  return {
    latencyWarnMs: o?.latency_warn_us != null ? usToMs(o.latency_warn_us) : '',
    latencyCritMs: o?.latency_crit_us != null ? usToMs(o.latency_crit_us) : '',
    lossWarnPct: o?.loss_warn_pct != null ? String(o.loss_warn_pct) : '',
    lossCritPct: o?.loss_crit_pct != null ? String(o.loss_crit_pct) : '',
  }
}

// Mirrors the server's validateOverride: set values are checked on their
// own, then warn/crit consistency runs on the EFFECTIVE values (empty
// fields fall back to the global row), with the inherited side named.
function validate(d: Draft, global: ThresholdSettings): { errors: string[]; body: OverrideBody | null } {
  const errors: string[] = []
  const num = (label: string, s: string): number | null => {
    if (s.trim() === '') return null
    const n = Number(s)
    if (!Number.isFinite(n)) {
      errors.push(`${label} must be a number (or empty to inherit)`)
      return null
    }
    return n
  }
  const warnMs = num('latency degraded', d.latencyWarnMs)
  const critMs = num('latency critical', d.latencyCritMs)
  const lossWarn = num('loss degraded', d.lossWarnPct)
  const lossCrit = num('loss critical', d.lossCritPct)
  if (errors.length === 0) {
    if (warnMs != null && warnMs <= 0) errors.push('latency degraded must be positive')
    if (critMs != null && critMs <= 0) errors.push('latency critical must be positive')
    if (critMs != null && critMs > MAX_LATENCY_CRIT_MS)
      errors.push(`latency critical must be at most ${MAX_LATENCY_CRIT_MS} ms`)
    if (lossWarn != null && (lossWarn < 0 || lossWarn > 100)) errors.push('loss degraded must be between 0 and 100%')
    if (lossCrit != null && (lossCrit <= 0 || lossCrit > 100))
      errors.push('loss critical must be positive and at most 100%')
  }
  if (errors.length === 0) {
    const inherit = ' (inherited from global)'
    const effWarnMs = warnMs ?? global.latency_warn_us / 1000
    const effCritMs = critMs ?? global.latency_crit_us / 1000
    if (effCritMs <= effWarnMs) {
      errors.push(
        `latency critical (${effCritMs} ms${critMs == null ? inherit : ''}) must be greater than ` +
          `degraded (${effWarnMs} ms${warnMs == null ? inherit : ''})`,
      )
    }
    const effLossWarn = lossWarn ?? global.loss_warn_pct
    const effLossCrit = lossCrit ?? global.loss_crit_pct
    if (effLossCrit <= effLossWarn) {
      errors.push(
        `loss critical (${effLossCrit}%${lossCrit == null ? inherit : ''}) must be greater than ` +
          `degraded (${effLossWarn}%${lossWarn == null ? inherit : ''})`,
      )
    }
  }
  if (errors.length > 0) return { errors, body: null }
  return {
    errors: [],
    body: {
      latency_warn_us: warnMs != null ? Math.round(warnMs * 1000) : null,
      latency_crit_us: critMs != null ? Math.round(critMs * 1000) : null,
      loss_warn_pct: lossWarn,
      loss_crit_pct: lossCrit,
    },
  }
}

export function pathThresholdURL(a: string, b: string): string {
  return `/api/v1/settings/path-thresholds/${encodeURIComponent(a)}/${encodeURIComponent(b)}`
}

export default function PathThresholdEditor({
  a,
  b,
  override,
  global,
  isAdmin,
  onChanged,
  onAuthError,
}: {
  a: string
  b: string
  override: PathThresholdOverride | null
  global: ThresholdSettings
  isAdmin: boolean
  onChanged: () => void
  onAuthError: (err: unknown) => void
}) {
  const [draft, setDraft] = useState<Draft | null>(null)
  const [errors, setErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)

  const current = draft ?? draftFrom(override)

  const save = async () => {
    const allEmpty = Object.values(current).every((v) => v.trim() === '')
    if (allEmpty && !override) {
      setErrors(['set at least one value — empty fields inherit the global thresholds'])
      return
    }
    setSaving(true)
    try {
      if (allEmpty) {
        // Every field cleared on an existing override = remove it.
        await apiDelete(pathThresholdURL(a, b))
      } else {
        const { errors: errs, body } = validate(current, global)
        setErrors(errs)
        if (!body) {
          setSaving(false)
          return
        }
        await apiPut(pathThresholdURL(a, b), body)
      }
      setErrors([])
      // Clear the draft so the form resumes following server state (same
      // poll-reconciliation contract as the global thresholds form).
      setDraft(null)
      onChanged()
    } catch (err) {
      onAuthError(err)
      setErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

  const field = (label: string, unit: string, key: keyof Draft, inheritValue: string) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          inputMode="decimal"
          value={current[key]}
          placeholder={`inherits ${inheritValue}`}
          disabled={!isAdmin || saving}
          onChange={(e) => setDraft((d) => ({ ...(d ?? draftFrom(override)), [key]: e.target.value }))}
        />
        <span className="hint">{unit}</span>
      </span>
    </label>
  )

  const savedDraft = draftFrom(override)
  const dirty = (Object.keys(current) as (keyof Draft)[]).some((key) => current[key] !== savedDraft[key])

  // threshold-settings-page is load-bearing: the bare .threshold-panel is
  // an absolutely-positioned popover, and this editor always renders
  // in-flow inside its host (a table edit row or the add form).
  return (
    <div className="threshold-settings threshold-settings-page">
      <div className="threshold-panel">
        <div className="threshold-grid">
          {field('Latency degraded', 'ms', 'latencyWarnMs', usToMs(global.latency_warn_us))}
          {field('Latency critical', 'ms', 'latencyCritMs', usToMs(global.latency_crit_us))}
          {field('Loss degraded', '%', 'lossWarnPct', String(global.loss_warn_pct))}
          {field('Loss critical', '%', 'lossCritPct', String(global.loss_crit_pct))}
        </div>
        {errors.length > 0 && (
          <ul className="error threshold-errors">
            {errors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        )}
        <div className="threshold-foot">
          <span className="hint">
            Empty fields inherit the global thresholds
            {override ? ' · clearing every field removes the override on save' : ''}
          </span>
          {isAdmin ? (
            <button className="primary" onClick={() => void save()} disabled={saving || !dirty}>
              {saving ? 'Saving…' : 'Save'}
            </button>
          ) : (
            <span className="hint">admin role required to edit</span>
          )}
        </div>
      </div>
    </div>
  )
}
