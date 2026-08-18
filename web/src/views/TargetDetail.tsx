import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import uPlot from 'uplot'
import { apiGet } from '../api'
import Chart from '../components/Chart'
import HealthStrip, { stripStats, UptimeValue } from '../components/HealthStrip'
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
import { buildThresholdResolver } from '../severity'
import {
  CHART_COLORS as COLORS,
  densify,
  hasAnyValue,
  latestLegendPlugin,
  lossScaleCeiling,
  ms,
  statusLabel,
  thresholdLinesPlugin,
  toChartData,
} from '../chartkit'
import type { Metric, ThresholdLevels } from '../chartkit'
import type {
  AgentBucketFailuresResponse,
  SettingsResponse,
  StagePoint,
  TargetHealthProbe,
  TargetHealthResponse,
  TargetSeriesResponse,
  TargetSourceSummary,
  TargetStagesResponse,
  TargetSummaryResponse,
  Window,
} from '../types'
import { WINDOWS } from '../types'

const POLL_MS = 60_000

// Stage lines get one categorical color each. tcp reuses the outbound
// blue and tls the return magenta (slots 1 and 5); dns takes the accent
// violet and ttfb a CVD-safe teal; total is the axis ink at full width so
// the envelope reads as structure, not another measurement hue. Never
// slot 2's orange — that reads as the warn/crit alarm ramp. Must stay in
// lockstep with the tokens in styles.css the way CHART_COLORS does.
const STAGE_COLORS = {
  light: { dns: '#4f46e5', tcp: '#2a78d6', tls: '#d55181', ttfb: '#0e7490', total: '#55544d' },
  dark: { dns: '#818cf8', tcp: '#3987e5', tls: '#d55181', ttfb: '#2dd4bf', total: '#b9b8ae' },
}

const STAGES = [
  { key: 'dns_us', label: 'DNS', color: 'dns' },
  { key: 'tcp_connect_us', label: 'TCP connect', color: 'tcp' },
  { key: 'tls_handshake_us', label: 'TLS handshake', color: 'tls' },
  { key: 'ttfb_us', label: 'TTFB', color: 'ttfb' },
  { key: 'total_us', label: 'total', color: 'total' },
] as const

// densify's stage twin: insert all-null points so gaps render as gaps.
function densifyStages(points: StagePoint[], resolution: number): StagePoint[] {
  if (points.length < 2) return points
  const out: StagePoint[] = []
  for (const point of points) {
    const previous = out.at(-1)
    if (previous) {
      for (let t = previous.t + resolution; t < point.t; t += resolution) {
        out.push({
          t,
          dns_us: null,
          tcp_connect_us: null,
          tls_handshake_us: null,
          ttfb_us: null,
          total_us: null,
          samples: 0,
        })
      }
    }
    out.push(point)
  }
  return out
}

function toStageChartData(points: StagePoint[]): uPlot.AlignedData {
  return [points.map((p) => p.t), ...STAGES.map((s) => points.map((p) => ms(p[s.key])))]
}

function hasAnyStage(points: StagePoint[]): boolean {
  return points.some((p) => STAGES.some((s) => p[s.key] != null))
}

