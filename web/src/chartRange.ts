export interface ChartRange {
  min: number
  max: number
}

export type ChartRangeReason = 'live' | 'preserved' | 'partial-overlap' | 'empty' | 'expired' | 'context-change'

export interface ChartRangeReconciliation {
  mode: 'live' | 'zoomed'
  range: ChartRange | null
  reason: ChartRangeReason
}

export function xExtent(values: ArrayLike<number | null | undefined>): ChartRange | null {
  let min = Number.POSITIVE_INFINITY
  let max = Number.NEGATIVE_INFINITY
  for (let index = 0; index < values.length; index++) {
    const value = values[index]
    if (value == null || !Number.isFinite(value)) continue
    min = Math.min(min, value)
    max = Math.max(max, value)
  }
  return Number.isFinite(min) && Number.isFinite(max) ? { min, max } : null
}

// Keyboard zoom: scale the span around the range's center (factor < 1
// zooms in). Returns null when the result would cover the whole extent —
// the caller returns to live instead of holding a zoom that shows
// everything. The span never shrinks below extent/1000 so repeated
// zoom-in cannot collapse the range to a point.
export function zoomChartRange(range: ChartRange, factor: number, extent: ChartRange | null): ChartRange | null {
  const center = (range.min + range.max) / 2
  let span = (range.max - range.min) * factor
  if (!extent) return { min: center - span / 2, max: center + span / 2 }
  const fullSpan = extent.max - extent.min
  span = Math.max(span, fullSpan / 1000)
  if (span >= fullSpan) return null
  let min = center - span / 2
  let max = center + span / 2
  if (min < extent.min) {
    max += extent.min - min
    min = extent.min
  } else if (max > extent.max) {
    min -= max - extent.max
    max = extent.max
  }
  return { min, max }
}

// Keyboard pan: shift by a fraction of the current span (negative =
// toward older data), span-preserving, clamped so the range never leaves
// the extent. A null extent (no data) pans nowhere, and neither does a
// preserved range already wider than the extent — clamping it would jump
// the same direction for both arrows; zooming in is the way back.
export function panChartRange(range: ChartRange, fraction: number, extent: ChartRange | null): ChartRange {
  if (!extent) return range
  const span = range.max - range.min
  if (span >= extent.max - extent.min) return range
  let min = range.min + span * fraction
  min = Math.min(Math.max(min, extent.min), extent.max - span)
  return { min, max: min + span }
}

// A zoom is an absolute investigation range, not a percentage of whatever
// data is currently retained. Partial overlap therefore preserves both
// endpoints (including the empty portion); only total expiry returns live.
export function reconcileChartRange(
  selected: ChartRange | null,
  data: ChartRange | null,
  contextChanged = false,
): ChartRangeReconciliation {
  if (contextChanged) return { mode: 'live', range: null, reason: 'context-change' }
  if (!selected) return { mode: 'live', range: null, reason: 'live' }
  if (!data) return { mode: 'zoomed', range: selected, reason: 'empty' }
  if (selected.max < data.min || selected.min > data.max) {
    return { mode: 'live', range: null, reason: 'expired' }
  }
  const partial = selected.min < data.min || selected.max > data.max
  return { mode: 'zoomed', range: selected, reason: partial ? 'partial-overlap' : 'preserved' }
}
