// Generates web/src/assets/mapGeo.ts: the dot-matrix operations map — the
// landmass sampled as a hex-offset grid of dots, pre-projected through
// Natural Earth I into a 1080×600 frame. One-time dev
// tooling (same category as `npm ci` / `make vendor`) — the OUTPUT is
// committed and release builds never run this or fetch anything.
//
// Source data: world-atlas countries-110m.json (Natural Earth 1:110m via
// TopoJSON, public domain).
// One-time acquisition on a dev box with network, then:
//
//   curl -sSLo /tmp/countries-110m.json \
//     https://cdn.jsdelivr.net/npm/world-atlas@2.0.2/countries-110m.json
//   node web/tools/build-map-geo.mjs /tmp/countries-110m.json > web/src/assets/mapGeo.ts
//
// Projection: d3.geoNaturalEarth1().fitExtent([[14,24],[1066,576]],
// {type:'Sphere'}) re-implemented without d3 — the raw Natural Earth I
// polynomial below is the one d3-geo uses, and the fit constants (MAP_K,
// MAP_TX, MAP_TY) are solved from the projected sphere bounds exactly the way
// d3.fitExtent does. web/src/geo.ts carries the SAME raw formula and applies
// the emitted constants, so runtime site positions and this baked geometry
// can never drift. Land rings are Douglas–Peucker simplified in projected
// space before the grid is point-in-polygon tested against them.

import { readFileSync } from 'node:fs'

const VIEW_W = 1080
const VIEW_H = 600
const EXTENT = [
  [14, 24],
  [VIEW_W - 14, VIEW_H - 24],
]
const EPSILON = 0.35 // Douglas–Peucker tolerance, viewBox px
const MIN_RING_AREA = 2 // drop islands smaller than this many px²
const DOT_STEP = 7 // dot pitch in viewBox px; alternate rows offset by half

// Natural Earth I raw projection (Šavrič et al.), radians in, unit sphere
// out, y positive north. Identical to d3-geo's naturalEarth1Raw — keep in
// lockstep with naturalEarth1Raw() in web/src/geo.ts.
function naturalEarth1Raw(lambda, phi) {
  const phi2 = phi * phi
  const phi4 = phi2 * phi2
  return [
    lambda * (0.8707 - 0.131979 * phi2 + phi4 * (-0.013791 + phi4 * (0.003971 * phi2 - 0.001529 * phi4))),
    phi * (1.007226 + phi2 * (0.015085 + phi4 * (-0.044475 + 0.028874 * phi2 - 0.005916 * phi4))),
  ]
}

const RAD = Math.PI / 180
const raw = (lon, lat) => naturalEarth1Raw(lon * RAD, lat * RAD)

// The sphere's edge under a pseudocylindrical projection: both pole lines
// (curved, since x ≠ 0 at ±90°) and both antimeridians, walked clockwise.
function sphereOutline() {
  const pts = []
  for (let lon = -180; lon <= 180; lon += 1) pts.push([lon, 90])
  for (let lat = 90; lat >= -90; lat -= 1) pts.push([180, lat])
  for (let lon = 180; lon >= -180; lon -= 1) pts.push([lon, -90])
  for (let lat = -90; lat <= 90; lat += 1) pts.push([-180, lat])
  return pts
}

// Solve scale + translate so the projected sphere fits EXTENT, centered —
// the same arithmetic as d3's projection.fitExtent(extent, {type:'Sphere'}).
let minX = Infinity
let maxX = -Infinity
let minY = Infinity
let maxY = -Infinity
for (const [lon, lat] of sphereOutline()) {
  const [x, y] = raw(lon, lat)
  if (x < minX) minX = x
  if (x > maxX) maxX = x
  if (y < minY) minY = y
  if (y > maxY) maxY = y
}
const dx = EXTENT[1][0] - EXTENT[0][0]
const dy = EXTENT[1][1] - EXTENT[0][1]
const K = Math.min(dx / (maxX - minX), dy / (maxY - minY))
// Screen y grows downward, raw y grows northward, hence the sign flip.
const TX = EXTENT[0][0] + (dx - K * (minX + maxX)) / 2
const TY = EXTENT[0][1] + (dy + K * (minY + maxY)) / 2
const project = (lon, lat) => {
  const [x, y] = raw(lon, lat)
  return [TX + K * x, TY - K * y]
}

