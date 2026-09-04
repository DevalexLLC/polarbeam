// Directional beams for the operations map. A link is one unordered site
// pair; each of its two directions carries the severity of that direction's
// matrix cell, so the map can draw what the product measures: A→B and B→A
// as separate strokes, each graded on its own.
import { cellSeverity, type Severity, type ThresholdResolver } from './severity.ts'
import type { MatrixCell } from './types'

export interface SiteLink {
  a: string // a < b, so the pair has one canonical key
  b: string
  ab: Severity | null // null when that direction has no cell at all
  ba: Severity | null
}

export function buildSiteLinks(cells: MatrixCell[], thresholds: ThresholdResolver): SiteLink[] {
  const byPair = new Map<string, SiteLink>()
  for (const cell of cells) {
    if (cell.src === cell.dst) continue
    const forward = cell.src < cell.dst
    const [a, b] = forward ? [cell.src, cell.dst] : [cell.dst, cell.src]
    const key = a + '\u0000' + b
    const link = byPair.get(key) ?? { a, b, ab: null, ba: null }
    const severity = cellSeverity(cell, thresholds)
    if (forward) link.ab = severity
    else link.ba = severity
    byPair.set(key, link)
  }
  // Name order is the determinism anchor: paint order must not depend on
  // API response ordering.
  // oxlint-disable-next-line unicorn/no-array-sort -- sorting a fresh array
  return [...byPair.values()].sort((p, q) => (p.a < q.a ? -1 : p.a > q.a ? 1 : p.b < q.b ? -1 : p.b > q.b ? 1 : 0))
}

export interface BeamEnd {
  x: number
  y: number
  r: number // bubble radius the beam must stop short of
}

export interface BeamSegment {
  x1: number
  y1: number
  x2: number
  y2: number
}

// The two directions of one pair run as parallel strokes a fixed gap apart,
// each on its own side of the center line, trimmed so they start at the
// origin bubble's edge and end (arrowhead included) at the destination's.
// `side` is +1 for a→b and -1 for b→a; both directions of a pair must be
// computed from the same (from, to) orientation for the sides to pair up,
// so callers pass a→b geometry and let `side` flip the direction.
export const BEAM_GAP = 2.4
const BEAM_CLEARANCE = 1.5

export function beamGeometry(from: BeamEnd, to: BeamEnd, side: 1 | -1): BeamSegment | null {
  const dx = to.x - from.x
  const dy = to.y - from.y
  const length = Math.hypot(dx, dy)
  const usable = length - from.r - to.r - 2 * BEAM_CLEARANCE
  // Overlapping or touching bubbles have no room for a legible beam.
  if (usable < 6) return null
  const ux = dx / length
  const uy = dy / length
  // Perpendicular offset; the sign puts each direction on its own side.
  const ox = -uy * BEAM_GAP * side
  const oy = ux * BEAM_GAP * side
  const start = from.r + BEAM_CLEARANCE
  const end = length - to.r - BEAM_CLEARANCE
  const seg = {
    x1: from.x + ux * start + ox,
    y1: from.y + uy * start + oy,
    x2: from.x + ux * end + ox,
    y2: from.y + uy * end + oy,
  }
  // The b→a stroke travels the other way: swap its ends so the arrowhead
  // sits at the true destination.
  return side === 1 ? seg : { x1: seg.x2, y1: seg.y2, x2: seg.x1, y2: seg.y1 }
}
