import { useMemo, useRef, useState } from 'react'
import { MAP_DOTS, MAP_VIEW_H, MAP_VIEW_W } from '../assets/mapGeo'
import { fmtLatency } from '../format'
import { projectMap } from '../geo'
import { directionSeverity, SEVERITY_LABEL, worst, type Severity } from '../severity'
import type { MatrixCell, Site, ThresholdSettings } from '../types'

// Site names are unrestricted text (spaces included); NUL cannot appear in
// Postgres text, so it is the one collision-free separator.
const pairKey = (a: string, b: string) => a + '\u0000' + b

// warn and crit are both publicly "Degraded"; crit keeps a stronger visual
// intensity via its own class, never a different label.
const SEVERITIES: Severity[] = ['ok', 'warn', 'crit', 'down', 'stale']

// The dot-matrix landmass: one path of zero-length round-capped segments,
// computed once — the geometry never changes at runtime.
let dotGrid = ''
for (let i = 0; i < MAP_DOTS.length; i += 2) {
  dotGrid += `M${MAP_DOTS[i]} ${MAP_DOTS[i + 1]}h.01`
}
const DOT_GRID_D = dotGrid

interface SiteStats {
  degree: number // monitored unordered pairs touching this site
  bestLatencyUs: number | null
  directions: number // configured directions (cells) touching this site
  dirCounts: Record<Severity, number>
  peers: string[] // the other end of each monitored pair, for pair links
}

function newStats(): SiteStats {
  return {
    degree: 0,
    bestLatencyUs: null,
    directions: 0,
    dirCounts: { ok: 0, warn: 0, crit: 0, down: 0, stale: 0 },
    peers: [],
  }
}