// ---- TopoJSON decode (delta-encoded quantized arcs) ----

const input = process.argv[2]
if (!input) {
  console.error('usage: node build-map-geo.mjs <countries-110m.json>')
  process.exit(2)
}
const topo = JSON.parse(readFileSync(input, 'utf8'))
const { scale, translate } = topo.transform
const arcs = topo.arcs.map((arc) => {
  let ax = 0
  let ay = 0
  return arc.map(([ddx, ddy]) => {
    ax += ddx
    ay += ddy
    return [ax * scale[0] + translate[0], ay * scale[1] + translate[1]]
  })
})

function ringLonLat(arcIndexes) {
  const pts = []
  for (const idx of arcIndexes) {
    let a = idx < 0 ? arcs[~idx].toReversed() : arcs[idx]
    if (pts.length > 0) a = a.slice(1) // consecutive arcs share an endpoint
    pts.push(...a)
  }
  return pts
}

// ---- Antimeridian cutting (lon/lat space, before projection) ----
//
// Natural Earth rings are NOT pre-cut at ±180°: Russia, Fiji, and a few
// Pacific islands jump from +179.x to −179.x mid-ring, and Antarctica winds
// a full revolution around the south pole. Projecting those vertex-wise
// draws 1000px streaks across the map interior. d3.geoPath does spherical
// clipping at render time; this bakes the equivalent cut into the asset.

const normLon = (d) => ((((d + 180) % 360) + 360) % 360) - 180

// Insert points along runs that sit on the ±180 meridian (or a pole line)
// so closure edges follow the projected curve, not a straight chord.
function densifyEdges(pts) {
  const out = []
  for (let i = 0; i < pts.length; i++) {
    const a = pts[i]
    const b = pts[(i + 1) % pts.length]
    out.push(a)
    const onMeridian = Math.abs(a[0]) === 180 && a[0] === b[0] && Math.abs(b[1] - a[1]) > 3
    const onPoleLine = Math.abs(a[1]) === 90 && a[1] === b[1] && Math.abs(b[0] - a[0]) > 3
    if (onMeridian) {
      const step = 2 * Math.sign(b[1] - a[1])
      for (let lat = a[1] + step; Math.sign(b[1] - lat) === Math.sign(step); lat += step) out.push([a[0], lat])
    } else if (onPoleLine) {
      const step = 2 * Math.sign(b[0] - a[0])
      for (let lon = a[0] + step; Math.sign(b[0] - lon) === Math.sign(step); lon += step) out.push([lon, a[1]])
    }
  }
  return out
}

// One Sutherland–Hodgman half-plane pass over a lon/lat ring.
function clipHalf(ring, inside, boundary) {
  const out = []
  for (let i = 0; i < ring.length; i++) {
    const cur = ring[i]
    const prev = ring[(i + ring.length - 1) % ring.length]
    const curIn = inside(cur)
    const prevIn = inside(prev)
    if (curIn !== prevIn) {
      const t = (boundary - prev[0]) / (cur[0] - prev[0])
      out.push([boundary, prev[1] + t * (cur[1] - prev[1])])
    }
    if (curIn) out.push(cur)
  }
  return out
}

// Sutherland–Hodgman clip of a lon/lat ring against lon ∈ [−180, 180].
function clipBand(pts) {
  let r = clipHalf(pts, (p) => p[0] >= -180, -180)
  if (r.length >= 3) r = clipHalf(r, (p) => p[0] <= 180, 180)
  return r.length >= 3 ? r : null
}

