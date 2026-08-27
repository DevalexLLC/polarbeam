import { useEffect, useId, useRef } from 'react'

// Wires a form's error summary to its fields (WCAG 3.3.1/3.3.2). The
// summary element takes `id`, `ref`, and tabIndex={-1}; each field the
// summary describes spreads `describedby`; `invalid` goes only onto
// controls the error specifically indicts — marking a whole form's valid
// fields invalid misleads screen-reader users. A failed submit calls
// `request()` so focus lands on the summary once it renders —
// the ref latch mirrors NetworksPanel's focus-after-commit pattern,
// because the summary element may not exist until React commits the
// error state.
export function useErrorSummary(hasErrors: boolean) {
  const element = useRef<HTMLElement | null>(null)
  const wanted = useRef(false)
  const id = useId()
  useEffect(() => {
    if (!hasErrors) {
      wanted.current = false
      return
    }
    if (wanted.current) {
      wanted.current = false
      element.current?.focus()
    }
  })
  return {
    id,
    ref: (el: HTMLElement | null) => {
      element.current = el
    },
    describedby: hasErrors ? id : undefined,
    invalid: hasErrors || undefined,
    request: () => {
      wanted.current = true
    },
  }
}
