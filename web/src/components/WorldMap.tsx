import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'
import { MAP_DOTS, MAP_VIEW_H, MAP_VIEW_W } from '../assets/mapGeo'
import { fmtLatency } from '../format'
import { projectMap } from '../geo'
import {
  fitMapViewport,
  FULL_MAP_VIEWPORT,
  layoutMapLabels,
  mapHitRadius,
  mapZoomPercent,
  panMapViewport,
  pinchMapViewport,
  revealMapPoint,
  zoomMapViewport,
  zoomMapViewportAt,
  type MapViewport,
} from '../mapViewport'
import { inheritRouteNetwork } from '../routeState'
import { bubbleRadius, declutter, type DeclutterNode } from '../mapLayout'
import { SEVERITY_LABEL, type Severity } from '../severity'
import type { SiteTopology } from '../siteTopology'

// warn and crit are both publicly "Degraded"; crit keeps a stronger visual
// intensity via its own class, never a different label.
const SEVERITIES: Severity[] = ['ok', 'warn', 'crit', 'down', 'stale']
const LARGE_MAP_TARGET = '(max-width: 760px), (pointer: coarse)'

function useMapTargetPixels(): number {
  const [large, setLarge] = useState(() => window.matchMedia(LARGE_MAP_TARGET).matches)
  useEffect(() => {
    const query = window.matchMedia(LARGE_MAP_TARGET)
    const update = () => setLarge(query.matches)
    query.addEventListener('change', update)
    update()
    return () => query.removeEventListener('change', update)
  }, [])
  return large ? 44 : 24
}

// The dot-matrix landmass: one path of zero-length round-capped segments,
// computed once — the geometry never changes at runtime.
let dotGrid = ''
for (let i = 0; i < MAP_DOTS.length; i += 2) {
  dotGrid += `M${MAP_DOTS[i]} ${MAP_DOTS[i + 1]}h.01`
}
const DOT_GRID_D = dotGrid

interface PlacedSite {
  topology: SiteTopology
  x: number // display center after declutter — markers AND the info card use this
  y: number
  anchorX: number // true projection — what the leader mark points back to
  anchorY: number
  r: number
  hitR: number
  displaced: boolean // shifted far enough to warrant an anchor mark
}

