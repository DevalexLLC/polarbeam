import type { Role, UserAccount } from './types'

// Mirrors store.RoleIsNetworkScoped: these two carry a network set, the
// other two are global and the server refuses the field for them. (caps.ts
// keeps its own string-keyed set on purpose — it classifies whatever the
// server reports, while this one is total over the Role union.)
export const SCOPED_ROLES: ReadonlySet<Role> = new Set<Role>(['network_admin', 'network_viewer'])
export const ALL_ROLES: Role[] = ['admin', 'viewer', 'network_admin', 'network_viewer']

// Only a local, scoped account has an editable scope: the server refuses the
// field for a global role, and a federated account's networks are re-derived
// from the IdP mapping on every login.
export const scopeEditable = (u: UserAccount) =>
  u.status !== 'deleted' && u.auth_source === 'local' && u.networks !== null
