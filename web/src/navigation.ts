export type PrimaryView =
  | 'overview'
  | 'pair'
  | 'target'
  | 'targets'
  | 'incidents'
  | 'routes'
  | 'agents'
  | 'settings'
  | 'about'

export interface NavigationItem {
  href: string
  label: string
}

export const PRIMARY_NAVIGATION: readonly NavigationItem[] = [
  { href: '#/', label: 'Overview' },
  { href: '#/incidents', label: 'Incidents' },
  { href: '#/routes', label: 'Routes' },
  { href: '#/targets', label: 'Targets' },
  { href: '#/agents', label: 'Agents' },
]

export const SETTINGS_NAVIGATION: NavigationItem = { href: '#/settings', label: 'Settings' }

export function navigationItemIsCurrent(href: string, view: PrimaryView): boolean {
  if (href === '#/') return view === 'overview' || view === 'pair'
  if (href === '#/targets') return view === 'targets' || view === 'target'
  return href === `#/${view}`
}

// Return a destination only when Tab would otherwise leave the drawer.
// Keeping this calculation pure makes the focus-containment edge cases
// testable without adding a DOM test dependency to the air-gapped SPA.
export function wrappedTabIndex(current: number, count: number, backwards: boolean): number | null {
  if (count <= 0) return null
  if (current < 0) return backwards ? count - 1 : 0
  if (backwards && current === 0) return count - 1
  if (!backwards && current === count - 1) return 0
  return null
}
