// Month-to-date site scores for the map card: the pure half (path, index,
// ratios, tone) so the DOM-free suite can pin the arithmetic while the
// views only wire it up.
import type { SiteScoreRow, SiteScoresResponse } from './types'

// Month-to-date tallies move slowly; polling them at the 30 s dashboard
// cadence would only rescan the month for nothing, and keeping the request
// out of the Overview's Promise.all means the map never waits on it.
export const SITE_SCORES_POLL_MS = 5 * 60_000

// The endpoint narrows server-side by plane: tallies are folded counts, so
// the top-bar filter cannot be applied client-side the way the matrix's
// sub-cells are.
export function siteScoresPath(network: string): string {
  return network === '' ? '/api/v1/sites/scores' : `/api/v1/sites/scores?network=${encodeURIComponent(network)}`
}

export function indexSiteScores(res: SiteScoresResponse | null): Map<string, SiteScoreRow> {
  const index = new Map<string, SiteScoreRow>()
  if (res) for (const row of res.sites) index.set(row.name, row)
  return index
}

// Percent with the trailing zeros trimmed: 100, 99.9, 97.25. Shared by the
// Site dashboard's incident-free tile and the map card's scores so the two
// percentages typeset alike. Lives here rather than format.ts so the
// DOM-free suite can import it (format.ts pulls in timezone state).
export function fmtPercent(ratio: number): string {
  const pct = ratio * 100
  if (pct >= 100) return '100'
  return pct.toFixed(2).replace(/\.?0+$/, '')
}

// The card's two rows sit one above the other, so they always carry two
// decimals: 99.90 % over 97.43 % reads as a matched pair, where the trimmed
// 99.9 % looks like a different size beside a four-digit neighbor.
export function fmtPercentFixed(ratio: number): string {
  return (Math.min(ratio, 1) * 100).toFixed(2)
}

export interface SiteScore {
  // null when the denominator is zero: no samples this month, or no
  // successful ones to grade.
  availability: number | null
  performance: number | null
}

export function siteScoreRatios(row: SiteScoreRow | undefined): SiteScore {
  if (!row || row.samples === 0) return { availability: null, performance: null }
  return {
    availability: row.ok_samples / row.samples,
    performance: row.ok_samples === 0 ? null : row.healthy_ok_samples / row.ok_samples,
  }
}

// The Site dashboard tiles' context line. Every loaded state names the
// response month (formatted server-side in UTC), so a snapshot retained
// across a month boundary — or after a failed refresh — can never read as
// the current month; `error` flags that retained-snapshot case the way the
// map card's caption does. Performance has its own zero-denominator state:
// samples with no OK run leave nothing to grade, which is not "no samples".
// The tiles' context lines wrap, and Chrome breaks after an ordinary hyphen,
// so the month's hyphen is rendered as U+2011 (non-breaking; IBM Plex Sans
// carries the glyph) to keep "2026-09" on one line.
export type SiteScoreKind = 'availability' | 'performance'

export function siteScoreContext(
  kind: SiteScoreKind,
  row: SiteScoreRow | undefined,
  month: string | null,
  error: boolean,
): string {
  if (month == null) return error ? 'Scores unavailable' : 'Loading month-to-date scores…'
  month = month.replaceAll('-', '\u2011')
  let text: string
  if (!row || row.samples === 0) text = `No samples · ${month}`
  else if (kind === 'performance' && row.ok_samples === 0) text = `No OK runs · ${month}`
  else text = `${kind === 'availability' ? 'OK probe runs' : 'OK runs in healthy hours'} · ${month}`
  return error ? `${text} · last snapshot` : text
}

// Tone bands shared with the Site dashboard's incident-free tile, so the
// product's two month-scale percentages never disagree about what "good"
// is: 100 % is good, anything down to 99 % is a warning, below is critical.
// Returns a class suffix (leading space) or '' for no data.
export function scoreTone(ratio: number | null): string {
  if (ratio == null) return ''
  if (ratio >= 1) return ' stat-good'
  if (ratio >= 0.99) return ' stat-warning'
  return ' stat-critical'
}
