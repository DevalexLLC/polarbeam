import type { Role, User } from './types'

// The SPA's mirror of the two route wrappers in
// internal/server/httpapi/httpapi.go. The field names ARE the wrapper names
// on purpose: a reviewer who sees `caps.networkWrite` on a panel can grep
// the server's route table and settle the question in one hop.
//
// Deliberately NOT a hierarchy, and never a third "convenience" boolean that
// is a disjunction of these two. The server's requireRole is an exact string
// compare (store/roles.go says never add a hierarchy), and that exactness is
// what denies a tenant every operator surface with no code of its own. The
// UI mirrors the guards; it never widens them.
export interface Caps {
  role: Role
  /** May call routes mounted behind adminWrite(...) — an exact `admin` compare. */
  adminWrite: boolean
  /** May call routes mounted behind networkWrite(...) — admin or network_admin. */
  networkWrite: boolean
  /**
   * The caller's planes: null for a global role (may omit `network` on a
   * write), [] for a scoped role with nothing assigned, else the server's
   * sorted scope.
   */
  networks: string[] | null
}

const SCOPED_ROLES: ReadonlySet<string> = new Set(['network_admin', 'network_viewer'])

// Total over unknown role strings: an old cached bundle talking to a newer
// server must degrade to least privilege, never to admin. A scoped role
// whose `networks` field is missing gets [], never null — null means
// "global, unfiltered" and would be over-permissive. Mirrors
// store.RoleIsNetworkScoped exactly.
export function capsOf(user: User): Caps {
  const role: string = user.role
  return {
    role: user.role,
    adminWrite: role === 'admin',
    networkWrite: role === 'admin' || role === 'network_admin',
    networks: SCOPED_ROLES.has(role) ? (user.networks ?? []) : null,
  }
}

/** True when the caller is limited to a set of planes rather than seeing all of them. */
export const isScoped = (c: Caps): boolean => c.networks !== null

// A row naming no plane ('') is the operator's alone: the global target
// every network may probe, and the all-planes threshold override that
// applies to every tenant. A plane-qualified row follows networkWrite.
// Meshes and probes never carry a '' row — their plane is always set.
export const canWriteRow = (c: Caps, network: string): boolean => (network === '' ? c.adminWrite : c.networkWrite)

const ROLE_LABELS: Record<Role, string> = {
  admin: 'Administrator',
  viewer: 'Viewer',
  network_admin: 'Network administrator',
  network_viewer: 'Network viewer',
}

// Falls back to the raw string so a role this build predates still renders
// something honest instead of "undefined".
export const roleLabel = (role: Role): string => ROLE_LABELS[role] ?? role