export default function WorldMap({ topology }: { topology: SiteTopology[] }) {
  const [pinned, setPinned] = useState<string | null>(null)
  const [hovered, setHovered] = useState<string | null>(null)
  const [viewport, setViewport] = useState<MapViewport>(FULL_MAP_VIEWPORT)
  const [dragging, setDragging] = useState(false)
  const [renderedMapWidth, setRenderedMapWidth] = useState(0)
  const svgRef = useRef<SVGSVGElement>(null)
  const targetPixels = useMapTargetPixels()
  const targetRadius = mapHitRadius(targetPixels, renderedMapWidth || MAP_VIEW_W)
  const drag = useRef<{
    pointerID: number
    clientX: number
    clientY: number
    viewport: MapViewport
    moved: boolean
  } | null>(null)
  const suppressBackgroundClick = useRef(false)
  // Once the operator pans or zooms, topology refreshes must not snap the
  // viewport back to the automatic fit.
  const interacted = useRef(false)
  // The wheel handler is a native listener and must read the live viewport
  // synchronously to decide whether zooming can consume the event.
  const viewportRef = useRef(viewport)
  viewportRef.current = viewport
  // Touch pinch: all live pointers on the map plus the gesture baseline.
  const pointers = useRef(new Map<number, { clientX: number; clientY: number }>())
  const pinch = useRef<{ distance: number; midX: number; midY: number; viewport: MapViewport } | null>(null)
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

  const { placed, unplaced } = useMemo(() => {
    const withCoords = topology.filter((entry) => entry.site.latitude != null && entry.site.longitude != null)
    const missingCoordinates = topology.filter((entry) => entry.site.latitude == null || entry.site.longitude == null)

    // Declutter layout uses the shared site's link count. The
    // name sort is the determinism anchor: declutter's output must not
    // depend on API response ordering. Stability across refreshes is free —
    // degree comes from pair topology, not per-refresh measurements, so
    // routine polling reproduces identical layouts.
    // toSorted would be cleaner but needs the ES2023 lib; sorting a fresh
    // copy mutates nothing the caller sees.
    // oxlint-disable-next-line unicorn/no-array-sort
    const ordered = [...withCoords].sort((a, b) => (a.site.name < b.site.name ? -1 : a.site.name > b.site.name ? 1 : 0))
    const nodes: DeclutterNode[] = ordered.map((entry) => {
      const { x, y } = projectMap(entry.site.longitude!, entry.site.latitude!)
      return { x, y, hitR: Math.max(targetRadius, bubbleRadius(entry.stats.degree)) }
    })
    const laidOut = declutter(nodes, { w: MAP_VIEW_W, h: MAP_VIEW_H }, Math.max(20, targetRadius))
    const layoutByName = new Map<string, PlacedSite>()
    ordered.forEach((entry, i) => {
      const r = bubbleRadius(entry.stats.degree)
      // The 3 px threshold keeps sub-visible nudges and float noise from
      // sprouting anchor marks.
      const displaced = Math.hypot(laidOut[i].x - nodes[i].x, laidOut[i].y - nodes[i].y) > 3
      layoutByName.set(entry.site.name, {
        topology: entry,
        x: laidOut[i].x,
        y: laidOut[i].y,
        anchorX: nodes[i].x,
        anchorY: nodes[i].y,
        r,
        hitR: nodes[i].hitR,
        displaced,
      })
    })
    // Back in the original sites order so DOM and tab order are unchanged.
    const placedEntries = withCoords.map((entry) => layoutByName.get(entry.site.name)!)
    return { placed: placedEntries, unplaced: missingCoordinates }
  }, [targetRadius, topology])

  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const measure = () => {
      const width = svg.getBoundingClientRect().width
      if (width > 0) setRenderedMapWidth((current) => (Math.abs(current - width) > 0.5 ? width : current))
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(svg)
    return () => observer.disconnect()
  }, [placed.length])

  // The fleet key tracks WHICH sites are on the map, not their health: a
  // topology poll leaves it unchanged, while a network-scope switch swaps it
  // — and must re-arm the automatic fit, or the map could keep a viewport
  // aimed at a region the new scope has no sites in.
  const fleetKey = useMemo(
    () =>
      placed
        .map((site) => site.topology.site.name)
        // oxlint-disable-next-line unicorn/no-array-sort -- sorting a fresh mapped array
        .sort()
        .join('\u0000'),
    [placed],
  )
  useEffect(() => {
    interacted.current = false
  }, [fleetKey])

  // Until the operator takes over, the viewport tracks the sites so the map
  // opens scaled to the fleet instead of the whole world.
  useEffect(() => {
    if (interacted.current || placed.length === 0) return
    setViewport(fitMapViewport(placed.map((site) => ({ x: site.anchorX, y: site.anchorY }))))
  }, [placed])

  // React registers wheel listeners as passive, which cannot stop the page
  // from scrolling — the zoom handler must be attached natively.
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onWheel = (event: WheelEvent) => {
      // Alt/meta/shift-modified wheel belongs to the browser. Ctrl+wheel
      // stays: that is how a trackpad pinch reaches the page, and a pinch
      // over the map must zoom the map.
      if (event.altKey || event.metaKey || event.shiftKey) return
      const bounds = svg.getBoundingClientRect()
      if (bounds.width === 0 || bounds.height === 0) return
      const step =
        event.deltaMode === WheelEvent.DOM_DELTA_PAGE
          ? 0.5
          : event.deltaMode === WheelEvent.DOM_DELTA_LINE
            ? 0.05
            : 0.0015
      const factor = Math.pow(2, event.deltaY * step)
      const current = viewportRef.current
      const next = zoomMapViewportAt(current, factor, {
        x: current.x + ((event.clientX - bounds.left) / bounds.width) * current.width,
        y: current.y + ((event.clientY - bounds.top) / bounds.height) * current.height,
      })
      // At the zoom limits the viewport cannot change; let the page scroll.
      if (next.width === current.width && next.x === current.x && next.y === current.y) return
      event.preventDefault()
      interacted.current = true
      setViewport(next)
    }
    svg.addEventListener('wheel', onWheel, { passive: false })
    return () => svg.removeEventListener('wheel', onWheel)
  }, [placed.length])

  const missingStrip = unplaced.length > 0 && (
    // Fail loud: sites without coordinates never silently vanish, and they
    // keep their live severity while off the map.
    <div className="map-missing">
      <span className="hint">
        Not on the map — set coordinates with <code>polarbeam-server site set</code>:
      </span>
      {unplaced.map(({ site, severity }) => {
        return (
          <span key={site.name} className="chip">
            <span className={`dot swatch sev-${severity}`} />
            {site.display_name || site.name} · {SEVERITY_LABEL[severity]}
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
  const pan = (horizontal: number, vertical: number) => {
    setViewport((current) => panMapViewport(current, current.width * horizontal, current.height * vertical))
  }
  const fitSites = () => setViewport(fitMapViewport(placed.map((site) => ({ x: site.anchorX, y: site.anchorY }))))
  // Pointer and wheel gestures need keyboard equivalents; the map itself is
  // the focus target now that there is no button toolbar.
  const keyboardViewport = (event: ReactKeyboardEvent<SVGSVGElement>) => {
    if (event.target !== event.currentTarget) return
    // Modified combinations belong to the browser: Ctrl/Cmd+F is find,
    // Ctrl/Cmd +/-/0 is page zoom, Alt+arrows is history navigation.
    if (event.ctrlKey || event.metaKey || event.altKey) return
    let handled = true
    switch (event.key) {
      case 'ArrowLeft':
        pan(-0.2, 0)
        break
      case 'ArrowRight':
        pan(0.2, 0)
        break
      case 'ArrowUp':
        pan(0, -0.2)
        break
      case 'ArrowDown':
        pan(0, 0.2)
        break
      case '+':
      case '=':
        setViewport((current) => zoomMapViewport(current, 0.7))
        break
      case '-':
      case '_':
        setViewport((current) => zoomMapViewport(current, 1.4))
        break
      case 'f':
      case 'F':
        fitSites()
        break
      case '0':
      case 'Home':
        setViewport(FULL_MAP_VIEWPORT)
        break
      default:
        handled = false
    }
    if (handled) {
      event.preventDefault()
      interacted.current = true
    }
  }
  const beginPointerPan = (event: ReactPointerEvent<SVGSVGElement>) => {
    if (event.button !== 0) return
    suppressBackgroundClick.current = false
    pointers.current.set(event.pointerId, { clientX: event.clientX, clientY: event.clientY })
    try {
      event.currentTarget.setPointerCapture(event.pointerId)
    } catch {
      // A pointer that already lifted cannot be captured; the pointerup
      // that follows cleans the gesture up normally.
    }
    if (pointers.current.size === 2) {
      // A second touch turns the pan into a pinch: touch fires no wheel
      // events, so this is the only zoom gesture touch-only devices have.
      const [a, b] = [...pointers.current.values()]
      drag.current = null
      pinch.current = {
        distance: Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY),
        midX: (a.clientX + b.clientX) / 2,
        midY: (a.clientY + b.clientY) / 2,
        viewport,
      }
      setDragging(true)
      return
    }
    if (pointers.current.size > 2) return
    drag.current = {
      pointerID: event.pointerId,
      clientX: event.clientX,
      clientY: event.clientY,
      viewport,
      moved: false,
    }
  }
  const movePointerPan = (event: ReactPointerEvent<SVGSVGElement>) => {
    const tracked = pointers.current.get(event.pointerId)
    if (tracked) {
      tracked.clientX = event.clientX
      tracked.clientY = event.clientY
    }
    const gesture = pinch.current
    if (gesture && pointers.current.size >= 2) {
      const bounds = event.currentTarget.getBoundingClientRect()
      if (bounds.width === 0 || bounds.height === 0) return
      const [a, b] = [...pointers.current.values()]
      const distance = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY)
      if (distance < 8) return
      interacted.current = true
      suppressBackgroundClick.current = true
      setViewport(
        pinchMapViewport(
          gesture.viewport,
          { distance: gesture.distance, midX: gesture.midX - bounds.left, midY: gesture.midY - bounds.top },
          {
            distance,
            midX: (a.clientX + b.clientX) / 2 - bounds.left,
            midY: (a.clientY + b.clientY) / 2 - bounds.top,
          },
          bounds,
        ),
      )
      return
    }
    const start = drag.current
    if (!start || start.pointerID !== event.pointerId) return
    const bounds = event.currentTarget.getBoundingClientRect()
    if (bounds.width === 0 || bounds.height === 0) return
    const dx = ((event.clientX - start.clientX) / bounds.width) * start.viewport.width
    const dy = ((event.clientY - start.clientY) / bounds.height) * start.viewport.height
    if (Math.hypot(event.clientX - start.clientX, event.clientY - start.clientY) > 3) {
      start.moved = true
      interacted.current = true
      setDragging(true)
    }
    setViewport(panMapViewport(start.viewport, -dx, -dy))
  }
  const endPointerPan = (event: ReactPointerEvent<SVGSVGElement>) => {
    pointers.current.delete(event.pointerId)
    if (pinch.current) {
      if (pointers.current.size >= 2) return
      pinch.current = null
      setDragging(false)
      // A finger left mid-pinch: hand the survivor a fresh pan baseline so
      // the viewport does not jump.
      const [remaining] = [...pointers.current.entries()]
      if (remaining) {
        drag.current = {
          pointerID: remaining[0],
          clientX: remaining[1].clientX,
          clientY: remaining[1].clientY,
          viewport: viewportRef.current,
          moved: true,
        }
        setDragging(true)
      }
      return
    }
    const start = drag.current
    if (!start || start.pointerID !== event.pointerId) return
    suppressBackgroundClick.current = start.moved
    drag.current = null
    setDragging(false)
  }
  // The pinned site holds the info card open; hover previews another site.
  // The card anchors to the DISPLAY position, not the raw projection — a
  // decluttered bubble may sit a nudge away from its city, and the card
  // must stay attached to the bubble the pointer is on.
  const shown = placed.find((p) => p.topology.site.name === (hovered ?? pinned)) ?? null
  const shownSite = shown ? shown.topology.site : null
  const shownPoint = shown ? { x: shown.x, y: shown.y } : null
  const shownStats = shown ? shown.topology.stats : null
  const shownSev = shown ? shown.topology.severity : null
  const zoomPercent = mapZoomPercent(viewport)
  const shownLeft = shownPoint ? ((shownPoint.x - viewport.x) / viewport.width) * 100 : 0
  const shownTop = shownPoint ? ((shownPoint.y - viewport.y) / viewport.height) * 100 : 0
  const shownInViewport = shownLeft >= 0 && shownLeft <= 100 && shownTop >= 0 && shownTop <= 100
  const labeledSites = placed.filter(({ topology: entry }) => entry.severity !== 'ok' || pinned === entry.site.name)
  const mapLabels = layoutMapLabels(
    labeledSites.map((placedSite) => ({ id: placedSite.topology.site.name, x: placedSite.x, y: placedSite.y })),
    viewport,
  )
  const placedByName = new Map(placed.map((placedSite) => [placedSite.topology.site.name, placedSite]))

  return (
    <>
      <span className="sr-only" role="status" aria-live="polite">
        Map viewport at {zoomPercent}% zoom.
      </span>
      <div className="worldmap-shell">
        {/* The svg is the pan/zoom widget itself: drag and wheel gestures get
            keyboard equivalents on the focused map, and no interactive ARIA
            role describes a map viewport. */}
        {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
        <svg
          ref={svgRef}
          className={'worldmap' + (dragging ? ' map-dragging' : '')}
          viewBox={`${viewport.x} ${viewport.y} ${viewport.width} ${viewport.height}`}
          role="application"
          // oxlint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- keyboard pan/zoom needs a focusable map
          tabIndex={0}
          aria-label={`World map of ${placed.length} monitored ${placed.length === 1 ? 'site' : 'sites'}, ${zoomPercent}% zoom. Drag to pan and scroll to zoom, or focus the map and use the arrow keys to pan, plus and minus to zoom, F to fit all sites, and 0 to reset.`}
          onKeyDown={keyboardViewport}
          onPointerDown={beginPointerPan}
          onPointerDownCapture={() => {
            suppressBackgroundClick.current = false
          }}
          onPointerMove={movePointerPan}
          onPointerUp={endPointerPan}
          onPointerCancel={endPointerPan}
        >
          <rect
            className="map-bg"
            width={MAP_VIEW_W}
            height={MAP_VIEW_H}
            onClick={() => {
              if (suppressBackgroundClick.current) {
                suppressBackgroundClick.current = false
                return
              }
              setPinned(null)
            }}
          />
          <path className="map-dotgrid" d={DOT_GRID_D} />
          {/* A nudged bubble no longer sits exactly on its city; the anchor
              dot keeps the map honest about the true projected location.
              Decorative only (each site's aria-label already names it), and
              painted under the markers so it never intercepts events. */}
          <g className="map-leaders" aria-hidden="true">
            {placed
              .filter((p) => p.displaced)
              .map((p) => (
                <g key={p.topology.site.name}>
                  <line className="map-leader" x1={p.anchorX} y1={p.anchorY} x2={p.x} y2={p.y} />
                  <circle className="map-anchor-dot" cx={p.anchorX} cy={p.anchorY} r={1.8} />
                </g>
              ))}
          </g>
          <g className="map-label-leaders" aria-hidden="true">
            {mapLabels.map((label) => {
              const placedSite = placedByName.get(label.id)!
              const desiredTop = ((placedSite.y - viewport.y) / viewport.height) * 100
              if (Math.abs(desiredTop - label.top) < 1) return null
              return (
                <line
                  key={label.id}
                  className="map-label-leader"
                  x1={placedSite.x}
                  y1={placedSite.y}
                  x2={placedSite.x}
                  y2={viewport.y + (label.top / 100) * viewport.height}
                />
              )
            })}
          </g>
          {placed.map((p) => {
            const { site: s, severity: sev } = p.topology
            const { x, y, r } = p
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
                onPointerDown={(event) => event.stopPropagation()}
                onMouseEnter={() => hoverSite(s.name)}
                onMouseLeave={scheduleHoverClear}
                onFocus={() => {
                  hoverSite(s.name)
                  setViewport((current) => revealMapPoint(current, { x, y }))
                }}
                onBlur={scheduleHoverClear}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    togglePin(s.name)
                  }
                }}
              >
                <title>{title}</title>
                <circle className="map-site-hit" cx={x} cy={y} r={p.hitR} />
                <circle className="map-bubble" cx={x} cy={y} r={r} />
                <circle className="map-bubble-core" cx={x} cy={y} r={3} />
                {pinned === s.name && <circle className="map-selection" cx={x} cy={y} r={r + 3.5} />}
              </g>
            )
          })}
        </svg>
        <span className="map-zoom-readout" aria-hidden="true">
          {zoomPercent}%
        </span>
        {mapLabels.map((label) => {
          const placedSite = placedByName.get(label.id)!
          const { site, severity } = placedSite.topology
          return (
            <span
              key={site.name}
              className={
                'map-site-label' +
                (pinned === site.name ? ' selected' : '') +
                (label.align === 'right' ? ' map-site-label-right' : '')
              }
              style={{ left: `${label.left}%`, top: `${label.top}%` }}
              aria-hidden="true"
            >
              {site.display_name || site.name} · {SEVERITY_LABEL[severity]}
            </span>
          )
        })}
        {shownSite &&
          shownPoint &&
          shownStats &&
          shownSev &&
          shownInViewport && (
            // The handlers below keep this hover card open while the pointer or
            // keyboard focus is inside it; they add no interaction of their own,
            // so the card stays a live region rather than becoming a control.
            // oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions
            <div
              className={'map-tip' + (shownLeft > 72 ? ' map-tip-left' : '')}
              style={{
                left: `${shownLeft}%`,
                top: `${shownTop}%`,
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
              {shownStats.netCounts.size > 1 &&
                [...shownStats.netCounts.entries()]
                  // oxlint-disable-next-line unicorn/no-array-sort -- toSorted needs ES2023 lib
                  .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
                  .map(([name, counts]) => (
                    <div key={name} className="map-tip-caption">
                      <span className="mono">{name}</span> · {counts.ok} of {counts.total} healthy
                    </div>
                  ))}
              {shownStats.peers.length > 0 && (
                <div className="map-tip-links">
                  {shownStats.peers.map((peer) => (
                    <a
                      key={peer}
                      href={inheritRouteNetwork(
                        `#/pair/${encodeURIComponent(shownSite.name)}/${encodeURIComponent(peer)}`,
                      )}
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
      {legend}
      {missingStrip}
    </>
  )
}
