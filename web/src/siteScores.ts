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
