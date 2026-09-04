import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { MAP_VIEW_H, MAP_VIEW_W } from '../assets/mapGeo'
import { fmtLatency } from '../format'
import { projectMap } from '../geo'
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
  type MapViewport,
} from '../mapViewport'
import { inheritRouteNetwork } from '../routeState'
import MapBeams, { BeamMarkers } from './MapBeams'
import { DOT_GRID_D } from '../mapDots'
import { bubbleRadius, declutter, type DeclutterNode } from '../mapLayout'
import type { BeamEnd, SiteLink } from '../mapLinks'
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

export default function WorldMap({ topology, links }: { topology: SiteTopology[]; links: SiteLink[] }) {
  const [pinned, setPinned] = useState<string | null>(null)
  const [hovered, setHovered] = useState<string | null>(null)
  const [viewport, setViewport] = useState<MapViewport>(FULL_MAP_VIEWPORT)
  const [dragging, setDragging] = useState(false)
  const [renderedMapWidth, setRenderedMapWidth] = useState(0)
  // The zoom announcer is debounced: wheel and pinch stream viewport
  // changes, and a polite live region firing on every frame reads as
  // babble. Only the settled value, half a second after the last change,
  // is announced.
  const [announcedZoom, setAnnouncedZoom] = useState<number | null>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const shellRef = useRef<HTMLDivElement>(null)
  const cardRef = useRef<HTMLDivElement>(null)
  const hintId = useId()
  const beamMarkerId = useId()
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

  // Beam endpoints follow the DISPLAY positions, so a decluttered bubble's
  // beams stay attached to the bubble, not to the empty city behind it.
  const beamEnds = useMemo(
    () => new Map<string, BeamEnd>(placed.map((p) => [p.topology.site.name, { x: p.x, y: p.y, r: p.r }])),
    [placed],
  )

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
  // from scrolling — the zoom handler must be attached natively. It listens
  // on the shell, not the svg: the marker buttons and the info card are
  // HTML siblings layered over the svg, and a wheel over them must still
  // zoom the map underneath.
  useEffect(() => {
    const shell = shellRef.current
    const svg = svgRef.current
    if (!shell || !svg) return
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
    shell.addEventListener('wheel', onWheel, { passive: false })
    return () => shell.removeEventListener('wheel', onWheel)
  }, [placed.length])

  // Keyboard and pointer handlers attach natively like the wheel listener:
  // the svg is a plain labeled image to assistive technology (its keyboard
  // affordance is described by the visible hint), and the delegated JSX
  // handlers a noninteractive element cannot carry move here instead. The
  // ref latch always dispatches to the latest render's closures, so
  // gestures never re-attach mid-drag.
  const handlerRef = useRef<{
    keydown: (event: KeyboardEvent) => void
    pointerdown: (event: PointerEvent) => void
    pointermove: (event: PointerEvent) => void
    pointerup: (event: PointerEvent) => void
  } | null>(null)
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    svg.tabIndex = 0
    const onKeyDown = (event: KeyboardEvent) => handlerRef.current?.keydown(event)
    const onPointerDown = (event: PointerEvent) => handlerRef.current?.pointerdown(event)
    const onPointerMove = (event: PointerEvent) => handlerRef.current?.pointermove(event)
    const onPointerUp = (event: PointerEvent) => handlerRef.current?.pointerup(event)
    svg.addEventListener('keydown', onKeyDown)
    svg.addEventListener('pointerdown', onPointerDown)
    svg.addEventListener('pointermove', onPointerMove)
    svg.addEventListener('pointerup', onPointerUp)
    svg.addEventListener('pointercancel', onPointerUp)
    return () => {
      svg.removeEventListener('keydown', onKeyDown)
      svg.removeEventListener('pointerdown', onPointerDown)
      svg.removeEventListener('pointermove', onPointerMove)
      svg.removeEventListener('pointerup', onPointerUp)
      svg.removeEventListener('pointercancel', onPointerUp)
    }
  }, [placed.length])

  // The info card keep-open handlers (pointer or focus inside the card
  // cancels the pending hover clear) are delegated from the shell so the
  // card stays a pure live region with no JSX handlers of its own. The
  // handlers only touch refs and stable setters, so mount-time closures
  // cannot go stale.
  useEffect(() => {
    const shell = shellRef.current
    if (!shell) return
    const within = (t: EventTarget | null) => t instanceof Node && (cardRef.current?.contains(t) ?? false)
    const onOver = (event: MouseEvent) => {
      if (within(event.target)) cancelHoverClear()
    }
    const onOut = (event: MouseEvent) => {
      if (within(event.target) && !within(event.relatedTarget)) scheduleHoverClear()
    }
    const onFocusIn = (event: FocusEvent) => {
      if (within(event.target)) cancelHoverClear()
    }
    const onFocusOut = (event: FocusEvent) => {
      if (within(event.target) && !within(event.relatedTarget)) scheduleHoverClear()
    }
    shell.addEventListener('mouseover', onOver)
    shell.addEventListener('mouseout', onOut)
    shell.addEventListener('focusin', onFocusIn)
    shell.addEventListener('focusout', onFocusOut)
    return () => {
      shell.removeEventListener('mouseover', onOver)
      shell.removeEventListener('mouseout', onOut)
      shell.removeEventListener('focusin', onFocusIn)
      shell.removeEventListener('focusout', onFocusOut)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [placed.length])

  const zoomPercent = mapZoomPercent(viewport)
  useEffect(() => {
    // Only operator-driven changes announce: the mount-time auto-fit and
    // fleet-swap refits are not zoom feedback anyone asked for.
    if (!interacted.current) return
    const timer = window.setTimeout(() => setAnnouncedZoom(zoomPercent), 500)
    return () => window.clearTimeout(timer)
  }, [zoomPercent])

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
  const keyboardViewport = (event: KeyboardEvent) => {
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
  const beginPointerPan = (event: PointerEvent) => {
    if (event.button !== 0) return
    suppressBackgroundClick.current = false
    pointers.current.set(event.pointerId, { clientX: event.clientX, clientY: event.clientY })
    try {
      svgRef.current?.setPointerCapture(event.pointerId)
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
  const movePointerPan = (event: PointerEvent) => {
    const svg = svgRef.current
    if (!svg) return
    const tracked = pointers.current.get(event.pointerId)
    if (tracked) {
      tracked.clientX = event.clientX
      tracked.clientY = event.clientY
    }
    const gesture = pinch.current
    if (gesture && pointers.current.size >= 2) {
      const bounds = svg.getBoundingClientRect()
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
    const bounds = svg.getBoundingClientRect()
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
  const endPointerPan = (event: PointerEvent) => {
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
  // Re-latched every render so the native listeners above always call the
  // closures over the current viewport and placement state.
  handlerRef.current = {
    keydown: keyboardViewport,
    pointerdown: beginPointerPan,
    pointermove: movePointerPan,
    pointerup: endPointerPan,
  }
  // The pinned site holds the info card open; hover previews another site.
  // The card anchors to the DISPLAY position, not the raw projection — a
  // decluttered bubble may sit a nudge away from its city, and the card
  // must stay attached to the bubble the pointer is on.
  const focusSite = hovered ?? pinned
  const shown = placed.find((p) => p.topology.site.name === focusSite) ?? null
  const shownSite = shown ? shown.topology.site : null
  const shownPoint = shown ? { x: shown.x, y: shown.y } : null
  const shownStats = shown ? shown.topology.stats : null
  const shownSev = shown ? shown.topology.severity : null
  const shownLeft = shownPoint ? ((shownPoint.x - viewport.x) / viewport.width) * 100 : 0
  const shownTop = shownPoint ? ((shownPoint.y - viewport.y) / viewport.height) * 100 : 0
  const shownInViewport = shownLeft >= 0 && shownLeft <= 100 && shownTop >= 0 && shownTop <= 100

  return (
    <>
      <span className="sr-only" role="status" aria-live="polite">
        {announcedZoom == null ? '' : `Map viewport at ${announcedZoom}% zoom.`}
      </span>
      <div ref={shellRef} className="worldmap-shell">
        {/* The svg is the pan/zoom surface: a labeled image whose drag and
            wheel gestures have keyboard equivalents on the focused map
            (native listeners, attached in the effect above), described by
            the visible key hint. Site markers are the HTML buttons layered
            over it, so browse mode reads them as ordinary buttons. */}
        <svg
          ref={svgRef}
          className={'worldmap' + (dragging ? ' map-dragging' : '') + (focusSite ? ' map-focus' : '')}
          viewBox={`${viewport.x} ${viewport.y} ${viewport.width} ${viewport.height}`}
          role="img"
          aria-label={`World map of ${placed.length} monitored ${placed.length === 1 ? 'site' : 'sites'}, ${zoomPercent}% zoom.`}
          aria-describedby={hintId}
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
          <BeamMarkers id={beamMarkerId} />
          <path className="map-dotgrid" d={DOT_GRID_D} />
          {/* Every measured direction, drawn from its origin's bubble to its
              destination's and graded on its own. Healthy beams recede so a
              quiet mesh reads as a faint lattice and only trouble stands
              out; hovering or pinning a site lifts its beams and dims the
              rest. */}
          <MapBeams links={links} ends={beamEnds} markerId={beamMarkerId} lit={focusSite} />
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
          {/* Purely decorative under the svg's role="img": the interactive
              markers are the HTML buttons layered over the shell, which
              also carry hover state back here via the hovered class. */}
          {placed.map((p) => {
            const { site: s, severity: sev } = p.topology
            const { x, y, r } = p
            return (
              <g
                key={s.name}
                className={
                  `map-site sev-${sev}` + (pinned === s.name ? ' pinned' : '') + (hovered === s.name ? ' hovered' : '')
                }
              >
                <circle className="map-bubble" cx={x} cy={y} r={r} />
                <circle className="map-bubble-core" cx={x} cy={y} r={3} />
                {pinned === s.name && <circle className="map-selection" cx={x} cy={y} r={r + 3.5} />}
              </g>
            )
          })}
        </svg>
        <div className="map-markers">
          {placed.map((p) => {
            const { site: s, severity: sev } = p.topology
            const { x, y } = p
            const title = `${s.display_name || s.name} · ${SEVERITY_LABEL[sev]}${s.location ? ` · ${s.location}` : ''}`
            // The button is the hit target the svg hit circle used to be:
            // at least the coarse-pointer touch size, grown to cover a
            // bubble the zoom has rendered larger.
            const bubblePx = ((2 * p.r) / viewport.width) * (renderedMapWidth || MAP_VIEW_W)
            const sizePx = Math.max(targetPixels, Math.ceil(bubblePx) + 8)
            return (
              <button
                key={s.name}
                type="button"
                className={`map-marker sev-${sev}` + (pinned === s.name ? ' pinned' : '')}
                style={{
                  left: `${((x - viewport.x) / viewport.width) * 100}%`,
                  top: `${((y - viewport.y) / viewport.height) * 100}%`,
                  width: sizePx,
                  height: sizePx,
                }}
                aria-pressed={pinned === s.name}
                aria-label={`${title}. Select to keep the site details open.`}
                onClick={() => togglePin(s.name)}
                onMouseEnter={() => hoverSite(s.name)}
                onMouseLeave={scheduleHoverClear}
                onFocus={(event) => {
                  // Focusing a clipped off-viewport marker makes the
                  // browser scroll the overflow-hidden marker layer; undo
                  // that and recenter the viewport on the site instead.
                  const layer = event.currentTarget.parentElement
                  if (layer) {
                    layer.scrollLeft = 0
                    layer.scrollTop = 0
                  }
                  hoverSite(s.name)
                  setViewport((current) => revealMapPoint(current, { x, y }))
                }}
                onBlur={scheduleHoverClear}
              />
            )
          })}
        </div>
        <span className="map-zoom-readout" aria-hidden="true">
          {zoomPercent}%
        </span>
        {shownSite &&
          shownPoint &&
          shownStats &&
          shownSev &&
          shownInViewport && (
            // Pointer or keyboard focus inside this card keeps it open (the
            // shell-delegated listeners in the effect above cancel the
            // pending hover clear), so the card stays a pure live region
            // rather than becoming a control.
            <div
              ref={cardRef}
              className={'map-tip' + (shownLeft > 72 ? ' map-tip-left' : '')}
              style={{
                left: `${shownLeft}%`,
                top: `${shownTop}%`,
              }}
              role="status"
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
      {/* The controls stay documented for the svg's aria-describedby (a
          closed disclosure's text still resolves) without occupying two
          lines of every Overview. */}
      <details className="map-help">
        <summary>Map controls</summary>
        <p className="hint" id={hintId}>
          Focus the map, then pan with the arrow keys, zoom with + and −, press F to fit all sites, 0 to reset. Drag to
          pan; scroll or pinch to zoom. Tab moves through site markers.
        </p>
      </details>
      {missingStrip}
    </>
  )
}
