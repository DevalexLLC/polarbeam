import { useEffect, useRef, useState, type ReactNode } from 'react'

// Focusable menu items inside the popover; excludes the summary itself and
// anything managed by its own roving tabindex (e.g. a RadioButtonGroup's
// unchecked radios, which carry tabindex="-1").
const ITEMS = 'a[href], button:not([disabled]):not([tabindex="-1"]), input:not([disabled]), select:not([disabled])'

// A menu item may open a modal <dialog> (confirm, change password): focus
// moving into it, or a click on it, must not dismiss the menu — the dialog
// returns focus to the item that opened it, which has to stay attached and
// visible.
function inModalDialog(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest('dialog') !== null
}

// The APG disclosure-menu behavior the native <details> element lacks:
// Escape closes and returns focus to the summary, ArrowDown/ArrowUp rove
// through the items (Home/End jump), a click outside dismisses, and focus
// leaving the menu dismisses without stealing focus back. Items keep their
// native roles — links stay links, buttons stay buttons — matching the
// prefer-tag-over-role stance in .oxlintrc.json. Listeners attach to the
// <details> in an effect (the MobileNavigation pattern): they delegate for
// the whole subtree, which is not something a JSX handler on a
// non-interactive element can express to the lint rules.
export default function DisclosureMenu({
  className,
  summaryLabel,
  summaryChildren,
  children,
}: {
  className: string
  summaryLabel: string
  summaryChildren: ReactNode
  children: ReactNode
}) {
  const details = useRef<HTMLDetailsElement>(null)
  const [open, setOpen] = useState(false)

  useEffect(() => {
    const element = details.current
    if (!element) return

    const items = () => {
      const popover = element.querySelector(':scope > :not(summary)')
      return Array.from(popover?.querySelectorAll<HTMLElement>(ITEMS) ?? []).filter(
        (item) => item.getClientRects().length > 0,
      )
    }

    const close = (focusSummary: boolean) => {
      element.removeAttribute('open')
      if (focusSummary) element.querySelector<HTMLElement>('summary')?.focus()
    }

    const onKeyDown = (event: KeyboardEvent) => {
      // A child widget with its own arrow handling (radiogroup) wins.
      if (event.defaultPrevented) return
      if (event.key === 'Escape') {
        if (!element.open) return
        event.preventDefault()
        close(true)
        return
      }
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
      if (!element.open) {
        // Only the arrows open from the summary (APG: Down focuses the
        // first item, Up the last); Home/End keep their page behavior.
        if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return
        event.preventDefault()
        element.setAttribute('open', '')
        // Re-query after the popover renders its content.
        const fromEnd = event.key === 'ArrowUp'
        requestAnimationFrame(() => {
          const list = items()
          ;(fromEnd ? list[list.length - 1] : list[0])?.focus()
        })
        return
      }
      event.preventDefault()
      const list = items()
      if (list.length === 0) return
      const current = list.indexOf(document.activeElement as HTMLElement)
      let destination: number
      if (event.key === 'Home') destination = 0
      else if (event.key === 'End') destination = list.length - 1
      else if (current === -1) destination = event.key === 'ArrowDown' ? 0 : list.length - 1
      else destination = (current + (event.key === 'ArrowDown' ? 1 : -1) + list.length) % list.length
      list[destination].focus()
    }

    const onFocusOut = (event: FocusEvent) => {
      if (!element.open) return
      const next = event.relatedTarget as Node | null
      if (element.contains(next) || inModalDialog(next)) return
      close(false)
    }

    element.addEventListener('keydown', onKeyDown)
    element.addEventListener('focusout', onFocusOut)
    return () => {
      element.removeEventListener('keydown', onKeyDown)
      element.removeEventListener('focusout', onFocusOut)
    }
  }, [])

  useEffect(() => {
    if (!open) return
    const dismiss = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (target && (details.current?.contains(target) || inModalDialog(target))) return
      details.current?.removeAttribute('open')
    }
    document.addEventListener('pointerdown', dismiss, true)
    return () => document.removeEventListener('pointerdown', dismiss, true)
  }, [open])

  return (
    <details ref={details} className={className} onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary aria-label={summaryLabel}>{summaryChildren}</summary>
      {children}
    </details>
  )
}
