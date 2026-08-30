// Sticky exact-lookup pin for the server-paged tables with a selected or
// expanded row (sites, probes, agents, routes): when the row is not in the
// loaded page, the request pins to an exact-ID query so the disclosure
// survives paging instead of being cleared.
//
// The pin decision needs the freshly committed page snapshot, but that
// snapshot comes from usePolledResource — whose request URL needs the
// decision. So the pin is state adjusted during render: read `pinnedID`
// BEFORE the resource hook to build the URL, call `reconcile` AFTER it
// with the fresh snapshot, and a changed decision re-renders immediately
// (before any effect can clear the selection) with the pinned URL.
import { useState } from 'react'

export function useStickyPin(selected: string | null): {
  // The exact-lookup pin currently in force, or null when the selected row
  // is (believed) present in the loaded page.
  pinnedID: string | null
  // Re-evaluate against the committed snapshot: `present` says whether the
  // selected row is in it (false while nothing is loaded yet). The pin is
  // sticky — once it targets the current selection it holds until the
  // selection changes — so a pinned fetch cycling through pages never
  // flickers the decision.
  reconcile: (present: boolean) => void
} {
  const target = selected || null
  const [pin, setPin] = useState<string | null>(target)
  const pinnedID = target !== null && pin === target ? target : null
  const reconcile = (present: boolean) => {
    const next = target === null ? null : pin === target ? target : present ? null : target
    if (next !== pin) setPin(next)
  }
  return { pinnedID, reconcile }
}