function cutAntimeridian(ring) {
  let pts = ring.slice()
  const first = pts[0]
  const last = pts[pts.length - 1]
  if (first[0] === last[0] && first[1] === last[1]) pts = pts.slice(0, -1)
  if (pts.length < 3) return []

  let jumpAt = -1
  for (let i = 0; i < pts.length; i++) {
    if (Math.abs(pts[(i + 1) % pts.length][0] - pts[i][0]) > 180) {
      jumpAt = i
      break
    }
  }
  if (jumpAt === -1) return [pts]

  // Winding: a ring that nets a full revolution encloses a pole.
  let winding = 0
  for (let i = 0; i < pts.length; i++) winding += normLon(pts[(i + 1) % pts.length][0] - pts[i][0])

  if (Math.abs(winding) > 180) {
    // Pole ring (Antarctica in this dataset). Rotate so the ring starts just
    // after its seam, then close it through the enclosed pole: down one edge
    // meridian, along the pole line, back up the other edge.
    const r = pts.slice(jumpAt + 1).concat(pts.slice(0, jumpAt + 1))
    const head = r[0]
    const tail = r[r.length - 1]
    const poleLat = r.reduce((s, p) => s + p[1], 0) / r.length < 0 ? -90 : 90
    const tailEdge = tail[0] > 0 ? 180 : -180
    const headEdge = head[0] > 0 ? 180 : -180
    const span = 360 - Math.abs(tail[0] - tailEdge) - Math.abs(head[0] - headEdge)
    const t = span === 0 ? 0.5 : Math.abs(tail[0] - tailEdge) / span
    const seamLat = tail[1] + t * (head[1] - tail[1])
    return [densifyEdges([...r, [tailEdge, seamLat], [tailEdge, poleLat], [headEdge, poleLat], [headEdge, seamLat]])]
  }

  // Plain crosser (Russia, Fiji, …): unwrap to a continuous lon range, then
  // band-clip a world-shifted copy for each side of the seam.
  const un = [pts[0]]
  for (let i = 1; i < pts.length; i++) {
    const prev = un[i - 1]
    un.push([prev[0] + normLon(pts[i][0] - pts[i - 1][0]), pts[i][1]])
  }
  const pieces = []
  for (const off of [-360, 0, 360]) {
    const clipped = clipBand(un.map((p) => [p[0] + off, p[1]]))
    if (clipped) pieces.push(densifyEdges(clipped))
  }
  return pieces
}

// ---- Simplification (projected space) ----

function perpDist(p, a, b) {
  const bx = b[0] - a[0]
  const by = b[1] - a[1]
  const len2 = bx * bx + by * by
  if (len2 === 0) return Math.hypot(p[0] - a[0], p[1] - a[1])
  const t = Math.max(0, Math.min(1, ((p[0] - a[0]) * bx + (p[1] - a[1]) * by) / len2))
  return Math.hypot(p[0] - (a[0] + t * bx), p[1] - (a[1] + t * by))
}

function douglasPeucker(points, eps) {
  if (points.length < 3) return points
  let maxDist = 0
  let index = 0
  const last = points.length - 1
  for (let i = 1; i < last; i++) {
    const d = perpDist(points[i], points[0], points[last])
    if (d > maxDist) {
      maxDist = d
      index = i
    }
  }
  if (maxDist <= eps) return [points[0], points[last]]
  const left = douglasPeucker(points.slice(0, index + 1), eps)
  const right = douglasPeucker(points.slice(index), eps)
  return left.slice(0, -1).concat(right)
}

function ringArea(points) {
  let area = 0
  for (let i = 0; i < points.length; i++) {
    const [x1, y1] = points[i]
    const [x2, y2] = points[(i + 1) % points.length]
    area += x1 * y2 - x2 * y1
  }
  return Math.abs(area / 2)
}

// ---- Land rings (projected + simplified, for point-in-polygon tests) ----

