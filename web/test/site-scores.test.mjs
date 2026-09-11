// Month-to-date site scores on the map card: pure-module unit tests for the
// ratio/tone arithmetic, a controller test for the plane-switch race, and
// source pins for the wiring (Overview fetch → ConnectivityCard → WorldMap
// card rows) plus the stylesheet the rows depend on.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { startPolledResource } from '../src/polledResource.ts'
import {
  SITE_SCORES_POLL_MS,
  fmtPercent,
  fmtPercentFixed,
  indexSiteScores,
  scoreTone,
  siteScoreRatios,
  siteScoresPath,
} from '../src/siteScores.ts'
import { readStyles } from './util/styles.mjs'

const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')

test('site scores poll far slower than the dashboard cadence', () => {
  assert.equal(SITE_SCORES_POLL_MS, 5 * 60_000)
})

test('the scores path narrows by plane and encodes the name', () => {
  assert.equal(siteScoresPath(''), '/api/v1/sites/scores')
  assert.equal(siteScoresPath('corp'), '/api/v1/sites/scores?network=corp')
  assert.equal(siteScoresPath('a b/c'), '/api/v1/sites/scores?network=a%20b%2Fc')
})

test('ratios come from the counts and a zero denominator is null, never 100 %', () => {
  assert.deepEqual(siteScoreRatios(undefined), { availability: null, performance: null })
  assert.deepEqual(siteScoreRatios({ name: 'x', samples: 0, ok_samples: 0, healthy_ok_samples: 0 }), {
    availability: null,
    performance: null,
  })
  // Every sample failed: availability is a real 0, performance has nothing to grade.
  assert.deepEqual(siteScoreRatios({ name: 'x', samples: 40, ok_samples: 0, healthy_ok_samples: 0 }), {
    availability: 0,
    performance: null,
  })
  const r = siteScoreRatios({ name: 'x', samples: 1000, ok_samples: 990, healthy_ok_samples: 900 })
  assert.equal(r.availability, 0.99)
  assert.ok(Math.abs(r.performance - 900 / 990) < 1e-12)
})

test('tone bands match the incident-free tile: 100 good, ≥99 warning, else critical, null none', () => {
  assert.equal(scoreTone(null), '')
  assert.equal(scoreTone(1), ' stat-good')
  assert.equal(scoreTone(0.999), ' stat-warning')
  assert.equal(scoreTone(0.99), ' stat-warning')
  assert.equal(scoreTone(0.9899), ' stat-critical')
  assert.equal(scoreTone(0), ' stat-critical')
})

test('fmtPercent trims trailing zeros and caps at 100', () => {
  assert.equal(fmtPercent(1), '100')
  assert.equal(fmtPercent(1.5), '100')
  assert.equal(fmtPercent(0.999), '99.9')
  assert.equal(fmtPercent(0.9725), '97.25')
  assert.equal(fmtPercent(0), '0')
})

test('the card keeps two decimals so its two rows read as a matched pair', () => {
  assert.equal(fmtPercentFixed(0.999), '99.90')
  assert.equal(fmtPercentFixed(0.9743), '97.43')
  assert.equal(fmtPercentFixed(1), '100.00')
  assert.equal(fmtPercentFixed(1.5), '100.00')
  assert.equal(fmtPercentFixed(0), '0.00')
})

test('indexing tolerates a missing response', () => {
  assert.equal(indexSiteScores(null).size, 0)
  const index = indexSiteScores({
    month: '2026-09',
    since: '',
    as_of: '',
    network: '',
    sites: [{ name: 'lon', samples: 1, ok_samples: 1, healthy_ok_samples: 1 }],
  })
  assert.equal(index.get('lon')?.samples, 1)
})

// The plane-switch race, at the controller level: a key change stops the
// old controller and starts a new one, and the superseded plane's response
// must never commit after the switch — whether it arrives late or fails.
const makeTimers = () => ({
  setInterval: () => ({}),
  clearInterval: () => {},
})
const deferredFetcher = () => {
  const calls = []
  const fetcher = () => new Promise((resolve, reject) => calls.push({ resolve, reject }))
  return { fetcher, calls }
}
const settle = () => new Promise((resolve) => setTimeout(resolve, 0))

test('a superseded plane response never commits after the key switches', async () => {
  const events = []
  const record = (plane) => ({
    onData: (data) => events.push(['data', plane, data]),
    onError: (err) => events.push(['error', plane, String(err)]),
  })
  const a = deferredFetcher()
  const first = startPolledResource(a.fetcher, SITE_SCORES_POLL_MS, record('a'), makeTimers())
  // The filter moves to plane b before a's response lands.
  first.stop()
  const b = deferredFetcher()
  startPolledResource(b.fetcher, SITE_SCORES_POLL_MS, record('b'), makeTimers())
  a.calls[0].resolve({ month: '2026-09', network: 'a', sites: [] })
  await settle()
  assert.deepEqual(events, [], 'the delayed plane-a response must be dropped')
  b.calls[0].reject(new Error('boom'))
  await settle()
  assert.deepEqual(events, [['error', 'b', 'Error: boom']], 'the failure surfaces as an error with no data')
})

