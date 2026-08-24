import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import {
  fitMapViewport,
  FULL_MAP_VIEWPORT,
  layoutMapLabels,
  mapHitRadius,
  mapZoomPercent,
  panMapViewport,
  revealMapPoint,
  zoomMapViewport,
} from '../src/mapViewport.ts'
import { declutter } from '../src/mapLayout.ts'
import { SEVERITY_LABEL } from '../src/severity.ts'
import { buildSiteTopology, rankSiteTopology, topologyUrgentSites } from '../src/siteTopology.ts'
import { resolveTopologyMode } from '../src/topologyMode.ts'

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
})

test('dense map labels use deterministic non-overlapping vertical tracks', () => {
  const labels = layoutMapLabels(
    ['delta', 'alpha', 'charlie', 'bravo'].map((id) => ({ id, x: 300, y: 300 })),
    FULL_MAP_VIEWPORT,
  )
  assert.deepEqual(
    labels.map(({ id }) => id),
    ['alpha', 'bravo', 'charlie', 'delta'],
  )
  for (let index = 1; index < labels.length; index++) {
    assert.ok(labels[index].top - labels[index - 1].top >= 12)
  }
  const edges = layoutMapLabels(
    [
      { id: 'top', x: 300, y: 0 },
      { id: 'bottom', x: 300, y: 600 },
    ],
    FULL_MAP_VIEWPORT,
  )
  assert.ok(edges.every(({ top }) => top >= 4 && top <= 96))
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
  const styles = await readFile(new URL('../src/styles.css', import.meta.url), 'utf8')
  assert.match(map, /aria-label="Map viewport controls"/)
  assert.match(map, /Pan map left/)
  assert.match(map, /Zoom map in/)
  assert.match(map, /Fit sites/)
  assert.match(map, /map-site-label/)
  assert.match(map, /suppressBackgroundClick\.current = false/)
  assert.match(map, /onPointerDownCapture/)
  assert.match(map, /revealMapPoint/)
  assert.match(map, /return large \? 44 : 24/)
  assert.match(map, /Math\.max\(targetRadius, bubbleRadius/)
  assert.match(styles, /\.map-site-hit[\s\S]*pointer-events: all/)
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
  const styles = await readFile(new URL('../src/styles.css', import.meta.url), 'utf8')
  assert.match(graph, /const CHAR_W = 7\.3/)
  assert.match(styles, /\.path-graph \.pg-label[\s\S]*font-size: 12px/)
  assert.match(styles, /\.path-graph \.pg-sub[\s\S]*font-size: 12px/)
})
