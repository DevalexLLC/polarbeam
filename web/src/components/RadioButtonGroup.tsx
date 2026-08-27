import { useRef, type MouseEvent, type ReactNode } from 'react'

export interface RadioButtonOption<Value extends string> {
  value: Value
  label: ReactNode
}

// A mutually exclusive choice rendered as styled buttons: radiogroup
// semantics with the APG keyboard contract — arrows move AND select, the
// checked item is the group's single tab stop (roving tabindex).
export default function RadioButtonGroup<Value extends string>({
  label,
  className,
  value,
  options,
  onChange,
  onActivate,
}: {
  label: string
  className?: string
  value: Value
  options: readonly RadioButtonOption<Value>[]
  onChange: (value: Value) => void
  // Fired only for explicit activation (click / Enter / Space), not arrow
  // browsing — a popover host closes here without trapping arrow users.
  onActivate?: (value: Value, event: MouseEvent<HTMLButtonElement>) => void
}) {
  const group = useRef<HTMLDivElement>(null)
  const checked = options.findIndex((option) => option.value === value)
  return (
    <div
      ref={group}
      role="radiogroup"
      aria-label={label}
      className={className}
      tabIndex={-1}
      onKeyDown={(event) => {
        if (!['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight'].includes(event.key)) return
        event.preventDefault()
        const direction = event.key === 'ArrowDown' || event.key === 'ArrowRight' ? 1 : -1
        const destination = (Math.max(checked, 0) + direction + options.length) % options.length
        onChange(options[destination].value)
        group.current?.querySelectorAll<HTMLElement>('[role="radio"]')[destination]?.focus()
      }}
    >
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={option.value === value}
          // The group is one tab stop; if the value matches no option
          // (transient filter state), the first radio stays reachable.
          tabIndex={option.value === value || (checked === -1 && option === options[0]) ? 0 : -1}
          className={option.value === value ? 'active' : ''}
          onClick={(event) => {
            onChange(option.value)
            onActivate?.(option.value, event)
          }}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}
