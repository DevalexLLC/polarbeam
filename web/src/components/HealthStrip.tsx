import { useState } from 'react'
import { useTimezone } from '../timezone'
import type { AgentHealthBucket } from '../types'

// 48 × 30-min slots = the 24 h the agent-health endpoints serve.
export const SLOTS = 48

type SlotClass = 'ok' | 'degraded' | 'down' | 'nodata'

// The hover card reuses the map info card's pill classes so slot states
// wear exactly the site severities' colors; nodata borrows the stale tint.
const SLOT_SEV: Record<SlotClass, string> = { ok: 'ok', degraded: 'warn', down: 'down', nodata: 'stale' }
const SLOT_LABEL: Record<SlotClass, string> = {
  ok: 'Healthy',
  degraded: 'Degraded',
  down: 'Down',
  nodata: 'No data',
}

function slotTime(epochS: number, utc: boolean, withZone: boolean): string {
  const d = new Date(epochS * 1000)
  if (utc) return `${String(d.getUTCHours()).padStart(2, '0')}:${String(d.getUTCMinutes()).padStart(2, '0')}`
  return d.toLocaleTimeString(
    [],
    withZone ? { hour: '2-digit', minute: '2-digit', timeZoneName: 'short' } : { hour: '2-digit', minute: '2-digit' },
  )
}

interface Slot {
  cls: SlotClass
  range: string
  b: AgentHealthBucket | undefined
}

// One series' 24 h as a segmented bar strip (hand-rolled SVG — uPlot is for
// real charts). Slots align to bucket boundaries ending at the current
// bucket; a slot with no samples renders muted, never as success. Hovering
// shows a map-info-card-style readout for the slot under the pointer: its
// local time window, state, and counts. The card is fixed-position because
// strips sit inside scroll containers that would clip an absolute child,
// and pointer-inert because it is a pure readout — unlike the map's card it
// carries no links, so it never needs the delayed-clear persistence dance.
export default function HealthStrip({
  buckets,
  bucketS,
  endS,
  label,
}: {
  buckets: AgentHealthBucket[]
  bucketS: number
  endS: number
  label: string
}) {
  const { mode } = useTimezone()
  const utc = mode === 'utc'
  const [hover, setHover] = useState<{ i: number; x: number; y: number; below: boolean } | null>(null)
  const byStart = new Map(buckets.map((b) => [b.t, b]))
  // Local mode only: a UTC-offset change inside the window means a DST
  // transition — label every slot with the zone that day so repeated
  // wall-clock hours stay distinguishable (fall-back repeats an hour, so
  // two buckets would otherwise share a label and a range could read
  // reversed). UTC mode has no transitions; the range carries one UTC tag.
  const withZone =
    !utc && new Date((endS - SLOTS * bucketS) * 1000).getTimezoneOffset() !== new Date(endS * 1000).getTimezoneOffset()
  const slots: Slot[] = []
  for (let i = 0; i < SLOTS; i++) {
    const t = endS - (SLOTS - i) * bucketS
    const b = byStart.get(t)
    let cls: SlotClass
    if (!b || b.samples === 0) cls = 'nodata'
    else if (b.ok === 0) cls = 'down'
    else if (b.ok < b.samples) cls = 'degraded'
    else cls = 'ok'
    const range = `${slotTime(t, utc, withZone)}–${slotTime(t + bucketS, utc, withZone)}${utc ? ' UTC' : ''}`
    slots.push({ cls, range, b })
  }

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect()
    const i = Math.min(SLOTS - 1, Math.max(0, Math.floor(((e.clientX - r.left) / r.width) * SLOTS)))
    // Clamp so the centered card stays on screen at the viewport edges;
    // flip it below the strip when a scrolled row leaves no headroom above.
    const x = Math.min(Math.max(r.left + ((i + 0.5) / SLOTS) * r.width, 140), window.innerWidth - 140)
    const below = r.top < 150
    const y = below ? r.bottom : r.top
    setHover((prev) =>
      prev && prev.i === i && prev.x === x && prev.y === y && prev.below === below ? prev : { i, x, y, below },
    )
  }

  const slot = hover ? slots[hover.i] : null
  const pct = slot?.b && slot.b.samples > 0 ? (100 * slot.b.ok) / slot.b.samples : null
  return (
    <>
      {/* Hover-only readout: the svg keeps its aggregate aria-label; the
          card mirrors what a sighted user gains, as on the world map. */}
      {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <svg
        className="fleet-strip"
        viewBox={`0 0 ${SLOTS * 2} 12`}
        preserveAspectRatio="none"
        role="img"
        aria-label={label}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        {slots.map((s, i) => (
          <rect key={i} className={'fleet-strip-seg strip-' + s.cls} x={i * 2} y={1} width={1.4} height={10} rx={0.7} />
        ))}
      </svg>
      {hover && slot && (
        <div
          className={'map-tip strip-tip' + (hover.below ? ' strip-tip-below' : '')}
          role="status"
          style={{ left: hover.x, top: hover.y }}
        >
          <div className="map-tip-head">
            <b>{slot.range}</b>
            <span className={`map-tip-pill sev-${SLOT_SEV[slot.cls]}`}>{SLOT_LABEL[slot.cls]}</span>
          </div>
          <div className="map-tip-value">
            {pct == null ? '—' : `${pct.toFixed(1)}%`}
            <small> probe success</small>
          </div>
          <div className="map-tip-caption">
            {slot.b && slot.b.samples > 0
              ? `${slot.b.ok} of ${slot.b.samples} ${slot.b.samples === 1 ? 'probe' : 'probes'} ok`
              : 'no samples in this half hour'}
          </div>
        </div>
      )}
    </>
  )
}

