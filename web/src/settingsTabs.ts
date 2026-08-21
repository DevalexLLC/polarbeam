import type { Caps } from './caps'

// Hash routing keeps the original route names as aliases, so bookmarks
// survive; the union lives here rather than in App so the view and the
// router share one definition instead of two lists that must be kept in
// step (they previously were three, counting the intro blurbs).
export type SettingsTab =
  | 'thresholds'
  | 'sites'
  | 'networks'
  | 'targets'
  | 'meshes'
  | 'probes'
  | 'enrollment'
  | 'users'
  | 'authentication'
  | 'banner'

export interface SettingsTabDef {
  tab: SettingsTab
  href: string
  label: string
  intro: string
  // The server guard behind this tab's own write surface. This table is the
  // client-side echo of the route dispositions in httpapi.go: a tab is
  // offered only when the caller could actually do something with it.
  need: 'adminWrite' | 'networkWrite'
}

// Order is nav order, and therefore also landing order: the first tab a
// role may open is where an unknown or forbidden hash lands.
export const SETTINGS_TABS: SettingsTabDef[] = [
  {
    tab: 'thresholds',
    href: '#/settings',
    label: 'Thresholds',
    intro: 'Shared thresholds used to classify network health across the dashboard.',
    need: 'networkWrite',
  },
  {
    tab: 'sites',
    href: '#/settings/sites',
    label: 'Sites',
    intro: 'The locations agents enroll into, with optional map placement and display metadata.',
    need: 'adminWrite',
  },
  {
    tab: 'networks',
    href: '#/settings/networks',
    label: 'Networks',
    intro: 'Connectivity planes. Agents join one at enrollment; meshes and direct probes measure within exactly one.',
    need: 'adminWrite',
  },
  {
    tab: 'targets',
    href: '#/settings/targets',
    label: 'Targets',
    intro: 'External hosts and URLs that site agents probe.',
    need: 'networkWrite',
  },
  {
    tab: 'meshes',
    href: '#/settings/meshes',
    label: 'Meshes',
    intro: 'Site groups whose members probe each other in both directions.',
    need: 'networkWrite',
  },
  {
    tab: 'probes',
    href: '#/settings/probes',
    label: 'Probes',
    intro: 'The measurement workload pushed to every affected agent within ~30 seconds.',
    need: 'networkWrite',
  },
  {
    tab: 'enrollment',
    href: '#/settings/enrollment',
    label: 'Enrollment',
    intro: 'Single-use join tokens that enroll new agents into a site.',
    need: 'networkWrite',
  },
  {
    tab: 'users',
    href: '#/settings/users',
    label: 'Users',
    intro: 'Dashboard accounts across local and single sign-on, with sign-in activity.',
    need: 'adminWrite',
  },
  {
    tab: 'authentication',
    href: '#/settings/authentication',
    label: 'Authentication',
    intro: 'Optional single sign-on via an OpenID Connect provider. Local accounts always keep working.',
    need: 'adminWrite',
  },
  {
    tab: 'banner',
    href: '#/settings/banner',
    label: 'Banner',
    intro: 'An optional marking shown at the top and bottom of every screen, the sign-in page included.',
    need: 'adminWrite',
  },
]

export const visibleTabs = (caps: Caps): SettingsTabDef[] => SETTINGS_TABS.filter((t) => caps[t.need])

// Settings is offered when the caller can act on something there. A viewer
// and a network_viewer read every config surface through the pages that
// already show it, so the entry point stays hidden for them exactly as it
// was before tenancy.
export const canOpenSettings = (caps: Caps): boolean => caps.adminWrite || caps.networkWrite

// Resolves a raw hash segment against what this role may open. Unknown tabs
// (a stale bookmark) and forbidden ones (a deep link, or a role change
// mid-session) both land on the first permitted tab rather than on a wall.
// Returns null when the role may open no tab at all.
export function resolveTab(raw: string | undefined, caps: Caps): SettingsTab | null {
  const allowed = visibleTabs(caps)
  if (allowed.length === 0) return null
  const hit = allowed.find((t) => t.tab === raw)
  return hit ? hit.tab : allowed[0].tab
}
