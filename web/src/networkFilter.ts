import { useSyncExternalStore } from 'react'

// Global network-filter store, mirroring timezone.ts. '' means all networks
// — the pre-networks fold — which is the codebase-wide convention for the
// selector value. Views subscribe through useNetworkFilter so a change in
// the top-bar popover re-renders them without prop-drilling the value
// through memoized subtrees (see the rationale in timezone.ts).
//
// The persisted value can go stale: the filtered network may be deleted, or
// the install may collapse back to a single plane (where the filter UI is
// hidden entirely). App reconciles against every /api/v1/config/networks
// fetch so a stale filter clears itself instead of pinning the dashboard to
// an empty subset with no visible control to escape it.

const STORAGE_KEY = 'polarbeam-network-filter'
const listeners = new Set<() => void>()

function readStored(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) ?? ''
  } catch {
    /* storage unavailable; behave as all networks */
    return ''
  }
}

let network = readStored()

export function getNetworkFilter(): string {
  return network
}

export function setNetworkFilter(next: string) {
  network = next
  try {
    if (next === '') localStorage.removeItem(STORAGE_KEY)
    else localStorage.setItem(STORAGE_KEY, next)
  } catch {
    /* preference just won't survive reload */
  }
  for (const l of listeners) l()
}

// Called by App after each networks fetch. Clears a filter naming an
// unknown network, and any filter on a single-network install — there the
// top-bar control is not rendered, so a lingering value would be unescapable.
export function reconcileNetworkFilter(known: string[]) {
  if (network === '') return
  if (known.length > 1 && known.includes(network)) return
  console.warn(`network filter "${network}" no longer applies; showing all networks`)
  setNetworkFilter('')
}

// True when a row belongs to the filtered plane. Rows whose plane is
// unknown ('' — outage/path events outlive their agent row) stay visible
// under any filter: an incident must never vanish because it can no longer
// be attributed to a network.
export function matchesNetworkFilter(filter: string, plane: string): boolean {
  return filter === '' || plane === '' || plane === filter
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

export function useNetworkFilter(): { network: string; setNetwork: (n: string) => void } {
  const n = useSyncExternalStore(subscribe, getNetworkFilter)
  return { network: n, setNetwork: setNetworkFilter }
}
