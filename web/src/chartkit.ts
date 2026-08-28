import uPlot from 'uplot'
import type { SeriesPoint } from './types'

// Shared uPlot machinery for the metric chart views (PairDetail,
// TargetDetail). Everything here is behavior-identical to what PairDetail
// originally defined inline; views keep their own mkOptions because the
// series shapes and cache keys differ.

export type Metric = 'latency' | 'loss'

// Chart series colors are categorical slots 1 (blue, outbound) and 5
// (magenta, return) — never slot 2's orange, which reads as the crit/down
// alarm ramp. Must stay in lockstep with --series-a/--series-b in
// styles.css; the magenta validates CVD + contrast against blue on both
// surfaces, so it is unstepped. warn/crit ARE the alarm ramp on purpose:
// they draw the effective threshold reference lines and mirror
// --status-warn/--status-crit in styles.css.
export const CHART_COLORS = {
  light: { aToB: '#2a78d6', bToA: '#d55181', grid: '#e0dfd9', axis: '#55544d', warn: '#8a6100', crit: '#cf4a10' },
  dark: { aToB: '#3987e5', bToA: '#d55181', grid: '#30312d', axis: '#b9b8ae', warn: '#ffc247', crit: '#f0692e' },
}

// Threshold levels in the CURRENT metric's y units (ms or loss %), read by
// the chart plugin at draw time. Views hold these in a ref, never options
// state: mkOptions caches options objects, and baking levels into them
// would either destroy the plots on every settings poll or draw stale
// lines.
export interface ThresholdLevels {
  warn: number | null
  crit: number | null
  warnColor: string
  critColor: string
}

// Draws one dashed horizontal reference line, skipped when outside the
// current y-range rather than forcing scale expansion — a 250 ms crit line
// must not flatten a 2 ms-baseline chart.
function drawThresholdLine(u: uPlot, v: number | null, color: string) {
  const yMin = u.scales.y.min
  const yMax = u.scales.y.max
  if (v == null || yMin == null || yMax == null || v < yMin || v > yMax) return
  const y = u.valToPos(v, 'y', true)
  const ctx = u.ctx
  ctx.save()
  ctx.strokeStyle = color
  ctx.lineWidth = window.devicePixelRatio || 1
  ctx.setLineDash([6, 6])
  ctx.beginPath()
  ctx.moveTo(u.bbox.left, y)
  ctx.lineTo(u.bbox.left + u.bbox.width, y)
  ctx.stroke()
  ctx.restore()
}

// getLevels is called at draw time so cached options never draw stale
// lines — pass a closure over a ref (or a per-chart lookup into one).
export function thresholdLinesPlugin(getLevels: () => ThresholdLevels): uPlot.Plugin {
  return {
    hooks: {
      draw: [
        (u) => {
          const { warn, crit, warnColor, critColor } = getLevels()
          drawThresholdLine(u, warn, warnColor)
          drawThresholdLine(u, crit, critColor)
        },
      ],
    },
  }
}

// Wire timings are microseconds; charts plot milliseconds.
export const ms = (v: number | null | undefined) => (v == null ? null : v / 1000)

// withPctl must match the series list the view's mkOptions builds for the
// same render: uPlot requires data columns and series definitions to agree
// in count.
export function toChartData(points: SeriesPoint[], metric: Metric, withPctl: boolean): uPlot.AlignedData {
  const ts = points.map((p) => p.t)
  if (metric === 'loss') {
    return [ts, points.map((p) => p.loss_pct)]
  }
  const cols: uPlot.AlignedData = [
    ts,
    points.map((p) => ms(p.avg_us)),
    points.map((p) => ms(p.min_us)),
    points.map((p) => ms(p.max_us)),
  ]
  if (withPctl) {
    cols.push(
      points.map((p) => ms(p.p50_us)),
      points.map((p) => ms(p.p95_us)),
      points.map((p) => ms(p.p99_us)),
    )
  }
  return cols
}

