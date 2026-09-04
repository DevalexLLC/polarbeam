import type { PlaneChoice } from '../plane'

// Renders the four shapes of PlaneChoice once, so no panel re-derives when
// to show a network selector. Nothing for a global caller on a single-plane
// install (the pre-networks look is unchanged), a static chip when the
// caller has exactly one plane, a selector when there is a real choice, and
// a loud notice when a scoped account has no networks at all — that last one
// is a real server state, not an edge case, and every write from it would
// fail.
export default function PlaneField({
  choice,
  value,
  onChange,
  disabled,
  label = 'Network',
  hint,
  invalid,
  describedby,
}: {
  choice: PlaneChoice
  value: string
  onChange: (v: string) => void
  disabled?: boolean
  label?: string
  hint?: string
  // Error-summary wiring (WCAG 3.3.1): applied to the select only — the
  // static shapes have nothing the user can correct.
  invalid?: boolean
  describedby?: string
}) {
  if (choice.kind === 'implicit') return null
  if (choice.kind === 'unknown') {
    return (
      <div className="inline-alert" role="status">
        Loading networks… the plane this will be created on is not known yet.
      </div>
    )
  }
  if (choice.kind === 'none') {
    return (
      <div className="inline-alert" role="status">
        No networks are assigned to your account, so there is nowhere to create this. Ask an administrator to assign
        one.
      </div>
    )
  }
  if (choice.kind === 'fixed') {
    return (
      <label className="threshold-field">
        <span className="label">{label}</span>
        <span className="threshold-input">
          <span className="chip">{choice.plane}</span>
          {hint && <span className="hint">{hint}</span>}
        </span>
      </label>
    )
  }
  return (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input">
        <select
          value={value}
          disabled={disabled}
          aria-invalid={invalid || undefined}
          aria-describedby={describedby}
          onChange={(e) => onChange(e.target.value)}
        >
          {choice.options.map((n) => (
            <option key={n} value={n}>
              {n === '' ? 'All networks (operator-owned)' : n}
            </option>
          ))}
        </select>
        {hint && <span className="hint">{hint}</span>}
      </span>
    </label>
  )
}
