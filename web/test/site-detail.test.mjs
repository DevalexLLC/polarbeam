import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { groupIncidents, incidentFreeRatio } from '../src/incidentGroups.ts'

const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')
const site = read('../src/views/SiteDetail.tsx')

test('site detail composes the overview feeds with a site-filtered incident history', () => {
  assert.match(site, /apiGet<MatrixResponse>\('\/api\/v1\/matrix'\)/)
  assert.match(site, /apiGet<AgentsResponse>\('\/api\/v1\/agents'\)/)
  assert.match(site, /`\/api\/v1\/outages\?window=\$\{win\}&site=\$\{encodeURIComponent\(name\)\}&include_routes=true`/)
  assert.match(site, /apiGet<AgentHealthResponse>\('\/api\/v1\/agents\/health\?window=24h'\)/)
  assert.match(site, /key: \[name, win\]\.join\('\\u0000'\)/)
  assert.match(site, /buildSiteTopology\(/)
  assert.match(site, /<FleetAgentsCard/)
  assert.match(site, /<IncidentTimeline/)
  assert.match(site, /<IncidentGroupRow/)
  // History figures read the response snapshot's window, never the
  // selector's: the previous response stays on screen while a new window
  // loads.
  assert.match(site, /outages\.window as Window/)
  assert.match(site, /WINDOW_MS\[snapshotWin\]/)
  assert.doesNotMatch(site, /WINDOW_MS\[win\]/)
  // Identity resolves unfiltered; the filtered topology may be absent.
  assert.match(site, /matrix\?\.sites\.find\(\(s\) => s\.name === name\)/)
  assert.match(site, /topology\?\.stats/)
  // The Pair detail column keeps its screen-reader heading (#185 was fixed
  // by containing it, never by removing it).
  assert.match(site, /className="site-peers-link">\s*<span className="sr-only">Pair detail<\/span>/)
})

test('every site surface links into the site page', () => {
  for (const path of [
    '../src/components/TopologySites.tsx',
    '../src/components/WorldMap.tsx',
    '../src/components/MatrixTable.tsx',
    '../src/components/FleetAgentsCard.tsx',
    '../src/components/SitesPanel.tsx',
    '../src/views/PairDetail.tsx',
    '../src/views/Agents.tsx',
  ]) {
    assert.match(read(path), /siteDetailHref\(/, path)
  }
  // The map's site link must not hide behind the peer list.
  const map = read('../src/components/WorldMap.tsx')
  assert.doesNotMatch(map, /peers\.length > 0 && \(\s*<div className="map-tip-links">/)
})

const event = (opened, closed) => ({ id: opened + closed, kind: 'probe_failing', opened_at: opened, closed_at: closed })

test('incident-free time unions overlapping spans and clips to the window', () => {
  const now = Date.parse('2026-09-07T12:00:00Z')
  const day = 24 * 60 * 60 * 1000
  const at = (hoursAgo) => new Date(now - hoursAgo * 60 * 60 * 1000).toISOString()
  assert.equal(incidentFreeRatio([], day, now), 1)
  // Two overlapping 2h spans cover 3h of 24h.
  assert.ok(Math.abs(incidentFreeRatio([event(at(4), at(2)), event(at(3), at(1))], day, now) - 21 / 24) < 1e-9)
  // Still open: clipped at now. Opened before the window: clipped at start.
  assert.ok(Math.abs(incidentFreeRatio([event(at(6), null)], day, now) - 18 / 24) < 1e-9)
  assert.ok(Math.abs(incidentFreeRatio([event(at(30), at(18))], day, now) - 18 / 24) < 1e-9)
  // Fully covered never goes below zero.
  assert.equal(incidentFreeRatio([event(at(48), null)], day, now), 0)
})

const ev = (id, opened, closed, extra = {}) => ({
  id,
  kind: 'probe_failing',
  agent_id: 'a',
  probe_id: null,
  target_id: null,
  agent: 'host',
  network: '',
  src_site: 'lon',
  dst_site: 'nyc',
  target: null,
  probe_type: 'icmp',
  opened_at: opened,
  closed_at: closed,
  error: 'i/o timeout',
  route_events: [],
  ...extra,
})

test('incident groups fold active first, then newest, by kind, probe, and cause', () => {
  const groups = groupIncidents([
    ev('r1', '2026-09-01T00:00:00Z', '2026-09-01T01:00:00Z'),
    ev('a1', '2026-09-02T00:00:00Z', null),
    ev('a2', '2026-09-03T00:00:00Z', null, { error: 'read: deadline exceeded' }),
    ev('r2', '2026-09-04T00:00:00Z', '2026-09-04T01:00:00Z'),
  ])
  assert.deepEqual(
    groups.map((g) => [g.open, g.events.map((e) => e.id)]),
    [
      [true, ['a1', 'a2']],
      [false, ['r1', 'r2']],
    ],
  )
})
