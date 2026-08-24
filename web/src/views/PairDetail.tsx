import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import uPlot from 'uplot'
import { apiGet } from '../api'
import Chart from '../components/Chart'
import PageError from '../components/PageError'
import PathGraph, { isWidePath } from '../components/PathGraph'
import { useNetworkFilter } from '../networkFilter'
import { inheritRouteNetwork } from '../routeState'
import { useTheme } from '../theme'
import { useTimezone } from '../timezone'
import { useRouteParam } from '../useRouteState'
import {
  fmtAgo,
  fmtLatency,
  fmtLatencyGroup,
  fmtLatencyParts,
  fmtTime,
  latencyAxisLabel,
  latencySourceName,
} from '../format'
import { buildThresholdResolver } from '../severity'
import {
  CHART_COLORS as COLORS,
  densify,
  hasAnyValue,
  latestLegendPlugin,
  lossScaleCeiling,
  statusLabel,
  thresholdLinesPlugin,
  toChartData,
} from '../chartkit'
import type { Metric, ThresholdLevels } from '../chartkit'
import type {
  CurrentPath,
  CurrentPathMtu,
  DirectionSummary,
  PairResponse,
  PathMtuResponse,
  SeriesResponse,
  SettingsResponse,
  TracerouteResponse,
  Window,
} from '../types'
import { WINDOWS } from '../types'

const POLL_MS = 30_000

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
          {checks.map((check, index) => {
            const cls = 'check-chip ' + (check.status === 'ok' ? 'check-ok' : 'check-failed')
            const chipTitle = [
              statusLabel(check.status),
              check.latency_us != null ? fmtLatency(check.latency_us) : '',
              check.loss_pct != null ? `${check.loss_pct.toFixed(0)}% loss` : '',
              fmtTime(check.as_of),
            ]
              .filter(Boolean)
              .join(' · ')
            const body = (
              <>
                <span className="check-indicator" aria-hidden="true" />
                {check.type} <strong>{statusLabel(check.status)}</strong>
              </>
            )
            // Each check row is one (agent, target, probe type) series, so
            // the chip links to exactly one target detail page.
            return check.target_id ? (
              <a
                key={`${check.type}-${index}`}
                className={cls}
                title={chipTitle + ' · open target detail'}
                href={inheritRouteNetwork('#/target/' + encodeURIComponent(check.target_id))}
              >
                {body}
              </a>
            ) : (
              <span key={`${check.type}-${index}`} className={cls} title={chipTitle}>
                {body}
              </span>
            )
          })}
        </div>
      )}
    </div>
  )
}

// PathList renders one direction's current traceroute paths as one merged
// path graph (a site can field several agents, so edges thicken where their
// paths agree) with per-agent monospace hop chains as the text fallback.
function PathList({ src, dst, dir, paths }: { src: string; dst: string; dir: 'a' | 'b'; paths: CurrentPath[] }) {
  return (
    <div className={'path-current' + (isWidePath(paths) ? ' path-current-wide' : '')}>
      <h4>
        <span className={'swatch series-' + dir} /> {src} → {dst}
      </h4>
      {paths.length === 0 ? (
        <p className="muted">No traceroute yet. Traces run on a slower cadence.</p>
      ) : (
        <>
          <PathGraph
            mode="current"
            source={src}
            dest={dst}
            paths={paths.map((p) => ({
              key: p.agent_id + ':' + p.probe_id,
              hops: p.hops,
              destReached: p.dest_reached,
            }))}
          />
          {paths.map((p) => (
            <div key={p.agent_id + ':' + p.probe_id} className="path-chain">
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
              <details className="path-id">
                <summary>Hop list</summary>
                <ol className="hops mono">
                  {p.hops.map((h) => (
                    <li key={h.ttl}>
                      {h.addrs.length === 0 ? '*' : h.addrs.join(', ')}
                      {h.rtt_us.length > 0 && <span className="hint"> {fmtLatency(Math.min(...h.rtt_us))}</span>}
                    </li>
                  ))}
                </ol>
              </details>
            </div>
          ))}
        </>
      )}
    </div>
  )
}