test('overview polls the scores separately, resets on plane change, and gates on the loaded key', () => {
  const overview = read('../src/views/Overview.tsx')
  assert.match(overview, /apiGet<SiteScoresResponse>\(scoresPath\)/)
  assert.match(overview, /const scoresPath = siteScoresPath\(netFilter\)/)
  assert.match(overview, /pollMs: SITE_SCORES_POLL_MS/)
  assert.match(overview, /key: scoresPath/)
  assert.match(overview, /resetOnChange: true/)
  assert.match(overview, /const scores = scoresLoadedKey === scoresPath \? scoresData : null/)
  // The Refresh button reloads both feeds.
  assert.match(overview, /void reload\(\)\s*void reloadScores\(\)/)
  assert.match(overview, /scores=\{scores\}/)
  assert.match(overview, /scoresError=\{scoresError\}/)
  // The 30 s Promise.all stays exactly five requests — scores never join it.
  assert.doesNotMatch(overview, /Promise\.all\(\[[^\]]*sites\/scores/s)
})

test('the connectivity card forwards scores to the map only', () => {
  const card = read('../src/components/ConnectivityCard.tsx')
  assert.match(
    card,
    /<WorldMap topology=\{topology\} links=\{links\} scores=\{scores\} scoresError=\{scoresError\} \/>/,
  )
  assert.doesNotMatch(card, /<TopologySites[^>]*scores/)
  assert.doesNotMatch(card, /<MatrixTable[^>]*scores/)
})

test('the map card renders both scores with the shared tone and an honest dash', () => {
  const map = read('../src/components/WorldMap.tsx')
  assert.match(map, /indexSiteScores\(scores\)/)
  assert.match(map, /siteScoreRatios\(shownScoreRow\)/)
  assert.match(map, /\['Availability', shownScore\.availability\]/)
  assert.match(map, /\['Performance', shownScore\.performance\]/)
  assert.match(map, /className=\{'map-tip-score' \+ scoreTone\(ratio\)\}/)
  assert.match(map, /ratio == null \? \(\s*'—'/)
  assert.match(map, /fmtPercentFixed\(ratio\)/)
  assert.doesNotMatch(map, /[^d]fmtPercent\(/)
  assert.match(map, /'Loading month-to-date scores…'/)
  assert.match(map, /'Month-to-date scores unavailable'/)
  assert.match(map, /`No samples this month · \$\{scores\.month\}`/)
  assert.match(map, /`Month to date · \$\{scores\.month\}`/)
  // A refresh failure after a successful load is flagged on the card,
  // never hidden behind the retained snapshot.
  assert.match(map, /\+ \(scoresError \? ' · refresh failed, last snapshot' : ''\)/)
  // The score rows sit inside the live-region card, above the direction bar.
  const rows = map.indexOf('className="map-tip-scores"')
  const bar = map.indexOf('className="map-tip-bar"')
  const value = map.indexOf('className="map-tip-value"')
  assert.ok(value < rows && rows < bar, 'score rows render between the headline value and the bar')
})

test('the site dashboard tile shares the score tone and percent formatting', () => {
  const site = read('../src/views/SiteDetail.tsx')
  assert.match(site, /import \{ fmtPercent, scoreTone \} from '\.\.\/siteScores'/)
  assert.match(site, /className=\{'stat-card' \+ scoreTone\(freeRatio\)\}/)
  assert.doesNotMatch(site, /function fmtPercent/)
})

test('the score rows are styled with existing tokens only', () => {
  const css = readStyles()
  assert.match(css, /\.map-tip-scores \{[^}]*grid-template-columns: 1fr auto/)
  assert.match(css, /\.map-tip-score \{\s*display: contents;/)
  assert.match(css, /\.map-tip-score > strong \{[^}]*tabular-nums/)
  // Tone colors come from the stat tiles' rules, which target the same
  // class > strong shape — no new color declared for the card.
  assert.match(css, /\.stat-warning > strong \{\s*color: var\(--status-degraded\)/)
  assert.match(css, /\.stat-critical > strong \{\s*color: var\(--status-down\)/)
  const block = css.slice(css.indexOf('.map-tip-scores {'), css.indexOf('.map-tip-bar {'))
  assert.doesNotMatch(block, /#[0-9a-f]{3,8}\b|rgb\(|hsl\(|oklch\(/i)
})
