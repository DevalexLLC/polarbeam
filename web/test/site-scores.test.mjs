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
  siteScoreContext,
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

test("the site tiles' context line names the month in every loaded state", () => {
  const month = '2026-09'
  // The month's hyphen renders as U+2011 (non-breaking) so a wrapped context
  // line never splits "2026-09"; the expectations spell it out to pin that.
  const shown = '2026\u201109'
  const shownPrev = '2026\u201108'
  // Nothing loaded yet: the same two captions the map card uses.
  assert.equal(siteScoreContext('availability', undefined, null, false), 'Loading month-to-date scores…')
  assert.equal(siteScoreContext('performance', undefined, null, true), 'Scores unavailable')
  // No row, or a row with no samples: both tiles say so, with the month.
  assert.equal(siteScoreContext('availability', undefined, month, false), `No samples · ${shown}`)
  const empty = { name: 'x', samples: 0, ok_samples: 0, healthy_ok_samples: 0 }
  assert.equal(siteScoreContext('performance', empty, month, false), `No samples · ${shown}`)
  // Samples but no OK run: availability is a real 0 with its normal line,
  // performance has nothing to grade and must not claim "no samples".
  const allFailed = { name: 'x', samples: 40, ok_samples: 0, healthy_ok_samples: 0 }
  assert.equal(siteScoreContext('availability', allFailed, month, false), `OK probe runs · ${shown}`)
  assert.equal(siteScoreContext('performance', allFailed, month, false), `No OK runs · ${shown}`)
  const row = { name: 'x', samples: 40, ok_samples: 39, healthy_ok_samples: 30 }
  assert.equal(siteScoreContext('availability', row, month, false), `OK probe runs · ${shown}`)
  assert.equal(siteScoreContext('performance', row, month, false), `OK runs in healthy hours · ${shown}`)
  // A retained snapshot after a failed refresh is flagged and still dated,
  // so last month's numbers can never read as this month's.
  assert.equal(
    siteScoreContext('performance', row, '2026-08', true),
    `OK runs in healthy hours · ${shownPrev} · last snapshot`,
  )
  assert.equal(
    siteScoreContext('availability', allFailed, '2026-08', true),
    `OK probe runs · ${shownPrev} · last snapshot`,
  )
  assert.equal(siteScoreContext('performance', allFailed, '2026-08', true), `No OK runs · ${shownPrev} · last snapshot`)
  assert.equal(
    siteScoreContext('availability', undefined, '2026-08', true),
    `No samples · ${shownPrev} · last snapshot`,
  )
  // The source pins the replacement so an ordinary hyphen cannot creep back.
  assert.match(read('../src/siteScores.ts'), /month = month\.replaceAll\('-', '\\u2011'\)/)
})

test('the site dashboard tile shares the score tone and percent formatting', () => {
  const site = read('../src/views/SiteDetail.tsx')
  assert.match(
    site.replaceAll(/\s+/g, ' '),
    /import \{ SITE_SCORES_POLL_MS, fmtPercent, scoreTone, siteScoreContext, siteScoreRatios, siteScoresPath, \} from '\.\.\/siteScores'/,
  )
  assert.match(site, /className=\{'stat-card' \+ scoreTone\(freeRatio\)\}/)
  assert.doesNotMatch(site, /function fmtPercent/)
})

