// A checkbox list rather than a multi-select: scope is a small set the
// operator must be able to read back at a glance, and <select multiple> is
// notoriously easy to clear by accident.
export default function NetworkPicker({
  all,
  value,
  disabled,
  onChange,
}: {
  all: string[]
  value: string[]
  disabled?: boolean
  onChange: (next: string[]) => void
}) {
  if (all.length === 0) {
    return <span className="hint">no networks exist yet — create one under Settings → Networks</span>
  }
  return (
    <div className="chips users-network-picker">
      {all.map((n) => (
        <label key={n} className="chip">
          <input
            type="checkbox"
            checked={value.includes(n)}
            disabled={disabled}
            onChange={(e) => onChange(e.target.checked ? [...value, n] : value.filter((v) => v !== n))}
          />
          {n}
        </label>
      ))}
    </div>
  )
}
