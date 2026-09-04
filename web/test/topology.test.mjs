import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import {
  fitMapViewport,
  FULL_MAP_VIEWPORT,
  mapHitRadius,
  mapZoomPercent,
  panMapViewport,
  pinchMapViewport,
  revealMapPoint,
  zoomMapViewport,
  zoomMapViewportAt,
} from '../src/mapViewport.ts'
import { declutter } from '../src/mapLayout.ts'
import { BEAM_GAP, beamGeometry, buildSiteLinks } from '../src/mapLinks.ts'
import { SEVERITY_LABEL } from '../src/severity.ts'
import { buildSiteTopology, rankSiteTopology, topologyUrgentSites } from '../src/siteTopology.ts'
import { resolveTopologyMode } from '../src/topologyMode.ts'
import { readStyles } from './util/styles.mjs'

const topologyEntry = (name, displayName, severity) => ({
  site: { name, display_name: displayName, location: '', latitude: null, longitude: null },
  severity,
  stats: {},
})

const topologyCell = (dst, status) => ({
  src: 'source',
  dst,
  status,
  latency_us: null,
  latency_source: '',
  loss_pct: null,
  as_of: '',
  probes: [],
  networks: [],
})

test('an omitted topology mode is responsive while explicit modes are stable', () => {
  assert.equal(resolveTopologyMode('', true), 'sites')
  assert.equal(resolveTopologyMode('', false), 'map')
  for (const mode of ['sites', 'map', 'matrix']) {
    assert.equal(resolveTopologyMode(mode, true), mode)
    assert.equal(resolveTopologyMode(mode, false), mode)
  }
})

test('site topology ranks urgent, degraded, no-data, then healthy with display-name ties', () => {
  const ranked = rankSiteTopology([
    topologyEntry('healthy', 'Bravo', 'ok'),
    topologyEntry('stale', 'Able', 'stale'),
    topologyEntry('degraded-z', 'Zulu', 'crit'),
    topologyEntry('urgent', 'Charlie', 'down'),
    topologyEntry('degraded-a', 'Alpha', 'warn'),
  ])
  assert.deepEqual(
    ranked.map(({ site }) => site.name),
    ['urgent', 'degraded-a', 'degraded-z', 'stale', 'healthy'],
  )
})

test('a degraded direction outranks missing data and an active incident outranks both', () => {
  const sites = ['source', 'degraded-peer', 'silent-peer'].map((name) => ({
    name,
    display_name: name,
    location: '',
    latitude: null,
    longitude: null,
  }))
  const topology = buildSiteTopology(
    sites,
    [topologyCell('degraded-peer', 'degraded'), topologyCell('silent-peer', 'stale')],
    () => null,
  )
  assert.equal(topology.find(({ site }) => site.name === 'source').severity, 'warn')

  const urgent = buildSiteTopology(
    sites,
    [topologyCell('degraded-peer', 'degraded'), topologyCell('silent-peer', 'stale')],
    () => null,
    new Set(['source']),
  )
  assert.equal(urgent.find(({ site }) => site.name === 'source').severity, 'down')
})

const meshSites = (...names) =>
  names.map((name) => ({ name, display_name: name, location: '', latitude: null, longitude: null }))

const direction = (src, dst, status) => ({
  src,
  dst,
  status,
  latency_us: null,
  latency_source: '',
  loss_pct: null,
  as_of: '',
  probes: [],
  networks: [],
})

const severityOf = (topology, name) => topology.find(({ site }) => site.name === name).severity

test('an unhealthy direction is charged to the end where unhealthy directions concentrate', () => {
  const sites = meshSites('colorado', 'a', 'b', 'c')
  const healthyBack = ['a', 'b', 'c'].map((peer) => direction(peer, 'colorado', 'ok'))

  // Colorado's egress degrades: only Colorado goes amber, not every peer.
  const egress = buildSiteTopology(
    sites,
    [...['a', 'b', 'c'].map((peer) => direction('colorado', peer, 'degraded')), ...healthyBack],
    () => null,
  )
  assert.equal(severityOf(egress, 'colorado'), 'warn')
  for (const peer of ['a', 'b', 'c']) assert.equal(severityOf(egress, peer), 'ok')

  // The mirror: Colorado's ingress fails from everywhere. Same answer, not the inverse.
  const ingress = buildSiteTopology(
    sites,
    [
      ...['a', 'b', 'c'].map((peer) => direction(peer, 'colorado', 'down')),
      ...['a', 'b', 'c'].map((peer) => direction('colorado', peer, 'ok')),
    ],
    () => null,
  )
  assert.equal(severityOf(ingress, 'colorado'), 'down')
  for (const peer of ['a', 'b', 'c']) assert.equal(severityOf(ingress, peer), 'ok')

  // The uncharged end still contributes to the per-site direction stats.
  const a = egress.find(({ site }) => site.name === 'a').stats
  assert.equal(a.directions, 2)
  assert.deepEqual(a.dirCounts, { ok: 1, warn: 1, crit: 0, down: 0, stale: 0 })
})

