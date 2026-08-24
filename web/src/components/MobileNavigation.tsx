import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { wrappedTabIndex } from '../navigation'

interface MobileNavigationItem {
  href: string
  label: string
  current: boolean
  sameRoute: boolean
}

const FOCUSABLE = 'a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])'

export default function MobileNavigation({
  items,
  routeKey,
}: {
  items: readonly MobileNavigationItem[]
  routeKey: string
}) {
  const [open, setOpen] = useState(false)
  const toggleRef = useRef<HTMLButtonElement>(null)
  const drawerRef = useRef<HTMLElement>(null)
  const returnFocusRef = useRef(true)
  const previousRouteRef = useRef(routeKey)

  const close = useCallback((returnFocus = true) => {
    returnFocusRef.current = returnFocus
    setOpen(false)
  }, [])

  const onDrawerKeyDown = useCallback(
    (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        close()
        return
      }
      if (event.key !== 'Tab' || !drawerRef.current) return
      const focusable = Array.from(drawerRef.current.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
        (element) => element.getClientRects().length > 0,
      )
      const current = focusable.indexOf(document.activeElement as HTMLElement)
      const destination = wrappedTabIndex(current, focusable.length, event.shiftKey)
      if (destination === null) return
      event.preventDefault()
      focusable[destination].focus()
    },
    [close],
  )

  useEffect(() => {
    if (!open) return
    const previousOverflow = document.body.style.overflow
    const toggle = toggleRef.current
    const drawer = drawerRef.current
    document.body.style.overflow = 'hidden'
    drawer?.addEventListener('keydown', onDrawerKeyDown)
    const frame = requestAnimationFrame(() => {
      const current = drawerRef.current?.querySelector<HTMLElement>('[aria-current="page"]')
      const first = drawerRef.current?.querySelector<HTMLElement>(FOCUSABLE)
      ;(current ?? first)?.focus()
    })
    return () => {
      cancelAnimationFrame(frame)
      drawer?.removeEventListener('keydown', onDrawerKeyDown)
      document.body.style.overflow = previousOverflow
      if (returnFocusRef.current) toggle?.focus()
      returnFocusRef.current = true
    }
  }, [onDrawerKeyDown, open])

  // Hash changes can come from Back/Forward or another in-app control, not
  // only a drawer link. Either way, stale navigation chrome must disappear.
  useEffect(() => {
    if (previousRouteRef.current !== routeKey && open) close(false)
    previousRouteRef.current = routeKey
  }, [close, open, routeKey])

  // A drawer left open while the viewport grows must not keep body scrolling
  // locked behind an element that desktop CSS no longer displays.
  useEffect(() => {
    const media = window.matchMedia('(max-width: 760px)')
    const onChange = () => {
      if (!media.matches) close(false)
    }
    media.addEventListener('change', onChange)
    return () => media.removeEventListener('change', onChange)
  }, [close])

  const drawer = open
    ? createPortal(
        <div className="mobile-nav-layer">
          <button
            type="button"
            className="mobile-nav-backdrop"
            tabIndex={-1}
            aria-hidden="true"
            onClick={() => close()}
          />
          <aside
            ref={drawerRef}
            id="mobile-navigation"
            className="mobile-nav-drawer"
            role="dialog"
            aria-modal="true"
            aria-label="Primary navigation"
            tabIndex={-1}
          >
            <div className="mobile-nav-heading">
              <span>Navigate</span>
              <button type="button" className="mobile-nav-close" aria-label="Close navigation" onClick={() => close()}>
                <svg viewBox="0 0 24 24" aria-hidden="true">
                  <path d="m6 6 12 12M18 6 6 18" />
                </svg>
              </button>
            </div>
            <nav aria-label="Mobile primary navigation">
              {items.map((item) => (
                <a
                  key={item.href}
                  href={item.href}
                  className="mobile-nav-link"
                  aria-current={item.current ? 'page' : undefined}
                  onClick={() => close(item.sameRoute)}
                >
                  {item.label}
                </a>
              ))}
            </nav>
          </aside>
        </div>,
        document.body,
      )
    : null

  return (
    <>
      <button
        ref={toggleRef}
        type="button"
        className="mobile-nav-toggle"
        aria-label="Open navigation"
        aria-expanded={open}
        aria-controls="mobile-navigation"
        onClick={() => {
          returnFocusRef.current = true
          setOpen(true)
        }}
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M4 7h16M4 12h16M4 17h16" />
        </svg>
      </button>
      {drawer}
    </>
  )
}
