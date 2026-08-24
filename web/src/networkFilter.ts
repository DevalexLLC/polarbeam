import { useSyncExternalStore } from 'react'
import {
  canonicalizeRouteHash,
  replaceCanonicalRoute,
  routeHashSnapshot,
  routeParam,
  subscribeRouteState,
  updateRouteParams,
} from './routeState'

// The network-filter URL adapter. '' means all networks — the pre-networks
// fold — which is the codebase-wide convention for the selector value.
//
// The URL value can go stale: the filtered network may be deleted, or
// the install may collapse back to a single plane (where the filter UI is
// hidden entirely). App reconciles against every /api/v1/config/networks
// fetch so a stale filter clears itself instead of pinning the dashboard to
// an empty subset with no visible control to escape it.

export function getNetworkFilter(): string {
  return routeParam(routeHashSnapshot(), 'network')
}

export function setNetworkFilter(next: string) {
  updateRouteParams({ network: next || null })
}

// Called by App after each networks fetch. Clears a filter naming an
// unknown network, and any filter on a single-network install — there the
// top-bar control is not rendered, so a lingering value would be unescapable.
export function reconcileNetworkFilter(known: string[]) {
  const current = getNetworkFilter()
  const canonical = canonicalizeRouteHash(routeHashSnapshot(), { knownNetworks: known })
  if (!canonical.changed) return
  if (current) console.warn(`network filter "${current}" no longer applies; showing all networks`)
  replaceCanonicalRoute({ knownNetworks: known })
}

// True when a row belongs to the filtered plane. Rows whose plane is
// unknown ('' — outage/path events outlive their agent row) stay visible
// under any filter: an incident must never vanish because it can no longer
// be attributed to a network.
export function matchesNetworkFilter(filter: string, plane: string): boolean {
  return filter === '' || plane === '' || plane === filter
}

export function useNetworkFilter(): { network: string; setNetwork: (n: string) => void } {
  const hash = useSyncExternalStore(subscribeRouteState, routeHashSnapshot)
  const n = routeParam(hash, 'network')
  return { network: n, setNetwork: setNetworkFilter }
}