// SourceCard is DirectionCard from the pair page with a site on the title
// line instead of a direction; every source shares the outbound swatch —
// sites are named, not color-coded, and the charts below match.
function SourceCard({ s }: { s: TargetSourceSummary }) {
  const checks = s.checks ?? []
  return (
    <div className="pair-card dir-a">
      <h3>
        <span className="swatch series-a" />
        {s.site}
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

// The expanded per-probe strips: Agents' ProbeDetail from the target's
// side, so the label names the probing site/agent instead of the target.
function probeSortKey(p: TargetHealthProbe): string {
  return `${p.failing ? 0 : 1} ${p.site} ${p.hostname} ${p.type}`
}

function StripRows({ probes, bucketS }: { probes: TargetHealthProbe[]; bucketS: number }) {
  const nowS = Date.now() / 1000
  // Sorting a freshly-spread copy, same as Agents' probe sort (toSorted
  // needs a newer TS lib target than the build uses).
  // oxlint-disable-next-line unicorn/no-array-sort
  const sorted = [...probes].sort((x, y) => probeSortKey(x).localeCompare(probeSortKey(y)))
  return (
    <div className="probe-strip-list">
      {sorted.map((p) => {
        const s = stripStats(p.buckets, bucketS, nowS)
        return (
          <div key={p.agent_id + ':' + p.probe_id} className="probe-strip-row">
            <div className="probe-strip-label">
              <span className="mono">{p.type}</span>
              <span>
                {p.site} · {p.hostname}
              </span>
              {p.type === 'traceroute' && (
                <span
                  className="hint"
                  title="Traceroute status is destination reached; excluded from agent uptime ratios"
                >
                  path watch
                </span>
              )}
            </div>
            {/* probe_id scopes the breakdown to this series; the drill-down
                endpoint is per-agent, which each row knows. */}
            <HealthStrip
              buckets={s.inWindow}
              bucketS={bucketS}
              endS={s.endS}
              label={s.stripLabel}
              fetchSlotDetail={(t) =>
                apiGet<AgentBucketFailuresResponse>(
                  `/api/v1/agents/${p.agent_id}/health/bucket?t=${t}&probe_id=${p.probe_id}`,
                )
              }
            />
            <div className="probe-strip-uptime">
              <UptimeValue uptime={s.uptime} partial={s.partial} stripLabel={s.stripLabel} />
            </div>
            {p.failing && (
              <div className="probe-strip-error">
                <span className="status-text-down">failing since {fmtAgo(p.open_since)}</span>
                {p.error && (
                  <code className="probe-strip-error-text" title={p.error}>
                    {p.error}
                  </code>
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

export default function TargetDetail({ id, onAuthError }: { id: string; onAuthError: (err: unknown) => void }) {
  const [win, setWin] = useState<Window>('24h')
  const [metric, setMetric] = useState<Metric>('latency')
  const { resolved } = useTheme()
  // Also covers the fmtTime tooltips below; mode reaches the charts through
  // the options factories so axis ticks and legends follow the toggle.
  const { mode } = useTimezone()
  const [summary, setSummary] = useState<TargetSummaryResponse | null>(null)
  const [series, setSeries] = useState<TargetSeriesResponse | null>(null)
  const [stages, setStages] = useState<TargetStagesResponse | null>(null)
  const [health, setHealth] = useState<TargetHealthResponse | null>(null)
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [error, setError] = useState('')

  // Same load discipline as PairDetail: settings ride along so threshold
  // changes and chart redraws land together, and the generation counter
  // drops superseded responses after a window/metric flip.
  const loadGen = useRef(0)
  const load = useCallback(() => {
    const gen = ++loadGen.current
    const base = `/api/v1/targets/${encodeURIComponent(id)}`
    return Promise.all([
      apiGet<TargetSummaryResponse>(`${base}?window=${win}`),
      apiGet<TargetSeriesResponse>(`${base}/series?metric=${metric}&window=${win}`),
      apiGet<TargetStagesResponse>(`${base}/stages?window=${win}`),
      apiGet<TargetHealthResponse>(`${base}/health`),
      apiGet<SettingsResponse>('/api/v1/settings'),
    ])
      .then(([su, se, st, he, cfg]) => {
        if (gen !== loadGen.current) return
        setSummary(su)
        setSeries(se)
        setStages(st)
        setHealth(he)
        setSettings(cfg)
        setError('')
      })
      .catch((err) => {
        onAuthError(err)
        if (gen !== loadGen.current) return
        setError(err instanceof Error ? err.message : String(err))
      })
  }, [id, win, metric, onAuthError])

  useEffect(() => {
    void load()
    const pollId = setInterval(() => void load(), POLL_MS)
    return () => clearInterval(pollId)
  }, [load])

  // Effective thresholds per source site: agent-kind targets grade on the
  // source↔destination site pair (override merged over global, matching
  // ingest); external targets always grade on the globals.
  const resolveThresholds = useMemo(() => buildThresholdResolver(settings), [settings])

  // Kept current every render; the chart plugin reads the ref at draw
  // time, keyed by site so each source chart draws its own lines.
  const thresholdLevels = useRef<Record<string, ThresholdLevels>>({})
  {
    const c = COLORS[resolved]
    const dstSite = summary?.target.dst_site ?? null
    const levels: Record<string, ThresholdLevels> = {}
    for (const src of summary?.sources ?? []) {
      const effective = dstSite ? resolveThresholds(src.site, dstSite) : (settings?.thresholds ?? null)
      levels[src.site] =
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
    thresholdLevels.current = levels
  }

  const mkOptions = useMemo(() => {
    // Cached options identity, same contract as PairDetail's factory: a
    // poll that changes only data hands Chart the SAME object so the plot
    // survives; every keyed input change gets a fresh plot.
    const cache = new Map<string, Omit<uPlot.Options, 'width'>>()
    return (site: string, axisLabel: string, withPctl: boolean, lossCeiling: number): Omit<uPlot.Options, 'width'> => {
      const key = [id, win, site, axisLabel, withPctl, metric === 'loss' ? lossCeiling : '', mode].join('|')
      const cached = cache.get(key)
      if (cached) return cached
      const c = COLORS[resolved]
      const stroke = c.aToB
      const axisStyle = {
        stroke: c.axis,
        grid: { stroke: c.grid, width: 1 },
        ticks: { stroke: c.grid, width: 1 },
      }
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
        // The ref is stable; the closure resolves this chart's site at
        // draw time, so cached options never draw another site's lines.
        plugins: [
          latestLegendPlugin(),
          thresholdLinesPlugin(
            () => thresholdLevels.current[site] ?? { warn: null, crit: null, warnColor: '', critColor: '' },
          ),
        ],
        ...(mode === 'utc' ? { tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), 'Etc/UTC') } : {}),
      }
      cache.set(key, options)
      return options
    }
  }, [metric, resolved, mode, win, id])

  const stageOptions = useMemo((): Omit<uPlot.Options, 'width'> => {
    // One stage chart per page: no cache map needed, the memo identity is
    // the cache. Rebuilt on theme/timezone/window flips only — polls reuse
    // the same object, so the plot survives refreshes.
    const c = COLORS[resolved]
    const sc = STAGE_COLORS[resolved]
    const axisStyle = {
      stroke: c.axis,
      grid: { stroke: c.grid, width: 1 },
      ticks: { stroke: c.grid, width: 1 },
    }
    const value = (_u: uPlot, v: number) => (v == null ? '—' : v.toFixed(3))
    return {
      height: 230,
      series: [
        {},
        ...STAGES.map((s): uPlot.Series => ({
          label: s.label,
          stroke: sc[s.color],
          width: s.key === 'total_us' ? 2 : 1.5,
          spanGaps: false,
          value,
        })),
      ],
      axes: [{ ...axisStyle }, { ...axisStyle, label: 'Stage timings (ms)', size: 64 }],
      cursor: { drag: { x: true, y: false } },
      legend: { live: true },
      plugins: [latestLegendPlugin()],
      ...(mode === 'utc' ? { tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), 'Etc/UTC') } : {}),
    }
    // Window flips only swap the data (setData resets scales); the id is
    // covered by the route-keyed remount. Theme/timezone need new options.
  }, [resolved, mode])

  if (error && !summary)
    return (
      <div className="state-panel state-error">
        <h1>Target detail unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!summary || !series)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading target detail…
      </div>
    )

  const target = summary.target
  // Agent-kind targets are titled by site: their targets.name is the
  // synthesized agent:<uuid> handle, never shown (Agents-page convention).
  const title = target.kind === 'agent' ? (target.dst_site ?? 'deleted site') : target.name
  const addressLabel = target.url ? target.url : target.port ? `${target.address}:${target.port}` : target.address
  const withPctl = metric === 'latency' && series.source !== 'raw'
  const lossCeiling = lossScaleCeiling(series.sources.map((s) => s.points))
  const sourceLabel = series.source === 'raw' ? 'raw' : `${series.source} aggregate`
  const bucketLabel =
    (series.resolution_s >= 3600
      ? `${series.resolution_s / 3600} h buckets`
      : `${series.resolution_s / 60} min buckets`) + ` · ${sourceLabel}`
  const stagePoints = stages ? densifyStages(stages.points, stages.resolution_s) : []
  const stageData = hasAnyStage(stagePoints)

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">
            <a href="#/">Overview</a> / Target detail
          </div>
          <h1>{title}</h1>
          <p>
            {addressLabel && <span className="mono">{addressLabel}</span>}
            {addressLabel ? ' · ' : ''}
            {target.kind === 'external' ? 'external target' : 'agent target'} · health, measurements, and stage timings
            from every probing site.
          </p>
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

      {summary.sources.length === 0 ? (
        <div className="card">
          <div className="empty-state">
            <strong>No probes target this yet</strong>
            <span>Assign one in Settings → Probes and results will appear within a probe interval.</span>
          </div>
        </div>
      ) : (
        <>
          <div className="pair-cards">
            {summary.sources.map((s) => (
              <SourceCard key={s.site} s={s} />
            ))}
          </div>

          <div className="card">
            <div className="card-head">
              <span className="eyebrow">Probe health</span>
              <span className="hint">per probe series, last 24 h — click a slot for its failures</span>
            </div>
            {health && health.probes.length > 0 ? (
              <StripRows probes={health.probes} bucketS={health.bucket_s || 1800} />
            ) : (
              <div className="empty-state">
                <strong>No probe series yet</strong>
                <span>Nothing has reported a result for this target.</span>
              </div>
            )}
          </div>

          {series.sources.map((src) => {
            const points = densify(src.points, series.resolution_s)
            const axisLabel = metric === 'loss' ? 'Loss (%)' : latencyAxisLabel(src.latency_source)
            return (
              <div key={src.site} className="card chart-card">
                <h3>
                  <span className="swatch series-a" /> {src.site} → {title}
                  {metric === 'latency' && (
                    <span className="metric-source">{latencySourceName(src.latency_source)}</span>
                  )}
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
                  <Chart
                    options={mkOptions(src.site, axisLabel, withPctl, lossCeiling)}
                    data={toChartData(points, metric, withPctl)}
                  />
                )}
              </div>
            )
          })}

          <div className="card chart-card">
            <h3>
              Stage timings
              <span className="metric-source">all probing sites</span>
            </h3>
            {stageData ? (
              <Chart options={stageOptions} data={toStageChartData(stagePoints)} />
            ) : (
              <div className="chart-empty">
                <p>
                  No stage timings in this window. DNS, TCP connect, TLS handshake, and TTFB come from probe types that
                  measure them (http, tls, tcp, dns); long windows fill in as the stage history accrues.
                </p>
              </div>
            )}
          </div>
        </>
      )}
    </>
  )
}