export function hasAnyValue(points: SeriesPoint[], metric: Metric): boolean {
  return metric === 'loss' ? points.some((p) => p.loss_pct != null) : points.some((p) => p.avg_us != null)
}

export function statusLabel(status: string): string {
  if (status === 'ok') return 'Healthy'
  return status.replaceAll('_', ' ').replace(/^\w/, (c) => c.toUpperCase())
}

// Inserts all-null points at resolution spacing between real points so
// gaps render as gaps (spanGaps: false alone can't — uPlot connects
// adjacent columns).
export function densify(points: SeriesPoint[], resolution: number): SeriesPoint[] {
  if (points.length < 2) return points
  const out: SeriesPoint[] = []
  for (const point of points) {
    const previous = out.at(-1)
    if (previous) {
      for (let t = previous.t + resolution; t < point.t; t += resolution) {
        out.push({
          t,
          min_us: null,
          avg_us: null,
          max_us: null,
          loss_pct: null,
          samples: 0,
          failures: 0,
          p50_us: null,
          p95_us: null,
          p99_us: null,
        })
      }
    }
    out.push(point)
  }
  return out
}

// One shared loss ceiling across a view's charts: max loss ×1.1 snapped up
// to a fixed band so the scale doesn't wobble between polls.
export function lossScaleCeiling(pointArrays: SeriesPoint[][]): number {
  const values = pointArrays
    .flat()
    .map((p) => p.loss_pct)
    .filter((v): v is number => v != null)
  const target = Math.max(0, ...values) * 1.1
  return [5, 10, 25, 50, 100].find((ceiling) => ceiling >= target) ?? 100
}

// Per-series digest of the visible x range, feeding each chart's sr-only
// text alternative. One entry per value column; nulls are skipped, an
// all-null column yields the null summary.
export interface SeriesSummary {
  count: number
  latest: number | null
  min: number | null
  max: number | null
  avg: number | null
}

export function summarizeSeries(data: uPlot.AlignedData, range: { min: number; max: number } | null): SeriesSummary[] {
  return data.slice(1).map((column) => {
    let count = 0
    let latest: number | null = null
    let min = Number.POSITIVE_INFINITY
    let max = Number.NEGATIVE_INFINITY
    let sum = 0
    for (let i = 0; i < column.length; i++) {
      const x = data[0][i]
      if (range && (x < range.min || x > range.max)) continue
      const value = column[i]
      if (value == null) continue
      count++
      latest = value
      min = Math.min(min, value)
      max = Math.max(max, value)
      sum += value
    }
    if (count === 0) return { count: 0, latest: null, min: null, max: null, avg: null }
    return { count, latest, min, max, avg: sum / count }
  })
}

export function latestValueIndex(data: uPlot.AlignedData, range?: { min: number; max: number }): number | null {
  for (let i = data[0].length - 1; i >= 0; i--) {
    const x = data[0][i]
    if (range && (x < range.min || x > range.max)) continue
    if (data.slice(1).some((column) => column[i] != null)) return i
  }
  return null
}

// Pins the live legend to the newest visible measured point whenever the
// cursor is away. A persisted investigation must not show a newer value from
// outside its x range. The index is read from the plot at call time, never
// captured: baking it into the plugin would make options change every poll.
export function latestLegendPlugin(): uPlot.Plugin {
  const restore = (u: uPlot) => {
    const min = u.scales.x.min
    const max = u.scales.x.max
    const range = min == null || max == null ? undefined : { min, max }
    const idx = latestValueIndex(u.data, range)
    u.setLegend({ idxs: u.series.map(() => idx) })
  }
  // Never while the operator is hovering: their cursor owns the readout.
  const restoreIfAway = (u: uPlot) => {
    if ((u.cursor.left ?? -1) < 0) restore(u)
  }
  return {
    hooks: {
      ready: [restore],
      setData: [restoreIfAway],
      setScale: [(u, key) => key === 'x' && restoreIfAway(u)],
      setCursor: [restoreIfAway],
    },
  }
}
