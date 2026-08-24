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
