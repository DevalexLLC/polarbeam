import type { Caps } from './caps'
import { isScoped } from './caps'

// Which plane a config form submits, resolved once from the caller's role
// and the planes it can see.
//
// This replaces the `multiNetwork = networks.length > 1` heuristic that used
// to decide it. That test conflated three separate questions — must I name a
// plane, do I have a choice to offer, and may I say "no plane at all" — and
// got the first one wrong for a tenant scoped to exactly one network: the
// form sent nothing, the server resolved 'default', and the write failed
// against a plane the tenant cannot even see.
//
// GET /api/v1/config/networks is already scope-filtered server-side, so the
// list a panel fetches IS the caller's scope. Nothing here re-derives it.
export type PlaneChoice =
  // Scoped caller with no networks assigned. Every write would 404, so the
  // form must refuse rather than submit and render the server's error.
  | { kind: 'none' }
  // Scoped caller with exactly one plane: no choice to offer, but the value
  // is still sent explicitly. Rendered as a static chip so the tenant can
  // see where its writes land.
  | { kind: 'fixed'; plane: string }
  // Global caller on a single-plane install: render nothing, keep the
  // pre-networks request shape.
  | { kind: 'implicit'; plane: string }
  // Render a selector.
  | { kind: 'choice'; options: string[]; initial: string }

export interface PlaneOpts {
  // Set for the two surfaces that have an operator-owned "no plane" row:
  // POST /config/targets with `network` omitted creates the global target
  // every plane shares, and a path-threshold write with no ?network= sets
  // the all-planes override. Both are reserved to a global admin, so the ''
  // option is offered to global callers only.
  allowGlobal?: boolean
}

// The plane the form should start on. Never the literal 'default' for a
// scoped caller — that plane is usually not theirs.
export function planeChoice(caps: Caps, known: string[], opts: PlaneOpts = {}): PlaneChoice {
  if (isScoped(caps)) {
    if (known.length === 0) return { kind: 'none' }
    if (known.length === 1) return { kind: 'fixed', plane: known[0] }
    return { kind: 'choice', options: known, initial: known[0] }
  }
  if (opts.allowGlobal) {
    // '' first: a global admin publishing a target or an all-planes
    // override is the pre-tenancy default and stays the default here.
    return { kind: 'choice', options: ['', ...known], initial: '' }
  }
  if (known.length > 1) return { kind: 'choice', options: known, initial: 'default' }
  return { kind: 'implicit', plane: 'default' }
}

/** The value a form starts on, for every shape of choice. */
export function initialPlane(choice: PlaneChoice): string {
  switch (choice.kind) {
    case 'none':
      return ''
    case 'fixed':
    case 'implicit':
      return choice.plane
    case 'choice':
      return choice.initial
  }
}

/** True when the form should render a selector rather than a chip or nothing. */
export const planeSelectable = (choice: PlaneChoice): boolean => choice.kind === 'choice'

// Spreads into a JSON body. '' means "no plane" — the global target or the
// all-planes row — and omits the field, which is exactly what the server
// reads it as. Replaces every `...(x !== 'default' ? { network: x } : {})`
// in the codebase; that 'default' special case WAS the bug.
export function networkField(plane: string): { network?: string } {
  return plane === '' ? {} : { network: plane }
}

// For the path-threshold routes, where the plane rides as a query param
// rather than a path segment or a body field.
export const networkQuery = (plane: string): string => (plane === '' ? '' : `?network=${encodeURIComponent(plane)}`)
