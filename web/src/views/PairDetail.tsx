import { useEffect, useMemo, useState } from 'react'
import uPlot from 'uplot'
import { apiGet } from '../api'
import Chart from '../components/Chart'
import { useTheme } from '../theme'
import { useTimezone } from '../timezone'
import {
  fmtAgo,
  fmtLatency,
  fmtLatencyGroup,
  fmtLatencyParts,
  fmtTime,
  latencyAxisLabel,
  latencySourceName,
} from '../format'
import type {
  CurrentPath,
  DirectionSummary,
  PairResponse,
  SeriesPoint,
  SeriesResponse,
  TracerouteResponse,
  Window,
} from '../types'
import { WINDOWS } from '../types'

const POLL_MS = 60_000
type Metric = 'latency' | 'loss'

// Direction colors are categorical slots 1 (blue, outbound) and 5 (magenta,
// return) — never slot 2's orange, which reads as the crit/down alarm ramp.
// Must stay in lockstep with --series-a/--series-b in styles.css; the magenta
// validates CVD + contrast against blue on both surfaces, so it is unstepped.
const COLORS = {
  light: { aToB: '#2a78d6', bToA: '#d55181', grid: '#e0dfd9', axis: '#55544d' },
  dark: { aToB: '#3987e5', bToA: '#d55181', grid: '#30312d', axis: '#b9b8ae' },
}

// Wire timings are microseconds; charts plot milliseconds.
const ms = (v: number | null | undefined) => (v == null ? null : v / 1000)

