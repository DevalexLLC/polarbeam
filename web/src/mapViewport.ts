import { MAP_VIEW_H, MAP_VIEW_W } from './assets/mapGeo.ts'

export interface MapPoint {
  x: number
  y: number
}

export interface MapViewport {
  x: number
  y: number
  width: number
  height: number
}

export const FULL_MAP_VIEWPORT: MapViewport = { x: 0, y: 0, width: MAP_VIEW_W, height: MAP_VIEW_H }

const ASPECT = MAP_VIEW_W / MAP_VIEW_H
const MIN_WIDTH = MAP_VIEW_W / 8
const FIT_PADDING = 0.14

export function clampMapViewport(view: MapViewport): MapViewport {
  const width = Math.min(MAP_VIEW_W, Math.max(MIN_WIDTH, view.width))
  const height = width / ASPECT
  return {
    x: Math.min(MAP_VIEW_W - width, Math.max(0, view.x)),
    y: Math.min(MAP_VIEW_H - height, Math.max(0, view.y)),
    width,
    height,
  }
}

export function zoomMapViewport(view: MapViewport, factor: number): MapViewport {
  const width = Math.min(MAP_VIEW_W, Math.max(MIN_WIDTH, view.width * factor))
  const height = width / ASPECT
  return clampMapViewport({
    x: view.x + (view.width - width) / 2,
    y: view.y + (view.height - height) / 2,
    width,
    height,
  })
}

// Wheel zoom anchors on the pointer: the map point under the cursor keeps
// its on-screen position, so zooming reads as diving into that spot.
export function zoomMapViewportAt(view: MapViewport, factor: number, focus: MapPoint): MapViewport {
  const width = Math.min(MAP_VIEW_W, Math.max(MIN_WIDTH, view.width * factor))
  const height = width / ASPECT
  const fx = (focus.x - view.x) / view.width
  const fy = (focus.y - view.y) / view.height
  return clampMapViewport({
    x: focus.x - fx * width,
    y: focus.y - fy * height,
    width,
    height,
  })
}

export interface PinchInput {
  distance: number // spacing between the two pointers, client px
  midX: number // gesture midpoint relative to the rendered map's left edge
  midY: number // gesture midpoint relative to the rendered map's top edge
}

// One combined pinch-and-pan step: the map point that started the gesture
// under its midpoint stays under the current midpoint at the new scale.
// `bounds` is the rendered map's client size.
export function pinchMapViewport(
  start: MapViewport,
  from: PinchInput,
  to: PinchInput,
  bounds: { width: number; height: number },
): MapViewport {
  // Clamp the scale BEFORE anchoring: past the zoom caps the requested
  // width is not the rendered width, and anchoring on it drifts the map
  // point out from under a stationary midpoint.
  const width = Math.min(MAP_VIEW_W, Math.max(MIN_WIDTH, start.width * (from.distance / to.distance)))
  const height = width / ASPECT
  const mapMidX = start.x + (from.midX / bounds.width) * start.width
  const mapMidY = start.y + (from.midY / bounds.height) * start.height
  return clampMapViewport({
    x: mapMidX - (to.midX / bounds.width) * width,
    y: mapMidY - (to.midY / bounds.height) * height,
    width,
    height,
  })
}

export function panMapViewport(view: MapViewport, dx: number, dy: number): MapViewport {
  return clampMapViewport({ ...view, x: view.x + dx, y: view.y + dy })
}

export function revealMapPoint(view: MapViewport, point: MapPoint): MapViewport {
  const padX = view.width * 0.08
  const padY = view.height * 0.08
  if (
    point.x >= view.x + padX &&
    point.x <= view.x + view.width - padX &&
    point.y >= view.y + padY &&
    point.y <= view.y + view.height - padY
  ) {
    return view
  }
  return clampMapViewport({
    ...view,
    x: point.x - view.width / 2,
    y: point.y - view.height / 2,
  })
}

// Fit uses projected points and ignores missing/non-finite coordinates. A
// single or coincident group gets a useful four-times zoom instead of a
// zero-size viewBox; worldwide bounds naturally clamp to the full map.
export function fitMapViewport(points: readonly (MapPoint | null)[]): MapViewport {
  const valid = points.filter(
    (point): point is MapPoint => point !== null && Number.isFinite(point.x) && Number.isFinite(point.y),
  )
  if (valid.length === 0) return FULL_MAP_VIEWPORT

  const xs = valid.map((point) => point.x)
  const ys = valid.map((point) => point.y)
  const minX = Math.min(...xs)
  const maxX = Math.max(...xs)
  const minY = Math.min(...ys)
  const maxY = Math.max(...ys)
  const usable = 1 - FIT_PADDING * 2
  const width = Math.max(MIN_WIDTH * 2, (maxX - minX) / usable, ((maxY - minY) * ASPECT) / usable)
  const height = width / ASPECT
  return clampMapViewport({
    x: (minX + maxX) / 2 - width / 2,
    y: (minY + maxY) / 2 - height / 2,
    width,
    height,
  })
}

export function mapZoomPercent(view: MapViewport): number {
  return Math.round((MAP_VIEW_W / view.width) * 100)
}

export function mapHitRadius(targetPixels: number, renderedMapWidth: number): number {
  if (!Number.isFinite(renderedMapWidth) || renderedMapWidth <= 0) return targetPixels / 2
  return (targetPixels / 2) * (MAP_VIEW_W / renderedMapWidth)
}
