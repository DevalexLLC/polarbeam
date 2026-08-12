// Declutter pass for the operations map: geographically close sites can
// project so near each other that one marker's invisible hit circle buries a
// neighbor entirely (SVG hit-testing is topmost-wins), making the buried
// site unhoverable and unclickable. This module nudges marker centers just
// far enough apart that every site keeps a reachable core, without pulling
// in a layout dependency — like geo.ts, it hand-rolls the math so the
// air-gapped build stays dependency-free.

export interface DeclutterNode {
  x: number // projected center, viewBox px
  y: number
  hitR: number // invisible hit radius — the quantity that causes burial
}

// Separation slack beyond the larger hit radius: once every pair is
// minSep apart, each center plus a PAD-radius disc lies outside every
// neighbor's hit circle, so topmost-wins hit-testing always leaves a
// clickable region at every core. Full bubble separation is deliberately
// NOT required — the translucent bubbles overlap legibly; only the
// hit-burial must be cured.
const PAD = 8

// Upper bound on relaxation sweeps. Sweeps stop early only at a fixed
// point (a pass that moved nothing), which is output-identical to running
// them all — so the result stays a total function of the input and
// identical data can never lay out differently between renders (no
// jitter).
const ITERATIONS = 24

// How far a marker may drift from its true projection. ~20 viewBox px is
// under 2% of map width — beyond that the map would lie about geography,
// so the cap wins over full separation in pathological pileups (leader
// marks and keyboard focus keep buried sites reachable there).
const MAX_SHIFT = 20

// Repulsion direction for exactly coincident sites, where the separation
// vector is undefined. The golden angle fans k stacked sites into a
// rosette on the first sweep instead of shoving them along one axis.
const GOLDEN_ANGLE = Math.PI * (3 - Math.sqrt(5))

// Returns display positions, same order/length as nodes. Pure and
// deterministic: same input array (order included) always yields the same
// output — the caller must pass nodes in a stable order (sorted by site
// name), never API response order.
export function declutter(nodes: DeclutterNode[], view: { w: number; h: number }): { x: number; y: number }[] {
  const pos = nodes.map((n) => ({ x: n.x, y: n.y }))
  for (let sweep = 0; sweep < ITERATIONS; sweep++) {
    // Accumulate-then-apply (Jacobi style): all pairwise displacements are
    // computed against this sweep's positions before any are applied, so
    // the result cannot depend on pair iteration order.
    const dx = pos.map(() => 0)
    const dy = pos.map(() => 0)
    for (let i = 0; i < pos.length; i++) {
      for (let j = i + 1; j < pos.length; j++) {
        const minSep = Math.max(nodes[i].hitR, nodes[j].hitR) + PAD
        let ux = pos[j].x - pos[i].x
        let uy = pos[j].y - pos[i].y
        const dist = Math.hypot(ux, uy)
        if (dist >= minSep) continue
        if (dist < 1e-6) {
          const theta = (i * nodes.length + j) * GOLDEN_ANGLE
          ux = Math.cos(theta)
          uy = Math.sin(theta)
        } else {
          ux /= dist
          uy /= dist
        }
        const push = (minSep - dist) / 2
        dx[i] -= ux * push
        dy[i] -= uy * push
        dx[j] += ux * push
        dy[j] += uy * push
      }
    }
    let changed = false
    for (let i = 0; i < pos.length; i++) {
      let x = pos[i].x + dx[i]
      let y = pos[i].y + dy[i]
      // Geographic-honesty cap: clamp radially back toward the true
      // projection rather than letting relaxation walk a marker away from
      // its city.
      const sx = x - nodes[i].x
      const sy = y - nodes[i].y
      const shift = Math.hypot(sx, sy)
      if (shift > MAX_SHIFT) {
        x = nodes[i].x + (sx / shift) * MAX_SHIFT
        y = nodes[i].y + (sy / shift) * MAX_SHIFT
      }
      // A nudge must never push a marker's hit area off-canvas.
      const { hitR } = nodes[i]
      x = Math.min(Math.max(x, hitR + 2), view.w - hitR - 2)
      y = Math.min(Math.max(y, hitR + 2), view.h - hitR - 2)
      if (x !== pos[i].x || y !== pos[i].y) changed = true
      pos[i].x = x
      pos[i].y = y
    }
    // A sweep that moved nothing is a fixed point — every remaining sweep
    // would be a no-op, so stopping cannot change the output, only the
    // cost. The layout recomputes on every poll refresh, and the common
    // case (no overlaps anywhere) now pays one pass instead of 24. The
    // test is "nothing moved", not "no overlapping pair": the edge clamp
    // can move a node all by itself, and in principle create a new overlap
    // a later sweep must resolve.
    if (!changed) break
  }
  return pos
}

// Bubble radius from a site's link count. Lives here (not in JSX) because
// the declutter pass needs radii before render.
export const bubbleRadius = (degree: number) => Math.max(9, Math.min(24, 8 + 3.5 * Math.sqrt(degree)))
