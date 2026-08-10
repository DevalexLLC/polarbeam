import { useEffect, useRef, useState } from 'react'

// Two-step destructive action: the first click arms the button (label flips
// to the confirm text carrying the blast radius), the second commits; it
// disarms itself after a pause. Keyboard-accessible with no modal/overlay
// machinery, which this codebase deliberately has none of.
const ARM_MS = 4000

export default function ConfirmButton({
  label,
  confirmLabel,
  disabled = false,
  title,
  onConfirm,
}: {
  label: string
  confirmLabel: string
  disabled?: boolean
  title?: string
  onConfirm: () => void
}) {
  const [armed, setArmed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => () => clearTimeout(timer.current), [])

  const click = () => {
    if (!armed) {
      setArmed(true)
      clearTimeout(timer.current)
      timer.current = setTimeout(() => setArmed(false), ARM_MS)
      return
    }
    clearTimeout(timer.current)
    setArmed(false)
    onConfirm()
  }

  return (
    <button
      type="button"
      className={'secondary-button inline-confirm' + (armed ? ' armed' : '')}
      disabled={disabled}
      title={title}
      aria-live="polite"
      onClick={click}
    >
      {armed ? confirmLabel : label}
    </button>
  )
}
