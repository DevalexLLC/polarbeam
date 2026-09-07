import { useMemo, type ComponentProps } from 'react'
import uPlot from 'uplot'
import { CHART_COLORS as COLORS, latestLegendPlugin, type Metric } from '../chartkit'
import { fmtLatency } from '../format'
import { useTheme } from '../theme'
import { useTimezone } from '../timezone'
import Chart from './Chart'

// Each mounted chart owns its memo. Polling and threshold updates preserve
// the plot; only changes to its series, axes, or palette rebuild options.
export default function MetricChart({
  metric,
  direction = 'aToB',
  axisLabel,
  withPctl,
  lossCeiling,
  ...chart
}: Omit<ComponentProps<typeof Chart>, 'options'> & {
  metric: Metric
  direction?: 'aToB' | 'bToA'
  axisLabel: string
  withPctl: boolean
  lossCeiling: number
}) {
  const { resolved } = useTheme()
  const { mode } = useTimezone()
  // Loss fluctuations must not recreate a latency plot.
  const ceiling = metric === 'loss' ? lossCeiling : 0
  const options = useMemo(() => {
    const c = COLORS[resolved]
    const stroke = c[direction]
    const axisStyle = {
      stroke: c.axis,
      grid: { stroke: c.grid, width: 1 },
      ticks: { stroke: c.grid, width: 1 },
    }
    // Live-legend readouts: fixed decimals so values don't jitter in width.
    const value =
      metric === 'loss'
        ? (_u: uPlot, v: number) => (v == null ? '—' : `${v.toFixed(1)}%`)
        : (_u: uPlot, v: number) => (v == null ? '—' : fmtLatency(v * 1000))
    const chartSeries: uPlot.Series[] =
      metric === 'loss'
        ? [{}, { label: 'loss %', stroke, width: 2, spanGaps: false, value }]
        : [
            {},
            { label: 'avg', stroke, width: 2, spanGaps: false, value },
            { label: 'min', stroke, width: 1, alpha: 0.4, spanGaps: false, value },
            { label: 'max', stroke, width: 1, alpha: 0.4, spanGaps: false, value },
          ]
    if (metric === 'latency' && withPctl) {
      // Aggregate windows only; must stay in lockstep with toChartData.
      chartSeries.push(
        { label: 'p50', stroke, width: 1.5, alpha: 0.7, spanGaps: false, value },
        { label: 'p95', stroke, width: 1, alpha: 0.55, dash: [6, 4], spanGaps: false, value },
        { label: 'p99', stroke, width: 1, alpha: 0.35, dash: [2, 4], spanGaps: false, value },
      )
    }
    const result: Omit<uPlot.Options, 'width'> = {
      height: 230,
      series: chartSeries,
      scales: metric === 'loss' ? { y: { range: [0, ceiling] } } : {},
      axes: [{ ...axisStyle }, { ...axisStyle, label: axisLabel, size: 64 }],
      cursor: { drag: { x: true, y: false } },
      legend: { live: true },
      plugins: [latestLegendPlugin()],
      // UTC mode pins axis ticks and the live-legend x readout to UTC
      // wall clock; local mode keeps uPlot's default (browser zone).
      ...(mode === 'utc' ? { tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), 'Etc/UTC') } : {}),
    }
    return result
  }, [axisLabel, ceiling, direction, metric, mode, resolved, withPctl])
  return <Chart {...chart} options={options} />
}