const rings = [] // { pts: [[x,y],…], minX, maxX, minY, maxY }
let ringCount = 0
for (const geom of topo.objects.countries.geometries) {
  if (!geom) continue
  const polys = geom.type === 'Polygon' ? [geom.arcs] : geom.type === 'MultiPolygon' ? geom.arcs : []
  for (const poly of polys) {
    for (const ring of poly) {
      for (const piece of cutAntimeridian(ringLonLat(ring))) {
        let pts = piece.map(([lon, lat]) => project(lon, lat))
        pts = douglasPeucker(pts, EPSILON)
        if (pts.length < 4 || ringArea(pts) < MIN_RING_AREA) continue
        let rMinX = Infinity
        let rMaxX = -Infinity
        let rMinY = Infinity
        let rMaxY = -Infinity
        for (const [x, y] of pts) {
          if (x < rMinX) rMinX = x
          if (x > rMaxX) rMaxX = x
          if (y < rMinY) rMinY = y
          if (y > rMaxY) rMaxY = y
        }
        rings.push({ pts, minX: rMinX, maxX: rMaxX, minY: rMinY, maxY: rMaxY })
        ringCount++
      }
    }
  }
}

// ---- Dot grid emit ----
//
// Even-odd crossing count over ALL rings at once: countries tile the land
// without overlapping, so a land point crosses exactly one outer ring an odd
// number of times, and interior lake rings add an even count that correctly
// excludes their dots. Bounding boxes keep the test cheap.

function crossings(x, y, pts) {
  let hits = 0
  for (let i = 0, j = pts.length - 1; i < pts.length; j = i++) {
    const [xi, yi] = pts[i]
    const [xj, yj] = pts[j]
    if (yi > y !== yj > y && x < ((xj - xi) * (y - yi)) / (yj - yi) + xi) hits++
  }
  return hits
}

function onLand(x, y) {
  let total = 0
  for (const r of rings) {
    if (x < r.minX || x > r.maxX || y < r.minY || y > r.maxY) continue
    total += crossings(x, y, r.pts)
  }
  return total % 2 === 1
}

const fmt = (n) => {
  const s = n.toFixed(1)
  return s.endsWith('.0') ? s.slice(0, -2) : s
}

const dots = []
let row = 0
for (let y = EXTENT[0][1]; y <= EXTENT[1][1]; y += DOT_STEP, row++) {
  const offset = row % 2 === 1 ? DOT_STEP / 2 : 0
  for (let x = EXTENT[0][0] + offset; x <= EXTENT[1][0]; x += DOT_STEP) {
    if (onLand(x, y)) dots.push(fmt(x), fmt(y))
  }
}

process.stdout.write(`// Generated by web/tools/build-map-geo.mjs — do not hand-edit.
// Source: world-atlas countries-110m.json (Natural Earth 1:110m, public
// domain), Natural Earth I projection fit to [[14,24],[1066,576]] in a
// ${VIEW_W}×${VIEW_H} frame. Landmass sampled as a ${DOT_STEP}px hex-offset dot grid
// (${dots.length / 2} dots over ${ringCount} land rings).
// MAP_K/MAP_TX/MAP_TY are the solved fitExtent constants — web/src/geo.ts
// applies them to the same raw projection for site positions.
export const MAP_VIEW_W = ${VIEW_W}
export const MAP_VIEW_H = ${VIEW_H}
export const MAP_K = ${K}
export const MAP_TX = ${TX}
export const MAP_TY = ${TY}
// Flat [x0,y0, x1,y1, …] pairs in viewBox coordinates.
export const MAP_DOTS: number[] = [
${wrapDots(dots)}
]
`)

function wrapDots(list) {
  const lines = []
  for (let i = 0; i < list.length; i += 20) {
    lines.push('  ' + list.slice(i, i + 20).join(', ') + ',')
  }
  return lines.join('\n')
}

console.error(`map geo: ${ringCount} rings, ${dots.length / 2} dots at ${DOT_STEP}px pitch`)