// withPctl must match the series list mkOptions builds for the same render:
// uPlot requires data columns and series definitions to agree in count.
function toChartData(points: SeriesPoint[], metric: Metric, withPctl: boolean): uPlot.AlignedData {
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

function hasAnyValue(points: SeriesPoint[], metric: Metric): boolean {
  return metric === 'loss' ? points.some((p) => p.loss_pct != null) : points.some((p) => p.avg_us != null)
}

function statusLabel(status: string): string {
  if (status === 'ok') return 'Healthy'
  return status.replaceAll('_', ' ').replace(/^\w/, (c) => c.toUpperCase())
}

function densify(points: SeriesPoint[], resolution: number): SeriesPoint[] {
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

function lossScaleCeiling(series: SeriesResponse): number {
  const values = [...series.a_to_b.points, ...series.b_to_a.points]
    .map((p) => p.loss_pct)
    .filter((v): v is number => v != null)
  const target = Math.max(0, ...values) * 1.1
  return [5, 10, 25, 50, 100].find((ceiling) => ceiling >= target) ?? 100
}

function latestValueIndex(data: uPlot.AlignedData): number {
  for (let i = data[0].length - 1; i >= 0; i--) {
    if (data.slice(1).some((column) => column[i] != null)) return i
  }
  return 0
}

// Pins the live legend to the newest measured point whenever the cursor is
// away. The index is read from the plot's own data at call time, never
// captured: baking it into the plugin would make the options identity change
// on every poll, and Chart recreates uPlot whenever options change.
function latestLegendPlugin(): uPlot.Plugin {
  const restore = (u: uPlot) => u.setLegend({ idx: latestValueIndex(u.data) })
  // Never while the operator is hovering: their cursor owns the readout.
  const restoreIfAway = (u: uPlot) => {
    if ((u.cursor.left ?? -1) < 0) restore(u)
  }
  return {
    hooks: {
      ready: [restore],
      setData: [restoreIfAway],
      setCursor: [restoreIfAway],
    },
  }
}

function DirectionCard({ title, s, dir }: { title: string; s: DirectionSummary; dir: 'a' | 'b' }) {
  const checks = s.checks ?? []
  return (
    <div className={'pair-card dir-' + dir}>
      <h3>
        <span className={'swatch series-' + dir} />
        {title}
        <span style={{ marginLeft: 'auto' }} className={'status-text-' + s.status}>
          {statusLabel(s.status)}
        </span>
      </h3>
      <div className="pair-headline">
        <span className="big">
          {fmtLatencyParts(s.latency.avg_us).value}
          <span className="unit"> {fmtLatencyParts(s.latency.avg_us).unit}</span>
        </span>
        <span className="eyebrow">avg {latencyAxisLabel(s.latency_source).replace(' (ms)', '')}</span>
      </div>
      <dl>
        <div>
          <dt>min / max</dt>
          <dd>{fmtLatencyGroup([s.latency.min_us, s.latency.max_us])}</dd>
        </div>
        {s.latency.p50_us != null && (
          <div>
            <dt>p50 / p95 / p99</dt>
            <dd>{fmtLatencyGroup([s.latency.p50_us, s.latency.p95_us ?? null, s.latency.p99_us ?? null])}</dd>
          </div>
        )}
        <div>
          <dt>Loss</dt>
          <dd>{s.loss_pct == null ? '—' : s.loss_pct.toFixed(1) + '%'}</dd>
        </div>
        <div>
          <dt>Jitter</dt>
          <dd>{fmtLatency(s.jitter_avg_us)}</dd>
        </div>
        {(s.tcp_connect_avg_us != null || s.tls_handshake_avg_us != null) && (
          <div>
            <dt>TCP / TLS</dt>
            <dd>{fmtLatencyGroup([s.tcp_connect_avg_us, s.tls_handshake_avg_us])}</dd>
          </div>
        )}
        <div>
          <dt>Last healthy</dt>
          <dd title={fmtTime(s.last_ok_at)}>{fmtAgo(s.last_ok_at)}</dd>
        </div>
        <div>
          <dt>Samples</dt>
          <dd>{s.samples}</dd>
        </div>
      </dl>
      {checks.length > 0 && (
        <div className="check-list" aria-label="Latest probe checks">
          {checks.map((check, index) => (
            <span
              key={`${check.type}-${index}`}
              className={'check-chip ' + (check.status === 'ok' ? 'check-ok' : 'check-failed')}
              title={[
                statusLabel(check.status),
                check.latency_us != null ? fmtLatency(check.latency_us) : '',
                check.loss_pct != null ? `${check.loss_pct.toFixed(0)}% loss` : '',
                fmtTime(check.as_of),
              ]
                .filter(Boolean)
                .join(' · ')}
            >
              <span className="check-indicator" aria-hidden="true" />
              {check.type} <strong>{statusLabel(check.status)}</strong>
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

// PathList renders one direction's current traceroute paths as monospace
// hop chains (a site can field several agents, so this is a list).
function PathList({ title, dir, paths }: { title: string; dir: 'a' | 'b'; paths: CurrentPath[] }) {
  return (
    <div className="path-current">
      <h4>
        <span className={'swatch series-' + dir} /> {title}
      </h4>
      {paths.length === 0 ? (
        <p className="muted">No traceroute yet. Traces run on a slower cadence.</p>
      ) : (
        paths.map((p) => (
          <div key={p.agent} className="path-chain">
            <div className="path-meta">
              <span className="mono">{p.agent}</span>
              <span className="hint" title={fmtTime(p.updated_at)}>
                {fmtAgo(p.updated_at)}
                {p.dest_reached ? '' : ' · incomplete'}
              </span>
            </div>
            <details className="path-id">
              <summary>Technical path ID</summary>
              <code>{p.path_hash}</code>
            </details>
            <ol className="hops mono">
              {p.hops.map((h) => (
                <li key={h.ttl}>
                  {h.addrs.length === 0 ? '*' : h.addrs.join(', ')}
                  {h.rtt_us.length > 0 && <span className="hint"> {fmtLatency(Math.min(...h.rtt_us))}</span>}
                </li>
              ))}
            </ol>
          </div>
        ))
      )}
    </div>
  )
}

export default function PairDetail({
  a,
  b,
  onAuthError,
}: {
  a: string
  b: string
  onAuthError: (err: unknown) => void
}) {
  const [win, setWin] = useState<Window>('24h')
  const [metric, setMetric] = useState<Metric>('latency')
  const { resolved } = useTheme()
  // Also covers the fmtTime tooltips below; mode reaches the charts through
  // mkOptions so axis ticks and the live-legend readout follow the toggle.
  const { mode } = useTimezone()
  const [pair, setPair] = useState<PairResponse | null>(null)
  const [series, setSeries] = useState<SeriesResponse | null>(null)
  const [paths, setPaths] = useState<TracerouteResponse | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = () =>
      Promise.all([
        apiGet<PairResponse>(`/api/v1/pairs/${encodeURIComponent(a)}/${encodeURIComponent(b)}?window=${win}`),
        apiGet<SeriesResponse>(
          `/api/v1/pairs/${encodeURIComponent(a)}/${encodeURIComponent(b)}/series?metric=${metric}&window=${win}`,
        ),
        apiGet<TracerouteResponse>(`/api/v1/traceroute/${encodeURIComponent(a)}/${encodeURIComponent(b)}`),
      ])
        .then(([p, s, tr]) => {
          if (!cancelled) {
            setPair(p)
            setSeries(s)
            setPaths(tr)
            setError('')
          }
        })
        .catch((err) => {
          onAuthError(err)
          if (!cancelled) setError(err instanceof Error ? err.message : String(err))
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [a, b, win, metric, onAuthError])

  const mkOptions = useMemo(() => {
    // Options are cached by everything they depend on, so a poll that changes
    // only the data hands Chart the SAME object and the uPlot instance
    // survives. A rebuilt object destroys and recreates the canvas on every
    // refresh, which is what this avoids. The key space is tiny and bounded:
    // one pair and window, two directions, a handful of axis labels, five
    // loss ceilings.
    const cache = new Map<string, Omit<uPlot.Options, 'width'>>()
    return (
      direction: 'aToB' | 'bToA',
      axisLabel: string,
      withPctl: boolean,
      lossCeiling: number,
    ): Omit<uPlot.Options, 'width'> => {
      // lossCeiling only reaches the options on the loss metric; keying on it
      // while showing latency would miss the cache — and so destroy both
      // charts — whenever loss crossed a ceiling band.
      // The pair and window are keyed too, so a different dataset always gets
      // a fresh plot rather than inheriting one built for the old series.
      const key = [a, b, win, direction, axisLabel, withPctl, metric === 'loss' ? lossCeiling : '', mode].join('|')
      const cached = cache.get(key)
      if (cached) return cached
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
          : (_u: uPlot, v: number) => (v == null ? '—' : v.toFixed(3))
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
      const options: Omit<uPlot.Options, 'width'> = {
        height: 230,
        series: chartSeries,
        scales: metric === 'loss' ? { y: { range: [0, lossCeiling] } } : {},
        axes: [{ ...axisStyle }, { ...axisStyle, label: axisLabel, size: 64 }],
        cursor: { drag: { x: true, y: false } },
        legend: { live: true },
        plugins: [latestLegendPlugin()],
        // UTC mode pins axis ticks and the live-legend x readout to UTC
        // wall clock; local mode keeps uPlot's default (browser zone).
        ...(mode === 'utc' ? { tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), 'Etc/UTC') } : {}),
      }
      cache.set(key, options)
      return options
    }
    // Dropping the cache entirely on a metric change (the series shape
    // differs), a theme flip, or a timezone flip is what makes every
    // identity new, so Chart recreates uPlot with the right palette and
    // axis zone and charts update live on toggle. Nothing in the key
    // changes on a poll, so charts survive refreshes.
  }, [metric, resolved, mode, win, a, b])

  if (error && !series)
    return (
      <div className="state-panel state-error">
        <h1>Pair detail unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!series || !pair)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading pair detail…
      </div>
    )

  const withPctl = metric === 'latency' && series.source !== 'raw'
  const lossCeiling = lossScaleCeiling(series)
  const sourceLabel = series.source === 'raw' ? 'raw' : `${series.source} aggregate`
  const bucketLabel =
    (series.resolution_s >= 3600
      ? `${series.resolution_s / 3600} h buckets`
      : `${series.resolution_s / 60} min buckets`) + ` · ${sourceLabel}`

  const directions: {
    key: 'a_to_b' | 'b_to_a'
    dir: 'a' | 'b'
    chart: 'aToB' | 'bToA'
    title: string
  }[] = [
    { key: 'a_to_b', dir: 'a', chart: 'aToB', title: `${a} → ${b}` },
    { key: 'b_to_a', dir: 'b', chart: 'bToA', title: `${b} → ${a}` },
  ]

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">
            <a href="#/">Overview</a> / Pair detail
          </div>
          <h1>
            {a} ⇄ {b}
          </h1>
          <p>Directional health, measurements, and current network paths.</p>
        </div>
        <span className="sub">
          {bucketLabel}
          {error ? ' · refresh failed, showing last data' : ''}
        </span>
      </div>

      <div className="controls">
        <div className="control-group" role="group" aria-label="Metric">
          {(['latency', 'loss'] as const).map((m) => (
            <button
              key={m}
              className={metric === m ? 'active' : ''}
              aria-pressed={metric === m}
              onClick={() => setMetric(m)}
            >
              {m}
            </button>
          ))}
        </div>
        <div className="control-group" role="group" aria-label="Window">
          {WINDOWS.map((w) => (
            <button key={w} className={win === w ? 'active' : ''} aria-pressed={win === w} onClick={() => setWin(w)}>
              {w}
            </button>
          ))}
        </div>
      </div>

      <div className="pair-cards">
        <DirectionCard title={`${a} → ${b}`} s={pair.a_to_b} dir="a" />
        <DirectionCard title={`${b} → ${a}`} s={pair.b_to_a} dir="b" />
      </div>

      <div className="card">
        <div className="card-head">
          <span className="eyebrow">Current path</span>
          <span className="hint">latest complete traceroute per direction</span>
        </div>
        <div className="path-pair">
          <PathList title={`${a} → ${b}`} dir="a" paths={paths?.a_to_b.paths ?? []} />
          <PathList title={`${b} → ${a}`} dir="b" paths={paths?.b_to_a.paths ?? []} />
        </div>
      </div>

      {directions.map(({ key, dir, chart, title }) => {
        const points = densify(series[key].points, series.resolution_s)
        const chartData = toChartData(points, metric, withPctl)
        const directionSource = series[key].latency_source || series.latency_source
        const axisLabel = metric === 'loss' ? 'Loss (%)' : latencyAxisLabel(directionSource)
        return (
          <div key={key} className="card chart-card">
            <h3>
              <span className={'swatch series-' + dir} /> {title}
              {metric === 'latency' && <span className="metric-source">{latencySourceName(directionSource)}</span>}
            </h3>
            {points.length === 0 ? (
              <div className="chart-empty">
                <p>No probe results in this window yet. New results arrive on each probe interval.</p>
              </div>
            ) : metric === 'latency' && !hasAnyValue(points, metric) ? (
              <div className="chart-empty">
                <p>
                  Every probe in this window failed, so there are no latencies to plot.{' '}
                  <button className="linklike" onClick={() => setMetric('loss')}>
                    Switch to the loss view
                  </button>{' '}
                  to see the failures over time.
                </p>
              </div>
            ) : (
              <Chart options={mkOptions(chart, axisLabel, withPctl, lossCeiling)} data={chartData} />
            )}
          </div>
        )
      })}
    </>
  )
}
