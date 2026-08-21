import { useState } from 'react'
import { apiDelete, apiPut } from '../api'
import type { ThresholdOverrideFields, ThresholdSettings } from '../types'

// The shared editor for one row of the threshold stack. Every layer below
// the global row has the same shape — four nullable metrics where empty
// means "inherit the layer outside this one" — so the per-pair overrides
// and the per-network defaults use this same form and differ only in the
// URL they write to and what they are called.
//
// Saving with every field empty deletes the row: the server rejects
// all-null rows by design, so "clear everything" can only mean "stop
// overriding".
const usToMs = (us: number) => String(us / 1000)
const MAX_LATENCY_CRIT_MS = 60_000

interface Draft {
  latencyWarnMs: string
  latencyCritMs: string
  lossWarnPct: string
  lossCritPct: string
}

// The PUT body every layer shares: the four nullable metrics and nothing
// else. Identity is in the URL and the audit columns are the server's.
type OverrideBody = ThresholdOverrideFields

function draftFrom(o: ThresholdOverrideFields | null): Draft {
  return {
    latencyWarnMs: o?.latency_warn_us != null ? usToMs(o.latency_warn_us) : '',
    latencyCritMs: o?.latency_crit_us != null ? usToMs(o.latency_crit_us) : '',
    lossWarnPct: o?.loss_warn_pct != null ? String(o.loss_warn_pct) : '',
    lossCritPct: o?.loss_crit_pct != null ? String(o.loss_crit_pct) : '',
  }
}

// Mirrors the server's validateOverride: set values are checked on their
// own, then warn/crit consistency runs on the EFFECTIVE values (empty
// fields fall back to the layer outside this row), with the inherited side
// named.
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
    const inherit = ' (inherited)'
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

export default function ThresholdOverrideForm({
  url,
  override,
  inherited,
  canWrite,
  emptyHint,
  onCancel,
  onChanged,
  onAuthError,
}: {
  // The row's own endpoint. Both callers address a single row, so the verb
  // is always PUT and the delete is always this same URL.
  url: string
  override: ThresholdOverrideFields | null
  // What an empty field falls back to: the layers outside this one, already
  // folded. For a plane-qualified pair row that is the plane's defaults over
  // the global row, not the global row itself.
  inherited: ThresholdSettings
  canWrite: boolean
  // Names the layer an empty field falls through to, in the footer.
  emptyHint: string
  // Add flows use this to keep Cancel beside Save. Existing-row editors
  // omit it because their host already provides a Close action.
  onCancel?: () => void
  // Called after a successful write with the server's advisory warnings
  // (empty when there were none). Hosts close the editor only on a clean
  // save — closing on a warning would discard the very thing the server
  // went out of its way to report.
  onChanged: (warnings: string[]) => void
  onAuthError: (err: unknown) => void
}) {
  const [draft, setDraft] = useState<Draft | null>(null)
  const [errors, setErrors] = useState<string[]>([])
  const [warnings, setWarnings] = useState<string[]>([])
  const [saving, setSaving] = useState(false)

  const current = draft ?? draftFrom(override)

  const save = async () => {
    const allEmpty = Object.values(current).every((v) => v.trim() === '')
    if (allEmpty && !override) {
      setErrors(['set at least one value — empty fields inherit ' + emptyHint])
      return
    }
    setSaving(true)
    try {
      if (allEmpty) {
        // Every field cleared on an existing row = remove it.
        await apiDelete(url)
      } else {
        const { errors: errs, body } = validate(current, inherited)
        setErrors(errs)
        if (!body) {
          setSaving(false)
          return
        }
        // The write can succeed WITH caveats: a new network default can
        // leave this plane's pair rows inverted, and a pair override can
        // name a plane neither site carries yet. The server reports those
        // as advisory warnings rather than refusing, so dropping them would
        // tell the operator "saved" and silently swallow the "but…".
        const res = await apiPut<{ warnings?: string[] }>(url, body)
        if (res?.warnings?.length) {
          setWarnings(res.warnings)
          setDraft(null)
          onChanged(res.warnings)
          setSaving(false)
          return
        }
      }
      setErrors([])
      setWarnings([])
      // Clear the draft so the form resumes following server state (same
      // poll-reconciliation contract as the global thresholds form).
      setDraft(null)
      onChanged([])
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
          disabled={!canWrite || saving}
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
  // in-flow inside its host (a table edit row or an add form).
  return (
    <div className="threshold-settings threshold-settings-page">
      <div className="threshold-panel">
        <div className="threshold-grid">
          {field('Latency degraded', 'ms', 'latencyWarnMs', usToMs(inherited.latency_warn_us))}
          {field('Latency critical', 'ms', 'latencyCritMs', usToMs(inherited.latency_crit_us))}
          {field('Loss degraded', '%', 'lossWarnPct', String(inherited.loss_warn_pct))}
          {field('Loss critical', '%', 'lossCritPct', String(inherited.loss_crit_pct))}
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
            Saved, with warnings:
            <ul className="threshold-errors">
              {warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </div>
        )}
        <div className="threshold-foot">
          <span className="hint">
            Empty fields inherit {emptyHint}
            {override ? ' · clearing every field removes this row on save' : ''}
          </span>
          {canWrite ? (
            <span className="threshold-actions">
              {onCancel && (
                <button type="button" className="secondary-button" onClick={onCancel} disabled={saving}>
                  Cancel
                </button>
              )}
              <button className="primary" onClick={() => void save()} disabled={saving || !dirty}>
                {saving ? 'Saving…' : 'Save'}
              </button>
            </span>
          ) : (
            <span className="hint">write access required to edit</span>
          )}
        </div>
      </div>
    </div>
  )
}