// MtuList renders one direction's current path MTU measurements (a site
// can field several agents, so this is a list). Sizes are IP-packet bytes
// including headers — comparable directly to interface MTUs.
function MtuList({ title, dir, mtus }: { title: string; dir: 'a' | 'b'; mtus: CurrentPathMtu[] }) {
  return (
    <div className="path-current">
      <h4>
        <span className={'swatch series-' + dir} /> {title}
      </h4>
      {mtus.length === 0 ? (
        <p className="muted">No path MTU measurement yet.</p>
      ) : (
        mtus.map((m) => (
          <div key={m.agent_id + ':' + m.probe_id} className="path-chain">
            <div className="path-meta">
              <span className="mono">{m.agent}</span>
              <span className="hint" title={fmtTime(m.updated_at)}>
                {fmtAgo(m.updated_at)}
              </span>
            </div>
            <div className="mono">
              {m.largest_ok_bytes} bytes (IPv{m.ip_version})
              {m.rtt_us !== null && <span className="hint"> {fmtLatency(m.rtt_us)}</span>}
            </div>
            {m.next_hop_mtu_bytes > 0 && <div className="hint">ICMP-reported next-hop MTU {m.next_hop_mtu_bytes}</div>}
            {m.black_hole && (
              <div className="hint">black hole suspected: larger packets vanish without any ICMP error</div>
            )}
            {m.local_constraint && <div className="hint">limited by the local interface, not the network</div>}
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
  const [windowParam, setWindowParam] = useRouteParam('window', '24h')
  const [metricParam, setMetricParam] = useRouteParam('metric', 'latency')
  const win = windowParam as Window
  const metric = metricParam as Metric
  const setWin = (value: Window) => setWindowParam(value)
  const setMetric = (value: Metric) => setMetricParam(value)
  // The global top-bar filter; '' = all planes (the pre-networks fold). A
  // filtered plane the pair does not span renders honestly stale/empty —
  // same as the matrix — rather than silently falling back to the fold.
  const { network: net } = useNetworkFilter()
  const { resolved } = useTheme()
  // Also covers the fmtTime tooltips below; mode reaches the charts through
  // mkOptions so axis ticks and the live-legend readout follow the toggle.
  const { mode } = useTimezone()
  const [pair, setPair] = useState<PairResponse | null>(null)
  const [series, setSeries] = useState<SeriesResponse | null>(null)
  const [paths, setPaths] = useState<TracerouteResponse | null>(null)
  const [mtus, setMtus] = useState<PathMtuResponse | null>(null)
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [error, setError] = useState<unknown>(null)

  // Settings ride the same load as the series so a threshold change and
  // the chart redraw land in one commit. The generation counter drops
  // superseded responses: switching a slow 365d fetch to 24h must not let
  // the older response land after the newer one and mislabel the charts.
  const loadGen = useRef(0)
  const load = useCallback(() => {
    const gen = ++loadGen.current
    // The network filter rides every pair endpoint so summaries, series,
    // paths, and MTUs all describe the same plane.
    const netQ = net === '' ? '' : `&network=${encodeURIComponent(net)}`
    const netQOnly = net === '' ? '' : `?network=${encodeURIComponent(net)}`
    return Promise.all([
      apiGet<PairResponse>(`/api/v1/pairs/${encodeURIComponent(a)}/${encodeURIComponent(b)}?window=${win}${netQ}`),
      apiGet<SeriesResponse>(
        `/api/v1/pairs/${encodeURIComponent(a)}/${encodeURIComponent(b)}/series?metric=${metric}&window=${win}${netQ}`,
      ),
      apiGet<TracerouteResponse>(`/api/v1/traceroute/${encodeURIComponent(a)}/${encodeURIComponent(b)}${netQOnly}`),
      apiGet<PathMtuResponse>(`/api/v1/path-mtu/${encodeURIComponent(a)}/${encodeURIComponent(b)}${netQOnly}`),
      apiGet<SettingsResponse>('/api/v1/settings'),
    ])
      .then(([p, s, tr, pm, st]) => {
        if (gen !== loadGen.current) return
        setPair(p)
        setSeries(s)
        setPaths(tr)
        setMtus(pm)
        setSettings(st)
        setError(null)
      })
      .catch((err) => {
        onAuthError(err)
        console.error('pair detail request failed', err)
        if (gen !== loadGen.current) return
        setError(err)
      })
  }, [a, b, win, metric, net, onAuthError])

  useEffect(() => {
    void load()
    const id = setInterval(() => void load(), POLL_MS)
    return () => clearInterval(id)
  }, [load])

  // Effective thresholds for this pair, resolved on the plane in view: the
  // top-bar filter when one is set, otherwise the pair's own plane when it
  // has exactly one. A pair spanning planes with no filter has no single
  // answer, so it falls back to the all-planes and global layers — the same
  // fold the charts above it already are. These only surface as the charts'
  // warn/crit reference lines; editing overrides lives on Settings →
  // Thresholds.
  const plane = net !== '' ? net : pair?.networks.length === 1 ? pair.networks[0] : ''
  const effective = useMemo(() => buildThresholdResolver(settings)(a, b, plane), [settings, a, b, plane])

  // Kept current every render; the chart plugin reads it at draw time.
  const thresholdLevels = useRef<ThresholdLevels>({ warn: null, crit: null, warnColor: '', critColor: '' })
  {
    const c = COLORS[resolved]
    thresholdLevels.current =
      metric === 'loss'
        ? {
            warn: effective && effective.loss_warn_pct > 0 ? effective.loss_warn_pct : null,
            crit: effective ? effective.loss_crit_pct : null,
            warnColor: c.warn,
            critColor: c.crit,
          }
        : {
            warn: effective ? effective.latency_warn_us / 1000 : null,
            crit: effective ? effective.latency_crit_us / 1000 : null,
            warnColor: c.warn,
            critColor: c.crit,
          }
  }

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
      // net is keyed so switching planes never reuses a chart built for a
      // different dataset (same rationale as the pair and window keys).
      const key = [a, b, win, net, direction, axisLabel, withPctl, metric === 'loss' ? lossCeiling : '', mode].join('|')
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
        // thresholdLevels is a stable ref — safe inside the cached options.
        plugins: [latestLegendPlugin(), thresholdLinesPlugin(() => thresholdLevels.current)],
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
  }, [metric, resolved, mode, win, net, a, b])

  if (error && !series)
    return (
      <PageError
        title="Pair detail unavailable"
        subject="pair"
        error={error}
        backHref={inheritRouteNetwork('#/')}
        backLabel="Back to Overview"
        onRetry={() => void load()}
      />
    )
  if (!series || !pair)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading pair detail…
      </div>
    )

  const withPctl = metric === 'latency' && series.source !== 'raw'
  const lossCeiling = lossScaleCeiling([series.a_to_b.points, series.b_to_a.points])
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
            <a href={inheritRouteNetwork('#/')}>Overview</a> / Pair detail
          </div>
          <h1>
            {a} ⇄ {b}
          </h1>
          <p>Directional health, measurements, and current network paths.</p>
        </div>
        <span className="sub">
          {bucketLabel}
          {pair.networks.length > 1 && net === '' ? ` · spans networks: ${pair.networks.join(', ')}` : ''}
          {net !== '' ? ` · network: ${net}` : ''}
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
          <PathList src={a} dst={b} dir="a" paths={paths?.a_to_b.paths ?? []} />
          <PathList src={b} dst={a} dir="b" paths={paths?.b_to_a.paths ?? []} />
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <span className="eyebrow">Path MTU</span>
          <span className="hint">
            largest IP packet each direction carries without fragmentation (bytes incl. IP header)
          </span>
        </div>
        <div className="path-pair">
          <MtuList title={`${a} → ${b}`} dir="a" mtus={mtus?.a_to_b.mtus ?? []} />
          <MtuList title={`${b} → ${a}`} dir="b" mtus={mtus?.b_to_a.mtus ?? []} />
        </div>
      </div>

      {directions.map(({ key, dir, chart, title }) => {
        const points = densify(series[key].points, series.resolution_s)
        const chartData = toChartData(points, metric, withPctl)
        const directionSource = series[key].latency_source || series.latency_source
        const axisLabel = metric === 'loss' ? 'Loss (%)' : latencyAxisLabel(directionSource)
        const empty =
          points.length === 0 ? (
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
          ) : undefined
        return (
          <div key={key} className="card chart-card">
            <h3>
              <span className={'swatch series-' + dir} /> {title}
              {metric === 'latency' && <span className="metric-source">{latencySourceName(directionSource)}</span>}
            </h3>
            <Chart
              options={mkOptions(chart, axisLabel, withPctl, lossCeiling)}
              data={chartData}
              contextKey={[a, b, net, win, metric, chart].join('\u0000')}
              empty={empty}
            />
          </div>
        )
      })}
    </>
  )
}