test('ambiguous evidence still colors both ends of a bad direction', () => {
  const sites = meshSites('x', 'y', 'z')
  const healthyRest = [
    direction('x', 'z', 'ok'),
    direction('z', 'x', 'ok'),
    direction('y', 'z', 'ok'),
    direction('z', 'y', 'ok'),
  ]

  // One path bad in both directions: two each, tie, both amber.
  const path = buildSiteTopology(
    sites,
    [direction('x', 'y', 'degraded'), direction('y', 'x', 'degraded'), ...healthyRest],
    () => null,
  )
  assert.equal(severityOf(path, 'x'), 'warn')
  assert.equal(severityOf(path, 'y'), 'warn')
  assert.equal(severityOf(path, 'z'), 'ok')

  // A lone bad direction: one each, tie, both amber.
  const lone = buildSiteTopology(
    sites,
    [direction('x', 'y', 'degraded'), direction('y', 'x', 'ok'), ...healthyRest],
    () => null,
  )
  assert.equal(severityOf(lone, 'x'), 'warn')
  assert.equal(severityOf(lone, 'y'), 'warn')
  assert.equal(severityOf(lone, 'z'), 'ok')
})

test('stale directions concentrate among themselves and never weigh a fault', () => {
  const sites = meshSites('colorado', 'a', 'x', 'y')
  const topology = buildSiteTopology(
    sites,
    [
      // One bad direction between a and Colorado: a tie on fault evidence.
      direction('a', 'colorado', 'degraded'),
      direction('colorado', 'a', 'ok'),
      // a has no data with x or y. That must not tip the fault onto a.
      direction('a', 'x', 'stale'),
      direction('x', 'a', 'stale'),
      direction('a', 'y', 'stale'),
      direction('y', 'a', 'stale'),
    ],
    () => null,
  )
  assert.equal(severityOf(topology, 'colorado'), 'warn')
  assert.equal(severityOf(topology, 'a'), 'warn')
  // The stale fan concentrates on a, and a stale direction clears nothing:
  // x and y are left without data rather than marked healthy.
  assert.equal(severityOf(topology, 'x'), 'stale')
  assert.equal(severityOf(topology, 'y'), 'stale')

  // A quiet agent: its outbound goes stale everywhere, its peers stay healthy.
  const quiet = buildSiteTopology(
    meshSites('colorado', 'a', 'x'),
    [
      direction('colorado', 'a', 'stale'),
      direction('colorado', 'x', 'stale'),
      direction('a', 'colorado', 'ok'),
      direction('x', 'colorado', 'ok'),
      direction('a', 'x', 'ok'),
      direction('x', 'a', 'ok'),
    ],
    () => null,
  )
  assert.equal(severityOf(quiet, 'colorado'), 'stale')
  assert.equal(severityOf(quiet, 'a'), 'ok')
  assert.equal(severityOf(quiet, 'x'), 'ok')
})

test('only offline state and agent-offline incidents make topology urgent', () => {
  const urgent = topologyUrgentSites(
    ['offline-site'],
    [
      { kind: 'agent_offline', src_site: 'agent-site', dst_site: null },
      { kind: 'probe_failing', src_site: 'pair-source', dst_site: 'pair-destination' },
      { kind: 'probe_failing', src_site: 'external-source', dst_site: null },
      { kind: 'probe_degraded', src_site: 'threshold-source', dst_site: 'threshold-destination' },
    ],
  )
  assert.deepEqual(urgent, new Set(['offline-site', 'agent-site']))
})

test('fit-to-sites handles missing, single, coincident, and worldwide coordinates', () => {
  assert.deepEqual(fitMapViewport([null]), FULL_MAP_VIEWPORT)

  const single = fitMapViewport([{ x: 540, y: 300 }])
  assert.ok(single.width < FULL_MAP_VIEWPORT.width)
  assert.ok(single.x <= 540 && single.x + single.width >= 540)
  assert.ok(single.y <= 300 && single.y + single.height >= 300)
  assert.deepEqual(
    fitMapViewport([
      { x: 540, y: 300 },
      { x: 540, y: 300 },
    ]),
    single,
  )

  assert.deepEqual(
    fitMapViewport([
      { x: 0, y: 0 },
      { x: 1080, y: 600 },
    ]),
    FULL_MAP_VIEWPORT,
  )
})