export default function WorldMap({
  sites,
  cells,
  thresholds,
}: {
  sites: Site[]
  cells: MatrixCell[]
  thresholds: ThresholdSettings | null
}) {
  const [pinned, setPinned] = useState<string | null>(null)
  const [hovered, setHovered] = useState<string | null>(null)
  // The info card is interactive (pair links), so leaving a bubble clears
  // the hover on a short delay — long enough to cross onto the card, which
  // cancels the clear. Pinning holds the card open regardless.
  const hoverClear = useRef<number | undefined>(undefined)
  const cancelHoverClear = () => window.clearTimeout(hoverClear.current)
  const scheduleHoverClear = () => {
    cancelHoverClear()
    hoverClear.current = window.setTimeout(() => setHovered(null), 140)
  }
  const hoverSite = (name: string) => {
    cancelHoverClear()
    setHovered(name)
  }

  // The locals below deliberately carry the same names the memo result is
  // destructured into — they are the same values, and renaming either side
  // would only invent a second vocabulary for one concept.
  /* oxlint-disable no-shadow */
  const { placed, unplaced, siteSeverity, siteStats } = useMemo(() => {
    const placed = sites.filter((s) => s.latitude != null && s.longitude != null)
    const unplaced = sites.filter((s) => s.latitude == null || s.longitude == null)

    // Every site's severity folds all cells touching it in either direction,
    // so an unplaced site's chip is as honest as a placed site's bubble.
    // Sites no cell touches read back as stale below — a site with no
    // measurements must not claim OK.
    const siteSeverity = new Map<string, Severity>()
    const siteStats = new Map<string, SiteStats>()
    for (const site of sites) siteStats.set(site.name, newStats())
    for (const c of cells) {
      const sev = directionSeverity(c, thresholds)
      for (const name of [c.src, c.dst]) {
        const prev = siteSeverity.get(name)
        siteSeverity.set(name, prev === undefined ? sev : worst(prev, sev))
        const stats = siteStats.get(name)
        if (stats) {
          stats.directions++
          stats.dirCounts[sev]++
        }
      }
    }

    // Link count and best latency cover every monitored pair — a placed site
    // probing an unplaced peer still has that link (only drawing needs
    // coordinates, and bubbles need only their own). One grouping pass keeps
    // this linear in cells; filtering per pair would go quadratic on large
    // meshes.
    const byPair = new Map<string, { x: string; y: string; cells: MatrixCell[] }>()
    for (const c of cells) {
      const [x, y] = c.src < c.dst ? [c.src, c.dst] : [c.dst, c.src]
      const key = pairKey(x, y)
      let entry = byPair.get(key)
      if (!entry) byPair.set(key, (entry = { x, y, cells: [] }))
      entry.cells.push(c)
    }
    for (const { x, y, cells: pairCells } of byPair.values()) {
      const liveLatencies = pairCells
        .filter((cell) => cell.status === 'ok' || cell.status === 'degraded')
        .map((cell) => cell.latency_us)
        .filter((latency): latency is number => latency != null)
      for (const [name, peer] of [
        [x, y],
        [y, x],
      ]) {
        const stats = siteStats.get(name)
        if (!stats) continue
        stats.degree++
        stats.peers.push(peer)
        if (liveLatencies.length > 0) {
          const best = Math.min(...liveLatencies)
          stats.bestLatencyUs = stats.bestLatencyUs == null ? best : Math.min(stats.bestLatencyUs, best)
        }
      }
    }
    for (const stats of siteStats.values()) stats.peers.sort()
    return { placed, unplaced, siteSeverity, siteStats }
  }, [sites, cells, thresholds])
  /* oxlint-enable no-shadow */

  // Sites the matrix has never measured read as stale, never as OK.
  const sevOf = (name: string): Severity => siteSeverity.get(name) ?? 'stale'

  const missingStrip = unplaced.length > 0 && (
    // Fail loud: sites without coordinates never silently vanish, and they
    // keep their live severity while off the map.
    <div className="map-missing">
      <span className="hint">
        Not on the map — set coordinates with <code>polarbeam-server site set</code>:
      </span>
      {unplaced.map((s) => {
        const sev = sevOf(s.name)
        return (
          <span key={s.name} className="chip">
            <span className={`dot swatch sev-${sev}`} />
            {s.display_name || s.name} · {SEVERITY_LABEL[sev]}
          </span>
        )
      })}
    </div>
  )

  const legend = (
    <div className="map-legend" aria-label="Map status legend">
      {(['ok', 'warn', 'down', 'stale'] as const).map((s) => (
        <span key={s} className="legend-item">
          <span className={'map-legend-dot sev-' + s} /> {SEVERITY_LABEL[s]}
        </span>
      ))}
      <span className="map-legend-hint">Select a site for details</span>
    </div>
  )

  if (placed.length === 0) {
    return (
      <>
        <div className="map-empty">
          <p className="muted">
            No sites have map coordinates yet. Place them with{' '}
            <code>polarbeam-server site set --name &lt;site&gt; --lat &lt;deg&gt; --lon &lt;deg&gt;</code>
          </p>
        </div>
        {missingStrip}
      </>
    )
  }

  const togglePin = (name: string) => setPinned((p) => (p === name ? null : name))
  // The pinned site holds the info card open; hover previews another site.
  const shownSite = placed.find((s) => s.name === (hovered ?? pinned)) ?? null
  const shownPoint = shownSite ? projectMap(shownSite.longitude!, shownSite.latitude!) : null
  const shownStats = shownSite ? siteStats.get(shownSite.name) : null
  const shownSev = shownSite ? sevOf(shownSite.name) : null

  return (
    <>
      <div className="worldmap-shell">
        <svg
          className="worldmap"
          viewBox={`0 0 ${MAP_VIEW_W} ${MAP_VIEW_H}`}
          role="img"
          aria-label={`World map of ${placed.length} monitored ${placed.length === 1 ? 'site' : 'sites'}`}
        >
          <rect className="map-bg" width={MAP_VIEW_W} height={MAP_VIEW_H} onClick={() => setPinned(null)} />
          <path className="map-dotgrid" d={DOT_GRID_D} />
          {placed.map((s) => {
            const { x, y } = projectMap(s.longitude!, s.latitude!)
            const sev = sevOf(s.name)
            const stats = siteStats.get(s.name) ?? newStats()
            const r = Math.max(9, Math.min(24, 8 + 3.5 * Math.sqrt(stats.degree)))
            const title = `${s.display_name || s.name} · ${SEVERITY_LABEL[sev]}${s.location ? ` · ${s.location}` : ''}`
            return (
              <g
                key={s.name}
                className={`map-site sev-${sev}` + (pinned === s.name ? ' pinned' : '')}
                role="button"
                tabIndex={0}
                aria-pressed={pinned === s.name}
                aria-label={`${title}. Select to keep the site details open.`}
                onClick={() => togglePin(s.name)}
                onMouseEnter={() => hoverSite(s.name)}
                onMouseLeave={scheduleHoverClear}
                onFocus={() => hoverSite(s.name)}
                onBlur={scheduleHoverClear}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    togglePin(s.name)
                  }
                }}
              >
                <title>{title}</title>
                <circle className="map-site-hit" cx={x} cy={y} r={Math.max(12, r)} />
                <circle className="map-bubble" cx={x} cy={y} r={r} />
                <circle className="map-bubble-core" cx={x} cy={y} r={3} />
                {pinned === s.name && <circle className="map-selection" cx={x} cy={y} r={r + 3.5} />}
              </g>
            )
          })}
        </svg>
        {legend}
        {shownSite &&
          shownPoint &&
          shownStats &&
          shownSev && (
            // The handlers below keep this hover card open while the pointer or
            // keyboard focus is inside it; they add no interaction of their own,
            // so the card stays a live region rather than becoming a control.
            // oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions
            <div
              className={'map-tip' + (shownPoint.x > MAP_VIEW_W * 0.72 ? ' map-tip-left' : '')}
              style={{
                left: `${(shownPoint.x / MAP_VIEW_W) * 100}%`,
                top: `${(shownPoint.y / MAP_VIEW_H) * 100}%`,
              }}
              role="status"
              onMouseEnter={cancelHoverClear}
              onMouseLeave={scheduleHoverClear}
              // Focus/blur bubble from the pair links, so keyboard entry
              // cancels the pending clear exactly like mouse entry — without
              // this, tabbing from the last bubble into the card races the
              // 140 ms clear and the focused link can unmount mid-tab.
              onFocus={cancelHoverClear}
              onBlur={scheduleHoverClear}
            >
              <div className="map-tip-head">
                <b>{shownSite.name.toUpperCase()}</b>
                <span className={`map-tip-pill sev-${shownSev}`}>{SEVERITY_LABEL[shownSev]}</span>
              </div>
              {(shownSite.location || shownSite.display_name) && (
                <div className="map-tip-sub">{shownSite.location || shownSite.display_name}</div>
              )}
              <div className="map-tip-value">
                {shownStats.bestLatencyUs == null ? '—' : fmtLatency(shownStats.bestLatencyUs)}
                <small> best live latency</small>
              </div>
              {shownStats.directions > 0 && (
                <div
                  className="map-tip-bar"
                  role="img"
                  aria-label={`${shownStats.dirCounts.ok} of ${shownStats.directions} directions healthy`}
                >
                  {SEVERITIES.filter((sev) => shownStats.dirCounts[sev] > 0).map((sev) => (
                    <span
                      key={sev}
                      className={`map-tip-bar-seg sev-${sev}`}
                      style={{ flexGrow: shownStats.dirCounts[sev] }}
                    />
                  ))}
                </div>
              )}
              <div className="map-tip-caption">
                {shownStats.degree} {shownStats.degree === 1 ? 'link' : 'links'} · {shownStats.dirCounts.ok} of{' '}
                {shownStats.directions} {shownStats.directions === 1 ? 'direction' : 'directions'} healthy
              </div>
              {shownStats.peers.length > 0 && (
                <div className="map-tip-links">
                  {shownStats.peers.map((peer) => (
                    <a
                      key={peer}
                      href={`#/pair/${encodeURIComponent(shownSite.name)}/${encodeURIComponent(peer)}`}
                      aria-label={`Open pair detail for ${shownSite.name} and ${peer}`}
                    >
                      {shownSite.name} ⇄ {peer}
                      <span className="map-tip-link-arrow" aria-hidden="true">
                        ↗
                      </span>
                    </a>
                  ))}
                </div>
              )}
            </div>
          )}
      </div>
      {missingStrip}
    </>
  )
}
