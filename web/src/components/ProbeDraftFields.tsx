import type { ProbeTypesResponse } from '../types'
import { paramSpecsFor, type ProbeDraft } from '../probeDraft'

// The cadence and parameter grids shared verbatim by the create form and
// the inline editor; the error-summary wiring stays with each caller, so
// this component only spreads the summary's describedby onto its inputs.
export default function ProbeDraftFields({
  draft,
  onChange,
  describedby,
  busy,
  registry,
}: {
  draft: ProbeDraft
  onChange: (fn: (d: ProbeDraft) => ProbeDraft) => void
  describedby: string | undefined
  busy: boolean
  registry: ProbeTypesResponse
}) {
  const numField = (
    label: string,
    unit: string,
    key: 'intervalS' | 'timeoutS' | 'trainCount' | 'trainSpacingMs',
    placeholder = '',
  ) => (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          inputMode="decimal"
          value={draft[key]}
          placeholder={placeholder}
          disabled={busy}
          aria-describedby={describedby}
          onChange={(e) => {
            onChange((prev) => ({ ...prev, [key]: e.target.value }))
          }}
        />
        <span className="hint">{unit}</span>
      </span>
    </label>
  )

  const paramFields = () => {
    const specs = paramSpecsFor(registry, draft.type, draft.mode)
    if (specs.length === 0) return null
    const setParam = (key: string, value: string) =>
      onChange((prev) => ({ ...prev, params: { ...prev.params, [key]: value } }))
    return (
      <div className="config-form-grid">
        {specs.map((spec) => {
          const required = draft.mode === 'mesh' ? spec.required_mesh : spec.required_direct
          const label = spec.key + (required ? ' (required)' : '')
          if (spec.kind === 'bool') {
            return (
              <label key={spec.key} className="config-param-check">
                <input
                  type="checkbox"
                  checked={draft.params[spec.key] === 'true'}
                  disabled={busy}
                  aria-describedby={describedby}
                  onChange={(e) => setParam(spec.key, e.target.checked ? 'true' : '')}
                />
                <span>{spec.key}</span>
                <span className="hint">{spec.hint}</span>
              </label>
            )
          }
          if (spec.kind === 'enum') {
            return (
              <label key={spec.key} className="threshold-field">
                <span className="label">{label}</span>
                <span className="threshold-input">
                  <select
                    value={draft.params[spec.key] ?? ''}
                    disabled={busy}
                    aria-describedby={describedby}
                    onChange={(e) => setParam(spec.key, e.target.value)}
                  >
                    <option value="">default</option>
                    {(spec.enum ?? []).map((v) => (
                      <option key={v} value={v}>
                        {v}
                      </option>
                    ))}
                  </select>
                  <span className="hint">{spec.hint}</span>
                </span>
              </label>
            )
          }
          if (spec.kind === 'int') {
            return (
              <label key={spec.key} className="threshold-field">
                <span className="label">{label}</span>
                <span className="threshold-input">
                  <input
                    type="text"
                    inputMode="numeric"
                    value={draft.params[spec.key] ?? ''}
                    placeholder={spec.hint}
                    disabled={busy}
                    aria-describedby={describedby}
                    onChange={(e) => setParam(spec.key, e.target.value)}
                  />
                  {spec.min !== undefined && spec.max !== undefined && (
                    <span className="hint">
                      {spec.min}–{spec.max}
                    </span>
                  )}
                </span>
              </label>
            )
          }
          return (
            <label key={spec.key} className="threshold-field">
              <span className="label">{label}</span>
              <span className="threshold-input">
                <input
                  type="text"
                  value={draft.params[spec.key] ?? ''}
                  placeholder={spec.hint}
                  disabled={busy}
                  aria-describedby={describedby}
                  onChange={(e) => setParam(spec.key, e.target.value)}
                />
              </span>
            </label>
          )
        })}
      </div>
    )
  }

  return (
    <>
      <div className="config-form-grid">
        {numField('Interval', 's', 'intervalS')}
        {numField('Timeout', 's', 'timeoutS')}
        {numField('Train count', 'pkts', 'trainCount', 'default 10 (icmp)')}
        {numField('Train spacing', 'ms', 'trainSpacingMs', 'default 200')}
      </div>
      {paramFields()}
    </>
  )
}