test('zoom and pan stay inside the world frame', () => {
  const zoomed = zoomMapViewport(FULL_MAP_VIEWPORT, 0.5)
  assert.equal(mapZoomPercent(zoomed), 200)
  assert.deepEqual(panMapViewport(zoomed, -10_000, -10_000), { ...zoomed, x: 0, y: 0 })
  const bottomRight = panMapViewport(zoomed, 10_000, 10_000)
  assert.equal(bottomRight.x + bottomRight.width, FULL_MAP_VIEWPORT.width)
  assert.equal(bottomRight.y + bottomRight.height, FULL_MAP_VIEWPORT.height)

  const focused = revealMapPoint(zoomed, { x: 1000, y: 550 })
  assert.ok(focused.x <= 1000 && focused.x + focused.width >= 1000)
  assert.ok(focused.y <= 550 && focused.y + focused.height >= 550)

  // Pointer-anchored zoom keeps the focused map point at the same relative
  // viewport position, so wheel zoom dives into the spot under the cursor.
  const anchor = { x: 300, y: 150 }
  const anchored = zoomMapViewportAt(FULL_MAP_VIEWPORT, 0.5, anchor)
  assert.equal(mapZoomPercent(anchored), 200)
  const before = (anchor.x - FULL_MAP_VIEWPORT.x) / FULL_MAP_VIEWPORT.width
  const after = (anchor.x - anchored.x) / anchored.width
  assert.ok(Math.abs(before - after) < 0.0001)
  // Anchored zoom-out at a corner still clamps inside the world frame.
  const cornered = zoomMapViewportAt(anchored, 4, { x: 0, y: 0 })
  assert.deepEqual(cornered, FULL_MAP_VIEWPORT)

  // Pinch is one pure combined step: pointer spacing sets the scale while
  // the map point that started under the gesture midpoint follows it.
  const pinched = pinchMapViewport(
    FULL_MAP_VIEWPORT,
    { distance: 100, midX: 540, midY: 300 },
    { distance: 200, midX: 640, midY: 300 },
    { width: 1080, height: 600 },
  )
  assert.equal(mapZoomPercent(pinched), 200)
  const startMapMid = FULL_MAP_VIEWPORT.x + (540 / 1080) * FULL_MAP_VIEWPORT.width
  assert.ok(Math.abs(pinched.x + (640 / 1080) * pinched.width - startMapMid) < 0.0001)
  // Collapsing the pinch far past the world frame clamps like every gesture.
  const spread = pinchMapViewport(
    pinched,
    { distance: 400, midX: 540, midY: 300 },
    { distance: 10, midX: 540, midY: 300 },
    { width: 1080, height: 600 },
  )
  assert.deepEqual(spread, FULL_MAP_VIEWPORT)

  // Crossing the 800% cap clamps the scale BEFORE anchoring, so the map
  // point under a stationary midpoint stays put instead of drifting.
  const inCap = pinchMapViewport(
    anchored,
    { distance: 50, midX: 540, midY: 300 },
    { distance: 400, midX: 540, midY: 300 },
    { width: 1080, height: 600 },
  )
  assert.equal(mapZoomPercent(inCap), 800)
  const capAnchor = anchored.x + (540 / 1080) * anchored.width
  assert.ok(Math.abs(inCap.x + (540 / 1080) * inCap.width - capAnchor) < 0.0001)
})

test('screen-sized pointer targets feed the same radius into decluttering', () => {
  assert.equal(mapHitRadius(24, 540), 24)
  assert.equal(mapHitRadius(44, 360), 66)
  const radius = mapHitRadius(44, 360)
  const placed = declutter(
    [
      { x: 540, y: 300, hitR: radius },
      { x: 540, y: 300, hitR: radius },
    ],
    { w: 1080, h: 600 },
    radius,
  )
  assert.ok(Math.hypot(placed[0].x - placed[1].x, placed[0].y - placed[1].y) >= radius + 8 - 0.001)
})

