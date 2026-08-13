import { useEffect, useRef, useState } from 'react'
import { useTimezone } from '../timezone'
import type { AgentBucketFailureGroup, AgentBucketFailuresResponse, AgentHealthBucket } from '../types'

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

// probeLabel names a failure group's destination the way ProbeLabel does on
// the Agents page: agent-kind targets by destination site, external ones by
// name, and a loud fallback when the target row is gone.
function probeLabel(g: AgentBucketFailureGroup): string {
  return g.dst_site ?? g.target ?? 'deleted target'
}

// One series' 24 h as a segmented bar strip (hand-rolled SVG — uPlot is for
// real charts). Slots align to bucket boundaries ending at the current
// bucket; a slot with no samples renders muted, never as success. Hovering
// shows a map-info-card-style readout for the slot under the pointer: its
// local time window, state, and counts. The card is fixed-position because
// strips sit inside scroll containers that would clip an absolute child.
//
// With fetchSlotDetail wired, clicking a degraded/down slot pins the card
// and fills it with that bucket's failure breakdown — the "why" behind the
// slot's color. Only then is the card interactive (the error text must be
// selectable); the hover readout stays pointer-inert, so the map card's
// delayed-clear persistence dance is still not needed — a pin is held by
// state, not by pointer position. Slot drill-down is a pointer refinement
// of the hover readout; the svg keeps its aggregate aria-label and the
// card's role="status" announces the loading→loaded transition.
export default function HealthStrip({
  buckets,
  bucketS,
  endS,
  label,
  fetchSlotDetail,
}: {
  buckets: AgentHealthBucket[]
  bucketS: number
  endS: number
  label: string
  fetchSlotDetail?: (t: number) => Promise<AgentBucketFailuresResponse>
}) {
  const { mode } = useTimezone()
  const utc = mode === 'utc'
  const [hover, setHover] = useState<{ i: number; x: number; y: number; below: boolean } | null>(null)
  const [pinned, setPinned] = useState<{ t: number; x: number; y: number; below: boolean } | null>(null)
  const [detail, setDetail] = useState<{ t: number; data: AgentBucketFailuresResponse | null; error: string } | null>(
    null,
  )
  const svgRef = useRef<SVGSVGElement>(null)
  const cardRef = useRef<HTMLDivElement>(null)
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

  const slotAt = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect()
    const i = Math.min(SLOTS - 1, Math.max(0, Math.floor(((e.clientX - r.left) / r.width) * SLOTS)))
    // Clamp so the centered card stays on screen at the viewport edges;
    // flip it below the strip when a scrolled row leaves no headroom above.
    const x = Math.min(Math.max(r.left + ((i + 0.5) / SLOTS) * r.width, 140), window.innerWidth - 140)
    const below = r.top < 150
    const y = below ? r.bottom : r.top
    return { i, x, y, below }
  }

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    if (pinned) return // the pinned card owns the readout until dismissed
    const p = slotAt(e)
    setHover((prev) =>
      prev && prev.i === p.i && prev.x === p.x && prev.y === p.y && prev.below === p.below ? prev : p,
    )
  }

  // Clicking a degraded/down slot pins its breakdown; clicking the pinned
  // slot again, or any slot with nothing to explain, unpins. Propagation
  // stops so a strip click can never double as the fleet row's navigate
  // (the row's closest() guard is the other half of that belt).
  const onClick = (e: React.MouseEvent<SVGSVGElement>) => {
    if (!fetchSlotDetail) return
    e.stopPropagation()
    const p = slotAt(e)
    const cls = slots[p.i].cls
    const t = endS - (SLOTS - p.i) * bucketS
    if ((cls !== 'degraded' && cls !== 'down') || pinned?.t === t) {
      setPinned(null)
      return
    }
    // Re-clamp for the pinned card's wider max-width (22rem vs the hover
    // card's 16rem) so a slot near the viewport edge doesn't clip it.
    setPinned({ t, x: Math.min(Math.max(p.x, 190), window.innerWidth - 190), y: p.y, below: p.below })
    setHover(null)
  }

  // Load the pinned slot's breakdown; a re-pin mid-flight drops the stale
  // response (same cancelled-flag pattern as the Agents detail fetch). A
  // failure renders in the card — fail loud, never a silent empty list.
  useEffect(() => {
    if (!pinned || !fetchSlotDetail) {
      setDetail(null)
      return
    }
    let cancelled = false
    const t = pinned.t
    setDetail({ t, data: null, error: '' })
    fetchSlotDetail(t)
      .then((res) => {
        if (!cancelled) setDetail({ t, data: res, error: '' })
      })
      .catch((err: unknown) => {
        if (!cancelled) setDetail({ t, data: null, error: err instanceof Error ? err.message : String(err) })
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pinned?.t])

  // Dismissal: Escape, click outside strip+card, or any scroll — the card
  // is fixed at click-time coordinates and would visibly detach from its
  // slot. Mouse-leave never dismisses; the pin is held by state.
  useEffect(() => {
    if (!pinned) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPinned(null)
    }
    const onDocClick = (e: MouseEvent) => {
      const t = e.target as Node
      if (!cardRef.current?.contains(t) && !svgRef.current?.contains(t)) setPinned(null)
    }
    const onScroll = () => setPinned(null)
    document.addEventListener('keydown', onKey)
    // Capture phase: another strip's onClick stops propagation (its
    // row-navigation belt), which would shield its clicks from a bubble
    // listener and leave this pin stacked under the new one.
    document.addEventListener('click', onDocClick, true)
    window.addEventListener('scroll', onScroll, { capture: true, passive: true })
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('click', onDocClick, true)
      window.removeEventListener('scroll', onScroll, { capture: true })
    }
  }, [pinned])

  // The pinned slot is addressed by bucket start, not index: a poll that
  // crosses a bucket boundary shifts every slot left, and the card must
  // keep describing the clicked half hour (vanishing only once it leaves
  // the window entirely).
  const activeI = pinned ? SLOTS - (endS - pinned.t) / bucketS : (hover?.i ?? -1)
  const active = pinned ?? hover
  const slot = active && activeI >= 0 && activeI < SLOTS ? slots[activeI] : null
  const pct = slot?.b && slot.b.samples > 0 ? (100 * slot.b.ok) / slot.b.samples : null
  return (
    <>
      {/* Pointer-gained readout: the svg keeps its aggregate aria-label; the
          card mirrors what a sighted user gains, as on the world map. The
          slot drill-down is likewise pointer-only for now (per-slot focus
          targets are the eventual fix), so no key handler either. */}
      {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/click-events-have-key-events */}
      <svg
        ref={svgRef}
        className={'fleet-strip' + (fetchSlotDetail ? ' fleet-strip-clickable' : '')}
        viewBox={`0 0 ${SLOTS * 2} 12`}
        preserveAspectRatio="none"
        role="img"
        aria-label={label}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
        onClick={onClick}
      >
        {slots.map((s, i) => (
          <rect key={i} className={'fleet-strip-seg strip-' + s.cls} x={i * 2} y={1} width={1.4} height={10} rx={0.7} />
        ))}
      </svg>
      {active && slot && (
        <div
          ref={cardRef}
          className={
            'map-tip strip-tip' + (active.below ? ' strip-tip-below' : '') + (pinned ? ' strip-tip-pinned' : '')
          }
          role="status"
          style={{ left: active.x, top: active.y }}
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
          {pinned && detail && (
            <div className="strip-tip-detail">
              {detail.error ? (
                <div className="map-tip-caption">Breakdown unavailable: {detail.error}</div>
              ) : !detail.data ? (
                <div className="map-tip-caption">
                  <span className="state-spinner" /> Loading failures…
                </div>
              ) : detail.data.groups.length === 0 ? (
                // Possible when the bucket aged past raw retention (or the
                // strip rendered stale poll data) between paint and click.
                <div className="map-tip-caption">no failures recorded in this half hour</div>
              ) : (
                detail.data.groups.map((g) => (
                  <div key={`${g.probe_id} ${g.status}`} className="strip-tip-fail">
                    <span>
                      <span className="mono">{g.type}</span> → {probeLabel(g)} · {g.count}× {g.status}
                    </span>
                    {g.last_error && (
                      <code className="strip-tip-error" title={g.last_error}>
                        {g.last_error}
                      </code>
                    )}
                  </div>
                ))
              )}
            </div>
          )}
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
