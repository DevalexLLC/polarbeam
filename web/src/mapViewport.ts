import { MAP_VIEW_H, MAP_VIEW_W } from './assets/mapGeo.ts'

export interface MapPoint {
  x: number
  y: number
}

export interface MapLabelPoint extends MapPoint {
  id: string
}

export interface MapLabelPlacement {
  id: string
  left: number
  top: number
  align: 'left' | 'right'
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

// Labels use two deterministic vertical tracks (left- and right-opening)
// so a cluster of unhealthy markers cannot paint unreadable text on top of
// itself. The leader layer connects any shifted label back to its marker.
export function layoutMapLabels(points: readonly MapLabelPoint[], view: MapViewport): MapLabelPlacement[] {
  const visible = points
    .map((point) => ({
      id: point.id,
      left: ((point.x - view.x) / view.width) * 100,
      desiredTop: ((point.y - view.y) / view.height) * 100,
      align: point.x < view.x + view.width / 2 ? ('left' as const) : ('right' as const),
    }))
    .filter((point) => point.left >= 0 && point.left <= 100 && point.desiredTop >= 0 && point.desiredTop <= 100)

  const placements: MapLabelPlacement[] = []
  for (const align of ['left', 'right'] as const) {
    const track = visible
      .filter((point) => point.align === align)
      // oxlint-disable-next-line unicorn/no-array-sort -- sorting a fresh filtered array
      .sort((a, b) => a.desiredTop - b.desiredTop || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
    if (track.length === 0) continue
    const gap = Math.min(12, 92 / Math.max(1, track.length - 1))
    let previous = 4 - gap
    const laidOut = track.map((point) => {
      const top = Math.max(4, point.desiredTop, previous + gap)
      previous = top
      return { id: point.id, left: point.left, top, align }
    })
    if (laidOut.at(-1)!.top > 96) {
      for (let index = laidOut.length - 1; index >= 0; index--) {
        const ceiling = index === laidOut.length - 1 ? 96 : laidOut[index + 1].top - gap
        laidOut[index].top = Math.min(laidOut[index].top, ceiling)
      }
    }
    placements.push(...laidOut)
  }
  return placements
}