test('the site dashboard polls the scores on their own cadence, narrowed by plane', () => {
  const site = read('../src/views/SiteDetail.tsx')
  assert.match(site, /const scoresPath = siteScoresPath\(netFilter\)/)
  assert.match(site, /usePolledResource\(\(\) => apiGet<SiteScoresResponse>\(scoresPath\), \{/)
  assert.match(site, /pollMs: SITE_SCORES_POLL_MS,\s*key: scoresPath,\s*resetOnChange: true,/)
  assert.match(site, /const scores = scoresLoadedKey === scoresPath \? scoresData : null/)
  assert.match(site, /const scoreRow = scores\?\.sites\.find\(\(s\) => s\.name === name\)/)
  assert.match(site, /const \{ availability, performance \} = siteScoreRatios\(scoreRow\)/)
  // Refresh reloads both feeds.
  assert.match(site, /void reload\(\)\s*void reloadScores\(\)/)
  // Two tone-classed tiles, trimmed percent like the incident-free tile
  // (never the card's fixed two decimals), an honest dash when null, and
  // the shared context helper for every state.
  assert.match(site, /className="stat-grid stat-grid-six"/)
  assert.match(site, /className=\{'stat-card' \+ scoreTone\(availability\)\} onClick=\{showScoreHelp\}/)
  assert.match(site, /className=\{'stat-card' \+ scoreTone\(performance\)\} onClick=\{showScoreHelp\}/)
  assert.match(site, /\{availability == null \? \(\s*'—'/)
  assert.match(site, /\{performance == null \? \(\s*'—'/)
  assert.match(site, /fmtPercent\(availability\)/)
  assert.match(site, /fmtPercent\(performance\)/)
  assert.doesNotMatch(site, /fmtPercentFixed/)
  assert.match(site, /siteScoreContext\('availability', scoreRow, scoreMonth, scoresStale\)/)
  assert.match(site, /siteScoreContext\('performance', scoreRow, scoreMonth, scoresStale\)/)
  assert.match(site, /const scoreMonth = scores\?\.month \?\? null/)
  assert.match(site, /const scoresStale = scoresError !== null/)
  // The tiles sit between the incident-group tile and the incident-free tile.
  const groups = site.indexOf('Active incident groups')
  const avail = site.indexOf('>Availability<')
  const perf = site.indexOf('>Performance<')
  const free = site.indexOf('Incident-free time</span>')
  assert.ok(
    groups < avail && avail < perf && perf < free,
    'score tiles render between incident groups and incident-free time',
  )
})

test('the site dashboard defines all three percentages in a disclosure', () => {
  const site = read('../src/views/SiteDetail.tsx')
  assert.match(site, /const SCORE_HELP_ID = 'site-score-help'/)
  assert.match(site, /if \(help instanceof HTMLDetailsElement\) help\.open = true/)
  assert.match(site, /<details id=\{SCORE_HELP_ID\} className="stat-help">/)
  assert.match(site, /<summary>How these figures are measured<\/summary>/)
  assert.match(site, /<dt>Availability<\/dt>/)
  assert.match(site, /<dt>Performance<\/dt>/)
  assert.match(site, /<dt>Incident-free time<\/dt>/)
  // The copy states the SQL semantics: OK share of runs, then the healthy-hour
  // share of the OK runs, then the wall-clock window figure. oxfmt reflows
  // JSX prose, so the pins read a whitespace-collapsed copy.
  const prose = site.replaceAll(/\s+/g, ' ')
  assert.match(prose, /Share of probe runs this UTC calendar month that returned OK/)
  assert.match(prose, /packet loss inside an OK run does not lower it/)
  assert.match(prose, /Share of those OK runs that fell in hours graded healthy/)
  assert.match(prose, /failed runs count as loss/)
  assert.match(prose, /pair, then network, then global/)
  assert.match(prose, /Share of the selected window \(\{snapshotWin\}\) during which no incident was open/)
  assert.match(prose, /counts wall-clock time, not probe runs/)
  // Tone bands as the stylesheet actually renders them: healthy stays in
  // ink (stat-good has no color rule), warn is amber, critical is red.
  assert.match(prose, /A figure at 100 % stays in plain ink; 99 % and above turns amber; below 99 % turns red/)
  assert.match(prose, /a dash for performance, because there are no successful runs to grade/)
  // No heading inside the disclosure: the page's heading order is untouched.
  const block = site.slice(site.indexOf('<details id={SCORE_HELP_ID}'), site.indexOf('</details>'))
  assert.doesNotMatch(block, /<h[1-6]/)
})

test('the six-tile strip wraps its context lines and steps 2 → 3 → 6 columns', () => {
  const css = readStyles()
  assert.match(css, /\.stat-grid-six \{\s*grid-template-columns: repeat\(3, minmax\(0, 1fr\)\);/)
  assert.match(
    css,
    /@media \(min-width: 1300px\) \{\s*\.stat-grid-six \{\s*grid-template-columns: repeat\(6, minmax\(0, 1fr\)\);/,
  )
  assert.match(css, /\.stat-grid-six \.stat-context \{[^}]*white-space: normal;/)
  // The mobile block restates the two-column rhythm for the modifier.
  const mobile = css.slice(css.indexOf('@media (max-width: 1100px)'))
  assert.match(mobile, /\.stat-grid-six \{\s*grid-template-columns: repeat\(2, minmax\(0, 1fr\)\);/)
  // The base four-column strip (the Overview) is untouched.
  assert.match(css, /\.stat-grid \{[^}]*grid-template-columns: repeat\(4, minmax\(0, 1fr\)\);/)
  const help = css.slice(css.indexOf('.stat-help {'), css.indexOf('.stat-help p {'))
  assert.doesNotMatch(help, /#[0-9a-f]{3,8}\b|rgb\(|hsl\(|oklch\(/i)
  assert.match(help, /font-size: 0\.8rem/)
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