export interface StripStats {
  inWindow: AgentHealthBucket[]
  endS: number
  uptime: number | null
  partial: boolean
  coveredHours: number
  stripLabel: string
}

// stripStats is the strip's shared coverage math — the fleet card and the
// per-probe detail must not drift on what "uptime" means. The ratio only
// covers buckets that have samples: a series that succeeded briefly and
// then went silent must not read as a confident 100%. Partial coverage
// renders muted with the measured span spelled out. Full confidence =
// every COMPLETED slot covered; the last slot is always the in-progress
// bucket, which is prorated into the measured hours but never decides
// coverage (or everything would flicker to "partial" right after each
// bucket boundary, and a lone fresh sample would claim a full 30 minutes).
export function stripStats(buckets: AgentHealthBucket[], bucketS: number, nowS: number): StripStats {
  // Align the slot grid to bucket boundaries; the newest (partial) bucket
  // is included so fresh failures show up within a poll cycle.
  const endS = (Math.floor(nowS / bucketS) + 1) * bucketS
  const currentStart = endS - bucketS
  const inWindow = buckets.filter((b) => b.t >= endS - SLOTS * bucketS)
  const samples = inWindow.reduce((s, b) => s + b.samples, 0)
  const ok = inWindow.reduce((s, b) => s + b.ok, 0)
  const uptime = samples > 0 ? (100 * ok) / samples : null
  const completedCovered = inWindow.filter((b) => b.samples > 0 && b.t < currentStart).length
  const currentHasData = inWindow.some((b) => b.t >= currentStart && b.samples > 0)
  const coveredHours = (completedCovered * bucketS) / 3600 + (currentHasData ? (nowS - currentStart) / 3600 : 0)
  const partial = uptime != null && completedCovered < SLOTS - 1
  const stripLabel =
    uptime == null
      ? 'No probe results in the last 24 hours'
      : partial
        ? `Probe success ${uptime.toFixed(1)}% over the ${coveredHours.toFixed(1)} measured hours of the last 24`
        : `24 hour probe success ${uptime.toFixed(1)}%`
  return { inWindow, endS, uptime, partial, coveredHours, stripLabel }
}

// UptimeValue renders the uptime figure that accompanies a strip: an
// em-dash for no data (never an invented 100%), and a muted asterisked
// figure when coverage is partial, with the measured span in the title.
export function UptimeValue({ uptime, partial, stripLabel }: Pick<StripStats, 'uptime' | 'partial' | 'stripLabel'>) {
  if (uptime == null) return <span title={stripLabel}>—</span>
  if (partial)
    return (
      <span className="fleet-uptime-partial" title={stripLabel}>
        {uptime.toFixed(1)}%*
      </span>
    )
  return <>{uptime.toFixed(1)}%</>
}