test('map controls, labels, and pointer targets expose the accessibility contract', async () => {
  const map = await readFile(new URL('../src/components/WorldMap.tsx', import.meta.url), 'utf8')
  const styles = readStyles()
  // Direct manipulation (drag + wheel) must keep keyboard equivalents: the
  // focusable map pans with arrows, zooms with +/-, fits with F, resets
  // with 0 — attached natively (like the wheel listener) through the
  // render-latched handler ref.
  assert.match(map, /addEventListener\('keydown', onKeyDown\)/)
  assert.match(map, /keydown: keyboardViewport/)
  assert.match(map, /case 'ArrowLeft':/)
  assert.match(map, /case 'F':/)
  assert.match(map, /case 'Home':/)
  // Modified combinations (Ctrl/Cmd+F, page zoom, Alt+arrows) stay with the
  // browser even while the map is focused.
  assert.match(map, /event\.ctrlKey \|\| event\.metaKey \|\| event\.altKey/)
  // Wheel zoom must be a native non-passive listener (React's own wheel
  // registration is passive and cannot preventDefault page scrolling), it
  // must anchor on the pointer position, and it must release the event to
  // page scrolling once the viewport is clamped at a zoom limit.
  assert.match(map, /addEventListener\('wheel', onWheel, \{ passive: false \}\)/)
  assert.match(map, /zoomMapViewportAt/)
  assert.match(map, /next\.width === current\.width && next\.x === current\.x && next\.y === current\.y/)
  // Touch fires no wheel events: two-pointer pinch is the touch zoom path
  // (via the pure viewport helper), and a fleet swap (network-scope change)
  // re-arms the automatic fit.
  assert.match(map, /pinch\.current = \{/)
  assert.match(map, /pinchMapViewport\(/)
  assert.match(map, /\[fleetKey\]/)
  assert.match(map, /suppressBackgroundClick\.current = false/)
  assert.match(map, /revealMapPoint/)
  assert.match(map, /return large \? 44 : 24/)
  assert.match(map, /Math\.max\(targetRadius, bubbleRadius/)
  // Site markers are HTML buttons layered over the decorative svg, sized to
  // the pointer target, clipped by a pointer-inert layer so drags between
  // markers still reach the pan surface; the visible key hint names the
  // bindings for sighted keyboard users.
  assert.match(map, /className=\{`map-marker sev-\$\{sev\}`/)
  assert.match(map, /Math\.max\(targetPixels, Math\.ceil\(bubblePx\) \+ 8\)/)
  assert.match(map, /aria-describedby=\{hintId\}/)
  assert.match(styles, /\.map-markers \{[^}]*pointer-events: none/)
  assert.match(styles, /\.map-marker \{[^}]*pointer-events: auto/)
  assert.match(styles, /touch-action: pan-y/)
})

test('all topology modes share one health vocabulary and the unscoped empty state', async () => {
  const sites = await readFile(new URL('../src/components/TopologySites.tsx', import.meta.url), 'utf8')
  assert.equal(SEVERITY_LABEL.stale, 'No data')
  assert.equal(SEVERITY_LABEL.down, 'Down')
  assert.match(sites, /No sites are enrolled\./)
  assert.doesNotMatch(sites, /enrolled in this network/)
})

test('PathGraph label size stays in the same fixed coordinate system as its width estimate', async () => {
  const graph = await readFile(new URL('../src/components/PathGraph.tsx', import.meta.url), 'utf8')
  const styles = readStyles()
  assert.match(graph, /const CHAR_W = 7\.3/)
  assert.match(styles, /\.path-graph \.pg-label[\s\S]*font-size: 12px/)
  assert.match(styles, /\.path-graph \.pg-sub[\s\S]*font-size: 12px/)
})

test('site links fold both directions of a pair and grade each on its own', () => {
  const cell = (src, dst, status) => ({ ...topologyCell(dst, status), src })
  const links = buildSiteLinks(
    [cell('lon', 'nyc', 'ok'), cell('nyc', 'lon', 'degraded'), cell('syd', 'lon', 'stale'), cell('nyc', 'nyc', 'ok')],
    () => null,
  )
  // Canonical a < b keys, name-ordered regardless of response order; a
  // self-pair never becomes a beam.
  assert.deepEqual(
    links.map(({ a, b, ab, ba }) => [a, b, ab, ba]),
    [
      ['lon', 'nyc', 'ok', 'warn'],
      ['lon', 'syd', null, 'stale'],
    ],
  )
})

test('beam geometry parts the two directions and stops at each bubble', () => {
  const from = { x: 0, y: 0, r: 10 }
  const to = { x: 100, y: 0, r: 20 }
  const ab = beamGeometry(from, to, 1)
  const ba = beamGeometry(from, to, -1)
  // Each stroke clears its origin and destination bubbles.
  assert.ok(ab.x1 > from.r && ab.x2 < 100 - to.r)
  // The strokes sit on opposite sides of the center line, one gap apart.
  assert.ok(Math.abs(Math.abs(ab.y1 - ba.y1) - 2 * BEAM_GAP) < 1e-9)
  assert.ok(ab.y1 * ba.y1 < 0)
  // b→a runs toward a, so its arrowhead end is the low-x end.
  assert.ok(ba.x2 < ba.x1)
  // Touching bubbles leave no room for a legible beam.
  assert.equal(beamGeometry(from, { x: 32, y: 0, r: 20 }, 1), null)
})
